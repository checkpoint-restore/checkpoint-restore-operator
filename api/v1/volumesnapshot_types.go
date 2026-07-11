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

package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Labels the operator stamps on every VolumeSnapshot it creates. Because a
// checkpoint archive is a node-local .tar with no owning Kubernetes object,
// these labels are the authoritative link from a snapshot back to the archive
// it belongs to. They let a restore or the retention GC resolve a checkpoint's
// snapshots by label selector even when no status record exists (for example an
// archive created by the annotation/event/on-demand triggers).
const (
	// LabelCheckpointArchive is the basename of the checkpoint .tar the
	// snapshot was captured alongside.
	LabelCheckpointArchive = "criu.org/checkpoint-archive"
	// LabelNode is the node that holds the checkpoint archive.
	LabelNode = "criu.org/node"
	// LabelPod is the pod that was checkpointed.
	LabelPod = "criu.org/pod"
	// LabelContainer is the container that was checkpointed.
	LabelContainer = "criu.org/container"
)

// VolumeSnapshotFailurePolicy controls what happens when a volume snapshot
// cannot be taken during a checkpoint round.
type VolumeSnapshotFailurePolicy string

const (
	// FailurePolicyBestEffort proceeds with the checkpoint even if some volume
	// snapshots fail; the failures are recorded but do not abort the round.
	FailurePolicyBestEffort VolumeSnapshotFailurePolicy = "BestEffort"
	// FailurePolicyRequire aborts the round before the checkpoint call if any
	// volume snapshot fails, so an archive is never produced without a full set
	// of volume snapshots.
	FailurePolicyRequire VolumeSnapshotFailurePolicy = "Require"
)

// VolumeSnapshotConfig is the opt-in block that turns on CSI VolumeSnapshot
// capture for a checkpoint producer (CheckpointSchedule or
// ForensicSnapshotChain). When enabled, each checkpoint round snapshots the
// CSI-backed PVCs mounted by the selected container(s) immediately before the
// kubelet checkpoint call. emptyDir, hostPath, configMap and secret volumes are
// skipped: they have no CSI VolumeSnapshot representation.
//
// The guarantee is best-effort crash consistency: the disk snapshot is cut a
// moment before the memory image, so writes in flight during that window may or
// may not be captured and the filesystem is recovered as if from a power cut.
type VolumeSnapshotConfig struct {
	// enabled turns on volume snapshotting for this checkpoint producer.
	// +required
	Enabled bool `json:"enabled"`

	// volumeSnapshotClassName optionally overrides the VolumeSnapshotClass used
	// for the snapshots. When empty, the CSI driver's default
	// VolumeSnapshotClass is used.
	// +optional
	VolumeSnapshotClassName string `json:"volumeSnapshotClassName,omitempty"`

	// failurePolicy selects how a snapshot failure is handled: BestEffort
	// (default) proceeds with the checkpoint and records the failure; Require
	// aborts the round before the checkpoint call.
	// +optional
	// +kubebuilder:validation:Enum=BestEffort;Require
	// +default="BestEffort"
	FailurePolicy VolumeSnapshotFailurePolicy `json:"failurePolicy,omitempty"`

	// readyTimeout is how long to wait for a created VolumeSnapshot to become
	// readyToUse before moving on. Defaults to 60s.
	// +optional
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')",message="readyTimeout must be at least 1s"
	ReadyTimeout *metav1.Duration `json:"readyTimeout,omitempty"`
}

// VolumeSnapshotRef records one VolumeSnapshot captured alongside a checkpoint
// archive, so a later restore can provision a PVC from it.
type VolumeSnapshotRef struct {
	// pvc is the name of the source PersistentVolumeClaim.
	// +required
	PVC string `json:"pvc"`
	// volumeName is the pod-template volume name that referenced the PVC.
	// +optional
	VolumeName string `json:"volumeName,omitempty"`
	// mountPath is where the volume was mounted in the checkpointed container.
	// +optional
	MountPath string `json:"mountPath,omitempty"`
	// snapshotName is the name of the created VolumeSnapshot object.
	// +required
	SnapshotName string `json:"snapshotName"`
	// driver is the CSI driver that backs the snapshot, when known.
	// +optional
	Driver string `json:"driver,omitempty"`
	// readyToUse reports whether the snapshot reached readyToUse before the
	// round completed. A false value under BestEffort still records the
	// reference so a restore can wait on it.
	// +optional
	ReadyToUse bool `json:"readyToUse,omitempty"`
	// error holds the snapshot failure message under BestEffort, if any.
	// +optional
	Error string `json:"error,omitempty"`
}
