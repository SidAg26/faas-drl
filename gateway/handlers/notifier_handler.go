// License: OpenFaaS Community Edition (CE) EULA
// Copyright (c) 2017,2019-2024 OpenFaaS Author(s)

// License: OpenFaaS Community Edition (CE) EULA
// Copyright (c) 2017,2019-2024 OpenFaaS Author(s)

// Copyright (c) OpenFaaS Author(s) 2018. All rights reserved.

package handlers

import (
	"net/http"
	"time"
	// SA - strconv is used for converting status codes to strings
	"strconv"

	"github.com/openfaas/faas-provider/httputil"
)

// MakeNotifierWrapper wraps a http.HandlerFunc in an interceptor to pass to HTTPNotifier
func MakeNotifierWrapper(next http.HandlerFunc, notifiers []HTTPNotifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		then := time.Now()
		url := r.URL.String()

		writer := httputil.NewHttpWriteInterceptor(w)
		next(writer, r)

		// SA - incldue the X-Request-ID header in the notification for custom tracking
		for _, notifier := range notifiers {
			// SA - use the string representation of the status code
			code := strconv.Itoa(writer.Status())
			// SA - notifier.Notify(r.Header.Get("X-Request-ID"), r.Method, url, url, writer.Status(), "completed", time.Since(then))
			notifier.Notify(r.Header.Get("X-Request-ID"), r.Method, url, url, code, "completed", time.Since(then))
		}
	}
}
