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

type RetentionPolicy int

const (
	ByCount RetentionPolicy = iota
)

var (
	GarbageCollector           garbageCollector
	policyMutex                sync.RWMutex
	checkpointDirectory        string = "/var/lib/kubelet/checkpoints"
	quit                       chan bool
	maxCheckpointsPerContainer int = 10
	maxCheckpointsPerPod       int = 20
	maxCheckpointsPerNamespace int = 30
	containerPolicies          []criuorgv1.ContainerPolicySpec
	podPolicies                []criuorgv1.PodPolicySpec
	namespacePolicies          []criuorgv1.NamespacePolicySpec
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

	r.handleGlobalPolicies(log, &input.Spec.GlobalPolicies)
	r.handleSpecificPolicies(log, &input.Spec)

	if input.Spec.CheckpointDirectory != "" && input.Spec.CheckpointDirectory != checkpointDirectory {
		checkpointDirectory = input.Spec.CheckpointDirectory
		r.restartGarbageCollector()
	}

	if input.Spec.ApplyPoliciesImmediately {
		go applyPoliciesImmediately(log, checkpointDirectory)
	}

	return ctrl.Result{}, nil
}

func (r *CheckpointRestoreOperatorReconciler) handleGlobalPolicies(log logr.Logger, globalPolicies *criuorgv1.GlobalPolicySpec) {
	policyMutex.Lock()
	defer policyMutex.Unlock()

	if globalPolicies.MaxCheckpointsPerContainer != nil && *globalPolicies.MaxCheckpointsPerContainer >= 0 {
		maxCheckpointsPerContainer = *globalPolicies.MaxCheckpointsPerContainer
		log.Info("Changed MaxCheckpointsPerContainer", "maxCheckpointsPerContainer", maxCheckpointsPerContainer)
	}

	if globalPolicies.MaxCheckpointsPerPod != nil && *globalPolicies.MaxCheckpointsPerPod >= 0 {
		maxCheckpointsPerPod = *globalPolicies.MaxCheckpointsPerPod
		log.Info("Changed MaxCheckpointsPerPod", "maxCheckpointsPerPod", maxCheckpointsPerPod)
	}

	if globalPolicies.MaxCheckpointsPerNamespaces != nil && *globalPolicies.MaxCheckpointsPerNamespaces >= 0 {
		maxCheckpointsPerNamespace = *globalPolicies.MaxCheckpointsPerNamespaces
		log.Info("Changed MaxCheckpointsPerNamespace", "maxCheckpointsPerNamespace", maxCheckpointsPerNamespace)
	}
}

func (r *CheckpointRestoreOperatorReconciler) handleSpecificPolicies(log logr.Logger, spec *criuorgv1.CheckpointRestoreOperatorSpec) {
	policyMutex.Lock()
	defer policyMutex.Unlock()

	// Clear existing policies before applying new ones
	containerPolicies = nil
	podPolicies = nil
	namespacePolicies = nil

	if len(spec.ContainerPolicies) > 0 {
		containerPolicies = spec.ContainerPolicies
		log.Info("Found and applied container-specific policies", "count", len(containerPolicies))
	} else {
		log.Info("No container-specific policies found")
	}

	if len(spec.PodPolicies) > 0 {
		podPolicies = spec.PodPolicies
		log.Info("Found and applied pod-specific policies", "count", len(podPolicies))
	} else {
		log.Info("No pod-specific policies found")
	}

	if len(spec.NamespacePolicies) > 0 {
		namespacePolicies = spec.NamespacePolicies
		log.Info("Found and applied namespace-specific policies", "count", len(namespacePolicies))
	} else {
		log.Info("No namespace-specific policies found")
	}
}

func (r *CheckpointRestoreOperatorReconciler) restartGarbageCollector() {
	quit <- true
	quit = make(chan bool)
	go GarbageCollector.runGarbageCollector()
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

func applyPolicies(log logr.Logger, details *checkpointDetails) {
	policyMutex.Lock()
	defer policyMutex.Unlock()

	if policy := findContainerPolicy(details); policy != nil {
		handleCheckpointsForLevel(log, details, "container", int(*policy.MaxCheckpoints))
	} else if policy := findPodPolicy(details); policy != nil {
		handleCheckpointsForLevel(log, details, "pod", int(*policy.MaxCheckpoints))
	} else if policy := findNamespacePolicy(details); policy != nil {
		handleCheckpointsForLevel(log, details, "namespace", int(*policy.MaxCheckpoints))
	} else {
		// Apply global policies if no specific policy found
		handleCheckpointsForLevel(log, details, "container", maxCheckpointsPerContainer)
		handleCheckpointsForLevel(log, details, "pod", maxCheckpointsPerPod)
		handleCheckpointsForLevel(log, details, "namespace", maxCheckpointsPerNamespace)
	}
}

func findContainerPolicy(details *checkpointDetails) *criuorgv1.ContainerPolicySpec {
	for _, policy := range containerPolicies {
		if policy.Namespace == details.namespace && policy.Pod == details.pod && policy.Container == details.container {
			return &policy
		}
	}
	return nil
}

func findPodPolicy(details *checkpointDetails) *criuorgv1.PodPolicySpec {
	for _, policy := range podPolicies {
		if policy.Namespace == details.namespace && policy.Pod == details.pod {
			return &policy
		}
	}
	return nil
}

func findNamespacePolicy(details *checkpointDetails) *criuorgv1.NamespacePolicySpec {
	for _, policy := range namespacePolicies {
		if policy.Namespace == details.namespace {
			return &policy
		}
	}
	return nil
}

func applyPoliciesImmediately(log logr.Logger, checkpointDirectory string) {
	log.Info("Applying policies immediately")

	checkpointFiles, err := filepath.Glob(filepath.Join(checkpointDirectory, "checkpoint-*_*-*-*.tar"))
	if err != nil {
		log.Error(err, "Failed to list checkpoint files")
		return
	}

	if len(checkpointFiles) == 0 {
		log.Info("No checkpoint files found")
		return
	}

	categorizedCheckpoints := categorizeCheckpoints(log, checkpointFiles)

	for _, details := range categorizedCheckpoints {
		applyPolicies(log, details)
	}
}

func categorizeCheckpoints(log logr.Logger, checkpointFiles []string) map[string]*checkpointDetails {
	categorizedCheckpoints := make(map[string]*checkpointDetails)

	for _, checkpointFile := range checkpointFiles {
		details, err := getCheckpointArchiveInformation(log, checkpointFile)
		if err != nil {
			log.Error(err, "Failed to get checkpoint archive information", "checkpointFile", checkpointFile)
			continue
		}

		key := fmt.Sprintf("%s/%s/%s", details.namespace, details.pod, details.container)
		categorizedCheckpoints[key] = details
	}

	return categorizedCheckpoints
}

func handleWriteFinished(ctx context.Context, event fsnotify.Event) {
	log := log.FromContext(ctx)
	details, err := getCheckpointArchiveInformation(log, event.Name)
	if err != nil {
		log.Error(err, "runGarbageCollector():getCheckpointArchiveInformation()")
		return
	}

	applyPolicies(log, details)
}

func handleCheckpointsForLevel(log logr.Logger, details *checkpointDetails, level string, maxCheckpoints int) {
	if maxCheckpoints <= 0 {
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

	checkpointArchives, err := filepath.Glob(globPattern)
	if err != nil {
		log.Error(err, "error looking for checkpoint archives", "pattern", globPattern)
		return
	}

	var filteredArchives []string
	for _, archive := range checkpointArchives {
		archiveDetails, err := getCheckpointArchiveInformation(log, archive)
		if err != nil {
			log.Error(err, "failed to get archive details", "archive", archive)
			continue
		}

		if (level == "pod" && findContainerPolicy(archiveDetails) != nil) ||
			(level == "namespace" && (findContainerPolicy(archiveDetails) != nil || findPodPolicy(archiveDetails) != nil)) {
			continue
		}

		filteredArchives = append(filteredArchives, archive)
	}

	checkpointArchivesCounter := len(filteredArchives)
	archivesToDelete := make(map[int64]string)

	for _, c := range filteredArchives {
		fi, err := os.Stat(c)
		if err != nil {
			log.Error(
				err,
				"failed to stat",
				"file",
				c,
			)
			continue
		}
		archivesToDelete[fi.ModTime().UnixMicro()] = c
	}

	// Handle excess checkpoints by count
	if maxCheckpoints > 0 && checkpointArchivesCounter > maxCheckpoints {
		excessCount := int64(checkpointArchivesCounter - maxCheckpoints)
		log.Info("Checkpoint count exceeds limit", "checkpointArchivesCounter", checkpointArchivesCounter, "maxCheckpoints", maxCheckpoints, "excessCount", excessCount)
		toDelete := selectArchivesToDelete(log, checkpointArchives, excessCount, ByCount)
		for _, archive := range toDelete {
			log.Info("Deleting checkpoint archive due to excess count", "archive", archive)
			err := os.Remove(archive)
			if err != nil {
				log.Error(err, "removal of checkpoint archive failed", "archive", archive)
			}
			checkpointArchivesCounter--
			if checkpointArchivesCounter <= maxCheckpoints {
				break
			}
		}
	}
}

func selectArchivesToDelete(log logr.Logger, archives []string, excess int64, policy RetentionPolicy) []string {
	toDelete := make([]string, 0)

	switch policy {
	case ByCount:
		// Sort by modification time (oldest first)
		sort.Slice(archives, func(i, j int) bool {
			fileInfo1, err1 := os.Stat(archives[i])
			if err1 != nil {
				log.Error(err1, "Error stating file", archives[i])
				return false
			}

			fileInfo2, err2 := os.Stat(archives[j])
			if err2 != nil {
				log.Error(err2, "Error stating file", archives[j])
				return false
			}

			return fileInfo1.ModTime().Before(fileInfo2.ModTime())
		})

		for i := 0; i < int(excess); i++ {
			toDelete = append(toDelete, archives[i])
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
