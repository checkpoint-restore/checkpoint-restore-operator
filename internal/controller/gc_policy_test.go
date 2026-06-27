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
	"os"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("applyPolicies", func() {
	var tmpDir string
	var savedDir string

	BeforeEach(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "gc-policy-test-*")
		Expect(err).NotTo(HaveOccurred())

		savedDir = checkpointDirectory
		checkpointDirectory = tmpDir

		resetAllPoliciesToDefault(logr.Discard())
	})

	AfterEach(func() {
		checkpointDirectory = savedDir
		Expect(os.RemoveAll(tmpDir)).To(Succeed())
	})

	It("does not panic when retainOrphan is nil", func() {
		// retainOrphan is nil after resetAllPoliciesToDefault.
		// Before the fix (*retainOrphan was used directly) this panicked.
		// After the fix (ifNil(retainOrphan)) this must return cleanly.
		Expect(retainOrphan).To(BeNil())

		details := &checkpointDetails{
			namespace: "default",
			pod:       "test-pod",
			container: "test-container",
		}

		Expect(func() {
			applyPolicies(logr.Discard(), details)
		}).NotTo(Panic())
	})
})