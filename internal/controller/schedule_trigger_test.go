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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

// mockCheckpointer records every createCheckpoint call for assertions. When
// err is set, calls are still recorded and return that error.
type mockCheckpointer struct {
	mu    sync.Mutex
	err   error
	calls []checkpointCall
}

type checkpointCall struct {
	ns, pod, container, node string
}

func (m *mockCheckpointer) createCheckpoint(_ context.Context, ns, pod, container, node string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, checkpointCall{ns, pod, container, node})
	return "", m.err
}

func makeSchedule(containerNames []string, interval time.Duration) *v1.CheckpointSchedule {
	return &v1.CheckpointSchedule{
		Spec: v1.CheckpointScheduleSpec{
			Namespace:      "default",
			ContainerNames: containerNames,
			Selector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Triggers:       v1.TriggersSpec{Interval: &metav1.Duration{Duration: interval}},
		},
	}
}

func makeClient(pods ...*corev1.Pod) client.Client {
	builder := fake.NewClientBuilder().WithScheme(scheme.Scheme)
	for _, p := range pods {
		builder = builder.WithObjects(p).WithStatusSubresource(p)
	}
	return builder.Build()
}

// snapshotTestScheme is a scheme that knows both core types (pods) and the
// unstructured VolumeSnapshot GVK, so a fake client can create and list
// VolumeSnapshots alongside pods.
func snapshotTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	Expect(corev1.AddToScheme(s)).To(Succeed())
	s.AddKnownTypeWithName(volumeSnapshotGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(
		volumeSnapshotGVK.GroupVersion().WithKind("VolumeSnapshotList"),
		&unstructured.UnstructuredList{},
	)
	return s
}

// makeVolumeSnapshotClient is makeClient with a scheme that also understands
// VolumeSnapshots, for the volume-snapshot-enabled schedule tests.
func makeVolumeSnapshotClient(pods ...*corev1.Pod) client.Client {
	builder := fake.NewClientBuilder().WithScheme(snapshotTestScheme())
	for _, p := range pods {
		builder = builder.WithObjects(p).WithStatusSubresource(p)
	}
	return builder.Build()
}

// snapshotRejectingClient fails every VolumeSnapshot create, simulating a CSI
// driver or snapshot-controller that cannot take the snapshot.
type snapshotRejectingClient struct {
	client.Client
}

func (c *snapshotRejectingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if obj.GetObjectKind().GroupVersionKind() == volumeSnapshotGVK {
		return fmt.Errorf("simulated snapshot create failure")
	}
	return c.Client.Create(ctx, obj, opts...)
}

var _ = Describe("runScheduledCheckpoints", func() {
	var mock *mockCheckpointer
	var origPoll time.Duration

	BeforeEach(func() {
		mock = &mockCheckpointer{}
		// Keep the ready-wait short so snapshot readiness polling doesn't slow
		// the suite when a fake snapshot never becomes ready.
		origPoll = snapshotReadyPollInterval
		snapshotReadyPollInterval = 5 * time.Millisecond
	})

	AfterEach(func() { snapshotReadyPollInterval = origPoll })

	It("calls createCheckpoint for every container in every matching Running pod", func() {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-1", Namespace: "default",
				Labels: map[string]string{"app": "test"},
			},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}, {Name: "sidecar"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		created := runScheduledCheckpoints(context.Background(), makeClient(pod), mock, makeSchedule(nil, time.Hour))

		Expect(created).To(Equal(int32(2)))
		Expect(mock.calls).To(HaveLen(2))
		Expect(mock.calls).To(ContainElements(
			checkpointCall{ns: "default", pod: "pod-1", container: "app"},
			checkpointCall{ns: "default", pod: "pod-1", container: "sidecar"},
		))
	})

	It("only checkpoints containers listed in ContainerNames", func() {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-1", Namespace: "default",
				Labels: map[string]string{"app": "test"},
			},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}, {Name: "sidecar"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		created := runScheduledCheckpoints(context.Background(), makeClient(pod), mock, makeSchedule([]string{"app"}, time.Hour))

		Expect(created).To(Equal(int32(1)))
		Expect(mock.calls).To(HaveLen(1))
		Expect(mock.calls[0].container).To(Equal("app"))
	})

	It("takes a volume snapshot before checkpointing when snapshots are enabled", func() {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-1", Namespace: "default",
				Labels: map[string]string{"app": "test"},
			},
			Spec: corev1.PodSpec{
				NodeName: "node-a",
				Volumes: []corev1.Volume{{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "my-pvc"},
					},
				}},
				Containers: []corev1.Container{{
					Name:         "app",
					VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
				}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}

		c := makeVolumeSnapshotClient(pod)
		schedule := makeSchedule([]string{"app"}, time.Hour)
		schedule.Spec.VolumeSnapshots = &v1.VolumeSnapshotConfig{
			Enabled:       true,
			FailurePolicy: v1.FailurePolicyBestEffort,
			ReadyTimeout:  &metav1.Duration{Duration: 20 * time.Millisecond},
		}

		created := runScheduledCheckpoints(context.Background(), c, mock, schedule)

		// The checkpoint still happened (best-effort) ...
		Expect(created).To(Equal(int32(1)))
		Expect(mock.calls).To(HaveLen(1))

		// ... and a VolumeSnapshot was created for the pod's PVC, labelled with
		// the checkpoint identity.
		snaps := &unstructured.UnstructuredList{}
		snaps.SetGroupVersionKind(volumeSnapshotGVK.GroupVersion().WithKind("VolumeSnapshotList"))
		Expect(c.List(context.Background(), snaps, client.InNamespace("default"))).To(Succeed())
		Expect(snaps.Items).To(HaveLen(1))
		labels := snaps.Items[0].GetLabels()
		Expect(labels[v1.LabelNode]).To(Equal("node-a"))
		Expect(labels[v1.LabelPod]).To(Equal("pod-1"))
		Expect(labels[v1.LabelContainer]).To(Equal("app"))
	})

	It("skips a pod's checkpoints when a required snapshot cannot be created", func() {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-1", Namespace: "default",
				Labels: map[string]string{"app": "test"},
			},
			Spec: corev1.PodSpec{
				NodeName: "node-a",
				Volumes: []corev1.Volume{{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "my-pvc"},
					},
				}},
				Containers: []corev1.Container{{
					Name:         "app",
					VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
				}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}

		// A client that rejects VolumeSnapshot creates forces the snapshot to fail.
		c := &snapshotRejectingClient{Client: makeVolumeSnapshotClient(pod)}
		schedule := makeSchedule([]string{"app"}, time.Hour)
		schedule.Spec.VolumeSnapshots = &v1.VolumeSnapshotConfig{
			Enabled:       true,
			FailurePolicy: v1.FailurePolicyRequire,
		}

		created := runScheduledCheckpoints(context.Background(), c, mock, schedule)

		Expect(created).To(Equal(int32(0)))
		Expect(mock.calls).To(BeEmpty())
	})

	It("makes no checkpoint calls when there are no Running pods", func() {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-pending", Namespace: "default",
				Labels: map[string]string{"app": "test"},
			},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			Status: corev1.PodStatus{Phase: corev1.PodPending},
		}
		created := runScheduledCheckpoints(context.Background(), makeClient(pod), mock, makeSchedule(nil, time.Hour))

		Expect(created).To(Equal(int32(0)))
		Expect(mock.calls).To(BeEmpty())
	})
})
