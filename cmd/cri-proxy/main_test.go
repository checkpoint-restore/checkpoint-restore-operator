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

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type fakeVersionClient struct {
	err error
}

func (f fakeVersionClient) Version(context.Context, *runtimeapi.VersionRequest, ...grpc.CallOption) (*runtimeapi.VersionResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &runtimeapi.VersionResponse{RuntimeName: "test-runtime"}, nil
}

func TestCheckUpstream(t *testing.T) {
	if err := checkUpstream(context.Background(), fakeVersionClient{}); err != nil {
		t.Fatalf("checkUpstream returned error: %v", err)
	}

	wantErr := errors.New("runtime unavailable")
	if err := checkUpstream(context.Background(), fakeVersionClient{err: wantErr}); !errors.Is(err, wantErr) {
		t.Fatalf("checkUpstream error = %v, want %v", err, wantErr)
	}
}

func TestReadyzHandler(t *testing.T) {
	tests := []struct {
		name       string
		client     fakeVersionClient
		wantStatus int
	}{
		{
			name:       "ready when upstream answers",
			client:     fakeVersionClient{},
			wantStatus: http.StatusOK,
		},
		{
			name:       "not ready when upstream fails",
			client:     fakeVersionClient{err: errors.New("runtime unavailable")},
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			rec := httptest.NewRecorder()
			readyzHandler(tc.client, 0)(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

func TestSystemdActivationFDCount(t *testing.T) {
	const currentPID = 1234
	tests := []struct {
		name          string
		listenPID     string
		listenFDs     string
		wantFDs       int
		wantActivated bool
		wantErr       bool
	}{
		{
			name:          "not activated when unset",
			wantActivated: false,
		},
		{
			name:          "not activated for another process",
			listenPID:     "4321",
			listenFDs:     "1",
			wantActivated: false,
		},
		{
			name:          "activated with one socket",
			listenPID:     "1234",
			listenFDs:     "1",
			wantFDs:       1,
			wantActivated: true,
		},
		{
			name:          "missing fd count is malformed activation",
			listenPID:     "1234",
			wantActivated: true,
			wantErr:       true,
		},
		{
			name:          "invalid pid is malformed activation",
			listenPID:     "not-a-pid",
			listenFDs:     "1",
			wantActivated: true,
			wantErr:       true,
		},
		{
			name:          "invalid fd count is malformed activation",
			listenPID:     "1234",
			listenFDs:     "not-a-count",
			wantActivated: true,
			wantErr:       true,
		},
		{
			name:          "negative fd count is rejected",
			listenPID:     "1234",
			listenFDs:     "-1",
			wantActivated: true,
			wantErr:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotFDs, gotActivated, err := systemdActivationFDCount(currentPID, tc.listenPID, tc.listenFDs)
			if gotActivated != tc.wantActivated {
				t.Fatalf("activated = %v, want %v", gotActivated, tc.wantActivated)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotFDs != tc.wantFDs {
				t.Fatalf("fds = %d, want %d", gotFDs, tc.wantFDs)
			}
		})
	}
}
