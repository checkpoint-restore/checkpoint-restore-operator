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
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	path  string
	calls []checkpointCall
}

type checkpointCall struct {
	ns, pod, container, node string
}

func (m *mockCheckpointer) createCheckpoint(_ context.Context, ns, pod, container, node string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, checkpointCall{ns, pod, container, node})
	return m.path, m.err
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

var _ = Describe("runScheduledCheckpoints", func() {
	var mock *mockCheckpointer

	BeforeEach(func() {
		Expect(v1.AddToScheme(scheme.Scheme)).To(Succeed())
		mock = &mockCheckpointer{}
	})

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

	It("records a CheckpointArchive when the global policy opts in", func() {
		resetAllPoliciesToDefault(logr.Discard())
		DeferCleanup(func() { resetAllPoliciesToDefault(logr.Discard()) })
		(&CheckpointRestoreOperatorReconciler{}).handleGlobalPolicies(logr.Discard(), &v1.GlobalPolicySpec{
			UploadToExternalStorage: ptr(true),
		})

		mock.path = "/var/lib/kubelet/checkpoints/x.tar"
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-1", Namespace: "default",
				Labels: map[string]string{"app": "test"},
			},
			Spec:   corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "app"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		c := makeClient(pod)

		created := runScheduledCheckpoints(context.Background(), c, mock, makeSchedule(nil, time.Hour))
		Expect(created).To(Equal(int32(1)))

		var list v1.CheckpointArchiveList
		Expect(c.List(context.Background(), &list)).To(Succeed())
		Expect(list.Items).To(HaveLen(1))
		Expect(list.Items[0].Spec.Node).To(Equal("node-a"))
	})
})
