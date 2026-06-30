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
	"testing"

	"github.com/go-logr/logr"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

const tarPath = "/var/lib/kubelet/checkpoints/checkpoint-redis_default-redis-x.tar"

func req(containerName, image string, ann map[string]string) *runtimeapi.CreateContainerRequest {
	return &runtimeapi.CreateContainerRequest{
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: containerName},
			Image:    &runtimeapi.ImageSpec{Image: image},
		},
		SandboxConfig: &runtimeapi.PodSandboxConfig{Annotations: ann},
	}
}

func imageOf(r *runtimeapi.CreateContainerRequest) string {
	if r.GetConfig().GetImage() == nil {
		return ""
	}
	return r.GetConfig().GetImage().GetImage()
}

func TestRewriteCreateContainer(t *testing.T) {
	ann := map[string]string{criuorgv1.RestoreCheckpointPathAnnotationPrefix + "redis": tarPath}

	tests := []struct {
		name      string
		req       *runtimeapi.CreateContainerRequest
		wantImage string
		wantRet   string
	}{
		{
			name:      "annotation present rewrites image to .tar",
			req:       req("redis", "redis:7.0", ann),
			wantImage: tarPath,
			wantRet:   tarPath,
		},
		{
			name:      "no annotation leaves image unchanged",
			req:       req("redis", "redis:7.0", nil),
			wantImage: "redis:7.0",
			wantRet:   "",
		},
		{
			name:      "annotation for a different container is ignored",
			req:       req("sidecar", "busybox", ann),
			wantImage: "busybox",
			wantRet:   "",
		},
		{
			name:      "empty annotation value is ignored",
			req:       req("redis", "redis:7.0", map[string]string{criuorgv1.RestoreCheckpointPathAnnotationPrefix + "redis": ""}),
			wantImage: "redis:7.0",
			wantRet:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rewriteCreateContainer(tc.req, logr.Discard())
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
// the runtime. This mirrors the controller's validateCheckpointPath check at the
// node, the last gate before a frozen image is executed.
func TestRewriteCreateContainerRejectsUnsafePath(t *testing.T) {
	for _, bad := range []string{
		"/var/lib/kubelet/checkpoints/../../etc/shadow.tar", // traversal
		"relative/path.tar",                   // not absolute
		"/var/lib/kubelet/checkpoints/cp.img", // not a .tar
		"/var//lib/cp.tar",                    // redundant separator
	} {
		r := req("redis", "redis:7.0", map[string]string{
			criuorgv1.RestoreCheckpointPathAnnotationPrefix + "redis": bad,
		})
		got, err := rewriteCreateContainer(r, logr.Discard())
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

func TestRewriteCreateContainerPreservesImageSpec(t *testing.T) {
	r := req("redis", "redis:7.0", map[string]string{
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

	got, err := rewriteCreateContainer(r, logr.Discard())
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
		got, err := rewriteCreateContainer(r, logr.Discard())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("expected no rewrite, got %q", got)
		}
	}
}
