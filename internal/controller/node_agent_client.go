/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CoordinatorConfig is the JSON written to /etc/criu/criu-coordinator.json.
type CoordinatorConfig struct {
	Address      string        `json:"address"`
	Port         string        `json:"port"`
	Dependencies DependencyMap `json:"dependencies"`
}

// NodeAgentClient writes/clears the coordinator dependency map on a node.
type NodeAgentClient interface {
	WriteMap(ctx context.Context, nodeName string, cfg CoordinatorConfig) error
	ClearMap(ctx context.Context, nodeName string) error
}

type httpNodeAgentClient struct {
	client        client.Client
	namespace     string
	labelSelector string
	port          int
	http          *http.Client
}

// NewHTTPNodeAgentClient talks to node-agent pods (selected by labelSelector in
// namespace) addressed by pod IP on the given port.
func NewHTTPNodeAgentClient(c client.Client, namespace, labelSelector string, port int) *httpNodeAgentClient {
	return &httpNodeAgentClient{
		client: c, namespace: namespace, labelSelector: labelSelector, port: port,
		http: &http.Client{},
	}
}

func (h *httpNodeAgentClient) hostForNode(ctx context.Context, nodeName string) (string, error) {
	pods := &corev1.PodList{}
	sel, err := labelSelectorFromString(h.labelSelector)
	if err != nil {
		return "", err
	}
	if err := h.client.List(ctx, pods, client.InNamespace(h.namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return "", err
	}
	for _, p := range pods.Items {
		if p.Spec.NodeName == nodeName && p.Status.PodIP != "" {
			return fmt.Sprintf("%s:%d", p.Status.PodIP, h.port), nil
		}
	}
	return "", fmt.Errorf("no ready node-agent pod found on node %s", nodeName)
}

func (h *httpNodeAgentClient) WriteMap(ctx context.Context, nodeName string, cfg CoordinatorConfig) error {
	host, err := h.hostForNode(ctx, nodeName)
	if err != nil {
		return err
	}
	return postMap(ctx, h.http, host, cfg)
}

func (h *httpNodeAgentClient) ClearMap(ctx context.Context, nodeName string) error {
	host, err := h.hostForNode(ctx, nodeName)
	if err != nil {
		return err
	}
	return deleteMap(ctx, h.http, host)
}

func postMap(ctx context.Context, hc *http.Client, host string, cfg CoordinatorConfig) error {
	body, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+host+"/map", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return doExpectOK(hc, req)
}

func deleteMap(ctx context.Context, hc *http.Client, host string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "http://"+host+"/map", nil)
	if err != nil {
		return err
	}
	return doExpectOK(hc, req)
}

func doExpectOK(hc *http.Client, req *http.Request) error {
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("node-agent %s %s: status %d", req.Method, req.URL, resp.StatusCode)
	}
	return nil
}

func labelSelectorFromString(s string) (labels.Selector, error) {
	ls, err := metav1.ParseToLabelSelector(s)
	if err != nil {
		return nil, err
	}
	return metav1.LabelSelectorAsSelector(ls)
}
