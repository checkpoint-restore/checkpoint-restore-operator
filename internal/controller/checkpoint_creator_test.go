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
})
