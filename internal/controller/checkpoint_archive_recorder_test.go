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
	"errors"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

// erroringClient wraps a client.Client and forces Create to fail, so tests
// can verify that recordCheckpointArchiveIfEnabled propagates the error
// instead of swallowing it.
type erroringClient struct {
	client.Client
	createErr error
}

func (e *erroringClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return e.createErr
}

var _ = Describe("recordCheckpointArchiveIfEnabled", func() {
	var c client.Client

	BeforeEach(func() {
		Expect(criuorgv1.AddToScheme(scheme.Scheme)).To(Succeed())
		c = fake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithStatusSubresource(&criuorgv1.CheckpointArchive{}).Build()
		resetAllPoliciesToDefault(logr.Discard())
	})

	AfterEach(func() {
		resetAllPoliciesToDefault(logr.Discard())
	})

	It("creates no CheckpointArchive when no policy opts in", func() {
		err := recordCheckpointArchiveIfEnabled(context.Background(), c,
			"ns", "pod", "ctr", "node-a", "/var/lib/kubelet/checkpoints/x.tar")
		Expect(err).NotTo(HaveOccurred())

		var list criuorgv1.CheckpointArchiveList
		Expect(c.List(context.Background(), &list)).To(Succeed())
		Expect(list.Items).To(BeEmpty())
	})

	It("creates a CheckpointArchive when the global policy opts in", func() {
		(&CheckpointRestoreOperatorReconciler{}).handleGlobalPolicies(logr.Discard(), &criuorgv1.GlobalPolicySpec{
			UploadToExternalStorage: ptr(true),
		})

		err := recordCheckpointArchiveIfEnabled(context.Background(), c,
			"ns", "pod", "ctr", "node-a", "/var/lib/kubelet/checkpoints/x.tar")
		Expect(err).NotTo(HaveOccurred())

		var list criuorgv1.CheckpointArchiveList
		Expect(c.List(context.Background(), &list)).To(Succeed())
		Expect(list.Items).To(HaveLen(1))
		archive := list.Items[0]
		Expect(archive.Namespace).To(Equal("ns"))
		Expect(archive.Spec).To(Equal(criuorgv1.CheckpointArchiveSpec{
			Node:      "node-a",
			LocalPath: "/var/lib/kubelet/checkpoints/x.tar",
			Namespace: "ns",
			Pod:       "pod",
			Container: "ctr",
		}))
	})

	It("lists the origin node in status.availableNodes", func() {
		if k8sClient == nil {
			Skip("envtest environment not available")
		}
		ctx := context.Background()

		(&CheckpointRestoreOperatorReconciler{}).handleGlobalPolicies(logr.Discard(), &criuorgv1.GlobalPolicySpec{
			UploadToExternalStorage: ptr(true),
		})

		Expect(recordCheckpointArchiveIfEnabled(ctx, k8sClient,
			"default", "web", "app", "node-a",
			"/var/lib/kubelet/checkpoints/checkpoint-web_default-app-x.tar")).To(Succeed())

		var list criuorgv1.CheckpointArchiveList
		Expect(k8sClient.List(ctx, &list, client.InNamespace("default"))).To(Succeed())
		Expect(list.Items).To(HaveLen(1))
		Expect(list.Items[0].Status.AvailableNodes).To(ConsistOf("node-a"))
	})

	It("propagates a Create error instead of swallowing it", func() {
		(&CheckpointRestoreOperatorReconciler{}).handleGlobalPolicies(logr.Discard(), &criuorgv1.GlobalPolicySpec{
			UploadToExternalStorage: ptr(true),
		})
		badClient := &erroringClient{Client: c, createErr: errors.New("create failed")}

		err := recordCheckpointArchiveIfEnabled(context.Background(), badClient,
			"ns", "pod", "ctr", "node-a", "/var/lib/kubelet/checkpoints/x.tar")
		Expect(err).To(HaveOccurred())
	})
})
