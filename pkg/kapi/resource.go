package kapi

import (
	"net/http"
	"path"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var defaultVerbs = metav1.Verbs{
	"get", "list", "watch",
	"create", "update", "patch", "delete",
	"proxy",
}

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
//
// Always use &Resource{...} — the type uses pointer receivers internally.
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

// descriptor returns a metav1.APIResource for discovery.
func (r *Resource) descriptor() metav1.APIResource {
	v := defaultVerbs
	if len(r.Verbs) > 0 {
		v = r.Verbs
	}
	return metav1.APIResource{
		Name:         r.Resource,
		Group:        r.GVK.Group,
		Version:      r.GVK.Version,
		SingularName: r.GVK.Kind,
		Kind:         r.GVK.Kind,
		Namespaced:   r.Namespaced,
		Verbs:        v,
	}
}

// newObject creates a new unstructured object with the resource's GVK set.
func (r *Resource) newObject() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(r.GVK)
	return u
}

// newList creates a new unstructured list with the resource's GVK set.
func (r *Resource) newList() *unstructured.UnstructuredList {
	u := &unstructured.UnstructuredList{}
	u.SetGroupVersionKind(r.GVK)
	return u
}

// collectVerbs returns the set of verbs for which a handler is registered.
// Used to determine which method patterns to register.
func (r *Resource) collectVerbs() map[string]bool {
	verbs := make(map[string]bool)
	if r.List != nil || r.Watch != nil {
		verbs["list"] = true // covers both GET with/without watch
	}
	if r.Get != nil {
		verbs["get"] = true
	}
	if r.Create != nil {
		verbs["create"] = true
	}
	if r.Update != nil {
		verbs["update"] = true
	}
	if r.Delete != nil {
		verbs["delete"] = true
	}
	if r.Patch != nil {
		verbs["patch"] = true
	}
	return verbs
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
	Name string // e.g. "status", "proxy", "port-forward"

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
// SameTypeAsParent = true and returns a pointer. Override handlers
// as needed on the returned Subresource.
func SubresourceOf(parent *Resource, name string) *Subresource {
	return &Subresource{Name: name, SameTypeAsParent: true}
}

// collectVerbs returns the set of verbs for which a subresource handler
// is registered.
func (s *Subresource) collectVerbs() map[string]bool {
	verbs := make(map[string]bool)
	if s.Get != nil {
		verbs["get"] = true
	}
	if s.Update != nil {
		verbs["update"] = true
	}
	if s.Create != nil {
		verbs["create"] = true
	}
	if s.Delete != nil {
		verbs["delete"] = true
	}
	if s.Patch != nil {
		verbs["patch"] = true
	}
	return verbs
}

// ── Path helpers ───────────────────────────────────────────

// expandPath substitutes {group}, {version}, {resource} placeholders.
func expandPath(tmpl, group, version, resource string) string {
	parts := strings.FieldsFunc(tmpl, func(c rune) bool { return c == '/' })
	fp := []string{"/"}
	for _, p := range parts {
		switch p {
		case "{group}":
			fp = append(fp, group)
		case "{version}":
			fp = append(fp, version)
		case "{resource}":
			fp = append(fp, resource)
		default:
			fp = append(fp, p)
		}
	}
	return path.Clean(path.Join(fp...))
}

func pathJoin(elem ...string) string {
	return path.Join(elem...)
}

// openAPIv3 generates a minimal OpenAPI v3 document for this group.
func (g *Group) openAPIv3() map[string]any {
	paths := make(map[string]any)
	schemas := make(map[string]any)

	for _, r := range g.Resources {
		d := r.descriptor()
		basePath := pathJoin("/apis", g.GroupVersion.Group, g.GroupVersion.Version, d.Name)

		// Path entry with minimal operation spec.
		op := map[string]any{
			"get": map[string]any{
				"x-kubernetes-group-version-kind": map[string]any{
					"group":   d.Group,
					"version": d.Version,
					"kind":    d.Kind,
				},
			},
		}
		paths[basePath] = op

		// Apply user enrichment.
		if r.OpenAPIOperation != nil {
			if enriched := r.OpenAPIOperation("list"); enriched != nil {
				mergeMap(op["get"].(map[string]any), enriched)
			}
		}

		// Component schema.
		schemaKey := strings.Join([]string{d.Group, d.Version, d.Kind}, ".")
		schema := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"apiVersion": map[string]any{"type": "string"},
				"kind":       map[string]any{"type": "string"},
			},
			"x-kubernetes-group-version-kind": []any{
				map[string]any{
					"group":   d.Group,
					"version": d.Version,
					"kind":    d.Kind,
				},
			},
		}
		if r.OpenAPISchema != nil {
			if enriched := r.OpenAPISchema(); enriched != nil {
				mergeMap(schema, enriched)
			}
		}
		schemas[schemaKey] = schema
	}

	doc := map[string]any{
		"paths": paths,
		"components": map[string]any{
			"schemas": schemas,
		},
	}

	// Group-wide injection.
	if g.OpenAPI != nil {
		if injected := g.OpenAPI(g.GroupVersion); injected != nil {
			mergeMap(doc, injected)
		}
	}

	return doc
}

// mergeMap shallow-merges src into dst. Keys in src overwrite dst.
func mergeMap(dst, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}