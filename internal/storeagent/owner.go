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

package storeagent

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// workload is the object a pod's snapshots are filed under.
type workload struct {
	kind, name string
	podUID     string
}

// workloadOf returns the workload that owns pod name in namespace: the
// Deployment above a ReplicaSet, the CronJob above a Job, or the pod's
// controller. A pod without a controller, or one that is already gone, is
// filed under itself, as kind Pod.
func workloadOf(ctx context.Context, c client.Reader, namespace, name string) (workload, error) {
	pod := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return workload{kind: "Pod", name: name}, nil
		}
		return workload{}, err
	}
	w := workload{kind: "Pod", name: name, podUID: string(pod.UID)}
	ref := metav1.GetControllerOf(pod)
	if ref == nil {
		return w, nil
	}
	w.kind, w.name = ref.Kind, ref.Name

	var parent client.Object
	switch ref.Kind {
	case "ReplicaSet":
		parent = &appsv1.ReplicaSet{}
	case "Job":
		parent = &batchv1.Job{}
	default:
		return w, nil
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, parent); err != nil {
		if apierrors.IsNotFound(err) {
			return w, nil
		}
		return workload{}, err
	}
	if top := metav1.GetControllerOf(parent); top != nil {
		w.kind, w.name = top.Kind, top.Name
	}
	return w, nil
}
