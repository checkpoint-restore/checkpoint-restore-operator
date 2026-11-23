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

// Package storageconfig holds the object storage that the cluster's
// CheckpointRestoreOperator configures, for the operator and the
// checkpoint-store agent.
package storageconfig

import (
	"context"
	"slices"
	"strings"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"

	criuorgv1 "github.com/checkpoint-restore/checkpoint-restore-operator/api/v1"
	"github.com/checkpoint-restore/checkpoint-restore-operator/pkg/checkpointstore"
)

// OpenFunc opens the Store at a location URL. Tests replace it.
type OpenFunc func(ctx context.Context, location string) (*checkpointstore.Store, error)

// Provider holds the Store of the current storage configuration, reopening
// it only when the location changes.
type Provider struct {
	open OpenFunc

	mu    sync.RWMutex
	spec  *criuorgv1.CheckpointStorageSpec
	store *checkpointstore.Store
}

// NewProvider returns a Provider that opens stores with open, or with
// checkpointstore.OpenLocation if open is nil.
func NewProvider(open OpenFunc) *Provider {
	if open == nil {
		open = checkpointstore.OpenLocation
	}
	return &Provider{open: open}
}

// Set applies spec; nil disables storage. A failure to open the new
// location leaves storage disabled and is returned.
func (p *Provider) Set(ctx context.Context, spec *criuorgv1.CheckpointStorageSpec) error {
	p.mu.RLock()
	same := p.spec != nil && spec != nil && p.spec.Location == spec.Location && p.store != nil
	p.mu.RUnlock()

	var store *checkpointstore.Store
	if same {
		p.mu.RLock()
		store = p.store
		p.mu.RUnlock()
	} else if spec != nil {
		var err error
		store, err = p.open(ctx, spec.Location)
		if err != nil {
			p.mu.Lock()
			p.spec, p.store = nil, nil
			p.mu.Unlock()
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if spec == nil {
		p.spec, p.store = nil, nil
		return nil
	}
	p.spec, p.store = spec.DeepCopy(), store
	return nil
}

// Get returns the current Store and its configuration, or nil, nil when
// storage is not configured.
func (p *Provider) Get() (*checkpointstore.Store, *criuorgv1.CheckpointStorageSpec) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.store == nil {
		return nil, nil
	}
	return p.store, p.spec.DeepCopy()
}

// SpecFromCluster returns the storage configuration of the cluster's
// CheckpointRestoreOperator objects: that of the first by name that sets
// one, so that every reader picks the same one when several exist.
func SpecFromCluster(ctx context.Context, c client.Reader) (*criuorgv1.CheckpointStorageSpec, error) {
	var list criuorgv1.CheckpointRestoreOperatorList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	slices.SortFunc(list.Items, func(a, b criuorgv1.CheckpointRestoreOperator) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	for i := range list.Items {
		if s := list.Items[i].Spec.Storage; s != nil {
			return s, nil
		}
	}
	return nil, nil
}
