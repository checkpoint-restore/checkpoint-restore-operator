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
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	"github.com/checkpoint-restore/checkpoint-restore-operator/internal/checkpointarchive"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	k8err "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/fsnotify/fsnotify"
)

type RetentionPolicy int

// This constants are used to specify the policy criteria
// for selecting which checkpoint archives to delete .
const (
	ByCount RetentionPolicy = iota
	BySize
)

var (
	GarbageCollector           garbageCollector
	policyMutex                sync.RWMutex
	checkpointDirectory        string = "/var/lib/kubelet/checkpoints"
	quit                       chan bool
	maxCheckpointsPerContainer int = 10
	maxCheckpointsPerPod       int = 20
	maxCheckpointsPerNamespace int = 30
	retainOrphan               *bool
	maxCheckpointSize          resource.Quantity = resource.MustParse("100Gi")
	maxTotalSizePerPod         resource.Quantity = resource.MustParse("100Gi")
	maxTotalSizePerContainer   resource.Quantity = resource.MustParse("100Gi")
	maxTotalSizePerNamespace   resource.Quantity = resource.MustParse("100Gi")
	containerPolicies          []criuorgv1.ContainerPolicySpec
	podPolicies                []criuorgv1.PodPolicySpec
	namespacePolicies          []criuorgv1.NamespacePolicySpec
)

type garbageCollector struct {
	sync.Mutex
}

type policySnapshot struct {
	checkpointDirectory        string
	retainOrphan               bool
	maxCheckpointsPerContainer int
	maxCheckpointsPerPod       int
	maxCheckpointsPerNamespace int
	maxCheckpointSize          resource.Quantity
	maxTotalSizePerPod         resource.Quantity
	maxTotalSizePerContainer   resource.Quantity
	maxTotalSizePerNamespace   resource.Quantity
	containerPolicies          []criuorgv1.ContainerPolicySpec
	podPolicies                []criuorgv1.PodPolicySpec
	namespacePolicies          []criuorgv1.NamespacePolicySpec
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

	policies, restartGarbageCollector := r.applyPolicySpec(log, &input.Spec)
	if restartGarbageCollector {
		r.restartGarbageCollector()
	}

	if input.Spec.ApplyPoliciesImmediately {
		go applyPoliciesImmediately(log, policies)
	}

	return ctrl.Result{}, nil
}

func resetAllPoliciesToDefault(log logr.Logger) {
	policyMutex.Lock()
	defer policyMutex.Unlock()

	resetAllPoliciesToDefaultLocked(log)
}

func resetAllPoliciesToDefaultLocked(log logr.Logger) {
	retainOrphan = nil
	maxCheckpointsPerContainer = 10
	maxCheckpointsPerPod = 20
	maxCheckpointsPerNamespace = 30
	maxCheckpointSize = resource.MustParse("1Ei")
	maxTotalSizePerContainer = resource.MustParse("1Ei")
	maxTotalSizePerPod = resource.MustParse("1Ei")
	maxTotalSizePerNamespace = resource.MustParse("1Ei")

	containerPolicies = nil
	podPolicies = nil
	namespacePolicies = nil

	log.Info("Policies have been reset to their default values")
}

func ptr(b bool) *bool {
	return &b
}

func (r *CheckpointRestoreOperatorReconciler) handleGlobalPolicies(log logr.Logger, globalPolicies *criuorgv1.GlobalPolicySpec) {
	policyMutex.Lock()
	defer policyMutex.Unlock()

	r.handleGlobalPoliciesLocked(log, globalPolicies)
}

func (r *CheckpointRestoreOperatorReconciler) handleGlobalPoliciesLocked(log logr.Logger, globalPolicies *criuorgv1.GlobalPolicySpec) {
	if globalPolicies.RetainOrphan == nil {
		retainOrphan = ptr(true)
		log.Info("RetainOrphan policy not set, using default", "retainOrphan", retainOrphan)
	} else {
		retainOrphan = globalPolicies.RetainOrphan
		log.Info("Changed RetainOrphan policy", "retainOrphan", retainOrphan)
	}

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

	if globalPolicies.MaxCheckpointSize != nil {
		maxCheckpointSize = *globalPolicies.MaxCheckpointSize
		log.Info("Changed MaxCheckpointSize", "maxCheckpointSize", maxCheckpointSize.String())
	}

	if globalPolicies.MaxTotalSizePerNamespace != nil {
		maxTotalSizePerNamespace = *globalPolicies.MaxTotalSizePerNamespace
		log.Info("Changed MaxTotalSizePerNamespace", "maxTotalSizePerNamespace", maxTotalSizePerNamespace.String())
	}

	if globalPolicies.MaxTotalSizePerPod != nil {
		maxTotalSizePerPod = *globalPolicies.MaxTotalSizePerPod
		log.Info("Changed MaxTotalSizePerPod", "maxTotalSizePerPod", maxTotalSizePerPod.String())
	}

	if globalPolicies.MaxTotalSizePerContainer != nil {
		maxTotalSizePerContainer = *globalPolicies.MaxTotalSizePerContainer
		log.Info("Changed MaxTotalSizePerContainer", "maxTotalSizePerContainer", maxTotalSizePerContainer.String())
	}
}

func (r *CheckpointRestoreOperatorReconciler) handleSpecificPolicies(log logr.Logger, spec *criuorgv1.CheckpointRestoreOperatorSpec) {
	policyMutex.Lock()
	defer policyMutex.Unlock()

	r.handleSpecificPoliciesLocked(log, spec)
}

func (r *CheckpointRestoreOperatorReconciler) handleSpecificPoliciesLocked(log logr.Logger, spec *criuorgv1.CheckpointRestoreOperatorSpec) {
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

func (r *CheckpointRestoreOperatorReconciler) applyPolicySpec(log logr.Logger, spec *criuorgv1.CheckpointRestoreOperatorSpec) (policySnapshot, bool) {
	policyMutex.Lock()
	defer policyMutex.Unlock()

	resetAllPoliciesToDefaultLocked(log)
	r.handleGlobalPoliciesLocked(log, &spec.GlobalPolicies)
	r.handleSpecificPoliciesLocked(log, spec)

	restartGarbageCollector := false
	if spec.CheckpointDirectory != "" && spec.CheckpointDirectory != checkpointDirectory {
		checkpointDirectory = spec.CheckpointDirectory
		restartGarbageCollector = true
	}

	return currentPolicySnapshotLocked(), restartGarbageCollector
}

func (r *CheckpointRestoreOperatorReconciler) restartGarbageCollector() {
	quit <- true
	quit = make(chan bool)
	go GarbageCollector.runGarbageCollector()
}

func currentPolicySnapshot() policySnapshot {
	policyMutex.RLock()
	defer policyMutex.RUnlock()

	return currentPolicySnapshotLocked()
}

func currentPolicySnapshotLocked() policySnapshot {
	retain := true
	if retainOrphan != nil {
		retain = *retainOrphan
	}

	return policySnapshot{
		checkpointDirectory:        checkpointDirectory,
		retainOrphan:               retain,
		maxCheckpointsPerContainer: maxCheckpointsPerContainer,
		maxCheckpointsPerPod:       maxCheckpointsPerPod,
		maxCheckpointsPerNamespace: maxCheckpointsPerNamespace,
		maxCheckpointSize:          maxCheckpointSize.DeepCopy(),
		maxTotalSizePerPod:         maxTotalSizePerPod.DeepCopy(),
		maxTotalSizePerContainer:   maxTotalSizePerContainer.DeepCopy(),
		maxTotalSizePerNamespace:   maxTotalSizePerNamespace.DeepCopy(),
		containerPolicies:          copyContainerPolicies(containerPolicies),
		podPolicies:                copyPodPolicies(podPolicies),
		namespacePolicies:          copyNamespacePolicies(namespacePolicies),
	}
}

func copyBoolPtr(value *bool) *bool {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func copyIntPtr(value *int) *int {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func copyQuantityPtr(value *resource.Quantity) *resource.Quantity {
	if value == nil {
		return nil
	}
	copied := value.DeepCopy()
	return &copied
}

func copyContainerPolicies(policies []criuorgv1.ContainerPolicySpec) []criuorgv1.ContainerPolicySpec {
	if len(policies) == 0 {
		return nil
	}
	copied := make([]criuorgv1.ContainerPolicySpec, len(policies))
	for i := range policies {
		copied[i] = policies[i]
		copied[i].RetainOrphan = copyBoolPtr(policies[i].RetainOrphan)
		copied[i].MaxCheckpoints = copyIntPtr(policies[i].MaxCheckpoints)
		copied[i].MaxCheckpointSize = copyQuantityPtr(policies[i].MaxCheckpointSize)
		copied[i].MaxTotalSize = copyQuantityPtr(policies[i].MaxTotalSize)
	}
	return copied
}

func copyPodPolicies(policies []criuorgv1.PodPolicySpec) []criuorgv1.PodPolicySpec {
	if len(policies) == 0 {
		return nil
	}
	copied := make([]criuorgv1.PodPolicySpec, len(policies))
	for i := range policies {
		copied[i] = policies[i]
		copied[i].RetainOrphan = copyBoolPtr(policies[i].RetainOrphan)
		copied[i].MaxCheckpoints = copyIntPtr(policies[i].MaxCheckpoints)
		copied[i].MaxCheckpointSize = copyQuantityPtr(policies[i].MaxCheckpointSize)
		copied[i].MaxTotalSize = copyQuantityPtr(policies[i].MaxTotalSize)
	}
	return copied
}

func copyNamespacePolicies(policies []criuorgv1.NamespacePolicySpec) []criuorgv1.NamespacePolicySpec {
	if len(policies) == 0 {
		return nil
	}
	copied := make([]criuorgv1.NamespacePolicySpec, len(policies))
	for i := range policies {
		copied[i] = policies[i]
		copied[i].RetainOrphan = copyBoolPtr(policies[i].RetainOrphan)
		copied[i].MaxCheckpoints = copyIntPtr(policies[i].MaxCheckpoints)
		copied[i].MaxCheckpointSize = copyQuantityPtr(policies[i].MaxCheckpointSize)
		copied[i].MaxTotalSize = copyQuantityPtr(policies[i].MaxTotalSize)
	}
	return copied
}

// UntarFiles unpacks only the specified files from an archive to the
// destination directory. It delegates to the shared checkpointarchive package
// so the operator and the node-side CRI proxy apply identical hardened
// extraction rules (exact entry matching, regular files only, traversal
// guard, per-file size bound).
func UntarFiles(src, dest string, files []string) error {
	return checkpointarchive.UntarFiles(src, dest, files)
}

type checkpointDetails struct {
	namespace string
	pod       string
	container string
}

func getCheckpointArchiveInformation(_ logr.Logger, checkpointPath string) (*checkpointDetails, error) {
	ref, err := checkpointarchive.ReadPodContainerRef(checkpointPath)
	if err != nil {
		return nil, err
	}
	details := &checkpointDetails{
		namespace: ref.Namespace,
		pod:       ref.Pod,
		container: ref.Container,
	}

	return details, nil
}

type Policy struct {
	RetainOrphan      bool
	MaxCheckpoints    int
	MaxCheckpointSize resource.Quantity
	MaxTotalSize      resource.Quantity
}

func applyPolicies(log logr.Logger, details *checkpointDetails) {
	policies := currentPolicySnapshot()
	policies.applyPolicies(log, details)
}

func (policies policySnapshot) applyPolicies(log logr.Logger, details *checkpointDetails) {
	// Function to handle default "infinity" value for count-based policies
	toInfinityCount := func(value *int) int {
		if value == nil {
			return math.MaxInt32
		}
		return *value
	}

	// Function to handle default "infinity" value for size-based policies
	toInfinitySize := func(value *resource.Quantity) resource.Quantity {
		if value == nil {
			return resource.MustParse("1Ei")
		}
		return *value
	}

	ifNil := func(value *bool) bool {
		if value == nil {
			return true
		}
		return *value
	}

	if policy := policies.findContainerPolicy(details); policy != nil {
		policies.handleCheckpointsForLevel(log, details, "container", Policy{
			RetainOrphan:      ifNil(policy.RetainOrphan),
			MaxCheckpoints:    toInfinityCount(policy.MaxCheckpoints),
			MaxCheckpointSize: toInfinitySize(policy.MaxCheckpointSize),
			MaxTotalSize:      toInfinitySize(policy.MaxTotalSize),
		})
	} else if policy := policies.findPodPolicy(details); policy != nil {
		policies.handleCheckpointsForLevel(log, details, "pod", Policy{
			RetainOrphan:      ifNil(policy.RetainOrphan),
			MaxCheckpoints:    toInfinityCount(policy.MaxCheckpoints),
			MaxCheckpointSize: toInfinitySize(policy.MaxCheckpointSize),
			MaxTotalSize:      toInfinitySize(policy.MaxTotalSize),
		})
	} else if policy := policies.findNamespacePolicy(details); policy != nil {
		policies.handleCheckpointsForLevel(log, details, "namespace", Policy{
			RetainOrphan:      ifNil(policy.RetainOrphan),
			MaxCheckpoints:    toInfinityCount(policy.MaxCheckpoints),
			MaxCheckpointSize: toInfinitySize(policy.MaxCheckpointSize),
			MaxTotalSize:      toInfinitySize(policy.MaxTotalSize),
		})
	} else {
		// Apply global policies if no specific policy found
		policies.handleCheckpointsForLevel(log, details, "container", Policy{
			RetainOrphan:      policies.retainOrphan,
			MaxCheckpoints:    policies.maxCheckpointsPerContainer,
			MaxCheckpointSize: policies.maxCheckpointSize,
			MaxTotalSize:      policies.maxTotalSizePerContainer,
		})
		policies.handleCheckpointsForLevel(log, details, "pod", Policy{
			RetainOrphan:      policies.retainOrphan,
			MaxCheckpoints:    policies.maxCheckpointsPerPod,
			MaxCheckpointSize: policies.maxCheckpointSize,
			MaxTotalSize:      policies.maxTotalSizePerPod,
		})
		policies.handleCheckpointsForLevel(log, details, "namespace", Policy{
			RetainOrphan:      policies.retainOrphan,
			MaxCheckpoints:    policies.maxCheckpointsPerNamespace,
			MaxCheckpointSize: policies.maxCheckpointSize,
			MaxTotalSize:      policies.maxTotalSizePerNamespace,
		})
	}
}

func (policies policySnapshot) findContainerPolicy(details *checkpointDetails) *criuorgv1.ContainerPolicySpec {
	for i := range policies.containerPolicies {
		policy := &policies.containerPolicies[i]
		if policy.Namespace == details.namespace && policy.Pod == details.pod && policy.Container == details.container {
			return policy
		}
	}
	return nil
}

func (policies policySnapshot) findPodPolicy(details *checkpointDetails) *criuorgv1.PodPolicySpec {
	for i := range policies.podPolicies {
		policy := &policies.podPolicies[i]
		if policy.Namespace == details.namespace && policy.Pod == details.pod {
			return policy
		}
	}
	return nil
}

func (policies policySnapshot) findNamespacePolicy(details *checkpointDetails) *criuorgv1.NamespacePolicySpec {
	for i := range policies.namespacePolicies {
		policy := &policies.namespacePolicies[i]
		if policy.Namespace == details.namespace {
			return policy
		}
	}
	return nil
}

func applyPoliciesImmediately(log logr.Logger, policies policySnapshot) {
	log.Info("Applying policies immediately")

	checkpointFiles, err := filepath.Glob(filepath.Join(policies.checkpointDirectory, "checkpoint-*_*-*-*.tar"))
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
		policies.applyPolicies(log, details)
	}
}

func categorizeCheckpoints(log logr.Logger, checkpointFiles []string) map[string]*checkpointDetails {
	categorizedCheckpoints := make(map[string]*checkpointDetails)

	for _, checkpointFile := range checkpointFiles {
		details, err := getCheckpointArchiveInformation(log, checkpointFile)
		if err != nil {
			log.V(1).Info("Skipping checkpoint archive with unreadable metadata", "checkpointFile", checkpointFile, "error", err)
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

func handleCheckpointsForLevel(log logr.Logger, details *checkpointDetails, level string, policy Policy) {
	policies := currentPolicySnapshot()
	policies.handleCheckpointsForLevel(log, details, level, policy)
}

func (policies policySnapshot) handleCheckpointsForLevel(log logr.Logger, details *checkpointDetails, level string, policy Policy) {
	if policy.MaxCheckpoints <= 0 {
		log.Info("MaxCheckpoints is less than or equal to 0, skipping checkpoint handling", "level", level, "policy.MaxCheckpoints", policy.MaxCheckpoints)
		return
	}
	if policy.MaxCheckpointSize.Value() <= 0 {
		log.Info("MaxCheckpointSize is less than or equal to 0, skipping checkpoint handling", "level", level, "policy.MaxCheckpointSize", policy.MaxCheckpointSize)
		return
	}
	if policy.MaxTotalSize.Value() <= 0 {
		log.Info("MaxTotalSize is less than or equal to 0, skipping checkpoint handling", "level", level, "policy.MaxTotalSize", policy.MaxTotalSize)
		return
	}

	var globPattern string
	switch level {
	case "container":
		globPattern = filepath.Join(
			policies.checkpointDirectory,
			fmt.Sprintf(
				"checkpoint-%s_%s-%s-[0-9][0-9][0-9][0-9]-[0-2][0-9]-[0-3][0-9]T[0-2][0-9]:[0-5][0-9]:[0-6][0-9]*.tar",
				details.pod,
				details.namespace,
				details.container,
			),
		)
	case "pod":
		globPattern = filepath.Join(
			policies.checkpointDirectory,
			fmt.Sprintf(
				"checkpoint-%s_%s-*-*.tar",
				details.pod,
				details.namespace,
			),
		)
	case "namespace":
		globPattern = filepath.Join(
			policies.checkpointDirectory,
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
			log.V(1).Info("Skipping checkpoint archive with unreadable metadata", "archive", archive, "error", err)
			continue
		}

		if (level == "pod" && policies.findContainerPolicy(archiveDetails) != nil) ||
			(level == "namespace" && (policies.findContainerPolicy(archiveDetails) != nil || policies.findPodPolicy(archiveDetails) != nil)) {
			continue
		}

		filteredArchives = append(filteredArchives, archive)
	}

	// Partition into deletable and pinned before any deletion.
	_, pinned := partitionArchives(filteredArchives)

	// Per-scope keys for edge-triggered "retention unreachable" logging, so the message
	// is logged only when the set of blocking pinned archives changes for this scope.
	var scope string
	switch level {
	case "pod":
		scope = details.namespace + "/" + details.pod
	case "namespace":
		scope = details.namespace
	default: // container
		scope = details.namespace + "/" + details.pod + "/" + details.container
	}
	countKey := level + "|count|" + scope
	sizeKey := level + "|size|" + scope

	if !policy.RetainOrphan {
		exist, err := resourceExistsInCluster(level, details)
		if err != nil {
			log.Error(err, "failed to check if resource exists in cluster", "level", level)
		}

		if !exist {
			log.Info("RetainOrphan is set to false, deleting all unpinned checkpoints", "level", level)

			for _, archive := range filteredArchives {
				if isCheckpointPinned(archive) {
					log.Info("checkpoint is pinned, skipping deletion", "archive", archive)
					continue
				}
				log.Info("Deleting checkpoint archive due to retainCheckpoint=false", "archive", archive)
				removeArchiveUnlessPinned(log, archive)
			}

			return
		}
	}

	checkpointArchivesCounter := len(filteredArchives)
	totalSize := resource.NewQuantity(0, resource.BinarySI)
	archiveSizes := make(map[string]int64)
	var deletableCandidates []string

	for _, c := range filteredArchives {
		fi, err := os.Stat(c)
		if err != nil {
			log.Error(err, "failed to stat", "file", c)
			continue
		}

		currentSize := resource.NewQuantity(fi.Size(), resource.BinarySI)
		log.Info("Checkpoint archive details", "archive", c, "size", currentSize.String(), "maxCheckpointSize", policy.MaxCheckpointSize.String())

		if policy.MaxCheckpointSize.Cmp(*currentSize) < 0 {
			if isCheckpointPinned(c) {
				// Pinned archive exceeds per-file limit: count it but do not delete.
				log.Info("checkpoint is pinned, skipping deletion", "archive", c)
				log.V(1).Info("pinned archive included in accounting", "archive", c, "size", currentSize.String())
				totalSize.Add(*currentSize)
				archiveSizes[c] = fi.Size()
				continue
			}
			log.Info("Deleting checkpoint archive due to exceeding MaxCheckpointSize", "archive", c, "size", currentSize.String(), "maxCheckpointSize", policy.MaxCheckpointSize.String())
			removeArchiveUnlessPinned(log, c)
			// The archive is no longer retained; drop it from the count so the
			// count-rotation phase below does not over-delete surviving archives.
			checkpointArchivesCounter--
			continue
		}

		totalSize.Add(*currentSize)
		archiveSizes[c] = fi.Size()

		if isCheckpointPinned(c) {
			log.V(1).Info("pinned archive included in accounting", "archive", c, "size", currentSize.String())
		} else {
			deletableCandidates = append(deletableCandidates, c)
		}
	}

	// Handle excess checkpoints by count.
	if policy.MaxCheckpoints > 0 && checkpointArchivesCounter > policy.MaxCheckpoints {
		excessCount := int64(checkpointArchivesCounter - policy.MaxCheckpoints)
		log.Info("Checkpoint count exceeds limit", "checkpointArchivesCounter", checkpointArchivesCounter, "maxCheckpoints", policy.MaxCheckpoints, "excessCount", excessCount)
		toDelete := selectArchivesToDelete(log, deletableCandidates, archiveSizes, excessCount, ByCount)
		for _, archive := range toDelete {
			log.Info("Deleting checkpoint archive due to excess count", "archive", archive)
			removeArchiveUnlessPinned(log, archive)
			checkpointArchivesCounter--
			if checkpointArchivesCounter <= policy.MaxCheckpoints {
				break
			}
		}
	}
	if checkpointArchivesCounter > policy.MaxCheckpoints {
		if logRetentionUnreachableChanged(countKey, pinned) {
			log.Info("retention limit unreachable: pinned checkpoints prevent full count rotation",
				"remaining", checkpointArchivesCounter,
				"limit", policy.MaxCheckpoints,
				"pinned", pinnedNames(pinned),
			)
		}
	} else {
		clearRetentionUnreachable(countKey)
	}

	// Handle total size against maxTotalSize.
	if policy.MaxTotalSize.Cmp(*totalSize) < 0 {
		excessSize := totalSize.DeepCopy()
		excessSize.Sub(policy.MaxTotalSize)
		log.Info("Total size of checkpoint archives exceeds limit", "totalSize", totalSize.String(), "maxTotalSize", policy.MaxTotalSize.String(), "excessSize", excessSize.String())
		toDelete := selectArchivesToDelete(log, deletableCandidates, archiveSizes, excessSize.Value(), BySize)
		for _, archive := range toDelete {
			log.Info("Deleting checkpoint archive due to excess size", "archive", archive)
			removeArchiveUnlessPinned(log, archive)
			currentSize := resource.NewQuantity(archiveSizes[archive], resource.BinarySI)
			totalSize.Sub(*currentSize)
			delete(archiveSizes, archive)
			if policy.MaxTotalSize.Cmp(*totalSize) >= 0 {
				break
			}
		}
	}
	if policy.MaxTotalSize.Cmp(*totalSize) < 0 {
		if logRetentionUnreachableChanged(sizeKey, pinned) {
			log.Info("retention limit unreachable: pinned checkpoints prevent full size rotation",
				"totalSize", totalSize.String(),
				"limit", policy.MaxTotalSize.String(),
				"pinned", pinnedNames(pinned),
			)
		}
	} else {
		clearRetentionUnreachable(sizeKey)
	}
}

// pinnedNames returns the base filenames of pinned archives for log messages.
func pinnedNames(paths []string) []string {
	names := make([]string, len(paths))
	for i, p := range paths {
		names[i] = filepath.Base(p)
	}
	return names
}

// retentionUnreachableState records, per scope+limit key, the set of pinned archive
// names last reported as blocking retention. It lets handleCheckpointsForLevel log the
// "retention unreachable" message edge-triggered (only when the blocking set changes)
// instead of on every evaluation, which would otherwise flood the log because retention
// is re-evaluated on every checkpoint write and pod event.
var (
	retentionUnreachableMu    sync.Mutex
	retentionUnreachableState = map[string]string{}
)

// logRetentionUnreachableChanged reports whether the set of pinned archives blocking
// retention for key differs from the last call. It records the new set as a side effect,
// so the caller should log only when this returns true.
func logRetentionUnreachableChanged(key string, pinned []string) bool {
	cur := strings.Join(pinnedNames(pinned), ",")
	retentionUnreachableMu.Lock()
	defer retentionUnreachableMu.Unlock()
	if retentionUnreachableState[key] == cur {
		return false
	}
	retentionUnreachableState[key] = cur
	return true
}

// clearRetentionUnreachable forgets any recorded blocking state for key, so that a later
// genuine breach is logged again rather than suppressed as a duplicate.
func clearRetentionUnreachable(key string) {
	retentionUnreachableMu.Lock()
	defer retentionUnreachableMu.Unlock()
	delete(retentionUnreachableState, key)
}

func selectArchivesToDelete(log logr.Logger, archives []string, archiveSizes map[string]int64, excess int64, policy RetentionPolicy) []string {
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

		// excess may exceed len(archives) when pinned archives are excluded
		// from the candidate list; only delete as many as are available.
		for i := 0; i < int(excess) && i < len(archives); i++ {
			toDelete = append(toDelete, archives[i])
		}

	case BySize:
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

func resourceExistsInCluster(level string, details *checkpointDetails) (bool, error) {
	switch level {
	case "container":
		pod, err := getPodFromNamespace(details.namespace, details.pod)
		if err != nil {
			if isNotFoundError(err) {
				return false, nil
			}
			return false, err
		}
		for _, container := range pod.Spec.Containers {
			if container.Name == details.container {
				return true, nil
			}
		}
		return false, nil

	case "pod":
		_, err := getPodFromNamespace(details.namespace, details.pod)
		if err != nil {
			if isNotFoundError(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil

	case "namespace":
		_, err := getNamespace(details.namespace)
		if err != nil {
			if isNotFoundError(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil

	default:
		return false, fmt.Errorf("invalid level: %s", level)
	}
}

func isNotFoundError(err error) bool {
	return k8err.IsNotFound(err)
}

func getKubernetesClient() (*kubernetes.Clientset, error) {
	// Use in-cluster configuration
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

func getPodFromNamespace(namespace, podName string) (*v1.Pod, error) {
	clientset, err := getKubernetesClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %v", err)
	}

	pod, err := clientset.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return pod, nil
}

func getNamespace(namespace string) (*v1.Namespace, error) {
	clientset, err := getKubernetesClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %v", err)
	}

	ns, err := clientset.CoreV1().Namespaces().Get(context.TODO(), namespace, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return ns, nil
}

func (gc *garbageCollector) PodWatcher(log logr.Logger, stopCh <-chan struct{}) {
	clientset, err := getKubernetesClient()
	if err != nil {
		log.Error(err, "Failed to create Kubernetes client")
		return
	}

	watchlist := cache.NewListWatchFromClient(
		clientset.CoreV1().RESTClient(),
		"pods",
		metav1.NamespaceAll,
		fields.Everything(),
	)

	_, controller := cache.NewInformerWithOptions(cache.InformerOptions{
		ListerWatcher: watchlist,
		ObjectType:    &v1.Pod{},
		Handler: cache.ResourceEventHandlerFuncs{
			DeleteFunc: func(obj interface{}) {
				pod, ok := obj.(*v1.Pod)
				if !ok {
					log.Info("Could not convert object to Pod")
					return
				}
				handlePodDeleted(log, pod)
			},
		},
	})

	go controller.Run(stopCh)

	<-stopCh
	log.Info("PodWatcher has been stopped")
}

func handlePodDeleted(log logr.Logger, pod *v1.Pod) {
	log.Info("Pod deleted", "pod", pod.Name)

	containerNames := getContainerNames(pod)

	for _, containerName := range containerNames {
		details := &checkpointDetails{
			namespace: pod.Namespace,
			pod:       pod.Name,
			container: containerName,
		}
		applyPolicies(log, details)
	}
}

func getContainerNames(pod *v1.Pod) []string {
	var containerNames []string
	for _, container := range pod.Spec.Containers {
		containerNames = append(containerNames, container.Name)
	}
	return containerNames
}

func (gc *garbageCollector) runGarbageCollector() {
	// This function tries to detect newly created checkpoint archives with the help
	// of inotify/fsnotify. If a new checkpoint archive is created we get a
	// CREATE event and many WRITE events. We have to wait until the last WRITE
	// event before accessing the checkpoint archive.
	// fsnotify has example code how to do this in cmd/fsnotify/dedup.go
	// This is based on that example code.

	var (
		// Wait 1s for new events; each new event resets the timer.
		// 100ms was too short for large checkpoint archives still being written.
		waitFor = 1 * time.Second

		// Keep track of the timers, as path -> timer.
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
		// Creating an inotify instance can fail ("too many open files" when
		// fs.inotify.max_user_instances is exhausted). Continuing with a nil
		// watcher would panic and crash-loop the whole operator; instead stay
		// parked on quit so restartGarbageCollector (a policy change) can retry.
		log.Error(err, "failed to create fsnotify watcher; retention garbage collection is disabled until the next policy change (check fs.inotify.max_user_instances)")
		<-quit
		return
	}
	defer func() { _ = watcher.Close() }()

	c := make(chan struct{})

	// Start pod watcher in a separate goroutine
	go gc.PodWatcher(log, c)

	go func() {
		// close(c), not a single send: c is read by the pod informer
		// (controller.Run), PodWatcher's final wait, and runGarbageCollector's
		// own <-c. A single send wakes only one of them, leaving the others
		// running: either the old GC never releases gc.Lock (deadlocking the
		// restarted GC) or informers pile up on every policy change.
		for {
			select {
			case <-quit:
				close(c)
				return
			case event, ok := <-watcher.Events:
				if !ok {
					close(c)
					return
				}
				if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) {
					continue
				}
				if strings.HasSuffix(event.Name, ".keep") || strings.HasSuffix(event.Name, ".keep.tmp") {
					log.V(1).Info("skipping .keep marker file in GC event handler", "path", event.Name)
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
					close(c)
					return
				}
				log.Error(err, "runGarbageCollector()")
			}
		}
	}()

	policies := currentPolicySnapshot()

	// Add a path.
	log.Info("Watching", "directory", policies.checkpointDirectory)
	log.Info("MaxCheckpointsPerContainer", "maxCheckpointsPerContainer", policies.maxCheckpointsPerContainer)
	log.Info("MaxCheckpointsPerPod", "maxCheckpointsPerPod", policies.maxCheckpointsPerPod)
	log.Info("MaxCheckpointsPerNamespace", "maxCheckpointsPerNamespace", policies.maxCheckpointsPerNamespace)
	log.Info("MaxCheckpointSize", "maxCheckpointSize", policies.maxCheckpointSize)
	log.Info("MaxTotalSizePerNamespace", "maxTotalSizePerNamespace", policies.maxTotalSizePerNamespace)
	log.Info("MaxTotalSizePerPod", "maxTotalSizePerPod", policies.maxTotalSizePerPod)
	log.Info("MaxTotalSizePerContainer", "maxTotalSizePerContainer", policies.maxTotalSizePerContainer)

	err = watcher.Add(policies.checkpointDirectory)
	if err != nil {
		// Keep waiting on c rather than returning: the event goroutine above is
		// already consuming quit, so a policy change can still restart the GC.
		log.Error(err, "failed to watch checkpoint directory; retention garbage collection is disabled until the next policy change", "directory", policies.checkpointDirectory)
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
