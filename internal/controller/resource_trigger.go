package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	v1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	defaultResourcePollInterval = 30 * time.Second
	checkpointCooldown          = 5 * time.Minute
)

// podMetricsResponse is a minimal representation of the metrics.k8s.io/v1beta1 PodMetrics object.
type podMetricsResponse struct {
	Containers []containerMetricsItem `json:"containers"`
}

type containerMetricsItem struct {
	Name  string            `json:"name"`
	Usage map[string]string `json:"usage"`
}

type ResourceTrigger struct {
	client     client.Client
	restConfig *rest.Config
	creator    Checkpointer
	schedule   *v1.CheckpointSchedule
	stopCh     chan struct{}
	interval   time.Duration

	mu             sync.Mutex
	lastCheckpoint map[string]time.Time // "pod/container" → last checkpoint time
}

func NewResourceTrigger(c client.Client, restConfig *rest.Config, creator Checkpointer, schedule *v1.CheckpointSchedule) *ResourceTrigger {
	interval := defaultResourcePollInterval
	if rt := schedule.Spec.Triggers.ResourceThreshold; rt != nil && rt.PollIntervalSeconds != nil {
		interval = time.Duration(*rt.PollIntervalSeconds) * time.Second
	}
	return &ResourceTrigger{
		client:         c,
		restConfig:     restConfig,
		creator:        creator,
		schedule:       schedule,
		stopCh:         make(chan struct{}),
		interval:       interval,
		lastCheckpoint: make(map[string]time.Time),
	}
}

func (rt *ResourceTrigger) Start(ctx context.Context) {
	logger := log.FromContext(ctx)
	logger.Info("resource trigger started", "interval", rt.interval)

	go func() {
		ticker := time.NewTicker(rt.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rt.run(ctx)
			case <-rt.stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (rt *ResourceTrigger) Stop() {
	close(rt.stopCh)
}

func (rt *ResourceTrigger) run(ctx context.Context) {
	logger := log.FromContext(ctx)
	spec := rt.schedule.Spec.Triggers.ResourceThreshold
	if spec == nil {
		return
	}

	pods, err := getMatchingPods(ctx, rt.client, rt.schedule.Spec.Namespace, &rt.schedule.Spec.Selector)
	if err != nil {
		logger.Error(err, "resource trigger: failed to list pods")
		return
	}

	httpClient, err := rest.HTTPClientFor(rt.restConfig)
	if err != nil {
		logger.Error(err, "resource trigger: failed to build HTTP client")
		return
	}

	for i := range pods {
		pod := &pods[i]
		metrics, err := rt.fetchPodMetrics(ctx, httpClient, pod.Namespace, pod.Name)
		if err != nil {
			logger.Error(err, "resource trigger: failed to fetch metrics", "pod", pod.Name)
			continue
		}

		// index container limits by name for O(1) lookup
		limits := make(map[string]corev1.ResourceList, len(pod.Spec.Containers))
		for _, c := range pod.Spec.Containers {
			limits[c.Name] = c.Resources.Limits
		}

		for _, cm := range metrics.Containers {
			containerLimits := limits[cm.Name]
			triggered, reason := rt.thresholdExceeded(cm, containerLimits, spec)
			if !triggered {
				continue
			}

			key := pod.Name + "/" + cm.Name
			rt.mu.Lock()
			last := rt.lastCheckpoint[key]
			rt.mu.Unlock()
			if time.Since(last) < checkpointCooldown {
				continue
			}

			logger.Info("resource trigger: threshold exceeded, checkpointing",
				"pod", pod.Name, "container", cm.Name, "reason", reason)

			if err := rt.creator.createCheckpoint(ctx, pod.Namespace, pod.Name, cm.Name, pod.Spec.NodeName); err != nil {
				logger.Error(err, "resource trigger: checkpoint failed", "pod", pod.Name, "container", cm.Name)
				continue
			}

			rt.mu.Lock()
			rt.lastCheckpoint[key] = time.Now()
			rt.mu.Unlock()
		}
	}
}

// thresholdExceeded returns true when any configured upper or lower threshold is breached.
func (rt *ResourceTrigger) thresholdExceeded(cm containerMetricsItem, limits corev1.ResourceList, spec *v1.ResourceThresholdSpec) (bool, string) {
	if spec.MemoryPercent != nil {
		if pct, ok := memoryUsagePercent(cm, limits); ok {
			if spec.MemoryPercent.Upper != nil && pct >= int64(*spec.MemoryPercent.Upper) {
				return true, fmt.Sprintf("memory %d%% >= upper threshold %d%%", pct, *spec.MemoryPercent.Upper)
			}
			if spec.MemoryPercent.Lower != nil && pct <= int64(*spec.MemoryPercent.Lower) {
				return true, fmt.Sprintf("memory %d%% <= lower threshold %d%%", pct, *spec.MemoryPercent.Lower)
			}
		}
	}
	if spec.CPUPercent != nil {
		if pct, ok := cpuUsagePercent(cm, limits); ok {
			if spec.CPUPercent.Upper != nil && pct >= int64(*spec.CPUPercent.Upper) {
				return true, fmt.Sprintf("CPU %d%% >= upper threshold %d%%", pct, *spec.CPUPercent.Upper)
			}
			if spec.CPUPercent.Lower != nil && pct <= int64(*spec.CPUPercent.Lower) {
				return true, fmt.Sprintf("CPU %d%% <= lower threshold %d%%", pct, *spec.CPUPercent.Lower)
			}
		}
	}
	return false, ""
}

func (rt *ResourceTrigger) fetchPodMetrics(ctx context.Context, httpClient *http.Client, ns, podName string) (*podMetricsResponse, error) {
	url := rt.restConfig.Host + "/apis/metrics.k8s.io/v1beta1/namespaces/" + ns + "/pods/" + podName
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics API returned %d for %s/%s", resp.StatusCode, ns, podName)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var m podMetricsResponse
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// memoryUsagePercent returns (usagePercent, true) when both usage and a non-zero limit are available.
func memoryUsagePercent(cm containerMetricsItem, limits corev1.ResourceList) (int64, bool) {
	usageStr, ok := cm.Usage["memory"]
	if !ok {
		return 0, false
	}
	usage, err := resource.ParseQuantity(usageStr)
	if err != nil {
		return 0, false
	}
	limit, ok := limits[corev1.ResourceMemory]
	if !ok || limit.IsZero() {
		return 0, false
	}
	return (usage.Value() * 100) / limit.Value(), true
}

// cpuUsagePercent returns (usagePercent, true) when both usage and a non-zero limit are available.
func cpuUsagePercent(cm containerMetricsItem, limits corev1.ResourceList) (int64, bool) {
	usageStr, ok := cm.Usage["cpu"]
	if !ok {
		return 0, false
	}
	usage, err := resource.ParseQuantity(usageStr)
	if err != nil {
		return 0, false
	}
	limit, ok := limits[corev1.ResourceCPU]
	if !ok || limit.IsZero() {
		return 0, false
	}
	return (usage.MilliValue() * 100) / limit.MilliValue(), true
}
