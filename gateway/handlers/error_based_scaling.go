package handlers

import (
    "bytes"
    "encoding/json"
    "log"
    "math"
    "net/http"
    "net/http/httptest"
    "net/url"
    "strconv"
    "strings"
    "sync"
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
    cooldown           time.Duration
    lastScalingAction  map[string]time.Time
    mu                 sync.Mutex
}

// Helper to extract function name from "functionname.namespace"
func extractFunctionName(fullName string) string {
    parts := strings.SplitN(fullName, ".", 2)
    return parts[0]
}

// getAllFunctionNames queries Prometheus for all function names with active replicas
func (e *ErrorBasedScaler) getAllFunctionNames() []string {
    query := `sum(gateway_service_count) by (function_name)`
    results, err := e.prometheusQuery.Fetch(url.QueryEscape(query))
    if err != nil {
        log.Printf("[ScaleCustom] Error querying Prometheus for function names: %s", err)
        return nil
    }

    var functionNames []string
    for _, result := range results.Data.Result {
        functionName := result.Metric.FunctionName
        if functionName != "" {
            functionName = extractFunctionName(functionName)
            functionNames = append(functionNames, functionName)
        }
    }
    log.Printf("[ScaleCustom] Found functions for pod failure scaling: %v", functionNames)
    return functionNames
}

// getCurrentReplicas queries Prometheus for the current replica count of a function
func (e *ErrorBasedScaler) getCurrentReplicas(functionName string) (int, error) {
    // Try with namespace
    fullName := functionName + "." + e.defaultNamespace
    query := `sum(gateway_service_count{function_name="` + fullName + `"})`
    results, err := e.prometheusQuery.Fetch(url.QueryEscape(query))
    if err != nil {
        log.Printf("[ScaleCustom] Error querying Prometheus for current replicas for %s: %v", fullName, err)
        return 0, err
    }

    // If no results, try without namespace
    if len(results.Data.Result) == 0 {
        query = `sum(gateway_service_count{function_name="` + functionName + `"})`
        results, err = e.prometheusQuery.Fetch(url.QueryEscape(query))
        if err != nil {
            log.Printf("[ScaleCustom] Error querying Prometheus for current replicas for %s: %v", functionName, err)
            return 0, err
        }
    }

    if len(results.Data.Result) == 0 {
        return 0, nil // No replicas found
    }

    valueStr := results.Data.Result[0].Value[1].(string)
    replicas, err := strconv.ParseFloat(valueStr, 64)
    if err != nil {
        log.Printf("[ScaleCustom] Error parsing current replicas for %s: %v", functionName, err)
        return 0, err
    }
    return int(replicas), nil
}

// countFailingPodsProm queries Prometheus (with kube-state-metrics) for OOMKilled and CrashLoopBackOff pods for a function
func (e *ErrorBasedScaler) countFailingPodsProm(functionName string) (int, int, error) {
    reasons := []string{
        "CrashLoopBackOff",
        "Error",
        "ImagePullBackOff",
        "ErrImagePull",
        "CreateContainerConfigError",
    }

    totalFailing := 0

    // Try with and without namespace for each query
    for _, reason := range reasons {
        // With namespace
        fullName := functionName + "." + e.defaultNamespace
        query := `sum(kube_pod_container_status_waiting_reason{reason="` + reason + `", pod=~".*` + fullName + `.*"})`
        results, err := e.prometheusQuery.Fetch(url.QueryEscape(query))
        if err != nil || len(results.Data.Result) == 0 {
            // Try without namespace
            query = `sum(kube_pod_container_status_waiting_reason{reason="` + reason + `", pod=~".*` + functionName + `.*"})`
            results, err = e.prometheusQuery.Fetch(url.QueryEscape(query))
        }
        if err != nil {
            log.Printf("Error querying Prometheus for %s: %s", reason, err)
            continue
        }
        if len(results.Data.Result) > 0 {
            valStr := results.Data.Result[0].Value[1].(string)
            val, err := strconv.ParseFloat(valStr, 64)
            if err == nil {
                totalFailing += int(val)
            }
        }
    }

    // OOMKilled: with and without namespace
    fullName := functionName + "." + e.defaultNamespace
    oomQuery := `
        sum(
            kube_pod_container_status_last_terminated_reason{reason="OOMKilled", pod=~".*` + fullName + `.*"}
            * ignoring(phase)
            (1 - kube_pod_status_phase{phase="Running", pod=~".*` + fullName + `.*"})
        )
    `
    oomResults, err := e.prometheusQuery.Fetch(url.QueryEscape(oomQuery))
    if err != nil || len(oomResults.Data.Result) == 0 {
        oomQuery = `
            sum(
                kube_pod_container_status_last_terminated_reason{reason="OOMKilled", pod=~".*` + functionName + `.*"}
                * ignoring(phase)
                (1 - kube_pod_status_phase{phase="Running", pod=~".*` + functionName + `.*"})
            )
        `
        oomResults, err = e.prometheusQuery.Fetch(url.QueryEscape(oomQuery))
    }
    if err != nil {
        log.Printf("Error querying Prometheus for OOMKilled (not running): %s", err)
    } else if len(oomResults.Data.Result) > 0 {
        valStr := oomResults.Data.Result[0].Value[1].(string)
        val, err := strconv.ParseFloat(valStr, 64)
        if err == nil {
            totalFailing += int(val)
        }
    }
    log.Printf("[ScaleCustom] Function %s has %d failing pods", functionName, totalFailing)

    // Total pods: with and without namespace
    totalPods := 0
    totalQuery := `sum(kube_pod_info{pod=~".*` + fullName + `.*"})`
    totalResults, err := e.prometheusQuery.Fetch(url.QueryEscape(totalQuery))
    if err != nil || len(totalResults.Data.Result) == 0 {
        totalQuery = `sum(kube_pod_info{pod=~".*` + functionName + `.*"})`
        totalResults, err = e.prometheusQuery.Fetch(url.QueryEscape(totalQuery))
    }
    if err == nil && len(totalResults.Data.Result) > 0 {
        valStr := totalResults.Data.Result[0].Value[1].(string)
        val, err := strconv.ParseFloat(valStr, 64)
        if err == nil {
            totalPods = int(val)
        }
    }

    return totalFailing, totalPods, nil
}

// scaleForPodFailures triggers scaling for pod failures
func (e *ErrorBasedScaler) scaleForPodFailures(functionName string, failingPods, totalPods int) {
    // Get current replicas (from Prometheus or Kubernetes)
    currentReplicas, err := e.getCurrentReplicas(functionName)
    if err != nil {
        log.Printf("[ScaleCustom] Error getting current replicas for %s: %v", functionName, err)
        return
    }
    desiredReplicas := currentReplicas + failingPods
    if desiredReplicas > e.maxReplicas {
        desiredReplicas = e.maxReplicas
    }
    desiredReplicasInt32 := int32(desiredReplicas)
    alert := requests.PrometheusAlert{
        Status: "firing",
        Alerts: []requests.PrometheusInnerAlert{
            {
                Status: "firing",
                Labels: requests.PrometheusInnerAlertLabel{
                    AlertName:       "PodFailureDetected",
                    FunctionName:    functionName,
                    DesiredReplicas: &desiredReplicasInt32,
                },
            },
        },
    }
    e.triggerAlert(alert)
    log.Printf("[ScaleCustom] Scaling up %s: %d pods failing out of %d, scaling to %d", functionName, failingPods, totalPods, desiredReplicas)
}

// decideAndScale centralizes all scaling logic for a function per interval
func (e *ErrorBasedScaler) decideAndScale() {
    functionNames := e.getAllFunctionNames()
    now := time.Now()
    for _, functionName := range functionNames {
        // Cooldown check for all scaling actions
        e.mu.Lock()
        last, ok := e.lastScalingAction[functionName]
        if ok && now.Sub(last) < e.cooldown {
            e.mu.Unlock()
            log.Printf("[ScaleCustom] Skipping scaling for %s due to cooldown", functionName)
            continue
        }
        e.mu.Unlock()

        // 1. Check for pod failures (highest priority)
        failingPods, totalPods, err := e.countFailingPodsProm(functionName)
        if err == nil && failingPods > 0 {
            e.mu.Lock()
            e.lastScalingAction[functionName] = time.Now()
            e.mu.Unlock()
            e.scaleForPodFailures(functionName, failingPods, totalPods)
            continue // Highest priority, skip other checks
        }

        // // 2. Check for high error rate
        // scaleUP := e.checkAndScaleForFunction(functionName)
        // if scaleUP {
        //     e.mu.Lock()
        //     e.lastScalingAction[functionName] = time.Now()
        //     e.mu.Unlock()
        //     continue // If scaling up was triggered, skip scale down check
        // }

        // // 3. Check for scale down
        // scaleDown := e.checkAndScaleDownForFunction(functionName)
        // if scaleDown {
        //     e.mu.Lock()
        //     e.lastScalingAction[functionName] = time.Now()
        //     e.mu.Unlock()
        //     continue // If scaling down was triggered, skip further checks
        // }
    }
}

// start begins periodic checking for functions with high/low error rates
func (e *ErrorBasedScaler) start() {
    ticker := time.NewTicker(e.interval)
    go func() {
        for {
            <-ticker.C
            log.Printf("Starting periodic error-based scaling check")
            e.decideAndScale()
        }
    }()
}

// checkAndScaleForFunction checks and scales up a single function if needed
func (e *ErrorBasedScaler) checkAndScaleForFunction(functionName string) bool {
    // Try with namespace
    fullName := functionName + "." + e.defaultNamespace
    query := `sum(increase(gateway_function_invocation_total{code!="200",function_name="` + fullName + `" }[30s])) by (function_name)`
    results, err := e.prometheusQuery.Fetch(url.QueryEscape(query))
    if err != nil || len(results.Data.Result) == 0 {
        // Try without namespace
        query = `sum(increase(gateway_function_invocation_total{code!="200",function_name="` + functionName + `" }[30s])) by (function_name)`
        results, err = e.prometheusQuery.Fetch(url.QueryEscape(query))
        if err != nil {
            log.Printf("Error querying Prometheus for error rates: %s", err)
            return false
        }
    }

    // Get current replica count for this function
    currentReplicas, err := e.getCurrentReplicas(functionName)
    if err != nil {
        log.Printf("Error getting current replicas for %s: %s", functionName, err)
        return false
    }

    for _, result := range results.Data.Result {
        fn := extractFunctionName(result.Metric.FunctionName)
        if fn != functionName {
            continue
        }
        valueStr := result.Value[1].(string)
        errorCount, err := strconv.ParseFloat(valueStr, 64)
        if err != nil {
            log.Printf("Error parsing value for %s: %s", functionName, err)
            continue
        }

        if int(errorCount) > e.errorThreshold {
            if currentReplicas >= e.maxReplicas {
                log.Printf("Function %s already at maximum replicas (%d)", functionName, currentReplicas)
                return false
            }
            alert := e.createErrorAlert(functionName, int(errorCount), currentReplicas)
            e.triggerAlert(alert)
            log.Printf("Scaling up %s due to high error rate: %d errors, current replicas: %d",
                functionName, int(errorCount), currentReplicas)
            return true
        }
    }
    return false
}

// checkAndScaleDownForFunction checks and scales down a single function if needed
func (e *ErrorBasedScaler) checkAndScaleDownForFunction(functionName string) bool {
    // Try with namespace
    fullName := functionName + "." + e.defaultNamespace
    query := `sum(increase(gateway_function_invocation_total{code!="200",function_name="` + fullName + `" }[30s])) by (function_name)`
    results, err := e.prometheusQuery.Fetch(url.QueryEscape(query))
    if err != nil || len(results.Data.Result) == 0 {
        // Try without namespace
        query = `sum(increase(gateway_function_invocation_total{code!="200",function_name="` + functionName + `" }[30s])) by (function_name)`
        results, err = e.prometheusQuery.Fetch(url.QueryEscape(query))
        if err != nil {
            log.Printf("Error querying Prometheus for error rates: %s", err)
            return false
        }
    }

    currentReplicas, err := e.getCurrentReplicas(functionName)
    if err != nil {
        log.Printf("Error getting current replicas for %s: %s", functionName, err)
        return false
    }

    for _, result := range results.Data.Result {
        fn := extractFunctionName(result.Metric.FunctionName)
        if fn != functionName {
            continue
        }
        valueStr := result.Value[1].(string)
        errorCount, err := strconv.ParseFloat(valueStr, 64)
        if err != nil {
            log.Printf("Error parsing value for %s: %s", functionName, err)
            continue
        }

        if int(errorCount) <= e.scaleDownThreshold && currentReplicas > 1 {
            alert := e.createScaleDownAlert(functionName, int(errorCount), currentReplicas)
            e.triggerAlert(alert)
            log.Printf("Scaling down %s due to low error rate: %d errors, current replicas: %d",
                functionName, int(errorCount), currentReplicas)
            return true
        }
    }
    return false
}

// createErrorAlert creates a PrometheusAlert for scaling up the given function based on error count
func (e *ErrorBasedScaler) createErrorAlert(functionName string, errorCount, currentReplicas int) requests.PrometheusAlert {
    desiredReplicas := currentReplicas + int(math.Ceil(float64(errorCount)/2.0))
    if desiredReplicas > e.maxReplicas {
        desiredReplicas = e.maxReplicas
    }
    if desiredReplicas < 1 {
        desiredReplicas = 1
    }
    desiredReplicasInt32 := int32(desiredReplicas)

    return requests.PrometheusAlert{
        Status: "firing",
        Alerts: []requests.PrometheusInnerAlert{
            {
                Status: "firing",
                Labels: requests.PrometheusInnerAlertLabel{
                    AlertName:       "HighErrorRate",
                    FunctionName:    functionName,
                    DesiredReplicas: &(desiredReplicasInt32),
                },
            },
        },
    }
}

// createScaleDownAlert creates a PrometheusAlert for scaling down the given function based on error count
func (e *ErrorBasedScaler) createScaleDownAlert(functionName string, errorCount, currentReplicas int) requests.PrometheusAlert {
    desiredReplicas := int32(math.Max(1, float64(currentReplicas)/2))
    return requests.PrometheusAlert{
        Status: "firing",
        Alerts: []requests.PrometheusInnerAlert{
            {
                Status: "firing",
                Labels: requests.PrometheusInnerAlertLabel{
                    AlertName:       "LowErrorRate",
                    FunctionName:    functionName,
                    DesiredReplicas: &desiredReplicas,
                },
            },
        },
    }
}

// triggerAlert sends an alert to the alert handler
func (e *ErrorBasedScaler) triggerAlert(alert requests.PrometheusAlert) {
    alertBytes, err := json.Marshal(alert)
    if err != nil {
        log.Printf("Error marshalling alert: %s", err)
        return
    }

    req, err := http.NewRequest(http.MethodPost, "/system/alert", bytes.NewReader(alertBytes))
    if err != nil {
        log.Printf("Error creating request: %s", err)
        return
    }

    req.Header.Set("Content-Type", "application/json")
    rec := httptest.NewRecorder()
    e.alertHandler(rec, req)

    if rec.Code != http.StatusOK {
        log.Printf("Error triggering alert for %s: %d - %s",
            alert.Alerts[0].Labels.FunctionName, rec.Code, rec.Body.String())
    } else {
        log.Printf("Successfully triggered scaling for %s to %v replicas",
            alert.Alerts[0].Labels.FunctionName, alert.Alerts[0].Labels.DesiredReplicas)
    }
}

// MakeErrorBasedScalingHandler creates an HTTP handler for error-based scaling
func MakeErrorBasedScalingHandler(prometheusQuery metrics.PrometheusQueryFetcher,
    alertHandler http.HandlerFunc,
    namespace string,
    errorThreshold int,
    scaleDownThreshold int,
    maxReplicas int,
    cooldown time.Duration) http.HandlerFunc {

    scaler := &ErrorBasedScaler{
        prometheusQuery:    prometheusQuery,
        alertHandler:       alertHandler,
        interval:           time.Second * 15,
        errorThreshold:     errorThreshold,
        scaleDownThreshold: scaleDownThreshold,
        defaultNamespace:   namespace,
        maxReplicas:        maxReplicas,
        cooldown:           cooldown,
        lastScalingAction:  make(map[string]time.Time),
        mu:                 sync.Mutex{},
    }

    go scaler.start()

    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            w.WriteHeader(http.StatusMethodNotAllowed)
            return
        }
        scaler.decideAndScale()
        w.WriteHeader(http.StatusOK)
        w.Write([]byte("Error-based scaling check triggered"))
    }
}