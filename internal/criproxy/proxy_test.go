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

package criproxy

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

// writeCheckpointArchive writes a minimal checkpoint .tar into dir whose
// spec.dump records namespace the way containerd does, and returns its path.
func writeCheckpointArchive(t *testing.T, dir, name, namespace string) string {
	t.Helper()
	spec := `{"ociVersion":"1.0.0","annotations":{"io.kubernetes.cri.sandbox-namespace":"` + namespace + `"}}`
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "spec.dump", Mode: 0o600, Size: int64(len(spec))}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write([]byte(spec)); err != nil {
		t.Fatalf("write spec.dump: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	return path
}

func req(containerName, image, sandboxNamespace string, ann map[string]string) *runtimeapi.CreateContainerRequest {
	return &runtimeapi.CreateContainerRequest{
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: containerName},
			Image:    &runtimeapi.ImageSpec{Image: image},
		},
		SandboxConfig: &runtimeapi.PodSandboxConfig{
			Metadata:    &runtimeapi.PodSandboxMetadata{Namespace: sandboxNamespace},
			Annotations: ann,
		},
	}
}

func imageOf(r *runtimeapi.CreateContainerRequest) string {
	if r.GetConfig().GetImage() == nil {
		return ""
	}
	return r.GetConfig().GetImage().GetImage()
}

func TestRewriteCreateContainer(t *testing.T) {
	dir := t.TempDir()
	tarPath := writeCheckpointArchive(t, dir, "checkpoint-redis_default-redis-x.tar", "default")
	opts := Options{CheckpointDir: dir}
	ann := map[string]string{criuorgv1.RestoreCheckpointPathAnnotationPrefix + "redis": tarPath}

	tests := []struct {
		name      string
		req       *runtimeapi.CreateContainerRequest
		wantImage string
		wantRet   string
	}{
		{
			name:      "annotation present rewrites image to .tar",
			req:       req("redis", "redis:7.0", "default", ann),
			wantImage: tarPath,
			wantRet:   tarPath,
		},
		{
			name:      "no annotation leaves image unchanged",
			req:       req("redis", "redis:7.0", "default", nil),
			wantImage: "redis:7.0",
			wantRet:   "",
		},
		{
			name:      "annotation for a different container is ignored",
			req:       req("sidecar", "busybox", "default", ann),
			wantImage: "busybox",
			wantRet:   "",
		},
		{
			name:      "empty annotation value is ignored",
			req:       req("redis", "redis:7.0", "default", map[string]string{criuorgv1.RestoreCheckpointPathAnnotationPrefix + "redis": ""}),
			wantImage: "redis:7.0",
			wantRet:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rewriteCreateContainer(tc.req, opts, logr.Discard())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantRet {
				t.Errorf("return = %q, want %q", got, tc.wantRet)
			}
			if img := imageOf(tc.req); img != tc.wantImage {
				t.Errorf("image = %q, want %q", img, tc.wantImage)
			}
		})
	}
}

// A restore annotation carrying an unsafe path must fail closed: the proxy
// returns an error and leaves the request untouched so nothing is forwarded to
// the runtime. This mirrors the controller's checkpoint path validation at the
// node, the last gate before a frozen image is executed.
func TestRewriteCreateContainerRejectsUnsafePath(t *testing.T) {
	for _, bad := range []string{
		"/var/lib/kubelet/checkpoints/../../etc/shadow.tar", // traversal
		"relative/path.tar",                       // not absolute
		"/var/lib/kubelet/checkpoints/cp.img",     // not a .tar
		"/var//lib/cp.tar",                        // redundant separator
		"/tmp/staged-by-attacker.tar",             // outside the checkpoint dir
		"/var/lib/kubelet/checkpoints/sub/cp.tar", // subdirectory of the checkpoint dir
	} {
		r := req("redis", "redis:7.0", "default", map[string]string{
			criuorgv1.RestoreCheckpointPathAnnotationPrefix + "redis": bad,
		})
		got, err := rewriteCreateContainer(r, Options{}, logr.Discard())
		if err == nil {
			t.Errorf("path %q: expected error, got none", bad)
		}
		if got != "" {
			t.Errorf("path %q: expected empty return, got %q", bad, got)
		}
		if img := imageOf(r); img != "redis:7.0" {
			t.Errorf("path %q: image was mutated to %q despite rejection", bad, img)
		}
	}
}

// The namespace recorded inside the archive must match the sandbox namespace;
// a mismatch, an unreadable archive, or a sandbox without a namespace all fail
// closed. AllowCrossNamespace disables only the namespace comparison.
func TestRewriteCreateContainerNamespaceEnforcement(t *testing.T) {
	dir := t.TempDir()
	teamATar := writeCheckpointArchive(t, dir, "checkpoint-redis_team-a-redis-x.tar", "team-a")
	missingTar := filepath.Join(dir, "missing.tar")
	annFor := func(p string) map[string]string {
		return map[string]string{criuorgv1.RestoreCheckpointPathAnnotationPrefix + "redis": p}
	}

	t.Run("matching namespace is allowed", func(t *testing.T) {
		r := req("redis", "redis:7.0", "team-a", annFor(teamATar))
		got, err := rewriteCreateContainer(r, Options{CheckpointDir: dir}, logr.Discard())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != teamATar {
			t.Errorf("return = %q, want %q", got, teamATar)
		}
	})

	t.Run("foreign namespace is denied", func(t *testing.T) {
		r := req("redis", "redis:7.0", "team-b", annFor(teamATar))
		_, err := rewriteCreateContainer(r, Options{CheckpointDir: dir}, logr.Discard())
		if err == nil || !strings.Contains(err.Error(), "belongs to namespace") {
			t.Fatalf("expected namespace denial, got %v", err)
		}
		if img := imageOf(r); img != "redis:7.0" {
			t.Errorf("image was mutated to %q despite denial", img)
		}
	})

	t.Run("unreadable archive fails closed", func(t *testing.T) {
		r := req("redis", "redis:7.0", "team-a", annFor(missingTar))
		_, err := rewriteCreateContainer(r, Options{CheckpointDir: dir}, logr.Discard())
		if err == nil || !strings.Contains(err.Error(), "cannot verify checkpoint namespace") {
			t.Fatalf("expected fail-closed verification error, got %v", err)
		}
	})

	t.Run("sandbox without a namespace fails closed", func(t *testing.T) {
		r := req("redis", "redis:7.0", "", annFor(teamATar))
		_, err := rewriteCreateContainer(r, Options{CheckpointDir: dir}, logr.Discard())
		if err == nil || !strings.Contains(err.Error(), "records no namespace") {
			t.Fatalf("expected fail-closed sandbox error, got %v", err)
		}
	})

	t.Run("AllowCrossNamespace skips the namespace comparison", func(t *testing.T) {
		r := req("redis", "redis:7.0", "team-b", annFor(teamATar))
		got, err := rewriteCreateContainer(r, Options{CheckpointDir: dir, AllowCrossNamespace: true}, logr.Discard())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != teamATar {
			t.Errorf("return = %q, want %q", got, teamATar)
		}
	})

	t.Run("AllowCrossNamespace still confines to the checkpoint dir", func(t *testing.T) {
		r := req("redis", "redis:7.0", "team-b", annFor("/tmp/evil.tar"))
		_, err := rewriteCreateContainer(r, Options{CheckpointDir: dir, AllowCrossNamespace: true}, logr.Discard())
		if err == nil {
			t.Fatal("expected rejection of a path outside the checkpoint dir")
		}
	})
}

func TestRewriteCreateContainerPreservesImageSpec(t *testing.T) {
	dir := t.TempDir()
	tarPath := writeCheckpointArchive(t, dir, "checkpoint-redis_default-redis-x.tar", "default")
	r := req("redis", "redis:7.0", "default", map[string]string{
		criuorgv1.RestoreCheckpointPathAnnotationPrefix + "redis": tarPath,
	})
	r.Config.Image = &runtimeapi.ImageSpec{
		Image:              "redis:7.0",
		UserSpecifiedImage: "redis:7.0",
		RuntimeHandler:     "kata",
		Annotations: map[string]string{
			"example.com/key": "value",
		},
	}

	got, err := rewriteCreateContainer(r, Options{CheckpointDir: dir}, logr.Discard())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != tarPath {
		t.Fatalf("return = %q, want %q", got, tarPath)
	}
	if r.Config.Image.GetImage() != tarPath {
		t.Fatalf("image = %q, want %q", r.Config.Image.GetImage(), tarPath)
	}
	if r.Config.Image.GetUserSpecifiedImage() != "redis:7.0" {
		t.Errorf("user specified image was not preserved")
	}
	if r.Config.Image.GetRuntimeHandler() != "kata" {
		t.Errorf("runtime handler was not preserved")
	}
	if got := r.Config.Image.GetAnnotations()["example.com/key"]; got != "value" {
		t.Errorf("image annotation = %q, want value", got)
	}
}

// Must not panic on missing config/metadata/sandbox.
func TestRewriteCreateContainerNilSafe(t *testing.T) {
	for _, r := range []*runtimeapi.CreateContainerRequest{
		{},
		{Config: &runtimeapi.ContainerConfig{}},
		{Config: &runtimeapi.ContainerConfig{Metadata: &runtimeapi.ContainerMetadata{Name: "x"}}},
	} {
		got, err := rewriteCreateContainer(r, Options{}, logr.Discard())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("expected no rewrite, got %q", got)
		}
	}
}
