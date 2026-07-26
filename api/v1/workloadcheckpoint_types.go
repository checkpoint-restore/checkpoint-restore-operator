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

// WorkloadCheckpointPhase is the high-level state of a one-shot workload checkpoint run.
type WorkloadCheckpointPhase string

const (
	WorkloadPhasePending         WorkloadCheckpointPhase = "Pending"
	WorkloadPhaseRunning         WorkloadCheckpointPhase = "Running"
	WorkloadPhaseCompleted       WorkloadCheckpointPhase = "Completed"
	WorkloadPhasePartiallyFailed WorkloadCheckpointPhase = "PartiallyFailed"
	WorkloadPhaseFailed          WorkloadCheckpointPhase = "Failed"
)

// WorkloadCheckpointFailurePolicy controls how a per-pod checkpoint failure
// affects the overall run.
type WorkloadCheckpointFailurePolicy string

const (
	// FailurePolicyBestEffort checkpoints as many pods as possible and reports
	// failures per-pod; the run ends Completed (all ok) or PartiallyFailed.
	FailurePolicyBestEffort WorkloadCheckpointFailurePolicy = "BestEffort"
	// FailurePolicyAllOrNothing marks the whole run Failed if any pod fails.
	FailurePolicyAllOrNothing WorkloadCheckpointFailurePolicy = "AllOrNothing"
)

// WorkloadReference names the workload object to checkpoint. It is resolved to
// the pods that object owns.
type WorkloadReference struct {
	// kind is the workload kind to checkpoint.
	// +required
	// +kubebuilder:validation:Enum=Job;Deployment;StatefulSet
	Kind string `json:"kind"`

	// name is the workload object name, in the same namespace as this resource.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// apiVersion is the workload apiVersion (e.g. apps/v1, batch/v1). Optional;
	// defaulted per kind by the controller when empty.
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`
}

// WorkloadCheckpointSpec defines the desired state of WorkloadCheckpoint.
type WorkloadCheckpointSpec struct {
	// targetRef identifies the workload whose pods will be checkpointed.
	// It is immutable: a WorkloadCheckpoint targets one workload for its lifetime.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="targetRef is immutable"
	TargetRef WorkloadReference `json:"targetRef"`

	// containerNames restricts checkpointing to specific containers.
	// Empty means all containers of each pod.
	// +optional
	// +listType=atomic
	ContainerNames []string `json:"containerNames,omitempty"`

	// failurePolicy controls how a per-pod checkpoint failure affects the run.
	// +optional
	// +kubebuilder:validation:Enum=BestEffort;AllOrNothing
	// +default="BestEffort"
	FailurePolicy WorkloadCheckpointFailurePolicy `json:"failurePolicy,omitempty"`
}

// ContainerCheckpointRecord records the outcome for one container.
type ContainerCheckpointRecord struct {
	// container is the container name.
	// +required
	Container string `json:"container"`
	// path is the checkpoint archive path on the node (empty if this container failed).
	// +optional
	Path string `json:"path,omitempty"`
	// checkpointTime is when this container was checkpointed.
	// +optional
	CheckpointTime metav1.Time `json:"checkpointTime,omitempty"`
	// error holds the failure message for this container, if any.
	// +optional
	Error string `json:"error,omitempty"`
}

// PodCheckpointRecord records the outcome for one pod (one workload replica).
type PodCheckpointRecord struct {
	// podName is the pod that was checkpointed.
	// +required
	PodName string `json:"podName"`
	// nodeName is the node the pod (and its archives) live on.
	// +optional
	NodeName string `json:"nodeName,omitempty"`
	// replicaKey identifies the replica within the workload (the pod name, which
	// encodes the ordinal for StatefulSets).
	// +optional
	ReplicaKey string `json:"replicaKey,omitempty"`
	// containers holds the per-container checkpoint results for this pod.
	// +optional
	// +listType=atomic
	Containers []ContainerCheckpointRecord `json:"containers,omitempty"`
}

// WorkloadCheckpointStatus defines the observed state of WorkloadCheckpoint.
type WorkloadCheckpointStatus struct {
	// conditions represent the latest observations of the run state (type "Ready").
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
	// phase is the current high-level state of the run.
	// +optional
	Phase WorkloadCheckpointPhase `json:"phase,omitempty"`
	// observedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// startTime is when the resource entered the Pending phase.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// completionTime is when the run reached a terminal phase.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// resolvedPodCount is how many owned Running pods were resolved.
	// +optional
	ResolvedPodCount int32 `json:"resolvedPodCount,omitempty"`
	// checkpointedPodCount is how many pods had all selected containers checkpointed.
	// +optional
	CheckpointedPodCount int32 `json:"checkpointedPodCount,omitempty"`
	// failedPodCount is how many pods had at least one container fail.
	// +optional
	FailedPodCount int32 `json:"failedPodCount,omitempty"`
	// checkpoints is the coherent group snapshot, one entry per pod.
	// +optional
	// +listType=atomic
	Checkpoints []PodCheckpointRecord `json:"checkpoints,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.targetRef.kind`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Pods",type=integer,JSONPath=`.status.checkpointedPodCount`

// WorkloadCheckpoint is the Schema for the workloadcheckpoints API.
type WorkloadCheckpoint struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of WorkloadCheckpoint
	// +required
	Spec WorkloadCheckpointSpec `json:"spec"`

	// status defines the observed state of WorkloadCheckpoint
	// +optional
	Status WorkloadCheckpointStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WorkloadCheckpointList contains a list of WorkloadCheckpoint.
type WorkloadCheckpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []WorkloadCheckpoint `json:"items"`
}
