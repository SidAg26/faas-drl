package scaling

import (
	"fmt"
	"log"
	"time"

	"github.com/openfaas/faas/gateway/types"
	"golang.org/x/sync/singleflight"
)

// NewFunctionScaler create a new scaler with the specified
// ScalingConfig
func NewFunctionScaler(config ScalingConfig, functionCacher FunctionCacher) FunctionScaler {
	return FunctionScaler{
		Cache:        functionCacher,
		Config:       config,
		SingleFlight: &singleflight.Group{},
	}
}

// FunctionScaler scales from zero
type FunctionScaler struct {
	Cache        FunctionCacher
	Config       ScalingConfig
	SingleFlight *singleflight.Group
}

// FunctionScaleResult holds the result of scaling from zero
type FunctionScaleResult struct {
	Available bool
	Error     error
	Found     bool
	Duration  time.Duration
	// SA - Add the Optional field for the matched function version
	// This is used to indicate if the function version was matched
	// with any other version of the function
	ServiceNameMatched *string
}

// Scale scales a function from zero replicas to 1 or the value set in
// the minimum replicas metadata
func (f *FunctionScaler) Scale(functionName, namespace string) FunctionScaleResult {
	start := time.Now()

	// First check the cache, if there are available replicas, then the
	// request can be served.
	if cachedResponse, hit := f.Cache.Get(functionName, namespace); hit &&
		cachedResponse.AvailableReplicas > 0 {
		// SA - Check if the cached response has the "version_checked" annotation
		// If the annotation is present, then the function was matched with
		// any other version of the function
		if cachedResponse.Annotations != nil && (*cachedResponse.Annotations)["version_checked"] == "true" {
			versionFound := (*cachedResponse.Annotations)["version_found"]
			return FunctionScaleResult{
				Error:              nil,
				Available:          true,
				Found:              true,
				Duration:           time.Since(start),
				ServiceNameMatched: &versionFound,
			}
		}
		return FunctionScaleResult{
			Error:     nil,
			Available: true,
			Found:     true,
			Duration:  time.Since(start),
		}
	}

	// The wasn't a hit, or there were no available replicas found
	// so query the live endpoint
	getKey := fmt.Sprintf("GetReplicas-%s.%s", functionName, namespace)
	res, err, _ := f.SingleFlight.Do(getKey, func() (interface{}, error) {
		// SA - This is where the logic for "Version Checking" needs to be added inside
		// the GetReplicas function.
		// This is the "First Layer" of Version Checking
		// if the function request for "functionName" (here referred to as serviceName) is not found
		// then we check for other available versions in the format "functionName-{memory}-{CPU}"
		// If the function is not found, then we return an error
		// If the function is found, then we return the replicas and available replicas
		// return f.Config.ServiceQuery.GetReplicas(functionName, namespace)
		// SA - Replace this with the custom GetReplicas function
		// that includes the version checking logic
		return f.Config.ServiceQuery.GetReplicasCustom(functionName, namespace)
	})

	if err != nil {
		return FunctionScaleResult{
			Error:     err,
			Available: false,
			Found:     false,
			Duration:  time.Since(start),
		}
	}
	if res == nil {
		return FunctionScaleResult{
			Error:     fmt.Errorf("empty response from server"),
			Available: false,
			Found:     false,
			Duration:  time.Since(start),
		}
	}

	// Check if there are available replicas in the live data
	if res.(ServiceQueryResponse).AvailableReplicas > 0 {
		// SA - Before the scale up logic is executed we need to check if the
		// function was matched with any other version of the function.
		// If the function was matched with any other version of the function
		// then we need to return the matched function version.
		queryResponse := res.(ServiceQueryResponse)
		if queryResponse.Annotations != nil && (*queryResponse.Annotations)["version_checked"] == "true" {
			versionFound := (*queryResponse.Annotations)["version_found"]
			return FunctionScaleResult{
				Error:              nil,
				Available:          true,
				Found:              true,
				Duration:           time.Since(start),
				ServiceNameMatched: &versionFound,
			}

		}
		return FunctionScaleResult{
			Error:     nil,
			Available: true,
			Found:     true,
			Duration:  time.Since(start),
		}
	}

	// Store the result of GetReplicas in the cache
	queryResponse := res.(ServiceQueryResponse)
	f.Cache.Set(functionName, namespace, queryResponse)

	// If the desired replica count is 0, then a scale up event
	// is required.
	if queryResponse.Replicas == 0 {
		minReplicas := uint64(1)
		if queryResponse.MinReplicas > 0 {
			minReplicas = queryResponse.MinReplicas
		}

		// In a retry-loop, first query desired replicas, then
		// set them if the value is still at 0.
		scaleResult := types.Retry(func(attempt int) error {

			res, err, _ := f.SingleFlight.Do(getKey, func() (interface{}, error) {
				// SA - Keeping this as the original GetReplicas function
				// that does not include the version checking logic.
				// But, check for the version matching logic
				if queryResponse.Annotations != nil && (*queryResponse.Annotations)["version_checked"] == "true" {
					version := (*queryResponse.Annotations)["version_found"]
					return f.Config.ServiceQuery.GetReplicas(version, namespace)
				}
				return f.Config.ServiceQuery.GetReplicas(functionName, namespace)
			})

			if err != nil {
				return err
			}

			// Cache the response
			queryResponseCache := res.(ServiceQueryResponse)
			// SA - Check if the cached response has the "version_checked" annotation
			// If the annotation is present, then the function was matched with
			// any other version of the function
			if queryResponse.Annotations != nil && (*queryResponse.Annotations)["version_checked"] == "true" {
				versionFound := (*queryResponse.Annotations)["version_found"]
				if queryResponseCache.Annotations == nil {
					queryResponseCache.Annotations = &map[string]string{}
				} else {
					(*queryResponseCache.Annotations)["version_checked"] = "true"
					(*queryResponseCache.Annotations)["version_found"] = versionFound
				}
			}
			f.Cache.Set(functionName, namespace, queryResponseCache)

			// The scale up is complete because the desired replica count
			// has been set to 1 or more.
			if queryResponseCache.Replicas > 0 {
				return nil
			}

			// Request a scale up to the minimum amount of replicas
			setKey := fmt.Sprintf("SetReplicas-%s.%s", functionName, namespace)

			if _, err, _ := f.SingleFlight.Do(setKey, func() (interface{}, error) {
				// SA - Check for the version matching logic
				if queryResponseCache.Annotations != nil && (*queryResponseCache.Annotations)["version_checked"] == "true" {
					version := (*queryResponseCache.Annotations)["version_found"]
					log.Printf("[FunctionScalerCustom %d/%d] function=%s 0 => %d requested for version %s",
						attempt, int(f.Config.SetScaleRetries), version, minReplicas, functionName)
					if err := f.Config.ServiceQuery.SetReplicas(version, namespace, minReplicas); err != nil {
						return nil, fmt.Errorf("[FunctionScalerCustom] unable to scale function [%s], err: %s", version, err)
					}
					return nil, nil
				} else {
					log.Printf("[Scale %d/%d] function=%s 0 => %d requested",
						attempt, int(f.Config.SetScaleRetries), functionName, minReplicas)

					if err := f.Config.ServiceQuery.SetReplicas(functionName, namespace, minReplicas); err != nil {
						return nil, fmt.Errorf("unable to scale function [%s], err: %s", functionName, err)
					}
					return nil, nil
				}
			}); err != nil {
				return err
			}

			return nil

		}, "Scale", int(f.Config.SetScaleRetries), f.Config.FunctionPollInterval)

		if scaleResult != nil {
			return FunctionScaleResult{
				Error:     scaleResult,
				Available: false,
				Found:     true,
				Duration:  time.Since(start),
			}
		}

	}

	// Holding pattern for at least one function replica to be available
	for i := 0; i < int(f.Config.MaxPollCount); i++ {

		res, err, _ := f.SingleFlight.Do(getKey, func() (interface{}, error) {
			// SA - Check for the version matching logic
			if queryResponse.Annotations != nil && (*queryResponse.Annotations)["version_checked"] == "true" {
				version := (*queryResponse.Annotations)["version_found"]
				return f.Config.ServiceQuery.GetReplicas(version, namespace)
			}
			// SA - Keeping this as the original GetReplicas function
			// that does not include the version checking logic.
			return f.Config.ServiceQuery.GetReplicas(functionName, namespace)
		})
		queryResponseHolding := res.(ServiceQueryResponse)

		if err == nil {
			// SA - Check if the cached response has the "version_checked" annotation
			// If the annotation is present, then the function was matched with
			// any other version of the function
			if queryResponse.Annotations != nil && (*queryResponse.Annotations)["version_checked"] == "true" {
				versionFound := (*queryResponse.Annotations)["version_found"]
				if queryResponseHolding.Annotations == nil {
					queryResponseHolding.Annotations = &map[string]string{}
				} else {
					(*queryResponseHolding.Annotations)["version_checked"] = "true"
					(*queryResponseHolding.Annotations)["version_found"] = versionFound
				}
			}
			f.Cache.Set(functionName, namespace, queryResponseHolding)
		}

		totalTime := time.Since(start)

		if err != nil {
			return FunctionScaleResult{
				Error:     err,
				Available: false,
				Found:     true,
				Duration:  totalTime,
			}
		}

		if queryResponseHolding.AvailableReplicas > 0 {

			log.Printf("[Ready] function=%s waited for - %.4fs", functionName, totalTime.Seconds())

			return FunctionScaleResult{
				Error:     nil,
				Available: true,
				Found:     true,
				Duration:  totalTime,
			}
		}

		time.Sleep(f.Config.FunctionPollInterval)
	}

	return FunctionScaleResult{
		Error:     nil,
		Available: true,
		Found:     true,
		Duration:  time.Since(start),
	}
}
