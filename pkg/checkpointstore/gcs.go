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
	"fmt"
	"io"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// gcsBucket stores objects in a Google Cloud Storage bucket.
type gcsBucket struct {
	bucket *storage.BucketHandle
}

// openGCS opens loc.Bucket with Application Default Credentials.
func openGCS(ctx context.Context, loc Location) (Bucket, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating GCS client: %w", err)
	}
	return &gcsBucket{bucket: client.Bucket(loc.Bucket)}, nil
}

func (b *gcsBucket) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	// The writer's context cancels the upload; Close commits it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := b.bucket.Object(key).NewWriter(ctx)
	if _, err := io.Copy(w, io.LimitReader(r, size)); err != nil {
		cancel()
		_ = w.Close()
		return err
	}
	return w.Close()
}

func (b *gcsBucket) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	r, err := b.bucket.Object(key).NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, fmt.Errorf("%s: %w", key, ErrNotExist)
	}
	return r, err
}

func (b *gcsBucket) List(ctx context.Context, prefix string, fn func(string) error) error {
	it := b.bucket.Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(attrs.Name); err != nil {
			return err
		}
	}
}

func (b *gcsBucket) Delete(ctx context.Context, key string) error {
	err := b.bucket.Object(key).Delete(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	return err
}
