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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/rest"
)

var _ = Describe("CheckpointCreator", func() {
	var (
		server     *httptest.Server
		creator    *CheckpointCreator
		lastPath   string
		lastMethod string
		statusCode int
	)

	BeforeEach(func() {
		statusCode = http.StatusOK
		checkpointTargetLocks = newCheckpointTargetLockTable()
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			lastPath = r.URL.Path
			lastMethod = r.Method
			w.WriteHeader(statusCode)
		}))
		creator = NewCheckpointCreator(nil, &rest.Config{Host: server.URL})
	})

	AfterEach(func() {
		server.Close()
	})

	It("POSTs to the correct kubelet checkpoint URL", func() {
		err := creator.createCheckpoint(context.Background(), "default", "my-pod", "my-container", "my-node")
		Expect(err).NotTo(HaveOccurred())
		Expect(lastMethod).To(Equal(http.MethodPost))
		Expect(lastPath).To(Equal("/api/v1/nodes/my-node/proxy/checkpoint/default/my-pod/my-container"))
	})

	It("returns nil on HTTP 200", func() {
		err := creator.createCheckpoint(context.Background(), "ns", "pod", "ctr", "node")
		Expect(err).NotTo(HaveOccurred())
	})

	It("returns an error on non-200 status", func() {
		statusCode = http.StatusInternalServerError
		err := creator.createCheckpoint(context.Background(), "ns", "pod", "ctr", "node")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("checkpoint failed"))
	})

	It("includes the kubelet response body in non-200 errors", func() {
		server.Close()
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("runtime checkpoint support is disabled"))
		}))
		creator = NewCheckpointCreator(nil, &rest.Config{Host: server.URL})

		err := creator.createCheckpoint(context.Background(), "ns", "pod", "ctr", "node")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("status 500"))
		Expect(err.Error()).To(ContainSubstring("runtime checkpoint support is disabled"))
	})

	It("retries transient checkpoint conflicts", func() {
		server.Close()
		var calls int32
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&calls, 1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("checkpointing container failed: cannot pause a paused container"))
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		creator = NewCheckpointCreator(nil, &rest.Config{Host: server.URL})

		origDelay := checkpointRetryDelay
		checkpointRetryDelay = time.Millisecond
		defer func() { checkpointRetryDelay = origDelay }()

		err := creator.createCheckpoint(context.Background(), "ns", "pod", "ctr", "node")
		Expect(err).NotTo(HaveOccurred())
		Expect(atomic.LoadInt32(&calls)).To(Equal(int32(2)))
	})

	It("serializes concurrent checkpoints for the same target", func() {
		server.Close()
		var inFlight int32
		var maxInFlight int32
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			current := atomic.AddInt32(&inFlight, 1)
			for {
				observed := atomic.LoadInt32(&maxInFlight)
				if current <= observed || atomic.CompareAndSwapInt32(&maxInFlight, observed, current) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
			w.WriteHeader(http.StatusOK)
		}))
		creator = NewCheckpointCreator(nil, &rest.Config{Host: server.URL})

		errs := make(chan error, 2)
		go func() {
			errs <- creator.createCheckpoint(context.Background(), "ns", "pod", "ctr", "node")
		}()
		go func() {
			errs <- creator.createCheckpoint(context.Background(), "ns", "pod", "ctr", "node")
		}()

		Expect(<-errs).NotTo(HaveOccurred())
		Expect(<-errs).NotTo(HaveOccurred())
		Expect(atomic.LoadInt32(&maxInFlight)).To(Equal(int32(1)))

		checkpointTargetLocks.mu.Lock()
		defer checkpointTargetLocks.mu.Unlock()
		Expect(checkpointTargetLocks.locks).To(BeEmpty())
	})

	It("removes target locks after all waiters leave", func() {
		table := newCheckpointTargetLockTable()

		unlock := table.lock("node/ns/pod/ctr")
		unlock()

		table.mu.Lock()
		defer table.mu.Unlock()
		Expect(table.locks).To(BeEmpty())
	})
})

var _ = Describe("CheckpointCreator request timeout", func() {
	It("aborts the request when the kubelet does not respond in time", func() {
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-release // hang until the test finishes
		}))
		defer func() { close(release); server.Close() }()

		orig := checkpointRequestTimeout
		checkpointRequestTimeout = 50 * time.Millisecond
		defer func() { checkpointRequestTimeout = orig }()

		creator := NewCheckpointCreator(nil, &rest.Config{Host: server.URL})
		start := time.Now()
		err := creator.createCheckpoint(context.Background(), "ns", "pod", "ctr", "node")

		Expect(err).To(HaveOccurred())
		Expect(time.Since(start)).To(BeNumerically("<", 5*time.Second))
		Expect(err.Error()).To(ContainSubstring("context deadline exceeded"))
	})
})
