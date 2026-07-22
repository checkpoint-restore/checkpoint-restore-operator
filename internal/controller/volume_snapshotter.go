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
	"fmt"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

// The operator creates and deletes CSI VolumeSnapshots for checkpointed PVCs
// and reads VolumeSnapshotClasses to select one.
//+kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotclasses,verbs=get;list;watch

// volumeSnapshotGVK identifies the CSI external-snapshotter VolumeSnapshot
// resource. The operator addresses it as an unstructured object so it does not
// have to vendor the external-snapshotter client and pin its version against
// the operator's own Kubernetes libraries. The operator only ever creates a
// snapshot, reads its readiness, and (elsewhere) deletes it, all of which the
// unstructured client handles.
var volumeSnapshotGVK = schema.GroupVersionKind{
	Group:   "snapshot.storage.k8s.io",
	Version: "v1",
	Kind:    "VolumeSnapshot",
}

// defaultSnapshotReadyTimeout bounds how long we wait for a created
// VolumeSnapshot to become readyToUse when the config does not set readyTimeout.
const defaultSnapshotReadyTimeout = 60 * time.Second

// snapshotReadyPollInterval is how often readiness is polled while waiting.
var snapshotReadyPollInterval = time.Second

// pvcMount is one PVC-backed volume mounted by a checkpointed container.
type pvcMount struct {
	// ClaimName is the PersistentVolumeClaim the volume references.
	ClaimName string
	// VolumeName is the pod-template volume name.
	VolumeName string
	// MountPath is where the container mounts the volume.
	MountPath string
}

// checkpointIdentity is the set of coordinates that link a VolumeSnapshot back
// to the checkpoint it was captured for. The archive basename is stamped after
// the checkpoint call returns (the snapshot is created first), so it is applied
// separately; node/pod/container are known up front.
type checkpointIdentity struct {
	Node      string
	Pod       string
	Container string
}

// mountedPVCsForContainers returns the PVC-backed volumes mounted by the named
// containers of pod. When containerNames is empty, every container is
// considered. Volumes that are not PVC-backed (emptyDir, hostPath, configMap,
// secret, projected, ...) have no CSI VolumeSnapshot representation and are
// skipped. A PVC mounted by more than one selected container is returned once,
// keyed by its volume name; the first mount path seen wins.
func mountedPVCsForContainers(pod *corev1.Pod, containerNames []string) []pvcMount {
	// volumeName -> claimName for PVC-backed pod volumes only.
	claimByVolume := make(map[string]string, len(pod.Spec.Volumes))
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claimByVolume[v.Name] = v.PersistentVolumeClaim.ClaimName
		}
	}

	selected := make(map[string]bool, len(containerNames))
	for _, n := range containerNames {
		selected[n] = true
	}

	var mounts []pvcMount
	seen := make(map[string]bool)
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if len(selected) > 0 && !selected[c.Name] {
			continue
		}
		for _, vm := range c.VolumeMounts {
			claim, ok := claimByVolume[vm.Name]
			if !ok || seen[vm.Name] {
				continue
			}
			seen[vm.Name] = true
			mounts = append(mounts, pvcMount{
				ClaimName:  claim,
				VolumeName: vm.Name,
				MountPath:  vm.MountPath,
			})
		}
	}
	return mounts
}

// snapshotVolumes creates a VolumeSnapshot for every eligible PVC mounted by the
// named containers of pod and returns a reference for each. Each snapshot is
// labelled with the checkpoint identity so it can be resolved back to its
// archive later.
//
// Under FailurePolicyRequire, the first snapshot failure aborts and returns an
// error so the caller can skip the checkpoint entirely. Under
// FailurePolicyBestEffort (the default), a per-PVC failure is captured in the
// returned ref's Error field and the remaining PVCs are still snapshotted; the
// function returns a nil error unless the pod has no eligible PVCs to report on.
func snapshotVolumes(
	ctx context.Context,
	c client.Client,
	pod *corev1.Pod,
	containerNames []string,
	cfg *criuorgv1.VolumeSnapshotConfig,
	ident checkpointIdentity,
) ([]criuorgv1.VolumeSnapshotRef, error) {
	logger := log.FromContext(ctx)

	mounts := mountedPVCsForContainers(pod, containerNames)
	if len(mounts) == 0 {
		logger.Info("volume snapshots enabled but no PVC-backed volumes are mounted; skipping",
			"pod", pod.Name, "containers", containerNames)
		return nil, nil
	}

	require := cfg.FailurePolicy == criuorgv1.FailurePolicyRequire
	timeout := defaultSnapshotReadyTimeout
	if cfg.ReadyTimeout != nil {
		timeout = cfg.ReadyTimeout.Duration
	}

	labels := map[string]string{
		criuorgv1.LabelNode:      ident.Node,
		criuorgv1.LabelPod:       ident.Pod,
		criuorgv1.LabelContainer: ident.Container,
	}

	refs := make([]criuorgv1.VolumeSnapshotRef, 0, len(mounts))
	for _, m := range mounts {
		ref := criuorgv1.VolumeSnapshotRef{
			PVC:        m.ClaimName,
			VolumeName: m.VolumeName,
			MountPath:  m.MountPath,
		}

		name, err := createVolumeSnapshot(ctx, c, pod.Namespace, m.ClaimName, cfg.VolumeSnapshotClassName, labels)
		if err != nil {
			if require {
				return refs, fmt.Errorf("snapshot of PVC %q failed: %w", m.ClaimName, err)
			}
			logger.Error(err, "volume snapshot failed (best-effort)", "pvc", m.ClaimName)
			ref.Error = err.Error()
			refs = append(refs, ref)
			continue
		}
		ref.SnapshotName = name

		ready, err := waitVolumeSnapshotReady(ctx, c, pod.Namespace, name, timeout)
		if err != nil && require {
			return refs, fmt.Errorf("waiting for snapshot %q of PVC %q: %w", name, m.ClaimName, err)
		}
		if err != nil {
			logger.Error(err, "waiting for volume snapshot readiness (best-effort)", "snapshot", name, "pvc", m.ClaimName)
			ref.Error = err.Error()
		}
		ref.ReadyToUse = ready
		refs = append(refs, ref)
	}
	return refs, nil
}

// captureVolumeSnapshots snapshots the PVC-backed volumes mounted by
// containerNames of pod immediately before their checkpoint, when cfg opts in.
// It is the single entry point every checkpoint trigger (schedule, annotation,
// event, resource-pressure, forensic chain) routes through, so snapshot behavior
// is identical everywhere.
//
// It returns the created references and whether the caller should proceed with
// the checkpoint(s): proceed is false only when a snapshot failed under
// FailurePolicyRequire, in which case no checkpoint should be produced for the
// pod so an archive never exists without its volume snapshots. When cfg is nil
// or disabled it is a no-op that returns (nil, true).
func captureVolumeSnapshots(
	ctx context.Context,
	c client.Client,
	cfg *criuorgv1.VolumeSnapshotConfig,
	pod *corev1.Pod,
	containerNames []string,
) ([]criuorgv1.VolumeSnapshotRef, bool) {
	if cfg == nil || !cfg.Enabled {
		return nil, true
	}
	logger := log.FromContext(ctx)

	ident := checkpointIdentity{Node: pod.Spec.NodeName, Pod: pod.Name}
	if len(containerNames) > 0 {
		ident.Container = containerNames[0]
	}

	refs, err := snapshotVolumes(ctx, c, pod, containerNames, cfg, ident)
	if err != nil {
		// Only returned under FailurePolicyRequire; skip the pod's checkpoints.
		logger.Error(err, "volume snapshot failed; skipping checkpoint for pod", "pod", pod.Name)
		return refs, false
	}
	for _, r := range refs {
		logger.Info("captured volume snapshot",
			"pod", pod.Name, "pvc", r.PVC, "snapshot", r.SnapshotName,
			"readyToUse", r.ReadyToUse, "error", r.Error)
	}
	return refs, true
}

// linkSnapshotsToArchive stamps the checkpoint archive basename onto each
// snapshot in refs, completing the link from a VolumeSnapshot back to the
// archive it was captured alongside. The archive name is known only after the
// checkpoint call returns, so this runs after createCheckpoint. It is
// best-effort: a labelling failure is logged but never fails the checkpoint,
// since the node/pod/container labels already provide a usable link.
func linkSnapshotsToArchive(
	ctx context.Context,
	c client.Client,
	namespace string,
	refs []criuorgv1.VolumeSnapshotRef,
	archivePath string,
) {
	if archivePath == "" || len(refs) == 0 {
		return
	}
	logger := log.FromContext(ctx)
	archive := filepath.Base(archivePath)

	for _, r := range refs {
		if r.SnapshotName == "" {
			continue
		}
		snap := &unstructured.Unstructured{}
		snap.SetGroupVersionKind(volumeSnapshotGVK)
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: r.SnapshotName}, snap); err != nil {
			logger.Error(err, "linking snapshot to archive: get failed", "snapshot", r.SnapshotName)
			continue
		}
		labels := snap.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		if labels[criuorgv1.LabelCheckpointArchive] == archive {
			continue
		}
		labels[criuorgv1.LabelCheckpointArchive] = archive
		snap.SetLabels(labels)
		if err := c.Update(ctx, snap); err != nil {
			logger.Error(err, "linking snapshot to archive: update failed", "snapshot", r.SnapshotName, "archive", archive)
		}
	}
}

// createVolumeSnapshot creates a VolumeSnapshot for claimName in namespace using
// a server-generated name, and returns that name. class may be empty to use the
// CSI driver's default VolumeSnapshotClass.
func createVolumeSnapshot(
	ctx context.Context,
	c client.Client,
	namespace, claimName, class string,
	labels map[string]string,
) (string, error) {
	snap := &unstructured.Unstructured{}
	snap.SetGroupVersionKind(volumeSnapshotGVK)
	snap.SetNamespace(namespace)
	snap.SetGenerateName(fmt.Sprintf("checkpoint-%s-", claimName))
	snap.SetLabels(labels)

	if err := unstructured.SetNestedField(
		snap.Object, claimName, "spec", "source", "persistentVolumeClaimName",
	); err != nil {
		return "", err
	}
	if class != "" {
		if err := unstructured.SetNestedField(
			snap.Object, class, "spec", "volumeSnapshotClassName",
		); err != nil {
			return "", err
		}
	}

	if err := c.Create(ctx, snap); err != nil {
		return "", err
	}
	return snap.GetName(), nil
}

// waitVolumeSnapshotReady polls the named VolumeSnapshot until its
// status.readyToUse is true or timeout elapses. A timeout is not itself an
// error for callers that tolerate a not-yet-ready snapshot; it returns
// (false, nil) so the caller records readyToUse=false. Any other error
// (e.g. the snapshot vanished) is returned.
func waitVolumeSnapshotReady(
	ctx context.Context,
	c client.Client,
	namespace, name string,
	timeout time.Duration,
) (bool, error) {
	ready := false
	err := wait.PollUntilContextTimeout(ctx, snapshotReadyPollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			snap := &unstructured.Unstructured{}
			snap.SetGroupVersionKind(volumeSnapshotGVK)
			if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, snap); err != nil {
				return false, err
			}
			r, found, err := unstructured.NestedBool(snap.Object, "status", "readyToUse")
			if err != nil {
				return false, err
			}
			if found && r {
				ready = true
				return true, nil
			}
			return false, nil
		})
	if wait.Interrupted(err) {
		// Timed out waiting for readiness; not fatal for best-effort callers.
		return false, nil
	}
	return ready, err
}
