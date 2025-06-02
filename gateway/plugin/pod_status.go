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
	req, err := http.NewRequest("POST", endpoint, bytes.NewBuffer(body))
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

func (p *PodStatusPlugin) GetPodStatus(podName, podIP string) (providertypes.PodStatus, bool) {
	endpoint := fmt.Sprintf("%s/system/podstatus/query?podName=%s&podIP=%s", p.providerURL.String(), podName, podIP)
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return providertypes.PodStatus{}, false
	}
	if p.auth != nil {
		p.auth.Inject(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return providertypes.PodStatus{}, false
	}
	defer resp.Body.Close()
	var status providertypes.PodStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return providertypes.PodStatus{}, false
	}
	return status, true
}
