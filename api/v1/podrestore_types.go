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
	"fmt"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// RestoreCheckpointPathAnnotationPrefix is the per-container annotation prefix the
// node-side restore mechanism (CRI proxy or OCI-runtime wrapper) reads to learn
// the checkpoint archive a given container should be restored from. The full key
// is this prefix followed by the container name, e.g.
// "restore.criu.org/checkpoint-path.web". A container without such an annotation
// is created normally.
const RestoreCheckpointPathAnnotationPrefix = "restore.criu.org/checkpoint-path."

// DefaultCheckpointDir is the directory in which the kubelet checkpoint API
// writes checkpoint archives. Both the PodRestore controller and the CRI
// proxy confine restore paths to a single checkpoint directory and use this
// as the default when none is configured.
const DefaultCheckpointDir = "/var/lib/kubelet/checkpoints"

// ValidateCheckpointPath rejects paths that are unsafe to hand to the node-side
// restore mechanism: it requires an absolute, lexically-clean path (no "." or
// ".." traversal, no redundant separators) ending in .tar. It lives in the API
// package so that every enforcement point applies exactly the same rule: the
// PodRestore controller (before it stamps the annotation) and the CRI proxy
// (before it hands the path to the runtime as a container image). The proxy must
// not trust the annotation blindly — admission and RBAC are the primary controls,
// but the proxy is the last gate at the node and validates independently.
func ValidateCheckpointPath(p string) error {
	if p == "" {
		return fmt.Errorf("checkpoint path is empty")
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("checkpoint path %q must be absolute", p)
	}
	if filepath.Clean(p) != p {
		return fmt.Errorf("checkpoint path %q must be clean (no '.', '..', or redundant separators)", p)
	}
	if filepath.Ext(p) != ".tar" {
		return fmt.Errorf("checkpoint path %q must be a .tar archive", p)
	}
	return nil
}

// ValidateCheckpointPathInDir extends ValidateCheckpointPath with directory
// confinement: the archive must live directly inside allowedDir (no
// subdirectories). This is what stops a restore from being pointed at an
// arbitrary .tar elsewhere on the node's filesystem — for example one an
// attacker staged through a hostPath or emptyDir mount. An empty allowedDir
// means "use DefaultCheckpointDir"; confinement is always enforced.
func ValidateCheckpointPathInDir(p, allowedDir string) error {
	if err := ValidateCheckpointPath(p); err != nil {
		return err
	}
	if allowedDir == "" {
		allowedDir = DefaultCheckpointDir
	}
	if filepath.Dir(p) != filepath.Clean(allowedDir) {
		return fmt.Errorf("checkpoint path %q is not inside the checkpoint directory %q", p, allowedDir)
	}
	return nil
}

// ValidateCheckpointNamespaceHint checks the archive filename against the
// kubelet checkpoint naming convention,
//
//	checkpoint-<pod>_<namespace>-<container>-<timestamp>.tar
//
// and rejects archives whose recorded namespace cannot be ns. Pod names
// cannot contain "_", so the pod/namespace split is unambiguous; but
// namespaces and container names may both contain "-", so the namespace
// field itself is only prefix-checkable. This is therefore a coarse,
// API-side gate: it reliably rejects archives from unrelated namespaces, but
// a namespace that is a "-"-prefix of another (team vs team-prod) passes it.
// The authoritative check happens at the node, where the CRI proxy compares
// the namespace recorded *inside* the archive with the namespace of the pod
// sandbox being created.
func ValidateCheckpointNamespaceHint(p, ns string) error {
	base := filepath.Base(p)
	rest, ok := strings.CutPrefix(base, "checkpoint-")
	if !ok {
		return fmt.Errorf("checkpoint archive %q does not follow the kubelet naming convention checkpoint-<pod>_<namespace>-<container>-<timestamp>.tar", base)
	}
	_, after, ok := strings.Cut(rest, "_")
	if !ok {
		return fmt.Errorf("checkpoint archive %q does not follow the kubelet naming convention checkpoint-<pod>_<namespace>-<container>-<timestamp>.tar", base)
	}
	if !strings.HasPrefix(after, ns+"-") {
		return fmt.Errorf("checkpoint archive %q does not belong to namespace %q", base, ns)
	}
	return nil
}

// ContainerCheckpoint maps a container in the Pod template to the checkpoint
// archive it should be restored from.
type ContainerCheckpoint struct {
	// Container is the name of the container in the template to restore.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Container string `json:"container"`
	// Path is the absolute path to the checkpoint .tar archive on the target node.
	// +kubebuilder:validation:Pattern=`^/.*\.tar$`
	// +kubebuilder:validation:MaxLength=4096
	Path string `json:"path"`
}

// PodRestoreSpec defines the desired state of PodRestore.
type PodRestoreSpec struct {
	// TargetNode is the node that holds the checkpoint archives. The restored Pod
	// is pinned to this node because the archives are node-local. It is immutable:
	// a restore targets one node's archives and cannot be repointed in place.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="targetNode is immutable"
	TargetNode string `json:"targetNode"`

	// Checkpoints maps each container to restore to its on-node checkpoint archive.
	// Containers in the template that are not listed here are started normally.
	// The list is keyed by container name, so each container may appear once. It is
	// immutable: editing it after the restore Pod exists has no effect, so changes
	// are rejected to avoid a misleading spec.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=container
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="checkpoints is immutable"
	Checkpoints []ContainerCheckpoint `json:"checkpoints"`

	// Template is the PodTemplateSpec describing the restored workload. The
	// controller injects the target node, the per-container restore annotation,
	// and, for restored containers whose image is left empty, the base image
	// recorded in the checkpoint (needed only to satisfy the kubelet image-pull
	// gate; it plays no role in the actual restore).
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="template is immutable"
	Template corev1.PodTemplateSpec `json:"template"`
}

// Condition types for a PodRestore. Following Kubernetes API conventions, the
// restore's state is expressed through conditions rather than a phase enum.
const (
	// ConditionReady is the summary condition: True once the restored Pod is
	// running. Its reason distinguishes the in-progress and failure states
	// (Restoring, RenderFailed, NodeNotFound, PodConflict, PodFailed, PodMissing,
	// InvalidSpec, Restored).
	ConditionReady = "Ready"
	// ConditionCheckpointsPinned reports whether the source checkpoints are
	// actually pinned against the retention garbage collector. It is False (not an
	// error) when the archive is not reachable from the operator, the normal
	// cross-node case.
	ConditionCheckpointsPinned = "CheckpointsPinned"
)

// PodRestoreStatus defines the observed state of PodRestore.
type PodRestoreStatus struct {
	// PodName is the name of the Pod created for this restore.
	// +optional
	PodName string `json:"podName,omitempty"`
	// Conditions represent the latest observations of the restore state. The
	// "Ready" condition is the summary; "CheckpointsPinned" reports retention
	// pinning. Read the condition reason/message for detail rather than a phase.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.targetNode`
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=`.status.podName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PodRestore is the Schema for the podrestores API.
type PodRestore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PodRestoreSpec   `json:"spec,omitempty"`
	Status PodRestoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PodRestoreList contains a list of PodRestore.
type PodRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodRestore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PodRestore{}, &PodRestoreList{})
}
