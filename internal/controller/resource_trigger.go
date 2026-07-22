package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	v1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	defaultResourcePollInterval = 30 * time.Second
	checkpointCooldown          = 5 * time.Minute
	metricsAPIGroup             = "metrics.k8s.io"
	podMetricsResource          = "pods"
)

// errMetricsNotAvailable is returned by fetchPodMetrics when the metrics API
// reports that the pod's metrics object is not available yet.
var errMetricsNotAvailable = errors.New("metrics not yet available for pod")

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
	wg         sync.WaitGroup
	interval   time.Duration

	mu             sync.Mutex
	lastCheckpoint map[string]time.Time // "pod/container" -> last checkpoint time
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

	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
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
	rt.wg.Wait()
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
			if errors.Is(err, errMetricsNotAvailable) {
				logger.Info("resource trigger: metrics not yet available, will retry", "pod", pod.Name)
			} else {
				logger.Error(err, "resource trigger: failed to fetch metrics", "pod", pod.Name)
			}
			continue
		}

		// index container limits by name for O(1) lookup; only containers
		// selected by spec.containerNames are considered
		containers := filterContainers(*pod, rt.schedule.Spec.ContainerNames)
		limits := make(map[string]corev1.ResourceList, len(containers))
		for _, c := range containers {
			limits[c.Name] = c.Resources.Limits
		}

		for _, cm := range metrics.Containers {
			containerLimits, selected := limits[cm.Name]
			if !selected {
				continue
			}
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

			// Snapshot this container's volumes before checkpointing it. Under
			// Require, a snapshot failure skips the checkpoint (and does not
			// record it as taken, so it retries after the cooldown).
			refs, proceed := captureVolumeSnapshots(ctx, rt.client, rt.schedule.Spec.VolumeSnapshots, pod, []string{cm.Name})
			if !proceed {
				continue
			}

			path, err := rt.creator.createCheckpoint(ctx, pod.Namespace, pod.Name, cm.Name, pod.Spec.NodeName)
			if err != nil {
				logger.Error(err, "resource trigger: checkpoint failed", "pod", pod.Name, "container", cm.Name)
				continue
			}
			linkSnapshotsToArchive(ctx, rt.client, pod.Namespace, refs, path)

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

	// bound the request so a slow metrics server cannot stall the poll loop
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusNotFound && isPodMetricsNotFound(body, podName) {
		return nil, errMetricsNotAvailable
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics API returned %d for %s/%s", resp.StatusCode, ns, podName)
	}

	var m podMetricsResponse
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func isPodMetricsNotFound(body []byte, podName string) bool {
	var status metav1.Status
	if err := json.Unmarshal(body, &status); err != nil {
		return false
	}
	if status.Reason != metav1.StatusReasonNotFound {
		return false
	}
	if status.Code != 0 && status.Code != http.StatusNotFound {
		return false
	}
	if status.Details == nil {
		return false
	}
	return status.Details.Group == metricsAPIGroup &&
		status.Details.Kind == podMetricsResource &&
		status.Details.Name == podName
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
