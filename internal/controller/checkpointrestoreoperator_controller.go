/*
Copyright 2024.

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
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"

	metadata "github.com/checkpoint-restore/checkpointctl/lib"
	"github.com/containers/storage/pkg/archive"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	kubelettypes "k8s.io/kubelet/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/fsnotify/fsnotify"
)

type DeletionPolicy int

const (
	ByCount DeletionPolicy = iota
	BySize
)

// Constants for unit conversion
const (
	KB = 1024
	MB = 1024 * KB
)

var (
	GarbageCollector           garbageCollector
	checkpointDirectory        string = "/var/lib/kubelet/checkpoints/"
	quit                       chan bool
	maxCheckpointsPerContainer int   = 10
	maxCheckpointsPerPod       int   = 10
	maxCheckpointsPerNamespace int   = 10
	maxCheckpointSize          int64 = 10 * MB
	maxTotalSizePerNamespace   int64 = 40 * MB
	maxTotalSizePerPod         int64
	maxTotalSizePerContainer   int64
)

type garbageCollector struct {
	sync.Mutex
}

// CheckpointRestoreOperatorReconciler reconciles a CheckpointRestoreOperator object
type CheckpointRestoreOperatorReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=criu.org,resources=checkpointrestoreoperators,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=criu.org,resources=checkpointrestoreoperators/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=criu.org,resources=checkpointrestoreoperators/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.15.0/pkg/reconcile
func (r *CheckpointRestoreOperatorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var input criuorgv1.CheckpointRestoreOperator

	log := log.FromContext(ctx)

	if err := r.Get(ctx, req.NamespacedName, &input); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if input.Spec.GlobalPolicies.MaxCheckpointsPerContainer != nil &&
		*input.Spec.GlobalPolicies.MaxCheckpointsPerContainer >= 0 &&
		*input.Spec.GlobalPolicies.MaxCheckpointsPerContainer != maxCheckpointsPerContainer {
		maxCheckpointsPerContainer = *input.Spec.GlobalPolicies.MaxCheckpointsPerContainer
		log.Info("Changed MaxCheckpointsPerContainer", "maxCheckpointsPerContainer", maxCheckpointsPerContainer)
	}

	// New code for MaxCheckpointsPerPod and MaxCheckpointsPerNamespace
	if input.Spec.GlobalPolicies.MaxCheckpointsPerPod != nil &&
		*input.Spec.GlobalPolicies.MaxCheckpointsPerPod >= 0 {
		maxCheckpointsPerPod = *input.Spec.GlobalPolicies.MaxCheckpointsPerPod
		log.Info("Changed MaxCheckpointsPerPod", "maxCheckpointsPerPod", maxCheckpointsPerPod)
	}

	if input.Spec.GlobalPolicies.MaxCheckpointsPerNamespace != nil &&
		*input.Spec.GlobalPolicies.MaxCheckpointsPerNamespace >= 0 {
		maxCheckpointsPerNamespace = *input.Spec.GlobalPolicies.MaxCheckpointsPerNamespace
		log.Info("Changed MaxCheckpointsPerNamespace", "maxCheckpointsPerNamespace", maxCheckpointsPerNamespace)
	}

	if input.Spec.GlobalPolicies.MaxCheckpointSize != nil && *input.Spec.GlobalPolicies.MaxCheckpointSize >= 0 {
		maxCheckpointSize = *input.Spec.GlobalPolicies.MaxCheckpointSize * MB
		log.Info("Changed MaxCheckpointSize", "maxCheckpointSize", maxCheckpointSize)
	}

	if input.Spec.GlobalPolicies.MaxTotalSizePerNamespace != nil && *input.Spec.GlobalPolicies.MaxTotalSizePerNamespace >= 0 {
		maxTotalSizePerNamespace = *input.Spec.GlobalPolicies.MaxTotalSizePerNamespace * MB
		log.Info("Changed MaxTotalSizePerNamespace", "maxTotalSizePerNamespace", maxTotalSizePerNamespace)
	}

	if input.Spec.GlobalPolicies.MaxTotalSizePerPod != nil && *input.Spec.GlobalPolicies.MaxTotalSizePerPod >= 0 {
		maxTotalSizePerPod = *input.Spec.GlobalPolicies.MaxTotalSizePerPod * MB
		log.Info("Changed MaxTotalSizePerPod", "maxTotalSizePerPod", maxTotalSizePerPod)
	}

	if input.Spec.GlobalPolicies.MaxTotalSizePerContainer != nil && *input.Spec.GlobalPolicies.MaxTotalSizePerContainer >= 0 {
		maxTotalSizePerContainer = *input.Spec.GlobalPolicies.MaxTotalSizePerContainer * MB
		log.Info("Changed MaxTotalSizePerContainer", "maxTotalSizePerContainer", maxTotalSizePerContainer)
	}

	if input.Spec.CheckpointDirectory != "" && input.Spec.CheckpointDirectory != checkpointDirectory {
		checkpointDirectory = input.Spec.CheckpointDirectory
		quit <- true
		quit = make(chan bool)
		go GarbageCollector.runGarbageCollector()
	}

	return ctrl.Result{}, nil
}

// UntarFiles unpack only specified files from an archive to the destination directory.
// Copied from checkpointctl
func UntarFiles(src, dest string, files []string) error {
	archiveFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer archiveFile.Close()

	if err := iterateTarArchive(src, func(r *tar.Reader, header *tar.Header) error {
		// Check if the current entry is one of the target files
		for _, file := range files {
			if strings.Contains(header.Name, file) {
				// Create the destination folder
				if err := os.MkdirAll(filepath.Join(dest, filepath.Dir(header.Name)), 0o644); err != nil {
					return err
				}
				// Create the destination file
				destFile, err := os.Create(filepath.Join(dest, header.Name))
				if err != nil {
					return err
				}
				defer destFile.Close()

				// Copy the contents of the entry to the destination file
				_, err = io.Copy(destFile, r)
				if err != nil {
					return err
				}

				// File successfully extracted, move to the next file
				break
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("unpacking of checkpoint archive failed: %w", err)
	}

	return nil
}

// iterateTarArchive reads a tar archive from the specified input file,
// decompresses it, and iterates through each entry, invoking the provided callback function.
// Copied from checkpointctl
func iterateTarArchive(archiveInput string, callback func(r *tar.Reader, header *tar.Header) error) error {
	archiveFile, err := os.Open(archiveInput)
	if err != nil {
		return err
	}
	defer archiveFile.Close()

	// Decompress the archive
	stream, err := archive.DecompressStream(archiveFile)
	if err != nil {
		return err
	}
	defer stream.Close()

	// Create a tar reader to read the files from the decompressed archive
	tarReader := tar.NewReader(stream)

	for {
		header, err := tarReader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}

		if err = callback(tarReader, header); err != nil {
			return err
		}
	}

	return nil
}

type checkpointDetails struct {
	namespace string
	pod       string
	container string
}

func getCheckpointArchiveInformation(log logr.Logger, checkpointPath string) (*checkpointDetails, error) {
	tempDir, err := os.MkdirTemp("", "get-spec-dump")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)

	filesToExtract := []string{"spec.dump"}
	if err := UntarFiles(checkpointPath, tempDir, filesToExtract); err != nil {
		log.Info("Error extracting files from archive: ", "checkpointPath", checkpointPath, "Error", err)
		return nil, err
	}

	dumpSpec, _, err := metadata.ReadContainerCheckpointSpecDump(tempDir)
	if err != nil {
		return nil, err
	}
	labels := make(map[string]string)

	if err := json.Unmarshal([]byte(dumpSpec.Annotations["io.kubernetes.cri-o.Labels"]), &labels); err != nil {
		return nil, fmt.Errorf("failed to read %q: %w", "io.kubernetes.cri-o.Labels", err)
	}

	details := &checkpointDetails{
		namespace: labels[kubelettypes.KubernetesPodNamespaceLabel],
		pod:       labels[kubelettypes.KubernetesPodNameLabel],
		container: labels[kubelettypes.KubernetesContainerNameLabel],
	}

	return details, nil
}

func handleWriteFinished(ctx context.Context, event fsnotify.Event) {
	log := log.FromContext(ctx)
	details, err := getCheckpointArchiveInformation(log, event.Name)
	if err != nil {
		log.Error(err, "runGarbageCollector():getCheckpointArchiveInformation()")
		return
	}

	// Check if the checkpoint exceeds MaxCheckpointSize
	fi, err := os.Stat(event.Name)
	if err != nil {
		log.Error(err, "failed to stat", "file", event.Name)
		return
	}

	if maxCheckpointSize > 0 && fi.Size() > maxCheckpointSize {
		log.Info("Deleting checkpoint archive due to exceeding MaxCheckpointSize", "archive", event.Name, "size", fi.Size(), "maxCheckpointSize", maxCheckpointSize)
		err := os.Remove(event.Name)
		if err != nil {
			log.Error(err, "failed to remove checkpoint archive", "archive", event.Name)
		}
		return
	}

	// Handle container-level policy
	log.Info("Handling container-level policy", "container", details.container)
	handleCheckpointsForLevel(log, details, "container", maxCheckpointsPerContainer, maxTotalSizePerContainer, details.container)

	// Handle pod-level policy
	log.Info("Handling pod-level policy", "pod", details.pod)
	handleCheckpointsForLevel(log, details, "pod", maxCheckpointsPerPod, maxTotalSizePerPod, details.pod)

	// Handle namespace-level policy
	log.Info("Handling namespace-level policy", "namespace", details.namespace)
	handleCheckpointsForLevel(log, details, "namespace", maxCheckpointsPerNamespace, maxTotalSizePerNamespace, details.namespace)
}

func handleCheckpointsForLevel(log logr.Logger, details *checkpointDetails, level string, maxCheckpoints int, maxTotalSize int64, levelName string) {
	if maxCheckpoints <= 0 && maxTotalSize <= 0 {
		return
	}

	var globPattern string
	switch level {
	case "container":
		globPattern = filepath.Join(
			checkpointDirectory,
			fmt.Sprintf(
				"checkpoint-%s_%s-%s-[0-9][0-9][0-9][0-9]-[0-2][0-9]-[0-3][0-9]T[0-2][0-9]:[0-5][0-9]:[0-6][0-9]*.tar",
				details.pod,
				details.namespace,
				details.container,
			),
		)
	case "pod":
		globPattern = filepath.Join(
			checkpointDirectory,
			fmt.Sprintf(
				"checkpoint-%s_%s-*-*.tar",
				details.pod,
				details.namespace,
			),
		)
	case "namespace":
		globPattern = filepath.Join(
			checkpointDirectory,
			fmt.Sprintf(
				"checkpoint-*_%s-*-*.tar",
				details.namespace,
			),
		)
	}

	log.Info("Looking for checkpoint archives", "pattern", globPattern)
	checkpointArchives, err := filepath.Glob(globPattern)
	if err != nil {
		log.Error(err, "error looking for checkpoint archives", "pattern", globPattern)
		return
	}

	checkpointArchivesCounter := len(checkpointArchives)
	totalSize := int64(0)
	archiveSizes := make(map[string]int64)
	archivesToDelete := make(map[int64]string)

	for _, c := range checkpointArchives {
		fi, err := os.Stat(c)
		if err != nil {
			log.Error(err, "failed to stat", "file", c)
			continue
		}
		totalSize += fi.Size()
		archiveSizes[c] = fi.Size()
		archivesToDelete[fi.ModTime().UnixMicro()] = c
	}

	// Handle excess checkpoints by count
	if maxCheckpoints > 0 && checkpointArchivesCounter > maxCheckpoints {
		excessCount := int64(checkpointArchivesCounter - maxCheckpoints)
		log.Info("Checkpoint count exceeds limit", "checkpointArchivesCounter", checkpointArchivesCounter, "maxCheckpoints", maxCheckpoints, "excessCount", excessCount)
		toDelete := selectArchivesToDelete(log, checkpointArchives, archiveSizes, excessCount, ByCount)
		for _, archive := range toDelete {
			log.Info("Deleting checkpoint archive due to excess count", "archive", archive)
			err := os.Remove(archive)
			if err != nil {
				log.Error(err, "Removal of checkpoint archive failed", "archive", archive)
			}
			checkpointArchivesCounter--
			if checkpointArchivesCounter <= maxCheckpoints {
				break
			}
		}
	}

	// Handle total size against maxTotalSize
	if maxTotalSize > 0 && totalSize > maxTotalSize {
		excessSize := totalSize - maxTotalSize
		log.Info("Total size of checkpoint archives exceeds limit", "totalSize", totalSize, "maxTotalSize", maxTotalSize, "excessSize", excessSize)
		toDelete := selectArchivesToDelete(log, checkpointArchives, archiveSizes, excessSize, BySize)
		for _, archive := range toDelete {
			log.Info("Deleting checkpoint archive due to excess size", "archive", archive)
			err := os.Remove(archive)
			if err != nil {
				log.Error(err, "Removal of checkpoint archive failed", "archive", archive)
			}
			totalSize -= archiveSizes[archive]
			delete(archiveSizes, archive)
			if totalSize <= maxTotalSize {
				break
			}
		}
	}
}

func selectArchivesToDelete(log logr.Logger, archives []string, archiveSizes map[string]int64, excess int64, policy DeletionPolicy) []string {
	toDelete := make([]string, 0)

	switch policy {
	case ByCount:
		// Sort by modification time (oldest first)
		sort.Slice(archives, func(i, j int) bool {
			fi1, _ := os.Stat(archives[i])
			fi2, _ := os.Stat(archives[j])
			return fi1.ModTime().Before(fi2.ModTime())
		})

		for i := 0; i < int(excess); i++ {
			toDelete = append(toDelete, archives[i])
		}

	case BySize:
		// Sort by modification time (oldest first)
		sort.Slice(archives, func(i, j int) bool {
			fi1, _ := os.Stat(archives[i])
			fi2, _ := os.Stat(archives[j])
			return fi1.ModTime().Before(fi2.ModTime())
		})

		for _, archive := range archives {
			toDelete = append(toDelete, archive)
			excess -= archiveSizes[archive]
			if excess <= 0 {
				break
			}
		}
	}

	return toDelete
}

func (gc *garbageCollector) runGarbageCollector() {
	// This function tries to detect newly created checkpoint archives with the help
	// of inotify/fsnotify. If a new checkpoint archive is created we get a
	// CREATE event and many WRITE events. We have to wait until the last WRITE
	// event before accessing the checkpoint archive.
	// fsnotify has example code how to do this in cmd/fsnotify/dedup.go
	// This is based on that example code.

	var (
		// Wait 100ms for new events; each new event resets the timer.
		waitFor = 100 * time.Millisecond

		// Keep track of the timers, as path → timer.
		mu     sync.Mutex
		timers = make(map[string]*time.Timer)
	)

	ctx := context.TODO()
	log := log.FromContext(ctx)
	// Only start one version of the garbage collection threads.
	gc.Lock()
	defer gc.Unlock()

	// based on fsnotify example code
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Error(err, "runGarbageCollector()")
	}
	defer watcher.Close()

	c := make(chan struct{})

	go func() {
		for {
			select {
			case <-quit:
				c <- struct{}{}
				return
			case event, ok := <-watcher.Events:
				if !ok {
					c <- struct{}{}
					return
				}
				if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) {
					continue
				}
				// Get timer.
				mu.Lock()
				t, ok := timers[event.Name]
				mu.Unlock()

				// No timer yet, so create one.
				if !ok {
					t = time.AfterFunc(math.MaxInt64, func() { handleWriteFinished(ctx, event) })
					t.Stop()

					mu.Lock()
					timers[event.Name] = t
					mu.Unlock()
				}

				// Reset the timer for this path, so it will start from 100ms again.
				t.Reset(waitFor)

			case err, ok := <-watcher.Errors:
				if !ok {
					c <- struct{}{}
					return
				}
				log.Error(err, "runGarbageCollector()")
			}
		}
	}()

	// Add a path.
	log.Info("Watching", "directory", checkpointDirectory)
	log.Info("MaxCheckpointsPerContainer", "maxCheckpointsPerContainer", maxCheckpointsPerContainer)
	log.Info("MaxCheckpointsPerPod", "maxCheckpointsPerPod", maxCheckpointsPerPod)
	log.Info("MaxCheckpointsPerNamespace", "maxCheckpointsPerNamespace", maxCheckpointsPerNamespace)
	log.Info("MaxCheckpointSize", "maxCheckpointSize", maxCheckpointSize)
	log.Info("MaxTotalSizePerNamespace", "maxTotalSizePerNamespace", maxTotalSizePerNamespace)
	log.Info("MaxTotalSizePerPod", "maxTotalSizePerPod", maxTotalSizePerPod)
	log.Info("MaxTotalSizePerContainer", "maxTotalSizePerContainer", maxTotalSizePerContainer)

	err = watcher.Add(checkpointDirectory)
	if err != nil {
		log.Error(err, "runGarbageCollector()")
	}
	<-c
}

// SetupWithManager sets up the controller with the Manager.
func (r *CheckpointRestoreOperatorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	quit = make(chan bool)

	go GarbageCollector.runGarbageCollector()

	return ctrl.NewControllerManagedBy(mgr).
		For(&criuorgv1.CheckpointRestoreOperator{}).
		Complete(r)
}
