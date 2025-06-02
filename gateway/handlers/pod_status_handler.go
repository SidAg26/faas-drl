// SA - This package provides a handler to mark a pod as idle in the OpenFaaS Kubernetes environment.
package handlers

import (
    "encoding/json"
    "net/http"

    "github.com/openfaas/faas/gateway/plugin"
)

type PodStatusRequest struct {
    PodName string `json:"podName"`
    PodIP   string `json:"podIP"`
}

func MakePodIdleHandler(podStatusUpdater *plugin.PodStatusPlugin) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        var req PodStatusRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
            http.Error(w, "invalid request", http.StatusBadRequest)
            return
        }
        if err := podStatusUpdater.MarkPodIdle(req.PodName, req.PodIP); err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        w.WriteHeader(http.StatusOK)
    }
}