// handlers/error_based_scaling.go
package handlers

import (
	"context"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/openfaas/faas/gateway/metrics"
	"github.com/openfaas/faas/gateway/scaling"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ErrorBasedScaler checks for non-200 responses and scales functions accordingly
type ErrorBasedScaler struct {
	PrometheusQuery  metrics.PrometheusQueryFetcher
	Scaler           scaling.FunctionScaler
	KubeClient       kubernetes.Interface
	Interval         time.Duration
	ErrorThreshold   int
	DefaultNamespace string
	MaxReplicas      int32
}

// Start begins periodic checking for functions with high error rates
func (e *ErrorBasedScaler) Start() {
	ticker := time.NewTicker(e.Interval)
	go func() {
		for {
			<-ticker.C
			e.checkAndScale()
		}
	}()
}

// checkAndScale queries Prometheus for non-200 responses and scales functions if needed
func (e *ErrorBasedScaler) checkAndScale() {
	query := `sum(increase(gateway_function_non_200_responses_total[1m])) by (function_name)`
	results, err := e.PrometheusQuery.Fetch(query)
	if err != nil {
		log.Printf("Error querying Prometheus for error rates: %s", err)
		return
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
		if int(errorCount) > e.ErrorThreshold {
			namespace := e.DefaultNamespace

			// First ensure function is scaled from zero if needed
			res := e.Scaler.Scale(functionName, namespace)
			if !res.Found {
				log.Printf("Function %s.%s not found", functionName, namespace)
				continue
			}

			// Now scale up based on error count
			err = e.scaleDeployment(functionName, namespace, int(errorCount))
			if err != nil {
				log.Printf("Error scaling deployment for %s.%s: %s", functionName, namespace, err)
			}
		}
	}
}

// scaleDeployment scales a deployment based on error count
func (e *ErrorBasedScaler) scaleDeployment(functionName, namespace string, errorCount int) error {
	// Get current deployment
	deployment, err := e.KubeClient.AppsV1().Deployments(namespace).Get(
		context.Background(),
		functionName,
		metav1.GetOptions{},
	)

	if err != nil {
		return err
	}

	// Calculate new replica count: current + errors/2
	currentReplicas := *deployment.Spec.Replicas
	additionalReplicas := int32(math.Ceil(float64(errorCount) / 2.0))
	newReplicas := currentReplicas + additionalReplicas

	// Cap at max replicas
	if newReplicas > e.MaxReplicas {
		newReplicas = e.MaxReplicas
	}

	// Only update if we need to scale up
	if newReplicas > currentReplicas {
		log.Printf("Scaling %s.%s from %d to %d replicas based on %d non-200 responses",
			functionName, namespace, currentReplicas, newReplicas, errorCount)

		deployment.Spec.Replicas = &newReplicas

		_, err = e.KubeClient.AppsV1().Deployments(namespace).Update(
			context.Background(),
			deployment,
			metav1.UpdateOptions{},
		)

		if err != nil {
			return err
		}

		log.Printf("Successfully scaled %s.%s to %d replicas", functionName, namespace, newReplicas)
	}

	return nil
}

// MakeErrorBasedScalingHandler creates an HTTP handler for manual triggering of error-based scaling
func MakeErrorBasedScalingHandler(scaler *ErrorBasedScaler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		scaler.checkAndScale()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Error-based scaling check triggered"))
	}
}



// in the main package, where you set up your HTTP server and other components
// In your imports section
import (
    // ... existing imports
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/rest"
)

// In your main function, after setting up other components
// Get Kubernetes client
config, err := rest.InClusterConfig()
if err != nil {
    log.Fatalf("Error getting Kubernetes config: %s", err)
}

kubeClient, err := kubernetes.NewForConfig(config)
if err != nil {
    log.Fatalf("Error creating Kubernetes client: %s", err)
}

// Initialize the error-based scaler
errorBasedScaler := &handlers.ErrorBasedScaler{
    PrometheusQuery:  prometheusQuery,
    Scaler:           functionScaler,
    KubeClient:       kubeClient,
    Interval:         time.Second * 30,
    ErrorThreshold:   10,
    DefaultNamespace: config.Namespace,
    MaxReplicas:      20,
}

// Start the periodic scaling checks
errorBasedScaler.Start()

// Add an endpoint to manually trigger the scaling check
r.HandleFunc("/system/scale-on-errors", handlers.MakeErrorBasedScalingHandler(errorBasedScaler)).Methods(http.MethodPost)



// ALternate approach using the fake prometheus alert generator for re-using the existing alert handler
// handlers/error_scaling.go
// handlers/error_based_scaling.go
package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"time"

	"github.com/openfaas/faas/gateway/metrics"
	"github.com/openfaas/faas/gateway/requests"
)

// errorBasedScaler checks for non-200 responses and triggers scaling via alerts
// type errorBasedScaler struct {
// 	prometheusQuery  metrics.PrometheusQueryFetcher
// 	alertHandler     http.HandlerFunc
// 	interval         time.Duration
// 	errorThreshold   int
// 	defaultNamespace string
// 	maxReplicas      int
// }

// errorBasedScaler checks for non-200 responses and triggers scaling via alerts
type errorBasedScaler struct {
	prometheusQuery    metrics.PrometheusQueryFetcher
	alertHandler       http.HandlerFunc
	interval           time.Duration
	errorThreshold     int
	scaleDownThreshold int
	defaultNamespace   string
	maxReplicas        int
}


// start begins periodic checking for functions with high error rates
// func (e *errorBasedScaler) start() {
// 	ticker := time.NewTicker(e.interval)
// 	go func() {
// 		for {
// 			<-ticker.C
// 			e.checkAndScale()
// 		}
// 	}()
// }

// start begins periodic checking for functions with high/low error rates
func (e *errorBasedScaler) start() {
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
func (e *errorBasedScaler) checkAndScale() {
	query := `sum(increase(gateway_function_non_200_responses_total[1m])) by (function_name)`
	results, err := e.prometheusQuery.Fetch(query)
	if err != nil {
		log.Printf("Error querying Prometheus for error rates: %s", err)
		return
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
			// Create an alert to trigger scaling
			alert := e.createErrorAlert(functionName, int(errorCount))
			
			// Send the alert to the alert handler
			e.triggerAlert(alert)
		}
	}
}

// createErrorAlert creates a PrometheusAlert for the given function
func (e *errorBasedScaler) createErrorAlert(functionName string, errorCount int) requests.PrometheusAlert {
	// Calculate desired replicas based on error count
	desiredReplicas := int(math.Ceil(float64(errorCount) / 2.0))
	if desiredReplicas < 1 {
		desiredReplicas = 1
	}
	
	// Cap at max replicas
	if desiredReplicas > e.maxReplicas {
		desiredReplicas = e.maxReplicas
	}
	
	return requests.PrometheusAlert{
		Status: "firing",
		Alerts: []requests.PrometheusInnerAlert{
			{
				Status: "firing",
				Labels: map[string]string{
					"alertname":     "HighErrorRate",
					"function_name": functionName,
					"code":          "non-200",
					"severity":      "major",
					"service":       "gateway",
					"replicas":      fmt.Sprintf("%d", desiredReplicas), // Include desired replicas
				},
				Annotations: map[string]string{
					"description": fmt.Sprintf("High error rate detected for %s: %d non-200 responses", 
						functionName, errorCount),
					"summary": fmt.Sprintf("Function %s has high error rate", functionName),
				},
			},
		},
	}
}

// triggerAlert sends an alert to the alert handler
func (e *errorBasedScaler) triggerAlert(alert requests.PrometheusAlert) {
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
			alert.Alerts[0].Labels["function_name"], rec.Code, rec.Body.String())
	} else {
		log.Printf("Successfully triggered scaling for %s to %s replicas due to high error rate", 
			alert.Alerts[0].Labels["function_name"], alert.Alerts[0].Labels["replicas"])
	}
}


// Add this method to the errorBasedScaler struct

// checkAndScaleDown queries Prometheus for non-200 responses and scales down if they're below threshold
func (e *errorBasedScaler) checkAndScaleDown() {
	query := `sum(increase(gateway_function_non_200_responses_total[1m])) by (function_name)`
	results, err := e.prometheusQuery.Fetch(query)
	if err != nil {
		log.Printf("Error querying Prometheus for error rates: %s", err)
		return
	}

	// Also get current replica counts for all functions
	replicaQuery := `sum(gateway_service_count) by (function_name)`
	replicaResults, err := e.prometheusQuery.Fetch(replicaQuery)
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

// createScaleDownAlert creates a PrometheusAlert for scaling down the given function
func (e *errorBasedScaler) createScaleDownAlert(functionName string, errorCount, currentReplicas int) requests.PrometheusAlert {
	// Calculate desired replicas: current / 2, but at least 1
	desiredReplicas := int(math.Max(1, float64(currentReplicas)/2))
	
	return requests.PrometheusAlert{
		Status: "firing",
		Alerts: []requests.PrometheusInnerAlert{
			{
				Status: "firing",
				Labels: map[string]string{
					"alertname":     "LowErrorRate",
					"function_name": functionName,
					"code":          "non-200",
					"severity":      "minor",
					"service":       "gateway",
					"replicas":      fmt.Sprintf("%d", desiredReplicas), // Include desired replicas
				},
				Annotations: map[string]string{
					"description": fmt.Sprintf("Low error rate detected for %s: %d non-200 responses, scaling down from %d to %d replicas", 
						functionName, errorCount, currentReplicas, desiredReplicas),
					"summary": fmt.Sprintf("Function %s has low error rate, scaling down", functionName),
				},
			},
		},
	}
}



// MakeErrorBasedScalingHandler creates an HTTP handler for error-based scaling
// func MakeErrorBasedScalingHandler(prometheusQuery metrics.PrometheusQueryFetcher, 
//                                  alertHandler http.HandlerFunc,
//                                  namespace string,
//                                  errorThreshold int,
//                                  maxReplicas int) http.HandlerFunc {
    
//     // Create the scaler instance
//     scaler := &errorBasedScaler{
//         prometheusQuery:  prometheusQuery,
//         alertHandler:     alertHandler,
//         interval:         time.Second * 30,
//         errorThreshold:   errorThreshold,
//         defaultNamespace: namespace,
//         maxReplicas:      maxReplicas,
//     }
    
//     // Start the periodic checks in a goroutine
//     go scaler.start()
    
//     // Return a handler function for manual triggering
//     return func(w http.ResponseWriter, r *http.Request) {
//         if r.Method != http.MethodPost {
//             w.WriteHeader(http.StatusMethodNotAllowed)
//             return
//         }

//         scaler.checkAndScale()
//         w.WriteHeader(http.StatusOK)
//         w.Write([]byte("Error-based scaling check triggered"))
//     }
// }

// MakeErrorBasedScalingHandler creates an HTTP handler for error-based scaling
func MakeErrorBasedScalingHandler(prometheusQuery metrics.PrometheusQueryFetcher, 
                                 alertHandler http.HandlerFunc,
                                 namespace string,
                                 errorThreshold int,
                                 scaleDownThreshold int,
                                 maxReplicas int) http.HandlerFunc {
    
    // Create the scaler instance
    scaler := &errorBasedScaler{
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


// in the main package, where you set up your HTTP server and other components
// Create the alert handler first
alertHandler := handlers.MakeAlertHandler(externalServiceQuery, config.Namespace)

// Add the error-based scaling handler that uses the alert handler
// r.HandleFunc("/system/scale-on-errors", 
//     handlers.MakeErrorBasedScalingHandler(
//         prometheusQuery,
//         alertHandler,
//         config.Namespace,
//         10, // errorThreshold
//         20, // maxReplicas
//     )).Methods(http.MethodPost)
// Add the error-based scaling handler that uses the alert handler
r.HandleFunc("/system/scale-on-errors", 
    handlers.MakeErrorBasedScalingHandler(
        prometheusQuery,
        alertHandler,
        config.Namespace,
        10,  // errorThreshold for scaling up
        2,   // scaleDownThreshold
        20,  // maxReplicas
    )).Methods(http.MethodPost)


// Register the regular alert handler as well
r.HandleFunc("/system/alert", alertHandler).Methods(http.MethodPost)


// In metrics/metrics.go
type MetricOptions struct {
    // Existing metrics...
    
    // New metric for tracking non-200 responses
    GatewayFunctionNon200Responses *prometheus.CounterVec
}

// Initialize in NewMetricOptions
opts.GatewayFunctionNon200Responses = prometheus.NewCounterVec(
    prometheus.CounterOpts{
        Namespace: "gateway",
        Name:      "function_non_200_responses_total",
        Help:      "Total of non-200 responses from functions",
    },
    []string{"function_name", "code"},
)
prometheus.MustRegister(opts.GatewayFunctionNon200Responses)


// In handlers/notifiers.go, update the Notify method
func (p PrometheusFunctionNotifier) Notify(requestID string, method string, URL string, originalURL string, statusCode string, event string, duration time.Duration) {
    serviceName := middleware.GetServiceName(originalURL)
    if len(p.FunctionNamespace) > 0 {
        if !strings.Contains(serviceName, ".") {
            serviceName = fmt.Sprintf("%s.%s", serviceName, p.FunctionNamespace)
        }
    }

    parts := strings.Split(statusCode, "+")
    code := parts[0]
    labels := prometheus.Labels{"function_name": serviceName, "code": code}

    if event == "completed" {
        seconds := duration.Seconds()
        p.Metrics.GatewayFunctionsHistogram.
            With(labels).
            Observe(seconds)

        p.Metrics.GatewayFunctionInvocation.
            With(labels).
            Inc()
            
        // Track non-200 responses
        if code != "200" {
            p.Metrics.GatewayFunctionNon200Responses.
                With(labels).
                Inc()
        }
    } else if event == "started" {
        p.Metrics.GatewayFunctionInvocationStarted.WithLabelValues(serviceName).Inc()
    }
}
