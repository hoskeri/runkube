# kapi — Lightweight Extension API Server Library

**Package:** `github.com/hoskeri/runkube/pkg/kapi`
**Status:** Draft design / under review
**Authors:** runkube

---

## 1. Motivation

Kubernetes extension API servers are typically built on
`k8s.io/apiserver`, which brings hundreds of dependencies, an Opinionated
storage abstraction (etcd via `k8s.io/apiserver/pkg/registry/rest`), web
of informers, and a complex delegation chain of handler filters. For many
use cases — internal CRDs served by a lightweight sidecar, mock API
servers for testing, single-node control planes like runkube — this is
gross overkill.

`pkg/kapi` provides just enough machinery to register Kubernetes-style
API resources and serve them over HTTP, without built-in persistence,
openapi v3 spec generation, or admission hooks. It is not a replacement
for `k8s.io/apiserver` in large deployments; it is a **lighter-weight
alternative** when you control the server and want minimal dependencies.

### What we give up

| Feature | kapi | k8s.io/apiserver |
|---------|------|-------------------|
| Built-in persistence (etcd) | Not provided | Core feature |
| OpenAPI v3 specs | Minimal by default, overridable | Generated from Go types |
| Admission webhooks | Not built-in | Integration chain |
| RBAC authorizer | Delegated to underlying apiserver | Built-in |
| Storage layer (CRUD uniform interface) | Per-resource handler | `RESTStorage` interface |

### What we keep

- Standard Kubernetes API URL patterns (`/apis/{group}/{version}/...`)
- Discovery endpoints (`/apis`, `/apis/{group}/{version}`, `openapi/v3`)
- Per-resource verb dispatch (list, get, create, update, delete, patch, watch)
- Sub-resources (status, scale, proxy, port-forward, etc.)
- APIService object generation (apiregistration.k8s.io/v1)
- Standard Go `net/http` handler interface — works anywhere an
  `http.Handler` is expected

---

## 2. Design goals

### 2.1 Make the easy things easy

Registering a resource that only needs discovery (shows up in
`kubectl api-resources`, returns empty stubs) must be a few lines:

```go
kapi.Resource{
    GVK:      schema.GroupVersionKind{Group:"my.example", Version:"v1", Kind:"Widget"},
    Resource: "widgets",
}
```

Resources use pointer receivers throughout — always `&Resource{...}`.

### 2.2 Make difficult things possible

When a resource needs custom logic — real CRUD, patches, WebSocket
upgrades, streaming watch — the user plugs in handler functions without
fighting the framework. When the framework doesn't have a specific
handler for a verb, a `RawHandler` catch-all handles the request as a
fallback — the user never has to choose between verb dispatch and full
HTTP control.

### 2.3 Errors are errors, not smuggled status objects

`k8s.io/apiserver` returns `runtime.Object` from handlers, where a
`*metav1.Status` counts as a "success" return. In kapi, handler
signatures use `(T, error)`. Errors are real Go errors; the library
maps them to HTTP response codes:

| Error interface | HTTP status |
|----------------|-------------|
| implements `HTTPStatusError{HTTPStatus() int32}` | User-specified |
| otherwise | `500 Internal Server Error` |

### 2.4 OpenAPI v3: functional defaults, no codegen

A minimal spec is generated automatically so that tools like `kubectl`
and client-go work. Users enrich the spec via per-resource hooks,
not code generation or reflection.

---

## 3. Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Service (http.Handler)                                     │
│  ┌───────────────────────────────────────────────────────┐  │
│  │  Global discovery: /apis, /openapi/{version}          │  │
│  │  Catch-all: /... → 404                                │  │
│  └───────────────────────────────────────────────────────┘  │
│                                                             │
│  ┌───────────────────────────────────────────────────────┐  │
│  │  Group                                                │  │
│  │  - GroupVersion discovery: /apis/{g}/{v}              │  │
│  │  - OpenAPI per-group: /openapi/v3/apis/{g}/{v}        │  │
│  │  ┌─────────────────┐  ┌─────────────────┐            │  │
│  │  │ Resource         │  │ Resource         │            │  │
│  │  │ - List / Get / … │  │ - List / Get / … │            │  │
│  │  │ - Subresources   │  │ - Subresources   │            │  │
│  │  │ - RawHandler     │  │ - RawHandler     │            │  │
│  │  └─────────────────┘  └─────────────────┘            │  │
│  └───────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
```

### 3.1 Routing

`Service` owns a top-level `*http.ServeMux` (Go 1.22+ pattern routing).
Each group installs its own patterns. Each resource inside a group installs
its own patterns. The flow is:

1. `Service.ServeHTTP` receives a request
2. Mux matches the longest registered pattern
3. If no pattern matches, the catch-all handler returns 404
4. If matched, the resource handler dispatches to the appropriate verb

### 3.2 Verb dispatch

The router uses the HTTP method (GET, POST, PUT, PATCH, DELETE) and
query parameter `?watch=true` to determine which handler to call:

| Method + query | Route | Handler |
|----------------|-------|---------|
| `GET ?watch=false` | collection | `List` |
| `GET ?watch=true` | collection | `Watch` |
| `POST` | collection | `Create` |
| `GET` | single item | `Get` |
| `PUT` | single item | `Update` |
| `PATCH` | single item | `Patch` |
| `DELETE` | single item | `Delete` |

If the required handler is nil, 405 Method Not Allowed is returned.

For subresources, the same dispatch applies with subresource-specific
handlers. Subresources share the parent's URL namespace with one
additional path segment.

---

## 4. Core types

```go
package kapi

import (
	"context"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

// ── OpenAPI hooks ──────────────────────────────────────────

// OpenAPIOperationSpec enriches the minimal per-verb operation spec.
// Receives the verb ("get", "list", "create", "update", "patch",
// "delete", "watch") and returns a map to merge into the generated
// operation object. Return nil for no enrichment.
type OpenAPIOperationSpec func(verb string) map[string]any

// OpenAPISchemaSpec overrides the generated component schema for a
// resource's Kind. Return nil to keep the generated minimal schema.
type OpenAPISchemaSpec func() map[string]any

// ── Resource ───────────────────────────────────────────────

// Resource describes one Kubernetes API resource and how to handle it.
//
// The zero value for any handler field (nil) means the verb is
// not supported. If a verb handler is nil and RawHandler is set,
// the request falls through to RawHandler. If both are nil, 405
// is returned.
type Resource struct {
	GVK        schema.GroupVersionKind
	Resource   string   // plural resource name, e.g. "buckets"
	Namespaced bool
	Verbs      []string // nil → defaultVerbs; advisory for discovery only

	Subresources []*Subresource

	// Verb handlers. nil → fall through to RawHandler, then 405.
	List    ListHandler
	Get     GetHandler
	Create  CreateHandler
	Update  UpdateHandler
	Delete  DeleteHandler
	Patch   PatchHandler
	Watch   WatchHandler

	// RawHandler, if set, serves as a catch-all default for any
	// HTTP request matching this resource that does not have a
	// specific verb handler. This includes WebSocket upgrades,
	// custom sub-path routing, and verbs outside the standard set.
	// It is checked only after the verb handler is nil.
	RawHandler http.Handler

	OpenAPIOperation OpenAPIOperationSpec // nil → no enrichment
	OpenAPISchema    OpenAPISchemaSpec    // nil → generated minimal schema
}

// ── Subresource ────────────────────────────────────────────

// Subresource represents a sub-resource under a parent Resource.
//
// Subresources are independent actors — none of the parent's handler
// fields are inherited. SameTypeAsParent only affects OpenAPI schema
// generation (it uses the parent's GVK for the component schema).
//
// Common usage patterns:
//
//   proxy/port-forward:  set RawHandler (catch-all for upgrades)
//   status/scale:        set Get/Update handlers individually
//   same-type behavior:  start with SubresourceOf(parent, name)
type Subresource struct {
	Name string

	// SameTypeAsParent, when true, causes OpenAPI schema generation
	// to use the parent's GVK for this subresource.
	SameTypeAsParent bool

	// Verb handlers. nil → fall through to RawHandler, then 405.
	Get    GetHandler
	Update UpdateHandler
	Create CreateHandler
	Delete DeleteHandler
	Patch  PatchHandler

	// RawHandler is a catch-all default for any verb that does not
	// have a specific handler on this subresource. Checked only
	// after the verb handler is nil.
	RawHandler http.Handler
}

// SubresourceOf is a convenience constructor for the common case
// where a subresource shares the parent's type. It sets
// SameTypeAsParent = true. Override handlers as needed.
func SubresourceOf(parent *Resource, name string) *Subresource {
	return &Subresource{Name: name, SameTypeAsParent: true}
}

// ── Handler type aliases ───────────────────────────────────

type ListHandler   func(ctx context.Context, namespace string, opts metav1.ListOptions) (*unstructured.UnstructuredList, error)
type GetHandler    func(ctx context.Context, namespace, name string, opts metav1.GetOptions) (*unstructured.Unstructured, error)
type CreateHandler func(ctx context.Context, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
type UpdateHandler func(ctx context.Context, namespace, name string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
type DeleteHandler func(ctx context.Context, namespace, name string) error
type PatchHandler  func(ctx context.Context, namespace, name string, pt types.PatchType, data []byte) (*unstructured.Unstructured, error)

// WatchHandler returns a watch.Interface (from k8s.io/apimachinery/pkg/watch).
// The library handles streaming the watch events as JSON and closing
// on completion or cancellation.
type WatchHandler  func(ctx context.Context, namespace string, opts metav1.ListOptions) (watch.Interface, error)

// ── Error handling ─────────────────────────────────────────

// HTTPStatusError may be implemented by handler errors to control
// the HTTP status code returned in the response body (as Status).
type HTTPStatusError interface {
	HTTPStatus() int32
}

// Convenience constructors. Each returns an error that implements
// HTTPStatusError with the standard Kubernetes semantic code.
func NotFound(name string) error   // 404
func AlreadyExists(name string) error // 409
func Conflict(name string) error   // 409
func Forbidden(reason string) error   // 403

// ── Group ──────────────────────────────────────────────────

type Group struct {
	GroupVersion schema.GroupVersion
	Resources    []*Resource

	// OpenAPI, if non-nil, injects additional top-level entries
	// into the group's OpenAPI v3 document. This is useful for
	// endpoints outside the resource path tree, e.g. health checks.
	OpenAPI func(gv schema.GroupVersion) map[string]any
}

// ── Service ────────────────────────────────────────────────

// Service is an http.Handler that serves a set of API groups.
type Service struct { /* unexported */ }

// NewAPI constructs a Service from the given groups.
func NewAPI(groups ...*Group) *Service

// ServeHTTP implements http.Handler.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request)

// APIServices yields APIService objects (apiregistration.k8s.io/v1)
// suitable for registering with the main Kubernetes API server.
func (s *Service) APIServices(caBundle []byte, namespace, name string) iter.Seq[runtime.Object]
```

---

## 5. URL patterns and routing

### 5.1 Standard resource patterns

For a cluster-scoped resource:

```
GET    /apis/{group}/{version}/{resource}                        → List
GET    /apis/{group}/{version}/{resource}?watch=true              → Watch
POST   /apis/{group}/{version}/{resource}                        → Create
GET    /apis/{group}/{version}/{resource}/{name}                  → Get
PUT    /apis/{group}/{version}/{resource}/{name}                  → Update
PATCH  /apis/{group}/{version}/{resource}/{name}                  → Patch
DELETE /apis/{group}/{version}/{resource}/{name}                  → Delete
```

For a namespaced resource:

```
GET    /apis/{group}/{version}/namespaces/{namespace}/{resource}           → List
GET    /apis/{group}/{version}/namespaces/{namespace}/{resource}?watch=true → Watch
POST   /apis/{group}/{version}/namespaces/{namespace}/{resource}           → Create
GET    /apis/{group}/{version}/namespaces/{namespace}/{resource}/{name}    → Get
PUT    /apis/{group}/{version}/namespaces/{namespace}/{resource}/{name}    → Update
PATCH  /apis/{group}/{version}/namespaces/{namespace}/{resource}/{name}    → Patch
DELETE /apis/{group}/{version}/namespaces/{namespace}/{resource}/{name}    → Delete
```

### 5.2 Subresource patterns

```
GET    /apis/{group}/{version}/namespaces/{namespace}/{resource}/{name}/{subresource}    → Sub.Get
PUT    /apis/{group}/{version}/namespaces/{namespace}/{resource}/{name}/{subresource}    → Sub.Update
PATCH  /apis/{group}/{version}/namespaces/{namespace}/{resource}/{name}/{subresource}    → Sub.Patch
DELETE /apis/{group}/{version}/namespaces/{namespace}/{resource}/{name}/{subresource}    → Sub.Delete
```

If `RawHandler` is set on the subresource, it handles any verb that
does not have a specific subresource handler — the user can register
Get/Update for standard operations while RawHandler catches upgrades
and custom protocols.

### 5.3 RawHandler fall-through dispatch

`RawHandler` is a **catch-all default**, not an override. The dispatch
order for every matched request:

```
1. Look up verb handler (List, Get, Create, etc.) from method + path
2. If verb handler is non-nil → call it
3. If verb handler is nil AND RawHandler is non-nil → call RawHandler
4. If both are nil → return 405 Method Not Allowed
```

This means a resource can register handlers for the verbs it cares about
and let `RawHandler` catch the rest: WebSocket upgrades, custom streaming
protocols, verbs outside the standard set, or sub-path routing.

`RawHandler` is registered with a pattern that matches the resource name
and everything beneath it, so it also catches undefined sub-paths.

```
RawHandler fall-through pattern (namespaced):
  /apis/{group}/{version}/namespaces/{namespace}/{resource}/{name}/{rest...}
```

The same fall-through logic applies per-subresource: if `RawHandler` is
set on the subresource, it catches any verb where the subresource's
`Get`/`Update`/`Create`/`Delete`/`Patch` handler is nil.

---

## 6. Watch protocol

Watch uses `Transfer-Encoding: chunked` with `Content-Type: application/json`.
Each event is a JSON object on its own line:

```json
{"type":"ADDED","object":{"apiVersion":"...","kind":"...",...}}
{"type":"MODIFIED","object":{...}}
{"type":"DELETED","object":{...}}
```

The library encodes and flushes each event from the `watch.Interface`
channel. When the client disconnects or the context is cancelled, the
handler stops and cleans up.

---

## 7. OpenAPI v3 design

The OpenAPI v3 spec is controversial in this package. The current
implementation generates it unconditionally. The new design:

### 7.1 Default behavior

A minimal spec is generated for each resource so that tools work:

```yaml
# Path entry (per resource)
/apis/{group}/{version}/{resource}:
  get:
    x-kubernetes-group-version-kind:
      group: my.example
      version: v1
      kind: Widget

# Component schema (per kind)
components:
  schemas:
    my.example.v1.Widget:
      type: object
      properties:
        apiVersion:
          type: string
        kind:
          type: string
      x-kubernetes-group-version-kind:
      - group: my.example
        version: v1
        kind: Widget
```

### 7.2 Enrichment hooks

Users provide functions per-resource:

```go
&Resource{
    // ...
    OpenAPIOperation: func(verb string) map[string]any {
        if verb == "list" {
            return map[string]any{
                "description": "List all Widgets",
            }
        }
        return nil
    },
    OpenAPISchema: func() map[string]any {
        return map[string]any{
            "type": "object",
            "properties": map[string]any{
                "apiVersion": map[string]any{"type": "string"},
                "kind":       map[string]any{"type": "string"},
                "spec": map[string]any{
                    "type": "object",
                    "properties": map[string]any{
                        "size": map[string]any{"type": "integer"},
                    },
                },
            },
        }
    },
}
```

The return values are shallow-merged into the generated document. This
avoids requiring the user to build the entire spec from scratch.

### 7.3 Group-wide injection

`Group.OpenAPI` allows injecting endpoints that aren't tied to a
specific resource:

```go
&Group{
    // ...
    OpenAPI: func(schema.GroupVersion) map[string]any {
        return map[string]any{
            "paths": map[string]any{
                "/healthz": map[string]any{
                    "get": map[string]any{"description": "Health check"},
                },
            },
        }
    },
}
```

---

## 8. Error handling and status responses

Every handler returns `(T, error)`. The library serializes success
values as JSON. For errors:

1. Check if the error implements `HTTPStatusError{HTTPStatus() int32}`
2. If yes, use that status code; otherwise 500
3. Serialize a `metav1.Status` object as the response body:

```json
{
  "apiVersion": "v1",
  "kind": "Status",
  "status": "Failure",
  "code": 404,
  "reason": "NotFound",
  "message": "\"my-bucket\" not found"
}
```

Helper constructors (`NotFound`, `AlreadyExists`, `Conflict`,
`Forbidden`) set the `StatusReason` field from an internal lookup
table mapping HTTP status codes to Kubernetes reasons.

---

## 9. Migration guide (current → new API)

### ResourceSpec → Resource

Before:
```go
&kapi.ResourceSpec{
    GroupVersionKind: schema.GroupVersionKind{
        Group:   "api.runkube.internal",
        Version: "v1",
        Kind:    "Bucket",
    },
    Resource: "buckets",
}
```

After:
```go
&kapi.Resource{
    GVK:      schema.GroupVersionKind{Group:"api.runkube.internal", Version:"v1", Kind:"Bucket"},
    Resource: "buckets",
}
```

The `Descriptor()`, `New()`, `NewList()` methods on `ResourceSpec` are
eliminated — the struct fields are used directly by the router.

### Group.Install → internal only

`Group.Install(*http.ServeMux)` was exported in the old API but is now
an internal detail handled by `NewAPI`. Users should not call it
directly.

### Handler registration

Before: no way to add custom handlers.
After: set handler fields on `Resource`.

---

## 10. Future considerations (not in scope for v1)

These may be added later but are deliberately excluded from the
initial design:

- **Admission hooks** — these belong in the wrapping server, not in
  kapi. kapi resources can call admission webhooks manually in their
  handlers.
- **Storage abstraction** — kapi handlers are expected to call their
  own storage. A future `WithStore(rest.Storage)` helper could be added
  on top, but is not part of the core.
- **Code generation** — no `deepcopy` or `zz_generated` files. Users
  work with `*unstructured.Unstructured` and handle their own serde.
- **Conversion / versioning** — multi-version resources belong in
  k8s.io/apiserver. kapi targets single-version APIs.

---

## 11. Open questions (for review)

1. **Handler value vs pointer receivers** — should `Resource` be used
   as `Resource{...}` (value) or `&Resource{...}` (pointer)? The
   `Subresources` slice and `OpenAPI` hooks are pointer fields, so
   `&Resource{...}` seems natural. But nothing prevents value usage
   for simple cases.

> decided: pointer receivers throughout. Resources use `&Resource{...}`.

2. **Watch interface** — `watch.Interface` from apimachinery is
   convenient but brings a dependency on `k8s.io/apimachinery/pkg/watch`.
   Is this acceptable given we already depend on apimachinery?

> decided: keep `watch.Interface`.

3. **SubresourceOf helper return type** — currently returns `*Subresource`
   so it can be directly appended to `[]*Subresource`. If we use value
   slices (`[]Subresource`), the helper would need to return a value
   or the user would always write `*SubresourceOf(...)`. Pointer slices
   feel cleaner.

> decided: pointers. `Resources []*Resource`, `Subresources []*Subresource`,
> `SubresourceOf` returns `*Subresource`.

4. **OpenAPI merge semantics** — "shallow merge" is unambiguous for
   simple cases but could confuse when both the generated spec and
   the user's hook set the same nested key. Deep merge vs. replace
   needs to be documented clearly.

> noted. Keeping shallow merge for now; revisit if it causes confusion.
