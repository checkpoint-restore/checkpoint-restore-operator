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
	"time"

	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1 "k8s.io/api/core/v1"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

// In case DefaultInterval is not specified by the user. Full checkpoints are
// expensive, so the default cadence is conservative.
const DefaultInterval = 30 * time.Second

// maxConsecutiveCheckpointFailures bounds how many consecutive capture rounds
// may fail before a chain configured with maxSnapshots but no maxDuration gives
// up and moves to the Failed phase. createCheckpoint already retries transient
// kubelet errors internally, so reaching this many whole-round failures
// indicates an unrecoverable target rather than a passing glitch. When a
// maxDuration backstop is set, that time bound governs instead and the chain
// keeps retrying until it elapses.
const maxConsecutiveCheckpointFailures = 5

// ForensicSnapshotChainReconciler reconciles a ForensicSnapshotChain object
type ForensicSnapshotChainReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// RestConfig is used to reach the kubelet checkpoint API.
	RestConfig *rest.Config
	// Checkpointer overrides the default kubelet-proxy checkpoint creator;
	// used by tests.
	Checkpointer Checkpointer
}

func (r *ForensicSnapshotChainReconciler) checkpointer() Checkpointer {
	if r.Checkpointer != nil {
		return r.Checkpointer
	}
	return NewCheckpointCreator(r.Client, r.RestConfig)
}

// +kubebuilder:rbac:groups=criu.org,resources=forensicsnapshotchains,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=criu.org,resources=forensicsnapshotchains/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=criu.org,resources=forensicsnapshotchains/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete

// Reconcile drives a ForensicSnapshotChain through its phase state machine
// ("" -> Pending -> Running -> Completed/Failed), creating a round of container
// checkpoints on each Running reconcile until a stop condition is reached.
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
		if chain.Spec.Capture.Interval == nil {
			log.Info(
				"Interval not specified, using default interval",
				"interval", DefaultInterval,
			)
		}
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
		chain.Status.ObservedGeneration = chain.Generation

		if err := r.Status().Update(ctx, chain); err != nil {
			return ctrl.Result{}, err
		}

		// The status update re-enqueues this object through the controller's
		// own watch; no explicit requeue is needed.
		return ctrl.Result{}, nil
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
		chain.Status.ObservedGeneration = chain.Generation

		if err := r.Status().Update(ctx, chain); err != nil {
			return ctrl.Result{}, err
		}

		// The status update re-enqueues this object through the controller's
		// own watch; no explicit requeue is needed.
		return ctrl.Result{}, nil

	}

	//RUNNING PHASE
	// If the phase is running, we create one round of checkpoints per reconcile
	// until a stop condition (maxDuration or maxSnapshots) is reached.
	if chain.Status.Phase == criuorgv1.PhaseRunning {
		// MaxDuration backstop. Checked once per reconcile, before the capture
		// round and independent of how many pods match, so an idle or
		// mis-targeted chain still terminates.
		if chain.Spec.Capture.MaxDuration != nil && chain.Status.StartTime != nil {
			elapsed := metav1.Now().Sub(chain.Status.StartTime.Time)
			if elapsed > chain.Spec.Capture.MaxDuration.Duration {
				// Only the DeletePod action needs the pod list; skip the
				// List call entirely when no destructive action is configured.
				var pods []corev1.Pod
				if chain.Spec.PostSnapshotAction == criuorgv1.PostSnapshotActionDeletePod {
					var err error
					pods, err = getMatchingPods(
						ctx,
						r.Client,
						chain.Spec.Namespace,
						&chain.Spec.Selector,
					)
					if err != nil {
						return ctrl.Result{}, err
					}
				}

				// Persist the terminal status before running the post-snapshot
				// action, so the destructive action only fires once the chain is
				// durably Completed. If the status write fails we retry with the
				// pods untouched, instead of deleting them and then looping.
				if err := r.updateChainStatus(ctx, chain, func(latest *criuorgv1.ForensicSnapshotChain) {
					now := metav1.Now()
					latest.Status.CompletionTime = &now
					latest.Status.Phase = criuorgv1.PhaseCompleted
					meta.SetStatusCondition(
						&latest.Status.Conditions,
						metav1.Condition{
							Type:               "Ready",
							Status:             metav1.ConditionTrue,
							Reason:             "MaxDurationReached",
							Message:            "Snapshot chain stopped as it reached the maximum duration",
							LastTransitionTime: metav1.Now(),
						},
					)
				}); err != nil {
					return ctrl.Result{}, err
				}

				//execute post-snapshot action if specified
				if err := r.executePostSnapshotAction(ctx, chain, pods, chain.Status.SnapshotCount); err != nil {
					log.Error(err, "Failed to execute post-snapshot action")
					//Irrespective of post snapshot action, chain shouldn't be failed.
					//Evidence should be preserved.
				}

				return ctrl.Result{}, nil
			}
		}

		interval := captureInterval(chain)
		if chain.Status.LastSnapshotTime != nil {
			next := chain.Status.LastSnapshotTime.Add(interval)
			if now := time.Now(); now.Before(next) {
				return ctrl.Result{RequeueAfter: next.Sub(now)}, nil
			}
		}

		pods, err := getMatchingPods(
			ctx,
			r.Client,
			chain.Spec.Namespace,
			&chain.Spec.Selector,
		)
		if err != nil {
			return ctrl.Result{}, err
		}

		// One capture round: checkpoint every selected container of every
		// matching pod. A round is not transactional: if a checkpoint fails
		// partway through, the round is abandoned and retried from the first
		// container, so containers checkpointed before the failure are
		// captured again on the retry. For forensic capture this duplicate
		// work is acceptable (each checkpoint is an independent archive).
		creator := r.checkpointer()
		captured := 0
		for _, pod := range pods {
			for _, container := range filterContainers(pod, chain.Spec.ContainerNames) {
				if _, err := creator.createCheckpoint(
					ctx,
					chain.Spec.Namespace,
					pod.Name,
					container.Name,
					pod.Spec.NodeName,
				); err != nil {
					// A checkpoint error is treated as transient by default: the
					// chain stays Running and the reconcile is retried with
					// rate-limited backoff. A single flaky kubelet response must
					// not end an ongoing forensic capture.
					//
					// To keep the termination guarantee for a chain configured
					// with maxSnapshots but no maxDuration, consecutive whole-round
					// failures are bounded: once maxConsecutiveCheckpointFailures
					// is reached the target is treated as unrecoverable and the
					// chain moves to Failed. When a maxDuration backstop is set,
					// that time bound governs instead and the chain keeps retrying.
					failureCount := chain.Status.FailureCount + 1
					giveUp := chain.Spec.Capture.MaxDuration == nil &&
						failureCount >= maxConsecutiveCheckpointFailures

					log.Error(err, "Checkpoint failed",
						"pod", pod.Name,
						"container", container.Name,
						"consecutiveFailures", failureCount,
						"giveUp", giveUp,
					)

					statusErr := r.updateChainStatus(ctx, chain, func(latest *criuorgv1.ForensicSnapshotChain) {
						latest.Status.FailureCount++
						latest.Status.ErrorMessage = err.Error()
						reason := "CheckpointError"
						if giveUp {
							latest.Status.Phase = criuorgv1.PhaseFailed
							failNow := metav1.Now()
							latest.Status.CompletionTime = &failNow
							reason = "CheckpointFailed"
						}
						meta.SetStatusCondition(
							&latest.Status.Conditions,
							metav1.Condition{
								Type:               "Ready",
								Status:             metav1.ConditionFalse,
								Reason:             reason,
								Message:            err.Error(),
								LastTransitionTime: metav1.Now(),
							},
						)
					})
					if statusErr != nil {
						return ctrl.Result{}, statusErr
					}
					// Once durably Failed there is nothing to retry, so do not
					// return the error (which would requeue with backoff).
					if giveUp {
						return ctrl.Result{}, nil
					}
					return ctrl.Result{}, err
				}

				captured++
				log.Info(
					"Checkpoint created",
					"pod", pod.Name,
					"container", container.Name,
				)
			}
		}

		now := metav1.Now()
		attemptCount := chain.Status.AttemptCount + 1
		snapshotCount := chain.Status.SnapshotCount
		if captured > 0 {
			snapshotCount++
		}

		// Decide whether this round completes the chain. There are two ways to
		// reach maxSnapshots:
		//   - snapshotCount: enough non-empty rounds have been captured. This
		//     is the normal success path.
		//   - attemptCount, only when maxDuration is unset and this round was
		//     itself empty (captured == 0): the selector is matching no pods,
		//     so the chain can never make progress toward maxSnapshots. Without
		//     a time backstop it would otherwise requeue forever, so capping
		//     attempts at maxSnapshots guarantees termination in a bounded
		//     number of rounds. The captured == 0 guard means a chain that is
		//     still capturing (for example one whose pods started late) is never
		//     cut off; it terminates only on an empty round. When maxDuration is
		//     set, empty rounds are instead bounded by the time backstop above
		//     and do not consume the snapshot budget.
		done := false
		reason, message := "", ""
		if chain.Spec.Capture.MaxSnapshots != nil {
			limit := *chain.Spec.Capture.MaxSnapshots
			switch {
			case snapshotCount >= limit:
				done, reason = true, "MaxSnapshotsReached"
				message = "Snapshot chain completed as it reached the maximum number of snapshots"
			case captured == 0 && chain.Spec.Capture.MaxDuration == nil && attemptCount >= limit:
				done, reason = true, "NoMatchingPods"
				message = "Snapshot chain completed without capturing the requested number of snapshots because no pods matched the selector"
			}
		}

		if done {
			// Persist the terminal status before running the post-snapshot
			// action, so the destructive action only fires once the chain is
			// durably Completed. If the status write fails we retry with the
			// pods untouched, instead of deleting them and then looping.
			if err := r.updateChainStatus(ctx, chain, func(latest *criuorgv1.ForensicSnapshotChain) {
				latest.Status.LastSnapshotTime = &now
				latest.Status.AttemptCount++
				// This round did not fail, so the consecutive-failure streak ends.
				latest.Status.FailureCount = 0
				if captured > 0 {
					latest.Status.SnapshotCount++
					// A successful round clears any error left by an earlier
					// transient checkpoint failure.
					latest.Status.ErrorMessage = ""
				}
				latest.Status.CompletionTime = &now
				latest.Status.Phase = criuorgv1.PhaseCompleted
				meta.SetStatusCondition(
					&latest.Status.Conditions,
					metav1.Condition{
						Type:               "Ready",
						Status:             metav1.ConditionTrue,
						Reason:             reason,
						Message:            message,
						LastTransitionTime: metav1.Now(),
					},
				)
			}); err != nil {
				return ctrl.Result{}, err
			}

			//execute post-snapshot action if specified
			if err := r.executePostSnapshotAction(ctx, chain, pods, snapshotCount); err != nil {
				log.Error(err, "Failed to execute post-snapshot action")
				//Irrespective of post snapshot action, chain shouldn't be failed.
				//Evidence should be preserved.
			}

			return ctrl.Result{}, nil
		}

		if err := r.updateChainStatus(ctx, chain, func(latest *criuorgv1.ForensicSnapshotChain) {
			latest.Status.LastSnapshotTime = &now
			latest.Status.AttemptCount++
			// This round did not fail, so the consecutive-failure streak ends.
			latest.Status.FailureCount = 0
			// SnapshotCount tracks completed capture rounds. Empty rounds (no
			// matching pods/containers) do not count toward maxSnapshots; they
			// are bounded by the maxDuration backstop above, or by the
			// attemptCount cap when maxDuration is unset.
			if captured > 0 {
				latest.Status.SnapshotCount++
				// A successful round clears the error and Ready condition left
				// by an earlier transient checkpoint failure.
				if latest.Status.ErrorMessage != "" {
					latest.Status.ErrorMessage = ""
					meta.SetStatusCondition(
						&latest.Status.Conditions,
						metav1.Condition{
							Type:               "Ready",
							Status:             metav1.ConditionFalse,
							Reason:             "Running",
							Message:            "ForensicSnapshotChain is running, snapshot chain is in progress",
							LastTransitionTime: metav1.Now(),
						},
					)
				}
			}
		}); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{
			RequeueAfter: captureInterval(chain),
		}, nil
	}

	// Any other phase value is unrecognized (for example a manually edited or
	// future-version status). Take no action and do not requeue, rather than
	// silently looping; a corrected status arrives as a new watch event.
	log.Info("Unrecognized phase, taking no action", "phase", chain.Status.Phase)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ForensicSnapshotChainReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&criuorgv1.ForensicSnapshotChain{}).
		Named("forensicsnapshotchain").
		Complete(r)
}

func captureInterval(chain *criuorgv1.ForensicSnapshotChain) time.Duration {
	if chain.Spec.Capture.Interval == nil {
		return DefaultInterval
	}
	return chain.Spec.Capture.Interval.Duration
}

func (r *ForensicSnapshotChainReconciler) updateChainStatus(
	ctx context.Context,
	chain *criuorgv1.ForensicSnapshotChain,
	mutate func(*criuorgv1.ForensicSnapshotChain),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &criuorgv1.ForensicSnapshotChain{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(chain), latest); err != nil {
			return err
		}
		mutate(latest)
		latest.Status.ObservedGeneration = latest.Generation
		return r.Status().Update(ctx, latest)
	})
}

// function to execute the post-snapshot action specified in the spec
func (r *ForensicSnapshotChainReconciler) executePostSnapshotAction(
	ctx context.Context,
	chain *criuorgv1.ForensicSnapshotChain,
	pods []corev1.Pod,
	snapshotCount int32,
) error {
	log := logf.FromContext(ctx)

	if chain.Spec.PostSnapshotAction != criuorgv1.PostSnapshotActionDeletePod {
		return nil
	}

	if snapshotCount == 0 {
		log.Info("Skipping pod deletion because no snapshots were captured", "chain", chain.Name)
		return nil
	}

	for _, pod := range pods {
		log.Info("Deleting pod as post-snapshot action", "pod", pod.Name)
		// IgnoreNotFound: the pod may already be gone (e.g. on a retried
		// reconcile), which is not an error for a containment action.
		if err := client.IgnoreNotFound(r.Delete(ctx, &pod)); err != nil {
			return err
		}
	}

	return nil
}
