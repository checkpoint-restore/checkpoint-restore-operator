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

	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

// WorkloadCheckpointReconciler reconciles a WorkloadCheckpoint object.
type WorkloadCheckpointReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	RestConfig *rest.Config
	// Checkpointer overrides the default kubelet-proxy checkpoint creator; used by tests.
	Checkpointer Checkpointer
	// Resolver overrides the default owner-based pod resolver; used by tests.
	Resolver *WorkloadResolver
	// Agent delivers the coordinator dependency map to nodes (coordinated runs).
	Agent NodeAgentClient
	// CoordinatorAddr is the numeric ClusterIP of the coordinator Service,
	// written into node maps (nodes cannot resolve cluster DNS).
	CoordinatorAddr string
	// CoordinatorPort is the coordinator server port (default "8080").
	CoordinatorPort string
}

func (r *WorkloadCheckpointReconciler) checkpointer() Checkpointer {
	if r.Checkpointer != nil {
		return r.Checkpointer
	}
	return NewCheckpointCreator(r.Client, r.RestConfig)
}

func (r *WorkloadCheckpointReconciler) resolver() *WorkloadResolver {
	if r.Resolver != nil {
		return r.Resolver
	}
	return NewWorkloadResolver(r.Client)
}

// +kubebuilder:rbac:groups=criu.org,resources=workloadcheckpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=criu.org,resources=workloadcheckpoints/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=criu.org,resources=workloadcheckpoints/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;replicasets;statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch

// Reconcile drives a WorkloadCheckpoint through "" -> Pending -> Running ->
// Completed/PartiallyFailed/Failed. It is one-shot: once terminal it does nothing.
func (r *WorkloadCheckpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	wc := &criuorgv1.WorkloadCheckpoint{}
	if err := r.Get(ctx, req.NamespacedName, wc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	switch wc.Status.Phase {
	case criuorgv1.WorkloadPhaseCompleted,
		criuorgv1.WorkloadPhasePartiallyFailed,
		criuorgv1.WorkloadPhaseFailed:
		return ctrl.Result{}, nil

	case criuorgv1.WorkloadCheckpointPhase(""):
		return r.toPending(ctx, wc)

	case criuorgv1.WorkloadPhasePending:
		return r.toRunning(ctx, wc)

	case criuorgv1.WorkloadPhaseRunning:
		return r.runCheckpoints(ctx, wc)

	default:
		log.Info("Unrecognized phase, taking no action", "phase", wc.Status.Phase)
		return ctrl.Result{}, nil
	}
}

func (r *WorkloadCheckpointReconciler) toPending(ctx context.Context, wc *criuorgv1.WorkloadCheckpoint) (ctrl.Result, error) {
	return ctrl.Result{}, r.updateStatus(ctx, wc, func(latest *criuorgv1.WorkloadCheckpoint) {
		latest.Status.Phase = criuorgv1.WorkloadPhasePending
		now := metav1.Now()
		latest.Status.StartTime = &now
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "Pending",
			Message: "WorkloadCheckpoint pending", LastTransitionTime: metav1.Now(),
		})
	})
}

func (r *WorkloadCheckpointReconciler) toRunning(ctx context.Context, wc *criuorgv1.WorkloadCheckpoint) (ctrl.Result, error) {
	return ctrl.Result{}, r.updateStatus(ctx, wc, func(latest *criuorgv1.WorkloadCheckpoint) {
		latest.Status.Phase = criuorgv1.WorkloadPhaseRunning
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "Running",
			Message: "WorkloadCheckpoint in progress", LastTransitionTime: metav1.Now(),
		})
	})
}

// runCheckpoints resolves the workload's Running pods and checkpoints each
// pod's selected containers. Pods already recorded in status.checkpoints are
// skipped, so a mid-run restart does not re-checkpoint them (idempotency).
//
// The group snapshot is not atomic: pods are captured sequentially, so it is
// crash-consistent per pod, not globally consistent across replicas.
func (r *WorkloadCheckpointReconciler) runCheckpoints(ctx context.Context, wc *criuorgv1.WorkloadCheckpoint) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if wc.Spec.Coordinated {
		return r.runCoordinatedCheckpoints(ctx, wc)
	}

	pods, err := r.resolver().ResolvePods(ctx, wc.Namespace, wc.Spec.TargetRef)
	if err != nil {
		// Resolution failure (missing workload, unknown kind) is terminal.
		return ctrl.Result{}, r.updateStatus(ctx, wc, func(latest *criuorgv1.WorkloadCheckpoint) {
			latest.Status.Phase = criuorgv1.WorkloadPhaseFailed
			now := metav1.Now()
			latest.Status.CompletionTime = &now
			meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
				Type: "Ready", Status: metav1.ConditionFalse, Reason: "ResolveFailed",
				Message: err.Error(), LastTransitionTime: metav1.Now(),
			})
		})
	}

	already := map[string]bool{}
	for _, rec := range wc.Status.Checkpoints {
		already[rec.PodName] = true
	}

	creator := r.checkpointer()
	var records []criuorgv1.PodCheckpointRecord
	checkpointed, failed := 0, 0

	for _, pod := range pods {
		if already[pod.Name] {
			continue
		}
		rec := criuorgv1.PodCheckpointRecord{
			PodName: pod.Name, NodeName: pod.Spec.NodeName, ReplicaKey: pod.Name,
		}
		podFailed := false
		for _, container := range filterContainers(pod, wc.Spec.ContainerNames) {
			path, cerr := creator.createCheckpoint(ctx, wc.Namespace, pod.Name, container.Name, pod.Spec.NodeName)
			cr := criuorgv1.ContainerCheckpointRecord{Container: container.Name}
			if cerr != nil {
				podFailed = true
				cr.Error = cerr.Error()
				log.Error(cerr, "checkpoint failed", "pod", pod.Name, "container", container.Name)
			} else {
				cr.Path = path
				cr.CheckpointTime = metav1.Now()
			}
			rec.Containers = append(rec.Containers, cr)
		}
		if podFailed {
			failed++
		} else {
			checkpointed++
		}
		records = append(records, rec)
	}

	resolved := int32(len(pods))
	allOrNothing := wc.Spec.FailurePolicy == criuorgv1.FailurePolicyAllOrNothing
	phase := criuorgv1.WorkloadPhaseCompleted
	reason, message := "AllPodsCheckpointed", "all pods checkpointed"
	status := metav1.ConditionTrue
	if failed > 0 {
		if allOrNothing {
			phase, reason, message, status = criuorgv1.WorkloadPhaseFailed, "CheckpointFailed", "one or more pods failed (AllOrNothing)", metav1.ConditionFalse
		} else {
			phase, reason, message, status = criuorgv1.WorkloadPhasePartiallyFailed, "PartiallyCheckpointed", "some pods failed", metav1.ConditionFalse
		}
	}

	return ctrl.Result{}, r.updateStatus(ctx, wc, func(latest *criuorgv1.WorkloadCheckpoint) {
		latest.Status.Checkpoints = append(latest.Status.Checkpoints, records...)
		latest.Status.ResolvedPodCount = resolved
		latest.Status.CheckpointedPodCount += int32(checkpointed)
		latest.Status.FailedPodCount += int32(failed)
		latest.Status.Phase = phase
		now := metav1.Now()
		latest.Status.CompletionTime = &now
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: status, Reason: reason, Message: message, LastTransitionTime: metav1.Now(),
		})
	})
}

// runCoordinatedCheckpoints checkpoints all resolved members together at the
// criu-coordinator barrier: it builds a full-mesh dependency map, delivers it
// to every involved node's agent, fires all members concurrently, and clears
// the node maps afterwards. Coordinated runs are group-atomic (AllOrNothing).
func (r *WorkloadCheckpointReconciler) runCoordinatedCheckpoints(ctx context.Context, wc *criuorgv1.WorkloadCheckpoint) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	pods, err := r.resolver().ResolvePods(ctx, wc.Namespace, wc.Spec.TargetRef)
	if err != nil {
		return ctrl.Result{}, r.failCoordinated(ctx, wc, "ResolveFailed", err.Error())
	}

	depMap, members, err := buildCoordinationMap(pods, wc.Spec.ContainerNames)
	if err != nil {
		return ctrl.Result{}, r.failCoordinated(ctx, wc, "MapBuildFailed", err.Error())
	}
	if len(members) == 0 {
		return ctrl.Result{}, r.failCoordinated(ctx, wc, "NoMembers", "no running containers to coordinate")
	}

	port := r.CoordinatorPort
	if port == "" {
		port = "8080"
	}
	cfg := CoordinatorConfig{Address: r.CoordinatorAddr, Port: port, Dependencies: depMap}

	// Nodes involved in this group.
	nodes := map[string]bool{}
	for _, m := range members {
		nodes[m.NodeName] = true
	}

	// Deliver the map to every involved node; always clear on the way out so a
	// stale map never lingers (even on failure).
	defer func() {
		for node := range nodes {
			if cerr := r.Agent.ClearMap(context.WithoutCancel(ctx), node); cerr != nil {
				log.Error(cerr, "failed to clear coordinator map", "node", node)
			}
		}
	}()
	for node := range nodes {
		if werr := r.Agent.WriteMap(ctx, node, cfg); werr != nil {
			return ctrl.Result{}, r.failCoordinated(ctx, wc, "MapWriteFailed", "node "+node+": "+werr.Error())
		}
	}

	// Fire all members concurrently so they rendezvous at the barrier.
	creator := r.checkpointer()
	type result struct {
		member coordMember
		path   string
		err    error
	}
	results := make([]result, len(members))
	var wg sync.WaitGroup
	for i, m := range members {
		wg.Add(1)
		go func(i int, m coordMember) {
			defer wg.Done()
			path, cerr := creator.createCheckpoint(ctx, wc.Namespace, m.PodName, m.ContainerName, m.NodeName)
			results[i] = result{member: m, path: path, err: cerr}
		}(i, m)
	}
	wg.Wait()

	// Aggregate per pod. Coordinated runs are group-atomic (AllOrNothing).
	recByPod := map[string]*criuorgv1.PodCheckpointRecord{}
	var order []string
	for _, res := range results {
		rec := recByPod[res.member.PodName]
		if rec == nil {
			rec = &criuorgv1.PodCheckpointRecord{
				PodName: res.member.PodName, NodeName: res.member.NodeName, ReplicaKey: res.member.PodName,
			}
			recByPod[res.member.PodName] = rec
			order = append(order, res.member.PodName)
		}
		cr := criuorgv1.ContainerCheckpointRecord{Container: res.member.ContainerName}
		if res.err != nil {
			cr.Error = res.err.Error()
			log.Error(res.err, "coordinated checkpoint failed", "pod", res.member.PodName, "container", res.member.ContainerName)
		} else {
			cr.Path = res.path
			cr.CheckpointTime = metav1.Now()
		}
		rec.Containers = append(rec.Containers, cr)
	}

	var records []criuorgv1.PodCheckpointRecord
	for _, name := range order {
		records = append(records, *recByPod[name])
	}
	failedPods := countFailedPods(recByPod)

	phase := criuorgv1.WorkloadPhaseCompleted
	reason, message, status := "AllPodsCheckpointed", "coordinated group checkpointed", metav1.ConditionTrue
	if failedPods > 0 {
		phase, reason, message, status = criuorgv1.WorkloadPhaseFailed, "CoordinatedGroupFailed",
			"one or more members failed the coordinated barrier", metav1.ConditionFalse
	}

	checkpointedPods := len(order) - failedPods
	return ctrl.Result{}, r.updateStatus(ctx, wc, func(latest *criuorgv1.WorkloadCheckpoint) {
		latest.Status.Checkpoints = append(latest.Status.Checkpoints, records...)
		latest.Status.ResolvedPodCount = int32(len(pods))
		latest.Status.CheckpointedPodCount += int32(checkpointedPods)
		latest.Status.FailedPodCount += int32(failedPods)
		latest.Status.Phase = phase
		now := metav1.Now()
		latest.Status.CompletionTime = &now
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: status, Reason: reason, Message: message, LastTransitionTime: metav1.Now(),
		})
	})
}

// countFailedPods counts pods that had at least one container fail.
func countFailedPods(recByPod map[string]*criuorgv1.PodCheckpointRecord) int {
	n := 0
	for _, rec := range recByPod {
		for _, c := range rec.Containers {
			if c.Error != "" {
				n++
				break
			}
		}
	}
	return n
}

// failCoordinated marks a coordinated run terminally Failed with the given reason.
func (r *WorkloadCheckpointReconciler) failCoordinated(ctx context.Context, wc *criuorgv1.WorkloadCheckpoint, reason, msg string) error {
	return r.updateStatus(ctx, wc, func(latest *criuorgv1.WorkloadCheckpoint) {
		latest.Status.Phase = criuorgv1.WorkloadPhaseFailed
		now := metav1.Now()
		latest.Status.CompletionTime = &now
		meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: reason, Message: msg, LastTransitionTime: metav1.Now(),
		})
	})
}

func (r *WorkloadCheckpointReconciler) updateStatus(
	ctx context.Context,
	wc *criuorgv1.WorkloadCheckpoint,
	mutate func(*criuorgv1.WorkloadCheckpoint),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &criuorgv1.WorkloadCheckpoint{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(wc), latest); err != nil {
			return err
		}
		mutate(latest)
		latest.Status.ObservedGeneration = latest.Generation
		return r.Status().Update(ctx, latest)
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkloadCheckpointReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&criuorgv1.WorkloadCheckpoint{}).
		Named("workloadcheckpoint").
		Complete(r)
}
