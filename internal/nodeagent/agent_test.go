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

package nodeagent

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestAgent(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()
	return &Agent{
		ConfigPath:      filepath.Join(dir, "criu-coordinator.json"),
		DefaultConfPath: filepath.Join(dir, "default.conf"),
		ScriptPath:      "/usr/local/bin/criu-coordinator",
	}
}

func TestPostThenDeleteMap(t *testing.T) {
	a := newTestAgent(t)
	h := a.Handler()

	body := `{"address":"10.0.0.1","port":"8080","dependencies":{"a":["b"]}}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/map", bytes.NewBufferString(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d", rec.Code)
	}
	got, err := os.ReadFile(a.ConfigPath)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if !strings.Contains(string(got), `"10.0.0.1"`) {
		t.Errorf("config missing address: %s", got)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/map", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d", rec.Code)
	}
	if _, err := os.Stat(a.ConfigPath); !os.IsNotExist(err) {
		t.Errorf("config still present after DELETE")
	}
}

func TestPostRejectsInvalidJSON(t *testing.T) {
	a := newTestAgent(t)
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/map", bytes.NewBufferString("not json")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for bad JSON, got %d", rec.Code)
	}
}

func TestEnsureActionScriptIsIdempotent(t *testing.T) {
	a := newTestAgent(t)
	for i := 0; i < 2; i++ {
		if err := a.EnsureActionScript(); err != nil {
			t.Fatalf("EnsureActionScript: %v", err)
		}
	}
	got, _ := os.ReadFile(a.DefaultConfPath)
	if strings.Count(string(got), "action-script "+a.ScriptPath) != 1 {
		t.Errorf("action-script line not exactly once: %q", got)
	}
}
