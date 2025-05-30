package handlers

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
	"sync" // SA - Added sync package for concurrency safety

	"github.com/openfaas/faas/gateway/metrics"
	"github.com/openfaas/faas/gateway/pkg/middleware"
	"github.com/prometheus/client_golang/prometheus"
)

// // HTTPNotifier notify about HTTP request/response
// type HTTPNotifier interface {
// 	Notify(method string, URL string, originalURL string, statusCode int, event string, duration time.Duration)
// }

// SA - custom HTTPNotifier interface
// HTTPNotifier interface defines a method to notify about HTTP requests and responses
// It includes details such as request ID, HTTP method, URL, original URL, status code, event type, and duration
type HTTPNotifier interface {
    Notify(requestID string, method string, URL string, originalURL string, statusCode int, event string, duration time.Duration)
}

func urlToLabel(path string) string {
	if len(path) > 0 {
		path = strings.TrimRight(path, "/")
	}
	if path == "" {
		path = "/"
	}
	return path
}

// PrometheusFunctionNotifier records metrics to Prometheus
type PrometheusFunctionNotifier struct {
	Metrics *metrics.MetricOptions
	//FunctionNamespace default namespace of the function
	FunctionNamespace string
}

// SA - Include the requestId in the interface
// Notify records metrics in Prometheus
func (p PrometheusFunctionNotifier) Notify(requestID string, method string, URL string, originalURL string, statusCode int, event string, duration time.Duration) {
	serviceName := middleware.GetServiceName(originalURL)
	if len(p.FunctionNamespace) > 0 {
		if !strings.Contains(serviceName, ".") {
			serviceName = fmt.Sprintf("%s.%s", serviceName, p.FunctionNamespace)
		}
	}

	code := strconv.Itoa(statusCode)
	labels := prometheus.Labels{"function_name": serviceName, "code": code}

	if event == "completed" {
		seconds := duration.Seconds()
		p.Metrics.GatewayFunctionsHistogram.
			With(labels).
			Observe(seconds)

		p.Metrics.GatewayFunctionInvocation.
			With(labels).
			Inc()
	} else if event == "started" {
		p.Metrics.GatewayFunctionInvocationStarted.WithLabelValues(serviceName).Inc()
	}

}

// LoggingNotifier notifies a log about a request
type LoggingNotifier struct {
}

// SA - Include the requestId in the interface
// Notify the LoggingNotifier about a request
func (LoggingNotifier) Notify(requestID string, method string, URL string, originalURL string, statusCode int, event string, duration time.Duration) {
	if event == "completed" {
		log.Printf("Forwarded [%s] to %s - [%d] - %.4fs", method, originalURL, statusCode, duration.Seconds())
	}
}


// SA - Log of changes:
// Required files are:
// - handlers/notifiers.go
// - handlers/forwarding_proxy.go
// - handlers/notifier_handler.go
// - main.go


// SA - CustomLoggingNotifier logs unique request details
// CustomLoggingNotifier logs unique request details with a request ID
// These would be available in the container logs for debugging purposes
type CustomLoggingNotifier struct {
    mu       sync.Mutex
    requests map[string]time.Time
}

func NewCustomLoggingNotifier() *CustomLoggingNotifier {
    return &CustomLoggingNotifier{
        requests: make(map[string]time.Time),
    }
}

// SA - Include the requestId in the interface
func (n *CustomLoggingNotifier) Notify(requestID string, method string, URL string, originalURL string, statusCode int, event string, duration time.Duration) {
    // requestID := fmt.Sprintf("%s-%s-%d", method, originalURL, time.Now().UnixNano())

    n.mu.Lock()
    defer n.mu.Unlock()

    if event == "started" {
        n.requests[requestID] = time.Now()
        log.Printf("[CustomNotifier] START: ID=%s Method=%s Path=%s Time=%s", requestID, method, originalURL, n.requests[requestID].Format(time.RFC3339Nano))
    } else if event == "completed" {
        startTime, ok := n.requests[requestID]
        endTime := time.Now()
        if !ok {
            startTime = endTime.Add(-duration)
        }
        log.Printf("[CustomNotifier] END: ID=%s Method=%s Path=%s Status=%d Start=%s End=%s Duration=%.4fs",
            requestID, method, originalURL, statusCode,
            startTime.Format(time.RFC3339Nano),
            endTime.Format(time.RFC3339Nano),
            duration.Seconds())
        delete(n.requests, requestID)
    }
}