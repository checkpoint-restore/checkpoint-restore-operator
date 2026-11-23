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
	"fmt"
	"io"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
)

// azureBucket stores objects as block blobs in an Azure Storage container.
type azureBucket struct {
	client    *azblob.Client
	container string
}

// openAzure opens container loc.Bucket with DefaultAzureCredential. The
// account query parameter names the storage account; the endpoint parameter
// gives the service URL instead, for sovereign clouds or Azurite.
func openAzure(loc Location) (Bucket, error) {
	endpoint := loc.Query.Get("endpoint")
	if endpoint == "" {
		endpoint = "https://" + loc.Query.Get("account") + ".blob.core.windows.net/"
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("loading Azure credentials: %w", err)
	}
	client, err := azblob.NewClient(endpoint, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("creating Azure Blob client: %w", err)
	}
	return &azureBucket{client: client, container: loc.Bucket}, nil
}

func (b *azureBucket) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	// UploadStream stages blocks and commits them at the end, so a failed
	// upload leaves no blob behind.
	_, err := b.client.UploadStream(ctx, b.container, key, io.LimitReader(r, size), nil)
	return err
}

func (b *azureBucket) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := b.client.DownloadStream(ctx, b.container, key, nil)
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return nil, fmt.Errorf("%s: %w", key, ErrNotExist)
	}
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (b *azureBucket) List(ctx context.Context, prefix string, fn func(string) error) error {
	pages := b.client.NewListBlobsFlatPager(b.container, &azblob.ListBlobsFlatOptions{Prefix: &prefix})
	for pages.More() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, item := range page.Segment.BlobItems {
			if item.Name == nil {
				continue
			}
			if err := fn(*item.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *azureBucket) Delete(ctx context.Context, key string) error {
	_, err := b.client.DeleteBlob(ctx, b.container, key, nil)
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return nil
	}
	return err
}
