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
	"sync"
	"sync/atomic"

	v1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// runScheduledCheckpoints checkpoints the containers of all pods matching the
// CheckpointSchedule concurrently and returns the number of checkpoints that
// were created successfully. Failures are logged and skipped.
func runScheduledCheckpoints(ctx context.Context, c client.Client, creator Checkpointer, schedule *v1.CheckpointSchedule) int32 {
	logger := log.FromContext(ctx)

	pods, err := getMatchingPods(ctx, c, schedule.Spec.Namespace, &schedule.Spec.Selector)
	if err != nil {
		logger.Error(err, "failed to get matching pods")
		return 0
	}

	containerSet := make(map[string]struct{}, len(schedule.Spec.ContainerNames))
	for _, name := range schedule.Spec.ContainerNames {
		containerSet[name] = struct{}{}
	}

	var created atomic.Int32
	var wg sync.WaitGroup
	for _, pod := range pods {
		for _, c := range pod.Spec.Containers {
			if len(containerSet) > 0 {
				if _, ok := containerSet[c.Name]; !ok {
					continue
				}
			}
			wg.Add(1)
			go func(ns, podName, containerName, nodeName string) {
				defer wg.Done()
				if err := creator.createCheckpoint(ctx, ns, podName, containerName, nodeName); err != nil {
					logger.Error(err, "failed to create checkpoint", "pod", podName, "container", containerName)
					return
				}
				created.Add(1)
			}(pod.Namespace, pod.Name, c.Name, pod.Spec.NodeName)
		}
	}
	wg.Wait()
	return created.Load()
}
