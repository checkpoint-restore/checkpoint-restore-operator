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

// Package checkpointarchive reads metadata out of container checkpoint
// archives (the .tar files the kubelet checkpoint API produces). It is shared
// by the operator's controllers and the node-side CRI proxy so that both
// sides apply the same hardened extraction rules to archives they do not
// trust: exact entry-name matching, regular files only, traversal guards, and
// a per-file size bound.
package checkpointarchive

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	metadata "github.com/checkpoint-restore/checkpointctl/lib"
	"github.com/containers/storage/pkg/archive"
	kubelettypes "k8s.io/kubelet/pkg/types"
)

// MaxExtractedFileSize bounds any single file extracted from a checkpoint
// archive. The files this package extracts (config.dump, spec.dump) are small
// JSON documents; the bound exists so a crafted or corrupted archive with a
// huge (or decompression-bomb) entry cannot fill the disk of whichever
// component inspects it. Extraction fails when the bound is exceeded rather
// than silently truncating, because a truncated JSON document would produce a
// misleading parse error.
const MaxExtractedFileSize = 16 << 20 // 16 MiB

// criOLabelsAnnotation is the spec.dump annotation under which CRI-O records
// the kubelet-standard container labels as a JSON map.
const criOLabelsAnnotation = "io.kubernetes.cri-o.Labels"

// containerdSandboxNamespaceAnnotation is the spec.dump annotation under
// which containerd records the namespace of the pod sandbox.
const containerdSandboxNamespaceAnnotation = "io.kubernetes.cri.sandbox-namespace"

// UntarFiles extracts the named entries from the archive src into dest.
//
// Entry names are compared exactly after lexical cleaning ("./spec.dump"
// matches "spec.dump"). This is deliberately not a substring match: an entry
// named "evil-spec.dump" or "x/spec.dump" is not extracted. Only regular-file
// entries are accepted; a matching entry of any other type (symlink,
// directory, device) is an error, because the callers parse the result as
// JSON and anything else indicates a crafted archive. Entries that escape
// dest ("zip slip") are rejected, and each file is limited to
// MaxExtractedFileSize after decompression.
func UntarFiles(src, dest string, files []string) error {
	want := make(map[string]bool, len(files))
	for _, f := range files {
		want[filepath.Clean(f)] = true
	}
	cleanDest := filepath.Clean(dest)

	if err := iterateTarArchive(src, func(r *tar.Reader, header *tar.Header) error {
		name := filepath.Clean(header.Name)
		if !want[name] {
			return nil
		}
		if header.Typeflag != tar.TypeReg {
			return fmt.Errorf("archive entry %q is not a regular file (type %q)", header.Name, header.Typeflag)
		}
		// Exact matching against clean relative names already prevents
		// traversal; keep the explicit guard as defense in depth.
		target := filepath.Join(cleanDest, name)
		if target != cleanDest && !strings.HasPrefix(target, cleanDest+string(os.PathSeparator)) {
			return fmt.Errorf("archive entry %q escapes destination directory", header.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		destFile, err := os.Create(target)
		if err != nil {
			return err
		}
		n, err := io.Copy(destFile, io.LimitReader(r, MaxExtractedFileSize+1))
		closeErr := destFile.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if n > MaxExtractedFileSize {
			return fmt.Errorf("archive entry %q exceeds the %d-byte extraction limit", header.Name, MaxExtractedFileSize)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("unpacking of checkpoint archive failed: %w", err)
	}

	return nil
}

// iterateTarArchive reads a tar archive from the specified input file,
// decompresses it, and iterates through each entry, invoking the provided
// callback function.
func iterateTarArchive(archiveInput string, callback func(r *tar.Reader, header *tar.Header) error) error {
	archiveFile, err := os.Open(archiveInput)
	if err != nil {
		return err
	}
	defer func() { _ = archiveFile.Close() }()

	stream, err := archive.DecompressStream(archiveFile)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()

	tarReader := tar.NewReader(stream)
	for {
		header, err := tarReader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}

		if err = callback(tarReader, header); err != nil {
			return err
		}
	}

	return nil
}

// extractSpecDump extracts and parses the archive's spec.dump (the OCI
// runtime spec recorded at checkpoint time) in a private temp dir.
func extractSpecDump(checkpointPath string) (map[string]string, error) {
	tempDir, err := os.MkdirTemp("", "ckpt-spec-dump")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	if err := UntarFiles(checkpointPath, tempDir, []string{metadata.SpecDumpFile}); err != nil {
		return nil, err
	}
	specDump, _, err := metadata.ReadContainerCheckpointSpecDump(tempDir)
	if err != nil {
		return nil, err
	}
	return specDump.Annotations, nil
}

// ReadPodNamespace returns the namespace of the pod the checkpoint was taken
// from, as recorded inside the archive's spec.dump by the container runtime.
// It understands the three places runtimes record it: CRI-O's kubelet-labels
// JSON annotation, containerd's sandbox-namespace annotation, and the
// checkpointctl-standard annotation. It fails (rather than returning "") when
// none is present, so callers that use it as an authorization signal fail
// closed on hand-crafted archives.
func ReadPodNamespace(checkpointPath string) (string, error) {
	annotations, err := extractSpecDump(checkpointPath)
	if err != nil {
		return "", fmt.Errorf("reading spec.dump from %s: %w", checkpointPath, err)
	}

	// CRI-O: kubelet labels as a JSON map.
	if raw := annotations[criOLabelsAnnotation]; raw != "" {
		labels := map[string]string{}
		if err := json.Unmarshal([]byte(raw), &labels); err != nil {
			return "", fmt.Errorf("parsing %q in spec.dump of %s: %w", criOLabelsAnnotation, checkpointPath, err)
		}
		if ns := labels[kubelettypes.KubernetesPodNamespaceLabel]; ns != "" {
			return ns, nil
		}
	}
	// containerd: sandbox namespace recorded directly.
	if ns := annotations[containerdSandboxNamespaceAnnotation]; ns != "" {
		return ns, nil
	}
	// checkpointctl-standard annotation (Podman and newer engines).
	if ns := annotations[metadata.CheckpointAnnotationNamespace]; ns != "" {
		return ns, nil
	}
	return "", fmt.Errorf("checkpoint %s records no pod namespace in spec.dump", checkpointPath)
}

// ReadBaseImage returns the base (rootfs) image name recorded in the
// checkpoint archive's config.dump.
func ReadBaseImage(checkpointPath string) (string, error) {
	tempDir, err := os.MkdirTemp("", "ckpt-config-dump")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	if err := UntarFiles(checkpointPath, tempDir, []string{metadata.ConfigDumpFile}); err != nil {
		return "", err
	}
	cfg, _, err := metadata.ReadContainerCheckpointConfigDump(tempDir)
	if err != nil {
		return "", fmt.Errorf("reading checkpoint config.dump: %w", err)
	}
	if cfg.RootfsImageName == "" {
		return "", fmt.Errorf("checkpoint %s records no rootfsImageName", checkpointPath)
	}
	return cfg.RootfsImageName, nil
}
