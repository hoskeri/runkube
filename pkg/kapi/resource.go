// Package kapi provides a lightweight framework for building Kubernetes
// extension API servers without the full k8s.io/apiserver dependency stack.
package kapi

import (
	"context"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

// ── Operation interfaces ───────────────────────────────────
//
// A Resource expresses its capabilities by implementing zero or more of
// these interfaces. Unimplemented operations return 405.
//
// Every method takes context.Context as the first parameter (omitted from
// the doc comments for brevity, but present in the signatures).

// Getter retrieves a single resource by name.
type Getter interface {
	Get(ctx context.Context, name string) (*unstructured.Unstructured, error)
}

// Creater creates a new resource from the provided object.
type Creater interface {
	Create(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
}

// Updater fully replaces an existing resource.
type Updater interface {
	Update(ctx context.Context, name string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
}

// Patcher applies a partial update to a resource.
type Patcher interface {
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte) (*unstructured.Unstructured, error)
}

// Deleter removes a single resource by name.
type Deleter interface {
	Delete(ctx context.Context, name string) error
}

// CollectionDeleter removes all resources matching the given list options.
type CollectionDeleter interface {
	DeleteCollection(ctx context.Context, opts metav1.ListOptions) error
}

// Lister unifies list and watch — both are accessed via GET on the
// collection URL, differentiated by ?watch=true.
//
// If the resource does not support watch, Watch should return (nil, nil).
// The router will then not register the ?watch=true path for this resource.
type Lister interface {
	List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error)
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
}

// RawHandler is a catch-all for any request that doesn't match a
// registered operation. It gives the user full control over the HTTP
// response. Typical uses: WebSocket upgrades, custom sub-path routing,
// streaming endpoints, verbs outside the standard set.
//
// If not set, unmatched requests return 405 Method Not Allowed.
type RawHandler func(ctx context.Context, w http.ResponseWriter, r *http.Request)

// ── Resource ───────────────────────────────────────────────

// Resource describes one Kubernetes API resource and how to handle it.
// Set the operation fields for the verbs you support; leave nil for 405.
//
// Always use &Resource{...}.
type Resource struct {
	GVK          schema.GroupVersionKind
	Resource     string // plural name, e.g. "buckets"
	Namespaced   bool
	Verbs        []string // nil → defaultVerbs; advisory for discovery only
	Subresources []*Subresource

	Getter            Getter
	Creater           Creater
	Updater           Updater
	Patcher           Patcher
	Deleter           Deleter
	CollectionDeleter CollectionDeleter
	Lister            Lister
	RawHandler        RawHandler

	OpenAPIOperation func(verb string) map[string]any // nil → no enrichment
	OpenAPISchema    func() map[string]any            // nil → generated minimal schema
}

// ── Subresource ────────────────────────────────────────────

// Subresource represents a sub-resource under a parent Resource.
// It has its own set of operation interfaces — nothing is inherited
// from the parent.
type Subresource struct {
	Name string // e.g. "status", "scale", "proxy"

	Getter  Getter
	Creater Creater
	Updater Updater
	Patcher Patcher
	Deleter Deleter

	// SameTypeAsParent marks the subresource as sharing the parent's GVK
	// for OpenAPI schema generation.
	SameTypeAsParent bool

	RawHandler RawHandler
}

// SubresourceOf returns a Subresource that shares the parent's type.
// It sets SameTypeAsParent = true. Override operation fields as needed.
func SubresourceOf(parent *Resource, name string) *Subresource {
	return &Subresource{Name: name, SameTypeAsParent: true}
}

// ── Adapters ───────────────────────────────────────────────
//
// These let users write inline implementations without defining
// named types:

type GetFunc func(context.Context, string) (*unstructured.Unstructured, error)

func (f GetFunc) Get(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	return f(ctx, name)
}

type CreateFunc func(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error)

func (f CreateFunc) Create(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return f(ctx, obj)
}

type UpdateFunc func(context.Context, string, *unstructured.Unstructured) (*unstructured.Unstructured, error)

func (f UpdateFunc) Update(ctx context.Context, name string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return f(ctx, name, obj)
}

type PatchFunc func(context.Context, string, types.PatchType, []byte) (*unstructured.Unstructured, error)

func (f PatchFunc) Patch(ctx context.Context, name string, pt types.PatchType, data []byte) (*unstructured.Unstructured, error) {
	return f(ctx, name, pt, data)
}

type DeleteFunc func(context.Context, string) error

func (f DeleteFunc) Delete(ctx context.Context, name string) error {
	return f(ctx, name)
}

type DeleteCollectionFunc func(context.Context, metav1.ListOptions) error

func (f DeleteCollectionFunc) DeleteCollection(ctx context.Context, opts metav1.ListOptions) error {
	return f(ctx, opts)
}

// ListWatch unifies list and watch into one adapter. Set WatchFunc to nil
// if watch is not supported.
type ListWatch struct {
	ListFunc  func(context.Context, metav1.ListOptions) (*unstructured.UnstructuredList, error)
	WatchFunc func(context.Context, metav1.ListOptions) (watch.Interface, error) // nil → no watch
}

func (lw *ListWatch) List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	return lw.ListFunc(ctx, opts)
}

func (lw *ListWatch) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	if lw.WatchFunc == nil {
		return nil, nil
	}
	return lw.WatchFunc(ctx, opts)
}

// ── Group ──────────────────────────────────────────────────

type Group struct {
	GroupVersion schema.GroupVersion
	Resources    []*Resource

	// OpenAPI injects additional top-level entries into the group's
	// OpenAPI v3 document.
	OpenAPI func(gv schema.GroupVersion) map[string]any
}
