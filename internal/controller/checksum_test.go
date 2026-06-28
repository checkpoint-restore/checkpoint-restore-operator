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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("parseSHA256FromLogs", func() {
	It("parses standard sha256sum output", func() {
		input := "6e89fdf3cd4e5a697c4f001b50295130aff6ac3c9929f6895c533f080105c980  /var/lib/kubelet/checkpoints/checkpoint-foo.tar\n"
		checksum, err := parseSHA256FromLogs(strings.NewReader(input))
		Expect(err).NotTo(HaveOccurred())
		Expect(checksum).To(Equal("6e89fdf3cd4e5a697c4f001b50295130aff6ac3c9929f6895c533f080105c980"))
	})

	It("returns an error on empty input", func() {
		_, err := parseSHA256FromLogs(strings.NewReader(""))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no output from helper pod"))
	})
})
