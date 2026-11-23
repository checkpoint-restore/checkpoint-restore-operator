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
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	"github.com/checkpoint-restore/checkpoint-restore-operator/internal/storageconfig"
	"github.com/checkpoint-restore/checkpoint-restore-operator/pkg/checkpointstore"
)

// StorageRetention keeps the operator's view of the checkpoint storage
// configuration current and deletes stored snapshots beyond its limits.
type StorageRetention struct {
	Client   client.Client
	Storage  *storageconfig.Provider
	Interval time.Duration
	Log      logr.Logger
	// now is time.Now; tests replace it.
	now func() time.Time
}

// +kubebuilder:rbac:groups=criu.org,resources=checkpointrestoreoperators,verbs=get;list;watch
// +kubebuilder:rbac:groups=criu.org,resources=podrestores,verbs=get;list;watch

// Start runs a retention pass every Interval until ctx ends. It implements
// manager.Runnable.
func (r *StorageRetention) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		if err := r.pass(ctx); err != nil {
			r.Log.Error(err, "checkpoint storage retention pass failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// NeedLeaderElection reports that only the leader deletes snapshots.
func (r *StorageRetention) NeedLeaderElection() bool { return true }

func (r *StorageRetention) pass(ctx context.Context) error {
	spec, err := storageconfig.SpecFromCluster(ctx, r.Client)
	if err != nil {
		return err
	}
	if err := r.Storage.Set(ctx, spec); err != nil {
		return err
	}
	store, spec := r.Storage.Get()
	if store == nil || (spec.MaxSnapshotsPerContainer == nil && spec.MaxSnapshotAge == nil) {
		return nil
	}

	// A snapshot a PodRestore names is kept while the PodRestore exists:
	// its agent may not have downloaded it yet.
	var restores criuorgv1.PodRestoreList
	if err := r.Client.List(ctx, &restores); err != nil {
		return err
	}
	inUse := make(map[string]bool)
	for i := range restores.Items {
		for _, cp := range restores.Items[i].Spec.Checkpoints {
			if cp.StoragePath != "" {
				inUse[cp.StoragePath] = true
			}
		}
	}

	keys, err := store.Keys(ctx)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		manifests, err := store.List(ctx, k)
		if err != nil {
			return err
		}
		for _, m := range expiredSnapshots(manifests, spec, r.clock()) {
			path := store.Path(m)
			if inUse[path] {
				continue
			}
			if err := store.Delete(ctx, path); err != nil {
				return err
			}
			r.Log.Info("deleted stored checkpoint beyond retention", "path", path)
		}
	}
	return nil
}

func (r *StorageRetention) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// expiredSnapshots returns the snapshots of one container, given oldest
// first, that spec's limits remove: all but the newest
// maxSnapshotsPerContainer, and those older than maxSnapshotAge except the
// newest.
func expiredSnapshots(
	manifests []checkpointstore.Manifest,
	spec *criuorgv1.CheckpointStorageSpec,
	now time.Time,
) []checkpointstore.Manifest {
	var expired []checkpointstore.Manifest
	for i, m := range manifests {
		newerCount := len(manifests) - 1 - i
		if newerCount == 0 {
			break
		}
		overCount := spec.MaxSnapshotsPerContainer != nil && newerCount >= int(*spec.MaxSnapshotsPerContainer)
		tooOld := spec.MaxSnapshotAge != nil && now.Sub(m.CreatedAt) > spec.MaxSnapshotAge.Duration
		if overCount || tooOld {
			expired = append(expired, m)
		}
	}
	return expired
}
