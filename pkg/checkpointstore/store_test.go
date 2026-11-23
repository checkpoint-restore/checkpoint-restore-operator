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
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testKey = Key{
	Namespace:    "ns",
	WorkloadKind: "StatefulSet",
	WorkloadName: "db",
	Pod:          "db-0",
	Container:    "main",
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func mustWrite(t *testing.T, s *Store, dir string) Manifest {
	t.Helper()
	files, err := FilesIn(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Write(context.Background(),
		Manifest{Key: testKey, Format: "criu-images", Producer: "test"}, dir, files)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestWriteThenFetchReturnsTheSameFiles(t *testing.T) {
	ctx := context.Background()
	s := New(NewMemoryBucket(), "/checkpoints/")
	src := writeTree(t, map[string]string{"pages-1.img": "pages", "sub/core.img": "core"})
	m := mustWrite(t, s, src)

	if got := s.Path(m); !strings.HasPrefix(got, "checkpoints/ns/StatefulSet/db/db-0/main/") {
		t.Fatalf("snapshot path %q is not under the key prefix", got)
	}
	dst := t.TempDir()
	fetched, n, err := s.Fetch(ctx, s.Path(m), dst)
	if err != nil {
		t.Fatal(err)
	}
	if fetched.ID != m.ID || n != int64(len("pages")+len("core")) {
		t.Fatalf("fetched %s with %d bytes, want %s with 9", fetched.ID, n, m.ID)
	}
	for name, want := range map[string]string{"pages-1.img": "pages", "sub/core.img": "core"} {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
}

// failingBucket fails every Put whose key ends in failSuffix, after the
// object is stored, as an upload interrupted after its last byte can. The
// failure also ends the caller's context, as a deadline that interrupts the
// upload does, and Delete refuses to work under an ended context.
type failingBucket struct {
	*MemoryBucket
	failSuffix string
	cancel     context.CancelFunc
}

func (b failingBucket) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if err := b.MemoryBucket.Put(ctx, key, r, size); err != nil {
		return err
	}
	if strings.HasSuffix(key, b.failSuffix) {
		b.cancel()
		return errors.New("upload interrupted")
	}
	return nil
}

func (b failingBucket) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.MemoryBucket.Delete(ctx, key)
}

func TestWriteDeletesWhatItStoredWhenItFails(t *testing.T) {
	for _, failing := range []string{"/data/b", "/manifest.json"} {
		t.Run(failing, func(t *testing.T) {
			b := NewMemoryBucket()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := New(failingBucket{MemoryBucket: b, failSuffix: failing, cancel: cancel}, "root")
			dir := writeTree(t, map[string]string{"a": "1", "b": "2"})

			if _, err := s.Write(ctx, Manifest{Key: testKey}, dir, []string{"a", "b"}); err == nil {
				t.Fatal("Write succeeded")
			}
			if keys := b.Keys(); len(keys) != 0 {
				t.Fatalf("a failed Write left %v", keys)
			}
		})
	}
}

func TestListLeavesOutSnapshotsWithoutAManifest(t *testing.T) {
	ctx := context.Background()
	b := NewMemoryBucket()
	s := New(b, "")
	first := mustWrite(t, s, writeTree(t, map[string]string{"a": "1"}))
	second := mustWrite(t, s, writeTree(t, map[string]string{"a": "2"}))
	// An interrupted Write leaves data but no manifest.
	b.Set("ns/StatefulSet/db/db-0/main/99991231T235959.000000000Z-00000000/data/a", []byte("partial"))

	got, err := s.List(ctx, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != first.ID || got[1].ID != second.ID {
		t.Fatalf("List = %v, want [%s %s]", ids(got), first.ID, second.ID)
	}
	latest, ok, err := s.Latest(ctx, testKey)
	if err != nil || !ok || latest.ID != second.ID {
		t.Fatalf("Latest = %s, %v, %v; want %s", latest.ID, ok, err, second.ID)
	}
}

func TestKeysListsOnlyContainersWithACompleteSnapshot(t *testing.T) {
	b := NewMemoryBucket()
	s := New(b, "root")
	mustWrite(t, s, writeTree(t, map[string]string{"a": "1"}))
	other := testKey
	other.Container = "sidecar"
	if _, err := s.Write(context.Background(), Manifest{Key: other}, writeTree(t, map[string]string{"a": "1"}), []string{"a"}); err != nil {
		t.Fatal(err)
	}
	// Data of an interrupted Write for a third container.
	b.Set("root/ns/StatefulSet/db/db-1/main/20260101T000000.000000000Z-00000000/data/a", []byte("x"))

	keys, err := s.Keys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != testKey || keys[1] != other {
		t.Fatalf("Keys = %v, want [%v %v]", keys, testKey, other)
	}
}

func TestLatestReportsNoneForAContainerWithoutSnapshots(t *testing.T) {
	_, ok, err := New(NewMemoryBucket(), "").Latest(context.Background(), testKey)
	if err != nil || ok {
		t.Fatalf("Latest = %v, %v; want false, nil", ok, err)
	}
}

func TestFetchRejectsDataThatDoesNotMatchTheManifest(t *testing.T) {
	for name, corrupt := range map[string]string{
		"same size, other content": "pagex",
		"longer than recorded":     "pages and more",
		"shorter than recorded":    "pag",
	} {
		t.Run(name, func(t *testing.T) {
			b := NewMemoryBucket()
			s := New(b, "")
			m := mustWrite(t, s, writeTree(t, map[string]string{"pages-1.img": "pages"}))
			b.Set(s.Path(m)+"/data/pages-1.img", []byte(corrupt))

			if _, _, err := s.Fetch(context.Background(), s.Path(m), t.TempDir()); err == nil {
				t.Fatal("Fetch accepted data that does not match the manifest")
			}
		})
	}
}

func TestDeleteRemovesTheManifestAndData(t *testing.T) {
	ctx := context.Background()
	b := NewMemoryBucket()
	s := New(b, "root")
	keep := mustWrite(t, s, writeTree(t, map[string]string{"a": "1"}))
	gone := mustWrite(t, s, writeTree(t, map[string]string{"a": "2"}))

	if err := s.Delete(ctx, s.Path(gone)); err != nil {
		t.Fatal(err)
	}
	for _, k := range b.Keys() {
		if strings.HasPrefix(k, s.Path(gone)) {
			t.Fatalf("object %s of the deleted snapshot remains", k)
		}
	}
	if _, err := s.Read(ctx, s.Path(gone)); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Read after Delete = %v, want ErrNotExist", err)
	}
	if _, err := s.Read(ctx, s.Path(keep)); err != nil {
		t.Fatalf("the other snapshot is gone: %v", err)
	}
	if err := s.Delete(ctx, s.Path(gone)); err != nil {
		t.Fatalf("deleting a deleted snapshot: %v", err)
	}
}

func TestReadRefusesPathsOutsideTheStore(t *testing.T) {
	s := New(NewMemoryBucket(), "root")
	m := mustWrite(t, s, writeTree(t, map[string]string{"a": "1"}))
	for _, p := range []string{
		"other/ns/StatefulSet/db/db-0/main/" + m.ID,
		"root/ns/StatefulSet/db/db-0/" + m.ID,
		"root/ns/StatefulSet/db/../main/" + m.ID,
		"root/ns/StatefulSet/db/db-0/main/not-an-id",
	} {
		if _, err := s.Read(context.Background(), p); err == nil {
			t.Errorf("Read(%q) succeeded", p)
		}
	}
}

func TestFetchRefusesAManifestNamingAFileOutsideTheSnapshot(t *testing.T) {
	b := NewMemoryBucket()
	s := New(b, "")
	m := mustWrite(t, s, writeTree(t, map[string]string{"a": "1"}))
	r, _ := b.Get(context.Background(), s.Path(m)+"/manifest.json")
	data, _ := io.ReadAll(r)
	b.Set(s.Path(m)+"/manifest.json", []byte(strings.Replace(string(data), `"path":"a"`, `"path":"../../a"`, 1)))

	dst := filepath.Join(t.TempDir(), "dst")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Fetch(context.Background(), s.Path(m), dst); err == nil {
		t.Fatal("Fetch wrote a file outside the snapshot directory")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dst), "..", "a")); err == nil {
		t.Fatal("file escaped the destination")
	}
}

func TestWriteRejectsAnInvalidKey(t *testing.T) {
	bad := testKey
	bad.Pod = "../x"
	_, err := New(NewMemoryBucket(), "").Write(context.Background(),
		Manifest{Key: bad}, t.TempDir(), nil)
	if err == nil {
		t.Fatal("Write accepted a key segment with a slash")
	}
}

func TestFilesInSkipsTheEntriesSkipSelects(t *testing.T) {
	dir := writeTree(t, map[string]string{"keep": "1", "derived/x": "2", "sub/keep": "3"})
	got, err := FilesIn(dir, func(rel string, _ os.DirEntry) bool { return rel == "derived" })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "keep,sub/keep" {
		t.Fatalf("FilesIn = %v", got)
	}
}

func TestParseLocation(t *testing.T) {
	for raw, want := range map[string]Location{
		"s3://bkt/a/b/?region=us-west-2": {Scheme: "s3", Bucket: "bkt", Root: "a/b"},
		"gs://bkt":                       {Scheme: "gs", Bucket: "bkt"},
		"azblob://ctr/root?account=acct": {Scheme: "azblob", Bucket: "ctr", Root: "root"},
	} {
		got, err := ParseLocation(raw)
		if err != nil {
			t.Fatalf("ParseLocation(%q): %v", raw, err)
		}
		if got.Scheme != want.Scheme || got.Bucket != want.Bucket || got.Root != want.Root {
			t.Errorf("ParseLocation(%q) = %+v, want %+v", raw, got, want)
		}
	}
	for _, raw := range []string{"file:///tmp", "s3:///root", "azblob://ctr/root", "s3://user@bkt/"} {
		if _, err := ParseLocation(raw); err == nil {
			t.Errorf("ParseLocation(%q) succeeded", raw)
		}
	}
}

func ids(ms []Manifest) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}
