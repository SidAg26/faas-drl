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
	"log"       // SA - Importing math/rand to use random number generation
	"math/rand" // SA - Importing math/rand to use random number generation
	"net"
	"net/http"
	"net/url"
	"sort" // SA - sort package is used to sort the alternative function versions
	"strconv"
	"strings"
	"time"

	// SA - for the regexp package
	"regexp"

	"golang.org/x/sync/singleflight" // SA - Importing singleflight for handling concurrent requests

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
	// SA - Add the deployGroup field to handle singleflight for deployments
	deployGroup singleflight.Group
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

	return &ExternalServiceQuery{
		URL:          externalURL,
		ProxyClient:  proxyClient,
		AuthInjector: authInjector,
		IncludeUsage: false,
		deployGroup:  singleflight.Group{},
	}
}

// GetReplicas replica count for function
func (s *ExternalServiceQuery) GetReplicas(serviceName, serviceNamespace string) (scaling.ServiceQueryResponse, error) {
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

		// log.Printf("GetReplicas [%s.%s] took: %fs", serviceName, serviceNamespace, time.Since(start).Seconds())

	} else {
		log.Printf("GetReplicas [%s.%s] took: %.4fs, code: %d\n", serviceName, serviceNamespace, time.Since(start).Seconds(), res.StatusCode)
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

// SA - Create a new GetReplicas function that will not overlap the exisitng logic at multiple places
// GetReplicas is called in the following places:
// alerthandler.go - to scale the function based on alerts
// scaling/function_scaler.go - to scale the function based on the current replicas, checks repelicas and available replicas
func (s *ExternalServiceQuery) GetReplicasCustom(serviceName, serviceNamespace string) (scaling.ServiceQueryResponse, error) {
	start := time.Now()

	var err error
	var emptyServiceQueryResponse scaling.ServiceQueryResponse

	function := types.FunctionStatus{}
	var functions []types.FunctionStatus

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
		// SA - Function exists in deployment, unmarshal the response
		if err := json.Unmarshal(bytesOut, &function); err != nil {
			log.Printf("[GetReplicasCustom] Unable to unmarshal: %q, %s", string(bytesOut), err)
			return emptyServiceQueryResponse, err
		}
		// -----------------------------------------------
		//SA - 1. Check if the function has idle pods
		// -----------------------------------------------
		podFound, err := s.GetFunctionPodStatus(serviceName, serviceNamespace)
		if err != nil {
			log.Printf("[GetReplicasCustom] Error fetching pod statuses: %v", err)
			return emptyServiceQueryResponse, err
		}

		// If idle pods are found, return the function status with
		// default behavior
		// If no idle pods are found, look for alternative solution
		// -----------------------------------------------
		// SA - 2. Check for alternative function versions - Resource Reused
		// -----------------------------------------------
		if podFound && function.AvailableReplicas == 0 {
			log.Printf("[GetReplicasCustom] Function %s has no idle pods - stale cache returned. Finding alternatives", function.Name)
			// If the function has no idle pods, we need to find alternatives
			functions, err = s.GetFunctionList(serviceNamespace)
			if err != nil {
				log.Printf("[GetReplicasCustom] Error fetching function list: %v", err)
				return emptyServiceQueryResponse, err
			}
			alternateVersionList, err := s.FindAlternativeFunctionVersion(serviceName, serviceNamespace, functions)
			if err != nil {
				log.Printf("[GetReplicasCustom] Error finding alternative function version: %v", err)
				return emptyServiceQueryResponse, err
			}
			if alternateVersionList != nil {
				index := rand.Intn(len(alternateVersionList))
				return prepareVersionResponse(alternateVersionList[index])
			}
			log.Printf("[GetReplicasCustom] No alternative function version found for %s in namespace %s", serviceName, serviceNamespace)
			// return emptyServiceQueryResponse, fmt.Errorf("no alternative function version found for %s in namespace %s", serviceName, serviceNamespace)
		}
		if !podFound {
			// SA - No idle pods found, look for alternative solutions
			// Score and Select the cheapest alternative solutions
			log.Printf("[GetReplicasCustom] No idle pods found for function %s in namespace %s", serviceName, serviceNamespace)
			functions, err = s.GetFunctionList(serviceNamespace)
			if err != nil {
				log.Printf("[GetReplicasCustom] Error fetching function list: %v", err)
				return emptyServiceQueryResponse, err
			}

			alternateVersionList, err := s.FindAlternativeFunctionVersion(serviceName, serviceNamespace, functions)
			if err != nil {
				log.Printf("[GetReplicasCustom] Error finding alternative function version: %v", err)
				return emptyServiceQueryResponse, err
			}
			if alternateVersionList != nil {
				index := rand.Intn(len(alternateVersionList))
				return prepareVersionResponse(alternateVersionList[index])
			}
			// SA - No idle pods found and no alternative function version found
			log.Printf("[GetReplicasCustom] No alternative function version found for %s in namespace %s", serviceName, serviceNamespace)
		}
		// No idle pods found and no alternative function version found
		// -----------------------------------------------
		// SA - 3. If we are here, then try in a Best Effort manner and redirect to the original function
		// -----------------------------------------------
		log.Printf("GetReplicas [%s.%s] took: %fs", serviceName, serviceNamespace, time.Since(start).Seconds())
	} else {
		// SA - This is where the logic for "Version Checking" needs to be added
		// if the function request for "functionName" (here referred to as serviceName) is not found
		// then we check for other available versions in the format "functionName-{memory}-{CPU}"
		// and return the first available version if found.

		// Try to find other available versions (both for FindAlternativeFunctionVersion and DeployFunctionWithResources)
		log.Printf("[GetReplicasCustom] Function %s not found in namespace %s, status code: %d",
			serviceName, serviceNamespace, res.StatusCode)
		functions, err = s.GetFunctionList(serviceNamespace)
		if err != nil {
			log.Printf("[GetReplicasCustom] Error fetching function list: %v", err)
			return emptyServiceQueryResponse, err
		}

		// SA - 4. Find, Score and Select the cheapest alternative function versions
		alternateVersionList, err := s.FindAlternativeFunctionVersion(serviceName, serviceNamespace, functions)
		if err != nil {
			log.Printf("[GetReplicasCustom] Error finding alternative function version: %v", err)
			return emptyServiceQueryResponse, err
		}
		scoreVersion := uint64(0)
		if alternateVersionList == nil {
			log.Printf("[GetReplicasCustom] No alternative function version found for %s in namespace %s", serviceName, serviceNamespace)
			scoreVersion = uint64(0) // No alternative version found, set score to 0
		} else {
			scoreVersion = alternateVersionList[0].Score
		}

		// SA - 5. Score the deployment scheme (cold start)
		// For now, we will use a simple scoring mechanism based on the version score
		scoreColdStart := ScoreColdStart(scoreVersion)

		// SA - 6. If the cold start is better, then deploy a new function with requested resources
		if scoreColdStart < scoreVersion || scoreVersion == 0 {
			log.Printf("[GetReplicasCustom] Deploying new function version with resources for %s in namespace %s, score: %d",
				serviceName, serviceNamespace, scoreColdStart)
			resp, err := s.DeployFunctionWithResources(serviceName, serviceNamespace, functions)
			if err != nil {
				log.Printf("[GetReplicasCustom] Error deploying function with resources: %v", err)
				return emptyServiceQueryResponse, err
			}
			log.Printf("[GetReplicasCustom] Successfully deployed new function version with resources for %s in namespace %s",
				serviceName, serviceNamespace)
			return resp, nil
		} else {
			log.Printf("[GetReplicasCustom] No need to deploy new function version with resources for %s in namespace %s, score: %d",
				serviceName, serviceNamespace, scoreColdStart)
			// Prepare the response with the first available version
			return prepareVersionResponse(alternateVersionList[0])

		}

	}
	log.Printf("[GetReplicasCustom] [%s.%s] took: %.4fs, code: %d\n", serviceName, serviceNamespace, time.Since(start).Seconds(), res.StatusCode)
	return prepareResponse(function)
}

// SetReplicas update the replica count
func (s *ExternalServiceQuery) SetReplicas(serviceName, serviceNamespace string, count uint64) error {
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

// Helper function to prepare the Version response
func prepareResponse(function types.FunctionStatus) (scaling.ServiceQueryResponse, error) {
	var emptyServiceQueryResponse scaling.ServiceQueryResponse
	fn := function
	log.Printf("[prepareResponse] Found function version: %s with available replicas: %d",
		fn.Name, fn.AvailableReplicas)
	minReplicas := uint64(scaling.DefaultMinReplicas)
	maxReplicas := uint64(scaling.DefaultMaxReplicas)
	scalingFactor := uint64(scaling.DefaultScalingFactor)
	if fn.Labels != nil {
		labels := *fn.Labels
		minReplicas = extractLabelValue(labels[scaling.MinScaleLabel], minReplicas)
		maxReplicas = extractLabelValue(labels[scaling.MaxScaleLabel], maxReplicas)
		extractedScalingFactor := extractLabelValue(labels[scaling.ScalingFactorLabel], scalingFactor)
		if extractedScalingFactor > 0 && extractedScalingFactor <= 100 {
			scalingFactor = extractedScalingFactor
		} else {
			return emptyServiceQueryResponse, fmt.Errorf("[prepareResponse] bad scaling factor: %d, is not in range of [0 - 100]", extractedScalingFactor)
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

	queryRes := scaling.ServiceQueryResponse{
		Replicas:          fn.Replicas,
		MaxReplicas:       maxReplicas,
		MinReplicas:       minReplicas,
		ScalingFactor:     scalingFactor,
		AvailableReplicas: fn.AvailableReplicas,
		Annotations:       annotations,
	}
	return queryRes, nil
}

// Helper function to prepare the Version response
func prepareVersionResponse(function struct {
	Function types.FunctionStatus
	MemoryMB uint64
	CPU      uint64
	Score    uint64
}) (scaling.ServiceQueryResponse, error) {
	var emptyServiceQueryResponse scaling.ServiceQueryResponse
	fn := function.Function
	log.Printf("[prepareVersionResponse] Found alternative function version: %s with available replicas: %d",
		fn.Name, fn.AvailableReplicas)
	minReplicas := uint64(scaling.DefaultMinReplicas)
	maxReplicas := uint64(scaling.DefaultMaxReplicas)
	scalingFactor := uint64(scaling.DefaultScalingFactor)
	if fn.Labels != nil {
		labels := *fn.Labels
		minReplicas = extractLabelValue(labels[scaling.MinScaleLabel], minReplicas)
		maxReplicas = extractLabelValue(labels[scaling.MaxScaleLabel], maxReplicas)
		extractedScalingFactor := extractLabelValue(labels[scaling.ScalingFactorLabel], scalingFactor)
		if extractedScalingFactor > 0 && extractedScalingFactor <= 100 {
			scalingFactor = extractedScalingFactor
		} else {
			return emptyServiceQueryResponse, fmt.Errorf("[prepareVersionResponse] bad scaling factor: %d, is not in range of [0 - 100]", extractedScalingFactor)
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
	queryRes := scaling.ServiceQueryResponse{
		Replicas:          fn.Replicas,
		MaxReplicas:       maxReplicas,
		MinReplicas:       minReplicas,
		ScalingFactor:     scalingFactor,
		AvailableReplicas: fn.AvailableReplicas,
		Annotations:       annotations,
	}
	return queryRes, nil
}

// SA - GetFunctionPodStatus retrieves the pod status for a specific function
func (s *ExternalServiceQuery) GetFunctionPodStatus(functionName, functionNamespace string) (bool, error) {
	timeStart := time.Now()
	key := fmt.Sprintf("%s-%s", "GetFunctionPodStatus", functionName)
	result, err, _ := s.deployGroup.Do(key, func() (interface{}, error) {
		urlPath := fmt.Sprintf("%ssystem/podstatus/query?functionName=%s&namespace=%s",
			s.URL.String(),
			functionName,
			functionNamespace)

		req, err := http.NewRequest(http.MethodGet, urlPath, nil)
		if err != nil {
			return false, err
		}

		if s.AuthInjector != nil {
			s.AuthInjector.Inject(req)
		}

		res, err := s.ProxyClient.Do(req)
		if err != nil {
			log.Println(urlPath, err)
			return false, err
		}
		defer res.Body.Close()

		if res.StatusCode == http.StatusNotFound {
			// No pods found for the function, return false
			// Maybe first time since the cache was cleared
			log.Printf("[GetFunctionPodStatus] No pods found for function %s in namespace %s", functionName, functionNamespace)
			return true, nil
		} else if res.StatusCode != http.StatusOK {
			// Unexpected status code, return an error
			body, _ := io.ReadAll(res.Body)
			return false, fmt.Errorf("[GetFunctionPodStatus] server returned non-200 status code (%d) for function %s in namespace %s, body: %s",
				res.StatusCode, functionName, functionNamespace, string(body))
		}

		if res.StatusCode == http.StatusOK {
			// SA - Function exists in deployment, unmarshal the response
			var podStatuses []types.PodStatus
			bytesOut, _ := io.ReadAll(res.Body)
			// Trim whitespace to check for empty or "{}"
			trimmed := strings.TrimSpace(string(bytesOut))
			if trimmed == "" || trimmed == "{}" || trimmed == "no pods found" || trimmed == "no pods found\n" {
				// Empty response, treat as no pods found (not an error)
				podStatuses = []types.PodStatus{}
				return true, nil // maybe first time since the cache was cleared
			} else {
				if err := json.Unmarshal(bytesOut, &podStatuses); err != nil {
					log.Printf("[GetFunctionPodStatus] Unable to unmarshal pod status: %q, %s", string(bytesOut), err)
					return false, err
				}
			}

			// Found the pod statuses for the function
			if len(podStatuses) > 0 {
				idlePods := 0
				for _, pod := range podStatuses {
					if pod.Status == "idle" {
						idlePods++
						break // We found at least one idle pod, no need to continue checking
					}
				}
				// The current function has idle pods and therefore continue with the current function
				// i.e., the default logic of forwarding the request to the function
				if idlePods > 0 {
					log.Printf("[GetFunctionPodStatus] Function %s in namespace %s has atleast %d idle pods", functionName, functionNamespace, idlePods)
					return true, nil // Function is idle, return true to indicate that the function is available
				}
				log.Printf("[GetFunctionPodStatus] Function %s in namespace %s has no idle pods", functionName, functionNamespace)
				// No idle pods found, return false to indicate that the function is busy
				// look for alternative solutions
				return false, nil
			}
		}
		log.Printf("[GetFunctionPodStatus] [%s.%s] took: %.4fs", functionName, functionNamespace, time.Since(timeStart).Seconds())
		// No idle pods found, return false to indicate that the function is busy
		return false, nil
	})
	if err != nil {
		log.Printf("[GetFunctionPodStatus] Error getting pod status for function %s in namespace %s: %v", functionName, functionNamespace, err)
		return false, err
	}

	return result.(bool), nil
}

// SA - FindAlternativeFunctionVersion searches for an alternative function version with available replicas.
func (s *ExternalServiceQuery) FindAlternativeFunctionVersion(serviceName string,
	serviceNamespace string,
	functions []types.FunctionStatus) ([]struct {
	Function types.FunctionStatus
	MemoryMB uint64
	CPU      uint64
	Score    uint64
}, error) {

	var matchedFunctions []struct {
		Function types.FunctionStatus
		MemoryMB uint64
		CPU      uint64
		Score    uint64
	}
	timeStart := time.Now()
	// Look for a function with the same base name and a version suffix
	// Extract the base service name from the function name
	requestedMemory, requestedCPU := uint64(512), uint64(1)
	baseName := serviceName // Default to the original service name
	// if strings.Contains(serviceName, "-") {
	// 	parts := strings.Split(serviceName, "-")
	// 	if len(parts) >= 3 {
	// 		baseName = parts[0] // this is to match other versions like "functionName-{memory}-{CPU}"
	// 		requestedMemory, _ = strconv.ParseUint(parts[1], 10, 64)
	// 		requestedCPU, _ = strconv.ParseUint(parts[2], 10, 64)
	// 	}
	// }
	// Use a regex to extract the base name and version parts
	// The regex matches the pattern: function-Name-{memory}-{CPU}
	versionRegex := regexp.MustCompile(`^(.+)-(\d+)-(\d+)$`)
	matches := versionRegex.FindStringSubmatch(serviceName)

	if len(matches) == 4 {
		// Deployment follows naming convention
		baseName = matches[1]

	}
	log.Printf("[FindAlternativeVersion] Base service name: %s as expected", baseName)
	log.Printf("[FindAlternativeVersion] Checking for available versions of function: %s in namespace: %s", serviceName, serviceNamespace)

	pattern := fmt.Sprintf(`^%s-\d+-\d+$`, regexp.QuoteMeta(baseName))
	re := regexp.MustCompile(pattern)

	for _, fn := range functions {
		if fn.Name == serviceName {
			continue // skip the original function
		}
		if re.MatchString(fn.Name) && fn.AvailableReplicas > 0 {
			// If the function is matched, check for idle pods
			podFound, err := s.GetFunctionPodStatus(fn.Name, serviceNamespace)
			if err != nil {
				log.Printf("[FindAlternativeVersion] Error checking for idle pods: %v", err)
				continue
			}
			if !podFound {
				continue // skip the function if it has no idle pods
			}
			// log.Printf("[FindAlternativeVersion] Found alternative function version: %s with available memory: %s and cpu: %s",
			// 	fn.Name, fn.Requests.Memory, fn.Requests.CPU)
			// If we found a function with the same base name, use its configuration
			// Extract memory and CPU from the function requests and limits
			memory, cpu := extractMemoryAndCPU(fn)

			// Only consider the function if it has enough available resources
			if memory >= requestedMemory && cpu >= requestedCPU {
				log.Printf("[FindAlternativeVersion] Found alternative function version: %s with memory: %dMB, CPU: %d cores",
					fn.Name, memory, cpu)
			} else {
				log.Printf("[FindAlternativeVersion] Skipping function version: %s with memory: %dMB, CPU: %d cores, as it does not meet the requested resources",
					fn.Name, memory, cpu)
				continue // Skip this function if it does not meet the requested resources
			}
			// Calculate the score based on the difference between requested and available resources
			// SA - Use a custom scoring function to score the alternative function version
			// The score is calculated based on the difference between requested and available resources
			// The lower the score, the better the alternative function version
			// score = abs(memory - requestedMemory) + abs(cpu - requestedCPU)
			score := ScorePodAlternative(memory, requestedMemory, cpu, requestedCPU)

			matchedFunctions = append(matchedFunctions, struct {
				Function types.FunctionStatus
				MemoryMB uint64
				CPU      uint64
				Score    uint64
			}{
				Function: fn,
				MemoryMB: memory,
				CPU:      cpu,
				Score:    score,
			})
		}
	}

	if len(matchedFunctions) == 0 {
		log.Printf("[FindAlternativeVersion] No alternative function version found for %s in namespace %s", serviceName, serviceNamespace)
		// return matchedFunctions, fmt.Errorf("[FindAlternativeVersion] no alternative function version found for %s in namespace %s",
		// serviceName, serviceNamespace)
		log.Printf("[FindAlternativeVersion] [%s.%s] took: %.4fs", serviceName, serviceNamespace, time.Since(timeStart).Seconds())
		return nil, nil // No alternative function version found, return nil
	}
	// Sort matched functions by score (lower is better)
	sort.Slice(matchedFunctions, func(i, j int) bool {
		return matchedFunctions[i].Score < matchedFunctions[j].Score
	})
	log.Printf("[FindAlternativeVersion] [%s.%s] took: %.4fs", serviceName, serviceNamespace, time.Since(timeStart).Seconds())
	return matchedFunctions, nil
}

func extractMemoryAndCPU(fn types.FunctionStatus) (uint64, uint64) {
	var memory, cpu uint64
	// Extract memory and CPU from the function requests and limits
	// Check for the format of memory and CPU in the function requests and limits
	// If the memory is in the form "512Mi" or "1Gi", we need to convert it to Mi
	// If the CPU is in the form "1000m" or "1", we need to convert it to millicores

	if fn.Requests != nil {
		// Check for memory in the function requests
		if strings.HasSuffix(fn.Requests.Memory, "Mi") {
			memory, _ = strconv.ParseUint(strings.TrimSuffix(fn.Requests.Memory, "Mi"), 10, 64)
		} else if strings.HasSuffix(fn.Requests.Memory, "Gi") {
			memory, _ = strconv.ParseUint(strings.TrimSuffix(fn.Requests.Memory, "Gi"), 10, 64)
			memory *= 1024 // Convert Gi to Mi
		} else {
			memory, _ = strconv.ParseUint(fn.Requests.Memory, 10, 64)
		}
		// Check for CPU in the function requests
		if strings.HasSuffix(fn.Requests.CPU, "m") {
			cpu, _ = strconv.ParseUint(strings.TrimSuffix(fn.Requests.CPU, "m"), 10, 64)
		} else if strings.HasSuffix(fn.Requests.CPU, "cores") {
			cpu, _ = strconv.ParseUint(strings.TrimSuffix(fn.Requests.CPU, "cores"), 10, 64)
			cpu *= 1000 // Convert cores to millicores
		} else {
			cpu, _ = strconv.ParseUint(fn.Requests.CPU, 10, 64)
		}
	}
	if memory == 0 && fn.Limits != nil {
		if strings.HasSuffix(fn.Limits.Memory, "Mi") {
			memory, _ = strconv.ParseUint(strings.TrimSuffix(fn.Limits.Memory, "Mi"), 10, 64)
		} else if strings.HasSuffix(fn.Limits.Memory, "Gi") {
			memory, _ = strconv.ParseUint(strings.TrimSuffix(fn.Limits.Memory, "Gi"), 10, 64)
			memory *= 1024 // Convert Gi to Mi
		} else {
			memory, _ = strconv.ParseUint(fn.Limits.Memory, 10, 64)
		}
	}
	if cpu == 0 && fn.Limits != nil {
		if strings.HasSuffix(fn.Limits.CPU, "m") {
			cpu, _ = strconv.ParseUint(strings.TrimSuffix(fn.Limits.CPU, "m"), 10, 64)
		} else if strings.HasSuffix(fn.Limits.CPU, "cores") {
			cpu, _ = strconv.ParseUint(strings.TrimSuffix(fn.Limits.CPU, "cores"), 10, 64)
			cpu *= 1000 // Convert cores to millicores
		} else {
			cpu, _ = strconv.ParseUint(fn.Limits.CPU, 10, 64)
		}
	}
	// If memory or CPU is still 0, set it to a default value
	if memory == 0 {
		memory = 512 // Default to 512 MiB if not specified
	}
	if cpu == 0 {
		cpu = 1000 // Default to 1 core (1000 millicores) if not specified
	}
	log.Printf("[extractMemoryAndCPU] Extracted memory: %d MiB, CPU: %d millicores from function: %s",
		memory, cpu, fn.Name)
	// Return the extracted memory and CPU values
	return memory, cpu
}

// Helper function to get the function list
func (s *ExternalServiceQuery) GetFunctionList(serviceNamespace string) ([]types.FunctionStatus, error) {
	var functions []types.FunctionStatus

	// SA - Get the list of functions from the external service
	key := fmt.Sprintf("%s-%s", "GetFunctionList", serviceNamespace)
	result, err, _ := s.deployGroup.Do(key, func() (interface{}, error) {
		// Use singleflight to ensure that only one request is processed at a time
		listURL := fmt.Sprintf("%ssystem/functions?namespace=%s", s.URL.String(), serviceNamespace)
		listReq, err := http.NewRequest(http.MethodGet, listURL, nil)
		if err != nil {
			return functions, err
		}
		if s.AuthInjector != nil {
			s.AuthInjector.Inject(listReq)
		}
		listRes, err := s.ProxyClient.Do(listReq)
		if err != nil {
			log.Println(listURL, err)
			return functions, err
		}
		defer listRes.Body.Close()

		if listRes.StatusCode == http.StatusOK {
			listBytes, _ := io.ReadAll(listRes.Body)
			if err := json.Unmarshal(listBytes, &functions); err != nil {
				log.Printf("[GetFunctionList] Unable to unmarshal function list: %q, %s", string(listBytes), err)
				return functions, err
			}
		}

		return functions, nil
	})
	if err != nil {
		log.Printf("[GetFunctionList] Error getting function list: %v", err)
		return functions, err
	}
	return result.([]types.FunctionStatus), nil

}

// SA - DeployFunctionWithResources creates a new function deployment with specified resources
// Add these as package-level variables at the top of the file
var (
	versionRegex = regexp.MustCompile(`^(.+)-(\d+)-(\d+)$`)
)

func (s *ExternalServiceQuery) DeployFunctionWithResources(serviceName, serviceNamespace string,
	functions []types.FunctionStatus) (scaling.ServiceQueryResponse, error) {
	timeStart := time.Now()
	var emptyServiceQueryResponse scaling.ServiceQueryResponse

	// Use singleflight to ensure that only one deployment request is processed at a time
	key := fmt.Sprintf("%s-%s-%s", "DeployFunctionWithResources", serviceName, serviceNamespace)
	result, err, _ := s.deployGroup.Do(key, func() (interface{}, error) {
		// Look for a function with the same base name and a version suffix
		// Extract the base service name from the function name
		var requestedMemory, requestedCPU int
		baseName := serviceName     // Default to the original service name
		originalName := serviceName // Store the original name for annotations
		
		// Use pre-compiled regex
		matches := versionRegex.FindStringSubmatch(serviceName)

		// fix the regex to match the function name with memory and CPU
		// e.g., function-Name-512-1000
		if len(matches) == 4 {
			// Deployment follows naming convention
			baseName = matches[1]
			requestedMemory, _ = strconv.Atoi(matches[2])
			requestedCPU, _ = strconv.Atoi(matches[3])
		}
		
		log.Printf("[DeployFunctionWithResources] Base service name: %s", baseName)

		if baseName != serviceName && requestedMemory > 0 && requestedCPU > 0 {
			log.Printf("[DeployFunctionWithResources] Deploying new function version with memory=%dMB, CPU=%d cores",
				requestedMemory, requestedCPU)
		}
		
		// 1. Get the original function definition (if it exists)
		var originalFunction types.FunctionStatus
		// Optimize function search by creating pattern once
		pattern := fmt.Sprintf(`^%s-\d+-\d+$`, regexp.QuoteMeta(baseName))
		re := regexp.MustCompile(pattern)

		// Use map for faster lookup if functions list is large
		for _, fn := range functions {
			if fn.Name == baseName {
				continue // skip the original function
			}
			if re.MatchString(fn.Name) && fn.AvailableReplicas > 0 {
				// If we found a function with the same base name, use its configuration
				originalFunction = fn
				log.Printf("[DeployFunctionWithResources] Found existing function with base name %s: %s", baseName, fn.Name)
				break
			}
		}

		// 2. Create a new function with updated resources
		newFunctionName := fmt.Sprintf("%s-%d-%d", baseName, requestedMemory, requestedCPU)

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
				Memory: fmt.Sprintf("%dMi", requestedMemory),
				CPU:    fmt.Sprintf("%dm", requestedCPU), // CPU in millicores
			},
			Requests: &types.FunctionResources{
				Memory: fmt.Sprintf("%dMi", requestedMemory),
				CPU:    fmt.Sprintf("%dm", requestedCPU), // CPU to millicores
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
			log.Printf("[DeployFunctionWithResources] Error marshalling deploy request: %v", err)
			return emptyServiceQueryResponse, err
		}

		deployURL := fmt.Sprintf("%ssystem/functions", s.URL.String())
		deployReqRes, err := http.NewRequest(http.MethodPost, deployURL, bytes.NewReader(deployBody))
		if err != nil {
			log.Printf("[DeployFunctionWithResources] Error creating deploy request: %v", err)
			return emptyServiceQueryResponse, err
		}

		if s.AuthInjector != nil {
			s.AuthInjector.Inject(deployReqRes)
		}

		deployRes, err := s.ProxyClient.Do(deployReqRes)
		if err != nil {
			return emptyServiceQueryResponse, err
		}
		defer deployRes.Body.Close()

		if deployRes.StatusCode != http.StatusOK && deployRes.StatusCode != http.StatusAccepted {
			body, _ := io.ReadAll(deployRes.Body)
			return emptyServiceQueryResponse, fmt.Errorf("[DeployFunctionWithResources] Failed to deploy function: %s, status: %d, body: %s",
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
				"base_function":      baseName,
				"memory_mb":          fmt.Sprintf("%d", requestedMemory),
				"cpu_cores":          fmt.Sprintf("%d", requestedCPU),
			},
		}
		return resp, nil
	})
	if err != nil {
		log.Printf("[DeployFunctionWithResources] Error deploying function with resources: %v", err)
		return emptyServiceQueryResponse, err
	}
	log.Printf("[DeployFunctionWithResources] [%s.%s] took: %.4fs", serviceName, serviceNamespace, time.Since(timeStart).Seconds())
	// Return the result from the singleflight group
	return result.(scaling.ServiceQueryResponse), nil
}
