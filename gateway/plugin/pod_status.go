package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"

	providertypes "github.com/openfaas/faas-provider/types"
	"github.com/openfaas/faas/gateway/pkg/middleware"
)

type PodStatusPlugin struct {
	providerURL *url.URL
	auth        middleware.AuthInjector
}

func NewPodStatusPlugin(providerURL url.URL, auth middleware.AuthInjector) *PodStatusPlugin {
	return &PodStatusPlugin{
		providerURL: &providerURL,
		auth:        auth,
	}
}

func (p *PodStatusPlugin) MarkPodBusy(podName, podIP string) error {
	return p.sendStatusUpdate("busy", podName, podIP)
}

func (p *PodStatusPlugin) MarkPodIdle(podName, podIP string) error {
	return p.sendStatusUpdate("idle", podName, podIP)
}

func (p *PodStatusPlugin) sendStatusUpdate(status, podName, podIP string) error {
	u := *p.providerURL // copy
	u.Path = path.Join(u.Path, "system/podstatus", status)
	endpoint := u.String()
	body, _ := json.Marshal(map[string]string{
		"podName": podName,
		"podIP":   podIP,
	})
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBuffer(body)) // SA - use http.MethodPost
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.auth != nil {
		p.auth.Inject(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to update pod status: %s", resp.Status)
	}
	return nil
}

func (p *PodStatusPlugin) GetPodStatus(functionName, namespace string) ([]providertypes.PodStatus, error) {
	u := *p.providerURL // copy
	u.Path = path.Join(u.Path, "system/podstatus/query")

	// endpoint := fmt.Sprintf("%s/system/podstatus/query?podName=%s&podIP=%s", p.providerURL.String(), podName, podIP)
	// Use query parameters for GET request
	q := u.Query()
	q.Set("functionName", functionName)
	q.Set("namespace", namespace)
	u.RawQuery = q.Encode()

	endpoint := u.String()

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.auth != nil {
		p.auth.Inject(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// If the status code is not OK, we return an empty slice and false
		return nil, fmt.Errorf("failed to fetch pod status: %s", resp.Status)
	}
	var statuses []providertypes.PodStatus
	if err := json.NewDecoder(resp.Body).Decode(&statuses); err != nil {
		return nil, fmt.Errorf("failed to decode response: %v", err)
	}
	return statuses, nil
}
