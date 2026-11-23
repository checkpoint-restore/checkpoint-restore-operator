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

// Package storeagent is the checkpoint-store agent that runs on every node:
// it uploads the node's checkpoint archives to object storage and downloads
// stored ones for the PodRestores that target the node.
package storeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	"github.com/checkpoint-restore/checkpoint-restore-operator/internal/checkpointarchive"
	"github.com/checkpoint-restore/checkpoint-restore-operator/internal/storageconfig"
	"github.com/checkpoint-restore/checkpoint-restore-operator/pkg/checkpointstore"
)

const (
	// ArchiveFormat is the manifest format of a snapshot holding one kubelet
	// checkpoint archive.
	ArchiveFormat = "kubelet-archive"
	// Producer is the producer recorded in the manifests the agent writes.
	Producer = "checkpoint-restore-operator"

	uploadedDir   = "uploaded"
	downloadedDir = "downloaded"

	// maxUploadBackoff caps how long an archive whose upload keeps failing
	// waits before the next attempt.
	maxUploadBackoff = time.Hour
)

// Agent is the per-node checkpoint-store agent.
type Agent struct {
	Client   client.Client
	Storage  *storageconfig.Provider
	NodeName string
	// CheckpointDir is where the kubelet writes checkpoint archives.
	CheckpointDir string
	// StateDir is a directory on the node, outside CheckpointDir, where the
	// agent records which archives it has uploaded and downloaded, so that
	// a restarted agent does not upload them again.
	StateDir string
	// AllowCrossNamespace lets a PodRestore restore a snapshot taken in
	// another namespace, matching the operator's flag of the same name.
	AllowCrossNamespace bool
	// Interval is how often the checkpoint directory is scanned.
	Interval time.Duration
	// Settle is how long an archive must go unmodified before it is
	// uploaded, so that one the kubelet is still writing is not.
	Settle time.Duration
	Log    logr.Logger

	// failures records the archives whose upload failed, so each is retried
	// with a growing delay rather than on every scan. Only the upload loop
	// uses it.
	failures map[string]uploadFailure
	// now is time.Now; tests replace it.
	now func() time.Time
}

// uploadFailure is the retry state of an archive whose upload failed.
type uploadFailure struct {
	attempts int
	retryAt  time.Time
}

func (a *Agent) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// refreshStorage applies the cluster's current storage configuration.
func (a *Agent) refreshStorage(ctx context.Context) error {
	spec, err := storageconfig.SpecFromCluster(ctx, a.Client)
	if err != nil {
		return fmt.Errorf("reading the storage configuration: %w", err)
	}
	if err := a.Storage.Set(ctx, spec); err != nil {
		return fmt.Errorf("opening checkpoint storage at %s: %w", spec.Location, err)
	}
	return nil
}

// downloadRecord is the state file of an archive the agent downloaded,
// naming the PodRestore it was downloaded for.
type downloadRecord struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// Start runs the upload loop until ctx ends. It implements manager.Runnable.
func (a *Agent) Start(ctx context.Context) error {
	for _, d := range []string{uploadedDir, downloadedDir} {
		if err := os.MkdirAll(filepath.Join(a.StateDir, d), 0o700); err != nil {
			return err
		}
	}
	ticker := time.NewTicker(a.Interval)
	defer ticker.Stop()
	for {
		a.sync(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// NeedLeaderElection reports that every node's agent runs.
func (a *Agent) NeedLeaderElection() bool { return false }

// sync refreshes the storage configuration, uploads new archives and removes
// downloaded ones whose PodRestore is gone.
func (a *Agent) sync(ctx context.Context) {
	if err := a.refreshStorage(ctx); err != nil {
		a.Log.Error(err, "refreshing the storage configuration")
		return
	}
	a.removeUnusedDownloads(ctx)
	store, _ := a.Storage.Get()
	if store == nil {
		return
	}
	entries, err := os.ReadDir(a.CheckpointDir)
	if err != nil {
		a.Log.Error(err, "reading the checkpoint directory", "dir", a.CheckpointDir)
		return
	}
	if a.failures == nil {
		a.failures = make(map[string]uploadFailure)
	}
	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		present[e.Name()] = true
	}
	for name := range a.failures {
		if !present[name] {
			delete(a.failures, name)
		}
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, "checkpoint-") || filepath.Ext(name) != ".tar" {
			continue
		}
		if a.recorded(uploadedDir, name) || a.recorded(downloadedDir, name+".json") {
			continue
		}
		info, err := e.Info()
		if err != nil || a.clock().Sub(info.ModTime()) < a.Settle {
			continue
		}
		failed, retrying := a.failures[name]
		if retrying && a.clock().Before(failed.retryAt) {
			continue
		}
		if err := a.upload(ctx, store, name); err != nil {
			failed.attempts++
			delay := min(a.Interval<<min(failed.attempts-1, 16), maxUploadBackoff)
			failed.retryAt = a.clock().Add(delay)
			a.failures[name] = failed
			a.Log.Error(err, "uploading checkpoint archive",
				"archive", name, "attempts", failed.attempts, "retry_in", delay.String())
			continue
		}
		delete(a.failures, name)
	}
}

func (a *Agent) recorded(dir, name string) bool {
	_, err := os.Stat(filepath.Join(a.StateDir, dir, name))
	return err == nil
}

// upload stores archive name of the checkpoint directory as a snapshot of
// the container the archive records, and marks it uploaded.
func (a *Agent) upload(ctx context.Context, store *checkpointstore.Store, name string) error {
	archive := filepath.Join(a.CheckpointDir, name)
	ref, err := checkpointarchive.ReadPodContainerRef(archive)
	if err != nil {
		return fmt.Errorf("reading the archive's pod and container: %w", err)
	}
	w, err := workloadOf(ctx, a.Client, ref.Namespace, ref.Pod)
	if err != nil {
		return fmt.Errorf("finding the workload of pod %s/%s: %w", ref.Namespace, ref.Pod, err)
	}
	image, err := checkpointarchive.ReadBaseImage(archive)
	if err != nil {
		// The image only fills a restore template that names none.
		a.Log.Info("archive records no base image", "archive", name, "error", err.Error())
	}
	m, err := store.Write(ctx, checkpointstore.Manifest{
		Key: checkpointstore.Key{
			Namespace:    ref.Namespace,
			WorkloadKind: w.kind,
			WorkloadName: w.name,
			Pod:          ref.Pod,
			Container:    ref.Container,
		},
		Format:     ArchiveFormat,
		Producer:   Producer,
		SourceNode: a.NodeName,
		PodUID:     w.podUID,
		Image:      image,
	}, a.CheckpointDir, []string{name})
	if err != nil {
		return err
	}
	a.Log.Info("uploaded checkpoint archive", "archive", name, "path", store.Path(m))
	return os.WriteFile(filepath.Join(a.StateDir, uploadedDir, name), []byte(store.Path(m)), 0o600)
}

// removeUnusedDownloads deletes the archives the agent downloaded for
// PodRestores that no longer exist, and the uploaded markers of archives
// that are gone.
func (a *Agent) removeUnusedDownloads(ctx context.Context) {
	dir := filepath.Join(a.StateDir, downloadedDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var rec downloadRecord
		if json.Unmarshal(data, &rec) != nil {
			continue
		}
		pr := &criuorgv1.PodRestore{}
		err = a.Client.Get(ctx, client.ObjectKey{Namespace: rec.Namespace, Name: rec.Name}, pr)
		if err == nil && string(pr.UID) == rec.UID {
			continue
		}
		if err != nil && !apierrors.IsNotFound(err) {
			continue
		}
		archive := filepath.Join(a.CheckpointDir, strings.TrimSuffix(e.Name(), ".json"))
		if err := os.Remove(archive); err != nil && !errors.Is(err, fs.ErrNotExist) {
			a.Log.Error(err, "removing a downloaded archive", "archive", archive)
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
		a.Log.Info("removed a downloaded archive of a deleted PodRestore", "archive", archive)
	}

	uploaded := filepath.Join(a.StateDir, uploadedDir)
	markers, err := os.ReadDir(uploaded)
	if err != nil {
		return
	}
	for _, e := range markers {
		if _, err := os.Stat(filepath.Join(a.CheckpointDir, e.Name())); errors.Is(err, fs.ErrNotExist) {
			_ = os.Remove(filepath.Join(uploaded, e.Name()))
		}
	}
}
