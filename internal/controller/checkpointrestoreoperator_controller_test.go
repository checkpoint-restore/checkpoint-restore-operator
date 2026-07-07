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
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrlLog "sigs.k8s.io/controller-runtime/pkg/log"
)

// makeCheckpointArchive creates a valid checkpoint tar archive for use in tests.
// The archive contains a spec.dump with the given pod/namespace/container labels
// so that getCheckpointArchiveInformation can parse it.
// sizeBytes controls the approximate on-disk size via a padding entry.
// ts is set as the file's modification time (used for GC sort order).
// If pin is true, a .keep marker file is also created.
func makeCheckpointArchive(dir, pod, ns, container string, ts time.Time, sizeBytes int64, pin bool) string {
	name := fmt.Sprintf("checkpoint-%s_%s-%s-%s.tar",
		pod, ns, container, ts.UTC().Format("2006-01-02T15:04:05Z"))
	archivePath := filepath.Join(dir, name)

	// Build the spec.dump JSON: the critical field is io.kubernetes.cri-o.Labels,
	// which is itself a JSON string containing pod/ns/container labels.
	labelsJSON, err := json.Marshal(map[string]string{
		"io.kubernetes.container.name": container,
		"io.kubernetes.pod.name":       pod,
		"io.kubernetes.pod.namespace":  ns,
	})
	Expect(err).NotTo(HaveOccurred())

	specDump, err := json.Marshal(map[string]interface{}{
		"annotations": map[string]string{
			"io.kubernetes.cri-o.Labels": string(labelsJSON),
		},
	})
	Expect(err).NotTo(HaveOccurred())

	f, err := os.Create(archivePath)
	Expect(err).NotTo(HaveOccurred())

	tw := tar.NewWriter(f)

	// Write spec.dump entry.
	Expect(tw.WriteHeader(&tar.Header{
		Name:    "spec.dump",
		Mode:    0o644,
		Size:    int64(len(specDump)),
		ModTime: ts,
	})).To(Succeed())
	_, err = tw.Write(specDump)
	Expect(err).NotTo(HaveOccurred())

	// Add a padding entry to reach approximately sizeBytes on disk.
	// Tar overhead per entry: 512-byte header + content rounded up to 512-byte blocks.
	specPadded := roundTo512(int64(len(specDump)))
	overhead := int64(512) + specPadded + 512 + 1024 // spec header + spec data + padding header + end-of-archive
	if paddingSize := sizeBytes - overhead; paddingSize > 0 {
		Expect(tw.WriteHeader(&tar.Header{
			Name:    "padding",
			Mode:    0o644,
			Size:    paddingSize,
			ModTime: ts,
		})).To(Succeed())
		buf := make([]byte, 65536)
		for remaining := paddingSize; remaining > 0; {
			n := remaining
			if n > int64(len(buf)) {
				n = int64(len(buf))
			}
			_, err = tw.Write(buf[:n])
			Expect(err).NotTo(HaveOccurred())
			remaining -= n
		}
	}

	Expect(tw.Close()).To(Succeed())
	Expect(f.Close()).To(Succeed())
	Expect(os.Chtimes(archivePath, ts, ts)).To(Succeed())

	if pin {
		Expect(os.WriteFile(archivePath+".keep", []byte(`{"reason":"test"}`), 0o644)).To(Succeed())
	}
	return archivePath
}

func makeCheckpointArchiveWithoutIdentity(dir, pod, ns, container string, ts time.Time) string {
	name := fmt.Sprintf("checkpoint-%s_%s-%s-%s.tar",
		pod, ns, container, ts.UTC().Format("2006-01-02T15:04:05Z"))
	archivePath := filepath.Join(dir, name)
	specDump := []byte(`{"annotations":{"unrelated":"x"}}`)

	f, err := os.Create(archivePath)
	Expect(err).NotTo(HaveOccurred())

	tw := tar.NewWriter(f)
	Expect(tw.WriteHeader(&tar.Header{
		Name:    "spec.dump",
		Mode:    0o644,
		Size:    int64(len(specDump)),
		ModTime: ts,
	})).To(Succeed())
	_, err = tw.Write(specDump)
	Expect(err).NotTo(HaveOccurred())

	Expect(tw.Close()).To(Succeed())
	Expect(f.Close()).To(Succeed())
	Expect(os.Chtimes(archivePath, ts, ts)).To(Succeed())
	return archivePath
}

type recordingLogSink struct {
	errorCount int
}

func (s *recordingLogSink) Init(logr.RuntimeInfo) {}

func (s *recordingLogSink) Enabled(int) bool {
	return true
}

func (s *recordingLogSink) Info(int, string, ...interface{}) {}

func (s *recordingLogSink) Error(error, string, ...interface{}) {
	s.errorCount++
}

func (s *recordingLogSink) WithValues(...interface{}) logr.LogSink {
	return s
}

func (s *recordingLogSink) WithName(string) logr.LogSink {
	return s
}

// roundTo512 rounds n up to the nearest multiple of 512 (tar block size).
func roundTo512(n int64) int64 {
	return ((n + 511) / 512) * 512
}

var _ = Describe("handleCheckpointsForLevel - checkpoint pinning", func() {
	var (
		dir     string
		details *checkpointDetails
		devNull logr.Logger
	)

	BeforeEach(func() {
		var err error
		dir, err = os.MkdirTemp("", "gc-pinning-test-*")
		Expect(err).NotTo(HaveOccurred())

		// Override the global checkpointDirectory so the GC looks in our temp dir.
		checkpointDirectory = dir

		details = &checkpointDetails{
			namespace: "default",
			pod:       "mypod",
			container: "mycontainer",
		}

		devNull = logr.Discard()
	})

	AfterEach(func() {
		Expect(os.RemoveAll(dir)).To(Succeed())
	})

	// ts returns a UTC timestamp n seconds after the Unix epoch.
	ts := func(n int) time.Time { return time.Unix(int64(n), 0).UTC() }

	It("does NOT delete a pinned archive even when it exceeds MaxCheckpointSize", func() {
		// 2 MB archive, pinned; MaxCheckpointSize = 1 MB - must survive.
		pinned := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", ts(1), 2*1024*1024, true)

		policy := Policy{
			RetainOrphan:      true,
			MaxCheckpoints:    10,
			MaxCheckpointSize: resource.MustParse("1Mi"),
			MaxTotalSize:      resource.MustParse("100Gi"),
		}

		handleCheckpointsForLevel(devNull, details, "container", policy)

		Expect(pinned).To(BeAnExistingFile())
	})

	It("still counts a pinned archive toward totalSize for accounting", func() {
		// 3 MB pinned + 3 MB deletable -> total ~6 MB, MaxTotalSize 4 MB.
		// The deletable one must be removed; the pinned one must survive.
		base := ts(1)
		pinnedPath := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base, 3*1024*1024, true)
		deletablePath := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base.Add(time.Second), 3*1024*1024, false)

		policy := Policy{
			RetainOrphan:      true,
			MaxCheckpoints:    10,
			MaxCheckpointSize: resource.MustParse("100Gi"),
			MaxTotalSize:      resource.MustParse("4Mi"),
		}

		handleCheckpointsForLevel(devNull, details, "container", policy)

		Expect(pinnedPath).To(BeAnExistingFile())
		Expect(deletablePath).NotTo(BeAnExistingFile())
	})

	It("only deletes unpinned archives when count limit is exceeded", func() {
		base := ts(1)
		p1 := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base, 1024, true)
		d1 := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base.Add(time.Second), 1024, false)
		d2 := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base.Add(2*time.Second), 1024, false)
		// MaxCheckpoints=2, total=3 -> 1 must be deleted, must be one of the deletable ones.
		policy := Policy{
			RetainOrphan:      true,
			MaxCheckpoints:    2,
			MaxCheckpointSize: resource.MustParse("100Gi"),
			MaxTotalSize:      resource.MustParse("100Gi"),
		}

		handleCheckpointsForLevel(devNull, details, "container", policy)

		Expect(p1).To(BeAnExistingFile()) // pinned - never deleted
		// The oldest deletable (d1) should be removed; the newer one (d2) survives.
		Expect(d1).NotTo(BeAnExistingFile())
		Expect(d2).To(BeAnExistingFile())
	})

	It("does not panic when the count excess exceeds the number of deletable archives", func() {
		// 4 pinned + 1 deletable, MaxCheckpoints=2 -> excessCount=3 but only
		// 1 candidate is deletable. The count rotation must clamp instead of
		// indexing past the end of the candidate slice.
		base := ts(1)
		p1 := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base, 1024, true)
		p2 := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base.Add(time.Second), 1024, true)
		p3 := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base.Add(2*time.Second), 1024, true)
		p4 := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base.Add(3*time.Second), 1024, true)
		d1 := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base.Add(4*time.Second), 1024, false)

		policy := Policy{
			RetainOrphan:      true,
			MaxCheckpoints:    2,
			MaxCheckpointSize: resource.MustParse("100Gi"),
			MaxTotalSize:      resource.MustParse("100Gi"),
		}

		Expect(func() {
			handleCheckpointsForLevel(devNull, details, "container", policy)
		}).NotTo(Panic())

		// All pinned archives survive; the single deletable one is removed.
		Expect(p1).To(BeAnExistingFile())
		Expect(p2).To(BeAnExistingFile())
		Expect(p3).To(BeAnExistingFile())
		Expect(p4).To(BeAnExistingFile())
		Expect(d1).NotTo(BeAnExistingFile())
	})

	It("does not delete anything when no limits are exceeded (regression guard)", func() {
		base := ts(1)
		a := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base, 1024, false)
		b := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base.Add(time.Second), 1024, false)

		policy := Policy{
			RetainOrphan:      true,
			MaxCheckpoints:    10,
			MaxCheckpointSize: resource.MustParse("100Gi"),
			MaxTotalSize:      resource.MustParse("100Gi"),
		}

		handleCheckpointsForLevel(devNull, details, "container", policy)

		Expect(a).To(BeAnExistingFile())
		Expect(b).To(BeAnExistingFile())
	})

	It("a .keep file added between GC cycles is respected on the next cycle", func() {
		base := ts(1)
		archive := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base, 1024, false)

		// First cycle: no .keep file, generous limits - archive survives.
		policy1 := Policy{
			RetainOrphan:      true,
			MaxCheckpoints:    10,
			MaxCheckpointSize: resource.MustParse("100Gi"),
			MaxTotalSize:      resource.MustParse("100Gi"),
		}
		handleCheckpointsForLevel(devNull, details, "container", policy1)
		Expect(archive).To(BeAnExistingFile())

		// Pin the archive between cycles.
		Expect(os.WriteFile(archive+".keep", []byte(`{}`), 0o644)).To(Succeed())

		// Second cycle: now pinned - must survive even under tight limits.
		policy2 := Policy{
			RetainOrphan:      true,
			MaxCheckpoints:    10,
			MaxCheckpointSize: resource.MustParse("1"), // 1 byte - archive is larger but pinned
			MaxTotalSize:      resource.MustParse("1"), // 1 byte - total exceeds but pinned
		}
		handleCheckpointsForLevel(devNull, details, "container", policy2)
		Expect(archive).To(BeAnExistingFile())
	})

	It("does not over-delete by count after removing oversized archives", func() {
		// Two oversized + one small archive, none pinned. Once the oversized
		// ones are removed for exceeding MaxCheckpointSize, only the small archive
		// is a valid checkpoint and MaxCheckpoints=1, so it must survive. This
		// guards the checkpointArchivesCounter decrement on oversized deletion:
		// without it the inflated count drives an extra, incorrect deletion.
		base := ts(1)
		big1 := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base, 200*1024*1024, false)
		big2 := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base.Add(time.Second), 200*1024*1024, false)
		small := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base.Add(2*time.Second), 1024, false)

		policy := Policy{
			RetainOrphan:      true,
			MaxCheckpoints:    1,
			MaxCheckpointSize: resource.MustParse("100Mi"),
			MaxTotalSize:      resource.MustParse("100Gi"),
		}

		handleCheckpointsForLevel(devNull, details, "container", policy)

		Expect(big1).NotTo(BeAnExistingFile())
		Expect(big2).NotTo(BeAnExistingFile())
		Expect(small).To(BeAnExistingFile())
	})

	It("deletes only unpinned orphans when RetainOrphan is false", func() {
		// With RetainOrphan=false and no matching resource in the cluster (the
		// unit-test environment has no in-cluster config, so the lookup reports
		// the resource as absent), the orphan-cleanup path runs. Pinned
		// archives must be skipped; unpinned ones must be deleted.
		base := ts(1)
		pinnedPath := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base, 1024, true)
		deletablePath := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base.Add(time.Second), 1024, false)

		policy := Policy{
			RetainOrphan:      false,
			MaxCheckpoints:    10,
			MaxCheckpointSize: resource.MustParse("100Gi"),
			MaxTotalSize:      resource.MustParse("100Gi"),
		}

		handleCheckpointsForLevel(devNull, details, "container", policy)

		Expect(pinnedPath).To(BeAnExistingFile())
		Expect(deletablePath).NotTo(BeAnExistingFile())
	})

	It("skips archives with unreadable metadata during retention scans without logging errors", func() {
		base := ts(1)
		unreadable := makeCheckpointArchiveWithoutIdentity(dir, "mypod", "default", "mycontainer", base)
		valid := makeCheckpointArchive(dir, "mypod", "default", "mycontainer", base.Add(time.Second), 1024, false)

		policy := Policy{
			RetainOrphan:      true,
			MaxCheckpoints:    1,
			MaxCheckpointSize: resource.MustParse("100Gi"),
			MaxTotalSize:      resource.MustParse("100Gi"),
		}
		sink := &recordingLogSink{}

		handleCheckpointsForLevel(logr.New(sink), details, "container", policy)

		Expect(unreadable).To(BeAnExistingFile())
		Expect(valid).To(BeAnExistingFile())
		Expect(sink.errorCount).To(Equal(0))
	})
})

var _ = Describe("handleWriteFinished", func() {
	It("retries archive metadata reads before applying policies", func() {
		sink := &recordingLogSink{}
		ctx := ctrlLog.IntoContext(context.Background(), logr.New(sink))
		expectedDetails := &checkpointDetails{
			namespace: "default",
			pod:       "mypod",
			container: "mycontainer",
		}
		transientErr := errors.New("archive is still being written")
		attempts := 0
		applied := 0

		handleWriteFinishedWithReader(
			ctx,
			fsnotify.Event{Name: "checkpoint-mypod_default-mycontainer.tar"},
			func(log logr.Logger, path string) (*checkpointDetails, error) {
				attempts++
				if attempts == 1 {
					return nil, transientErr
				}
				return expectedDetails, nil
			},
			func(log logr.Logger, details *checkpointDetails) {
				applied++
				Expect(details).To(Equal(expectedDetails))
			},
			3,
			0,
		)

		Expect(attempts).To(Equal(2))
		Expect(applied).To(Equal(1))
		Expect(sink.errorCount).To(Equal(0))
	})

	It("skips unreadable archives after bounded retries without logging errors", func() {
		sink := &recordingLogSink{}
		ctx := ctrlLog.IntoContext(context.Background(), logr.New(sink))
		readErr := errors.New("archive metadata is unreadable")
		attempts := 0
		applied := 0

		handleWriteFinishedWithReader(
			ctx,
			fsnotify.Event{Name: "checkpoint-mypod_default-mycontainer.tar"},
			func(log logr.Logger, path string) (*checkpointDetails, error) {
				attempts++
				return nil, readErr
			},
			func(log logr.Logger, details *checkpointDetails) {
				applied++
			},
			3,
			0,
		)

		Expect(attempts).To(Equal(3))
		Expect(applied).To(Equal(0))
		Expect(sink.errorCount).To(Equal(0))
	})

	It("skips policy application when the context is cancelled after reading metadata", func() {
		sink := &recordingLogSink{}
		ctx, cancel := context.WithCancel(ctrlLog.IntoContext(context.Background(), logr.New(sink)))
		expectedDetails := &checkpointDetails{
			namespace: "default",
			pod:       "mypod",
			container: "mycontainer",
		}
		attempts := 0
		applied := 0

		handleWriteFinishedWithReader(
			ctx,
			fsnotify.Event{Name: "checkpoint-mypod_default-mycontainer.tar"},
			func(log logr.Logger, path string) (*checkpointDetails, error) {
				attempts++
				cancel()
				return expectedDetails, nil
			},
			func(log logr.Logger, details *checkpointDetails) {
				applied++
			},
			3,
			0,
		)

		Expect(attempts).To(Equal(1))
		Expect(applied).To(Equal(0))
		Expect(sink.errorCount).To(Equal(0))
	})

	It("does not read archive metadata after context cancellation", func() {
		sink := &recordingLogSink{}
		ctx, cancel := context.WithCancel(ctrlLog.IntoContext(context.Background(), logr.New(sink)))
		cancel()
		attempts := 0
		applied := 0

		handleWriteFinishedWithReader(
			ctx,
			fsnotify.Event{Name: "checkpoint-mypod_default-mycontainer.tar"},
			func(log logr.Logger, path string) (*checkpointDetails, error) {
				attempts++
				return &checkpointDetails{}, nil
			},
			func(log logr.Logger, details *checkpointDetails) {
				applied++
			},
			3,
			0,
		)

		Expect(attempts).To(Equal(0))
		Expect(applied).To(Equal(0))
		Expect(sink.errorCount).To(Equal(0))
	})

	It("stops waiting for retries when the context is cancelled", func() {
		sink := &recordingLogSink{}
		ctx, cancel := context.WithCancel(ctrlLog.IntoContext(context.Background(), logr.New(sink)))
		readErr := errors.New("archive metadata is unreadable")
		attempts := 0
		applied := 0

		handleWriteFinishedWithReader(
			ctx,
			fsnotify.Event{Name: "checkpoint-mypod_default-mycontainer.tar"},
			func(log logr.Logger, path string) (*checkpointDetails, error) {
				attempts++
				cancel()
				return nil, readErr
			},
			func(log logr.Logger, details *checkpointDetails) {
				applied++
			},
			3,
			time.Hour,
		)

		Expect(attempts).To(Equal(1))
		Expect(applied).To(Equal(0))
		Expect(sink.errorCount).To(Equal(0))
	})
})
