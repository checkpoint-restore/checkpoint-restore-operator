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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// CheckpointRestoreOperatorSpec defines the desired state of CheckpointRestoreOperator
type CheckpointRestoreOperatorSpec struct {
	// checkpointDirectory is the directory on each node where checkpoint
	// archives are stored.
	// +optional
	CheckpointDirectory string `json:"checkpointDirectory,omitempty"`
	// applyPoliciesImmediately controls whether garbage-collection policies
	// are applied as soon as they change, rather than only on the next
	// checkpoint event.
	// +optional
	ApplyPoliciesImmediately bool `json:"applyPoliciesImmediately,omitempty"`
	// globalPolicy defines the retention limits applied to all checkpoints
	// that are not matched by a more specific policy.
	// +optional
	GlobalPolicies GlobalPolicySpec `json:"globalPolicy,omitempty"`
	// containerPolicies defines retention limits for checkpoints of specific
	// containers.
	// +optional
	// +listType=atomic
	ContainerPolicies []ContainerPolicySpec `json:"containerPolicies,omitempty"`
	// podPolicies defines retention limits for checkpoints of specific pods.
	// +optional
	// +listType=atomic
	PodPolicies []PodPolicySpec `json:"podPolicies,omitempty"`
	// namespacePolicies defines retention limits for checkpoints in specific
	// namespaces.
	// +optional
	// +listType=atomic
	NamespacePolicies []NamespacePolicySpec `json:"namespacePolicies,omitempty"`
	// externalStorage configures the S3-compatible backend that the
	// checkpoint-syncer component uploads opted-in checkpoints to. Leaving
	// this unset disables external storage entirely; it has no effect on
	// local checkpoint creation, retention, or restore.
	// +optional
	ExternalStorage *ExternalStorageSpec `json:"externalStorage,omitempty"`
}

// ExternalStorageSpec configures the S3-compatible object storage backend
// used by the checkpoint-syncer component. It is read only by the syncer -
// the main controller-manager and cri-proxy never use these credentials.
type ExternalStorageSpec struct {
	// backend selects the storage backend. Only "s3" is supported initially,
	// but any S3-compatible endpoint (AWS, MinIO, etc.) works through it.
	// +required
	// +kubebuilder:validation:Enum=s3
	Backend string `json:"backend"`
	// bucket is the destination bucket for uploaded checkpoint archives.
	// +required
	Bucket string `json:"bucket"`
	// endpoint overrides the default AWS endpoint, for S3-compatible providers.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// region is the bucket's region.
	// +optional
	Region string `json:"region,omitempty"`
	// secretRef names a Secret (in the syncer's namespace) holding
	// credentials (access key / secret key, or provider-specific fields).
	// +required
	SecretRef corev1.LocalObjectReference `json:"secretRef"`
}

type GlobalPolicySpec struct {
	// retainOrphan controls whether checkpoints are kept after their source
	// container, pod or namespace no longer exists.
	// +optional
	RetainOrphan *bool `json:"retainOrphan,omitempty"`
	// maxCheckpointsPerNamespace is the maximum number of checkpoints to
	// retain per namespace.
	// +optional
	MaxCheckpointsPerNamespaces *int `json:"maxCheckpointsPerNamespace,omitempty"`
	// maxCheckpointsPerPod is the maximum number of checkpoints to retain per
	// pod.
	// +optional
	MaxCheckpointsPerPod *int `json:"maxCheckpointsPerPod,omitempty"`
	// maxCheckpointsPerContainer is the maximum number of checkpoints to
	// retain per container.
	// +optional
	MaxCheckpointsPerContainer *int `json:"maxCheckpointsPerContainer,omitempty"`
	// maxCheckpointSize is the maximum size of a single checkpoint archive.
	// +optional
	MaxCheckpointSize *resource.Quantity `json:"maxCheckpointSize,omitempty"`
	// maxTotalSizePerNamespace is the maximum combined size of checkpoints
	// retained per namespace.
	// +optional
	MaxTotalSizePerNamespace *resource.Quantity `json:"maxTotalSizePerNamespace,omitempty"`
	// maxTotalSizePerPod is the maximum combined size of checkpoints retained
	// per pod.
	// +optional
	MaxTotalSizePerPod *resource.Quantity `json:"maxTotalSizePerPod,omitempty"`
	// maxTotalSizePerContainer is the maximum combined size of checkpoints
	// retained per container.
	// +optional
	MaxTotalSizePerContainer *resource.Quantity `json:"maxTotalSizePerContainer,omitempty"`
	// uploadToExternalStorage opts checkpoints matched by this policy into
	// external storage sync, performed by the checkpoint-syncer component.
	// Defaults to false: existing policies are unaffected until set.
	// +optional
	UploadToExternalStorage *bool `json:"uploadToExternalStorage,omitempty"`
}

type ContainerPolicySpec struct {
	// namespace is the namespace of the container this policy applies to.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// pod is the name of the pod this policy applies to.
	// +optional
	Pod string `json:"pod,omitempty"`
	// container is the name of the container this policy applies to.
	// +optional
	Container string `json:"container,omitempty"`
	// retainOrphan controls whether checkpoints are kept after the container
	// no longer exists.
	// +optional
	RetainOrphan *bool `json:"retainOrphan,omitempty"`
	// maxCheckpoints is the maximum number of checkpoints to retain for the
	// container.
	// +optional
	MaxCheckpoints *int `json:"maxCheckpoints,omitempty"`
	// maxCheckpointSize is the maximum size of a single checkpoint archive for
	// the container.
	// +optional
	MaxCheckpointSize *resource.Quantity `json:"maxCheckpointSize,omitempty"`
	// maxTotalSize is the maximum combined size of checkpoints retained for the
	// container.
	// +optional
	MaxTotalSize *resource.Quantity `json:"maxTotalSize,omitempty"`
	// uploadToExternalStorage opts checkpoints matched by this policy into
	// external storage sync, performed by the checkpoint-syncer component.
	// Defaults to false: existing policies are unaffected until set.
	// +optional
	UploadToExternalStorage *bool `json:"uploadToExternalStorage,omitempty"`
}

type PodPolicySpec struct {
	// namespace is the namespace of the pod this policy applies to.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// pod is the name of the pod this policy applies to.
	// +optional
	Pod string `json:"pod,omitempty"`
	// retainOrphan controls whether checkpoints are kept after the pod no
	// longer exists.
	// +optional
	RetainOrphan *bool `json:"retainOrphan,omitempty"`
	// maxCheckpoints is the maximum number of checkpoints to retain for the
	// pod.
	// +optional
	MaxCheckpoints *int `json:"maxCheckpoints,omitempty"`
	// maxCheckpointSize is the maximum size of a single checkpoint archive for
	// the pod.
	// +optional
	MaxCheckpointSize *resource.Quantity `json:"maxCheckpointSize,omitempty"`
	// maxTotalSize is the maximum combined size of checkpoints retained for the
	// pod.
	// +optional
	MaxTotalSize *resource.Quantity `json:"maxTotalSize,omitempty"`
	// uploadToExternalStorage opts checkpoints matched by this policy into
	// external storage sync, performed by the checkpoint-syncer component.
	// Defaults to false: existing policies are unaffected until set.
	// +optional
	UploadToExternalStorage *bool `json:"uploadToExternalStorage,omitempty"`
}

type NamespacePolicySpec struct {
	// namespace is the namespace this policy applies to.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// retainOrphan controls whether checkpoints are kept after their source no
	// longer exists.
	// +optional
	RetainOrphan *bool `json:"retainOrphan,omitempty"`
	// maxCheckpoints is the maximum number of checkpoints to retain in the
	// namespace.
	// +optional
	MaxCheckpoints *int `json:"maxCheckpoints,omitempty"`
	// maxCheckpointSize is the maximum size of a single checkpoint archive in
	// the namespace.
	// +optional
	MaxCheckpointSize *resource.Quantity `json:"maxCheckpointSize,omitempty"`
	// maxTotalSize is the maximum combined size of checkpoints retained in the
	// namespace.
	// +optional
	MaxTotalSize *resource.Quantity `json:"maxTotalSize,omitempty"`
	// uploadToExternalStorage opts checkpoints matched by this policy into
	// external storage sync, performed by the checkpoint-syncer component.
	// Defaults to false: existing policies are unaffected until set.
	// +optional
	UploadToExternalStorage *bool `json:"uploadToExternalStorage,omitempty"`
}

// CheckpointRestoreOperatorStatus defines the observed state of CheckpointRestoreOperator
type CheckpointRestoreOperatorStatus struct {
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// CheckpointRestoreOperator is the Schema for the checkpointrestoreoperators API
type CheckpointRestoreOperator struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of CheckpointRestoreOperator.
	// +required
	Spec CheckpointRestoreOperatorSpec `json:"spec"`
	// status defines the observed state of CheckpointRestoreOperator.
	// +optional
	Status CheckpointRestoreOperatorStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// CheckpointRestoreOperatorList contains a list of CheckpointRestoreOperator
type CheckpointRestoreOperatorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CheckpointRestoreOperator `json:"items"`
}
