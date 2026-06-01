package controller

import (
	"context"
	"sync"
	"time"

	v1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const eventPollInterval = 5 * time.Second

// KubernetesEvent values used in spec.triggers.onKubernetesEvents.
const (
	EventNodeDrain   = "NodeDrain"
	EventPodEviction = "PodEviction"
	// EventPreemption requires Kubernetes 1.26+ (DisruptionTarget pod condition).
	EventPreemption = "Preemption"
)

// disruptionTargetCondition is the pod condition type set by the scheduler when
// a pod is about to be preempted. Introduced in Kubernetes 1.26.
const (
	disruptionTargetCondition = "DisruptionTarget"
	preemptingEvictorReason   = "PreemptingEvictor"
)

type EventTrigger struct {
	client   client.Client
	creator  Checkpointer
	schedule *v1.CheckpointSchedule
	stopCh   chan struct{}

	watchNodeDrain   bool
	watchPodEviction bool
	watchPreemption  bool

	mu        sync.Mutex
	seenNodes map[string]bool // node name  → drain already checkpointed
	seenPods  map[string]bool // "ns/name"  → eviction/preemption already checkpointed
}

func NewEventTrigger(c client.Client, creator Checkpointer, schedule *v1.CheckpointSchedule) *EventTrigger {
	et := &EventTrigger{
		client:    c,
		creator:   creator,
		schedule:  schedule,
		stopCh:    make(chan struct{}),
		seenNodes: make(map[string]bool),
		seenPods:  make(map[string]bool),
	}
	for _, e := range schedule.Spec.Triggers.OnKubernetesEvents {
		switch e {
		case EventNodeDrain:
			et.watchNodeDrain = true
		case EventPodEviction:
			et.watchPodEviction = true
		case EventPreemption:
			et.watchPreemption = true
		}
	}
	return et
}

func (et *EventTrigger) Start(ctx context.Context) {
	logger := log.FromContext(ctx)
	logger.Info("event trigger started",
		"nodeDrain", et.watchNodeDrain,
		"podEviction", et.watchPodEviction,
		"preemption", et.watchPreemption,
	)

	go func() {
		ticker := time.NewTicker(eventPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				et.run(ctx)
			case <-et.stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (et *EventTrigger) Stop() {
	close(et.stopCh)
}

func (et *EventTrigger) run(ctx context.Context) {
	logger := log.FromContext(ctx)

	pods, err := getMatchingPods(ctx, et.client, et.schedule.Spec.Namespace, &et.schedule.Spec.Selector)
	if err != nil {
		logger.Error(err, "event trigger: failed to list pods")
		return
	}

	// clean up seenPods entries for pods that no longer exist
	live := make(map[string]bool, len(pods))
	for _, p := range pods {
		live[p.Namespace+"/"+p.Name] = true
	}
	et.mu.Lock()
	for key := range et.seenPods {
		if !live[key] {
			delete(et.seenPods, key)
		}
	}
	et.mu.Unlock()

	if et.watchNodeDrain {
		et.handleNodeDrain(ctx, pods)
	}

	for i := range pods {
		pod := &pods[i]
		key := pod.Namespace + "/" + pod.Name

		if et.watchPodEviction && pod.DeletionTimestamp != nil {
			et.checkpointOnce(ctx, pod, key, "pod eviction")
		}

		if et.watchPreemption && podIsPreempted(pod) {
			et.checkpointOnce(ctx, pod, key, "preemption")
		}
	}
}

func (et *EventTrigger) handleNodeDrain(ctx context.Context, pods []corev1.Pod) {
	logger := log.FromContext(ctx)

	// collect the set of node names that host matching pods
	podNodeNames := make(map[string]bool, len(pods))
	for _, p := range pods {
		if p.Spec.NodeName != "" {
			podNodeNames[p.Spec.NodeName] = true
		}
	}

	nodeList := &corev1.NodeList{}
	if err := et.client.List(ctx, nodeList); err != nil {
		logger.Error(err, "event trigger: failed to list nodes")
		return
	}

	// which of our nodes are currently draining?
	drainingNow := make(map[string]bool)
	for _, node := range nodeList.Items {
		if node.Spec.Unschedulable && podNodeNames[node.Name] {
			drainingNow[node.Name] = true
		}
	}

	// evict nodes from seenNodes once they are no longer draining
	// so the next drain cycle is caught fresh
	et.mu.Lock()
	for nodeName := range et.seenNodes {
		if !drainingNow[nodeName] {
			delete(et.seenNodes, nodeName)
		}
	}
	et.mu.Unlock()

	for nodeName := range drainingNow {
		et.mu.Lock()
		already := et.seenNodes[nodeName]
		et.mu.Unlock()
		if already {
			continue
		}

		logger.Info("event trigger: node drain detected", "node", nodeName)

		fired := false
		for i := range pods {
			pod := &pods[i]
			if pod.Spec.NodeName != nodeName {
				continue
			}
			et.checkpointPodContainers(ctx, pod, "node drain")
			fired = true
		}

		if fired {
			et.mu.Lock()
			et.seenNodes[nodeName] = true
			et.mu.Unlock()
		}
	}
}

// checkpointOnce checkpoints a pod exactly once per disruption event.
func (et *EventTrigger) checkpointOnce(ctx context.Context, pod *corev1.Pod, key, reason string) {
	et.mu.Lock()
	already := et.seenPods[key]
	et.mu.Unlock()
	if already {
		return
	}

	log.FromContext(ctx).Info("event trigger: pod disruption detected",
		"pod", pod.Name, "reason", reason, "intent", string(v1.PodDisruption))

	et.checkpointPodContainers(ctx, pod, reason)

	et.mu.Lock()
	et.seenPods[key] = true
	et.mu.Unlock()
}

// checkpointPodContainers checkpoints all (or configured) containers in a pod.
func (et *EventTrigger) checkpointPodContainers(ctx context.Context, pod *corev1.Pod, reason string) {
	logger := log.FromContext(ctx)

	containerSet := make(map[string]struct{}, len(et.schedule.Spec.ContainerNames))
	for _, name := range et.schedule.Spec.ContainerNames {
		containerSet[name] = struct{}{}
	}

	for _, c := range pod.Spec.Containers {
		if len(containerSet) > 0 {
			if _, ok := containerSet[c.Name]; !ok {
				continue
			}
		}
		if err := et.creator.createCheckpoint(ctx, pod.Namespace, pod.Name, c.Name, pod.Spec.NodeName); err != nil {
			logger.Error(err, "event trigger: checkpoint failed",
				"pod", pod.Name, "container", c.Name, "reason", reason)
		} else {
			logger.Info("event trigger: checkpoint created",
				"pod", pod.Name, "container", c.Name,
				"reason", reason, "intent", string(v1.PodDisruption))
		}
	}
}

// podIsPreempted returns true when the pod carries the DisruptionTarget condition
// with reason PreemptingEvictor. This condition is available since Kubernetes 1.26.
func podIsPreempted(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == disruptionTargetCondition &&
			cond.Reason == preemptingEvictorReason &&
			cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}