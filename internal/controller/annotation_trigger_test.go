package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

func makeAnnotationTrigger(mock *mockCheckpointer, pods ...*corev1.Pod) *AnnotationTrigger {
	builder := fake.NewClientBuilder().WithScheme(scheme.Scheme)
	for _, p := range pods {
		builder = builder.WithObjects(p).WithStatusSubresource(p)
	}
	sched := &v1.CheckpointSchedule{
		Spec: v1.CheckpointScheduleSpec{
			Namespace: "default",
			Selector:  metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
		},
	}
	return NewAnnotationTrigger(builder.Build(), mock, sched)
}

var _ = Describe("AnnotationTrigger.run", func() {
	var mock *mockCheckpointer

	BeforeEach(func() {
		mock = &mockCheckpointer{}
	})

	It("checkpoints a pod that has the trigger annotation and clears it", func() {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-1", Namespace: "default",
				Labels:      map[string]string{"app": "test"},
				Annotations: map[string]string{CheckpointTriggerAnnotation: "true"},
			},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		trigger := makeAnnotationTrigger(mock, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(HaveLen(1))
		Expect(mock.calls[0].container).To(Equal("app"))

		// annotation must be cleared
		updated := &corev1.Pod{}
		Expect(trigger.client.Get(context.Background(), client.ObjectKeyFromObject(pod), updated)).To(Succeed())
		Expect(updated.Annotations).NotTo(HaveKey(CheckpointTriggerAnnotation))
	})

	It("skips pods that do not have the trigger annotation", func() {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-1", Namespace: "default",
				Labels: map[string]string{"app": "test"},
			},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		trigger := makeAnnotationTrigger(mock, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(BeEmpty())
	})
})
