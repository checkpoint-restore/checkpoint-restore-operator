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

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestExternalStorageSpecJSONRoundTrip(t *testing.T) {
	spec := CheckpointRestoreOperatorSpec{
		ExternalStorage: &ExternalStorageSpec{
			Backend:   "s3",
			Bucket:    "checkpoints",
			Endpoint:  "https://minio.example.com",
			Region:    "us-east-1",
			SecretRef: corev1.LocalObjectReference{Name: "checkpoint-syncer-creds"},
		},
	}

	data, err := json.Marshal(&spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded CheckpointRestoreOperatorSpec
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.ExternalStorage == nil {
		t.Fatal("expected externalStorage to survive round trip")
	}
	if *decoded.ExternalStorage != *spec.ExternalStorage {
		t.Errorf("round trip mismatch: got %+v, want %+v", *decoded.ExternalStorage, *spec.ExternalStorage)
	}
}

func TestExternalStorageSpecOmittedWhenUnset(t *testing.T) {
	spec := CheckpointRestoreOperatorSpec{}

	data, err := json.Marshal(&spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, present := raw["externalStorage"]; present {
		t.Error("externalStorage should be omitted from JSON when unset, so existing CRs are unaffected")
	}
}

func TestCheckpointRestoreOperatorSpecDeepCopyIsIndependent(t *testing.T) {
	original := &CheckpointRestoreOperatorSpec{
		ExternalStorage: &ExternalStorageSpec{
			Backend: "s3",
			Bucket:  "checkpoints",
		},
	}

	clone := original.DeepCopy()
	clone.ExternalStorage.Bucket = "other-bucket"

	if original.ExternalStorage.Bucket != "checkpoints" {
		t.Error("mutating the deep copy's ExternalStorage must not affect the original")
	}
}
