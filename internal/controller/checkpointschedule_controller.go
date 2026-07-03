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
	"sync"
	"time"

	v1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

//+kubebuilder:rbac:groups=criu.org,resources=checkpointschedules,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=criu.org,resources=checkpointschedules/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=criu.org,resources=checkpointschedules/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=nodes/proxy,verbs=get;create
//+kubebuilder:rbac:groups="",resources=nodes/checkpoint,verbs=create
//+kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list

const checkpointScheduleFinalizer = "criu.org/checkpoint-schedule-finalizer"

type Stoppable interface {
	Stop()
}

type CheckpointScheduleReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// RestConfig is used to reach the kubelet checkpoint API.
	RestConfig *rest.Config
	// Checkpointer overrides the default kubelet-proxy checkpoint creator;
	// used by tests.
	Checkpointer   Checkpointer
	activeTriggers map[string][]Stoppable
	mu             sync.Mutex
}

func (r *CheckpointScheduleReconciler) checkpointer() Checkpointer {
	if r.Checkpointer != nil {
		return r.Checkpointer
	}
	return NewCheckpointCreator(r.Client, r.RestConfig)
}

func (r *CheckpointScheduleReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	schedule := &v1.CheckpointSchedule{}
	if err := r.Get(ctx, req.NamespacedName, schedule); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	key := req.String()

	// CR is being deleted - stop triggers and remove finalizer
	if !schedule.DeletionTimestamp.IsZero() {
		r.stopTriggers(key)
		if controllerutil.ContainsFinalizer(schedule, checkpointScheduleFinalizer) {
			controllerutil.RemoveFinalizer(schedule, checkpointScheduleFinalizer)
			if err := r.Update(ctx, schedule); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// add finalizer if not present
	if !controllerutil.ContainsFinalizer(schedule, checkpointScheduleFinalizer) {
		controllerutil.AddFinalizer(schedule, checkpointScheduleFinalizer)
		if err := r.Update(ctx, schedule); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling", "name", schedule.Name)

	creator := r.checkpointer()

	// start the poll-based triggers once; periodic checkpoints below are
	// driven by the reconcile loop itself and need no goroutine
	r.mu.Lock()
	_, running := r.activeTriggers[key]
	r.mu.Unlock()
	if !running {
		var triggers []Stoppable

		if schedule.Spec.Triggers.OnAnnotation {
			t := NewAnnotationTrigger(r.Client, creator, schedule)
			t.Start(ctx)
			triggers = append(triggers, t)
		}

		if schedule.Spec.Triggers.ResourceThreshold != nil {
			t := NewResourceTrigger(r.Client, r.RestConfig, creator, schedule)
			t.Start(ctx)
			triggers = append(triggers, t)
		}

		if len(schedule.Spec.Triggers.OnKubernetesEvents) > 0 {
			t := NewEventTrigger(r.Client, creator, schedule)
			t.Start(ctx)
			triggers = append(triggers, t)
		}

		r.mu.Lock()
		if r.activeTriggers == nil {
			r.activeTriggers = make(map[string][]Stoppable)
		}
		r.activeTriggers[key] = triggers
		r.mu.Unlock()
	}

	return r.reconcileInterval(ctx, creator, schedule)
}

// reconcileInterval takes the periodic checkpoints driven by
// spec.triggers.interval. The last checkpoint time is persisted in
// status, so the cadence survives operator restarts and leader changes, and
// the next run is scheduled with RequeueAfter instead of a timer goroutine.
func (r *CheckpointScheduleReconciler) reconcileInterval(
	ctx context.Context,
	creator Checkpointer,
	schedule *v1.CheckpointSchedule,
) (ctrl.Result, error) {
	if schedule.Spec.Triggers.Interval == nil {
		return ctrl.Result{}, nil
	}
	interval := schedule.Spec.Triggers.Interval.Duration

	last := schedule.CreationTimestamp.Time
	if schedule.Status.LastCheckpointTime != nil {
		last = schedule.Status.LastCheckpointTime.Time
	}

	now := time.Now()
	if next := last.Add(interval); now.Before(next) {
		return ctrl.Result{RequeueAfter: next.Sub(now)}, nil
	}

	created := runScheduledCheckpoints(ctx, r.Client, creator, schedule)

	// the checkpoints have already been taken at this point: retry the
	// status update on conflict so a concurrent writer cannot cause the next
	// reconcile to see a stale anchor and checkpoint again
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &v1.CheckpointSchedule{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(schedule), latest); err != nil {
			return err
		}
		latest.Status.LastCheckpointTime = &metav1.Time{Time: now}
		latest.Status.CheckpointsCreated += int(created)
		return r.Status().Update(ctx, latest)
	}); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: interval}, nil
}

func (r *CheckpointScheduleReconciler) stopTriggers(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.activeTriggers[key] {
		t.Stop()
	}
	delete(r.activeTriggers, key)
}

func (r *CheckpointScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.CheckpointSchedule{}).
		Complete(r)
}
