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
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// s3PartSize is the size of one part of a multipart upload, and the largest
// object Put stores with a single PutObject. S3 allows 10,000 parts, so an
// object may reach 640 GiB. Each part is held in memory while it is sent,
// because the SDK has to read a part twice to sign it.
const s3PartSize = 64 << 20

// s3Bucket stores objects in an S3 bucket, or one served by an S3-compatible
// endpoint such as MinIO.
type s3Bucket struct {
	client *s3.Client
	bucket string
}

// openS3 opens loc.Bucket. The region query parameter sets the region;
// otherwise the AWS default chain supplies it. The endpoint parameter
// selects an S3-compatible server, addressed path-style.
func openS3(ctx context.Context, loc Location) (Bucket, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if region := loc.Query.Get("region"); region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS configuration: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint := loc.Query.Get("endpoint"); endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		}
	})
	return &s3Bucket{client: client, bucket: loc.Bucket}, nil
}

func (b *s3Bucket) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if size <= s3PartSize {
		data, err := readPart(r, size)
		if err != nil {
			return err
		}
		_, err = b.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(b.bucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(data),
			ContentLength: aws.Int64(size),
		})
		return err
	}
	return b.putMultipart(ctx, key, r, size)
}

// putMultipart uploads an object larger than s3PartSize in parts. A failed
// upload is aborted, so its parts are not left to be billed.
func (b *s3Bucket) putMultipart(ctx context.Context, key string, r io.Reader, size int64) (err error) {
	created, err := b.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}
	defer func() {
		if err == nil {
			return
		}
		// The caller's context may be what failed the upload.
		_, _ = b.client.AbortMultipartUpload(context.WithoutCancel(ctx), &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(b.bucket),
			Key:      aws.String(key),
			UploadId: created.UploadId,
		})
	}()

	var parts []s3types.CompletedPart
	for number, left := int32(1), size; left > 0; number++ {
		n := min(left, s3PartSize)
		data, err := readPart(r, n)
		if err != nil {
			return err
		}
		out, err := b.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(b.bucket),
			Key:           aws.String(key),
			UploadId:      created.UploadId,
			PartNumber:    aws.Int32(number),
			Body:          bytes.NewReader(data),
			ContentLength: aws.Int64(n),
		})
		if err != nil {
			return fmt.Errorf("uploading part %d: %w", number, err)
		}
		parts = append(parts, s3types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(number)})
		left -= n
	}
	_, err = b.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(b.bucket),
		Key:             aws.String(key),
		UploadId:        created.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts},
	})
	return err
}

// readPart reads exactly n bytes from r.
func readPart(r io.Reader, n int64) ([]byte, error) {
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("reading %d bytes to upload: %w", n, err)
	}
	return data, nil
}

func (b *s3Bucket) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	var noKey *s3types.NoSuchKey
	if errors.As(err, &noKey) {
		return nil, fmt.Errorf("%s: %w", key, ErrNotExist)
	}
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (b *s3Bucket) List(ctx context.Context, prefix string, fn func(string) error) error {
	pages := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(prefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			if err := fn(*obj.Key); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *s3Bucket) Delete(ctx context.Context, key string) error {
	// DeleteObject succeeds for a missing key.
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	return err
}
