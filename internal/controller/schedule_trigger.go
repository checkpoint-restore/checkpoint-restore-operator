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
	"github.com/robfig/cron/v3"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type ScheduleTrigger struct {
	scheduler *cron.Cron
	creator   *CheckpointCreator
	client    client.Client
	schedule  *v1.CheckpointSchedule
}

func NewScheduleTrigger(c client.Client, creator *CheckpointCreator, schedule *v1.CheckpointSchedule) *ScheduleTrigger {
	return &ScheduleTrigger{
		scheduler: cron.New(),
		creator:   creator,
		client:    c,
		schedule:  schedule,
	}
}

func (st *ScheduleTrigger) Start(ctx context.Context) error {
	logger := log.FromContext(ctx)

	_, err := st.scheduler.AddFunc(st.schedule.Spec.Triggers.Schedule, func() {
		pods, err := getMatchingPods(ctx, st.client, st.schedule.Spec.Namespace, &st.schedule.Spec.Selector)
		if err != nil {
			logger.Error(err, "failed to get matching pods")
			return
		}

		for _, pod := range pods {
			containers := filterContainers(pod, st.schedule.Spec.ContainerNames)
			for _, container := range containers {
				err := st.creator.createCheckpoint(ctx, pod.Namespace, pod.Name, container.Name, pod.Spec.NodeName)
				if err != nil {
					logger.Error(err, "failed to create checkpoint", "pod", pod.Name, "container", container.Name)
				}
			}
		}
	})
	if err != nil {
		return err
	}

	st.scheduler.Start()
	logger.Info("schedule trigger started", "schedule", st.schedule.Spec.Triggers.Schedule)
	return nil
}

func (st *ScheduleTrigger) Stop() {
	st.scheduler.Stop()
}
