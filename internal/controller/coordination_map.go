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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// DependencyMap is the criu-coordinator "dependencies" map: container ID -> the
// container IDs it must rendezvous with at the barrier.
type DependencyMap map[string][]string

// coordMember is one container that participates in a coordinated run.
type coordMember struct {
	PodName       string
	NodeName      string
	ContainerName string
	ContainerID   string
}

// stripRuntimePrefix removes a "runtime://" scheme from a CRI container ID.
func stripRuntimePrefix(id string) string {
	if i := strings.Index(id, "://"); i >= 0 {
		return id[i+len("://"):]
	}
	return id
}

// buildCoordinationMap resolves the selected containers of the given pods into
// coordinated members and a full-mesh dependency map (every member depends on
// every other member). It errors if any selected running container has no
// container ID yet, because a missing member would break the barrier.
func buildCoordinationMap(pods []corev1.Pod, containerNames []string) (DependencyMap, []coordMember, error) {
	var members []coordMember
	for _, pod := range pods {
		idByName := map[string]string{}
		for _, cs := range pod.Status.ContainerStatuses {
			idByName[cs.Name] = stripRuntimePrefix(cs.ContainerID)
		}
		for _, container := range filterContainers(pod, containerNames) {
			id := idByName[container.Name]
			if id == "" {
				return nil, nil, fmt.Errorf("pod %s container %s has no container ID yet", pod.Name, container.Name)
			}
			members = append(members, coordMember{
				PodName:       pod.Name,
				NodeName:      pod.Spec.NodeName,
				ContainerName: container.Name,
				ContainerID:   id,
			})
		}
	}

	depMap := DependencyMap{}
	for i, m := range members {
		var deps []string
		for j, other := range members {
			if i != j {
				deps = append(deps, other.ContainerID)
			}
		}
		depMap[m.ContainerID] = deps
	}
	return depMap, members, nil
}
