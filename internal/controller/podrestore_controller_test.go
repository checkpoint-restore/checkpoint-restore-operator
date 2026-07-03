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
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

var _ = Describe("PodRestoreReconciler", func() {
	BeforeEach(func() {
		Expect(criuorgv1.AddToScheme(scheme.Scheme)).To(Succeed())
	})

	const (
		name = "redis-restore"
		ns   = "default"
		tar  = "/var/lib/kubelet/checkpoints/checkpoint-redis_default-redis-x.tar"
	)

	newPodRestore := func() *criuorgv1.PodRestore {
		return &criuorgv1.PodRestore{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: criuorgv1.PodRestoreSpec{
				TargetNode:  "worker-1",
				Checkpoints: []criuorgv1.ContainerCheckpoint{{Container: "redis", Path: tar}},
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							// Explicit image so the reconciler need not read the archive.
							{Name: "redis", Image: "redis:7.0"},
						},
					},
				},
			},
		}
	}

	// The target node must exist for the restore to proceed past Pending.
	node := func() *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}
	}

	makeReconciler := func(objs ...client.Object) *PodRestoreReconciler {
		c := fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(objs...).
			WithStatusSubresource(&criuorgv1.PodRestore{}).
			Build()
		return &PodRestoreReconciler{Client: c, Scheme: scheme.Scheme}
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}

	// reconcileN runs Reconcile n times so the resource walks its phase machine
	// (each status write re-enqueues in production; here we drive it explicitly).
	reconcileN := func(r *PodRestoreReconciler, n int) {
		for i := 0; i < n; i++ {
			_, err := r.Reconcile(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())
		}
	}

	get := func(r *PodRestoreReconciler) *criuorgv1.PodRestore {
		out := &criuorgv1.PodRestore{}
		Expect(r.Get(context.Background(), req.NamespacedName, out)).To(Succeed())
		return out
	}

	// ready returns the Ready condition, failing the test if it is absent.
	ready := func(r *PodRestoreReconciler) *metav1.Condition {
		c := meta.FindStatusCondition(get(r).Status.Conditions, criuorgv1.ConditionReady)
		Expect(c).NotTo(BeNil(), "Ready condition should be set")
		return c
	}

	It("adds a finalizer, then creates the restore Pod and reports Restoring", func() {
		r := makeReconciler(newPodRestore(), node())
		reconcileN(r, 1) // adds finalizer and returns
		Expect(get(r).Finalizers).To(ContainElement(podRestoreFinalizer))

		reconcileN(r, 1) // creates the Pod
		Expect(get(r).Status.PodName).To(Equal(name))
		c := ready(r)
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("Restoring"))
	})

	It("renders a node-pinned, annotated Pod", func() {
		r := makeReconciler(newPodRestore(), node())
		reconcileN(r, 2) // finalizer -> create Pod

		Expect(get(r).Status.PodName).To(Equal(name))

		pod := &corev1.Pod{}
		Expect(r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, pod)).To(Succeed())
		Expect(pod.Spec.NodeName).To(Equal("worker-1"))
		Expect(pod.Spec.Containers[0].Image).To(Equal("redis:7.0"))
		Expect(pod.Annotations).To(HaveKeyWithValue(
			criuorgv1.RestoreCheckpointPathAnnotationPrefix+"redis", tar))
		Expect(pod.Labels).To(HaveKeyWithValue(podRestoreLabel, name))
		Expect(pod.OwnerReferences).To(HaveLen(1))
		Expect(pod.OwnerReferences[0].Name).To(Equal(name))
	})

	It("copies only labels and annotations from template metadata", func() {
		pr := newPodRestore()
		pr.Spec.Template.Labels = map[string]string{"app": "redis"}
		pr.Spec.Template.Annotations = map[string]string{"example.com/source": "restore"}
		pr.Spec.Template.GenerateName = "user-supplied-"
		pr.Spec.Template.Finalizers = []string{"example.com/hold"}
		pr.Spec.Template.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "v1",
			Kind:       "ConfigMap",
			Name:       "foreign",
			UID:        types.UID("foreign-uid"),
		}}

		r := makeReconciler(pr, node())
		reconcileN(r, 2)

		pod := &corev1.Pod{}
		Expect(r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, pod)).To(Succeed())
		Expect(pod.Labels).To(HaveKeyWithValue("app", "redis"))
		Expect(pod.Annotations).To(HaveKeyWithValue("example.com/source", "restore"))
		Expect(pod.GenerateName).To(BeEmpty())
		Expect(pod.Finalizers).To(BeEmpty())
		Expect(pod.OwnerReferences).To(HaveLen(1))
		Expect(pod.OwnerReferences[0].Kind).To(Equal("PodRestore"))
		Expect(pod.OwnerReferences[0].Name).To(Equal(name))
	})

	It("goes Ready=True when the Pod is running", func() {
		r := makeReconciler(newPodRestore(), node())
		reconcileN(r, 2)

		pod := &corev1.Pod{}
		Expect(r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, pod)).To(Succeed())
		pod.Status.Phase = corev1.PodRunning
		Expect(r.Status().Update(context.Background(), pod)).To(Succeed())

		reconcileN(r, 1)
		c := ready(r)
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal("Restored"))
	})

	It("reports InvalidSpec when a checkpoint references an unknown container", func() {
		pr := newPodRestore()
		pr.Spec.Checkpoints[0].Container = "does-not-exist"
		r := makeReconciler(pr, node())
		reconcileN(r, 2) // finalizer -> validateSpec fails

		c := ready(r)
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("InvalidSpec"))
		Expect(c.Message).To(ContainSubstring("not defined in the Pod template"))

		pod := &corev1.Pod{}
		err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, pod)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("removes the finalizer on deletion", func() {
		r := makeReconciler(newPodRestore(), node())
		reconcileN(r, 2)

		pr := get(r)
		Expect(r.Delete(context.Background(), pr)).To(Succeed())
		reconcileN(r, 1)

		out := &criuorgv1.PodRestore{}
		err := r.Get(context.Background(), req.NamespacedName, out)
		// With the finalizer gone, the fake client purges the object.
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("only restores listed containers; sidecars start normally", func() {
		pr := newPodRestore()
		pr.Spec.Template.Spec.Containers = append(pr.Spec.Template.Spec.Containers,
			corev1.Container{Name: "sidecar", Image: "busybox:latest"})
		r := makeReconciler(pr, node())
		reconcileN(r, 2)

		pod := &corev1.Pod{}
		Expect(r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, pod)).To(Succeed())
		// Restored container is annotated; sidecar is not, and keeps its image.
		Expect(pod.Annotations).To(HaveKey(criuorgv1.RestoreCheckpointPathAnnotationPrefix + "redis"))
		Expect(pod.Annotations).NotTo(HaveKey(criuorgv1.RestoreCheckpointPathAnnotationPrefix + "sidecar"))
		Expect(pod.Spec.Containers[1].Image).To(Equal("busybox:latest"))
	})

	It("reports InvalidSpec on an unsafe checkpoint path", func() {
		pr := newPodRestore()
		pr.Spec.Checkpoints[0].Path = "/var/lib/kubelet/checkpoints/../../etc/shadow.tar"
		r := makeReconciler(pr, node())
		reconcileN(r, 2)

		c := ready(r)
		Expect(c.Reason).To(Equal("InvalidSpec"))
		Expect(c.Message).To(ContainSubstring("must be clean"))
	})

	It("reports InvalidSpec when the template sets a reserved restore annotation", func() {
		pr := newPodRestore()
		pr.Spec.Template.Annotations = map[string]string{
			criuorgv1.RestoreCheckpointPathAnnotationPrefix + "sidecar": "/var/lib/kubelet/checkpoints/sidecar.tar",
		}
		r := makeReconciler(pr, node())
		reconcileN(r, 2)

		c := ready(r)
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("InvalidSpec"))
		Expect(c.Message).To(ContainSubstring("reserved for the PodRestore controller"))

		pod := &corev1.Pod{}
		err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, pod)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("reports NodeNotFound when the target node does not exist", func() {
		r := makeReconciler(newPodRestore()) // no node()
		reconcileN(r, 2)                     // finalizer -> node check fails

		c := ready(r)
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("NodeNotFound"))

		pod := &corev1.Pod{}
		err := r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, pod)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("reports CheckpointsPinned=False when the archive is unreachable", func() {
		r := makeReconciler(newPodRestore(), node())
		reconcileN(r, 2)

		cond := meta.FindStatusCondition(get(r).Status.Conditions, criuorgv1.ConditionCheckpointsPinned)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("PinNotEnforced"))
	})

	It("reports PodConflict if a Pod of the same name is not owned by the PodRestore", func() {
		foreign := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "x", Image: "busybox"}}},
		}
		r := makeReconciler(newPodRestore(), node(), foreign)
		reconcileN(r, 2)

		c := ready(r)
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("PodConflict"))
		Expect(c.Message).To(ContainSubstring("not owned by this PodRestore"))
	})

	It("reports PodMissing if the restored Pod disappears after Running", func() {
		r := makeReconciler(newPodRestore(), node())
		reconcileN(r, 2)

		pod := &corev1.Pod{}
		Expect(r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, pod)).To(Succeed())
		pod.Status.Phase = corev1.PodRunning
		Expect(r.Status().Update(context.Background(), pod)).To(Succeed())
		reconcileN(r, 1)
		Expect(ready(r).Status).To(Equal(metav1.ConditionTrue))

		// The Pod is deleted out from under us; the next reconcile must notice
		// instead of latching Ready=True.
		Expect(r.Delete(context.Background(), pod)).To(Succeed())
		reconcileN(r, 1)
		c := ready(r)
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("PodMissing"))
	})

	It("does not remove a pre-existing keep marker on deletion", func() {
		dir := GinkgoT().TempDir()
		archive := filepath.Join(dir, "checkpoint-redis_default-redis-x.tar")
		Expect(os.WriteFile(archive, []byte("tar placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(archive+".keep", []byte("manual pin"), 0o600)).To(Succeed())

		pr := newPodRestore()
		pr.Spec.Checkpoints[0].Path = archive
		r := makeReconciler(pr, node())
		r.CheckpointDir = dir
		reconcileN(r, 2)

		Expect(r.Delete(context.Background(), get(r))).To(Succeed())
		reconcileN(r, 1)

		Expect(archive + ".keep").To(BeAnExistingFile())
		data, err := os.ReadFile(archive + ".keep")
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(Equal("manual pin"))
	})

	It("keeps an operator-owned marker until all restore owners are removed", func() {
		dir := GinkgoT().TempDir()
		archive := filepath.Join(dir, "checkpoint.tar")
		Expect(os.WriteFile(archive, []byte("tar placeholder"), 0o600)).To(Succeed())

		Expect(pinCheckpoint(logr.Discard(), archive, "default/restore-a")).To(BeTrue())
		Expect(pinCheckpoint(logr.Discard(), archive, "default/restore-b")).To(BeTrue())

		unpinCheckpoint(logr.Discard(), archive, "default/restore-a")
		Expect(archive + ".keep").To(BeAnExistingFile())
		data, err := os.ReadFile(archive + ".keep")
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(ContainSubstring("default/restore-b"))
		Expect(string(data)).NotTo(ContainSubstring("default/restore-a"))

		unpinCheckpoint(logr.Discard(), archive, "default/restore-b")
		Expect(archive + ".keep").NotTo(BeAnExistingFile())
	})

	DescribeTable("checkpoint path shape validation",
		func(path string, ok bool) {
			err := criuorgv1.ValidateCheckpointPath(path)
			if ok {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(HaveOccurred())
			}
		},
		Entry("absolute .tar", "/var/lib/kubelet/checkpoints/cp.tar", true),
		Entry("empty", "", false),
		Entry("relative", "checkpoints/cp.tar", false),
		Entry("traversal", "/var/lib/kubelet/checkpoints/../x.tar", false),
		Entry("double slash", "/var//lib/cp.tar", false),
		Entry("not a tar", "/var/lib/kubelet/checkpoints/cp.img", false),
		Entry("trailing slash", "/var/lib/cp.tar/", false),
	)

	It("reports InvalidSpec when the archive is outside the checkpoint directory", func() {
		pr := newPodRestore()
		pr.Spec.Checkpoints[0].Path = "/tmp/checkpoint-redis_default-redis-x.tar"
		r := makeReconciler(pr, node())
		reconcileN(r, 2)

		c := ready(r)
		Expect(c.Reason).To(Equal("InvalidSpec"))
		Expect(c.Message).To(ContainSubstring("not inside the checkpoint directory"))
	})

	It("reports InvalidSpec when the archive belongs to another namespace", func() {
		pr := newPodRestore()
		pr.Spec.Checkpoints[0].Path = "/var/lib/kubelet/checkpoints/checkpoint-redis_other-redis-x.tar"
		r := makeReconciler(pr, node())
		reconcileN(r, 2)

		c := ready(r)
		Expect(c.Reason).To(Equal("InvalidSpec"))
		Expect(c.Message).To(ContainSubstring("does not belong to namespace"))
	})

	It("allows a foreign-namespace archive when AllowCrossNamespace is set", func() {
		pr := newPodRestore()
		pr.Spec.Checkpoints[0].Path = "/var/lib/kubelet/checkpoints/checkpoint-redis_other-redis-x.tar"
		r := makeReconciler(pr, node())
		r.AllowCrossNamespace = true
		reconcileN(r, 2)

		c := ready(r)
		Expect(c.Reason).To(Equal("Restoring"))
	})
})
