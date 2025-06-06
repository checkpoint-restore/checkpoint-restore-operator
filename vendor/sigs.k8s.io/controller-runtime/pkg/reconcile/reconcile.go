/*
Copyright 2018 The Kubernetes Authors.

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

package reconcile

import (
	"context"
	"errors"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Result contains the result of a Reconciler invocation.
type Result struct {
	// Requeue tells the Controller to perform a ratelimited requeue
	// using the workqueues ratelimiter. Defaults to false.
	//
	// This setting is deprecated as it causes confusion and there is
	// no good reason to use it. When waiting for an external event to
	// happen, either the duration until it is supposed to happen or an
	// appropriate poll interval should be used, rather than an
	// interval emitted by a ratelimiter whose purpose it is to control
	// retry on error.
	//
	// Deprecated: Use `RequeueAfter` instead.
	Requeue bool

	// RequeueAfter if greater than 0, tells the Controller to requeue the reconcile key after the Duration.
	// Implies that Requeue is true, there is no need to set Requeue to true at the same time as RequeueAfter.
	RequeueAfter time.Duration
}

// IsZero returns true if this result is empty.
func (r *Result) IsZero() bool {
	if r == nil {
		return true
	}
	return *r == Result{}
}

// Request contains the information necessary to reconcile a Kubernetes object.  This includes the
// information to uniquely identify the object - its Name and Namespace.  It does NOT contain information about
// any specific Event or the object contents itself.
type Request struct {
	// NamespacedName is the name and namespace of the object to reconcile.
	types.NamespacedName
}

/*
Reconciler implements a Kubernetes API for a specific Resource by Creating, Updating or Deleting Kubernetes
objects, or by making changes to systems external to the cluster (e.g. cloudproviders, github, etc).

reconcile implementations compare the state specified in an object by a user against the actual cluster state,
and then perform operations to make the actual cluster state reflect the state specified by the user.

Typically, reconcile is triggered by a Controller in response to cluster Events (e.g. Creating, Updating,
Deleting Kubernetes objects) or external Events (GitHub Webhooks, polling external sources, etc).

Example reconcile Logic:

* Read an object and all the Pods it owns.
* Observe that the object spec specifies 5 replicas but actual cluster contains only 1 Pod replica.
* Create 4 Pods and set their OwnerReferences to the object.

reconcile may be implemented as either a type:

	type reconciler struct {}

	func (reconciler) Reconcile(ctx context.Context, o reconcile.Request) (reconcile.Result, error) {
		// Implement business logic of reading and writing objects here
		return reconcile.Result{}, nil
	}

Or as a function:

	reconcile.Func(func(ctx context.Context, o reconcile.Request) (reconcile.Result, error) {
		// Implement business logic of reading and writing objects here
		return reconcile.Result{}, nil
	})

Reconciliation is level-based, meaning action isn't driven off changes in individual Events, but instead is
driven by actual cluster state read from the apiserver or a local cache.
For example if responding to a Pod Delete Event, the Request won't contain that a Pod was deleted,
instead the reconcile function observes this when reading the cluster state and seeing the Pod as missing.
*/
type Reconciler = TypedReconciler[Request]

// TypedReconciler implements an API for a specific Resource by Creating, Updating or Deleting Kubernetes
// objects, or by making changes to systems external to the cluster (e.g. cloudproviders, github, etc).
//
// The request type is what event handlers put into the workqueue. The workqueue then de-duplicates identical
// requests.
type TypedReconciler[request comparable] interface {
	// Reconcile performs a full reconciliation for the object referred to by the Request.
	//
	// If the returned error is non-nil, the Result is ignored and the request will be
	// requeued using exponential backoff. The only exception is if the error is a
	// TerminalError in which case no requeuing happens.
	//
	// If the error is nil and the returned Result has a non-zero result.RequeueAfter, the request
	// will be requeued after the specified duration.
	//
	// If the error is nil and result.RequeueAfter is zero and result.Requeue is true, the request
	// will be requeued using exponential backoff.
	Reconcile(context.Context, request) (Result, error)
}

// Func is a function that implements the reconcile interface.
type Func = TypedFunc[Request]

// TypedFunc is a function that implements the reconcile interface.
type TypedFunc[request comparable] func(context.Context, request) (Result, error)

var _ Reconciler = Func(nil)

// Reconcile implements Reconciler.
func (r TypedFunc[request]) Reconcile(ctx context.Context, req request) (Result, error) {
	return r(ctx, req)
}

// ObjectReconciler is a specialized version of Reconciler that acts on instances of client.Object. Each reconciliation
// event gets the associated object from Kubernetes before passing it to Reconcile. An ObjectReconciler can be used in
// Builder.Complete by calling AsReconciler. See Reconciler for more details.
type ObjectReconciler[object client.Object] interface {
	Reconcile(context.Context, object) (Result, error)
}

// AsReconciler creates a Reconciler based on the given ObjectReconciler.
func AsReconciler[object client.Object](client client.Client, rec ObjectReconciler[object]) Reconciler {
	return &objectReconcilerAdapter[object]{
		objReconciler: rec,
		client:        client,
	}
}

type objectReconcilerAdapter[object client.Object] struct {
	objReconciler ObjectReconciler[object]
	client        client.Client
}

// Reconcile implements Reconciler.
func (a *objectReconcilerAdapter[object]) Reconcile(ctx context.Context, req Request) (Result, error) {
	o := reflect.New(reflect.TypeOf(*new(object)).Elem()).Interface().(object)
	if err := a.client.Get(ctx, req.NamespacedName, o); err != nil {
		return Result{}, client.IgnoreNotFound(err)
	}

	return a.objReconciler.Reconcile(ctx, o)
}

// TerminalError is an error that will not be retried but still be logged
// and recorded in metrics.
func TerminalError(wrapped error) error {
	return &terminalError{err: wrapped}
}

type terminalError struct {
	err error
}

// This function will return nil if te.err is nil.
func (te *terminalError) Unwrap() error {
	return te.err
}

func (te *terminalError) Error() string {
	if te.err == nil {
		return "nil terminal error"
	}
	return "terminal error: " + te.err.Error()
}

func (te *terminalError) Is(target error) bool {
	tp := &terminalError{}
	return errors.As(target, &tp)
}
