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

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// CheckpointScheduleSpec defines which pods to checkpoint and what should
// initiate a checkpoint.
type CheckpointScheduleSpec struct {
	// namespace is the namespace in which pods are selected.
	// +required
	Namespace string `json:"namespace"`
	// selector selects the pods to checkpoint by label. Only pods in the
	// Running phase are checkpointed.
	// +required
	Selector metav1.LabelSelector `json:"selector"`
	// containerNames restricts checkpointing to the named containers of a
	// matching pod. When empty, all containers are checkpointed.
	// +optional
	// +listType=atomic
	ContainerNames []string `json:"containerNames,omitempty"`
	// triggers describes what initiates a checkpoint. Multiple triggers can
	// be combined.
	// +required
	Triggers TriggersSpec `json:"triggers"`
}

// TriggersSpec describes what initiates a checkpoint.
type TriggersSpec struct {
	// interval is the time between periodic checkpoints of the matching
	// pods, expressed as a duration string (e.g. "30s", "15m", "12h"). The
	// first checkpoint is taken one interval after the CheckpointSchedule
	// is created.
	// +optional
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')",message="interval must be at least 1s"
	Interval *metav1.Duration `json:"interval,omitempty"`
	// resourceThreshold checkpoints a container when its CPU or memory usage
	// crosses a configured percentage of its resource limit. Requires the
	// Kubernetes Metrics API (metrics-server).
	// +optional
	ResourceThreshold *ResourceThresholdSpec `json:"resourceThreshold,omitempty"`
	// onKubernetesEvents lists pod disruption signals that trigger a
	// checkpoint: NodeDrain (the pod's node is marked unschedulable),
	// PodEviction (the pod is being deleted) or Preemption (the pod has the
	// DisruptionTarget condition, Kubernetes 1.26+).
	// +optional
	// +listType=atomic
	OnKubernetesEvents []string `json:"onKubernetesEvents,omitempty"`
	// onAnnotation enables on-demand checkpoints: annotating a matching pod
	// with checkpoint.criu.org/trigger=true checkpoints it once and removes
	// the annotation.
	// +optional
	OnAnnotation bool `json:"onAnnotation,omitempty"`
}

// ResourcePercentThreshold defines upper and lower percentage bounds for a
// resource, relative to the container's resource limit.
type ResourcePercentThreshold struct {
	// upper triggers a checkpoint when usage exceeds this percentage of the
	// container's limit.
	// +optional
	Upper *int `json:"upper,omitempty"`
	// lower triggers a checkpoint when usage drops below this percentage of
	// the container's limit.
	// +optional
	Lower *int `json:"lower,omitempty"`
}

// ResourceThresholdSpec configures resource-usage based checkpointing.
// Containers without a resource limit are skipped.
type ResourceThresholdSpec struct {
	// cpuPercent defines CPU usage bounds relative to the container's limit.
	// +optional
	CPUPercent *ResourcePercentThreshold `json:"cpuPercent,omitempty"`
	// memoryPercent defines memory usage bounds relative to the container's
	// limit.
	// +optional
	MemoryPercent *ResourcePercentThreshold `json:"memoryPercent,omitempty"`
	// pollIntervalSeconds is the number of seconds between metrics polls.
	// Defaults to 30.
	// +optional
	PollIntervalSeconds *int `json:"pollIntervalSeconds,omitempty"`
}

// CheckpointScheduleStatus defines the observed state of CheckpointSchedule
type CheckpointScheduleStatus struct {
	// conditions represent the latest available observations of the
	// schedule's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
	// lastCheckpointTime is when the interval trigger last checkpointed the
	// matching pods.
	// +optional
	LastCheckpointTime *metav1.Time `json:"lastCheckpointTime,omitempty"`
	// checkpointsCreated is the number of checkpoints created by the
	// interval trigger.
	// +optional
	CheckpointsCreated int `json:"checkpointsCreated,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// CheckpointSchedule is the Schema for the checkpointschedules API
type CheckpointSchedule struct {
	metav1.TypeMeta `json:",inline"`
	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of CheckpointSchedule.
	// +required
	Spec CheckpointScheduleSpec `json:"spec"`
	// status defines the observed state of CheckpointSchedule.
	// +optional
	Status CheckpointScheduleStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// CheckpointScheduleList contains a list of CheckpointSchedule
type CheckpointScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CheckpointSchedule `json:"items"`
}
