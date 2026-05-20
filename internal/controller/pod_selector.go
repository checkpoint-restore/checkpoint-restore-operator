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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func getMatchingPods(
	ctx context.Context,
	c client.Client,
	namespace string,
	selector *metav1.LabelSelector,
) ([]corev1.Pod, error) {

	podList := &corev1.PodList{}

	labelSelector, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, err
	}
	err = c.List(ctx, podList, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: labelSelector})
	if err != nil {
		return nil, err
	}

	var runninPod []corev1.Pod

	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodRunning {
			runninPod = append(runninPod, pod)
		}
	}
	return runninPod, nil

}

func filterContainers(pod corev1.Pod, containerNames []string) []corev1.Container {
	if len(containerNames) == 0 {
		return pod.Spec.Containers
	}

	var result []corev1.Container

	nameSet := make(map[string]bool)
	for _, name := range containerNames {
		nameSet[name] = true
	}

	for _, container := range pod.Spec.Containers {
		if nameSet[container.Name] {
			result = append(result, container)
		}
	}

	return result
}
