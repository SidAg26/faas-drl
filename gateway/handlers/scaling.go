// License: OpenFaaS Community Edition (CE) EULA
// Copyright (c) 2017,2019-2024 OpenFaaS Author(s)

// Copyright (c) OpenFaaS Author(s). All rights reserved.

package handlers

import (
	"fmt"
	"log"
	"net/http"

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
