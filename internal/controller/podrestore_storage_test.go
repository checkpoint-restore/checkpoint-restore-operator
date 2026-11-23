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
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	kptr "k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	"github.com/checkpoint-restore/checkpoint-restore-operator/internal/storageconfig"
	"github.com/checkpoint-restore/checkpoint-restore-operator/pkg/checkpointstore"
)

var _ = Describe("PodRestoreReconciler with a stored checkpoint", func() {
	const (
		name        = "db-restore"
		ns          = "data"
		storagePath = "root/data/StatefulSet/db/db-0/main/20260101T000000.000000000Z-0a0b0c0d"
	)

	BeforeEach(func() {
		Expect(criuorgv1.AddToScheme(scheme.Scheme)).To(Succeed())
	})

	newPodRestore := func(image string) *criuorgv1.PodRestore {
		return &criuorgv1.PodRestore{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Generation: 1},
			Spec: criuorgv1.PodRestoreSpec{
				TargetNode:  "worker-2",
				Checkpoints: []criuorgv1.ContainerCheckpoint{{Container: "main", StoragePath: storagePath}},
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: image}},
				}},
			},
		}
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}

	makeReconciler := func(storage *storageconfig.Provider, objs ...client.Object) *PodRestoreReconciler {
		c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).
			WithStatusSubresource(&criuorgv1.PodRestore{}).Build()
		return &PodRestoreReconciler{Client: c, Scheme: scheme.Scheme, Storage: storage}
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-2"}}

	reconcile := func(r *PodRestoreReconciler, n int) {
		for range n {
			_, err := r.Reconcile(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())
		}
	}
	get := func(r *PodRestoreReconciler) *criuorgv1.PodRestore {
		pr := &criuorgv1.PodRestore{}
		Expect(r.Get(context.Background(), req.NamespacedName, pr)).To(Succeed())
		return pr
	}
	// markDownloaded writes the condition the agent on the target node sets.
	markDownloaded := func(r *PodRestoreReconciler) {
		pr := get(r)
		meta.SetStatusCondition(&pr.Status.Conditions, metav1.Condition{
			Type: criuorgv1.ConditionCheckpointsDownloaded, Status: metav1.ConditionTrue,
			ObservedGeneration: pr.Generation, Reason: "Downloaded",
		})
		Expect(r.Status().Update(context.Background(), pr)).To(Succeed())
	}

	It("creates no restore Pod until the agent reports the checkpoint downloaded", func() {
		r := makeReconciler(nil, newPodRestore("db:1"), node.DeepCopy())
		reconcile(r, 2)

		err := r.Get(context.Background(), req.NamespacedName, &corev1.Pod{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "restore Pod created before the download")
		c := meta.FindStatusCondition(get(r).Status.Conditions, criuorgv1.ConditionReady)
		Expect(c.Reason).To(Equal("Downloading"))

		markDownloaded(r)
		reconcile(r, 1)

		pod := &corev1.Pod{}
		Expect(r.Get(context.Background(), req.NamespacedName, pod)).To(Succeed())
		Expect(pod.Spec.NodeName).To(Equal("worker-2"))
		Expect(pod.Annotations).To(HaveKeyWithValue(
			criuorgv1.RestoreCheckpointPathAnnotationPrefix+"main",
			criuorgv1.StoredCheckpointArchivePath("", get(r), "main", storagePath)))
	})

	It("takes the image of a template that names none from the snapshot's manifest", func() {
		bucket := checkpointstore.NewMemoryBucket()
		store := checkpointstore.New(bucket, "root")
		src := GinkgoT().TempDir()
		Expect(os.WriteFile(src+"/a.tar", []byte("x"), 0o600)).To(Succeed())
		m, err := store.Write(context.Background(), checkpointstore.Manifest{
			Key:   checkpointstore.Key{Namespace: ns, WorkloadKind: "StatefulSet", WorkloadName: "db", Pod: "db-0", Container: "main"},
			Image: "registry/db:7",
		}, src, []string{"a.tar"})
		Expect(err).NotTo(HaveOccurred())
		provider := storageconfig.NewProvider(func(context.Context, string) (*checkpointstore.Store, error) {
			return store, nil
		})
		Expect(provider.Set(context.Background(), &criuorgv1.CheckpointStorageSpec{Location: "s3://bucket/root"})).To(Succeed())

		pr := newPodRestore("")
		pr.Spec.Checkpoints[0].StoragePath = store.Path(m)
		r := makeReconciler(provider, pr, node.DeepCopy())
		reconcile(r, 1)
		markDownloaded(r)
		reconcile(r, 1)

		pod := &corev1.Pod{}
		Expect(r.Get(context.Background(), req.NamespacedName, pod)).To(Succeed())
		Expect(pod.Spec.Containers[0].Image).To(Equal("registry/db:7"))
	})
})

func TestExpiredSnapshots(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	snap := func(id string, age time.Duration) checkpointstore.Manifest {
		return checkpointstore.Manifest{ID: id, CreatedAt: now.Add(-age)}
	}
	oldestFirst := []checkpointstore.Manifest{
		snap("a", 72*time.Hour), snap("b", 48*time.Hour), snap("c", 24*time.Hour), snap("d", time.Hour),
	}
	for name, tc := range map[string]struct {
		spec criuorgv1.CheckpointStorageSpec
		want []string
	}{
		"count keeps the newest": {
			spec: criuorgv1.CheckpointStorageSpec{MaxSnapshotsPerContainer: kptr.To[int32](2)},
			want: []string{"a", "b"},
		},
		"age removes the old": {
			spec: criuorgv1.CheckpointStorageSpec{MaxSnapshotAge: &metav1.Duration{Duration: 30 * time.Hour}},
			want: []string{"a", "b"},
		},
		"age never removes the newest": {
			spec: criuorgv1.CheckpointStorageSpec{MaxSnapshotAge: &metav1.Duration{Duration: time.Minute}},
			want: []string{"a", "b", "c"},
		},
		"either limit removes": {
			spec: criuorgv1.CheckpointStorageSpec{
				MaxSnapshotsPerContainer: kptr.To[int32](3),
				MaxSnapshotAge:           &metav1.Duration{Duration: 30 * time.Hour},
			},
			want: []string{"a", "b"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var got []string
			for _, m := range expiredSnapshots(oldestFirst, &tc.spec, now) {
				got = append(got, m.ID)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("expired %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("expired %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestRetentionKeepsASnapshotAPodRestoreNames(t *testing.T) {
	ctx := context.Background()
	Expect := NewWithT(t).Expect
	Expect(criuorgv1.AddToScheme(scheme.Scheme)).To(Succeed())
	store := checkpointstore.New(checkpointstore.NewMemoryBucket(), "root")
	key := checkpointstore.Key{Namespace: "data", WorkloadKind: "StatefulSet", WorkloadName: "db", Pod: "db-0", Container: "main"}
	var paths []string
	for range 3 {
		src := t.TempDir()
		Expect(os.WriteFile(src+"/a.tar", []byte("x"), 0o600)).To(Succeed())
		m, err := store.Write(ctx, checkpointstore.Manifest{Key: key}, src, []string{"a.tar"})
		Expect(err).NotTo(HaveOccurred())
		paths = append(paths, store.Path(m))
	}
	operator := &criuorgv1.CheckpointRestoreOperator{
		ObjectMeta: metav1.ObjectMeta{Name: "config", Namespace: "system"},
		Spec: criuorgv1.CheckpointRestoreOperatorSpec{Storage: &criuorgv1.CheckpointStorageSpec{
			Location: "s3://bucket/root", MaxSnapshotsPerContainer: kptr.To[int32](1),
		}},
	}
	restore := &criuorgv1.PodRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "data"},
		Spec: criuorgv1.PodRestoreSpec{Checkpoints: []criuorgv1.ContainerCheckpoint{
			{Container: "main", StoragePath: paths[0]},
		}},
	}
	r := &StorageRetention{
		Client: fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(operator, restore).Build(),
		Storage: storageconfig.NewProvider(func(context.Context, string) (*checkpointstore.Store, error) {
			return store, nil
		}),
	}

	Expect(r.pass(ctx)).To(Succeed())

	left, err := store.List(ctx, key)
	Expect(err).NotTo(HaveOccurred())
	var ids []string
	for _, m := range left {
		ids = append(ids, store.Path(m))
	}
	Expect(ids).To(Equal([]string{paths[0], paths[2]}), "kept the named and the newest snapshot only")
}
