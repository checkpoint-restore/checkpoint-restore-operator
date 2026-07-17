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
	"os"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/resource"
)

var _ = Describe("applyPolicies", func() {
	var tmpDir string
	var savedDir string

	BeforeEach(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "gc-policy-test-*")
		Expect(err).NotTo(HaveOccurred())

		savedDir = checkpointDirectory
		checkpointDirectory = tmpDir

		resetAllPoliciesToDefault(logr.Discard())
	})

	AfterEach(func() {
		checkpointDirectory = savedDir
		Expect(os.RemoveAll(tmpDir)).To(Succeed())
	})

	It("does not panic when retainOrphan is nil", func() {
		// retainOrphan is nil after resetAllPoliciesToDefault.
		// Before the fix (*retainOrphan was used directly) this panicked.
		// After the fix (ifNil(retainOrphan)) this must return cleanly.
		Expect(retainOrphan).To(BeNil())

		details := &checkpointDetails{
			namespace: "default",
			pod:       "test-pod",
			container: "test-container",
		}

		Expect(func() {
			applyPolicies(logr.Discard(), details)
		}).NotTo(Panic())
	})

	It("keeps cleanup policy snapshots independent from later policy changes", func() {
		retainOrphanValue := false
		containerLimit := 1
		podLimit := 2
		namespaceLimit := 3
		containerPolicyLimit := 4
		podPolicyLimit := 5
		namespacePolicyLimit := 6
		checkpointSize := resource.MustParse("1Gi")
		totalContainerSize := resource.MustParse("2Gi")
		containerPolicySize := resource.MustParse("3Gi")
		podPolicySize := resource.MustParse("4Gi")
		namespacePolicySize := resource.MustParse("5Gi")

		spec := &criuorgv1.CheckpointRestoreOperatorSpec{
			CheckpointDirectory: tmpDir,
			GlobalPolicies: criuorgv1.GlobalPolicySpec{
				RetainOrphan:                &retainOrphanValue,
				MaxCheckpointsPerContainer:  &containerLimit,
				MaxCheckpointsPerPod:        &podLimit,
				MaxCheckpointsPerNamespaces: &namespaceLimit,
				MaxCheckpointSize:           &checkpointSize,
				MaxTotalSizePerContainer:    &totalContainerSize,
				MaxTotalSizePerPod:          &totalContainerSize,
				MaxTotalSizePerNamespace:    &totalContainerSize,
			},
			ContainerPolicies: []criuorgv1.ContainerPolicySpec{
				{
					Namespace:         "default",
					Pod:               "test-pod",
					Container:         "test-container",
					RetainOrphan:      &retainOrphanValue,
					MaxCheckpoints:    &containerPolicyLimit,
					MaxCheckpointSize: &containerPolicySize,
					MaxTotalSize:      &containerPolicySize,
				},
			},
			PodPolicies: []criuorgv1.PodPolicySpec{
				{
					Namespace:         "default",
					Pod:               "test-pod",
					RetainOrphan:      &retainOrphanValue,
					MaxCheckpoints:    &podPolicyLimit,
					MaxCheckpointSize: &podPolicySize,
					MaxTotalSize:      &podPolicySize,
				},
			},
			NamespacePolicies: []criuorgv1.NamespacePolicySpec{
				{
					Namespace:         "default",
					RetainOrphan:      &retainOrphanValue,
					MaxCheckpoints:    &namespacePolicyLimit,
					MaxCheckpointSize: &namespacePolicySize,
					MaxTotalSize:      &namespacePolicySize,
				},
			},
		}

		policies, restartGarbageCollector := (&CheckpointRestoreOperatorReconciler{}).applyPolicySpec(logr.Discard(), spec)
		Expect(restartGarbageCollector).To(BeFalse())

		retainOrphanValue = true
		containerLimit = 10
		podLimit = 20
		namespaceLimit = 30
		containerPolicyLimit = 40
		podPolicyLimit = 50
		namespacePolicyLimit = 60
		checkpointSize = resource.MustParse("10Gi")
		totalContainerSize = resource.MustParse("20Gi")
		containerPolicySize = resource.MustParse("30Gi")
		podPolicySize = resource.MustParse("40Gi")
		namespacePolicySize = resource.MustParse("50Gi")
		resetAllPoliciesToDefault(logr.Discard())

		Expect(policies.checkpointDirectory).To(Equal(tmpDir))
		Expect(policies.retainOrphan).To(BeFalse())
		Expect(policies.maxCheckpointsPerContainer).To(Equal(1))
		Expect(policies.maxCheckpointsPerPod).To(Equal(2))
		Expect(policies.maxCheckpointsPerNamespace).To(Equal(3))
		Expect(policies.maxCheckpointSize.Cmp(resource.MustParse("1Gi"))).To(Equal(0))
		Expect(policies.maxTotalSizePerContainer.Cmp(resource.MustParse("2Gi"))).To(Equal(0))

		Expect(policies.containerPolicies).To(HaveLen(1))
		Expect(*policies.containerPolicies[0].RetainOrphan).To(BeFalse())
		Expect(*policies.containerPolicies[0].MaxCheckpoints).To(Equal(4))
		Expect(policies.containerPolicies[0].MaxCheckpointSize.Cmp(resource.MustParse("3Gi"))).To(Equal(0))

		Expect(policies.podPolicies).To(HaveLen(1))
		Expect(*policies.podPolicies[0].RetainOrphan).To(BeFalse())
		Expect(*policies.podPolicies[0].MaxCheckpoints).To(Equal(5))
		Expect(policies.podPolicies[0].MaxCheckpointSize.Cmp(resource.MustParse("4Gi"))).To(Equal(0))

		Expect(policies.namespacePolicies).To(HaveLen(1))
		Expect(*policies.namespacePolicies[0].RetainOrphan).To(BeFalse())
		Expect(*policies.namespacePolicies[0].MaxCheckpoints).To(Equal(6))
		Expect(policies.namespacePolicies[0].MaxCheckpointSize.Cmp(resource.MustParse("5Gi"))).To(Equal(0))
	})
})

var _ = Describe("policySnapshot.resolveUploadToExternalStorage", func() {
	details := &checkpointDetails{namespace: "ns", pod: "pod", container: "ctr"}

	It("defaults to false when nothing is configured", func() {
		snap := policySnapshot{}
		Expect(snap.resolveUploadToExternalStorage(details)).To(BeFalse())
	})

	It("uses the global policy when no specific policy matches", func() {
		snap := policySnapshot{uploadToExternalStorage: true}
		Expect(snap.resolveUploadToExternalStorage(details)).To(BeTrue())
	})

	It("prefers a matching namespace policy over the global default", func() {
		snap := policySnapshot{
			uploadToExternalStorage: true,
			namespacePolicies: []criuorgv1.NamespacePolicySpec{
				{Namespace: "ns", UploadToExternalStorage: ptr(false)},
			},
		}
		Expect(snap.resolveUploadToExternalStorage(details)).To(BeFalse())
	})

	It("prefers a matching pod policy over a matching namespace policy", func() {
		snap := policySnapshot{
			namespacePolicies: []criuorgv1.NamespacePolicySpec{
				{Namespace: "ns", UploadToExternalStorage: ptr(false)},
			},
			podPolicies: []criuorgv1.PodPolicySpec{
				{Namespace: "ns", Pod: "pod", UploadToExternalStorage: ptr(true)},
			},
		}
		Expect(snap.resolveUploadToExternalStorage(details)).To(BeTrue())
	})

	It("prefers a matching container policy over pod and namespace policies", func() {
		snap := policySnapshot{
			namespacePolicies: []criuorgv1.NamespacePolicySpec{
				{Namespace: "ns", UploadToExternalStorage: ptr(false)},
			},
			podPolicies: []criuorgv1.PodPolicySpec{
				{Namespace: "ns", Pod: "pod", UploadToExternalStorage: ptr(false)},
			},
			containerPolicies: []criuorgv1.ContainerPolicySpec{
				{Namespace: "ns", Pod: "pod", Container: "ctr", UploadToExternalStorage: ptr(true)},
			},
		}
		Expect(snap.resolveUploadToExternalStorage(details)).To(BeTrue())
	})

	It("falls through to the global default when a matching policy leaves the field unset", func() {
		snap := policySnapshot{
			uploadToExternalStorage: true,
			podPolicies: []criuorgv1.PodPolicySpec{
				{Namespace: "ns", Pod: "pod"},
			},
		}
		Expect(snap.resolveUploadToExternalStorage(details)).To(BeTrue())
	})
})
