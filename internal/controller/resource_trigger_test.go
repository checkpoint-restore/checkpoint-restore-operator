package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

func intPtr(i int) *int { return &i }

func buildResourceTrigger(mock *mockCheckpointer, serverURL string, threshold *v1.ResourceThresholdSpec, pods ...*corev1.Pod) *ResourceTrigger {
	builder := fake.NewClientBuilder().WithScheme(scheme.Scheme)
	for _, p := range pods {
		builder = builder.WithObjects(p).WithStatusSubresource(p)
	}
	sched := &v1.CheckpointSchedule{
		Spec: v1.CheckpointScheduleSpec{
			Namespace: "default",
			Selector:  metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Triggers:  v1.TriggersSpec{ResourceThreshold: threshold},
		},
	}
	return NewResourceTrigger(builder.Build(), &rest.Config{Host: serverURL}, mock, sched)
}

func runningPodWithLimits(name string, memLimit, cpuLimit string) *corev1.Pod {
	limits := corev1.ResourceList{}
	if memLimit != "" {
		limits[corev1.ResourceMemory] = resource.MustParse(memLimit)
	}
	if cpuLimit != "" {
		limits[corev1.ResourceCPU] = resource.MustParse(cpuLimit)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Labels: map[string]string{"app": "test"},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{{
				Name:      "app",
				Resources: corev1.ResourceRequirements{Limits: limits},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

var _ = Describe("memoryUsagePercent", func() {
	It("returns correct percent when limit is set", func() {
		cm := containerMetricsItem{Name: "app", Usage: map[string]string{"memory": "400Mi"}}
		limits := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("500Mi")}
		pct, ok := memoryUsagePercent(cm, limits)
		Expect(ok).To(BeTrue())
		Expect(pct).To(Equal(int64(80)))
	})

	It("returns false when no memory limit is set", func() {
		cm := containerMetricsItem{Name: "app", Usage: map[string]string{"memory": "100Mi"}}
		_, ok := memoryUsagePercent(cm, corev1.ResourceList{})
		Expect(ok).To(BeFalse())
	})

	It("returns false when usage key is missing", func() {
		cm := containerMetricsItem{Name: "app", Usage: map[string]string{}}
		limits := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("500Mi")}
		_, ok := memoryUsagePercent(cm, limits)
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("cpuUsagePercent", func() {
	It("returns correct percent when limit is set", func() {
		cm := containerMetricsItem{Name: "app", Usage: map[string]string{"cpu": "500m"}}
		limits := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000m")}
		pct, ok := cpuUsagePercent(cm, limits)
		Expect(ok).To(BeTrue())
		Expect(pct).To(Equal(int64(50)))
	})

	It("returns false when no CPU limit is set", func() {
		cm := containerMetricsItem{Name: "app", Usage: map[string]string{"cpu": "200m"}}
		_, ok := cpuUsagePercent(cm, corev1.ResourceList{})
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("ResourceTrigger.run", func() {
	var (
		mock       *mockCheckpointer
		server     *httptest.Server
		serverResp podMetricsResponse
	)

	BeforeEach(func() {
		mock = &mockCheckpointer{}
		serverResp = podMetricsResponse{}
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(serverResp)
		}))
	})

	AfterEach(func() { server.Close() })

	// --- memory upper threshold ---

	It("checkpoints when memory exceeds the upper threshold", func() {
		serverResp = podMetricsResponse{
			Containers: []containerMetricsItem{
				{Name: "app", Usage: map[string]string{"memory": "420Mi"}},
			},
		}
		pod := runningPodWithLimits("pod-1", "500Mi", "")
		threshold := &v1.ResourceThresholdSpec{
			MemoryPercent: &v1.ResourcePercentThreshold{Upper: intPtr(80)},
		}
		trigger := buildResourceTrigger(mock, server.URL, threshold, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(HaveLen(1))
		Expect(mock.calls[0].pod).To(Equal("pod-1"))
		Expect(mock.calls[0].container).To(Equal("app"))
	})

	It("does not checkpoint when memory is between lower and upper", func() {
		serverResp = podMetricsResponse{
			Containers: []containerMetricsItem{
				{Name: "app", Usage: map[string]string{"memory": "250Mi"}},
			},
		}
		pod := runningPodWithLimits("pod-1", "500Mi", "")
		threshold := &v1.ResourceThresholdSpec{
			MemoryPercent: &v1.ResourcePercentThreshold{Upper: intPtr(80), Lower: intPtr(25)},
		}
		trigger := buildResourceTrigger(mock, server.URL, threshold, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(BeEmpty())
	})

	// --- memory lower threshold ---

	It("checkpoints when memory drops below the lower threshold", func() {
		serverResp = podMetricsResponse{
			Containers: []containerMetricsItem{
				{Name: "app", Usage: map[string]string{"memory": "100Mi"}},
			},
		}
		pod := runningPodWithLimits("pod-1", "500Mi", "")
		threshold := &v1.ResourceThresholdSpec{
			MemoryPercent: &v1.ResourcePercentThreshold{Lower: intPtr(25)},
		}
		trigger := buildResourceTrigger(mock, server.URL, threshold, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(HaveLen(1))
		Expect(mock.calls[0].pod).To(Equal("pod-1"))
	})

	// --- CPU upper threshold ---

	It("checkpoints when CPU exceeds the upper threshold", func() {
		serverResp = podMetricsResponse{
			Containers: []containerMetricsItem{
				{Name: "app", Usage: map[string]string{"cpu": "950m"}},
			},
		}
		pod := runningPodWithLimits("pod-1", "", "1000m")
		threshold := &v1.ResourceThresholdSpec{
			CPUPercent: &v1.ResourcePercentThreshold{Upper: intPtr(90)},
		}
		trigger := buildResourceTrigger(mock, server.URL, threshold, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(HaveLen(1))
		Expect(mock.calls[0].container).To(Equal("app"))
	})

	// --- CPU lower threshold ---

	It("checkpoints when CPU drops below the lower threshold", func() {
		serverResp = podMetricsResponse{
			Containers: []containerMetricsItem{
				{Name: "app", Usage: map[string]string{"cpu": "30m"}},
			},
		}
		pod := runningPodWithLimits("pod-1", "", "1000m")
		threshold := &v1.ResourceThresholdSpec{
			CPUPercent: &v1.ResourcePercentThreshold{Lower: intPtr(5)},
		}
		trigger := buildResourceTrigger(mock, server.URL, threshold, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(HaveLen(1))
		Expect(mock.calls[0].container).To(Equal("app"))
	})

	// --- cooldown ---

	It("respects the cooldown and does not checkpoint twice in quick succession", func() {
		serverResp = podMetricsResponse{
			Containers: []containerMetricsItem{
				{Name: "app", Usage: map[string]string{"memory": "420Mi"}},
			},
		}
		pod := runningPodWithLimits("pod-1", "500Mi", "")
		threshold := &v1.ResourceThresholdSpec{
			MemoryPercent: &v1.ResourcePercentThreshold{Upper: intPtr(80)},
		}
		trigger := buildResourceTrigger(mock, server.URL, threshold, pod)
		trigger.run(context.Background())
		trigger.run(context.Background()) // within cooldown window

		Expect(mock.calls).To(HaveLen(1))
	})

	// --- no limit ---

	It("skips containers without a resource limit", func() {
		serverResp = podMetricsResponse{
			Containers: []containerMetricsItem{
				{Name: "app", Usage: map[string]string{"memory": "420Mi"}},
			},
		}
		pod := runningPodWithLimits("pod-1", "", "") // no limits
		threshold := &v1.ResourceThresholdSpec{
			MemoryPercent: &v1.ResourcePercentThreshold{Upper: intPtr(80)},
		}
		trigger := buildResourceTrigger(mock, server.URL, threshold, pod)
		trigger.run(context.Background())

		Expect(mock.calls).To(BeEmpty())
	})
})

var _ = Describe("ResourceTrigger.run - selection and failure handling", func() {
	var mock *mockCheckpointer

	BeforeEach(func() {
		mock = &mockCheckpointer{}
	})

	It("only checkpoints containers listed in ContainerNames", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(podMetricsResponse{
				Containers: []containerMetricsItem{
					{Name: "app", Usage: map[string]string{"memory": "420Mi"}},
					{Name: "sidecar", Usage: map[string]string{"memory": "420Mi"}},
				},
			})
		}))
		defer server.Close()

		limits := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("500Mi")}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-1", Namespace: "default",
				Labels: map[string]string{"app": "test"},
			},
			Spec: corev1.PodSpec{
				NodeName: "node-1",
				Containers: []corev1.Container{
					{Name: "app", Resources: corev1.ResourceRequirements{Limits: limits}},
					{Name: "sidecar", Resources: corev1.ResourceRequirements{Limits: limits}},
				},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		builder := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pod)
		sched := &v1.CheckpointSchedule{
			Spec: v1.CheckpointScheduleSpec{
				Namespace:      "default",
				ContainerNames: []string{"app"},
				Selector:       metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
				Triggers: v1.TriggersSpec{ResourceThreshold: &v1.ResourceThresholdSpec{
					MemoryPercent: &v1.ResourcePercentThreshold{Upper: intPtr(80)},
				}},
			},
		}
		trigger := NewResourceTrigger(builder.Build(), &rest.Config{Host: server.URL}, mock, sched)
		trigger.run(context.Background())

		Expect(mock.calls).To(HaveLen(1))
		Expect(mock.calls[0].container).To(Equal("app"))
	})

	It("continues with other pods when the metrics fetch fails for one", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "pod-broken") {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(podMetricsResponse{
				Containers: []containerMetricsItem{
					{Name: "app", Usage: map[string]string{"memory": "420Mi"}},
				},
			})
		}))
		defer server.Close()

		podBroken := runningPodWithLimits("pod-broken", "500Mi", "")
		podOK := runningPodWithLimits("pod-ok", "500Mi", "")
		threshold := &v1.ResourceThresholdSpec{
			MemoryPercent: &v1.ResourcePercentThreshold{Upper: intPtr(80)},
		}
		trigger := buildResourceTrigger(mock, server.URL, threshold, podBroken, podOK)
		trigger.run(context.Background())

		Expect(mock.calls).To(HaveLen(1))
		Expect(mock.calls[0].pod).To(Equal("pod-ok"))
	})

	It("checkpoints again once the cooldown has expired", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(podMetricsResponse{
				Containers: []containerMetricsItem{
					{Name: "app", Usage: map[string]string{"memory": "420Mi"}},
				},
			})
		}))
		defer server.Close()

		pod := runningPodWithLimits("pod-1", "500Mi", "")
		threshold := &v1.ResourceThresholdSpec{
			MemoryPercent: &v1.ResourcePercentThreshold{Upper: intPtr(80)},
		}
		trigger := buildResourceTrigger(mock, server.URL, threshold, pod)

		// pretend the last checkpoint happened beyond the cooldown window
		trigger.lastCheckpoint["pod-1/app"] = time.Now().Add(-2 * checkpointCooldown)
		trigger.run(context.Background())

		Expect(mock.calls).To(HaveLen(1))
	})
})
