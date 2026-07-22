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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// CaptureSpec defines how snapshots are collected. At least one of maxSnapshots
// or maxDuration must be set so the capture run is guaranteed to terminate.
// +kubebuilder:validation:XValidation:rule="has(self.maxSnapshots) || has(self.maxDuration)",message="at least one of maxSnapshots or maxDuration must be set"
type CaptureSpec struct {
	// interval is the time between snapshots
	// +optional
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')",message="interval must be at least 1s"
	Interval *metav1.Duration `json:"interval,omitempty"`

	// maxSnapshots is the maximum number of capture rounds to perform before
	// completing the capture run. Each round checkpoints every selected
	// container of every matching pod once.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxSnapshots *int32 `json:"maxSnapshots,omitempty"`

	// maxDuration is the maximum lifetime of the snapshot run
	// +optional
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')",message="maxDuration must be at least 1s"
	MaxDuration *metav1.Duration `json:"maxDuration,omitempty"`
}

// IntegritySpec defines how to verify the integrity of snapshots
type IntegritySpec struct {
	// hashAlgorithm defines the hash algorithm used to verify integrity
	// +optional
	HashAlgorithm string `json:"hashAlgorithm,omitempty"`
}

// SnapshotChainPhase represents the state of a forensic snapshot run
type SnapshotChainPhase string

const (
	PhasePending   SnapshotChainPhase = "Pending"
	PhaseRunning   SnapshotChainPhase = "Running"
	PhaseCompleted SnapshotChainPhase = "Completed"
	PhaseFailed    SnapshotChainPhase = "Failed"
)

// PostSnapshotAction defines the action to take after the snapshot run completes.
type PostSnapshotAction string

const (
	PostSnapshotActionNone      PostSnapshotAction = "None"
	PostSnapshotActionDeletePod PostSnapshotAction = "DeletePod"
)

// SnapshotChainRecord records a single captured checkpoint round in a
// forensic snapshot chain, including its SHA-256 hash and the hash of the
// previous record for tamper-evident chain linking.
type SnapshotChainRecord struct {
	// index is the position of this record within the snapshot chain.
	// +required
	Index int32 `json:"index"`
	// podName is the name of the pod that was checkpointed.
	// +required
	PodName string `json:"podName"`
	// containerName is the name of the container that was checkpointed.
	// +required
	ContainerName string `json:"containerName"`
	// checkpointPath is the path of the checkpoint archive on the node.
	// +required
	CheckpointPath string `json:"checkpointPath"`
	// snapshotTime is when this checkpoint round was captured.
	// +required
	SnapshotTime metav1.Time `json:"snapshotTime"`
	// sha256Hash is the SHA-256 checksum of the checkpoint archive.
	// +optional
	SHA256Hash string `json:"sha256Hash,omitempty"`
	// previousSHA256Hash is the sha256Hash of the preceding record in the
	// chain, linking records into a tamper-evident sequence.
	// +optional
	PreviousSHA256Hash string `json:"previousSHA256Hash,omitempty"`
	// volumeSnapshots records the CSI VolumeSnapshots captured alongside this
	// checkpoint round, empty when volume snapshotting is disabled.
	// +optional
	// +listType=atomic
	VolumeSnapshots []VolumeSnapshotRef `json:"volumeSnapshots,omitempty"`
}

// ForensicSnapshotChainSpec defines the desired state of ForensicSnapshotChain
type ForensicSnapshotChainSpec struct {
	// namespace is the namespace containing the selected pods
	// +required
	Namespace string `json:"namespace"`

	// selector identifies the target pods
	// +required
	Selector metav1.LabelSelector `json:"selector"`

	// containerNames restricts the snapshotting to specific containers
	// +optional
	// +listType=atomic
	ContainerNames []string `json:"containerNames,omitempty"`
	// capture defines snapshot collection
	// +required
	Capture CaptureSpec `json:"capture"`

	// integrity defines integrity verification
	// +optional
	Integrity IntegritySpec `json:"integrity,omitempty"`

	// volumeSnapshots optionally enables CSI VolumeSnapshot capture of the
	// selected container's PVCs alongside each checkpoint round. When nil or
	// disabled, no volume snapshots are taken and behavior is unchanged.
	// +optional
	VolumeSnapshots *VolumeSnapshotConfig `json:"volumeSnapshots,omitempty"`

	// postSnapshotAction is the action to take after the snapshot run completes.
	// +optional
	// +kubebuilder:validation:Enum=None;DeletePod
	// +default="None"
	PostSnapshotAction PostSnapshotAction `json:"postSnapshotAction,omitempty"`
}

// ForensicSnapshotChainStatus defines the observed state of ForensicSnapshotChain.
type ForensicSnapshotChainStatus struct {
	// conditions represent the latest observations of the snapshot run state.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
	// phase is the current high-level state of the snapshot run.
	// +optional
	Phase SnapshotChainPhase `json:"phase,omitempty"`
	// snapshotCount is the number of capture rounds completed so far.
	// +optional
	SnapshotCount int32 `json:"snapshotCount,omitempty"`
	// attemptCount is the number of capture rounds attempted so far,
	// including rounds in which no pods matched and nothing was captured.
	// It backstops termination of a maxSnapshots-only capture run whose selector
	// never matches; see the controller for details.
	// +optional
	AttemptCount int32 `json:"attemptCount,omitempty"`
	// failureCount is the number of consecutive capture rounds that failed
	// with a checkpoint error. It resets to zero after any round that did not
	// fail. When no maxDuration is configured it backstops termination of a
	// capture run whose target cannot be checkpointed, moving it to Failed rather
	// than retrying forever.
	// +optional
	FailureCount int32 `json:"failureCount,omitempty"`
	// startTime is when the resource entered the Pending phase.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// lastSnapshotTime is when the most recent capture round ran.
	// +optional
	LastSnapshotTime *metav1.Time `json:"lastSnapshotTime,omitempty"`
	// completionTime is when the resource reached a terminal phase.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// errorMessage holds the most recent capture error, if any.
	// +optional
	ErrorMessage string `json:"errorMessage,omitempty"`
	// observedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// snapshotChainRecords is the ordered list of captured checkpoint rounds,
	// each linked to the previous by its SHA-256 hash for integrity verification.
	// +optional
	// +listType=atomic
	SnapshotChainRecords []SnapshotChainRecord `json:"snapshotChainRecords,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ForensicSnapshotChain is the Schema for the forensicsnapshotchains API
type ForensicSnapshotChain struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ForensicSnapshotChain
	// +required
	Spec ForensicSnapshotChainSpec `json:"spec"`

	// status defines the observed state of ForensicSnapshotChain
	// +optional
	Status ForensicSnapshotChainStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ForensicSnapshotChainList contains a list of ForensicSnapshotChain
type ForensicSnapshotChainList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ForensicSnapshotChain `json:"items"`
}
