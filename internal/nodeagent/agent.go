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

// Package nodeagent implements the node-side component that manages the
// criu-coordinator files on a node: it ensures the CRIU action-script is
// installed and writes/clears the per-run dependency map that the coordinator
// client reads from /etc/criu/criu-coordinator.json.
package nodeagent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// Agent manages the node-local criu-coordinator files.
type Agent struct {
	ConfigPath      string // /etc/criu/criu-coordinator.json
	DefaultConfPath string // /etc/criu/default.conf
	ScriptPath      string // /usr/local/bin/criu-coordinator
}

// coordinatorConfig mirrors the operator's CoordinatorConfig JSON.
type coordinatorConfig struct {
	Address      string              `json:"address"`
	Port         string              `json:"port"`
	Dependencies map[string][]string `json:"dependencies"`
}

// EnsureActionScript makes sure the action-script line is present exactly once
// in DefaultConfPath. Idempotent.
func (a *Agent) EnsureActionScript() error {
	line := "action-script " + a.ScriptPath
	existing, err := os.ReadFile(a.DefaultConfPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if strings.Contains(string(existing), line) {
		return nil
	}
	content := string(existing)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += line + "\n"
	return os.WriteFile(a.DefaultConfPath, []byte(content), 0o644)
}

// Handler serves POST /map (write) and DELETE /map (clear).
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/map", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			a.handleWrite(w, r)
		case http.MethodDelete:
			a.handleClear(w)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func (a *Agent) handleWrite(w http.ResponseWriter, r *http.Request) {
	var cfg coordinatorConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, fmt.Sprintf("invalid config JSON: %v", err), http.StatusBadRequest)
		return
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Atomic write: temp file + rename.
	tmp := a.ConfigPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmp, a.ConfigPath); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *Agent) handleClear(w http.ResponseWriter) {
	if err := os.Remove(a.ConfigPath); err != nil && !os.IsNotExist(err) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
