// License: OpenFaaS Community Edition (CE) EULA
// Copyright (c) 2017,2019-2024 OpenFaaS Author(s)

// Copyright (c) Alex Ellis 2017. All rights reserved.

// README: Alert Handler for OpenFaaS Gateway
//
// This file implements the handler for Prometheus Alertmanager webhooks and synthetic alerts for scaling OpenFaaS functions.
// To use or modify this component, follow these instructions:
//
// 1. Integration:
//    - Register the handler in your main.go with:
//        r.HandleFunc("/system/alert", handlers.MakeAlertHandler(...)).Methods(http.MethodPost)
//
// 2. Alert Format:
//    - The handler expects a JSON body matching the requests.PrometheusAlert struct.
//    - Alerts should include at least "alertname" and "function_name" in the labels.
//    - To trigger a specific replica count, set the "desired_replicas" field in the alert labels (as *int32).
//
// 3. Authentication:
//    - If your gateway is protected by Basic Auth, ensure that POST requests to /system/alert include the correct credentials.
//    - For synthetic alerts (e.g., from error_based_scaling.go), set credentials using req.SetBasicAuth("admin", "yourpassword").
//
// 4. Scaling Logic:
//    - If "desired_replicas" is set in the alert, the handler will scale the function to that count.
//    - If not set, the handler uses CalculateReplicas() to determine the new replica count based on alert status and scaling factors.
//
// 5. Error Handling:
//    - All errors in alert parsing or scaling are logged and returned as HTTP 400 or 500 responses.
//
// 6. Customization:
//    - You can extend the alert labels or annotations as needed for your use case.
//    - To support more flexible label handling, consider using map[string]string for labels in the alert struct.
//
// CHANGES MADE BY SA:
// - Added support for "desired_replicas" field in PrometheusInnerAlertLabel to allow custom scaling via alerts.
// - Updated scaleService to use the desired replica count from the alert if provided.
// - Updated README instructions to reflect the new custom scaling capability.
//
// Place these instructions at the top of this file for quick reference and onboarding.
//
// --- End README ---

package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"

	"github.com/openfaas/faas/gateway/pkg/middleware"
	"github.com/openfaas/faas/gateway/requests"
	"github.com/openfaas/faas/gateway/scaling"
)

// MakeAlertHandler handles alerts from Prometheus Alertmanager
func MakeAlertHandler(service scaling.ServiceQuery, defaultNamespace string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		if r.Body == nil {
			http.Error(w, "A body is required for this endpoint", http.StatusBadRequest)
			return
		}

		defer r.Body.Close()

		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("Unable to read alert."))

			log.Println(err)
			return
		}

		var req requests.PrometheusAlert
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("Unable to parse alert, bad format."))
			log.Println(err)
			return
		}

		errors := handleAlerts(req, service, defaultNamespace)
		if len(errors) > 0 {
			log.Println(errors)
			var errorOutput string
			for d, err := range errors {
				errorOutput += fmt.Sprintf("[%d] %s\n", d, err)
			}
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(errorOutput))
			return
		}

		w.WriteHeader(http.StatusOK)
	}
}

func handleAlerts(req requests.PrometheusAlert, service scaling.ServiceQuery, defaultNamespace string) []error {
	var errors []error
	for _, alert := range req.Alerts {
		if err := scaleService(alert, service, defaultNamespace); err != nil {
			log.Println(err)
			errors = append(errors, err)
		}
	}

	return errors
}

func scaleService(alert requests.PrometheusInnerAlert, service scaling.ServiceQuery, defaultNamespace string) error {
	var err error

	serviceName, namespace := middleware.GetNamespace(defaultNamespace, alert.Labels.FunctionName)

	if len(serviceName) > 0 {
		queryResponse, getErr := service.GetReplicas(serviceName, namespace)
		if getErr == nil {
			status := alert.Status
			// SA - Check if the alert has a specific replica count specified
			var newReplicas uint64
			if alert.Labels.DesiredReplicas != nil {
				// SA - Use the desired replicas from the alert
				newReplicas = uint64(*alert.Labels.DesiredReplicas)
				log.Printf("[ScaleCustom] function=%s %d => %d (using desired replicas).\n",
					serviceName, queryResponse.Replicas, newReplicas)
			} else {
				// Use the standard calculation if no desired replicas are specified
				newReplicas = CalculateReplicas(status, queryResponse.Replicas, uint64(queryResponse.MaxReplicas), queryResponse.MinReplicas, queryResponse.ScalingFactor)

				log.Printf("[Scale] function=%s %d => %d.\n", serviceName, queryResponse.Replicas, newReplicas)
				if newReplicas == queryResponse.Replicas {
					return nil
				}
			}

			updateErr := service.SetReplicas(serviceName, namespace, newReplicas)
			if updateErr != nil {
				err = updateErr
			}
		}
	}
	return err
}

// CalculateReplicas decides what replica count to set depending on current/desired amount
func CalculateReplicas(status string, currentReplicas uint64, maxReplicas uint64, minReplicas uint64, scalingFactor uint64) uint64 {
	var newReplicas uint64

	maxReplicas = uint64(math.Min(float64(maxReplicas), float64(scaling.DefaultMaxReplicas)))
	step := uint64(math.Ceil(float64(maxReplicas) / 100 * float64(scalingFactor)))

	if status == "firing" && step > 0 {
		if currentReplicas+step > maxReplicas {
			newReplicas = maxReplicas
		} else {
			newReplicas = currentReplicas + step
		}
	} else { // Resolved event.
		newReplicas = minReplicas
	}

	return newReplicas
}

// SA - Updated scaleService function to handle custom scaling logic
// Modify the scaleService function in alerthandler.go
// 	"strconv"  // Add this import

// func scaleService(alert requests.PrometheusInnerAlert, service scaling.ServiceQuery, defaultNamespace string) error {
// 	var err error

// 	serviceName, namespace := middleware.GetNamespace(defaultNamespace, alert.Labels.FunctionName)

// 	if len(serviceName) > 0 {
// 		queryResponse, getErr := service.GetReplicas(serviceName, namespace)
// 		if getErr == nil {
// 			status := alert.Status

// 			// Check if the alert has a specific replica count specified
// 			var newReplicas uint64
// 			if replicaStr, exists := alert.Labels["replicas"]; exists {
// 				// Parse the replica count from the label
// 				if parsedReplicas, parseErr := strconv.ParseUint(replicaStr, 10, 64); parseErr == nil {
// 					newReplicas = parsedReplicas
// 					log.Printf("[Scale] function=%s %d => %d (using specified replicas).\n",
// 						serviceName, queryResponse.Replicas, newReplicas)
// 				} else {
// 					// Fall back to calculated replicas if parsing fails
// 					newReplicas = CalculateReplicas(status, queryResponse.Replicas, uint64(queryResponse.MaxReplicas),
// 						queryResponse.MinReplicas, queryResponse.ScalingFactor)
// 					log.Printf("[Scale] function=%s %d => %d (calculated, parse error: %s).\n",
// 						serviceName, queryResponse.Replicas, newReplicas, parseErr)
// 				}
// 			} else {
// 				// Use the standard calculation if no replica count is specified
// 				newReplicas = CalculateReplicas(status, queryResponse.Replicas, uint64(queryResponse.MaxReplicas),
// 					queryResponse.MinReplicas, queryResponse.ScalingFactor)
// 				log.Printf("[Scale] function=%s %d => %d (calculated).\n",
// 					serviceName, queryResponse.Replicas, newReplicas)
// 			}

// 			// Ensure we don't exceed max replicas
// 			if newReplicas > uint64(queryResponse.MaxReplicas) {
// 				newReplicas = uint64(queryResponse.MaxReplicas)
// 			}

// 			// Ensure we don't go below min replicas
// 			if newReplicas < queryResponse.MinReplicas {
// 				newReplicas = queryResponse.MinReplicas
// 			}

// 			if newReplicas == queryResponse.Replicas {
// 				return nil
// 			}

// 			updateErr := service.SetReplicas(serviceName, namespace, newReplicas)
// 			if updateErr != nil {
// 				err = updateErr
// 			}
// 		}
// 	}
// 	return err
// }
