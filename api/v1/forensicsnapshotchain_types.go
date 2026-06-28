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

// CaptureSpec defines how snapshots are collected
type CaptureSpec struct {
	// interval is the time between snapshots
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	//maximum number of snapshots to create
	// +optional
	MaxSnapshots *int `json:"maxSnapshots,omitempty"`

	//maximum lifetime of the snapshot chain
	// +optional
	MaxDuration *metav1.Duration `json:"maxDuration,omitempty"`
}

// IntegritySpec defines how to verify the integrity of snapshots
type IntegritySpec struct {
	//defines the hash algorithm used to verify integrity
	// +optional
	HashAlgorithm string `json:"hashAlgorithm,omitempty"`
}

// SnapshotChainPhase represents the state of a forensic snapshot chain
type SnapshotChainPhase string

const (
	PhasePending   SnapshotChainPhase = "Pending"
	PhaseRunning   SnapshotChainPhase = "Running"
	PhaseCompleted SnapshotChainPhase = "Completed"
	PhaseFailed    SnapshotChainPhase = "Failed"
)

// PostSnapshotAction defines the action to take after each snapshotting of a pod, like after the snapshot chain is created
type PostSnapshotAction string

const (
	PostSnapshotActionNone      PostSnapshotAction = "None"
	PostSnapshotActionDeletePod PostSnapshotAction = "DeletePod"
)

//Snapshot Record to keep track of all snapshots in a chain

type SnapshotChainRecord struct {
	Index              int         `json:"index"`
	PodName            string      `json:"podName"`
	ContainerName      string      `json:"containerName"`
	CheckpointPath     string      `json:"checkpointPath"`
	SnapshotTimestamp  metav1.Time `json:"snapshotTimestamp"`
	SHA256Hash         string      `json:"sha256Hash,omitempty"`
	PreviousSHA256Hash string      `json:"previousSHA256Hash,omitempty"`
}

// ForensicSnapshotChainSpec defines the desired state of ForensicSnapshotChain
type ForensicSnapshotChainSpec struct {
	// Namespace is the namespace containing the selected pods
	Namespace string `json:"namespace"`

	//Selector identifies the target pods
	Selector metav1.LabelSelector `json:"selector"`

	//ContainerNames restricts the snapshotiing to specific containers
	// +optional
	ContainerNames []string `json:"containerNames,omitempty"`
	// Capture defines snapshot collection
	Capture CaptureSpec `json:"capture"`

	// Integrity defines integrity verification
	// +optional
	Integrity IntegritySpec `json:"integrity,omitempty"`

	// +kubebuilder:validation:Enum=None;DeletePod
	// +kubebuilder:default=None
	PostSnapshotAction PostSnapshotAction `json:"postSnapshotAction,omitempty"`
}

// ForensicSnapshotChainStatus defines the observed state of ForensicSnapshotChain.
type ForensicSnapshotChainStatus struct {
	Phase          SnapshotChainPhase `json:"phase,omitempty"`
	SnapshotCount  int                `json:"snapshotCount,omitempty"`
	StartTime      *metav1.Time       `json:"startTime,omitempty"`
	CompletionTime *metav1.Time       `json:"completionTime,omitempty"`
	ErrorMessage   string             `json:"errorMessage,omitempty"`
	// Conditions represent the latest observations of the snapshot chain state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// SnapshotRecords is a list of snapshot records
	// +optional
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

func init() {
	SchemeBuilder.Register(&ForensicSnapshotChain{}, &ForensicSnapshotChainList{})
}
