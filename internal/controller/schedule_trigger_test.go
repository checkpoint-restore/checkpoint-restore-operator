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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

// mockCheckpointer records every createCheckpoint call for assertions.
type mockCheckpointer struct {
	mu    sync.Mutex
	calls []checkpointCall
}

type checkpointCall struct {
	ns, pod, container, node string
}

func (m *mockCheckpointer) createCheckpoint(_ context.Context, ns, pod, container, node string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, checkpointCall{ns, pod, container, node})
	return nil
}

func makeTrigger(mock *mockCheckpointer, containerNames []string, pods ...*corev1.Pod) *ScheduleTrigger {
	builder := fake.NewClientBuilder().WithScheme(scheme.Scheme)
	for _, p := range pods {
		builder = builder.WithObjects(p).WithStatusSubresource(p)
	}
	sched := &v1.CheckpointSchedule{
		Spec: v1.CheckpointScheduleSpec{
			Namespace:      "default",
			ContainerNames: containerNames,
			Selector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Triggers:       v1.TriggersSpec{Schedule: "@every 1h"},
		},
	}
	return NewScheduleTrigger(builder.Build(), mock, sched)
}

var _ = Describe("ScheduleTrigger.runCheckpoints", func() {
	var mock *mockCheckpointer

	BeforeEach(func() {
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
		trigger := makeTrigger(mock, nil, pod)
		trigger.runCheckpoints(context.Background())

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
		trigger := makeTrigger(mock, []string{"app"}, pod)
		trigger.runCheckpoints(context.Background())

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
		trigger := makeTrigger(mock, nil, pod)
		trigger.runCheckpoints(context.Background())

		Expect(mock.calls).To(BeEmpty())
	})
})

var _ = Describe("ScheduleTrigger Start/Stop", func() {
	It("starts and stops without error", func() {
		sched := &v1.CheckpointSchedule{
			Spec: v1.CheckpointScheduleSpec{
				Namespace: "default",
				Selector:  metav1.LabelSelector{},
				Triggers:  v1.TriggersSpec{Schedule: "@every 1h"},
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		trigger := NewScheduleTrigger(fakeClient, &mockCheckpointer{}, sched)

		Expect(trigger.Start(context.Background())).To(Succeed())
		trigger.Stop()
	})

	It("returns an error for an invalid cron expression", func() {
		sched := &v1.CheckpointSchedule{
			Spec: v1.CheckpointScheduleSpec{
				Triggers: v1.TriggersSpec{Schedule: "not-a-valid-cron"},
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		trigger := NewScheduleTrigger(fakeClient, &mockCheckpointer{}, sched)

		Expect(trigger.Start(context.Background())).To(HaveOccurred())
	})
})
