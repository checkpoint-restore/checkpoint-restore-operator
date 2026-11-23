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

package checkpointstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ManifestVersion is the manifest schema this package writes and reads.
const ManifestVersion = 1

const (
	manifestName = "manifest.json"
	dataDir      = "data"
	// maxManifestSize bounds how much of a manifest Read loads, so a
	// corrupt or hostile object cannot exhaust memory.
	maxManifestSize = 16 << 20
)

// snapshotIDPattern matches the IDs newSnapshotID creates.
var snapshotIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}\.[0-9]{9}Z-[0-9a-f]{8}$`)

// Key identifies the container whose snapshots share one prefix. Every field
// is one path segment, so none may be empty, ".", ".." or contain a slash.
type Key struct {
	Namespace    string `json:"namespace"`
	WorkloadKind string `json:"workloadKind"`
	WorkloadName string `json:"workloadName"`
	Pod          string `json:"pod"`
	Container    string `json:"container"`
}

func (k Key) segments() []string {
	return []string{k.Namespace, k.WorkloadKind, k.WorkloadName, k.Pod, k.Container}
}

func (k Key) validate() error {
	for _, s := range k.segments() {
		if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\\") {
			return fmt.Errorf("invalid snapshot key segment %q", s)
		}
	}
	return nil
}

// File is one data file of a snapshot.
type File struct {
	// Path is the file's slash-separated path inside the snapshot.
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest describes one complete snapshot.
type Manifest struct {
	Version int `json:"version"`
	// ID orders a container's snapshots by creation time.
	ID  string `json:"id"`
	Key Key    `json:"key"`
	// Format names the layout of the data files, such as the producer's
	// checkpoint format. The store does not interpret it.
	Format string `json:"format"`
	// Producer names the component that took the snapshot.
	Producer string `json:"producer"`
	// Reason says why the snapshot was taken, such as "interval" or
	// "pod eviction".
	Reason     string    `json:"reason,omitempty"`
	SourceNode string    `json:"sourceNode,omitempty"`
	PodUID     string    `json:"podUID,omitempty"`
	Image      string    `json:"image,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	Files      []File    `json:"files"`
	// Signature is an optional detached signature over the manifest with
	// this field empty. The store does not create or check it.
	Signature string `json:"signature,omitempty"`
}

// Store reads and writes snapshots under a root in one bucket.
type Store struct {
	bucket Bucket
	root   string
}

// New returns a Store keeping snapshots under root in bucket.
func New(bucket Bucket, root string) *Store {
	return &Store{bucket: bucket, root: strings.Trim(root, "/")}
}

// OpenLocation parses a location URL and opens the Store it names.
func OpenLocation(ctx context.Context, raw string) (*Store, error) {
	loc, err := ParseLocation(raw)
	if err != nil {
		return nil, err
	}
	bucket, err := Open(ctx, loc)
	if err != nil {
		return nil, err
	}
	return New(bucket, loc.Root), nil
}

// keyPrefix returns the prefix all of key's snapshots are stored under,
// ending in a slash.
func (s *Store) keyPrefix(k Key) string {
	return path.Join(append([]string{s.root}, k.segments()...)...) + "/"
}

// Path returns the bucket path of a snapshot, which Fetch, Read and Delete
// take. It is what a caller records to find the snapshot again.
func (s *Store) Path(m Manifest) string {
	return s.keyPrefix(m.Key) + m.ID
}

// Write uploads the named files of dir as a new snapshot and returns its
// manifest. m supplies the Key and the descriptive fields; Write sets
// Version, ID, CreatedAt and Files. files are slash-separated paths relative
// to dir. The snapshot becomes visible only when every file and then the
// manifest have been stored. If Write fails, it deletes the data it stored,
// even when ctx has ended; data left behind by a process that died during a
// Write has no manifest and is ignored by readers.
func (s *Store) Write(ctx context.Context, m Manifest, dir string, files []string) (_ Manifest, err error) {
	if err := m.Key.validate(); err != nil {
		return Manifest{}, err
	}
	for _, name := range files {
		if !filepath.IsLocal(filepath.FromSlash(name)) {
			return Manifest{}, fmt.Errorf("snapshot file %q is not a local path", name)
		}
	}
	id, err := newSnapshotID(time.Now())
	if err != nil {
		return Manifest{}, err
	}
	m.Version = ManifestVersion
	m.ID = id
	m.CreatedAt = time.Now().UTC()
	m.Files = make([]File, 0, len(files))
	m.Signature = ""
	prefix := s.Path(m)

	var stored []string
	defer func() {
		if err != nil {
			s.deleteKeys(ctx, stored)
		}
	}()
	for _, name := range files {
		key := prefix + "/" + dataDir + "/" + name
		// Recorded before the upload, which may leave an object behind
		// even when it fails.
		stored = append(stored, key)
		f, err := s.putFile(ctx, filepath.Join(dir, filepath.FromSlash(name)), key)
		if err != nil {
			return Manifest{}, err
		}
		f.Path = name
		m.Files = append(m.Files, f)
	}

	data, err := json.Marshal(m)
	if err != nil {
		return Manifest{}, fmt.Errorf("encoding manifest: %w", err)
	}
	stored = append(stored, prefix+"/"+manifestName)
	if err := s.bucket.Put(ctx, prefix+"/"+manifestName, bytes.NewReader(data), int64(len(data))); err != nil {
		return Manifest{}, fmt.Errorf("storing manifest of snapshot %s: %w", prefix, err)
	}
	return m, nil
}

// putFile uploads the local file at name to key and returns its size and
// digest as uploaded.
func (s *Store) putFile(ctx context.Context, name, key string) (File, error) {
	f, err := os.Open(name)
	if err != nil {
		return File{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return File{}, err
	}
	if !info.Mode().IsRegular() {
		return File{}, fmt.Errorf("snapshot file %s is not a regular file", name)
	}
	digest := sha256.New()
	counted := &countingReader{r: io.TeeReader(f, digest)}
	if err := s.bucket.Put(ctx, key, counted, info.Size()); err != nil {
		return File{}, fmt.Errorf("storing %s: %w", name, err)
	}
	// A file that changed size while it was read would upload content the
	// recorded size does not describe.
	if counted.n != info.Size() {
		return File{}, fmt.Errorf("%s changed size while it was stored", name)
	}
	return File{Size: info.Size(), SHA256: hex.EncodeToString(digest.Sum(nil))}, nil
}

// deleteCleanupTimeout bounds how long a failed Write spends deleting the
// data it stored.
const deleteCleanupTimeout = 30 * time.Second

// deleteKeys deletes keys on a best-effort basis. It does not run under ctx
// alone, because a Write that failed when ctx ended still has data to remove.
func (s *Store) deleteKeys(ctx context.Context, keys []string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteCleanupTimeout)
	defer cancel()
	for _, key := range keys {
		_ = s.bucket.Delete(ctx, key)
	}
}

// Read returns the manifest of the snapshot at snapshotPath, or an error
// wrapping ErrNotExist if it has none.
func (s *Store) Read(ctx context.Context, snapshotPath string) (Manifest, error) {
	if err := s.checkPath(snapshotPath); err != nil {
		return Manifest{}, err
	}
	r, err := s.bucket.Get(ctx, snapshotPath+"/"+manifestName)
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(io.LimitReader(r, maxManifestSize+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("reading manifest of %s: %w", snapshotPath, err)
	}
	if len(data) > maxManifestSize {
		return Manifest{}, fmt.Errorf("manifest of %s is larger than %d bytes", snapshotPath, maxManifestSize)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("decoding manifest of %s: %w", snapshotPath, err)
	}
	if m.Version != ManifestVersion {
		return Manifest{}, fmt.Errorf("manifest of %s has unsupported version %d", snapshotPath, m.Version)
	}
	if err := m.Key.validate(); err != nil {
		return Manifest{}, fmt.Errorf("manifest of %s: %w", snapshotPath, err)
	}
	if s.Path(m) != snapshotPath {
		return Manifest{}, fmt.Errorf("manifest of %s describes snapshot %s", snapshotPath, s.Path(m))
	}
	return m, nil
}

// checkPath rejects a snapshot path that is not one of this store's, so that
// a path read from an untrusted record cannot reach another part of the
// bucket.
func (s *Store) checkPath(snapshotPath string) error {
	rest := snapshotPath
	if s.root != "" {
		var ok bool
		rest, ok = strings.CutPrefix(snapshotPath, s.root+"/")
		if !ok {
			return fmt.Errorf("snapshot path %q is outside the store root %q", snapshotPath, s.root)
		}
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 6 || !snapshotIDPattern.MatchString(parts[5]) {
		return fmt.Errorf("snapshot path %q is not a snapshot of this store", snapshotPath)
	}
	k := Key{parts[0], parts[1], parts[2], parts[3], parts[4]}
	return k.validate()
}

// List returns the manifests of key's complete snapshots, oldest first.
// Snapshots without a manifest are left out.
func (s *Store) List(ctx context.Context, k Key) ([]Manifest, error) {
	if err := k.validate(); err != nil {
		return nil, err
	}
	prefix := s.keyPrefix(k)
	var ids []string
	err := s.bucket.List(ctx, prefix, func(key string) error {
		id, name, ok := strings.Cut(strings.TrimPrefix(key, prefix), "/")
		if ok && name == manifestName && snapshotIDPattern.MatchString(id) {
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing snapshots under %s: %w", prefix, err)
	}
	slices.Sort(ids)
	manifests := make([]Manifest, 0, len(ids))
	for _, id := range ids {
		m, err := s.Read(ctx, prefix+id)
		if errors.Is(err, ErrNotExist) {
			// Deleted after it was listed.
			continue
		}
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, m)
	}
	return manifests, nil
}

// Keys returns every container that has at least one complete snapshot in
// the store, sorted by path.
func (s *Store) Keys(ctx context.Context) ([]Key, error) {
	prefix := ""
	if s.root != "" {
		prefix = s.root + "/"
	}
	seen := make(map[Key]bool)
	var keys []Key
	err := s.bucket.List(ctx, prefix, func(key string) error {
		parts := strings.Split(strings.TrimPrefix(key, prefix), "/")
		if len(parts) != 7 || parts[6] != manifestName || !snapshotIDPattern.MatchString(parts[5]) {
			return nil
		}
		k := Key{parts[0], parts[1], parts[2], parts[3], parts[4]}
		if k.validate() != nil || seen[k] {
			return nil
		}
		seen[k] = true
		keys = append(keys, k)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing snapshots under %q: %w", prefix, err)
	}
	slices.SortFunc(keys, func(a, b Key) int {
		return strings.Compare(strings.Join(a.segments(), "/"), strings.Join(b.segments(), "/"))
	})
	return keys, nil
}

// Latest returns the newest complete snapshot of key, and false if it has
// none.
func (s *Store) Latest(ctx context.Context, k Key) (Manifest, bool, error) {
	manifests, err := s.List(ctx, k)
	if err != nil || len(manifests) == 0 {
		return Manifest{}, false, err
	}
	return manifests[len(manifests)-1], true, nil
}

// Fetch downloads the snapshot at snapshotPath into dir, which must exist
// and should be empty, and returns its manifest and the number of bytes
// written. Each file is checked against the size and digest in the manifest;
// on any error dir may hold some of the files, and the caller removes it.
func (s *Store) Fetch(ctx context.Context, snapshotPath, dir string) (Manifest, int64, error) {
	m, err := s.Read(ctx, snapshotPath)
	if err != nil {
		return Manifest{}, 0, err
	}
	var written int64
	for _, f := range m.Files {
		local := filepath.FromSlash(f.Path)
		if !filepath.IsLocal(local) {
			return m, written, fmt.Errorf("snapshot %s lists file %q outside its directory", snapshotPath, f.Path)
		}
		n, err := s.fetchFile(ctx, snapshotPath+"/"+dataDir+"/"+f.Path, filepath.Join(dir, local), f)
		written += n
		if err != nil {
			return m, written, err
		}
	}
	return m, written, nil
}

func (s *Store) fetchFile(ctx context.Context, key, name string, want File) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return 0, err
	}
	r, err := s.bucket.Get(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("downloading %s: %w", key, err)
	}
	defer func() { _ = r.Close() }()
	out, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	digest := sha256.New()
	// Read one byte past the recorded size, so an object longer than its
	// manifest says fails the size check instead of being cut short.
	n, err := io.Copy(io.MultiWriter(out, digest), io.LimitReader(r, want.Size+1))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return n, fmt.Errorf("downloading %s: %w", key, err)
	}
	return n, verify(key, n, digest, want)
}

func verify(key string, n int64, digest hash.Hash, want File) error {
	if n != want.Size {
		return fmt.Errorf("%s has %d bytes, its manifest records %d", key, n, want.Size)
	}
	if got := hex.EncodeToString(digest.Sum(nil)); got != want.SHA256 {
		return fmt.Errorf("%s has SHA-256 %s, its manifest records %s", key, got, want.SHA256)
	}
	return nil
}

// Delete removes the snapshot at snapshotPath. The manifest goes first, so
// the snapshot disappears for readers before its data does. Deleting a
// snapshot that does not exist succeeds.
func (s *Store) Delete(ctx context.Context, snapshotPath string) error {
	if err := s.checkPath(snapshotPath); err != nil {
		return err
	}
	if err := s.bucket.Delete(ctx, snapshotPath+"/"+manifestName); err != nil {
		return fmt.Errorf("deleting manifest of %s: %w", snapshotPath, err)
	}
	var keys []string
	if err := s.bucket.List(ctx, snapshotPath+"/", func(key string) error {
		keys = append(keys, key)
		return nil
	}); err != nil {
		return fmt.Errorf("listing data of %s: %w", snapshotPath, err)
	}
	for _, key := range keys {
		if err := s.bucket.Delete(ctx, key); err != nil {
			return fmt.Errorf("deleting %s: %w", key, err)
		}
	}
	return nil
}

// newSnapshotID returns an ID that sorts by t, with a random suffix that
// keeps two snapshots taken in the same nanosecond apart.
func newSnapshotID(t time.Time) (string, error) {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generating snapshot ID: %w", err)
	}
	return t.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(suffix[:]), nil
}

// FilesIn returns the slash-separated paths of the regular files under dir,
// skipping any entry for which skip returns true; a skipped directory is not
// walked. skip may be nil.
func FilesIn(dir string, skip func(rel string, d os.DirEntry) bool) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if skip != nil && skip(rel, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			files = append(files, rel)
		}
		return nil
	})
	return files, err
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
