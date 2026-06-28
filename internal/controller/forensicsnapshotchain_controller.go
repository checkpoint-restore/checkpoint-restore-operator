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

	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1 "k8s.io/api/core/v1"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

// In case DefaultInterval is not specified by the user
const DefaultInterval = 2 * time.Second

// ForensicSnapshotChainReconciler reconciles a ForensicSnapshotChain object
type ForensicSnapshotChainReconciler struct {
	client.Client

	ClientSet *kubernetes.Clientset
	Scheme    *runtime.Scheme

	RestConfig *rest.Config
}

// +kubebuilder:rbac:groups=criu.org,resources=forensicsnapshotchains,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=criu.org,resources=forensicsnapshotchains/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=criu.org,resources=forensicsnapshotchains/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get

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
		log.Info(
			"ForensicSnapshotChain creation failed",
			"error", chain.Status.ErrorMessage,
		)
		return ctrl.Result{}, nil
	}

	// no PHASE -> PENDING PHASE
	//If the phase is empty, this is a new ForensicSnapshotChain
	// and we need to initialize it, that's why pending
	if chain.Status.Phase == criuorgv1.SnapshotChainPhase("") {
		chain.Status.Phase = criuorgv1.PhasePending

		meta.SetStatusCondition(
			&chain.Status.Conditions,
			metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				Reason:             "Pending",
				Message:            "ForensicSnapshotChain is pending, waiting to start snapshot chain",
				LastTransitionTime: metav1.Now(),
			},
		)

		//to record start time of the snapshot chain
		now := metav1.Now()
		chain.Status.StartTime = &now

		if err := r.Status().Update(ctx, chain); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil
	}

	//PENDING -> RUNNING PHASE
	//If snapshot chain is pending then make it running
	if chain.Status.Phase == criuorgv1.PhasePending {
		chain.Status.Phase = criuorgv1.PhaseRunning

		meta.SetStatusCondition(
			&chain.Status.Conditions,
			metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				Reason:             "Running",
				Message:            "ForensicSnapshotChain is running, snapshot chain is in progress",
				LastTransitionTime: metav1.Now(),
			},
		)

		if err := r.Status().Update(ctx, chain); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil

	}

	//RUNNING PHASE
	// If the phase is running, we need to start/continue the snapshot chain
	if chain.Status.Phase == criuorgv1.PhaseRunning {

		if chain.Spec.Integrity.HashAlgorithm != "" && chain.Spec.Integrity.HashAlgorithm != "sha256" {
			chain.Status.Phase = criuorgv1.PhaseFailed
			chain.Status.ErrorMessage = fmt.Sprintf("unsupported hash algorithm: %s", chain.Spec.Integrity.HashAlgorithm)

			meta.SetStatusCondition(
				&chain.Status.Conditions,
				metav1.Condition{
					Type:    "Ready",
					Status:  metav1.ConditionFalse,
					Reason:  "Failed",
					Message: fmt.Sprintf("unsupported hash algorithm: %s", chain.Spec.Integrity.HashAlgorithm),
				},
			)

			_ = r.Status().Update(ctx, chain)

			return ctrl.Result{}, fmt.Errorf("unsupported hash algorithm: %s", chain.Spec.Integrity.HashAlgorithm)
		}

		hashingEnabled := chain.Spec.Integrity.HashAlgorithm == "sha256"

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

		previousHash := ""
		if len(chain.Status.SnapshotChainRecords) > 0 {
			previousHash = chain.Status.SnapshotChainRecords[len(chain.Status.SnapshotChainRecords)-1].SHA256Hash
		}

		//logic for container selection/filteration
		for _, pod := range pods {

			containers := filterContainers(
				pod, chain.Spec.ContainerNames,
			)

			// Checking duration constraint before creating a checkpoint
			if chain.Spec.Capture.MaxDuration == nil {
				log.Info(
					"MaxDuration not specified is nil",
				)

			} else if chain.Status.StartTime != nil {
				//Calculating the elapsed time since the start of the snapshot chain execution
				elapsed := metav1.Now().Sub(chain.Status.StartTime.Time)

				//If the elapsed time exceeds the specified maximum duration, we can update the status to completed and exit the reconciliation loop
				if elapsed > chain.Spec.Capture.MaxDuration.Duration {

					now := metav1.Now()
					chain.Status.CompletionTime = &now
					chain.Status.Phase = criuorgv1.PhaseCompleted

					meta.SetStatusCondition(
						&chain.Status.Conditions,
						metav1.Condition{
							Type:               "Ready",
							Status:             metav1.ConditionTrue,
							Reason:             "MaxDurationReached",
							Message:            "Snapshot chain stopped as it reached the maximum duration",
							LastTransitionTime: metav1.Now(),
						},
					)

					//execute post-snapshot action if specified
					if err := r.executePostSnapshotAction(ctx, chain, pods); err != nil {
						log.Error(err, "Failed to execute post-snapshot action")
						//Irrespective of post snapshot action, chain shouldn't be failed.
						//Evidence should be preserved.
					}

					if err := r.Status().Update(ctx, chain); err != nil {
						return ctrl.Result{}, err
					}

					return ctrl.Result{}, nil

				}
			}

			for _, container := range containers {

				checkpointPath, err := creator.createCheckpoint(
					ctx,
					chain.Spec.Namespace,
					pod.Name,
					container.Name,
					pod.Spec.NodeName,
				)

				if err != nil {
					chain.Status.Phase = criuorgv1.PhaseFailed
					chain.Status.ErrorMessage = err.Error()

					meta.SetStatusCondition(
						&chain.Status.Conditions,
						metav1.Condition{
							Type:               "Ready",
							Status:             metav1.ConditionFalse,
							Reason:             "Failed",
							Message:            err.Error(),
							LastTransitionTime: metav1.Now(),
						},
					)

					_ = r.Status().Update(ctx, chain)

					return ctrl.Result{}, err
				}

				record := criuorgv1.SnapshotChainRecord{
					Index:             chain.Status.SnapshotCount,
					PodName:           pod.Name,
					ContainerName:     container.Name,
					SnapshotTimestamp: metav1.Now(),
					CheckpointPath:    checkpointPath,
				}
				if hashingEnabled {
					hash, hashErr := computeChecksum(ctx, r.Client, r.ClientSet, chain.Spec.Namespace, pod.Spec.NodeName, checkpointPath)
					if hashErr != nil {
						log.Error(hashErr, "Failed to compute checksum")
						meta.SetStatusCondition(&chain.Status.Conditions, metav1.Condition{
							Type:               "IntegrityVerified",
							Status:             metav1.ConditionFalse,
							Reason:             "HashGenerationFailed",
							Message:            fmt.Sprintf("snapshot %d: %s", record.Index, hashErr.Error()),
							LastTransitionTime: metav1.Now(),
						})
					} else {
						record.PreviousSHA256Hash = previousHash
						previousHash = hash
						record.SHA256Hash = hash
						meta.SetStatusCondition(&chain.Status.Conditions, metav1.Condition{
							Type:               "IntegrityVerified",
							Status:             metav1.ConditionTrue,
							Reason:             "HashGenerationSucceeded",
							Message:            "all snapshots successfully hashed",
							LastTransitionTime: metav1.Now(),
						})
					}
				}

				chain.Status.SnapshotChainRecords = append(chain.Status.SnapshotChainRecords, record)

				//this counts the number of checkpoint files created
				chain.Status.SnapshotCount++

				log.Info(
					"Checkpoint created",
					"pod", pod.Name,
					"container", container.Name,
				)

				if chain.Spec.Capture.MaxSnapshots == nil {
					log.Info(
						"MaxSnapshots not specified is nil",
					)

				} else if chain.Status.SnapshotCount >= *chain.Spec.Capture.MaxSnapshots {
					//Recording the completion time of the snapshot chain,
					//this will be used to calculate the duration of the snapshot chain execution

					now := metav1.Now()
					chain.Status.CompletionTime = &now

					//Once checkpoint creation is completed, we can update the status to completed and exit the reconciliation loop
					chain.Status.Phase = criuorgv1.PhaseCompleted

					meta.SetStatusCondition(
						&chain.Status.Conditions,
						metav1.Condition{
							Type:               "Ready",
							Status:             metav1.ConditionTrue,
							Reason:             "MaxSnapshotsReached",
							Message:            "Snapshot chain completed as it reached the maximum number of snapshots",
							LastTransitionTime: metav1.Now(),
						},
					)

					//execute post-snapshot action if specified
					if err := r.executePostSnapshotAction(ctx, chain, pods); err != nil {
						log.Error(err, "Failed to execute post-snapshot action")
						//Irrespective of post snapshot action, chain shouldn't be failed.
						//Evidence should be preserved.
					}

					if err := r.Status().Update(ctx, chain); err != nil {
						return ctrl.Result{}, err
					}
					return ctrl.Result{}, nil
				}

			}

		}

		if err := r.Status().Update(ctx, chain); err != nil {
			return ctrl.Result{}, err
		}

	}

	if chain.Spec.Capture.Interval == nil {
		log.Info(
			"Interval not specified, using default interval",
			"interval", DefaultInterval,
		)
		return ctrl.Result{
			RequeueAfter: DefaultInterval,
		}, nil
	} else {
		return ctrl.Result{
			RequeueAfter: chain.Spec.Capture.Interval.Duration,
		}, nil
	}

	//Unreachable code, but we need to return something to satisfy the function signature
	//return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ForensicSnapshotChainReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&criuorgv1.ForensicSnapshotChain{}).
		Named("forensicsnapshotchain").
		Complete(r)
}

// function to execute the post-snapshot action specified in the spec
func (r *ForensicSnapshotChainReconciler) executePostSnapshotAction(
	ctx context.Context,
	chain *criuorgv1.ForensicSnapshotChain,
	pods []corev1.Pod,
) error {
	log := logf.FromContext(ctx)

	if chain.Spec.PostSnapshotAction != criuorgv1.PostSnapshotActionDeletePod {
		return nil
	}

	for _, pod := range pods {
		log.Info("Deleting pod as post-snapshot action", "pod", pod.Name)
		if err := r.Delete(ctx, &pod); err != nil {
			return err
		}
	}

	return nil
}
