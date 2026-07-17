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
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

var _ = Describe("isCheckpointPinned", func() {
	var (
		dir     string
		archive string
	)

	BeforeEach(func() {
		var err error
		dir, err = os.MkdirTemp("", "checkpoint-pinning-test-*")
		Expect(err).NotTo(HaveOccurred())
		archive = filepath.Join(dir, "checkpoint-mypod_default-mycontainer-2026-06-12T10:00:00Z.tar")
	})

	AfterEach(func() {
		Expect(os.RemoveAll(dir)).To(Succeed())
	})

	It("returns false when no .keep file exists", func() {
		Expect(isCheckpointPinned(archive)).To(BeFalse())
	})

	It("returns true for an empty .keep file", func() {
		Expect(os.WriteFile(archive+".keep", []byte{}, 0o644)).To(Succeed())
		Expect(isCheckpointPinned(archive)).To(BeTrue())
	})

	It("returns true for a .keep file with valid JSON", func() {
		content := `{"reason":"used-for-restore","pinnedBy":"user","pinnedAt":"2026-06-12T10:05:00Z"}`
		Expect(os.WriteFile(archive+".keep", []byte(content), 0o644)).To(Succeed())
		Expect(isCheckpointPinned(archive)).To(BeTrue())
	})

	It("returns true for a .keep file with malformed JSON (existence wins)", func() {
		Expect(os.WriteFile(archive+".keep", []byte("not json {{{"), 0o644)).To(Succeed())
		Expect(isCheckpointPinned(archive)).To(BeTrue())
	})

	It("returns true when .keep is a directory (os.Stat returns nil for dirs)", func() {
		Expect(os.Mkdir(archive+".keep", 0o755)).To(Succeed())
		Expect(isCheckpointPinned(archive)).To(BeTrue())
	})

	It("returns true even when .keep has mode 0o000 (os.Stat needs only dir-execute, not file-read)", func() {
		Expect(os.WriteFile(archive+".keep", []byte("pinned"), 0o644)).To(Succeed())
		Expect(os.Chmod(archive+".keep", 0o000)).To(Succeed())
		// os.Stat requires execute permission on the containing directory, not read
		// permission on the file itself.  A 0o000 .keep file is still treated as
		// pinned - safer than silently losing pinning due to a permission mistake.
		Expect(isCheckpointPinned(archive)).To(BeTrue())
	})
})

var _ = Describe("partitionArchives", func() {
	var dir string

	BeforeEach(func() {
		var err error
		dir, err = os.MkdirTemp("", "partition-test-*")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		Expect(os.RemoveAll(dir)).To(Succeed())
	})

	makeArchive := func(name string) string {
		path := filepath.Join(dir, name)
		Expect(os.WriteFile(path, []byte("fake tar"), 0o644)).To(Succeed())
		return path
	}

	pin := func(archivePath string) {
		Expect(os.WriteFile(archivePath+".keep", []byte("{}"), 0o644)).To(Succeed())
	}

	It("returns nil slices for empty input", func() {
		deletable, pinned := partitionArchives(nil)
		Expect(deletable).To(BeNil())
		Expect(pinned).To(BeNil())
	})

	It("returns all archives as deletable when none are pinned", func() {
		a := makeArchive("a.tar")
		b := makeArchive("b.tar")
		deletable, pinned := partitionArchives([]string{a, b})
		Expect(deletable).To(Equal([]string{a, b}))
		Expect(pinned).To(BeNil())
	})

	It("returns all archives as pinned when all are pinned", func() {
		a := makeArchive("a.tar")
		b := makeArchive("b.tar")
		pin(a)
		pin(b)
		deletable, pinned := partitionArchives([]string{a, b})
		Expect(deletable).To(BeNil())
		Expect(pinned).To(Equal([]string{a, b}))
	})

	It("correctly splits a mixed list, preserving input order", func() {
		a := makeArchive("a.tar")
		b := makeArchive("b.tar")
		c := makeArchive("c.tar")
		pin(b)
		deletable, pinned := partitionArchives([]string{a, b, c})
		Expect(deletable).To(Equal([]string{a, c}))
		Expect(pinned).To(Equal([]string{b}))
	})

	It("produces no duplicates in either slice", func() {
		a := makeArchive("a.tar")
		pin(a)
		deletable, pinned := partitionArchives([]string{a, a})
		Expect(append(deletable, pinned...)).To(HaveLen(2))
		for _, p := range pinned {
			Expect(deletable).NotTo(ContainElement(p))
		}
	})
})

var _ = Describe("removeArchiveUnlessPinned mirrors deletion to CheckpointArchive", func() {
	var (
		dir     string
		archive string
	)

	BeforeEach(func() {
		Expect(criuorgv1.AddToScheme(scheme.Scheme)).To(Succeed())

		var err error
		dir, err = os.MkdirTemp("", "checkpoint-archive-gc-test-*")
		Expect(err).NotTo(HaveOccurred())
		archive = filepath.Join(dir, "checkpoint-mypod_default-mycontainer-2026-06-12T10:00:00Z.tar")
		Expect(os.WriteFile(archive, []byte("fake tar"), 0o644)).To(Succeed())
	})

	AfterEach(func() {
		Expect(os.RemoveAll(dir)).To(Succeed())
		GarbageCollector.Client = nil
	})

	It("does nothing beyond the local delete when the GC has no client configured", func() {
		removeArchiveUnlessPinned(logr.Discard(), archive)
		_, err := os.Stat(archive)
		Expect(os.IsNotExist(err)).To(BeTrue())
	})

	It("deletes the matching CheckpointArchive when the local file is removed", func() {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(&criuorgv1.CheckpointArchive{
			ObjectMeta: metav1.ObjectMeta{Name: "archive-1", Namespace: "default"},
			Spec: criuorgv1.CheckpointArchiveSpec{
				Node:      "node-a",
				LocalPath: archive,
				Namespace: "default",
				Pod:       "mypod",
				Container: "mycontainer",
			},
		}).Build()
		GarbageCollector.Client = fakeClient

		removeArchiveUnlessPinned(logr.Discard(), archive)

		var list criuorgv1.CheckpointArchiveList
		Expect(fakeClient.List(context.Background(), &list)).To(Succeed())
		Expect(list.Items).To(BeEmpty())
	})

	It("leaves unrelated CheckpointArchive records untouched", func() {
		other := filepath.Join(dir, "other.tar")
		fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(&criuorgv1.CheckpointArchive{
			ObjectMeta: metav1.ObjectMeta{Name: "archive-1", Namespace: "default"},
			Spec: criuorgv1.CheckpointArchiveSpec{
				Node:      "node-a",
				LocalPath: other,
				Namespace: "default",
				Pod:       "otherpod",
				Container: "othercontainer",
			},
		}).Build()
		GarbageCollector.Client = fakeClient

		removeArchiveUnlessPinned(logr.Discard(), archive)

		var list criuorgv1.CheckpointArchiveList
		Expect(fakeClient.List(context.Background(), &list)).To(Succeed())
		Expect(list.Items).To(HaveLen(1))
	})

	It("does not delete the CheckpointArchive when the archive is pinned", func() {
		Expect(os.WriteFile(archive+".keep", []byte("{}"), 0o644)).To(Succeed())
		fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(&criuorgv1.CheckpointArchive{
			ObjectMeta: metav1.ObjectMeta{Name: "archive-1", Namespace: "default"},
			Spec: criuorgv1.CheckpointArchiveSpec{
				Node:      "node-a",
				LocalPath: archive,
				Namespace: "default",
				Pod:       "mypod",
				Container: "mycontainer",
			},
		}).Build()
		GarbageCollector.Client = fakeClient

		removeArchiveUnlessPinned(logr.Discard(), archive)

		Expect(archive).To(BeAnExistingFile())
		var list criuorgv1.CheckpointArchiveList
		Expect(fakeClient.List(context.Background(), &list)).To(Succeed())
		Expect(list.Items).To(HaveLen(1))
	})
})

var _ = Describe("retention unreachable log dedup", func() {
	const key = "container|count|ns/pod/ctr"

	AfterEach(func() {
		clearRetentionUnreachable(key)
	})

	It("logs on first breach", func() {
		Expect(logRetentionUnreachableChanged(key, []string{"/a.tar"})).To(BeTrue())
	})

	It("suppresses repeats of the same blocking set", func() {
		Expect(logRetentionUnreachableChanged(key, []string{"/a.tar"})).To(BeTrue())
		Expect(logRetentionUnreachableChanged(key, []string{"/a.tar"})).To(BeFalse())
		Expect(logRetentionUnreachableChanged(key, []string{"/a.tar"})).To(BeFalse())
	})

	It("logs again when the blocking set changes", func() {
		Expect(logRetentionUnreachableChanged(key, []string{"/a.tar"})).To(BeTrue())
		Expect(logRetentionUnreachableChanged(key, []string{"/a.tar", "/b.tar"})).To(BeTrue())
		Expect(logRetentionUnreachableChanged(key, []string{"/a.tar", "/b.tar"})).To(BeFalse())
	})

	It("logs again after the breach clears and recurs", func() {
		Expect(logRetentionUnreachableChanged(key, []string{"/a.tar"})).To(BeTrue())
		clearRetentionUnreachable(key)
		Expect(logRetentionUnreachableChanged(key, []string{"/a.tar"})).To(BeTrue())
	})

	It("tracks count and size keys independently", func() {
		countKey := "container|count|ns/pod/ctr"
		sizeKey := "container|size|ns/pod/ctr"
		defer clearRetentionUnreachable(sizeKey)
		Expect(logRetentionUnreachableChanged(countKey, []string{"/a.tar"})).To(BeTrue())
		Expect(logRetentionUnreachableChanged(sizeKey, []string{"/a.tar"})).To(BeTrue())
	})
})
