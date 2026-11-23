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

// Package checkpointstore keeps container checkpoints in object storage so
// that they outlive the node that took them.
//
// A Store holds snapshots under a root in one bucket:
//
//	<root>/<namespace>/<workload-kind>/<workload-name>/<pod>/<container>/<id>/manifest.json
//	<root>/<namespace>/<workload-kind>/<workload-name>/<pod>/<container>/<id>/data/<file>
//
// Snapshot IDs sort by creation time. A snapshot exists only once its
// manifest does: Write uploads every data file before the manifest, and
// Delete removes the manifest before the data, so a reader never sees a
// partial snapshot and no backend needs a rename or a transaction. The
// manifest records each file's size and SHA-256, which Fetch checks.
//
// Buckets are opened from a location URL: s3://bucket/root, gs://bucket/root
// or azblob://container/root?account=name. See Open.
package checkpointstore
