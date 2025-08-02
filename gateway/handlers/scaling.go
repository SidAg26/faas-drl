// License: OpenFaaS Community Edition (CE) EULA
// Copyright (c) 2017,2019-2024 OpenFaaS Author(s)

// Copyright (c) OpenFaaS Author(s). All rights reserved.

package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/openfaas/faas/gateway/pkg/middleware"
	"github.com/openfaas/faas/gateway/scaling"
)

// MakeScalingHandler creates handler which can scale a function from
// zero to N replica(s). After scaling the next http.HandlerFunc will
// be called. If the function is not ready after the configured
// amount of attempts / queries then next will not be invoked and a status
// will be returned to the client.
func MakeScalingHandler(next http.HandlerFunc, scaler scaling.FunctionScaler, config scaling.ScalingConfig, defaultNamespace string) http.HandlerFunc {

	return func(w http.ResponseWriter, r *http.Request) {

		functionName, namespace := middleware.GetNamespace(defaultNamespace, middleware.GetServiceName(r.URL.String()))

		// SA - Check for the MemFigLess prediction for the function
		body, err := io.ReadAll(r.Body)
		if err != nil {
			log.Printf("Error reading request body: %s\n", err)
			http.Error(w, "Internal Server Error while reading request Payload", http.StatusInternalServerError)
			return
		}
		defer r.Body.Close()

		// Create a new reader with the same body for downstream handlers
		// This is necessary because the body can only be read once
		// and we need to pass it to the next handler.
		// This allows us to read the body again in the next handler.
		// SA - This is necessary for the MemFigLess prediction to work
		// as it needs to read the request body to extract the input parameters.
		r.Body = io.NopCloser(bytes.NewBuffer(body))

		// Extract base function name and resource requirements
		baseName := extractBaseFunctionName(functionName)
		configName, memory, cpu, err := getMemFigLessPrediction(baseName, body)
		if err != nil {
			// SA - A Fallback mechanism to handle prediction errors
			// If we fail to get the prediction, we log the error and continue
			// without modifying the function name.
			// This allows the request to proceed with the original function name.
			log.Printf("Error getting MemFigLess prediction: %s, Trying [BESTEFFORT] with %s\n", err, functionName)
			// http.Error(w, "Internal Server Error while getting MemFigLess prediction", http.StatusInternalServerError)
			// return
		}
		if err == nil && configName != "" {
			log.Printf("[ScalingMemFigLess] Using MemFigLess prediction for function %s: %s, memory: %d, cpu: %d\n",
				functionName, configName, memory, cpu)
			functionName = configName
			// Update the request context with the new function name
			r.URL.Path = fmt.Sprintf("/function/%s.%s", functionName, namespace)
			r.URL.RawPath = fmt.Sprintf("/function/%s.%s", functionName, namespace)
		}
		// SA - This scaler wraps the function proxy request and checks whether exists or not
		// before allowing the request to proceed. If the function is not found
		// or if it is not available, then we return an error to the client. If found, then
		// it checks and ensures that the function has minimumReplicas available
		// before allowing the request to proceed.
		// SA - This is where the logic for "Version Checking" needs to be added
		res := scaler.Scale(functionName, namespace)

		// SA - If any function version is matched, then we update the URL path
		// to point to the matched function version. This is useful for
		// routing requests to the correct function version.
		if res.Available && res.ServiceNameMatched != nil {
			log.Printf("[ScaleCustom] function=%s.%s available after %.4fs for the cuurent function=%s\n",
				*res.ServiceNameMatched, namespace, res.Duration.Seconds(), functionName)
			r.URL.Path = fmt.Sprintf("/function/%s.%s", *res.ServiceNameMatched, namespace)
			r.URL.RawPath = fmt.Sprintf("/function/%s.%s", *res.ServiceNameMatched, namespace)
		}

		if !res.Found {
			errStr := fmt.Sprintf("error finding function %s.%s: %s", functionName, namespace, res.Error.Error())
			log.Printf("Scaling: %s\n", errStr)

			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(errStr))
			return
		}

		if res.Error != nil {
			errStr := fmt.Sprintf("error finding function %s.%s: %s", functionName, namespace, res.Error.Error())
			log.Printf("Scaling: %s\n", errStr)

			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(errStr))
			return
		}

		if res.Available {
			if res.ServiceNameMatched != nil {
				r.URL.Path = fmt.Sprintf("/function/%s.%s", *res.ServiceNameMatched, namespace)
				r.URL.RawPath = fmt.Sprintf("/function/%s.%s", *res.ServiceNameMatched, namespace)
			}
			// log.Printf("[ScaleCustom] function request=%s", r)
			next.ServeHTTP(w, r)
			return
		}

		log.Printf("[Scale] function=%s.%s 0=>N timed-out after %.4fs\n",
			functionName, namespace, res.Duration.Seconds())
	}
}

func extractBaseFunctionName(functionName string) string {
	// Extract the base function name from the full function name
	versionRegex := regexp.MustCompile(`^(.+)-(\d+)-(\d+)$`)
	matches := versionRegex.FindStringSubmatch(functionName)

	if len(matches) == 4 {
		return matches[1] // Return the base name without memory and CPU
	}
	return functionName // Return the original name if no match
}

func getMemFigLessPrediction(functionName string, requestBody []byte) (string, uint64, uint64, error) {
	// Extract input parameters from the request body if possible
	var inputParams map[string]interface{}

	// Try to parse the request body as JSON
	if len(requestBody) > 0 {
		var requestData map[string]interface{}
		if err := json.Unmarshal(requestBody, &requestData); err == nil {
			inputParams = requestData
		}
	}

	// If parsing failed or body was empty, use an empty map
	if inputParams == nil {
		inputParams = make(map[string]interface{})
	}

	// Create the prediction request payload
	payload := map[string]interface{}{
		"function_name": functionName,
		"input_params":  inputParams,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", 0, 0, fmt.Errorf("failed to marshal prediction payload: %v", err)
	}

	// Create HTTP client
	client := &http.Client{Timeout: 5 * time.Second}

	// Create request with payload
	urlPath := "http://prediction-service.prediction-service.svc.cluster.local:8002/predict/single"
	req, err := http.NewRequest(http.MethodPost, urlPath, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return "", 0, 0, err
	}

	req.Header.Set("Content-Type", "application/json")

	// Send request
	res, err := client.Do(req)
	if err != nil {
		return "", 0, 0, fmt.Errorf("error making prediction request: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		return "", 0, 0, fmt.Errorf("prediction service returned non-200 status code (%d): %s",
			res.StatusCode, string(body))
	}

	// Parse response - the response is a map with a single key (config name)
	// and value containing cpu and mem
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", 0, 0, err
	}

	var predictionResponse map[string]map[string]uint64
	if err := json.Unmarshal(body, &predictionResponse); err != nil {
		return "", 0, 0, fmt.Errorf("failed to parse prediction response: %v", err)
	}

	// Extract the first (and only) key-value pair
	var configName string
	var cpu, memory uint64

	for name, resources := range predictionResponse {
		configName = name
		cpu = resources["cpu"]
		memory = resources["mem"]
		break // Only one entry expected
	}

	return configName, memory, cpu, nil
}
