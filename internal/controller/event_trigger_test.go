package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

func buildEventTrigger(mock *mockCheckpointer, events []string, nodes []*corev1.Node, pods ...*corev1.Pod) *EventTrigger {
	builder := fake.NewClientBuilder().WithScheme(scheme.Scheme)
	for _, n := range nodes {
		builder = builder.WithObjects(n)
	}
	for _, p := range pods {
		builder = builder.WithObjects(p)
	}
	sched := &v1.CheckpointSchedule{
		Spec: v1.CheckpointScheduleSpec{
			Namespace: "default",
			Selector:  metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Triggers:  v1.TriggersSpec{OnKubernetesEvents: events},
		},
	}
	return NewEventTrigger(builder.Build(), mock, sched)
}

func testNode(name string, unschedulable bool) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: unschedulable},
	}
}

func testPod(name, nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels: map[string]string{"app": "test"},
		},
		Spec: corev1.PodSpec{
			NodeName:   nodeName,
			Containers: []corev1.Container{{Name: "app"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

var _ = Describe("EventTrigger — NodeDrain", func() {
	var mock *mockCheckpointer

	BeforeEach(func() { mock = &mockCheckpointer{} })

	It("checkpoints pods on an unschedulable node", func() {
		node := testNode("node-1", true)
		pod := testPod("pod-1", "node-1")
		trigger := buildEventTrigger(mock, []string{EventNodeDrain}, []*corev1.Node{node}, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(HaveLen(1))
		Expect(mock.calls[0].pod).To(Equal("pod-1"))
	})

	It("does not checkpoint pods on a schedulable node", func() {
		node := testNode("node-1", false)
		pod := testPod("pod-1", "node-1")
		trigger := buildEventTrigger(mock, []string{EventNodeDrain}, []*corev1.Node{node}, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(BeEmpty())
	})

	It("checkpoints each drain only once until node becomes schedulable again", func() {
		node := testNode("node-1", true)
		pod := testPod("pod-1", "node-1")
		trigger := buildEventTrigger(mock, []string{EventNodeDrain}, []*corev1.Node{node}, pod)

		trigger.run(context.Background())
		trigger.run(context.Background()) // second run — node still draining

		Expect(mock.calls).To(HaveLen(1))
	})

	It("re-checkpoints after a node finishes draining and drains again", func() {
		node := testNode("node-1", true)
		pod := testPod("pod-1", "node-1")
		trigger := buildEventTrigger(mock, []string{EventNodeDrain}, []*corev1.Node{node}, pod)

		trigger.run(context.Background())
		Expect(mock.calls).To(HaveLen(1))

		// simulate node becoming schedulable (drain complete)
		trigger.mu.Lock()
		delete(trigger.seenNodes, "node-1")
		trigger.mu.Unlock()

		trigger.run(context.Background())
		Expect(mock.calls).To(HaveLen(2))
	})
})

var _ = Describe("EventTrigger — PodEviction", func() {
	var mock *mockCheckpointer

	BeforeEach(func() { mock = &mockCheckpointer{} })

	It("checkpoints a pod with a deletion timestamp", func() {
		now := metav1.NewTime(time.Now())
		pod := testPod("pod-1", "node-1")
		pod.Finalizers = []string{"test/keep-alive"} // required by fake client to accept DeletionTimestamp
		pod.DeletionTimestamp = &now

		trigger := buildEventTrigger(mock, []string{EventPodEviction}, nil, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(HaveLen(1))
		Expect(mock.calls[0].pod).To(Equal("pod-1"))
	})

	It("does not checkpoint a running pod without a deletion timestamp", func() {
		pod := testPod("pod-1", "node-1")
		trigger := buildEventTrigger(mock, []string{EventPodEviction}, nil, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(BeEmpty())
	})

	It("checkpoints an evicting pod only once", func() {
		now := metav1.NewTime(time.Now())
		pod := testPod("pod-1", "node-1")
		pod.Finalizers = []string{"test/keep-alive"}
		pod.DeletionTimestamp = &now

		trigger := buildEventTrigger(mock, []string{EventPodEviction}, nil, pod)
		trigger.run(context.Background())
		trigger.run(context.Background())

		Expect(mock.calls).To(HaveLen(1))
	})
})

var _ = Describe("EventTrigger — Preemption (k8s 1.26+)", func() {
	var mock *mockCheckpointer

	BeforeEach(func() { mock = &mockCheckpointer{} })

	It("checkpoints a pod with DisruptionTarget/PreemptingEvictor condition", func() {
		pod := testPod("pod-1", "node-1")
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:   disruptionTargetCondition,
			Reason: preemptingEvictorReason,
			Status: corev1.ConditionTrue,
		}}

		trigger := buildEventTrigger(mock, []string{EventPreemption}, nil, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(HaveLen(1))
		Expect(mock.calls[0].pod).To(Equal("pod-1"))
	})

	It("does not checkpoint a pod without the preemption condition", func() {
		pod := testPod("pod-1", "node-1")
		trigger := buildEventTrigger(mock, []string{EventPreemption}, nil, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(BeEmpty())
	})

	It("checkpoints a preempted pod only once", func() {
		pod := testPod("pod-1", "node-1")
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:   disruptionTargetCondition,
			Reason: preemptingEvictorReason,
			Status: corev1.ConditionTrue,
		}}

		trigger := buildEventTrigger(mock, []string{EventPreemption}, nil, pod)
		trigger.run(context.Background())
		trigger.run(context.Background())

		Expect(mock.calls).To(HaveLen(1))
	})
})

var _ = Describe("EventTrigger — event filtering", func() {
	It("does not fire NodeDrain when only PodEviction is configured", func() {
		mock := &mockCheckpointer{}
		node := testNode("node-1", true)
		pod := testPod("pod-1", "node-1")

		trigger := buildEventTrigger(mock, []string{EventPodEviction}, []*corev1.Node{node}, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(BeEmpty())
	})
})