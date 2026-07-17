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

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// CheckpointArchiveSpec records where a single checkpoint archive was created.
// One object is created per archive that a retention policy has opted into
// external storage sync for (see GlobalPolicySpec.UploadToExternalStorage);
// checkpoints outside that scope have no CheckpointArchive at all.
type CheckpointArchiveSpec struct {
	// node is the node the archive was created on and currently lives on.
	// +required
	// +kubebuilder:validation:MinLength=1
	Node string `json:"node"`
	// localPath is the absolute path of the archive on node's disk at the
	// time this object was created. It is not re-validated after creation;
	// the local garbage collector remains the sole authority on whether the
	// file still exists there.
	// +required
	// +kubebuilder:validation:Pattern=`^/.*\.tar$`
	LocalPath string `json:"localPath"`
	// namespace is the namespace of the pod the archive was taken from.
	// +required
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// pod is the name of the pod the archive was taken from.
	// +required
	// +kubebuilder:validation:MinLength=1
	Pod string `json:"pod"`
	// container is the name of the container the archive was taken from.
	// +required
	// +kubebuilder:validation:MinLength=1
	Container string `json:"container"`
	// requestedNodes lists nodes that need a local copy of this archive staged
	// for an imminent restore. PodRestoreReconciler appends its target node;
	// the checkpoint-syncer on each listed node downloads the archive locally.
	// +optional
	// +listType=set
	RequestedNodes []string `json:"requestedNodes,omitempty"`
}

// Condition types for a CheckpointArchive.
const (
	// ConditionArchiveUploaded reports whether the checkpoint-syncer has
	// uploaded the archive to external storage. False until it has.
	ConditionArchiveUploaded = "Uploaded"
)

// CheckpointArchiveStatus defines the observed state of CheckpointArchive.
type CheckpointArchiveStatus struct {
	// conditions represent the latest observations of the archive's storage
	// state. See ConditionArchiveUploaded.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
	// externalURI is the object storage location (e.g. s3://bucket/key.tar)
	// once the checkpoint-syncer has uploaded the archive. Empty until then.
	// +optional
	ExternalURI string `json:"externalURI,omitempty"`
	// availableNodes lists nodes where a local copy of the archive currently
	// exists. The origin node (spec.node) is listed at creation; a syncer
	// appends its node after a successful download.
	// +optional
	// +listType=set
	AvailableNodes []string `json:"availableNodes,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.node`
//+kubebuilder:printcolumn:name="Uploaded",type=string,JSONPath=`.status.conditions[?(@.type=="Uploaded")].status`
//+kubebuilder:printcolumn:name="ExternalURI",type=string,JSONPath=`.status.externalURI`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CheckpointArchive is the Schema for the checkpointarchives API. It records
// a single checkpoint archive that a retention policy opted into external
// storage sync, tracking where it lives locally and, once uploaded, where it
// lives in object storage.
type CheckpointArchive struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of CheckpointArchive.
	// +required
	Spec CheckpointArchiveSpec `json:"spec"`
	// status defines the observed state of CheckpointArchive.
	// +optional
	Status CheckpointArchiveStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// CheckpointArchiveList contains a list of CheckpointArchive.
type CheckpointArchiveList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CheckpointArchive `json:"items"`
}
