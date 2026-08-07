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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

// WorkloadResolver turns a WorkloadReference into the Running pods that
// workload owns, using ownerReferences (UID-based) as the source of truth.
type WorkloadResolver struct {
	client client.Client
}

// NewWorkloadResolver constructs a WorkloadResolver backed by the given client.
func NewWorkloadResolver(c client.Client) *WorkloadResolver {
	return &WorkloadResolver{client: c}
}

// ResolvePods returns the Running pods owned (transitively) by the referenced
// workload in the given namespace.
func (r *WorkloadResolver) ResolvePods(
	ctx context.Context,
	namespace string,
	ref criuorgv1.WorkloadReference,
) ([]corev1.Pod, error) {
	switch ref.Kind {
	case "Job":
		return r.resolveJob(ctx, namespace, ref.Name)
	case "Deployment":
		return r.resolveDeployment(ctx, namespace, ref.Name)
	case "StatefulSet":
		return r.resolveStatefulSet(ctx, namespace, ref.Name)
	default:
		return nil, fmt.Errorf("unsupported workload kind %q", ref.Kind)
	}
}

func (r *WorkloadResolver) resolveJob(ctx context.Context, namespace, name string) ([]corev1.Pod, error) {
	job := &batchv1.Job{}
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, job); err != nil {
		return nil, fmt.Errorf("get Job %s/%s: %w", namespace, name, err)
	}
	return r.runningPodsOwnedBy(ctx, namespace, map[types.UID]bool{job.UID: true})
}

// runningPodsOwnedBy lists all pods in the namespace and returns those in the
// Running phase whose controller ownerReference UID is in ownerUIDs.
func (r *WorkloadResolver) runningPodsOwnedBy(
	ctx context.Context,
	namespace string,
	ownerUIDs map[types.UID]bool,
) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := r.client.List(ctx, podList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var out []corev1.Pod
	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, ref := range pod.OwnerReferences {
			if ownerUIDs[ref.UID] {
				out = append(out, pod)
				break
			}
		}
	}
	return out, nil
}

func (r *WorkloadResolver) resolveDeployment(ctx context.Context, namespace, name string) ([]corev1.Pod, error) {
	dep := &appsv1.Deployment{}
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, dep); err != nil {
		return nil, fmt.Errorf("get Deployment %s/%s: %w", namespace, name, err)
	}

	// Deployment -> ReplicaSet: collect every RS this Deployment controls.
	rsList := &appsv1.ReplicaSetList{}
	if err := r.client.List(ctx, rsList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	rsUIDs := map[types.UID]bool{}
	for _, rs := range rsList.Items {
		for _, ref := range rs.OwnerReferences {
			if ref.UID == dep.UID {
				rsUIDs[rs.UID] = true
				break
			}
		}
	}
	if len(rsUIDs) == 0 {
		return nil, nil
	}

	// ReplicaSet -> Pod.
	return r.runningPodsOwnedBy(ctx, namespace, rsUIDs)
}

func (r *WorkloadResolver) resolveStatefulSet(ctx context.Context, namespace, name string) ([]corev1.Pod, error) {
	sts := &appsv1.StatefulSet{}
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); err != nil {
		return nil, fmt.Errorf("get StatefulSet %s/%s: %w", namespace, name, err)
	}
	return r.runningPodsOwnedBy(ctx, namespace, map[types.UID]bool{sts.UID: true})
}
