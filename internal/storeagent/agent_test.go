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
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	"github.com/checkpoint-restore/checkpoint-restore-operator/internal/storageconfig"
	"github.com/checkpoint-restore/checkpoint-restore-operator/pkg/checkpointstore"
)

const testLocation = "s3://bucket/root"

// writeArchive writes a kubelet-style checkpoint archive for container of
// pod in namespace into dir.
func writeArchive(t *testing.T, dir, namespace, pod, container string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, data := range map[string]string{
		"spec.dump": `{"annotations":{"io.kubernetes.cri.sandbox-namespace":"` + namespace +
			`","io.kubernetes.cri.sandbox-name":"` + pod + `","io.kubernetes.cri.container-name":"` + container + `"}}`,
		"config.dump": `{"rootfsImageName":"registry/app:1"}`,
		"pages-1.img": "memory",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	name := "checkpoint-" + pod + "_" + namespace + "-" + container + "-2026-01-01T00:00:00Z.tar"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return path
}

type testEnv struct {
	agent  *Agent
	bucket *checkpointstore.MemoryBucket
	store  *checkpointstore.Store
	client client.Client
}

func newTestEnv(t *testing.T, objs ...client.Object) *testEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := criuorgv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	operator := &criuorgv1.CheckpointRestoreOperator{
		ObjectMeta: metav1.ObjectMeta{Name: "config", Namespace: "system"},
		Spec: criuorgv1.CheckpointRestoreOperatorSpec{
			Storage: &criuorgv1.CheckpointStorageSpec{Location: testLocation},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(append(objs, operator)...).
		WithStatusSubresource(&criuorgv1.PodRestore{}).Build()
	bucket := checkpointstore.NewMemoryBucket()
	store := checkpointstore.New(bucket, "root")
	provider := storageconfig.NewProvider(func(_ context.Context, loc string) (*checkpointstore.Store, error) {
		if loc != testLocation {
			t.Fatalf("opened %q", loc)
		}
		return store, nil
	})
	a := &Agent{
		Client:        c,
		Storage:       provider,
		NodeName:      "worker-1",
		CheckpointDir: t.TempDir(),
		StateDir:      t.TempDir(),
		Interval:      time.Hour,
		Settle:        time.Second,
		Log:           logr.Discard(),
	}
	for _, d := range []string{uploadedDir, downloadedDir} {
		if err := os.MkdirAll(filepath.Join(a.StateDir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return &testEnv{agent: a, bucket: bucket, store: store, client: c}
}

func TestSyncUploadsEachArchiveOnceUnderItsDeployment(t *testing.T) {
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "web-5d8", Namespace: "shop",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "web", UID: "d", Controller: ptr.To(true)}},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "web-5d8-x", Namespace: "shop", UID: "pod-uid",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web-5d8", UID: "r", Controller: ptr.To(true)}},
	}}
	env := newTestEnv(t, rs, pod)
	writeArchive(t, env.agent.CheckpointDir, "shop", "web-5d8-x", "app")

	env.agent.sync(context.Background())
	env.agent.sync(context.Background())

	key := checkpointstore.Key{Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web", Pod: "web-5d8-x", Container: "app"}
	manifests, err := env.store.List(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 {
		t.Fatalf("stored %d snapshots of the archive, want 1", len(manifests))
	}
	m := manifests[0]
	if m.Format != ArchiveFormat || m.Image != "registry/app:1" || m.SourceNode != "worker-1" || m.PodUID != "pod-uid" {
		t.Fatalf("manifest = %+v", m)
	}
}

// rejectingBucket refuses every upload and counts the attempts.
type rejectingBucket struct {
	*checkpointstore.MemoryBucket
	puts *int
}

func (b rejectingBucket) Put(context.Context, string, io.Reader, int64) error {
	*b.puts++
	return errors.New("access denied")
}

func TestSyncRetriesAFailedUploadOnlyAfterABackoff(t *testing.T) {
	env := newTestEnv(t)
	puts := 0
	store := checkpointstore.New(rejectingBucket{MemoryBucket: env.bucket, puts: &puts}, "root")
	env.agent.Storage = storageconfig.NewProvider(func(context.Context, string) (*checkpointstore.Store, error) {
		return store, nil
	})
	now := time.Now()
	env.agent.now = func() time.Time { return now }
	env.agent.Interval = 15 * time.Second
	writeArchive(t, env.agent.CheckpointDir, "shop", "solo", "app")

	env.agent.sync(context.Background())
	if puts != 1 {
		t.Fatalf("first scan made %d upload attempts, want 1", puts)
	}
	env.agent.sync(context.Background())
	if puts != 1 {
		t.Fatalf("the next scan retried at once: %d attempts", puts)
	}
	now = now.Add(15 * time.Second)
	env.agent.sync(context.Background())
	if puts != 2 {
		t.Fatalf("no retry after the backoff: %d attempts", puts)
	}
	// The second failure doubles the delay.
	now = now.Add(15 * time.Second)
	env.agent.sync(context.Background())
	if puts != 2 {
		t.Fatalf("retried before the doubled backoff: %d attempts", puts)
	}
}

func TestSyncDoesNotUploadAnArchiveStillBeingWritten(t *testing.T) {
	env := newTestEnv(t)
	path := writeArchive(t, env.agent.CheckpointDir, "shop", "solo", "app")
	if err := os.Chtimes(path, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}

	env.agent.sync(context.Background())

	if keys := env.bucket.Keys(); len(keys) != 0 {
		t.Fatalf("uploaded %v while the archive was still changing", keys)
	}
}

// storeSnapshot stores an archive taken in namespace and returns its path.
func storeSnapshot(t *testing.T, env *testEnv, namespace string) string {
	t.Helper()
	src := t.TempDir()
	archive := writeArchive(t, src, namespace, "db-0", "main")
	m, err := env.store.Write(context.Background(), checkpointstore.Manifest{
		Key:    checkpointstore.Key{Namespace: namespace, WorkloadKind: "StatefulSet", WorkloadName: "db", Pod: "db-0", Container: "main"},
		Format: ArchiveFormat,
	}, src, []string{filepath.Base(archive)})
	if err != nil {
		t.Fatal(err)
	}
	return env.store.Path(m)
}

func storedRestore(storagePath string) *criuorgv1.PodRestore {
	return &criuorgv1.PodRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "db-restore", Namespace: "data", UID: "pr-uid", Generation: 1},
		Spec: criuorgv1.PodRestoreSpec{
			TargetNode:  "worker-1",
			Checkpoints: []criuorgv1.ContainerCheckpoint{{Container: "main", StoragePath: storagePath}},
		},
	}
}

func downloaded(t *testing.T, env *testEnv) *metav1.Condition {
	t.Helper()
	pr := &criuorgv1.PodRestore{}
	if err := env.client.Get(context.Background(), types.NamespacedName{Namespace: "data", Name: "db-restore"}, pr); err != nil {
		t.Fatal(err)
	}
	return meta.FindStatusCondition(pr.Status.Conditions, criuorgv1.ConditionCheckpointsDownloaded)
}

// The PodRestore is reconciled before the upload loop has ever read the
// storage configuration, as on an agent that has just started.
func TestReconcileDownloadsAStoredCheckpointWhereTheControllerLooksForIt(t *testing.T) {
	env := newTestEnv(t)
	path := storeSnapshot(t, env, "data")
	pr := storedRestore(path)
	if err := env.client.Create(context.Background(), pr); err != nil {
		t.Fatal(err)
	}

	if _, err := env.agent.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pr)}); err != nil {
		t.Fatal(err)
	}

	c := downloaded(t, env)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("CheckpointsDownloaded = %+v, want True", c)
	}
	target := criuorgv1.StoredCheckpointArchivePath(env.agent.CheckpointDir, pr, "main", path)
	if err := criuorgv1.ValidateCheckpointNamespaceHint(target, "data"); err != nil {
		t.Fatalf("downloaded archive fails the controller's namespace check: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("archive not at %s: %v", target, err)
	}
	// The downloaded archive is not uploaded again as a new checkpoint.
	before := len(env.bucket.Keys())
	env.agent.sync(context.Background())
	if after := len(env.bucket.Keys()); after != before {
		t.Fatalf("the downloaded archive was uploaded: %d objects, then %d", before, after)
	}
}

func TestReconcileRefusesASnapshotFromAnotherNamespace(t *testing.T) {
	env := newTestEnv(t)
	pr := storedRestore(storeSnapshot(t, env, "other-team"))
	if err := env.client.Create(context.Background(), pr); err != nil {
		t.Fatal(err)
	}
	env.agent.sync(context.Background())

	if _, err := env.agent.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pr)}); err != nil {
		t.Fatal(err)
	}

	c := downloaded(t, env)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "DownloadFailed" {
		t.Fatalf("CheckpointsDownloaded = %+v, want False/DownloadFailed", c)
	}
	entries, _ := os.ReadDir(env.agent.CheckpointDir)
	if len(entries) != 0 {
		t.Fatalf("checkpoint directory holds %v after a refused download", entries)
	}
}

func TestSyncRemovesTheArchiveOfADeletedPodRestore(t *testing.T) {
	env := newTestEnv(t)
	path := storeSnapshot(t, env, "data")
	pr := storedRestore(path)
	if err := env.client.Create(context.Background(), pr); err != nil {
		t.Fatal(err)
	}
	env.agent.sync(context.Background())
	if _, err := env.agent.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pr)}); err != nil {
		t.Fatal(err)
	}
	target := criuorgv1.StoredCheckpointArchivePath(env.agent.CheckpointDir, pr, "main", path)

	if err := env.client.Delete(context.Background(), pr); err != nil {
		t.Fatal(err)
	}
	env.agent.sync(context.Background())

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("archive of the deleted PodRestore remains: %v", err)
	}
}
