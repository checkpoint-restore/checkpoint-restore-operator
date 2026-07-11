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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

var _ = Describe("mountedPVCsForContainers", func() {
	// A pod whose "app" container mounts a PVC-backed volume and an emptyDir,
	// and whose "sidecar" container mounts a configMap and the same PVC.
	newPod := func() *corev1.Pod {
		return &corev1.Pod{
			Spec: corev1.PodSpec{
				Volumes: []corev1.Volume{
					{
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: "data-pvc",
							},
						},
					},
					{
						Name:         "scratch",
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
					},
					{
						Name: "cfg",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: "cm"},
							},
						},
					},
				},
				Containers: []corev1.Container{
					{
						Name: "app",
						VolumeMounts: []corev1.VolumeMount{
							{Name: "data", MountPath: "/data"},
							{Name: "scratch", MountPath: "/scratch"},
						},
					},
					{
						Name: "sidecar",
						VolumeMounts: []corev1.VolumeMount{
							{Name: "cfg", MountPath: "/etc/cfg"},
							{Name: "data", MountPath: "/shared"},
						},
					},
				},
			},
		}
	}

	It("returns only PVC-backed volumes, skipping emptyDir and configMap", func() {
		mounts := mountedPVCsForContainers(newPod(), nil)
		Expect(mounts).To(HaveLen(1))
		Expect(mounts[0].ClaimName).To(Equal("data-pvc"))
		Expect(mounts[0].VolumeName).To(Equal("data"))
	})

	It("deduplicates a PVC mounted by more than one container", func() {
		mounts := mountedPVCsForContainers(newPod(), nil)
		Expect(mounts).To(HaveLen(1))
	})

	It("restricts to the named containers and reports that container's mount path", func() {
		mounts := mountedPVCsForContainers(newPod(), []string{"sidecar"})
		Expect(mounts).To(HaveLen(1))
		Expect(mounts[0].ClaimName).To(Equal("data-pvc"))
		Expect(mounts[0].MountPath).To(Equal("/shared"))
	})

	It("returns nothing when the selected container mounts no PVC", func() {
		pod := newPod()
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
			{Name: "scratch", MountPath: "/scratch"},
		}
		Expect(mountedPVCsForContainers(pod, []string{"app"})).To(BeEmpty())
	})
})

var _ = Describe("snapshotVolumes", func() {
	var (
		ctx        context.Context
		fakeClient client.Client
		pod        *corev1.Pod
		cfg        *criuorgv1.VolumeSnapshotConfig
		origPoll   time.Duration
	)

	// snapshotScheme registers corev1 plus the unstructured VolumeSnapshot GVK
	// so the fake client can create and get VolumeSnapshot objects.
	snapshotScheme := func() *runtime.Scheme {
		s := runtime.NewScheme()
		Expect(corev1.AddToScheme(s)).To(Succeed())
		s.AddKnownTypeWithName(volumeSnapshotGVK, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(
			volumeSnapshotGVK.GroupVersion().WithKind("VolumeSnapshotList"),
			&unstructured.UnstructuredList{},
		)
		return s
	}

	BeforeEach(func() {
		ctx = context.Background()
		// Keep the ready-wait short so best-effort timeouts don't slow the suite.
		origPoll = snapshotReadyPollInterval
		snapshotReadyPollInterval = 5 * time.Millisecond
		readyTimeout := metav1.Duration{Duration: 20 * time.Millisecond}
		cfg = &criuorgv1.VolumeSnapshotConfig{
			Enabled:      true,
			ReadyTimeout: &readyTimeout,
		}
		pod = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "redis-0", Namespace: "default"},
			Spec: corev1.PodSpec{
				Volumes: []corev1.Volume{
					{
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: "data-pvc",
							},
						},
					},
				},
				Containers: []corev1.Container{
					{Name: "redis", VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}},
				},
			},
		}
		fakeClient = fake.NewClientBuilder().WithScheme(snapshotScheme()).Build()
	})

	AfterEach(func() { snapshotReadyPollInterval = origPoll })

	ident := checkpointIdentity{Node: "worker-1", Pod: "redis-0", Container: "redis"}

	It("creates a VolumeSnapshot for the mounted PVC with the source and identity labels", func() {
		refs, err := snapshotVolumes(ctx, fakeClient, pod, []string{"redis"}, cfg, ident)
		Expect(err).NotTo(HaveOccurred())
		Expect(refs).To(HaveLen(1))
		Expect(refs[0].PVC).To(Equal("data-pvc"))
		Expect(refs[0].MountPath).To(Equal("/data"))
		Expect(refs[0].SnapshotName).NotTo(BeEmpty())

		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(volumeSnapshotGVK.GroupVersion().WithKind("VolumeSnapshotList"))
		Expect(fakeClient.List(ctx, list, client.InNamespace("default"))).To(Succeed())
		Expect(list.Items).To(HaveLen(1))

		snap := list.Items[0]
		source, _, _ := unstructured.NestedString(snap.Object, "spec", "source", "persistentVolumeClaimName")
		Expect(source).To(Equal("data-pvc"))
		Expect(snap.GetLabels()).To(HaveKeyWithValue(criuorgv1.LabelNode, "worker-1"))
		Expect(snap.GetLabels()).To(HaveKeyWithValue(criuorgv1.LabelPod, "redis-0"))
		Expect(snap.GetLabels()).To(HaveKeyWithValue(criuorgv1.LabelContainer, "redis"))
	})

	It("sets the VolumeSnapshotClass when configured", func() {
		cfg.VolumeSnapshotClassName = "csi-snapclass"
		_, err := snapshotVolumes(ctx, fakeClient, pod, []string{"redis"}, cfg, ident)
		Expect(err).NotTo(HaveOccurred())

		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(volumeSnapshotGVK.GroupVersion().WithKind("VolumeSnapshotList"))
		Expect(fakeClient.List(ctx, list, client.InNamespace("default"))).To(Succeed())
		Expect(list.Items).To(HaveLen(1))
		class, _, _ := unstructured.NestedString(list.Items[0].Object, "spec", "volumeSnapshotClassName")
		Expect(class).To(Equal("csi-snapclass"))
	})

	It("records readyToUse=false without erroring when the snapshot never becomes ready (best-effort)", func() {
		refs, err := snapshotVolumes(ctx, fakeClient, pod, []string{"redis"}, cfg, ident)
		Expect(err).NotTo(HaveOccurred())
		Expect(refs).To(HaveLen(1))
		Expect(refs[0].ReadyToUse).To(BeFalse())
	})

	It("returns no refs when the container mounts no PVC-backed volume", func() {
		pod.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
		refs, err := snapshotVolumes(ctx, fakeClient, pod, []string{"redis"}, cfg, ident)
		Expect(err).NotTo(HaveOccurred())
		Expect(refs).To(BeEmpty())
	})
})
