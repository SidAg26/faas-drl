// License: OpenFaaS Community Edition (CE) EULA
// Copyright (c) 2017,2019-2024 OpenFaaS Author(s)

// Copyright (c) Alex Ellis 2017. All rights reserved.

package plugin

// SA - This package is used as an interface for the gateway
// to query external providers like faas-netes in our case for
// scaling and querying function metadata. It uses HTTP to communicate
// with the external service and handles authentication if needed.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	// SA - for the regexp package
	"regexp"

	types "github.com/openfaas/faas-provider/types"
	middleware "github.com/openfaas/faas/gateway/pkg/middleware"
	"github.com/openfaas/faas/gateway/scaling"
)

// ExternalServiceQuery proxies service queries to external plugin via HTTP
type ExternalServiceQuery struct {
	URL          url.URL
	ProxyClient  http.Client
	AuthInjector middleware.AuthInjector

	// IncludeUsage includes usage metrics in the response
	IncludeUsage bool
}

// NewExternalServiceQuery proxies service queries to external plugin via HTTP
func NewExternalServiceQuery(externalURL url.URL, authInjector middleware.AuthInjector) scaling.ServiceQuery {
	timeout := 3 * time.Second

	proxyClient := http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   timeout,
				KeepAlive: 0,
			}).DialContext,
			MaxIdleConns:          1,
			DisableKeepAlives:     true,
			IdleConnTimeout:       120 * time.Millisecond,
			ExpectContinueTimeout: 1500 * time.Millisecond,
		},
	}

	return ExternalServiceQuery{
		URL:          externalURL,
		ProxyClient:  proxyClient,
		AuthInjector: authInjector,
		IncludeUsage: false,
	}
}

// GetReplicas replica count for function
func (s ExternalServiceQuery) GetReplicas(serviceName, serviceNamespace string) (scaling.ServiceQueryResponse, error) {
	start := time.Now()

	var err error
	var emptyServiceQueryResponse scaling.ServiceQueryResponse

	function := types.FunctionStatus{}

	urlPath := fmt.Sprintf("%ssystem/function/%s?namespace=%s&usage=%v",
		s.URL.String(),
		serviceName,
		serviceNamespace,
		s.IncludeUsage)

	req, err := http.NewRequest(http.MethodGet, urlPath, nil)
	if err != nil {
		return emptyServiceQueryResponse, err
	}

	if s.AuthInjector != nil {
		s.AuthInjector.Inject(req)
	}

	res, err := s.ProxyClient.Do(req)
	if err != nil {
		log.Println(urlPath, err)
		return emptyServiceQueryResponse, err

	}

	var bytesOut []byte
	if res.Body != nil {
		bytesOut, _ = io.ReadAll(res.Body)
		defer res.Body.Close()
	}

	if res.StatusCode == http.StatusOK {
		if err := json.Unmarshal(bytesOut, &function); err != nil {
			log.Printf("Unable to unmarshal: %q, %s", string(bytesOut), err)
			return emptyServiceQueryResponse, err
		}
		// -----------------------------------------------
		//SA - TESTING THE POD STATUS FETCHING

		urlPath = fmt.Sprintf("%ssystem/podstatus/query?functionName=%s&namespace=%s",
			s.URL.String(),
			serviceName,
			serviceNamespace)

		req, err = http.NewRequest(http.MethodGet, urlPath, nil)
		if err != nil {
			return emptyServiceQueryResponse, err
		}
		if s.AuthInjector != nil {
			s.AuthInjector.Inject(req)
		}
		res, err = s.ProxyClient.Do(req)
		if err != nil {
			log.Println(urlPath, err)
			return emptyServiceQueryResponse, err
		}
		if res.Body != nil {
			bytesOut, _ = io.ReadAll(res.Body)
			defer res.Body.Close()
		}
		if res.StatusCode == http.StatusOK {
			var podStatuses []types.PodStatus
			if err := json.Unmarshal(bytesOut, &podStatuses); err != nil {
				log.Printf("Unable to unmarshal: %q, %s", string(bytesOut), err)
				return emptyServiceQueryResponse, err
			}
			// Log the pod statuses for debugging
			log.Printf("Pod statuses for function %s in namespace %s: %+v", serviceName, serviceNamespace, podStatuses)
		}
		//SA - TESTING THE POD STATUS FETCHING
		// ---------------------------------------------------

		// log.Printf("GetReplicas [%s.%s] took: %fs", serviceName, serviceNamespace, time.Since(start).Seconds())

	} else {
		// // SA - This is where the logic for "Version Checking" needs to be added
		// // if the function request for "functionName" (here referred to as serviceName) is not found
		// // then we check for other available versions in the format "functionName-{memory}-{CPU}"
		// // and return the first available version if found.

		// Try to find other available versions (both for FindAlternativeFunctionVersion and DeployFunctionWithResources)
		listURL := fmt.Sprintf("%ssystem/functions?namespace=%s", s.URL.String(), serviceNamespace)
		listReq, err := http.NewRequest(http.MethodGet, listURL, nil)
		if err != nil {
			return emptyServiceQueryResponse, err
		}
		if s.AuthInjector != nil {
			s.AuthInjector.Inject(listReq)
		}
		listRes, err := s.ProxyClient.Do(listReq)
		if err != nil {
			log.Println(listURL, err)
			return emptyServiceQueryResponse, err
		}
		defer listRes.Body.Close()

		var functions []types.FunctionStatus
		log.Printf("[GetReplicasCustom] Checking for available versions of function: %s in namespace: %s", serviceName, serviceNamespace)
		if listRes.StatusCode == http.StatusOK {
			listBytes, _ := io.ReadAll(listRes.Body)
			if err := json.Unmarshal(listBytes, &functions); err != nil {
				log.Printf("Unable to unmarshal function list: %q, %s", string(listBytes), err)
				return emptyServiceQueryResponse, err
			}
		}
		// // Look for a function with the same base name and a version suffix
		// // Extract the base service name from the function name
		// baseName := serviceName
		// if idx := strings.Index(serviceName, "-"); idx != -1 {
		// 	baseName = serviceName[:idx]
		// }
		// log.Printf("Base service name: %s", baseName)

		// 	alternativeFunction, err := FindAlternativeFunctionVersion(serviceName, baseName, functions)
		// 	if err != nil {
		// 		return emptyServiceQueryResponse, fmt.Errorf("error finding alternative function version: %w", err)
		// 	}
		// 	if alternativeFunction != nil {
		// 		log.Printf("Found alternative function version: %s with available replicas: %d", serviceName, alternativeFunction.AvailableReplicas)
		// 		return *alternativeFunction, nil
		// 	}
		// }

		// ---------- SA - Implementing the logic to Deploy a new function with resources ----------
		// Extract memory and CPU requirements from the requested function name
		requestedMemory, requestedCPU := 0, 0
		baseName := serviceName // Default to the original service name
		if strings.Contains(serviceName, "-") {
			parts := strings.Split(serviceName, "-")
			if len(parts) >= 3 {
				baseName = parts[0] // this is to match other versions like "functionName-{memory}-{CPU}"
				requestedMemory, _ = strconv.Atoi(parts[1])
				requestedCPU, _ = strconv.Atoi(parts[2])
			}
		}

		// If we have valid memory and CPU requirements, deploy a new function
		if baseName != serviceName && requestedMemory > 0 && requestedCPU > 0 {
			log.Printf("Deploying new function version with memory=%dMB, CPU=%d cores",
				requestedMemory, requestedCPU)

			newDeployment, err := s.DeployFunctionWithResources(
				serviceName, baseName, serviceNamespace, requestedMemory, requestedCPU, functions)

			if err != nil {
				log.Printf("Failed to deploy new function: %v", err)
			} else {
				log.Printf("Successfully initiated deployment for %s", serviceName)
				return *newDeployment, nil
			}
		}

		log.Printf("[GetReplicasCustom] [%s.%s] took: %.4fs, code: %d\n", serviceName, serviceNamespace, time.Since(start).Seconds(), res.StatusCode)
		return emptyServiceQueryResponse, fmt.Errorf("server returned non-200 status code (%d) for function, %s, body: %s", res.StatusCode, serviceName, string(bytesOut))
	}

	minReplicas := uint64(scaling.DefaultMinReplicas)
	maxReplicas := uint64(scaling.DefaultMaxReplicas)
	scalingFactor := uint64(scaling.DefaultScalingFactor)
	availableReplicas := function.AvailableReplicas

	if function.Labels != nil {
		labels := *function.Labels

		minReplicas = extractLabelValue(labels[scaling.MinScaleLabel], minReplicas)
		maxReplicas = extractLabelValue(labels[scaling.MaxScaleLabel], maxReplicas)
		extractedScalingFactor := extractLabelValue(labels[scaling.ScalingFactorLabel], scalingFactor)

		if extractedScalingFactor > 0 && extractedScalingFactor <= 100 {
			scalingFactor = extractedScalingFactor
		} else {
			return scaling.ServiceQueryResponse{}, fmt.Errorf("bad scaling factor: %d, is not in range of [0 - 100]", extractedScalingFactor)
		}
	}

	return scaling.ServiceQueryResponse{
		Replicas:          function.Replicas,
		MaxReplicas:       maxReplicas,
		MinReplicas:       minReplicas,
		ScalingFactor:     scalingFactor,
		AvailableReplicas: availableReplicas,
		Annotations:       function.Annotations,
	}, err
}

// SetReplicas update the replica count
func (s ExternalServiceQuery) SetReplicas(serviceName, serviceNamespace string, count uint64) error {
	var err error

	scaleReq := types.ScaleServiceRequest{
		ServiceName: serviceName,
		Replicas:    count,
	}

	requestBody, err := json.Marshal(scaleReq)
	if err != nil {
		return err
	}

	start := time.Now()
	urlPath := fmt.Sprintf("%ssystem/scale-function/%s?namespace=%s", s.URL.String(), serviceName, serviceNamespace)
	req, _ := http.NewRequest(http.MethodPost, urlPath, bytes.NewReader(requestBody))

	if s.AuthInjector != nil {
		s.AuthInjector.Inject(req)
	}

	defer req.Body.Close()
	res, err := s.ProxyClient.Do(req)

	if err != nil {
		log.Println(urlPath, err)
	} else {
		if res.Body != nil {
			defer res.Body.Close()
		}
	}

	if !(res.StatusCode == http.StatusOK || res.StatusCode == http.StatusAccepted) {
		err = fmt.Errorf("error scaling HTTP code %d, %s", res.StatusCode, urlPath)
	}

	log.Printf("SetReplicas [%s.%s] took: %.4fs",
		serviceName, serviceNamespace, time.Since(start).Seconds())

	return err
}

// extractLabelValue will parse the provided raw label value and if it fails
// it will return the provided fallback value and log an message
func extractLabelValue(rawLabelValue string, fallback uint64) uint64 {
	if len(rawLabelValue) <= 0 {
		return fallback
	}

	value, err := strconv.Atoi(rawLabelValue)

	if err != nil {
		log.Printf("Provided label value %s should be of type uint", rawLabelValue)
		return fallback
	}

	return uint64(value)
}

// SA - FindAlternativeFunctionVersion searches for an alternative function version with available replicas.
func FindAlternativeFunctionVersion(serviceName, baseName string, functions []types.FunctionStatus) (*scaling.ServiceQueryResponse, error) {
	pattern := fmt.Sprintf(`^%s-\d+-\d+$`, regexp.QuoteMeta(baseName))
	re := regexp.MustCompile(pattern)

	for _, fn := range functions {
		if fn.Name == serviceName {
			continue // skip the original function
		}
		if re.MatchString(fn.Name) && fn.AvailableReplicas > 0 {
			minReplicas := uint64(scaling.DefaultMinReplicas)
			maxReplicas := uint64(scaling.DefaultMaxReplicas)
			scalingFactor := uint64(scaling.DefaultScalingFactor)
			availableReplicas := fn.AvailableReplicas

			if fn.Labels != nil {
				labels := *fn.Labels
				minReplicas = extractLabelValue(labels[scaling.MinScaleLabel], minReplicas)
				maxReplicas = extractLabelValue(labels[scaling.MaxScaleLabel], maxReplicas)
				extractedScalingFactor := extractLabelValue(labels[scaling.ScalingFactorLabel], scalingFactor)
				if extractedScalingFactor > 0 && extractedScalingFactor <= 100 {
					scalingFactor = extractedScalingFactor
				} else {
					return nil, fmt.Errorf("bad scaling factor: %d, is not in range of [0 - 100]", extractedScalingFactor)
				}
			}

			if fn.Annotations == nil {
				fn.Annotations = &map[string]string{}
			}
			annotations := fn.Annotations
			if annotations == nil {
				m := make(map[string]string)
				annotations = &m
			}
			(*annotations)["version_found"] = fn.Name
			(*annotations)["version_checked"] = "true"

			resp := scaling.ServiceQueryResponse{
				Replicas:          fn.Replicas,
				MaxReplicas:       maxReplicas,
				MinReplicas:       minReplicas,
				ScalingFactor:     scalingFactor,
				AvailableReplicas: availableReplicas,
				Annotations:       annotations,
			}
			return &resp, nil
		}
	}
	return nil, nil
}

// SA - DeployFunctionWithResources creates a new function deployment with specified resources
func (s ExternalServiceQuery) DeployFunctionWithResources(originalName, baseServiceName, serviceNamespace string,
	memoryMB, cpuCores int, functions []types.FunctionStatus) (*scaling.ServiceQueryResponse, error) {
	// 1. Get the original function definition (if it exists)
	var originalFunction types.FunctionStatus

	// baseServiceName is just the name of the function without any resource suffix
	// therefore, first find any existing function with the same base name
	pattern := fmt.Sprintf(`^%s-\d+-\d+$`, regexp.QuoteMeta(baseServiceName))
	re := regexp.MustCompile(pattern)

	for _, fn := range functions {
		if fn.Name == baseServiceName {
			continue // skip the original function
		}
		if re.MatchString(fn.Name) && fn.AvailableReplicas > 0 {
			// If we found a function with the same base name, use its configuration
			originalFunction = fn
			log.Printf("Found existing function with base name %s: %s", baseServiceName, fn.Name)
			break
		}
	}

	// 2. Create a new function with updated resources
	newFunctionName := fmt.Sprintf("%s-%d-%d", baseServiceName, memoryMB, cpuCores)

	// Create deployment request
	deployReq := types.FunctionDeployment{
		Service:     newFunctionName,
		Image:       originalFunction.Image,
		EnvProcess:  originalFunction.EnvProcess,
		EnvVars:     originalFunction.EnvVars,
		Constraints: originalFunction.Constraints,
		Secrets:     originalFunction.Secrets,
		Labels:      originalFunction.Labels,
		Annotations: originalFunction.Annotations,
		Namespace:   serviceNamespace,
		Limits: &types.FunctionResources{
			Memory: fmt.Sprintf("%dMi", memoryMB),
			CPU:    fmt.Sprintf("%dm", cpuCores*1000), // Convert to millicores i.e. x core = x*10 millicores
		},
		Requests: &types.FunctionResources{
			Memory: fmt.Sprintf("%dMi", memoryMB),
			CPU:    fmt.Sprintf("%dm", cpuCores*1000), // Convert to millicores
		},
	}

	// Update the Labels on the new function deployment
	if deployReq.Labels == nil {
		deployReq.Labels = &map[string]string{}
	}
	(*deployReq.Labels)["faas_function"] = newFunctionName

	// 3. Deploy the new function
	deployBody, err := json.Marshal(deployReq)
	if err != nil {
		return nil, err
	}

	deployURL := fmt.Sprintf("%ssystem/functions", s.URL.String())
	deployReqRes, err := http.NewRequest(http.MethodPost, deployURL, bytes.NewReader(deployBody))
	if err != nil {
		return nil, err
	}

	if s.AuthInjector != nil {
		s.AuthInjector.Inject(deployReqRes)
	}

	deployRes, err := s.ProxyClient.Do(deployReqRes)
	if err != nil {
		return nil, err
	}
	defer deployRes.Body.Close()

	if deployRes.StatusCode != http.StatusOK && deployRes.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(deployRes.Body)
		return nil, fmt.Errorf("failed to deploy function: %s, status: %d, body: %s",
			newFunctionName, deployRes.StatusCode, string(body))
	}

	// 4. Return a response that indicates the function is being deployed
	resp := scaling.ServiceQueryResponse{
		Replicas:          0, // Start with 0 replica to let the Scaler do minimum scale up
		MaxReplicas:       uint64(scaling.DefaultMaxReplicas),
		MinReplicas:       uint64(scaling.DefaultMinReplicas),
		ScalingFactor:     uint64(scaling.DefaultScalingFactor),
		AvailableReplicas: 0, // Initially 0 until deployment completes
		Annotations: &map[string]string{
			"dynamic_deployment": "true",
			"original_request":   originalName,
			"base_function":      baseServiceName,
			"memory_mb":          fmt.Sprintf("%d", memoryMB),
			"cpu_cores":          fmt.Sprintf("%d", cpuCores),
		},
	}

	return &resp, nil
}
