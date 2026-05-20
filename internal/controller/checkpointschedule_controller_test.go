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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

var _ = Describe("CheckpointScheduleReconciler interval trigger", func() {
	var mock *mockCheckpointer

	BeforeEach(func() {
		Expect(v1.AddToScheme(scheme.Scheme)).To(Succeed())
		mock = &mockCheckpointer{}
	})

	makeReconciler := func(schedule *v1.CheckpointSchedule, pods ...*corev1.Pod) *CheckpointScheduleReconciler {
		builder := fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(schedule).
			WithStatusSubresource(schedule)
		for _, p := range pods {
			builder = builder.WithObjects(p)
		}
		return &CheckpointScheduleReconciler{
			Client:       builder.Build(),
			Scheme:       scheme.Scheme,
			Checkpointer: mock,
		}
	}

	scheduleWithInterval := func(interval time.Duration, created time.Time) *v1.CheckpointSchedule {
		return &v1.CheckpointSchedule{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-schedule",
				Namespace:         "default",
				CreationTimestamp: metav1.Time{Time: created},
			},
			Spec: v1.CheckpointScheduleSpec{
				Namespace: "default",
				Selector:  metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
				Triggers:  v1.TriggersSpec{Interval: &metav1.Duration{Duration: interval}},
			},
		}
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-schedule"}}

	It("requeues for the remaining interval when no checkpoint is due", func() {
		schedule := scheduleWithInterval(time.Hour, time.Now())
		r := makeReconciler(schedule)

		res, err := r.Reconcile(context.Background(), req)

		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 59*time.Minute))
		Expect(res.RequeueAfter).To(BeNumerically("<=", time.Hour))
		Expect(mock.calls).To(BeEmpty())
	})

	It("checkpoints matching pods and records status when the interval has elapsed", func() {
		schedule := scheduleWithInterval(time.Hour, time.Now().Add(-2*time.Hour))
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-1", Namespace: "default",
				Labels: map[string]string{"app": "test"},
			},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		r := makeReconciler(schedule, pod)

		res, err := r.Reconcile(context.Background(), req)

		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(time.Hour))
		Expect(mock.calls).To(HaveLen(1))

		updated := &v1.CheckpointSchedule{}
		Expect(r.Get(context.Background(), req.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.LastCheckpointTime).NotTo(BeNil())
		Expect(updated.Status.CheckpointsCreated).To(Equal(1))
	})

	It("anchors the next run on status.lastCheckpointTime", func() {
		schedule := scheduleWithInterval(time.Hour, time.Now().Add(-24*time.Hour))
		schedule.Status.LastCheckpointTime = &metav1.Time{Time: time.Now().Add(-10 * time.Minute)}
		r := makeReconciler(schedule)

		res, err := r.Reconcile(context.Background(), req)

		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 49*time.Minute))
		Expect(res.RequeueAfter).To(BeNumerically("<=", 50*time.Minute))
		Expect(mock.calls).To(BeEmpty())
	})
})
