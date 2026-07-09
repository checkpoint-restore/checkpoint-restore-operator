package controller

import (
	"context"
	"errors"

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

func makeAnnotationTrigger(mock *mockCheckpointer, containerNames []string, pods ...*corev1.Pod) *AnnotationTrigger {
	builder := fake.NewClientBuilder().WithScheme(scheme.Scheme)
	for _, p := range pods {
		builder = builder.WithObjects(p).WithStatusSubresource(p)
	}
	sched := &v1.CheckpointSchedule{
		Spec: v1.CheckpointScheduleSpec{
			Namespace:      "default",
			ContainerNames: containerNames,
			Selector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
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
		trigger := makeAnnotationTrigger(mock, nil, pod)
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
		trigger := makeAnnotationTrigger(mock, nil, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(BeEmpty())
	})
})

var _ = Describe("AnnotationTrigger.run records checkpoint archives", func() {
	It("creates a CheckpointArchive when the global policy opts in", func() {
		Expect(v1.AddToScheme(scheme.Scheme)).To(Succeed())
		resetAllPoliciesToDefault(logr.Discard())
		DeferCleanup(func() { resetAllPoliciesToDefault(logr.Discard()) })
		(&CheckpointRestoreOperatorReconciler{}).handleGlobalPolicies(logr.Discard(), &v1.GlobalPolicySpec{
			UploadToExternalStorage: ptr(true),
		})

		mock := &mockCheckpointer{path: "/var/lib/kubelet/checkpoints/x.tar"}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-1", Namespace: "default",
				Labels:      map[string]string{"app": "test"},
				Annotations: map[string]string{CheckpointTriggerAnnotation: "true"},
			},
			Spec:   corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "app"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		trigger := makeAnnotationTrigger(mock, nil, pod)
		trigger.run(context.Background())

		var list v1.CheckpointArchiveList
		Expect(trigger.client.List(context.Background(), &list)).To(Succeed())
		Expect(list.Items).To(HaveLen(1))
		Expect(list.Items[0].Spec.Node).To(Equal("node-a"))
	})
})

var _ = Describe("AnnotationTrigger.run with ContainerNames", func() {
	It("only checkpoints containers listed in ContainerNames", func() {
		mock := &mockCheckpointer{}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-1", Namespace: "default",
				Labels:      map[string]string{"app": "test"},
				Annotations: map[string]string{CheckpointTriggerAnnotation: "true"},
			},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}, {Name: "sidecar"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		trigger := makeAnnotationTrigger(mock, []string{"app"}, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(HaveLen(1))
		Expect(mock.calls[0].container).To(Equal("app"))
	})
})

var _ = Describe("AnnotationTrigger.run on checkpoint failure", func() {
	It("keeps the annotation so the request is retried on the next poll", func() {
		mock := &mockCheckpointer{err: errors.New("kubelet unavailable")}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-1", Namespace: "default",
				Labels:      map[string]string{"app": "test"},
				Annotations: map[string]string{CheckpointTriggerAnnotation: "true"},
			},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		trigger := makeAnnotationTrigger(mock, nil, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(HaveLen(1))
		updated := &corev1.Pod{}
		Expect(trigger.client.Get(context.Background(), client.ObjectKeyFromObject(pod), updated)).To(Succeed())
		Expect(updated.Annotations).To(HaveKeyWithValue(CheckpointTriggerAnnotation, "true"))

		// once the checkpoint succeeds the annotation is consumed
		mock.err = nil
		trigger.run(context.Background())
		Expect(mock.calls).To(HaveLen(2))
		Expect(trigger.client.Get(context.Background(), client.ObjectKeyFromObject(pod), updated)).To(Succeed())
		Expect(updated.Annotations).NotTo(HaveKey(CheckpointTriggerAnnotation))
	})
})
