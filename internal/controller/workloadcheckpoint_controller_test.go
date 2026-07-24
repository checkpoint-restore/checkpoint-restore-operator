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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

// fakeCheckpointer records calls and returns a synthetic archive path, or an
// error for containers whose name is in failOn.
type fakeCheckpointer struct {
	failOn map[string]bool
}

func (f *fakeCheckpointer) createCheckpoint(_ context.Context, ns, pod, container, node string) (string, error) {
	if f.failOn[container] {
		return "", fmt.Errorf("simulated failure for %s", container)
	}
	return fmt.Sprintf("/var/lib/kubelet/checkpoints/checkpoint-%s_%s-%s.tar", pod, ns, container), nil
}

var _ = Describe("WorkloadCheckpoint controller", func() {
	BeforeEach(func() {
		if cfg == nil {
			Skip("envtest not available")
		}
	})

	reconcileToTerminal := func(r *WorkloadCheckpointReconciler, key types.NamespacedName) *criuorgv1.WorkloadCheckpoint {
		ctx := context.Background()
		for i := 0; i < 5; i++ {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			wc := &criuorgv1.WorkloadCheckpoint{}
			Expect(k8sClient.Get(ctx, key, wc)).To(Succeed())
			switch wc.Status.Phase {
			case criuorgv1.WorkloadPhaseCompleted,
				criuorgv1.WorkloadPhasePartiallyFailed,
				criuorgv1.WorkloadPhaseFailed:
				return wc
			}
		}
		Fail("did not reach terminal phase")
		return nil
	}

	// createRunningJobPod creates a Job and one Running pod owned by it, with
	// NodeName set at create time (envtest treats a later NodeName update as a
	// binding), and returns the pod.
	createRunningJobPod := func(jobName, podName string) {
		ctx := context.Background()
		ns := "default"
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns},
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers:    []corev1.Container{{Name: "app", Image: "busybox"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, job)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(job), job)).To(Succeed())

		controller := true
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: podName, Namespace: ns,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "batch/v1", Kind: "Job", Name: jobName, UID: job.UID, Controller: &controller,
				}},
			},
			Spec: corev1.PodSpec{
				NodeName:   "node-1",
				Containers: []corev1.Container{{Name: "app", Image: "busybox"}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.Phase = corev1.PodRunning
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	It("checkpoints all pods of a Job and records archives", func() {
		ctx := context.Background()
		createRunningJobPod("job-a", "job-a-xyz")

		wc := &criuorgv1.WorkloadCheckpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "wc-a", Namespace: "default"},
			Spec: criuorgv1.WorkloadCheckpointSpec{
				TargetRef: criuorgv1.WorkloadReference{Kind: "Job", Name: "job-a"},
			},
		}
		Expect(k8sClient.Create(ctx, wc)).To(Succeed())

		r := &WorkloadCheckpointReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Checkpointer: &fakeCheckpointer{},
		}
		got := reconcileToTerminal(r, client.ObjectKeyFromObject(wc))
		Expect(got.Status.Phase).To(Equal(criuorgv1.WorkloadPhaseCompleted))
		Expect(got.Status.CheckpointedPodCount).To(Equal(int32(1)))
		Expect(got.Status.Checkpoints).To(HaveLen(1))
		Expect(got.Status.Checkpoints[0].NodeName).To(Equal("node-1"))
		Expect(got.Status.Checkpoints[0].Containers[0].Path).To(ContainSubstring("checkpoint-job-a-xyz"))
	})

	It("marks PartiallyFailed under BestEffort when a container fails", func() {
		ctx := context.Background()
		createRunningJobPod("job-b", "job-b-xyz")

		wc := &criuorgv1.WorkloadCheckpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "wc-b", Namespace: "default"},
			Spec: criuorgv1.WorkloadCheckpointSpec{
				TargetRef:     criuorgv1.WorkloadReference{Kind: "Job", Name: "job-b"},
				FailurePolicy: criuorgv1.FailurePolicyBestEffort,
			},
		}
		Expect(k8sClient.Create(ctx, wc)).To(Succeed())

		r := &WorkloadCheckpointReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Checkpointer: &fakeCheckpointer{failOn: map[string]bool{"app": true}},
		}
		got := reconcileToTerminal(r, client.ObjectKeyFromObject(wc))
		Expect(got.Status.Phase).To(Equal(criuorgv1.WorkloadPhasePartiallyFailed))
		Expect(got.Status.FailedPodCount).To(Equal(int32(1)))
	})

	It("marks Failed under AllOrNothing when a container fails", func() {
		ctx := context.Background()
		createRunningJobPod("job-c", "job-c-xyz")

		wc := &criuorgv1.WorkloadCheckpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "wc-c", Namespace: "default"},
			Spec: criuorgv1.WorkloadCheckpointSpec{
				TargetRef:     criuorgv1.WorkloadReference{Kind: "Job", Name: "job-c"},
				FailurePolicy: criuorgv1.FailurePolicyAllOrNothing,
			},
		}
		Expect(k8sClient.Create(ctx, wc)).To(Succeed())

		r := &WorkloadCheckpointReconciler{
			Client:       k8sClient,
			Scheme:       k8sClient.Scheme(),
			Checkpointer: &fakeCheckpointer{failOn: map[string]bool{"app": true}},
		}
		got := reconcileToTerminal(r, client.ObjectKeyFromObject(wc))
		Expect(got.Status.Phase).To(Equal(criuorgv1.WorkloadPhaseFailed))
	})
})
