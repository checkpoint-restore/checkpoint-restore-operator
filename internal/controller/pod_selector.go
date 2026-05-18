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

	for _, container := range pod.Spec.Containers {
		for _, name := range containerNames {
			if container.Name == name {
				result = append(result, container)
				break
			}
		}
	}

	return result
}
