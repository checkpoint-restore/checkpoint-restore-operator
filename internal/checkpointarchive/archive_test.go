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

package checkpointarchive

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tarEntry struct {
	name     string
	data     string
	typeflag byte
	linkname string
}

func writeTar(t *testing.T, entries []tarEntry) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     0o600,
			Size:     int64(len(e.data)),
			Typeflag: typeflag,
			Linkname: e.linkname,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		if typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.data)); err != nil {
				t.Fatalf("write data %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	path := filepath.Join(t.TempDir(), "archive.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	return path
}

func TestUntarFilesExtractsExactMatchesOnly(t *testing.T) {
	src := writeTar(t, []tarEntry{
		{name: "./spec.dump", data: `{"a":1}`},
		{name: "evil-spec.dump", data: `{"evil":1}`},   // substring of nothing we asked for
		{name: "sub/spec.dump", data: `{"nested":1}`},  // wrong path
		{name: "config.dump", data: `{"unrelated":1}`}, // not requested
	})
	dest := t.TempDir()
	if err := UntarFiles(src, dest, []string{"spec.dump"}); err != nil {
		t.Fatalf("UntarFiles: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "spec.dump"))
	if err != nil {
		t.Fatalf("expected spec.dump to be extracted: %v", err)
	}
	if string(data) != `{"a":1}` {
		t.Errorf("spec.dump content = %q", data)
	}
	for _, absent := range []string{"evil-spec.dump", "sub/spec.dump", "config.dump"} {
		if _, err := os.Stat(filepath.Join(dest, absent)); !os.IsNotExist(err) {
			t.Errorf("entry %q should not have been extracted", absent)
		}
	}
}

func TestUntarFilesRejectsTraversal(t *testing.T) {
	src := writeTar(t, []tarEntry{
		{name: "../../escape.dump", data: "x"},
	})
	dest := t.TempDir()
	err := UntarFiles(src, dest, []string{"../../escape.dump"})
	if err == nil || !strings.Contains(err.Error(), "escapes destination") {
		t.Fatalf("expected traversal rejection, got %v", err)
	}
}

func TestUntarFilesRejectsNonRegularEntries(t *testing.T) {
	src := writeTar(t, []tarEntry{
		{name: "spec.dump", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
	})
	err := UntarFiles(src, t.TempDir(), []string{"spec.dump"})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("expected non-regular-file rejection, got %v", err)
	}
}

func TestUntarFilesEnforcesSizeLimit(t *testing.T) {
	big := strings.Repeat("A", MaxExtractedFileSize+1)
	src := writeTar(t, []tarEntry{{name: "spec.dump", data: big}})
	err := UntarFiles(src, t.TempDir(), []string{"spec.dump"})
	if err == nil || !strings.Contains(err.Error(), "extraction limit") {
		t.Fatalf("expected size-limit rejection, got %v", err)
	}
}

func specDumpArchive(t *testing.T, annotations string) string {
	t.Helper()
	return writeTar(t, []tarEntry{
		{name: "spec.dump", data: `{"ociVersion":"1.0.0","annotations":` + annotations + `}`},
	})
}

func TestReadPodNamespace(t *testing.T) {
	tests := []struct {
		name        string
		annotations string
		want        string
		wantErr     bool
	}{
		{
			name:        "cri-o labels annotation",
			annotations: `{"io.kubernetes.cri-o.Labels":"{\"io.kubernetes.pod.namespace\":\"team-a\"}"}`,
			want:        "team-a",
		},
		{
			name:        "containerd sandbox namespace annotation",
			annotations: `{"io.kubernetes.cri.sandbox-namespace":"team-b"}`,
			want:        "team-b",
		},
		{
			name:        "checkpointctl standard annotation",
			annotations: `{"org.criu.checkpoint.pod.namespace":"team-c"}`,
			want:        "team-c",
		},
		{
			name:        "no namespace recorded fails closed",
			annotations: `{"unrelated":"x"}`,
			wantErr:     true,
		},
		{
			name:        "malformed cri-o labels fail closed",
			annotations: `{"io.kubernetes.cri-o.Labels":"not-json"}`,
			wantErr:     true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadPodNamespace(specDumpArchive(t, tc.annotations))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got namespace %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("namespace = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadPodNamespaceMissingArchive(t *testing.T) {
	if _, err := ReadPodNamespace(filepath.Join(t.TempDir(), "missing.tar")); err == nil {
		t.Fatal("expected error for a missing archive")
	}
}

func TestReadBaseImage(t *testing.T) {
	src := writeTar(t, []tarEntry{
		{name: "config.dump", data: `{"rootfsImageName":"docker.io/library/redis:7.0"}`},
	})
	img, err := ReadBaseImage(src)
	if err != nil {
		t.Fatalf("ReadBaseImage: %v", err)
	}
	if img != "docker.io/library/redis:7.0" {
		t.Errorf("image = %q", img)
	}

	empty := writeTar(t, []tarEntry{{name: "config.dump", data: `{}`}})
	if _, err := ReadBaseImage(empty); err == nil {
		t.Fatal("expected error when rootfsImageName is missing")
	}
}
