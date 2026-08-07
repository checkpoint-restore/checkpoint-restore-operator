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
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func podWith(name, node string, statuses ...corev1.ContainerStatus) corev1.Pod {
	var specContainers []corev1.Container
	for _, s := range statuses {
		specContainers = append(specContainers, corev1.Container{Name: s.Name})
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.PodSpec{NodeName: node, Containers: specContainers},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: statuses},
	}
}

func cstatus(name, id string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, ContainerID: id}
}

func TestStripRuntimePrefix(t *testing.T) {
	cases := map[string]string{
		"containerd://abc123": "abc123",
		"cri-o://def456":      "def456",
		"plain":               "plain",
		"":                    "",
	}
	for in, want := range cases {
		if got := stripRuntimePrefix(in); got != want {
			t.Errorf("stripRuntimePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildCoordinationMapFullMesh(t *testing.T) {
	pods := []corev1.Pod{
		podWith("a", "node1", cstatus("app", "containerd://cidA")),
		podWith("b", "node2", cstatus("app", "containerd://cidB")),
	}
	depMap, members, err := buildCoordinationMap(pods, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("want 2 members, got %d", len(members))
	}
	assertDeps(t, depMap, "cidA", []string{"cidB"})
	assertDeps(t, depMap, "cidB", []string{"cidA"})
}

func TestBuildCoordinationMapErrorsWhenContainerHasNoID(t *testing.T) {
	pods := []corev1.Pod{podWith("a", "node1", cstatus("app", ""))}
	if _, _, err := buildCoordinationMap(pods, nil); err == nil {
		t.Fatal("expected error for missing container ID, got nil")
	}
}

func assertDeps(t *testing.T, m DependencyMap, key string, want []string) {
	t.Helper()
	got := append([]string(nil), m[key]...)
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("deps[%s] = %v, want %v", key, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("deps[%s] = %v, want %v", key, got, want)
		}
	}
}
