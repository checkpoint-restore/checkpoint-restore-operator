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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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

		chain:= &criuorgv1.ForensicSnapshotChain{
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

		_,err := reconciler.Reconcile(context.Background(), request)

		Expect(err).ToNot(HaveOccurred())

		updatedChain := &criuorgv1.ForensicSnapshotChain{}
		Expect(reconciler.Get(context.Background(), request.NamespacedName, updatedChain)).To(Succeed())
		Expect(updatedChain.Status.Phase).To(Equal(criuorgv1.PhasePending))
	})

	It("should transition from Pending to Running phase", func() {
		
		chain:= &criuorgv1.ForensicSnapshotChain{
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

		_,err := reconciler.Reconcile(context.Background(), request)

		Expect(err).ToNot(HaveOccurred())

		updatedChain := &criuorgv1.ForensicSnapshotChain{}
		Expect(reconciler.Get(context.Background(), request.NamespacedName, updatedChain)).To(Succeed())
		Expect(updatedChain.Status.Phase).To(Equal(criuorgv1.PhaseRunning))


	})


	It("should return immediately when already in Completed phase", func() {
		
		chain:= &criuorgv1.ForensicSnapshotChain{
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


	It("should return immediantely when already in Failed phase", func() {
		
		chain:= &criuorgv1.ForensicSnapshotChain{
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

		result,err := reconciler.Reconcile(context.Background(), request)

		Expect(err).ToNot(HaveOccurred())

		Expect(result).To(Equal(ctrl.Result{}))
	})


})