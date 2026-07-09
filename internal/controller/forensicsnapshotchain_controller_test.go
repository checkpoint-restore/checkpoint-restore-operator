/*
Copyright 2024.

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
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ctrl "sigs.k8s.io/controller-runtime"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

var _ = Describe("ForensicSnapshotChainReconciler", func() {

	BeforeEach(func() {
		Expect(criuorgv1.AddToScheme(scheme.Scheme)).To(Succeed())
	})

	makeReconciler := func(chain *criuorgv1.ForensicSnapshotChain) *ForensicSnapshotChainReconciler {
		c := fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(chain).
			WithStatusSubresource(chain).
			Build()

		return &ForensicSnapshotChainReconciler{
			Client: c,
			Scheme: scheme.Scheme,
		}
	}

	It("should initialize a new chain with Pending phase", func() {

		chain := &criuorgv1.ForensicSnapshotChain{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-chain",
				Namespace: "default",
			},
		}

		reconciler := makeReconciler(chain)

		request := ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-chain",
				Namespace: "default",
			},
		}

		_, err := reconciler.Reconcile(context.Background(), request)

		Expect(err).ToNot(HaveOccurred())

		updatedChain := &criuorgv1.ForensicSnapshotChain{}
		Expect(reconciler.Get(context.Background(), request.NamespacedName, updatedChain)).To(Succeed())
		Expect(updatedChain.Status.Phase).To(Equal(criuorgv1.PhasePending))
	})

	It("should transition from Pending to Running phase", func() {

		chain := &criuorgv1.ForensicSnapshotChain{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-chain",
				Namespace: "default",
			},
		}

		chain.Status.Phase = criuorgv1.PhasePending

		reconciler := makeReconciler(chain)

		request := ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-chain",
				Namespace: "default",
			},
		}

		_, err := reconciler.Reconcile(context.Background(), request)

		Expect(err).ToNot(HaveOccurred())

		updatedChain := &criuorgv1.ForensicSnapshotChain{}
		Expect(reconciler.Get(context.Background(), request.NamespacedName, updatedChain)).To(Succeed())
		Expect(updatedChain.Status.Phase).To(Equal(criuorgv1.PhaseRunning))

	})

	It("should return immediately when already in Completed phase", func() {

		chain := &criuorgv1.ForensicSnapshotChain{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-chain",
				Namespace: "default",
			},
		}

		chain.Status.Phase = criuorgv1.PhaseCompleted

		reconciler := makeReconciler(chain)

		request := ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-chain",
				Namespace: "default",
			},
		}

		result, err := reconciler.Reconcile(context.Background(), request)

		Expect(err).ToNot(HaveOccurred())

		Expect(result).To(Equal(ctrl.Result{}))
	})

	It("should return immediately when already in Failed phase", func() {

		chain := &criuorgv1.ForensicSnapshotChain{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-chain",
				Namespace: "default",
			},
		}

		chain.Status.Phase = criuorgv1.PhaseFailed

		reconciler := makeReconciler(chain)

		request := ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      "test-chain",
				Namespace: "default",
			},
		}

		result, err := reconciler.Reconcile(context.Background(), request)

		Expect(err).ToNot(HaveOccurred())

		Expect(result).To(Equal(ctrl.Result{}))
	})

	// --- Running phase ---

	makeRunningReconciler := func(chain *criuorgv1.ForensicSnapshotChain, mock Checkpointer, pods ...*corev1.Pod) *ForensicSnapshotChainReconciler {
		builder := fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(chain).
			WithStatusSubresource(chain)
		for _, p := range pods {
			builder = builder.WithObjects(p).WithStatusSubresource(p)
		}
		return &ForensicSnapshotChainReconciler{
			Client:       builder.Build(),
			Scheme:       scheme.Scheme,
			Checkpointer: mock,
		}
	}

	runningChain := func() *criuorgv1.ForensicSnapshotChain {
		start := metav1.Now()
		chain := &criuorgv1.ForensicSnapshotChain{
			ObjectMeta: metav1.ObjectMeta{Name: "test-chain", Namespace: "default"},
			Spec: criuorgv1.ForensicSnapshotChainSpec{
				Namespace: "default",
				Selector:  metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			},
		}
		chain.Status.Phase = criuorgv1.PhaseRunning
		chain.Status.StartTime = &start
		return chain
	}

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-chain", Namespace: "default"}}

	It("checkpoints every container once per round and counts the round once", func() {
		chain := runningChain()
		mock := &mockCheckpointer{}
		pod := runningPod("pod-a", map[string]string{"app": "test"}, "c1", "c2")
		r := makeRunningReconciler(chain, mock, pod)

		_, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())
		Expect(mock.calls).To(HaveLen(2))

		updated := &criuorgv1.ForensicSnapshotChain{}
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseRunning))
		Expect(updated.Status.SnapshotCount).To(Equal(int32(1)))
	})

	It("completes once maxSnapshots rounds have been captured", func() {
		chain := runningChain()
		max := int32(2)
		chain.Spec.Capture.MaxSnapshots = &max
		mock := &mockCheckpointer{}
		pod := runningPod("pod-a", map[string]string{"app": "test"}, "c1")
		r := makeRunningReconciler(chain, mock, pod)

		updated := &criuorgv1.ForensicSnapshotChain{}

		_, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseRunning))
		Expect(updated.Status.SnapshotCount).To(Equal(int32(1)))

		markChainDue(r, request.NamespacedName)
		_, err = r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseCompleted))
		Expect(updated.Status.SnapshotCount).To(Equal(int32(2)))
		Expect(updated.Status.CompletionTime).ToNot(BeNil())
	})

	It("completes via maxDuration even when no pods match", func() {
		chain := runningChain()
		past := metav1.NewTime(time.Now().Add(-time.Hour))
		chain.Status.StartTime = &past
		chain.Spec.Capture.MaxDuration = &metav1.Duration{Duration: time.Minute}
		mock := &mockCheckpointer{}
		r := makeRunningReconciler(chain, mock) // no pods match the selector

		_, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())
		Expect(mock.calls).To(BeEmpty())

		updated := &criuorgv1.ForensicSnapshotChain{}
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseCompleted))
		Expect(updated.Status.CompletionTime).ToNot(BeNil())
	})

	It("keeps the chain Running and requeues when a checkpoint fails", func() {
		chain := runningChain()
		mock := &mockCheckpointer{err: fmt.Errorf("kubelet checkpoint failed")}
		pod := runningPod("pod-a", map[string]string{"app": "test"}, "c1")
		r := makeRunningReconciler(chain, mock, pod)

		// A checkpoint error is transient: the reconcile returns the error so
		// it is retried with backoff, but the chain must not be terminally
		// Failed and must record the error for observability.
		_, err := r.Reconcile(context.Background(), request)
		Expect(err).To(HaveOccurred())

		updated := &criuorgv1.ForensicSnapshotChain{}
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseRunning))
		Expect(updated.Status.ErrorMessage).To(ContainSubstring("kubelet checkpoint failed"))
		cond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
		Expect(cond).ToNot(BeNil())
		Expect(cond.Reason).To(Equal("CheckpointError"))
	})

	It("moves to Failed after consecutive checkpoint failures when no maxDuration is set", func() {
		// A maxSnapshots-only chain whose target cannot be checkpointed must
		// still terminate: after maxConsecutiveCheckpointFailures failed rounds
		// it gives up rather than retrying forever.
		chain := runningChain()
		max := int32(10)
		chain.Spec.Capture.MaxSnapshots = &max
		mock := &mockCheckpointer{err: fmt.Errorf("kubelet checkpoint failed")}
		pod := runningPod("pod-a", map[string]string{"app": "test"}, "c1")
		r := makeRunningReconciler(chain, mock, pod)

		updated := &criuorgv1.ForensicSnapshotChain{}
		for i := 0; i < maxConsecutiveCheckpointFailures; i++ {
			_, _ = r.Reconcile(context.Background(), request)
			Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		}

		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseFailed))
		Expect(updated.Status.FailureCount).To(Equal(int32(maxConsecutiveCheckpointFailures)))
		Expect(updated.Status.CompletionTime).ToNot(BeNil())
		cond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
		Expect(cond).ToNot(BeNil())
		Expect(cond.Reason).To(Equal("CheckpointFailed"))
	})

	It("keeps retrying checkpoint failures while maxDuration has not elapsed", func() {
		// With a time backstop, persistent failures do not trip the
		// consecutive-failure cap; the chain stays Running until maxDuration.
		chain := runningChain()
		chain.Spec.Capture.MaxDuration = &metav1.Duration{Duration: time.Hour}
		mock := &mockCheckpointer{err: fmt.Errorf("kubelet checkpoint failed")}
		pod := runningPod("pod-a", map[string]string{"app": "test"}, "c1")
		r := makeRunningReconciler(chain, mock, pod)

		updated := &criuorgv1.ForensicSnapshotChain{}
		for i := 0; i < maxConsecutiveCheckpointFailures+2; i++ {
			_, _ = r.Reconcile(context.Background(), request)
			Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		}

		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseRunning))
		Expect(updated.Status.FailureCount).To(BeNumerically(">", int32(maxConsecutiveCheckpointFailures)))
	})

	It("resets the failure count after a successful round", func() {
		chain := runningChain()
		chain.Status.FailureCount = 3
		mock := &mockCheckpointer{}
		pod := runningPod("pod-a", map[string]string{"app": "test"}, "c1")
		r := makeRunningReconciler(chain, mock, pod)

		_, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())

		updated := &criuorgv1.ForensicSnapshotChain{}
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.FailureCount).To(Equal(int32(0)))
		Expect(updated.Status.SnapshotCount).To(Equal(int32(1)))
	})

	It("takes no action and does not requeue on an unrecognized phase", func() {
		chain := runningChain()
		chain.Status.Phase = criuorgv1.SnapshotChainPhase("Bogus")
		mock := &mockCheckpointer{}
		r := makeRunningReconciler(chain, mock)

		result, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())
		Expect(result).To(Equal(ctrl.Result{}))
		Expect(mock.calls).To(BeEmpty())
	})

	It("deletes matching pods on completion when postSnapshotAction is DeletePod", func() {
		chain := runningChain()
		max := int32(1)
		chain.Spec.Capture.MaxSnapshots = &max
		chain.Spec.PostSnapshotAction = criuorgv1.PostSnapshotActionDeletePod
		mock := &mockCheckpointer{}
		pod := runningPod("pod-a", map[string]string{"app": "test"}, "c1")
		r := makeRunningReconciler(chain, mock, pod)

		_, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())

		updated := &criuorgv1.ForensicSnapshotChain{}
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseCompleted))

		gotPod := &corev1.Pod{}
		err = r.Get(context.Background(), types.NamespacedName{Name: "pod-a", Namespace: "default"}, gotPod)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("deletes matching pods on maxDuration completion when postSnapshotAction is DeletePod", func() {
		chain := runningChain()
		past := metav1.NewTime(time.Now().Add(-time.Hour))
		chain.Status.StartTime = &past
		chain.Status.SnapshotCount = 1
		chain.Spec.Capture.MaxDuration = &metav1.Duration{Duration: time.Minute}
		chain.Spec.PostSnapshotAction = criuorgv1.PostSnapshotActionDeletePod
		mock := &mockCheckpointer{}
		pod := runningPod("pod-a", map[string]string{"app": "test"}, "c1")
		r := makeRunningReconciler(chain, mock, pod)

		_, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())

		updated := &criuorgv1.ForensicSnapshotChain{}
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseCompleted))

		gotPod := &corev1.Pod{}
		err = r.Get(context.Background(), types.NamespacedName{Name: "pod-a", Namespace: "default"}, gotPod)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("does not delete pods on maxDuration completion before any snapshot was captured", func() {
		chain := runningChain()
		past := metav1.NewTime(time.Now().Add(-time.Hour))
		chain.Status.StartTime = &past
		chain.Spec.Capture.MaxDuration = &metav1.Duration{Duration: time.Minute}
		chain.Spec.PostSnapshotAction = criuorgv1.PostSnapshotActionDeletePod
		mock := &mockCheckpointer{}
		pod := runningPod("pod-a", map[string]string{"app": "test"}, "c1")
		r := makeRunningReconciler(chain, mock, pod)

		_, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())

		updated := &criuorgv1.ForensicSnapshotChain{}
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseCompleted))
		Expect(updated.Status.SnapshotCount).To(Equal(int32(0)))

		gotPod := &corev1.Pod{}
		Expect(r.Get(context.Background(), types.NamespacedName{Name: "pod-a", Namespace: "default"}, gotPod)).To(Succeed())
	})

	It("does not delete pods on completion when postSnapshotAction is None", func() {
		chain := runningChain()
		max := int32(1)
		chain.Spec.Capture.MaxSnapshots = &max
		chain.Spec.PostSnapshotAction = criuorgv1.PostSnapshotActionNone
		mock := &mockCheckpointer{}
		pod := runningPod("pod-a", map[string]string{"app": "test"}, "c1")
		r := makeRunningReconciler(chain, mock, pod)

		_, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())

		updated := &criuorgv1.ForensicSnapshotChain{}
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseCompleted))

		gotPod := &corev1.Pod{}
		Expect(r.Get(context.Background(), types.NamespacedName{Name: "pod-a", Namespace: "default"}, gotPod)).To(Succeed())
	})

	It("counts capture rounds, not individual checkpoints, toward maxSnapshots", func() {
		chain := runningChain()
		max := int32(2)
		chain.Spec.Capture.MaxSnapshots = &max
		mock := &mockCheckpointer{}
		pod := runningPod("pod-a", map[string]string{"app": "test"}, "c1", "c2")
		r := makeRunningReconciler(chain, mock, pod)

		// Two pods-worth of containers per round; 2 rounds must complete the
		// chain (SnapshotCount == rounds), producing 4 checkpoint calls.
		_, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())
		markChainDue(r, request.NamespacedName)
		_, err = r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())

		updated := &criuorgv1.ForensicSnapshotChain{}
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseCompleted))
		Expect(updated.Status.SnapshotCount).To(Equal(int32(2)))
		Expect(mock.calls).To(HaveLen(4))
	})

	It("does not count an empty round (no matching pods) toward maxSnapshots", func() {
		chain := runningChain()
		// maxSnapshots is 2 and maxDuration is set, so a single empty round
		// neither increments SnapshotCount nor terminates the chain.
		max := int32(2)
		chain.Spec.Capture.MaxSnapshots = &max
		chain.Spec.Capture.MaxDuration = &metav1.Duration{Duration: time.Hour}
		mock := &mockCheckpointer{}
		r := makeRunningReconciler(chain, mock) // selector matches no pods

		_, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())
		Expect(mock.calls).To(BeEmpty())

		updated := &criuorgv1.ForensicSnapshotChain{}
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseRunning))
		Expect(updated.Status.SnapshotCount).To(Equal(int32(0)))
		Expect(updated.Status.LastSnapshotTime).ToNot(BeNil())
	})

	It("terminates a maxSnapshots-only chain whose selector never matches", func() {
		// With no maxDuration backstop, empty rounds must still bound the
		// chain: it completes once maxSnapshots rounds have been attempted,
		// rather than requeuing forever.
		chain := runningChain()
		max := int32(2)
		chain.Spec.Capture.MaxSnapshots = &max
		mock := &mockCheckpointer{}
		r := makeRunningReconciler(chain, mock) // selector matches no pods

		updated := &criuorgv1.ForensicSnapshotChain{}

		_, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseRunning))
		Expect(updated.Status.AttemptCount).To(Equal(int32(1)))

		markChainDue(r, request.NamespacedName)
		_, err = r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseCompleted))
		Expect(updated.Status.SnapshotCount).To(Equal(int32(0)))
		Expect(updated.Status.CompletionTime).ToNot(BeNil())
		Expect(mock.calls).To(BeEmpty())
		cond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
		Expect(cond).ToNot(BeNil())
		Expect(cond.Reason).To(Equal("NoMatchingPods"))
	})

	It("does not terminate early as NoMatchingPods when pods appear after an empty round", func() {
		// Regression: the attemptCount backstop must not cut off a chain that
		// is still capturing. A maxSnapshots-only chain whose pods start after
		// the first (empty) round must still reach maxSnapshots, not complete
		// as NoMatchingPods on the round the pods first appear.
		chain := runningChain()
		max := int32(2)
		chain.Spec.Capture.MaxSnapshots = &max
		mock := &mockCheckpointer{}
		r := makeRunningReconciler(chain, mock) // no pods yet

		updated := &criuorgv1.ForensicSnapshotChain{}

		// Round 1: empty (no pods). attemptCount=1, still Running.
		_, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseRunning))
		Expect(updated.Status.AttemptCount).To(Equal(int32(1)))

		// Pods appear; attemptCount(==2) now equals maxSnapshots, but the round
		// captures, so it must NOT complete as NoMatchingPods.
		pod := runningPod("pod-a", map[string]string{"app": "test"}, "c1")
		Expect(r.Create(context.Background(), pod)).To(Succeed())
		markChainDue(r, request.NamespacedName)
		_, err = r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseRunning))
		Expect(updated.Status.SnapshotCount).To(Equal(int32(1)))

		// Final capture reaches maxSnapshots.
		markChainDue(r, request.NamespacedName)
		_, err = r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseCompleted))
		Expect(updated.Status.SnapshotCount).To(Equal(int32(2)))
		cond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
		Expect(cond.Reason).To(Equal("MaxSnapshotsReached"))
	})

	It("clears a recorded checkpoint error after a later successful round", func() {
		chain := runningChain()
		chain.Status.ErrorMessage = "kubelet checkpoint failed"
		meta.SetStatusCondition(&chain.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  "CheckpointError",
			Message: "kubelet checkpoint failed",
		})
		mock := &mockCheckpointer{}
		pod := runningPod("pod-a", map[string]string{"app": "test"}, "c1")
		r := makeRunningReconciler(chain, mock, pod)

		_, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())

		updated := &criuorgv1.ForensicSnapshotChain{}
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.ErrorMessage).To(BeEmpty())
		cond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
		Expect(cond).ToNot(BeNil())
		Expect(cond.Reason).To(Equal("Running"))
	})

	It("does not capture again before the interval elapses after a status update", func() {
		chain := runningChain()
		chain.Spec.Capture.Interval = &metav1.Duration{Duration: 10 * time.Second}
		mock := &mockCheckpointer{}
		pod := runningPod("pod-a", map[string]string{"app": "test"}, "c1")
		r := makeRunningReconciler(chain, mock, pod)

		result, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(10 * time.Second))
		Expect(mock.calls).To(HaveLen(1))

		result, err = r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", 10*time.Second))
		Expect(mock.calls).To(HaveLen(1))
	})

	It("requeues after the default interval when none is configured", func() {
		chain := runningChain()
		mock := &mockCheckpointer{}
		pod := runningPod("pod-a", map[string]string{"app": "test"}, "c1")
		r := makeRunningReconciler(chain, mock, pod)

		result, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(DefaultInterval))
	})

	It("requeues after the configured interval", func() {
		chain := runningChain()
		chain.Spec.Capture.Interval = &metav1.Duration{Duration: 10 * time.Second}
		mock := &mockCheckpointer{}
		pod := runningPod("pod-a", map[string]string{"app": "test"}, "c1")
		r := makeRunningReconciler(chain, mock, pod)

		result, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(10 * time.Second))
	})

	It("tolerates an already-deleted pod during the DeletePod action", func() {
		chain := runningChain()
		chain.Spec.PostSnapshotAction = criuorgv1.PostSnapshotActionDeletePod
		mock := &mockCheckpointer{}
		r := makeRunningReconciler(chain, mock) // pod below is not stored in the client

		absent := runningPod("ghost", map[string]string{"app": "test"}, "c1")
		Expect(r.executePostSnapshotAction(context.Background(), chain, []corev1.Pod{*absent}, 1)).To(Succeed())
	})

	// --- Integrity verification ---

	It("fails the chain when hashAlgorithm is unsupported", func() {
		chain := runningChain()
		chain.Spec.Integrity = criuorgv1.IntegritySpec{HashAlgorithm: "md5"}
		mock := &mockCheckpointer{}
		r := makeRunningReconciler(chain, mock)

		_, err := r.Reconcile(context.Background(), request)
		Expect(err).To(HaveOccurred())

		updated := &criuorgv1.ForensicSnapshotChain{}
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(criuorgv1.PhaseFailed))
		Expect(updated.Status.ErrorMessage).To(ContainSubstring("md5"))
		Expect(mock.calls).To(BeEmpty())
	})

	It("records a CheckpointArchive when the global policy opts in", func() {
		resetAllPoliciesToDefault(logr.Discard())
		DeferCleanup(func() { resetAllPoliciesToDefault(logr.Discard()) })
		(&CheckpointRestoreOperatorReconciler{}).handleGlobalPolicies(logr.Discard(), &criuorgv1.GlobalPolicySpec{
			UploadToExternalStorage: ptr(true),
		})

		chain := runningChain()
		mock := &mockCheckpointer{path: "/var/lib/kubelet/checkpoints/x.tar"}
		pod := runningPod("pod-a", map[string]string{"app": "test"}, "c1")
		r := makeRunningReconciler(chain, mock, pod)

		_, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())

		var list criuorgv1.CheckpointArchiveList
		Expect(r.List(context.Background(), &list)).To(Succeed())
		Expect(list.Items).To(HaveLen(1))
		Expect(list.Items[0].Spec.Node).To(Equal("node-1"))
	})

	It("records a snapshot without a hash when hashAlgorithm is empty", func() {
		chain := runningChain()
		mock := &mockCheckpointer{}
		pod := runningPod("pod-1", map[string]string{"app": "test"}, "c1")
		r := makeRunningReconciler(chain, mock, pod)

		_, err := r.Reconcile(context.Background(), request)
		Expect(err).ToNot(HaveOccurred())

		updated := &criuorgv1.ForensicSnapshotChain{}
		Expect(r.Get(context.Background(), request.NamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Phase).ToNot(Equal(criuorgv1.PhaseFailed))
		Expect(updated.Status.SnapshotChainRecords).To(HaveLen(1))
		Expect(updated.Status.SnapshotChainRecords[0].SHA256Hash).To(BeEmpty())
		Expect(meta.FindStatusCondition(updated.Status.Conditions, "IntegrityVerified")).To(BeNil())
	})

})

func runningPod(name string, labels map[string]string, containers ...string) *corev1.Pod {
	cs := make([]corev1.Container, 0, len(containers))
	for _, c := range containers {
		cs = append(cs, corev1.Container{Name: c, Image: "nginx"})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels},
		Spec:       corev1.PodSpec{NodeName: "node-1", Containers: cs},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func markChainDue(r *ForensicSnapshotChainReconciler, key types.NamespacedName) {
	chain := &criuorgv1.ForensicSnapshotChain{}
	Expect(r.Get(context.Background(), key, chain)).To(Succeed())
	past := metav1.NewTime(time.Now().Add(-DefaultInterval))
	chain.Status.LastSnapshotTime = &past
	Expect(r.Status().Update(context.Background(), chain)).To(Succeed())
}
