// README: Error-Based Scaling Handler for OpenFaaS Gateway
//
// Developed by SA as an add-on to the original OpenFaaS codebase.
//
// This file implements error-based auto-scaling for OpenFaaS functions by periodically querying Prometheus for non-200 responses
// and triggering scaling actions via the alert handler. To use or modify this component, follow these instructions:
//
// 1. Prometheus Metrics Requirements:
//    - Your Prometheus must expose the following metrics with the correct labels:
//      * gateway_function_invocation_total{function_name, code} (for error rate detection)
//      * gateway_service_count{function_name} (for current replica counts)
//
// 2. Prometheus QueryFetcher Interface:
//    - Ensure you pass a PrometheusQueryFetcher implementation (e.g., *PrometheusQuery) to this handler.
//    - The Fetch method must accept a query string and return a VectorQueryResponse.
//
// 3. Query String Encoding:
//    - Queries are URL-escaped using url.QueryEscape before being sent to Prometheus.
//
// 4. Alert Handler Authentication:
//    - If your /system/alert endpoint is protected by Basic Auth, you must set credentials in the triggerAlert method.
//      Example:
//        req.SetBasicAuth("admin", "yourpassword") // Replace with your actual credentials
//    - If no authentication is required, you can remove this line.
//    - Else follow the handler creation in the main.go file to set up the alert handler and wrap the
//      authentication middleware.
//
// 5. Replica Calculation Logic:
//    - Scaling up: desiredReplicas = currentReplicas + ceil(errorCount/2), capped at maxReplicas.
//    - Scaling down: desiredReplicas = max(1, currentReplicas/2).
//    - The desired replica count is passed as a pointer to int32 in the alert struct.
//
// 6. Manual and Periodic Triggering:
//    - The handler runs both scale-up and scale-down checks every interval (default: 30s).
//    - You can also trigger checks manually via HTTP POST to /system/scale-on-errors.
//
// 7. Integration:
//    - Register the handler in your main.go with:
//        r.HandleFunc("/system/scale-on-errors", handlers.MakeErrorBasedScalingHandler(...)).Methods(http.MethodPost)
//
// 8. Error Handling:
//    - All Prometheus query and scaling errors are logged. No panics should occur if error checks are followed.
//
// 9. Customization:
//    - Adjust errorThreshold, scaleDownThreshold, maxReplicas, and interval as needed for your environment.
//
// 10. Example Prometheus Queries Used:
//      sum(increase(gateway_function_invocation_total{code!="200"}[30s])) by (function_name)
//      sum(gateway_service_count) by (function_name)
//
// Place these instructions at the top of this file for quick reference and onboarding.
//
// --- End README ---

// handlers/error_based_scaling.go
package handlers

import (
	"bytes"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url" // SA - Added url import for QueryEscape
	"strconv"
	"time"

	"github.com/openfaas/faas/gateway/metrics"
	"github.com/openfaas/faas/gateway/requests"
)

// ErrorBasedScaler checks for non-200 responses and triggers scaling via alerts
type ErrorBasedScaler struct {
	prometheusQuery    metrics.PrometheusQueryFetcher
	alertHandler       http.HandlerFunc
	interval           time.Duration
	errorThreshold     int
	scaleDownThreshold int
	defaultNamespace   string
	maxReplicas        int
}

// start begins periodic checking for functions with high/low error rates
func (e *ErrorBasedScaler) start() {
	ticker := time.NewTicker(e.interval)
	go func() {
		for {
			<-ticker.C
			// Check for scaling up
			e.checkAndScale()

			// Check for scaling down
			e.checkAndScaleDown()
		}
	}()
}

// checkAndScale queries Prometheus for non-200 responses and triggers scaling
func (e *ErrorBasedScaler) checkAndScale() {
	query := `sum(increase(gateway_function_invocation_total{code!="200"}[30s])) by (function_name)`
	results, err := e.prometheusQuery.Fetch(url.QueryEscape(query))
	if err != nil {
		log.Printf("Error querying Prometheus for error rates: %s", err)
		return
	}

	// Get current replica counts for all functions
	replicaQuery := `sum(gateway_service_count) by (function_name)`
	replicaResults, err := e.prometheusQuery.Fetch(url.QueryEscape(replicaQuery))
	if err != nil {
		log.Printf("Error querying Prometheus for replica counts: %s", err)
		return
	}

	// Create a map of function name to replica count
	replicaCounts := make(map[string]int)
	for _, result := range replicaResults.Data.Result {
		functionName := result.Metric.FunctionName
		valueStr := result.Value[1].(string)
		replicas, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			log.Printf("Error parsing replica count for %s: %s", functionName, err)
			continue
		}
		replicaCounts[functionName] = int(replicas)
	}

	for _, result := range results.Data.Result {
		functionName := result.Metric.FunctionName

		// Convert value from interface{} to float64
		valueStr := result.Value[1].(string)
		errorCount, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			log.Printf("Error parsing value for %s: %s", functionName, err)
			continue
		}

		// Only proceed if error count exceeds threshold
		if int(errorCount) > e.errorThreshold {
			// Get current replicas
			currentReplicas, exists := replicaCounts[functionName]
			if !exists {
				currentReplicas = 0
			}

			// Check if the current replica count is already at the maximum
			if currentReplicas >= e.maxReplicas {
				log.Printf("Function %s already at maximum replicas (%d)", functionName, currentReplicas)
				continue
			}

			// Create an alert to trigger scaling
			alert := e.createErrorAlert(functionName, int(errorCount), currentReplicas)

			// Send the alert to the alert handler
			e.triggerAlert(alert)
		}
	}
}

// createErrorAlert creates a PrometheusAlert for scaling up the given function based on error count
func (e *ErrorBasedScaler) createErrorAlert(functionName string, errorCount, currentReplicas int) requests.PrometheusAlert {
	// Calculate desired replicas based on error count and add to current replicas
	desiredReplicas := currentReplicas + int(math.Ceil(float64(errorCount)/2.0))

	// Cap at max replicas
	if desiredReplicas > e.maxReplicas {
		desiredReplicas = e.maxReplicas
	}

	// Ensure at least 1 replica
	if desiredReplicas < 1 {
		desiredReplicas = 1
	}
	// Create the alert with desired replicas as an int32 pointer
	desiredReplicasInt32 := int32(desiredReplicas)

	return requests.PrometheusAlert{
		Status: "firing",
		Alerts: []requests.PrometheusInnerAlert{
			{
				Status: "firing",
				Labels: requests.PrometheusInnerAlertLabel{
					AlertName:       "HighErrorRate",
					FunctionName:    functionName,
					DesiredReplicas: &(desiredReplicasInt32), // Include desired replicas
				},
			},
		},
	}
}

// triggerAlert sends an alert to the alert handler
func (e *ErrorBasedScaler) triggerAlert(alert requests.PrometheusAlert) {
	// Convert alert to JSON
	alertBytes, err := json.Marshal(alert)
	if err != nil {
		log.Printf("Error marshalling alert: %s", err)
		return
	}

	// Create a fake request to the alert handler
	req, err := http.NewRequest(http.MethodPost, "/system/alert",
		bytes.NewReader(alertBytes))
	if err != nil {
		log.Printf("Error creating request: %s", err)
		return
	}

	// Set appropriate headers
	req.Header.Set("Content-Type", "application/json")

	// Create a recorder to capture the response
	rec := httptest.NewRecorder()

	// Call the alert handler
	e.alertHandler(rec, req)

	// Check the response
	if rec.Code != http.StatusOK {
		log.Printf("Error triggering alert for %s: %d - %s",
			alert.Alerts[0].Labels.FunctionName, rec.Code, rec.Body.String())
	} else {
		log.Printf("Successfully triggered scaling for %s to %s replicas due to high error rate",
			alert.Alerts[0].Labels.FunctionName, "Not Defined") // Desired replicas not included in this alert
	}
}

// checkAndScaleDown queries Prometheus for non-200 responses and scales down if they're below threshold
func (e *ErrorBasedScaler) checkAndScaleDown() {
	query := `sum(increase(gateway_function_invocation_total{code!="200"}[30s])) by (function_name)`

	results, err := e.prometheusQuery.Fetch(url.QueryEscape(query))
	if err != nil {
		log.Printf("Error querying Prometheus for error rates: %s", err)
		return
	}

	// Also get current replica counts for all functions
	replicaQuery := `sum(gateway_service_count) by (function_name)`
	replicaResults, err := e.prometheusQuery.Fetch(url.QueryEscape(replicaQuery))
	if err != nil {
		log.Printf("Error querying Prometheus for replica counts: %s", err)
		return
	}

	// Create a map of function name to replica count
	replicaCounts := make(map[string]int)
	for _, result := range replicaResults.Data.Result {
		functionName := result.Metric.FunctionName
		valueStr := result.Value[1].(string)
		replicas, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			log.Printf("Error parsing replica count for %s: %s", functionName, err)
			continue
		}
		replicaCounts[functionName] = int(replicas)
	}

	// Check each function for low error rates
	for _, result := range results.Data.Result {
		functionName := result.Metric.FunctionName

		// Convert value from interface{} to float64
		valueStr := result.Value[1].(string)
		errorCount, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			log.Printf("Error parsing value for %s: %s", functionName, err)
			continue
		}

		// Only proceed if error count is below threshold and we have more than 1 replica
		currentReplicas, exists := replicaCounts[functionName]
		if !exists {
			continue // Skip if we don't know the current replica count
		}

		if int(errorCount) <= e.scaleDownThreshold && currentReplicas > 1 {
			// Create an alert to trigger scaling down
			alert := e.createScaleDownAlert(functionName, int(errorCount), currentReplicas)

			// Send the alert to the alert handler
			e.triggerAlert(alert)
		}
	}
}

// createScaleDownAlert creates a PrometheusAlert for scaling down the given function based on error count
func (e *ErrorBasedScaler) createScaleDownAlert(functionName string, errorCount, currentReplicas int) requests.PrometheusAlert {
	// Calculate desired replicas: current / 2, but at least 1
	desiredReplicas := int32(math.Max(1, float64(currentReplicas)/2))

	return requests.PrometheusAlert{
		Status: "firing",
		Alerts: []requests.PrometheusInnerAlert{
			{
				Status: "firing",
				Labels: requests.PrometheusInnerAlertLabel{
					AlertName:       "LowErrorRate",
					FunctionName:    functionName,
					DesiredReplicas: &desiredReplicas, // Include desired replicas
				},
			},
		},
	}
}

// MakeErrorBasedScalingHandler creates an HTTP handler for error-based scaling
func MakeErrorBasedScalingHandler(prometheusQuery metrics.PrometheusQueryFetcher,
	alertHandler http.HandlerFunc,
	namespace string,
	errorThreshold int,
	scaleDownThreshold int,
	maxReplicas int) http.HandlerFunc {

	// Create the scaler instance
	scaler := &ErrorBasedScaler{
		prometheusQuery:    prometheusQuery,
		alertHandler:       alertHandler,
		interval:           time.Second * 30,
		errorThreshold:     errorThreshold,
		scaleDownThreshold: scaleDownThreshold,
		defaultNamespace:   namespace,
		maxReplicas:        maxReplicas,
	}

	// Start the periodic checks in a goroutine
	go scaler.start()

	// Return a handler function for manual triggering
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		// Run both scale up and scale down checks
		scaler.checkAndScale()
		scaler.checkAndScaleDown()

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Error-based scaling check triggered"))
	}
}

// // In metrics/metrics.go
// type MetricOptions struct {
//     // Existing metrics...

//     // New metric for tracking non-200 responses
//     GatewayFunctionNon200Responses *prometheus.CounterVec
// }

// // Initialize in NewMetricOptions
// opts.GatewayFunctionNon200Responses = prometheus.NewCounterVec(
//     prometheus.CounterOpts{
//         Namespace: "gateway",
//         Name:      "function_non_200_responses_total",
//         Help:      "Total of non-200 responses from functions",
//     },
//     []string{"function_name", "code"},
// )
// prometheus.MustRegister(opts.GatewayFunctionNon200Responses)

// // In handlers/notifiers.go, update the Notify method
// func (p PrometheusFunctionNotifier) Notify(requestID string, method string, URL string, originalURL string, statusCode string, event string, duration time.Duration) {
//     serviceName := middleware.GetServiceName(originalURL)
//     if len(p.FunctionNamespace) > 0 {
//         if !strings.Contains(serviceName, ".") {
//             serviceName = fmt.Sprintf("%s.%s", serviceName, p.FunctionNamespace)
//         }
//     }

//     parts := strings.Split(statusCode, "+")
//     code := parts[0]
//     labels := prometheus.Labels{"function_name": serviceName, "code": code}

//     if event == "completed" {
//         seconds := duration.Seconds()
//         p.Metrics.GatewayFunctionsHistogram.
//             With(labels).
//             Observe(seconds)

//         p.Metrics.GatewayFunctionInvocation.
//             With(labels).
//             Inc()

//         // Track non-200 responses
//         if code != "200" {
//             p.Metrics.GatewayFunctionNon200Responses.
//                 With(labels).
//                 Inc()
//         }
//     } else if event == "started" {
//         p.Metrics.GatewayFunctionInvocationStarted.WithLabelValues(serviceName).Inc()
//     }
// }
