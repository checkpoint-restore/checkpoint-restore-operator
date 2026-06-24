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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// checkpointRequestTimeout bounds the kubelet checkpoint request so a hung
// kubelet cannot block the caller indefinitely. Variable so tests can lower it.
var checkpointRequestTimeout = 30 * time.Second
var checkpointRetryDelay = time.Second

const maxCheckpointAttempts = 3
const maxCheckpointErrorBodyBytes = 4096

var checkpointTargetLocks = newCheckpointTargetLockTable()

type Checkpointer interface {
	createCheckpoint(ctx context.Context, ns, podName, containerName, nodeName string) error
}

type checkpointHTTPError struct {
	target     string
	statusCode int
	body       string
}

func (e *checkpointHTTPError) Error() string {
	if strings.TrimSpace(e.body) == "" {
		return fmt.Sprintf("checkpoint failed for %s: status %d", e.target, e.statusCode)
	}
	return fmt.Sprintf("checkpoint failed for %s: status %d: %s", e.target, e.statusCode, strings.TrimSpace(e.body))
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

	target := checkpointTargetKey(nodeName, nameSpace, podName, containerName)
	unlock := checkpointTargetLocks.lock(target)
	defer unlock()

	httpClient, err := rest.HTTPClientFor(cc.restConfig)
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := 1; attempt <= maxCheckpointAttempts; attempt++ {
		lastErr = cc.createCheckpointOnce(ctx, httpClient, url, target)
		if lastErr == nil {
			return nil
		}

		if !isTransientCheckpointError(lastErr) || attempt == maxCheckpointAttempts {
			return lastErr
		}

		logger.Info("retrying transient checkpoint failure", "target", target, "attempt", attempt, "error", lastErr)
		if err := sleepWithContext(ctx, checkpointRetryDelay); err != nil {
			return err
		}
	}

	return lastErr
}

func (cc *CheckpointCreator) createCheckpointOnce(
	ctx context.Context,
	httpClient *http.Client,
	url string,
	target string,
) error {
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxCheckpointErrorBodyBytes))
		return &checkpointHTTPError{
			target:     target,
			statusCode: resp.StatusCode,
			body:       string(body),
		}
	}
	return nil
}

func checkpointTargetKey(nodeName, namespace, podName, containerName string) string {
	return nodeName + "/" + namespace + "/" + podName + "/" + containerName
}

func isTransientCheckpointError(err error) bool {
	var checkpointErr *checkpointHTTPError
	if errors.As(err, &checkpointErr) {
		return checkpointErr.statusCode >= http.StatusInternalServerError &&
			isTransientCheckpointErrorText(checkpointErr.body)
	}
	return isTransientCheckpointErrorText(err.Error())
}

func isTransientCheckpointErrorText(text string) bool {
	return strings.Contains(text, "cannot pause a paused container") ||
		strings.Contains(text, "ctrd-checkpoint/status: no such file or directory")
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type checkpointTargetLockTable struct {
	mu    sync.Mutex
	locks map[string]*checkpointTargetLock
}

type checkpointTargetLock struct {
	mu   sync.Mutex
	refs int
}

func newCheckpointTargetLockTable() *checkpointTargetLockTable {
	return &checkpointTargetLockTable{
		locks: make(map[string]*checkpointTargetLock),
	}
}

func (t *checkpointTargetLockTable) lock(target string) func() {
	t.mu.Lock()
	lock := t.locks[target]
	if lock == nil {
		lock = &checkpointTargetLock{}
		t.locks[target] = lock
	}
	lock.refs++
	t.mu.Unlock()

	lock.mu.Lock()

	return func() {
		lock.mu.Unlock()

		t.mu.Lock()
		defer t.mu.Unlock()

		lock.refs--
		if lock.refs == 0 {
			delete(t.locks, target)
		}
	}
}

func NewCheckpointCreator(c client.Client, restConfig *rest.Config) *CheckpointCreator {
	return &CheckpointCreator{
		client:     c,
		restConfig: restConfig,
	}
}
