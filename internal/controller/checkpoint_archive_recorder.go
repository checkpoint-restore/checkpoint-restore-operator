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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
)

// recordCheckpointArchiveIfEnabled creates a CheckpointArchive for a freshly
// created checkpoint when the policy matching its namespace/pod/container
// opts into external storage sync. It is a no-op for checkpoints outside any
// opted-in policy, so callers that never configure external storage see no
// extra API traffic.
func recordCheckpointArchiveIfEnabled(
	ctx context.Context,
	c client.Client,
	namespace, pod, container, node, path string,
) error {
	details := &checkpointDetails{namespace: namespace, pod: pod, container: container}
	if !currentPolicySnapshot().resolveUploadToExternalStorage(details) {
		return nil
	}

	archive := &criuorgv1.CheckpointArchive{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "ckpt-" + pod + "-" + container + "-",
			Namespace:    namespace,
		},
		Spec: criuorgv1.CheckpointArchiveSpec{
			Node:      node,
			LocalPath: path,
			Namespace: namespace,
			Pod:       pod,
			Container: container,
		},
	}
	if err := c.Create(ctx, archive); err != nil {
		return err
	}
	log.FromContext(ctx).Info("recorded checkpoint archive for external storage sync",
		"namespace", namespace, "pod", pod, "container", container, "node", node, "path", path)
	return nil
}
