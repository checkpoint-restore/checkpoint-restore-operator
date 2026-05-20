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

	v1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type CheckpointScheduleReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	RestConfig *rest.Config
}

func (r *CheckpointScheduleReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	// fetch the CR
	schedule := &v1.CheckpointSchedule{}
	if err := r.Get(ctx, req.NamespacedName, schedule); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	creator := NewCheckpointCreator(r.Client, r.RestConfig)

	trigger := NewScheduleTrigger(r.Client, creator, schedule)
	if err := trigger.Start(ctx); err != nil {
		logger.Error(err, "failed to start schedule trigger")
		return ctrl.Result{}, err
	}

	logger.Info("schedule trigger started", "name", schedule.Name)
	return ctrl.Result{}, nil

}
func (r *CheckpointScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.CheckpointSchedule{}).
		Complete(r)
}
