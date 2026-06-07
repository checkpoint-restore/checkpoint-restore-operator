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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	//If checkpoint creation is completed, we can exit the reconciliation loop
	if chain.Status.Phase == criuorgv1.PhaseCompleted {
		return ctrl.Result{}, nil
	}

	if chain.Status.Phase == criuorgv1.PhaseFailed {
		return ctrl.Result{}, nil
	}

	//If the phase is empty, this is a new ForensicSnapshotChain
	// and we need to initialize it, that's why pending
	if chain.Status.Phase == criuorgv1.SnapshotChainPhase("") {
		chain.Status.Phase = criuorgv1.PhasePending

		//to record start time of the snapshot chain
		now := metav1.Now()
		chain.Status.StartTime = &now

		if err := r.Status().Update(ctx, chain); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil
	}

	//If snapshot chain is pending then make it running
	if chain.Status.Phase == criuorgv1.PhasePending {
		chain.Status.Phase = criuorgv1.PhaseRunning

		if err := r.Status().Update(ctx, chain); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil

	}

	// If the phase is running, we need to start/continue the snapshot chain
	if chain.Status.Phase == criuorgv1.PhaseRunning {

		//creating checkpoints
		creator := NewCheckpointCreator(
			r.Client,
			r.RestConfig,
		)

		pods, err := getMatchingPods(
			ctx,
			r.Client,
			chain.Spec.Namespace,
			&chain.Spec.Selector,
		)

		if err != nil {
			return ctrl.Result{}, err
		}

		//logic for container selection
		for _, pod := range pods {

			log.Info(
				"Resolved pod placement",
				"pod", pod.Name,
				"node", pod.Spec.NodeName,
			)

			containers := filterContainers(
				pod, chain.Spec.ContainerNames,
			)

			for _, container := range containers {

				err := creator.createCheckpoint(
					ctx,
					chain.Spec.Namespace,
					pod.Name,
					container.Name,
					pod.Spec.NodeName,
				)

				if err != nil {
					chain.Status.Phase = criuorgv1.PhaseFailed
					chain.Status.ErrorMessage = err.Error()

					_ = r.Status().Update(ctx, chain)

					return ctrl.Result{}, err
				}

				//this counts the number of checkpoint files created
				chain.Status.SnapshotCount++

				if chain.Spec.Capture.MaxSnapshots != nil && chain.Status.SnapshotCount >= *chain.Spec.Capture.MaxSnapshots {
					//Recording the completion time of the snapshot chain,
					//this will be used to calculate the duration of the snapshot chain execution

					now := metav1.Now()
					chain.Status.CompletionTime = &now

					//Once checkpoint creation is completed, we can update the status to completed and exit the reconciliation loop
					chain.Status.Phase = criuorgv1.PhaseCompleted

					if err := r.Status().Update(ctx, chain); err != nil {
						return ctrl.Result{}, err
					}

					return ctrl.Result{}, nil

				}

			}

			if err := r.Status().Update(ctx, chain); err != nil {
				return ctrl.Result{}, err
			}

		}

	}

	if chain.Spec.Capture.Interval != nil {
		return ctrl.Result{
			RequeueAfter: chain.Spec.Capture.Interval.Duration,
		}, nil
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
