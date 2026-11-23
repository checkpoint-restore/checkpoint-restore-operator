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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	"github.com/checkpoint-restore/checkpoint-restore-operator/pkg/checkpointstore"
)

// downloadRetry is how soon a failed or unconfigured download is retried.
const downloadRetry = 30 * time.Second

// Reconcile downloads the stored checkpoints of a PodRestore that targets
// this node into the checkpoint directory, and reports the outcome in its
// CheckpointsDownloaded condition, which gates the restore Pod.
func (a *Agent) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pr := &criuorgv1.PodRestore{}
	if err := a.Client.Get(ctx, req.NamespacedName, pr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if pr.Spec.TargetNode != a.NodeName || !pr.DeletionTimestamp.IsZero() || !hasStoredCheckpoints(pr) {
		return ctrl.Result{}, nil
	}
	if c := meta.FindStatusCondition(pr.Status.Conditions, criuorgv1.ConditionCheckpointsDownloaded); c != nil &&
		c.Status == metav1.ConditionTrue && c.ObservedGeneration == pr.Generation {
		return ctrl.Result{}, nil
	}

	// The upload loop refreshes the configuration too, but a PodRestore
	// may arrive before its first scan or right after a change.
	if err := a.refreshStorage(ctx); err != nil {
		return ctrl.Result{RequeueAfter: downloadRetry},
			a.setDownloaded(ctx, pr, metav1.ConditionFalse, "StorageUnavailable", err.Error())
	}
	store, _ := a.Storage.Get()
	if store == nil {
		return ctrl.Result{RequeueAfter: downloadRetry},
			a.setDownloaded(ctx, pr, metav1.ConditionFalse, "StorageNotConfigured",
				"no CheckpointRestoreOperator configures checkpoint storage")
	}
	for _, cp := range pr.Spec.Checkpoints {
		if cp.StoragePath == "" {
			continue
		}
		if err := a.download(ctx, store, pr, cp); err != nil {
			a.Log.Error(err, "downloading a stored checkpoint",
				"podrestore", req.NamespacedName, "container", cp.Container, "path", cp.StoragePath)
			return ctrl.Result{RequeueAfter: downloadRetry},
				a.setDownloaded(ctx, pr, metav1.ConditionFalse, "DownloadFailed",
					fmt.Sprintf("container %q: %v", cp.Container, err))
		}
	}
	return ctrl.Result{}, a.setDownloaded(ctx, pr, metav1.ConditionTrue, "Downloaded",
		"every stored checkpoint is on the target node")
}

func hasStoredCheckpoints(pr *criuorgv1.PodRestore) bool {
	for _, cp := range pr.Spec.Checkpoints {
		if cp.StoragePath != "" {
			return true
		}
	}
	return false
}

// download fetches the snapshot of cp into the checkpoint directory under
// the name the controller annotates the restore Pod with, unless it is
// already there.
func (a *Agent) download(
	ctx context.Context,
	store *checkpointstore.Store,
	pr *criuorgv1.PodRestore,
	cp criuorgv1.ContainerCheckpoint,
) error {
	target := criuorgv1.StoredCheckpointArchivePath(a.CheckpointDir, pr, cp.Container, cp.StoragePath)
	if _, err := os.Stat(target); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Only the namespace is checked here; the CRI proxy checks the archive
	// itself against the restore Pod's sandbox.
	m, err := store.Read(ctx, cp.StoragePath)
	if err != nil {
		return err
	}
	if m.Format != ArchiveFormat || len(m.Files) != 1 {
		return fmt.Errorf("snapshot %s is %s with %d files, not one %s", cp.StoragePath, m.Format, len(m.Files), ArchiveFormat)
	}
	if !a.AllowCrossNamespace && m.Key.Namespace != pr.Namespace {
		return fmt.Errorf("snapshot %s was taken in namespace %q, not %q", cp.StoragePath, m.Key.Namespace, pr.Namespace)
	}

	// Download next to the target, so the rename into place cannot cross a
	// file system and a partial archive is never visible under its name.
	tmp, err := os.MkdirTemp(a.CheckpointDir, ".store-download-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if _, _, err := store.Fetch(ctx, cp.StoragePath, tmp); err != nil {
		return err
	}
	// Record the download before the archive appears, so the uploader never
	// mistakes it for a new checkpoint.
	rec, err := json.Marshal(downloadRecord{Namespace: pr.Namespace, Name: pr.Name, UID: string(pr.UID)})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(a.StateDir, downloadedDir, filepath.Base(target)+".json"), rec, 0o600); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(tmp, filepath.FromSlash(m.Files[0].Path)), target); err != nil {
		return err
	}
	a.Log.Info("downloaded stored checkpoint", "path", cp.StoragePath, "archive", target)
	return nil
}

// setDownloaded writes the CheckpointsDownloaded condition. The patch carries
// the resource version, so it fails rather than overwrite conditions the
// controller wrote meanwhile; the reconcile is then retried.
func (a *Agent) setDownloaded(
	ctx context.Context,
	pr *criuorgv1.PodRestore,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	patch := client.MergeFromWithOptions(pr.DeepCopy(), client.MergeFromWithOptimisticLock{})
	changed := meta.SetStatusCondition(&pr.Status.Conditions, metav1.Condition{
		Type:               criuorgv1.ConditionCheckpointsDownloaded,
		Status:             status,
		ObservedGeneration: pr.Generation,
		Reason:             reason,
		Message:            message,
	})
	if !changed {
		return nil
	}
	return a.Client.Status().Patch(ctx, pr, patch)
}

// SetupWithManager registers the download reconciler for the PodRestores
// that target this node.
func (a *Agent) SetupWithManager(mgr ctrl.Manager) error {
	onThisNode := predicate.NewPredicateFuncs(func(o client.Object) bool {
		pr, ok := o.(*criuorgv1.PodRestore)
		return ok && pr.Spec.TargetNode == a.NodeName
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("checkpoint-store-agent").
		For(&criuorgv1.PodRestore{}, builder.WithPredicates(onThisNode)).
		Complete(a)
}
