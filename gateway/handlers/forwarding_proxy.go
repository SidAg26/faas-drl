// License: OpenFaaS Community Edition (CE) EULA
// Copyright (c) 2017,2019-2024 OpenFaaS Author(s)

// Copyright (c) OpenFaaS Author(s). All rights reserved.

package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"strings"
	"sync"
	"time"

	// SA - uuid is used for generating unique identifiers
	// "github.com/google/uuid"
	// SA - strconv is used for converting status codes to strings
	"crypto/rand"
	"strconv"

	// SA - plugin is used for updating the pod status
	"github.com/openfaas/faas/gateway/plugin"

	fhttputil "github.com/openfaas/faas-provider/httputil"
	"github.com/openfaas/faas/gateway/pkg/middleware"
	"github.com/openfaas/faas/gateway/types"
)

// MakeForwardingProxyHandler create a handler which forwards HTTP requests
func MakeForwardingProxyHandler(proxy *types.HTTPClientReverseProxy,
	notifiers []HTTPNotifier,
	baseURLResolver middleware.BaseURLResolver,
	urlPathTransformer middleware.URLPathTransformer,
	serviceAuthInjector middleware.AuthInjector,
	podStatusUpdater *plugin.PodStatusPlugin, // SA - add the podStatusUpdater to update the pod status
) http.HandlerFunc {

	writeRequestURI := false
	if _, exists := os.LookupEnv("write_request_uri"); exists {
		writeRequestURI = exists
	}

	// This creates a *httputil.ReverseProxy configured to rewrite requests using the provided resolvers
	// For regular requests, the code builds and sends the upstream request manually, but for event streams,
	// it delegates to the reverse proxy created by makeRewriteProxy.
	reverseProxy := makeRewriteProxy(baseURLResolver, urlPathTransformer)

	return func(w http.ResponseWriter, r *http.Request) {

		// Resolve the base URL for the request
		// This function determines the base URL (host and port) of the backend service
		// (function) that should handle the request, based on the incoming HTTP request r
		// The logic for resolving the backend is implemented in the BaseURLResolver
		// interface (from middleware.BaseURLResolver), which is passed in as a parameter to
		// MakeForwardingProxyHandler.
		// This resolver typically uses information from the request (such as the function
		// name in the path) to map to the correct backend service
		baseURL := baseURLResolver.Resolve(r)
		originalURL := r.URL.String()
		// This function transforms the incoming request path to the path expected by the backend.
		requestURL := urlPathTransformer.Transform(r)
		// SA - set a flag to indicate if this request is a function request
		isFunctionRequest := strings.HasPrefix(requestURL, "/function/")

		// SA - Generate a unique request ID for tracing purposes
		// This request ID is set in the "X-Request-ID" header of the request
		// and is used to track the request through the system.
		reqID := randomID()
		r.Header.Set("X-Request-ID", reqID)
		// All registered notifiers are notified that the request has started processing.
		for _, notifier := range notifiers {
			// SA - Notify the notifiers that the request has started processing as string
			code := strconv.Itoa(http.StatusProcessing)
			// SA - notifier.Notify(reqID, r.Method, requestURL, originalURL, http.StatusProcessing, "started", time.Second*0)
			notifier.Notify(reqID, r.Method, requestURL, originalURL, code, "started", time.Second*0)
		}

		start := time.Now()

		// ---------------------------Replacing with Exponentially Backoff---------------------------
		// SA - To ensure that non-200 status codes are retried once,
		// we will retry the request once if the status code is not 200.
		// The first request is sent to the backend service using the forwardRequest function.
		// This also ensures that non-200 status code is not sent to the client
		// and the client only receives a 200 status code if the request is successful.
		// rec := httptest.NewRecorder()

		// // This also ensures that non-200 status code is not sent to the client
		// // and the client only receives a 200 status code if the request is successful.
		// statusCode, err := forwardRequest(rec, r, proxy.Client, baseURL, requestURL, proxy.Timeout, writeRequestURI, serviceAuthInjector, reverseProxy)
		// if err != nil {
		// 	log.Printf("error with upstream request to: %s, %s\n", requestURL, err.Error())
		// }

		// // SA - If the status code is not 200, retry once more
		// statusCodeCheck, _, _ := splitStatusCode(statusCode)
		// if statusCodeCheck != strconv.Itoa(http.StatusOK) {
		// 	log.Printf("Retrying request to %s with status code %s\n", requestURL, statusCodeCheck)
		// 	// Use a ResponseRecorder for the retry
		// 	recRetry := httptest.NewRecorder()
		// 	// Retry the request once more
		// 	statusCode, err = forwardRequest(recRetry, r, proxy.Client, baseURL, requestURL, proxy.Timeout, writeRequestURI, serviceAuthInjector, reverseProxy)
		// 	if err != nil {
		// 		log.Printf("error with upstream request to: %s, %s\n", requestURL, err.Error())
		// 		// If the retry fails, set the status code to 502 Bad Gateway
		// 		// statusCode = strconv.Itoa(http.StatusBadGateway) + "+" + baseURL + "+" + "retry_failed"
		// 	} else {
		// 		for k, v := range recRetry.Header() {
		// 			w.Header()[k] = v
		// 		}
		// 		w.WriteHeader(recRetry.Code)
		// 		recRetry.Body.WriteTo(w)
		// 		log.Printf("Retry successful for request to %s with status code %s\n", requestURL, statusCode)
		// 	}
		// } else {
		// 	// If the status code is 200, write the response to the client
		// 	for k, v := range rec.Header() {
		// 		w.Header()[k] = v
		// 	}
		// 	w.WriteHeader(rec.Code)
		// 	rec.Body.WriteTo(w)
		// }
		// ---------------------------Replacing with Exponentially Backoff---------------------------

		// SA - Exponential backoff retry logic
		// This retry logic is ONLY applied to function requests
		// else an error is returned immediately for "/system/functions" routes
		// If the request is not a function request, it will be sent once without retrying.
		// The retry logic will retry the request up to 3 times with an exponential backoff
		// The backoff will start at 200ms and double each time, up to a maximum of 3 retries.
		maxRetries := 1
		baseDelay := 200 * time.Millisecond

		var (
			rec         *httptest.ResponseRecorder
			statusCode  string
			statusCheck string
			err         error
			internalID  string
			podIP       string
			podName     string
		)

		if isFunctionRequest {
			for attempt := 1; attempt <= maxRetries; attempt++ {
				rec = httptest.NewRecorder()
				statusCode, err = forwardRequest(rec, r, proxy.Client, baseURL, requestURL, proxy.Timeout, writeRequestURI, serviceAuthInjector, reverseProxy)
				if err != nil {
					log.Printf("error with upstream request to: %s, %s\n", requestURL, err.Error())
					// SA - If the error is not nil, we log it and continue to retry
					// We mark the pod as idle here to ensure that the pod is not marked as busy
					// even if the request fails
					if podStatusUpdater != nil {
						_, podIP, podName, internalID = splitStatusCode(statusCode)
						if podName != "" && podIP != "" {
							go func() {
								if err := podStatusUpdater.MarkPodIdle(podName, podIP); err != nil {
									log.Printf("[CustomInternalID: %s] error marking pod as idle: %s\n", &internalID, err.Error())
								}
							}()
							log.Printf("Pod %s marked as idle with IP %s\n", podName, podIP)
						}
					}
					backoff := baseDelay * (1 << (attempt - 1)) // 100ms, 200ms, 400ms
					log.Printf("[RequestID:%s, CustomInternalID: %s] Attempt %d: request to %s returned status %s, retrying in %v...", reqID, internalID, attempt, requestURL, statusCheck, backoff)
					time.Sleep(backoff)
					continue
				}
				statusCheck, _, _, internalID = splitStatusCode(statusCode)
				if statusCheck == strconv.Itoa(http.StatusOK) || statusCheck == strconv.Itoa((http.StatusAccepted)) {
					break
				}
				if attempt < maxRetries {
					// SA - mark the pod status as idle
					if podStatusUpdater != nil {
						_, podIP, podName, internalID = splitStatusCode(statusCode)
						if podName != "" && podIP != "" {
							go func() {
								if err := podStatusUpdater.MarkPodIdle(podName, podIP); err != nil {
									log.Printf("[RequestID:%s, CustomInternalID: %s] error marking pod as idle: %s\n", reqID, internalID, err.Error())
								}
							}()
							log.Printf("[RequestID:%s, CustomInternalID: %s] Pod %s marked as idle with IP %s\n", reqID, internalID, podName, podIP)
						}

					}
					backoff := baseDelay * (1 << (attempt - 1)) // 100ms, 200ms, 400ms
					log.Printf("[RequestID:%s, CustomInternalID: %s] Attempt %d: request to %s returned status %s, retrying in %v...", reqID, internalID, attempt, requestURL, statusCheck, backoff)
					time.Sleep(backoff)
				}
			}

			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			rec.Body.WriteTo(w)

			if statusCheck == strconv.Itoa(http.StatusOK) {
				log.Printf("[RequestID:%s, CustomInternalID: %s] Request to %s succeeded with status %s within %d attempt(s)", reqID, internalID, requestURL, statusCheck, maxRetries)
			} else {
				log.Printf("[RequestID:%s, CustomInternalID: %s] Request to %s failed after %d attempts, last status %s, error %s", reqID, internalID, requestURL, maxRetries, statusCheck, err)
			}

			// SA - mark the pod status as idle
			if podStatusUpdater != nil {
				_, podIP, podName, internalID = splitStatusCode(statusCode)
				if podName != "" && podIP != "" {
					go func() {
						if err := podStatusUpdater.MarkPodIdle(podName, podIP); err != nil {
							log.Printf("[RequestID:%s, CustomInternalID: %s] error marking pod as idle: %s\n", reqID, internalID, err.Error())
						}
					}()
					log.Printf("[RequestID:%s, CustomInternalID: %s] Pod %s marked as idle with IP %s\n", reqID, internalID, podName, podIP)
				}

			}
		} else {
			// For non-function requests, we send the request once without retrying
			statusCode, err = forwardRequest(w, r, proxy.Client, baseURL, requestURL, proxy.Timeout, writeRequestURI, serviceAuthInjector, reverseProxy)
			if err != nil {
				log.Printf("error with upstream request to: %s, %s\n", requestURL, err.Error())
			}

		}

		seconds := time.Since(start)

		// All notifiers are notified that the request has completed,
		// along with the status code and duration
		for _, notifier := range notifiers {
			notifier.Notify(reqID, r.Method, requestURL, originalURL, statusCode, "completed", seconds)
		}
	}
}

func splitStatusCode(statusCode string) (string, string, string, string) {
	parts := strings.Split(statusCode, "+")
	if len(parts) < 3 {
		return statusCode, "", "", ""
	}
	return parts[0], parts[1], parts[2], parts[3]
}

// SA - randomID generates a random ID for tracing purposes
// This function generates a random 16-byte ID and returns it as a hexadecimal string.
func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func buildUpstreamRequest(r *http.Request, baseURL string, requestURL string) *http.Request {
	url := baseURL + requestURL

	if len(r.URL.RawQuery) > 0 {
		url = fmt.Sprintf("%s?%s", url, r.URL.RawQuery)
	}

	upstreamReq, _ := http.NewRequest(r.Method, url, nil)

	copyHeaders(upstreamReq.Header, &r.Header)
	deleteHeaders(&upstreamReq.Header, &hopHeaders)

	if len(r.Host) > 0 && upstreamReq.Header.Get("X-Forwarded-Host") == "" {
		upstreamReq.Header["X-Forwarded-Host"] = []string{r.Host}
	}

	if upstreamReq.Header.Get("X-Forwarded-For") == "" {
		upstreamReq.Header["X-Forwarded-For"] = []string{r.RemoteAddr}
	}

	if r.Body != nil {
		upstreamReq.Body = r.Body
	}

	return upstreamReq
}

// SA - update the function signature to include the string return type for status code
func forwardRequest(w http.ResponseWriter,
	r *http.Request,
	proxyClient *http.Client,
	baseURL string,
	requestURL string,
	timeout time.Duration,
	writeRequestURI bool,
	serviceAuthInjector middleware.AuthInjector,
	reverseProxy *httputil.ReverseProxy) (string, error) {

	if r.Body != nil {
		defer r.Body.Close()
	}
	// The backend is specified by the combination of baseURL (from the resolver)
	// and requestURL (from the transformer) and then the actual request is built
	// using the buildUpstreamRequest function. This function creates a new HTTP request
	// with the method, URL, and headers from the original request, but modifies the URL
	// to point to the backend service. It also copies headers from the original request
	// to the new request, while removing any hop-by-hop headers that should not be sent
	// to the backend.
	upstreamReq := buildUpstreamRequest(r, baseURL, requestURL)

	if serviceAuthInjector != nil {
		serviceAuthInjector.Inject(upstreamReq)
	}

	if writeRequestURI {
		log.Printf("forwardRequest: %s %s\n", upstreamReq.Host, upstreamReq.URL.String())
	}

	if strings.HasPrefix(r.Header.Get("Accept"), "text/event-stream") {
		// SA - update the handleEventStream function to return a string status code
		// return handleEventStream(w, r, reverseProxy, upstreamReq, timeout)
		statusCode, err := handleEventStream(w, r, reverseProxy, upstreamReq, timeout)
		return strconv.Itoa(statusCode), err
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	// sends the request to the backend using the HTTP client
	res, err := proxyClient.Do(upstreamReq.WithContext(ctx))
	if err != nil {
		badStatus := http.StatusBadGateway
		w.WriteHeader(badStatus)
		// SA - update the badStatus to a string
		badStatusStr := strconv.Itoa(badStatus)
		return badStatusStr, err
	}

	if res.Body != nil {
		defer res.Body.Close()
	}

	copyHeaders(w.Header(), &res.Header)

	w.WriteHeader(res.StatusCode)

	if res.Body != nil {
		io.Copy(w, res.Body)
	}

	// SA - return the "res.StatusCode + X-OpenFaaS-Backend-IP" to indicate the status
	// and the Pod IP of the request
	code := strconv.Itoa(res.StatusCode)
	// SA - Get the service IP from the response header
	serviceIP := res.Header.Get("X-OpenFaaS-Backend-IP")
	// podIP := res.Header.Get("X-OpenFaaS-Pod-IP")
	podName := res.Header.Get("X-OpenFaaS-Pod-Name")
	// SA -  get the internal ID from the response header
	internalID := res.Header.Get("X-OpenFaaS-Internal-ID")
	statusCode := code + "+" + serviceIP + "+" + podName + "+" + internalID

	return statusCode, nil
}

func handleEventStream(w http.ResponseWriter, r *http.Request, reverseProxy *httputil.ReverseProxy, upstreamReq *http.Request, timeout time.Duration) (int, error) {
	ww := fhttputil.NewHttpWriteInterceptor(w)

	ctx, cancel := context.WithTimeoutCause(r.Context(), timeout, http.ErrHandlerTimeout)
	defer cancel()

	r = r.WithContext(ctx)
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer func() {
			wg.Done()
			if r := recover(); r != nil {
				if errors.Is(r.(error), http.ErrAbortHandler) {
					log.Printf("Aborted [%s] for: %s", upstreamReq.Method, upstreamReq.URL.Path)

				} else {
					log.Printf("Recovered from panic in reverseproxy: %v", r)
				}
			}
		}()

		reverseProxy.ServeHTTP(ww, r)
	}()

	wg.Wait()

	return ww.Status(), nil
}

func copyHeaders(destination http.Header, source *http.Header) {
	for k, v := range *source {
		vClone := make([]string, len(v))
		copy(vClone, v)
		(destination)[k] = vClone
	}
}

func deleteHeaders(target *http.Header, exclude *[]string) {
	for _, h := range *exclude {
		target.Del(h)
	}
}

// Hop-by-hop headers. These are removed when sent to the backend.
// As of RFC 7230, hop-by-hop headers are required to appear in the
// Connection header field. These are the headers defined by the
// obsoleted RFC 2616 (section 13.5.1) and are used for backward
// compatibility.
// Copied from: https://golang.org/src/net/http/httputil/reverseproxy.go
var hopHeaders = []string{
	"Connection",
	"Proxy-Connection", // non-standard but still sent by libcurl and rejected by e.g. google
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",      // canonicalized version of "TE"
	"Trailer", // not Trailers per URL above; https://www.rfc-editor.org/errata_search.php?eid=4522
	"Transfer-Encoding",
	"Upgrade",
}

func makeRewriteProxy(baseURLResolver middleware.BaseURLResolver, urlPathTransformer middleware.URLPathTransformer) *httputil.ReverseProxy {

	return &httputil.ReverseProxy{

		ErrorLog: log.New(io.Discard, "proxy:", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		},
		Director: func(r *http.Request) {

			baseURL := baseURLResolver.Resolve(r)
			baseURLu, _ := r.URL.Parse(baseURL)

			requestURL := urlPathTransformer.Transform(r)

			r.URL.Scheme = "http"
			r.URL.Path = requestURL
			r.URL.Host = baseURLu.Host
		},
	}
}
