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
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
)

// MemoryBucket is a Bucket held in memory, for tests.
type MemoryBucket struct {
	mu      sync.Mutex
	objects map[string][]byte
}

// NewMemoryBucket returns an empty MemoryBucket.
func NewMemoryBucket() *MemoryBucket {
	return &MemoryBucket{objects: make(map[string][]byte)}
}

func (b *MemoryBucket) Put(_ context.Context, key string, r io.Reader, size int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return fmt.Errorf("put %s: read %d bytes, expected %d", key, len(data), size)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[key] = data
	return nil
}

func (b *MemoryBucket) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.objects[key]
	if !ok {
		return nil, fmt.Errorf("%s: %w", key, ErrNotExist)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *MemoryBucket) List(_ context.Context, prefix string, fn func(string) error) error {
	b.mu.Lock()
	var keys []string
	for k := range b.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	b.mu.Unlock()
	slices.Sort(keys)
	for _, k := range keys {
		if err := fn(k); err != nil {
			return err
		}
	}
	return nil
}

func (b *MemoryBucket) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, key)
	return nil
}

// Keys returns every stored key, sorted.
func (b *MemoryBucket) Keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := make([]string, 0, len(b.objects))
	for k := range b.objects {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Set stores data at key directly, for tests that corrupt a snapshot.
func (b *MemoryBucket) Set(key string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[key] = slices.Clone(data)
}
