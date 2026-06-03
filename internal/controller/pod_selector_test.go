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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("filterContainers", func() {
	var pod corev1.Pod

	BeforeEach(func() {
		pod = corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "app"},
					{Name: "sidecar"},
					{Name: "proxy"},
				},
			},
		}
	})

	It("returns all containers when the names list is empty", func() {
		Expect(filterContainers(pod, nil)).To(HaveLen(3))
		Expect(filterContainers(pod, []string{})).To(HaveLen(3))
	})

	It("returns only the named containers", func() {
		result := filterContainers(pod, []string{"app", "proxy"})
		Expect(result).To(HaveLen(2))
		Expect([]string{result[0].Name, result[1].Name}).To(ConsistOf("app", "proxy"))
	})

	It("returns empty when no container matches", func() {
		Expect(filterContainers(pod, []string{"unknown"})).To(BeEmpty())
	})
})

var _ = Describe("getMatchingPods", func() {
	var fakeClient fake.ClientBuilder

	BeforeEach(func() {
		runningPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "running-pod",
				Namespace: "default",
				Labels:    map[string]string{"app": "test"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		pendingPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pending-pod",
				Namespace: "default",
				Labels:    map[string]string{"app": "test"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodPending},
		}
		otherNsPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "other-ns-pod",
				Namespace: "other",
				Labels:    map[string]string{"app": "test"},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		fakeClient = *fake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(runningPod, pendingPod, otherNsPod).
			WithStatusSubresource(runningPod, pendingPod, otherNsPod)
	})

	It("returns only Running pods in the given namespace", func() {
		selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}}
		pods, err := getMatchingPods(context.Background(), fakeClient.Build(), "default", selector)
		Expect(err).NotTo(HaveOccurred())
		Expect(pods).To(HaveLen(1))
		Expect(pods[0].Name).To(Equal("running-pod"))
	})

	It("returns empty when no pods match the selector", func() {
		selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "nonexistent"}}
		pods, err := getMatchingPods(context.Background(), fakeClient.Build(), "default", selector)
		Expect(err).NotTo(HaveOccurred())
		Expect(pods).To(BeEmpty())
	})
})
