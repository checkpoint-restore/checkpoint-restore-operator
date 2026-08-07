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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestPostMapEncodesConfig(t *testing.T) {
	var got CoordinatorConfig
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	cfg := CoordinatorConfig{Address: "10.0.0.1", Port: "8080", Dependencies: DependencyMap{"a": {"b"}}}
	if err := postMap(context.Background(), srv.Client(), u.Host, cfg); err != nil {
		t.Fatalf("postMap: %v", err)
	}
	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}
	if got.Address != "10.0.0.1" || got.Dependencies["a"][0] != "b" {
		t.Errorf("server got %+v", got)
	}
}

func TestPostMapNonOKIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	if err := postMap(context.Background(), srv.Client(), u.Host, CoordinatorConfig{}); err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}
