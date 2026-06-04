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
	"context"
	"fmt"
	"net/http"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// checkpointRequestTimeout bounds the kubelet checkpoint request so a hung
// kubelet cannot block the caller indefinitely. Variable so tests can lower it.
var checkpointRequestTimeout = 30 * time.Second

type Checkpointer interface {
	createCheckpoint(ctx context.Context, ns, podName, containerName, nodeName string) error
}

type CheckpointCreator struct {
	client     client.Client
	restConfig *rest.Config
}

func (cc *CheckpointCreator) createCheckpoint(
	ctx context.Context,
	nameSpace string,
	podName string,
	containerName string,
	nodeName string,
) error {
	logger := log.FromContext(ctx)

	url := cc.restConfig.Host + "/api/v1/nodes/" + nodeName + "/proxy/checkpoint/" + nameSpace + "/" + podName + "/" + containerName
	logger.Info("creating checkpoint", "url", url, "pod", podName, "container", containerName)

	httpClient, err := rest.HTTPClientFor(cc.restConfig)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, checkpointRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("checkpoint failed for %s/%s/%s: status %d",
			nameSpace, podName, containerName, resp.StatusCode)
	}
	return nil
}

func NewCheckpointCreator(c client.Client, restConfig *rest.Config) *CheckpointCreator {
	return &CheckpointCreator{
		client:     c,
		restConfig: restConfig,
	}
}
