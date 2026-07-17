package checkpointsyncer

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ObjectStore is the minimal object-storage surface the syncer needs. It is an
// interface so reconciler tests can substitute a fake without a real backend.
type ObjectStore interface {
	Put(ctx context.Context, key, localPath string) error
	Get(ctx context.Context, key, localPath string) error
	Delete(ctx context.Context, key string) error
}

// ObjectKey is the deterministic bucket key for a checkpoint archive.
func ObjectKey(namespace, pod, container, localPath string) string {
	return strings.Join([]string{namespace, pod, container, path.Base(localPath)}, "/")
}

// ExternalURI renders the s3:// URI stored on CheckpointArchive.status.
func ExternalURI(bucket, key string) string {
	return "s3://" + bucket + "/" + key
}

// ParseS3URI splits an s3://bucket/key URI back into its parts.
func ParseS3URI(uri string) (bucket, key string, err error) {
	rest, ok := strings.CutPrefix(uri, "s3://")
	if !ok {
		return "", "", fmt.Errorf("not an s3 URI: %q", uri)
	}
	bucket, key, ok = strings.Cut(rest, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", fmt.Errorf("malformed s3 URI: %q", uri)
	}
	return bucket, key, nil
}

type minioStore struct {
	client *minio.Client
	bucket string
}

// NewMinioStore builds an ObjectStore for the given resolved Config.
func NewMinioStore(cfg Config) (ObjectStore, error) {
	endpoint := cfg.Endpoint
	secure := true
	switch {
	case strings.HasPrefix(endpoint, "https://"):
		endpoint = strings.TrimPrefix(endpoint, "https://")
	case strings.HasPrefix(endpoint, "http://"):
		endpoint, secure = strings.TrimPrefix(endpoint, "http://"), false
	}
	if endpoint == "" {
		endpoint = "s3.amazonaws.com"
	}
	c, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: secure,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, err
	}
	return &minioStore{client: c, bucket: cfg.Bucket}, nil
}

func (s *minioStore) Put(ctx context.Context, key, localPath string) error {
	_, err := s.client.FPutObject(ctx, s.bucket, key, localPath,
		minio.PutObjectOptions{ContentType: "application/x-tar"})
	return err
}

func (s *minioStore) Get(ctx context.Context, key, localPath string) error {
	return s.client.FGetObject(ctx, s.bucket, key, localPath, minio.GetObjectOptions{})
}

func (s *minioStore) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}
