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

package v1

import "testing"

func TestValidateCheckpointPathInDir(t *testing.T) {
	tests := []struct {
		name string
		path string
		dir  string
		ok   bool
	}{
		{
			name: "inside default dir",
			path: "/var/lib/kubelet/checkpoints/checkpoint-a_b-c-t.tar",
			ok:   true,
		},
		{
			name: "empty dir means the default, not disabled",
			path: "/srv/other/checkpoint-a_b-c-t.tar",
			ok:   false,
		},
		{
			name: "inside a configured dir",
			path: "/srv/ckpts/checkpoint-a_b-c-t.tar",
			dir:  "/srv/ckpts",
			ok:   true,
		},
		{
			name: "subdirectory of the allowed dir is rejected",
			path: "/var/lib/kubelet/checkpoints/sub/checkpoint-a_b-c-t.tar",
			ok:   false,
		},
		{
			name: "arbitrary node path is rejected",
			path: "/tmp/evil.tar",
			ok:   false,
		},
		{
			name: "traversal still rejected by shape validation",
			path: "/var/lib/kubelet/checkpoints/../x.tar",
			ok:   false,
		},
		{
			name: "trailing slash on the configured dir is tolerated",
			path: "/srv/ckpts/checkpoint-a_b-c-t.tar",
			dir:  "/srv/ckpts/",
			ok:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCheckpointPathInDir(tc.path, tc.dir)
			if tc.ok && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Errorf("expected rejection of %q in dir %q", tc.path, tc.dir)
			}
		})
	}
}

func TestValidateCheckpointNamespaceHint(t *testing.T) {
	tests := []struct {
		name string
		path string
		ns   string
		ok   bool
	}{
		{
			name: "matching namespace",
			path: "/var/lib/kubelet/checkpoints/checkpoint-redis_default-redis-2026-01-01T00:00:00Z.tar",
			ns:   "default",
			ok:   true,
		},
		{
			name: "namespace with hyphens matches",
			path: "/var/lib/kubelet/checkpoints/checkpoint-redis_team-a-redis-t.tar",
			ns:   "team-a",
			ok:   true,
		},
		{
			name: "foreign namespace rejected",
			path: "/var/lib/kubelet/checkpoints/checkpoint-redis_other-redis-t.tar",
			ns:   "default",
			ok:   false,
		},
		{
			name: "namespace must be followed by a separator",
			path: "/var/lib/kubelet/checkpoints/checkpoint-redis_defaultx-redis-t.tar",
			ns:   "default",
			ok:   false,
		},
		{
			name: "non-convention filename rejected",
			path: "/var/lib/kubelet/checkpoints/random.tar",
			ns:   "default",
			ok:   false,
		},
		{
			name: "missing pod/namespace separator rejected",
			path: "/var/lib/kubelet/checkpoints/checkpoint-nodash.tar",
			ns:   "default",
			ok:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCheckpointNamespaceHint(tc.path, tc.ns)
			if tc.ok && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Errorf("expected rejection of %q for namespace %q", tc.path, tc.ns)
			}
		})
	}
}
