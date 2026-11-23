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
	"net/url"
	"strings"
)

// ErrNotExist is returned by Bucket.Get for a key that has no object.
var ErrNotExist = errors.New("object does not exist")

// Bucket is the object storage a Store writes to. Keys are slash-separated
// paths without a leading slash.
type Bucket interface {
	// Put stores size bytes read from r at key, replacing any object there.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// Get opens the object at key, or returns an error wrapping ErrNotExist.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// List calls fn with every key that starts with prefix, in lexical
	// order, and stops at the first error fn returns.
	List(ctx context.Context, prefix string, fn func(key string) error) error
	// Delete removes the object at key. Deleting a missing key succeeds.
	Delete(ctx context.Context, key string) error
}

// Location is a parsed storage location URL.
type Location struct {
	// Scheme is s3, gs or azblob.
	Scheme string
	// Bucket is the S3 or GCS bucket, or the Azure container.
	Bucket string
	// Root is the key prefix every snapshot is stored under, without
	// leading or trailing slashes. It may be empty.
	Root string
	// Query holds the scheme's options: region and endpoint for s3,
	// account and endpoint for azblob.
	Query url.Values
}

// ParseLocation parses a location URL: s3://bucket/root, gs://bucket/root or
// azblob://container/root?account=name.
func ParseLocation(raw string) (Location, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Location{}, fmt.Errorf("parsing storage location: %w", err)
	}
	switch u.Scheme {
	case "s3", "gs", "azblob":
	default:
		return Location{}, fmt.Errorf("storage location %q: scheme must be s3, gs or azblob", raw)
	}
	if u.Host == "" {
		return Location{}, fmt.Errorf("storage location %q names no bucket", raw)
	}
	if u.User != nil || u.Fragment != "" {
		return Location{}, fmt.Errorf("storage location %q has a user or fragment", raw)
	}
	loc := Location{
		Scheme: u.Scheme,
		Bucket: u.Host,
		Root:   strings.Trim(u.Path, "/"),
		Query:  u.Query(),
	}
	if loc.Scheme == "azblob" && loc.Query.Get("account") == "" && loc.Query.Get("endpoint") == "" {
		return Location{}, fmt.Errorf("storage location %q needs an account or endpoint parameter", raw)
	}
	return loc, nil
}

// Open connects to the bucket a location names, with the credentials of the
// provider's default chain: an IAM role or AWS environment for s3,
// Application Default Credentials for gs, and DefaultAzureCredential for
// azblob.
func Open(ctx context.Context, loc Location) (Bucket, error) {
	switch loc.Scheme {
	case "s3":
		return openS3(ctx, loc)
	case "gs":
		return openGCS(ctx, loc)
	case "azblob":
		return openAzure(loc)
	default:
		return nil, fmt.Errorf("unsupported storage scheme %q", loc.Scheme)
	}
}
