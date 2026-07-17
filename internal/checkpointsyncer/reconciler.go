package checkpointsyncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

// Finalizer holds a CheckpointArchive open until its object-storage copy is
// deleted, so a bucket object is never orphaned by a deleted record.
const Finalizer = "criu.org/checkpoint-syncer"

// Reconciler syncs the CheckpointArchive objects relevant to its own node
// between local disk and object storage.
type Reconciler struct {
	client.Client
	NodeName        string
	SecretNamespace string
	NewStore        func(Config) (ObjectStore, error)
}

// resolve reads the current config and builds a store from it. It returns the
// Config too, so callers can use cfg.Bucket for URI rendering without a fragile
// type assertion on the ObjectStore.
func (r *Reconciler) resolve(ctx context.Context) (ObjectStore, Config, error) {
	cfg, err := ResolveConfig(ctx, r.Client, r.SecretNamespace)
	if err != nil {
		return nil, Config{}, err
	}
	build := r.NewStore
	if build == nil {
		build = NewMinioStore
	}
	store, err := build(cfg)
	return store, cfg, err
}

func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	var archive criuorgv1.CheckpointArchive
	if err := r.Get(ctx, req.NamespacedName, &archive); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion: only the origin node's syncer owns object cleanup.
	if !archive.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &archive)
	}

	// Upload: this node created the archive and it isn't uploaded yet.
	if archive.Spec.Node == r.NodeName &&
		!meta.IsStatusConditionTrue(archive.Status.Conditions, criuorgv1.ConditionArchiveUploaded) {
		return r.reconcileUpload(ctx, &archive)
	}

	// Download: this node is requested and doesn't have a local copy yet.
	if slices.Contains(archive.Spec.RequestedNodes, r.NodeName) &&
		!slices.Contains(archive.Status.AvailableNodes, r.NodeName) &&
		meta.IsStatusConditionTrue(archive.Status.Conditions, criuorgv1.ConditionArchiveUploaded) {
		return r.reconcileDownload(ctx, &archive)
	}

	logger.V(1).Info("nothing to do", "archive", archive.Name, "node", r.NodeName)
	return reconcile.Result{}, nil
}

func (r *Reconciler) reconcileUpload(ctx context.Context, archive *criuorgv1.CheckpointArchive) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	store, cfg, err := r.resolve(ctx)
	if err != nil {
		if errors.Is(err, ErrExternalStorageDisabled) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	if _, statErr := os.Stat(archive.Spec.LocalPath); statErr != nil {
		// File not present on this node yet (GC race or not the origin). Requeue.
		return reconcile.Result{Requeue: true}, nil
	}

	// Ensure the finalizer before creating the remote object, so cleanup can
	// always run.
	if controllerutil.AddFinalizer(archive, Finalizer) {
		if err := r.Update(ctx, archive); err != nil {
			return reconcile.Result{}, err
		}
	}

	key := ObjectKey(archive.Spec.Namespace, archive.Spec.Pod, archive.Spec.Container, archive.Spec.LocalPath)
	if err := store.Put(ctx, key, archive.Spec.LocalPath); err != nil {
		meta.SetStatusCondition(&archive.Status.Conditions, metav1.Condition{
			Type: criuorgv1.ConditionArchiveUploaded, Status: metav1.ConditionFalse,
			Reason: "UploadFailed", Message: err.Error(),
		})
		_ = r.Status().Update(ctx, archive)
		return reconcile.Result{}, err
	}

	archive.Status.ExternalURI = ExternalURI(cfg.Bucket, key)
	meta.SetStatusCondition(&archive.Status.Conditions, metav1.Condition{
		Type: criuorgv1.ConditionArchiveUploaded, Status: metav1.ConditionTrue, Reason: "Uploaded",
	})
	if !slices.Contains(archive.Status.AvailableNodes, r.NodeName) {
		archive.Status.AvailableNodes = append(archive.Status.AvailableNodes, r.NodeName)
	}
	if err := r.Status().Update(ctx, archive); err != nil {
		return reconcile.Result{}, err
	}
	logger.Info("uploaded checkpoint archive", "archive", archive.Name, "uri", archive.Status.ExternalURI)
	return reconcile.Result{}, nil
}

func (r *Reconciler) reconcileDownload(ctx context.Context, archive *criuorgv1.CheckpointArchive) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	store, _, err := r.resolve(ctx)
	if err != nil {
		if errors.Is(err, ErrExternalStorageDisabled) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	_, key, err := ParseS3URI(archive.Status.ExternalURI)
	if err != nil {
		return reconcile.Result{}, err
	}
	// Download to a temp file in the same dir, then rename for atomicity.
	dir := filepath.Dir(archive.Spec.LocalPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return reconcile.Result{}, err
	}
	tmp := archive.Spec.LocalPath + ".syncer.tmp"
	if err := store.Get(ctx, key, tmp); err != nil {
		return reconcile.Result{}, err
	}
	if err := os.Rename(tmp, archive.Spec.LocalPath); err != nil {
		_ = os.Remove(tmp)
		return reconcile.Result{}, err
	}

	if !slices.Contains(archive.Status.AvailableNodes, r.NodeName) {
		archive.Status.AvailableNodes = append(archive.Status.AvailableNodes, r.NodeName)
	}
	if err := r.Status().Update(ctx, archive); err != nil {
		return reconcile.Result{}, err
	}
	logger.Info("staged checkpoint archive locally", "archive", archive.Name, "node", r.NodeName)
	return reconcile.Result{}, nil
}

func (r *Reconciler) reconcileDelete(ctx context.Context, archive *criuorgv1.CheckpointArchive) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(archive, Finalizer) {
		return reconcile.Result{}, nil
	}
	// Only the origin node deletes the object.
	if archive.Spec.Node == r.NodeName && archive.Status.ExternalURI != "" {
		store, _, err := r.resolve(ctx)
		if err != nil && !errors.Is(err, ErrExternalStorageDisabled) {
			return reconcile.Result{}, err
		}
		if store != nil {
			_, key, err := ParseS3URI(archive.Status.ExternalURI)
			if err != nil {
				return reconcile.Result{}, err
			}
			if err := store.Delete(ctx, key); err != nil {
				return reconcile.Result{}, err // retry; finalizer stays
			}
		}
	} else if archive.Spec.Node != r.NodeName {
		// Non-origin nodes must not remove the finalizer; leave for the origin.
		return reconcile.Result{}, nil
	}

	controllerutil.RemoveFinalizer(archive, Finalizer)
	if err := r.Update(ctx, archive); err != nil {
		return reconcile.Result{}, err
	}
	logger.Info("deleted external object and released finalizer", "archive", archive.Name)
	return reconcile.Result{}, nil
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&criuorgv1.CheckpointArchive{}).
		Named("checkpoint-syncer").
		Complete(r)
}
