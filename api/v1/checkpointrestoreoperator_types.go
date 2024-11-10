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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// CheckpointRestoreOperatorSpec defines the desired state of CheckpointRestoreOperator
type CheckpointRestoreOperatorSpec struct {
	// Important: Run "make" to regenerate code after modifying this file
	CheckpointDirectory      string                `json:"checkpointDirectory,omitempty"`
	ApplyPoliciesImmediately bool                  `json:"applyPoliciesImmediately,omitempty"`
	GlobalPolicies           GlobalPolicySpec      `json:"globalPolicy,omitempty"`
	ContainerPolicies        []ContainerPolicySpec `json:"containerPolicies,omitempty"`
	PodPolicies              []PodPolicySpec       `json:"podPolicies,omitempty"`
	NamespacePolicies        []NamespacePolicySpec `json:"namespacePolicies,omitempty"`
}

type GlobalPolicySpec struct {
	MaxCheckpointsPerNamespaces *int               `json:"maxCheckpointsPerNamespace,omitempty"`
	MaxCheckpointsPerPod        *int               `json:"maxCheckpointsPerPod,omitempty"`
	MaxCheckpointsPerContainer  *int               `json:"maxCheckpointsPerContainer,omitempty"`
	MaxCheckpointSize           *resource.Quantity `json:"maxCheckpointSize,omitempty"`
	MaxTotalSizePerNamespace    *resource.Quantity `json:"maxTotalSizePerNamespace,omitempty"`
	MaxTotalSizePerPod          *resource.Quantity `json:"maxTotalSizePerPod,omitempty"`
	MaxTotalSizePerContainer    *resource.Quantity `json:"maxTotalSizePerContainer,omitempty"`
}

type ContainerPolicySpec struct {
	Namespace         string             `json:"namespace,omitempty"`
	Pod               string             `json:"pod,omitempty"`
	Container         string             `json:"container,omitempty"`
	MaxCheckpoints    *int               `json:"maxCheckpoints,omitempty"`
	MaxCheckpointSize *resource.Quantity `json:"maxCheckpointSize,omitempty"`
	MaxTotalSize      *resource.Quantity `json:"maxTotalSize,omitempty"`
}

type PodPolicySpec struct {
	Namespace         string             `json:"namespace,omitempty"`
	Pod               string             `json:"pod,omitempty"`
	MaxCheckpoints    *int               `json:"maxCheckpoints,omitempty"`
	MaxCheckpointSize *resource.Quantity `json:"maxCheckpointSize,omitempty"`
	MaxTotalSize      *resource.Quantity `json:"maxTotalSize,omitempty"`
}

type NamespacePolicySpec struct {
	Namespace         string             `json:"namespace,omitempty"`
	MaxCheckpoints    *int               `json:"maxCheckpoints,omitempty"`
	MaxCheckpointSize *resource.Quantity `json:"maxCheckpointSize,omitempty"`
	MaxTotalSize      *resource.Quantity `json:"maxTotalSize,omitempty"`
}

// CheckpointRestoreOperatorStatus defines the observed state of CheckpointRestoreOperator
type CheckpointRestoreOperatorStatus struct {
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// CheckpointRestoreOperator is the Schema for the checkpointrestoreoperators API
type CheckpointRestoreOperator struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CheckpointRestoreOperatorSpec   `json:"spec,omitempty"`
	Status CheckpointRestoreOperatorStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// CheckpointRestoreOperatorList contains a list of CheckpointRestoreOperator
type CheckpointRestoreOperatorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CheckpointRestoreOperator `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CheckpointRestoreOperator{}, &CheckpointRestoreOperatorList{})
}
