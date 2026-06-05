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

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

// ForensicSnapshotChainReconciler reconciles a ForensicSnapshotChain object
type ForensicSnapshotChainReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	RestConfig *rest.Config
}

// +kubebuilder:rbac:groups=criu.org,resources=forensicsnapshotchains,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=criu.org,resources=forensicsnapshotchains/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=criu.org,resources=forensicsnapshotchains/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ForensicSnapshotChain object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/reconcile
func (r *ForensicSnapshotChainReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	log := logf.FromContext(ctx)

	chain := &criuorgv1.ForensicSnapshotChain{}

	if err := r.Get(ctx, req.NamespacedName, chain); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reconciling ForensicSnapshotChain", "name", chain.Name)

	//If the phase is empty, this is a new ForensicSnapshotChain
	// and we need to initialize it, that's why pending
	if chain.Status.Phase == criuorgv1.SnapshotChainPhase("") {
		chain.Status.Phase = criuorgv1.PhasePending

		if err := r.Status().Update(ctx, chain); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil
	}

	// If the phase is pending, we need to start the snapshot chain
	if chain.Status.Phase == criuorgv1.PhasePending {

		pods, err := getMatchingPods(
			ctx,
			r.Client,
			chain.Spec.Namespace,
			&chain.Spec.Selector,
		)

		if err != nil {
			return ctrl.Result{}, err
		}

		log.Info(
			"Found matching pods for ForensicSnapshotChain",
			"count", len(pods),
		)

		//logic for container selection
		for _, pod := range pods {
			containers := filterContainers(
				pod,
				chain.Spec.ContainerNames,
			)

			log.Info(
				"Selected containers for ForensicSnapshotChain",
				"pod", pod.Name,
				"containers", len(containers),
			)

		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ForensicSnapshotChainReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&criuorgv1.ForensicSnapshotChain{}).
		Named("forensicsnapshotchain").
		Complete(r)
}
