package controller

import (
	"context"
	"sync"
	"time"

	v1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const CheckpointTriggerAnnotation = "checkpoint.criu.org/trigger"

type AnnotationTrigger struct {
	client   client.Client
	creator  Checkpointer
	schedule *v1.CheckpointSchedule
	stopCh   chan struct{}
	wg       sync.WaitGroup
	interval time.Duration
}

func NewAnnotationTrigger(c client.Client, creator Checkpointer, schedule *v1.CheckpointSchedule) *AnnotationTrigger {
	return &AnnotationTrigger{
		client:   c,
		creator:  creator,
		schedule: schedule,
		stopCh:   make(chan struct{}),
		interval: 30 * time.Second,
	}
}

func (at *AnnotationTrigger) Start(ctx context.Context) {
	logger := log.FromContext(ctx)
	logger.Info("annotation trigger started", "interval", at.interval)

	at.wg.Add(1)
	go func() {
		defer at.wg.Done()
		ticker := time.NewTicker(at.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				at.run(ctx)
			case <-at.stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (at *AnnotationTrigger) run(ctx context.Context) {
	logger := log.FromContext(ctx)

	pods, err := getMatchingPods(ctx, at.client, at.schedule.Spec.Namespace, &at.schedule.Spec.Selector)
	if err != nil {
		logger.Error(err, "annotation trigger: failed to list pods")
		return
	}

	for _, pod := range pods {
		if pod.Annotations[CheckpointTriggerAnnotation] != "true" {
			continue
		}

		containers := filterContainers(pod, at.schedule.Spec.ContainerNames)
		containerNames := make([]string, 0, len(containers))
		for _, c := range containers {
			containerNames = append(containerNames, c.Name)
		}

		// Snapshot the pod's volumes before checkpointing any of its containers.
		// Under Require, a snapshot failure skips the pod (and keeps the
		// annotation) so the request is retried rather than silently consumed.
		refs, proceed := captureVolumeSnapshots(ctx, at.client, at.schedule.Spec.VolumeSnapshots, &pod, containerNames)
		if !proceed {
			continue
		}

		failed := false
		for _, c := range containers {
			path, err := at.creator.createCheckpoint(ctx, pod.Namespace, pod.Name, c.Name, pod.Spec.NodeName)
			if err != nil {
				logger.Error(err, "annotation trigger: checkpoint failed", "pod", pod.Name, "container", c.Name)
				failed = true
				continue
			}
			linkSnapshotsToArchive(ctx, at.client, pod.Namespace, refs, path)
		}

		// keep the annotation when a checkpoint failed, so the request is
		// retried on the next poll instead of being silently consumed
		if failed {
			continue
		}

		// clear the annotation so we don't checkpoint again next poll
		patch := client.MergeFrom(pod.DeepCopy())
		delete(pod.Annotations, CheckpointTriggerAnnotation)
		if err := at.client.Patch(ctx, &pod, patch); err != nil {
			logger.Error(err, "annotation trigger: failed to clear annotation", "pod", pod.Name)
		}
	}
}

func (at *AnnotationTrigger) Stop() {
	close(at.stopCh)
	at.wg.Wait()
}
