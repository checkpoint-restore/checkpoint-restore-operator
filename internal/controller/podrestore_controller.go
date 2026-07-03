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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	"github.com/checkpoint-restore/checkpoint-restore-operator/internal/checkpointarchive"
)

const (
	podRestoreFinalizer = "criu.org/pod-restore-finalizer"
	// podRestoreLabel links a restored Pod back to its PodRestore.
	podRestoreLabel = "restore.criu.org/pod-restore"

	podRestorePinManager = "checkpoint-restore-operator/podrestore"
)

var podRestorePinMu sync.Mutex

type podRestorePinMarker struct {
	ManagedBy string   `json:"managedBy"`
	Owners    []string `json:"podRestoreOwners,omitempty"`
}

// PodRestoreReconciler reconciles a PodRestore object into an ordinary, node-pinned
// Pod annotated for the node-side restore mechanism. It is intentionally agnostic
// to how the node bridges "local .tar -> container image" (CRI proxy, OCI-runtime
// wrapper, or local OCI import): it only renders the Pod and tracks its state.
type PodRestoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// CheckpointDir is the only directory PodRestore specs may reference
	// checkpoint archives in. Empty means criuorgv1.DefaultCheckpointDir;
	// confinement is always enforced so the zero value fails secure. It must
	// match the directory the kubelet writes checkpoints to (and the CRI
	// proxy's --checkpoint-dir) if that was customized.
	CheckpointDir string

	// AllowCrossNamespace disables the archive-filename namespace check, which
	// otherwise requires a checkpoint to have been taken in the PodRestore's
	// own namespace. It is an operator-level (cluster admin) knob for
	// deliberate cross-namespace restores; the node-side CRI proxy has an
	// equivalent --allow-cross-namespace flag that must also be set, because
	// the proxy independently verifies the namespace recorded inside the
	// archive.
	AllowCrossNamespace bool
}

// +kubebuilder:rbac:groups=criu.org,resources=podrestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=criu.org,resources=podrestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=criu.org,resources=podrestores/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile drives a PodRestore. It is level-based: it re-derives the restore's
// state from the world (spec, target node, source archives, and the restore Pod)
// on every pass and expresses that state through status conditions rather than a
// stored phase, so drift (a Pod deleted or crashed after it was running) is
// reflected instead of latched.
func (r *PodRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	pr := &criuorgv1.PodRestore{}
	if err := r.Get(ctx, req.NamespacedName, pr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Finalizer changes go out as merge patches that touch only
	// metadata.finalizers, never as full-object updates: a full Update
	// re-serializes spec.template through the Go structs, whose normalization
	// (creationTimestamp: null, resources: {}, ...) differs byte-wise from what
	// the user stored, and the CRD's "template is immutable" CEL rule
	// (self == oldSelf) rejects that as a template change. With a patch the
	// stored spec is untouched, so the rule trivially holds.

	// Handle deletion: unpin the source checkpoints and drop the finalizer. The
	// restored Pod is garbage-collected via its owner reference.
	if !pr.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(pr, podRestoreFinalizer) {
			owner := podRestoreOwnerKey(pr)
			for _, cp := range pr.Spec.Checkpoints {
				unpinCheckpoint(logger, cp.Path, owner)
			}
			patch := client.MergeFrom(pr.DeepCopy())
			controllerutil.RemoveFinalizer(pr, podRestoreFinalizer)
			if err := r.Patch(ctx, pr, patch); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(pr, podRestoreFinalizer) {
		patch := client.MergeFrom(pr.DeepCopy())
		controllerutil.AddFinalizer(pr, podRestoreFinalizer)
		if err := r.Patch(ctx, pr, patch); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Snapshot the status so we only write when something actually changes; this
	// keeps a steady Running Pod (re-observed on every Pod watch event) from
	// bumping resourceVersion and re-triggering reconciliation in a hot loop.
	before := pr.Status.DeepCopy()

	// A malformed spec is not retryable until the user edits it; report and stop.
	if err := r.validateSpec(pr); err != nil {
		setReady(pr, metav1.ConditionFalse, "InvalidSpec", err.Error())
		return r.saveStatus(ctx, pr, before, ctrl.Result{})
	}

	// The target node must exist: NodeName pins the Pod to it and bypasses the
	// scheduler, so a bad node otherwise yields a Pod stuck Pending with no signal.
	// The controller does not watch Nodes, so requeue: a target node that joins
	// later must be picked up without waiting for the cache resync.
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: pr.Spec.TargetNode}, node); err != nil {
		if apierrors.IsNotFound(err) {
			setReady(pr, metav1.ConditionFalse, "NodeNotFound",
				fmt.Sprintf("target node %q not found", pr.Spec.TargetNode))
			return r.saveStatus(ctx, pr, before, ctrl.Result{RequeueAfter: 30 * time.Second})
		}
		return ctrl.Result{}, err
	}

	// Pin the source checkpoints against retention while the restore is in flight.
	// This is best-effort and node-local: cross-node the archive lives on
	// TargetNode and the pin is a no-op, so report whether it was enforced rather
	// than silently implying the source is protected.
	pinnedAll := true
	owner := podRestoreOwnerKey(pr)
	for _, cp := range pr.Spec.Checkpoints {
		if !pinCheckpoint(logger, cp.Path, owner) {
			pinnedAll = false
		}
	}
	if pinnedAll {
		setPinned(pr, metav1.ConditionTrue, "Pinned",
			"Source checkpoints pinned against retention GC")
	} else {
		setPinned(pr, metav1.ConditionFalse, "PinNotEnforced",
			"Checkpoint archive is not reachable from the operator; pinning against retention GC is not enforced. Ensure the checkpoint is retained on "+pr.Spec.TargetNode+" for the duration of the restore.")
	}

	// Ensure the restore Pod exists and is owned by us, then map its state onto the
	// Ready condition.
	pod := &corev1.Pod{}
	err := r.Get(ctx, client.ObjectKey{Namespace: pr.Namespace, Name: pr.Name}, pod)
	switch {
	case apierrors.IsNotFound(err):
		// If we recorded a Pod earlier and it is now gone, that is drift: report it
		// rather than silently re-restore.
		if pr.Status.PodName != "" {
			setReady(pr, metav1.ConditionFalse, "PodMissing", "restore Pod disappeared")
			return r.saveStatus(ctx, pr, before, ctrl.Result{})
		}
		newPod, rerr := r.renderPod(logger, pr)
		if rerr != nil {
			logger.Error(rerr, "failed to render restore Pod")
			setReady(pr, metav1.ConditionFalse, "RenderFailed", rerr.Error())
			// Rendering can fail transiently (the archive is not readable yet);
			// no watch fires when it becomes readable, so poll.
			return r.saveStatus(ctx, pr, before, ctrl.Result{RequeueAfter: 30 * time.Second})
		}
		if cerr := r.Create(ctx, newPod); cerr != nil {
			if !apierrors.IsAlreadyExists(cerr) {
				return ctrl.Result{}, cerr
			}
			// Raced against another writer; re-read and let the ownership check
			// below decide on the next line.
			if gerr := r.Get(ctx, client.ObjectKeyFromObject(newPod), pod); gerr != nil {
				return ctrl.Result{}, gerr
			}
		} else {
			pod = newPod
		}
	case err != nil:
		return ctrl.Result{}, err
	}

	// Adopt the Pod only if we own it; otherwise fail rather than report progress
	// against a Pod we did not create.
	if !metav1.IsControlledBy(pod, pr) {
		setReady(pr, metav1.ConditionFalse, "PodConflict",
			fmt.Sprintf("a Pod named %q already exists and is not owned by this PodRestore", pod.Name))
		return r.saveStatus(ctx, pr, before, ctrl.Result{})
	}
	pr.Status.PodName = pod.Name

	switch pod.Status.Phase {
	case corev1.PodRunning, corev1.PodSucceeded:
		setReady(pr, metav1.ConditionTrue, "Restored", "Restored Pod is running")
		return r.saveStatus(ctx, pr, before, ctrl.Result{})
	case corev1.PodFailed:
		reason := pod.Status.Reason
		if reason == "" {
			reason = "the restore Pod entered the Failed phase"
		}
		setReady(pr, metav1.ConditionFalse, "PodFailed", reason)
		return r.saveStatus(ctx, pr, before, ctrl.Result{})
	default:
		// Still pending/unknown; the Pod watch re-enqueues us, with a poll backstop.
		setReady(pr, metav1.ConditionFalse, "Restoring", "Restore Pod is not yet running")
		return r.saveStatus(ctx, pr, before, ctrl.Result{RequeueAfter: 3 * time.Second})
	}
}

// validateSpec rejects a spec that cannot produce a valid restore Pod, before any
// side effects. It is deterministic, so callers treat its failure as terminal
// until the spec changes. Beyond structural checks it enforces the two access
// controls: checkpoint paths are confined to r.CheckpointDir, and (unless
// AllowCrossNamespace) the archive filename must record the PodRestore's own
// namespace. The CRI proxy re-verifies the namespace authoritatively from the
// data inside the archive.
func (r *PodRestoreReconciler) validateSpec(pr *criuorgv1.PodRestore) error {
	for key := range pr.Spec.Template.Annotations {
		if strings.HasPrefix(key, criuorgv1.RestoreCheckpointPathAnnotationPrefix) {
			return fmt.Errorf("template annotation %q is reserved for the PodRestore controller", key)
		}
	}

	inTemplate := make(map[string]bool, len(pr.Spec.Template.Spec.Containers))
	for i := range pr.Spec.Template.Spec.Containers {
		inTemplate[pr.Spec.Template.Spec.Containers[i].Name] = true
	}
	for _, cp := range pr.Spec.Checkpoints {
		if err := criuorgv1.ValidateCheckpointPathInDir(cp.Path, r.CheckpointDir); err != nil {
			return fmt.Errorf("container %q: %w", cp.Container, err)
		}
		if !r.AllowCrossNamespace {
			if err := criuorgv1.ValidateCheckpointNamespaceHint(cp.Path, pr.Namespace); err != nil {
				return fmt.Errorf("container %q: %w", cp.Container, err)
			}
		}
		if !inTemplate[cp.Container] {
			return fmt.Errorf("container %q is not defined in the Pod template", cp.Container)
		}
	}
	return nil
}

// renderPod builds the restore Pod from the template, pinning it to the target
// node, annotating each restored container with its checkpoint path, and filling
// in the base image from the checkpoint when the template leaves it empty.
func (r *PodRestoreReconciler) renderPod(logger logr.Logger, pr *criuorgv1.PodRestore) (*corev1.Pod, error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: copyStringMap(pr.Spec.Template.Annotations),
			Labels:      copyStringMap(pr.Spec.Template.Labels),
		},
		Spec: *pr.Spec.Template.Spec.DeepCopy(),
	}
	pod.Name = pr.Name
	pod.Namespace = pr.Namespace
	pod.Spec.NodeName = pr.Spec.TargetNode

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[podRestoreLabel] = pr.Name

	// Spec validity (paths, container names) is already checked by validateSpec.
	for _, cp := range pr.Spec.Checkpoints {
		idx := containerIndex(pod.Spec.Containers, cp.Container)
		if idx < 0 {
			return nil, fmt.Errorf("container %q is not defined in the Pod template", cp.Container)
		}
		// The node-side restore mechanism reads this to restore the container.
		pod.Annotations[criuorgv1.RestoreCheckpointPathAnnotationPrefix+cp.Container] = cp.Path

		// The image only satisfies the kubelet's image-pull gate. Prefer an
		// explicit template image; otherwise use the checkpoint's base image.
		if pod.Spec.Containers[idx].Image == "" {
			img, err := readCheckpointBaseImage(logger, cp.Path)
			if err != nil {
				return nil, fmt.Errorf("container %q: %w", cp.Container, err)
			}
			pod.Spec.Containers[idx].Image = img
		}
	}

	if err := controllerutil.SetControllerReference(pr, pod, r.Scheme); err != nil {
		return nil, err
	}
	return pod, nil
}

func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// saveStatus persists the PodRestore status (with observedGeneration) and returns
// result, but skips the API write when nothing changed relative to before. That
// no-op skip is what keeps re-observing a steady state from bumping
// resourceVersion and re-triggering reconciliation in a hot loop; the requeue in
// result still stands even when no write happens.
func (r *PodRestoreReconciler) saveStatus(
	ctx context.Context,
	pr *criuorgv1.PodRestore,
	before *criuorgv1.PodRestoreStatus,
	result ctrl.Result,
) (ctrl.Result, error) {
	pr.Status.ObservedGeneration = pr.Generation
	if apiequality.Semantic.DeepEqual(before, &pr.Status) {
		return result, nil
	}
	if err := r.Status().Update(ctx, pr); err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

func setReady(pr *criuorgv1.PodRestore, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&pr.Status.Conditions, metav1.Condition{
		Type:               criuorgv1.ConditionReady,
		Status:             status,
		ObservedGeneration: pr.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// setPinned records whether the source checkpoints are actually pinned against
// the retention garbage collector. It is False (not an error) when the archive
// is not reachable from the operator, which is the normal cross-node case.
func setPinned(pr *criuorgv1.PodRestore, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&pr.Status.Conditions, metav1.Condition{
		Type:               criuorgv1.ConditionCheckpointsPinned,
		Status:             status,
		ObservedGeneration: pr.Generation,
		Reason:             reason,
		Message:            message,
	})
}

func containerIndex(containers []corev1.Container, name string) int {
	for i := range containers {
		if containers[i].Name == name {
			return i
		}
	}
	return -1
}

// readCheckpointBaseImage extracts the base (rootfs) image name recorded in a
// checkpoint archive's config.dump. This requires the archive to be readable from
// where the controller runs; if it is not, the user should set the container image
// explicitly in the template.
func readCheckpointBaseImage(logger logr.Logger, checkpointPath string) (string, error) {
	img, err := checkpointarchive.ReadBaseImage(checkpointPath)
	if err != nil {
		return "", fmt.Errorf("%w (set the container image explicitly if the archive is not reachable from the operator)", err)
	}
	logger.V(1).Info("resolved base image from checkpoint", "path", checkpointPath, "image", img)
	return img, nil
}

// pinCheckpoint writes a .keep marker next to the archive so the garbage
// collector retains it, and reports whether the pin was actually written. This
// is inherently best-effort: the archive is node-local to TargetNode, so the pin
// only takes effect when that path is reachable from where the controller runs
// (single-node clusters, or the operator scheduled onto TargetNode). Cross-node
// it returns false and the caller surfaces a CheckpointsPinned=False condition;
// enforcing retention there requires a node-side actor.
func pinCheckpoint(logger logr.Logger, path, owner string) bool {
	if _, err := os.Stat(path); err != nil {
		logger.Info("checkpoint not reachable from controller; pin not enforced (retain it node-side)", "path", path)
		return false
	}
	keep := path + ".keep"

	podRestorePinMu.Lock()
	defer podRestorePinMu.Unlock()

	data, err := os.ReadFile(keep)
	if os.IsNotExist(err) {
		if err := writePodRestorePinMarker(keep, podRestorePinMarker{
			ManagedBy: podRestorePinManager,
			Owners:    []string{owner},
		}); err != nil {
			logger.Error(err, "failed to write .keep marker", "path", keep)
			return false
		}
		return true
	}
	if err != nil {
		logger.Error(err, "failed to read .keep marker", "path", keep)
		return false
	}

	marker, managed := parsePodRestorePinMarker(data)
	if !managed {
		return true
	}
	if containsString(marker.Owners, owner) {
		return true
	}
	marker.Owners = append(marker.Owners, owner)
	sort.Strings(marker.Owners)
	if err := writePodRestorePinMarker(keep, *marker); err != nil {
		logger.Error(err, "failed to update .keep marker", "path", keep)
		return false
	}
	return true
}

// unpinCheckpoint removes this PodRestore's owner entry from the .keep marker.
// Existing user-created markers and markers owned by another restore are left in
// place so deleting one restore cannot unpin a checkpoint still protected by
// someone else.
func unpinCheckpoint(logger logr.Logger, path, owner string) {
	keep := path + ".keep"

	podRestorePinMu.Lock()
	defer podRestorePinMu.Unlock()

	data, err := os.ReadFile(keep)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		logger.Error(err, "failed to read .keep marker", "path", keep)
		return
	}

	marker, managed := parsePodRestorePinMarker(data)
	if !managed {
		return
	}
	owners := removeString(marker.Owners, owner)
	if len(owners) == len(marker.Owners) {
		return
	}
	if len(owners) > 0 {
		marker.Owners = owners
		if err := writePodRestorePinMarker(keep, *marker); err != nil {
			logger.Error(err, "failed to update .keep marker", "path", keep)
		}
		return
	}
	if err := os.Remove(keep); err != nil && !os.IsNotExist(err) {
		logger.Error(err, "failed to remove .keep marker", "path", keep)
	}
}

func podRestoreOwnerKey(pr *criuorgv1.PodRestore) string {
	return pr.Namespace + "/" + pr.Name
}

func parsePodRestorePinMarker(data []byte) (*podRestorePinMarker, bool) {
	var marker podRestorePinMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return nil, false
	}
	if marker.ManagedBy != podRestorePinManager {
		return nil, false
	}
	return &marker, true
}

// writePodRestorePinMarker writes the marker atomically (temp file + rename) so
// a crash mid-write can never leave a truncated marker: a corrupt marker parses
// as unmanaged, which would strand the archive pinned until manually cleaned up.
func writePodRestorePinMarker(path string, marker podRestorePinMarker) error {
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func removeString(items []string, remove string) []string {
	out := items[:0]
	for _, item := range items {
		if item != remove {
			out = append(out, item)
		}
	}
	return out
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&criuorgv1.PodRestore{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}
