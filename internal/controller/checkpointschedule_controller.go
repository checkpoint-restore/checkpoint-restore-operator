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

	v1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

//+kubebuilder:rbac:groups=criu.org,resources=checkpointschedules,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=criu.org,resources=checkpointschedules/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=criu.org,resources=checkpointschedules/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch

const checkpointScheduleFinalizer = "criu.org/checkpoint-schedule-finalizer"

type Stoppable interface {
	Stop()
}

type CheckpointScheduleReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	RestConfig     *rest.Config
	activeTriggers map[string][]Stoppable
	mu             sync.Mutex
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

	key := req.NamespacedName.String()

	// CR is being deleted — stop triggers and remove finalizer
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

	// already running — don't start duplicates
	r.mu.Lock()
	_, running := r.activeTriggers[key]
	r.mu.Unlock()
	if running {
		return ctrl.Result{}, nil
	}

	creator := NewCheckpointCreator(r.Client, r.RestConfig)
	var triggers []Stoppable

	if schedule.Spec.Triggers.Schedule != "" {
		t := NewScheduleTrigger(r.Client, creator, schedule)
		if err := t.Start(ctx); err != nil {
			logger.Error(err, "failed to start schedule trigger")
			return ctrl.Result{}, err
		}
		triggers = append(triggers, t)
	}

	if schedule.Spec.Triggers.OnAnnotation {
		t := NewAnnotationTrigger(r.Client, creator, schedule)
		t.Start(ctx)
		triggers = append(triggers, t)
	}

	r.mu.Lock()
	if r.activeTriggers == nil {
		r.activeTriggers = make(map[string][]Stoppable)
	}
	r.activeTriggers[key] = triggers
	r.mu.Unlock()

	return ctrl.Result{}, nil
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
