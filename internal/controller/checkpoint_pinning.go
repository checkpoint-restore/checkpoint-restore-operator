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
	"os"

	"github.com/go-logr/logr"
)

// isCheckpointPinned reports whether a checkpoint archive has a .keep marker file.
// Existence of the marker is sufficient; its content is not validated.
// The GC never creates or deletes .keep files - it only reads them.
// Pinning state is re-read from disk on every GC cycle (no in-memory cache).
func isCheckpointPinned(archivePath string) bool {
	_, err := os.Stat(archivePath + ".keep")
	return err == nil
}

// removeArchiveUnlessPinned deletes a checkpoint archive on behalf of the
// retention GC, re-checking the .keep marker under the pin mutex immediately
// before the unlink. Candidate selection happens earlier and unlocked, so
// without this re-check a pin written by the PodRestore controller between
// selection and deletion would be ignored and an in-use archive deleted.
func removeArchiveUnlessPinned(log logr.Logger, archive string) {
	podRestorePinMu.Lock()
	defer podRestorePinMu.Unlock()
	if isCheckpointPinned(archive) {
		log.Info("checkpoint became pinned after selection; skipping deletion", "archive", archive)
		return
	}
	if err := os.Remove(archive); err != nil {
		log.Error(err, "removal of checkpoint archive failed", "archive", archive)
	}
}

// partitionArchives splits archive paths into deletable and pinned slices.
// Order is preserved from input; selectArchivesToDelete handles its own sorting.
func partitionArchives(archives []string) (deletable, pinned []string) {
	for _, a := range archives {
		if isCheckpointPinned(a) {
			pinned = append(pinned, a)
		} else {
			deletable = append(deletable, a)
		}
	}
	return
}
