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
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

// coordFakeCheckpointer proves the members are fired concurrently: every call
// waits on a shared WaitGroup that only releases once all expected members have
// arrived. Sequential firing would time out instead of releasing.
type coordFakeCheckpointer struct {
	ready *sync.WaitGroup
	mu    sync.Mutex
	calls []string
}

func (f *coordFakeCheckpointer) createCheckpoint(_ context.Context, ns, pod, container, node string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, pod+"/"+container)
	f.mu.Unlock()

	f.ready.Done()
	done := make(chan struct{})
	go func() { f.ready.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		return "", fmt.Errorf("member %s/%s was not fired concurrently", pod, container)
	}
	return "/var/lib/kubelet/checkpoints/" + pod + "-" + container + ".tar", nil
}

type fakeAgent struct {
	mu      sync.Mutex
	writes  map[string]CoordinatorConfig
	cleared map[string]bool
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{writes: map[string]CoordinatorConfig{}, cleared: map[string]bool{}}
}

func (f *fakeAgent) WriteMap(_ context.Context, node string, cfg CoordinatorConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes[node] = cfg
	return nil
}

func (f *fakeAgent) ClearMap(_ context.Context, node string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared[node] = true
	return nil
}

func coordinatedScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := criuorgv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// runningJobPod builds a Running pod owned by the given Job UID, with one
// container "app" carrying a container ID in its status.
func runningJobPod(name, node string, jobUID types.UID, containerID string) *corev1.Pod {
	controller := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: "j", UID: jobUID, Controller: &controller,
			}},
		},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", ContainerID: containerID}},
		},
	}
}

func TestRunCoordinatedCheckpointsWritesMapAndCompletes(t *testing.T) {
	scheme := coordinatedScheme(t)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default", UID: types.UID("job-uid")}}
	podA := runningJobPod("a", "node1", job.UID, "containerd://cidA")
	podB := runningJobPod("b", "node2", job.UID, "containerd://cidB")

	wc := &criuorgv1.WorkloadCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "wc", Namespace: "default"},
		Spec: criuorgv1.WorkloadCheckpointSpec{
			TargetRef:   criuorgv1.WorkloadReference{Kind: "Job", Name: "j"},
			Coordinated: true,
		},
		Status: criuorgv1.WorkloadCheckpointStatus{Phase: criuorgv1.WorkloadPhaseRunning},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(job, podA, podB, wc).
		WithStatusSubresource(wc).Build()

	var ready sync.WaitGroup
	ready.Add(2)
	fc := &coordFakeCheckpointer{ready: &ready}
	fa := newFakeAgent()
	r := &WorkloadCheckpointReconciler{
		Client: cl, Scheme: scheme,
		Checkpointer:    fc,
		Agent:           fa,
		CoordinatorAddr: "10.96.0.10",
		CoordinatorPort: "8080",
	}

	if _, err := r.runCoordinatedCheckpoints(context.Background(), wc); err != nil {
		t.Fatalf("runCoordinatedCheckpoints: %v", err)
	}

	// Both nodes got the full-mesh map pointing at the ClusterIP.
	if fa.writes["node1"].Address != "10.96.0.10" || len(fa.writes["node1"].Dependencies["cidA"]) != 1 {
		t.Errorf("node1 map wrong: %+v", fa.writes["node1"])
	}
	if fa.writes["node2"].Dependencies["cidB"][0] != "cidA" {
		t.Errorf("node2 map wrong: %+v", fa.writes["node2"])
	}
	if !fa.cleared["node1"] || !fa.cleared["node2"] {
		t.Errorf("maps not cleared: %+v", fa.cleared)
	}

	got := &criuorgv1.WorkloadCheckpoint{}
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(wc), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != criuorgv1.WorkloadPhaseCompleted {
		t.Errorf("phase = %s, want Completed", got.Status.Phase)
	}
	if got.Status.CheckpointedPodCount != 2 {
		t.Errorf("checkpointedPodCount = %d, want 2", got.Status.CheckpointedPodCount)
	}
}
