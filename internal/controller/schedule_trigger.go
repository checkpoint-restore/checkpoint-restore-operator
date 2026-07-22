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
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// runScheduledCheckpoints checkpoints the containers of all pods matching the
// CheckpointSchedule concurrently and returns the number of checkpoints that
// were created successfully. Failures are logged and skipped.
//
// When the schedule opts into volume snapshots, each matching pod's PVC-backed
// volumes are snapshotted immediately before its containers are checkpointed, so
// the disk snapshot is cut a moment before the memory image. Under
// FailurePolicyRequire a snapshot failure skips the pod's checkpoints entirely,
// so an archive is never produced without its volume snapshots.
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
	for i := range pods {
		pod := pods[i]

		selected := selectedContainerNames(&pod, containerSet)
		if len(selected) == 0 {
			continue
		}

		// Snapshot the pod's volumes before checkpointing any of its containers.
		// Under Require, a snapshot failure skips this pod so no memory image is
		// produced without a matching disk snapshot.
		if _, proceed := captureVolumeSnapshots(ctx, c, schedule.Spec.VolumeSnapshots, &pod, selected); !proceed {
			continue
		}

		for _, containerName := range selected {
			wg.Add(1)
			go func(ns, podName, containerName, nodeName string) {
				defer wg.Done()
				if _, err := creator.createCheckpoint(ctx, ns, podName, containerName, nodeName); err != nil {
					logger.Error(err, "failed to create checkpoint", "pod", podName, "container", containerName)
					return
				}
				created.Add(1)
			}(pod.Namespace, pod.Name, containerName, pod.Spec.NodeName)
		}
	}
	wg.Wait()
	return created.Load()
}

// selectedContainerNames returns the names of pod's containers that the schedule
// targets. An empty containerSet selects every container.
func selectedContainerNames(pod *corev1.Pod, containerSet map[string]struct{}) []string {
	var names []string
	for _, ctr := range pod.Spec.Containers {
		if len(containerSet) > 0 {
			if _, ok := containerSet[ctr.Name]; !ok {
				continue
			}
		}
		names = append(names, ctr.Name)
	}
	return names
}
