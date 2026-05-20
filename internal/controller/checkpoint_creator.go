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

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

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

	// build URL
	url := cc.restConfig.Host + "/api/v1/nodes/" + nodeName + "/proxy/checkpoint/" + nameSpace + "/" + podName + "/" + containerName
	logger.Info("creating checkpoint", "url", url, "pod", podName, "container", containerName)

	// make POST request
	httpClient, err := rest.HTTPClientFor(cc.restConfig)
	if err != nil {
		return err
	}
	resp, err := httpClient.Post(url, "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// handle response
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
