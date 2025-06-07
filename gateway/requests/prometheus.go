// License: OpenFaaS Community Edition (CE) EULA
// Copyright (c) 2017,2019-2024 OpenFaaS Author(s)

// Copyright (c) Alex Ellis 2017. All rights reserved.

// README: Prometheus Alert Types for OpenFaaS Gateway
//
// This file defines the types for Prometheus Alertmanager webhooks and synthetic alerts used for scaling in OpenFaaS.
// To use or modify this component, follow these instructions:
//
// 1. Alert Structure:
//    - Alerts must match the PrometheusAlert struct defined below.
//    - Each alert in the "alerts" array must have "alertname" and "function_name" in the labels.
//
// 2. Custom Fields (SA):
//    - The "desired_replicas" field in PrometheusInnerAlertLabel is optional and can be set to specify the target replica count for scaling.
//      // SA - DesiredReplicas is optional, so it can be nil
//
// 3. Extending Labels:
//    - To support more flexible or arbitrary labels, consider changing PrometheusInnerAlertLabel to a map[string]string.
//
// 4. Compatibility:
//    - These types are used by both the alert handler and error-based scaling logic. Ensure any changes are reflected in both places.
//
// Place these instructions at the top of this file for quick reference and onboarding.
//
// --- End README ---

// CHANGES MADE BY SA:
// - Added DesiredReplicas *int32 to PrometheusInnerAlertLabel for custom scaling via alerts.

package requests

// PrometheusInnerAlertLabel PrometheusInnerAlertLabel
type PrometheusInnerAlertLabel struct {
	AlertName       string `json:"alertname"`
	FunctionName    string `json:"function_name"`
	DesiredReplicas *int32 `json:"desired_replicas,omitempty"` // SA - DesiredReplicas is optional, so it can be nil
}

// PrometheusInnerAlert PrometheusInnerAlert
type PrometheusInnerAlert struct {
	Status string                    `json:"status"`
	Labels PrometheusInnerAlertLabel `json:"labels"`
}

// PrometheusAlert as produced by AlertManager
type PrometheusAlert struct {
	Status   string                 `json:"status"`
	Receiver string                 `json:"receiver"`
	Alerts   []PrometheusInnerAlert `json:"alerts"`
}
