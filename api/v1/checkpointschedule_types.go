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

type CheckpointIntent string

const (
	Backup           CheckpointIntent = "Backup"
	PreEviction      CheckpointIntent = "PreEviction"
	ResourcePressure CheckpointIntent = "ResourcePressure"
	Manual           CheckpointIntent = "Manual"
)

type CheckpointScheduleSpec struct {
	Namespace      string               `json:"namespace"`
	Selector       metav1.LabelSelector `json:"selector"`
	ContainerNames []string             `json:"containerNames,omitempty"`
	Intent         CheckpointIntent     `json:"intent"`
	Triggers       TriggersSpec         `json:"triggers"`
}

type TriggersSpec struct {
	Schedule           string                 `json:"schedule,omitempty"`
	ResourceThreshold  *ResourceThresholdSpec `json:"resourceThreshold,omitempty"`
	OnKubernetesEvents []string               `json:"onKubernetesEvents,omitempty"`
	OnAnnotation       bool                   `json:"onAnnotation,omitempty"`
}

type ResourceThresholdSpec struct {
	CPUPercent          *int `json:"cpuPercent,omitempty"`
	MemoryPercent       *int `json:"memoryPercent,omitempty"`
	PollIntervalSeconds *int `json:"pollIntervalSeconds,omitempty"`
}

// CheckpointScheduleStatus defines the observed state of CheckpointSchedule
type CheckpointScheduleStatus struct {
	LastCheckpointTime *metav1.Time       `json:"lastCheckpointTime,omitempty"`
	CheckpointsCreated int                `json:"checkpointsCreated,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// CheckpointSchedule is the Schema for the checkpointschedules API
type CheckpointSchedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CheckpointScheduleSpec   `json:"spec,omitempty"`
	Status CheckpointScheduleStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// CheckpointScheduleList contains a list of CheckpointSchedule
type CheckpointScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CheckpointSchedule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CheckpointSchedule{}, &CheckpointScheduleList{})
}
