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
	"sort"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

func resolverScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func ownerRef(apiVersion, kind, name string, uid types.UID) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		UID:        uid,
		Controller: &controller,
	}
}

func ownedRunningPod(name, ns, node string, owners ...metav1.OwnerReference) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: owners},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func podNames(pods []corev1.Pod) []string {
	names := make([]string, 0, len(pods))
	for _, p := range pods {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}

func TestResolvePodsJob(t *testing.T) {
	s := resolverScheme(t)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns", UID: "job-uid"}}
	owned := ownedRunningPod("j-abc", "ns", "node-1", ownerRef("batch/v1", "Job", "j", "job-uid"))
	unrelated := ownedRunningPod("other", "ns", "node-1", ownerRef("batch/v1", "Job", "x", "other-uid"))

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(job, owned, unrelated).Build()
	r := NewWorkloadResolver(c)

	pods, err := r.ResolvePods(context.Background(), "ns", criuorgv1.WorkloadReference{Kind: "Job", Name: "j"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := podNames(pods); len(got) != 1 || got[0] != "j-abc" {
		t.Fatalf("expected [j-abc], got %v", got)
	}
}

func TestResolvePodsSkipsNonRunning(t *testing.T) {
	s := resolverScheme(t)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns", UID: "job-uid"}}
	running := ownedRunningPod("j-run", "ns", "node-1", ownerRef("batch/v1", "Job", "j", "job-uid"))
	pending := ownedRunningPod("j-pend", "ns", "node-1", ownerRef("batch/v1", "Job", "j", "job-uid"))
	pending.Status.Phase = corev1.PodPending

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(job, running, pending).Build()
	r := NewWorkloadResolver(c)

	pods, err := r.ResolvePods(context.Background(), "ns", criuorgv1.WorkloadReference{Kind: "Job", Name: "j"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := podNames(pods); len(got) != 1 || got[0] != "j-run" {
		t.Fatalf("expected [j-run], got %v", got)
	}
}

func TestResolvePodsDeployment(t *testing.T) {
	s := resolverScheme(t)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns", UID: "dep-uid"}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "web-abc", Namespace: "ns", UID: "rs-uid",
		OwnerReferences: []metav1.OwnerReference{ownerRef("apps/v1", "Deployment", "web", "dep-uid")},
	}}
	p1 := ownedRunningPod("web-abc-1", "ns", "node-1", ownerRef("apps/v1", "ReplicaSet", "web-abc", "rs-uid"))
	p2 := ownedRunningPod("web-abc-2", "ns", "node-2", ownerRef("apps/v1", "ReplicaSet", "web-abc", "rs-uid"))
	orphan := ownedRunningPod("web-old", "ns", "node-1", ownerRef("apps/v1", "ReplicaSet", "web-xyz", "other-rs"))

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(dep, rs, p1, p2, orphan).Build()
	r := NewWorkloadResolver(c)

	pods, err := r.ResolvePods(context.Background(), "ns", criuorgv1.WorkloadReference{Kind: "Deployment", Name: "web"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := podNames(pods)
	if len(got) != 2 || got[0] != "web-abc-1" || got[1] != "web-abc-2" {
		t.Fatalf("expected [web-abc-1 web-abc-2], got %v", got)
	}
}

func TestResolvePodsStatefulSet(t *testing.T) {
	s := resolverScheme(t)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns", UID: "sts-uid"}}
	p0 := ownedRunningPod("db-0", "ns", "node-1", ownerRef("apps/v1", "StatefulSet", "db", "sts-uid"))
	p1 := ownedRunningPod("db-1", "ns", "node-2", ownerRef("apps/v1", "StatefulSet", "db", "sts-uid"))

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(sts, p0, p1).Build()
	r := NewWorkloadResolver(c)

	pods, err := r.ResolvePods(context.Background(), "ns", criuorgv1.WorkloadReference{Kind: "StatefulSet", Name: "db"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := podNames(pods); len(got) != 2 || got[0] != "db-0" || got[1] != "db-1" {
		t.Fatalf("expected [db-0 db-1], got %v", got)
	}
}

func TestResolvePodsUnknownKind(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(resolverScheme(t)).Build()
	r := NewWorkloadResolver(c)
	_, err := r.ResolvePods(context.Background(), "ns", criuorgv1.WorkloadReference{Kind: "DaemonSet", Name: "d"})
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestResolvePodsMissingWorkload(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(resolverScheme(t)).Build()
	r := NewWorkloadResolver(c)
	_, err := r.ResolvePods(context.Background(), "ns", criuorgv1.WorkloadReference{Kind: "Job", Name: "nope"})
	if err == nil {
		t.Fatal("expected error for missing job")
	}
}
