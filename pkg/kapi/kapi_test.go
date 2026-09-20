package kapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func gvk(group, version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}

func gv(group, version string) schema.GroupVersion {
	return schema.GroupVersion{Group: group, Version: version}
}

// ── Service basics ─────────────────────────────────────────

func TestNewAPI_ServeHTTP(t *testing.T) {
	s := NewAPI()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis", nil)
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestNewAPI_NotFound(t *testing.T) {
	s := NewAPI()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/nonexistent", nil)
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestNewAPI_WithGroups(t *testing.T) {
	g := &Group{
		GroupVersion: gv("test.example", "v1"),
		Resources: []*Resource{
			{
				GVK:      gvk("test.example", "v1", "Widget"),
				Resource: "widgets",
			},
		},
	}
	s := NewAPI(g)

	// Group discovery.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis/test.example/v1", nil)
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var rl metav1.APIResourceList
	if err := json.NewDecoder(rr.Body).Decode(&rl); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rl.APIResources) != 1 || rl.APIResources[0].Name != "widgets" {
		t.Fatalf("unexpected resources: %+v", rl.APIResources)
	}
}

// ── Verb dispatch ──────────────────────────────────────────

func TestResource_GetHandler(t *testing.T) {
	g := &Group{
		GroupVersion: gv("test.example", "v1"),
		Resources: []*Resource{
			{
				GVK:      gvk("test.example", "v1", "Widget"),
				Resource: "widgets",
				Namespaced: true,
				Get: func(ctx context.Context, ns, name string, opts metav1.GetOptions) (*unstructured.Unstructured, error) {
					obj := &unstructured.Unstructured{}
					obj.SetGroupVersionKind(gvk("test.example", "v1", "Widget"))
					obj.SetName(name)
					obj.SetNamespace(ns)
					return obj, nil
				},
			},
		},
	}
	s := NewAPI(g)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis/test.example/v1/namespaces/default/widgets/my-widget", nil)
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var obj unstructured.Unstructured
	if err := json.NewDecoder(rr.Body).Decode(&obj); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if obj.GetName() != "my-widget" || obj.GetNamespace() != "default" {
		t.Fatalf("unexpected obj: name=%s ns=%s", obj.GetName(), obj.GetNamespace())
	}
}

func TestResource_ListHandler(t *testing.T) {
	g := &Group{
		GroupVersion: gv("test.example", "v1"),
		Resources: []*Resource{
			{
				GVK:      gvk("test.example", "v1", "Widget"),
				Resource: "widgets",
				Namespaced: true,
				List: func(ctx context.Context, ns string, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
					l := &unstructured.UnstructuredList{}
					l.SetGroupVersionKind(gvk("test.example", "v1", "Widget"))
					return l, nil
				},
			},
		},
	}
	s := NewAPI(g)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis/test.example/v1/namespaces/default/widgets", nil)
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestResource_CreateHandler(t *testing.T) {
	g := &Group{
		GroupVersion: gv("test.example", "v1"),
		Resources: []*Resource{
			{
				GVK:      gvk("test.example", "v1", "Widget"),
				Resource: "widgets",
				Namespaced: true,
				Create: func(ctx context.Context, ns string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
					obj.SetNamespace(ns)
					obj.SetName("created")
					return obj, nil
				},
			},
		},
	}
	s := NewAPI(g)

	body := `{"apiVersion":"test.example/v1","kind":"Widget","metadata":{"name":"test"}}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/apis/test.example/v1/namespaces/default/widgets", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestResource_UpdateHandler(t *testing.T) {
	g := &Group{
		GroupVersion: gv("test.example", "v1"),
		Resources: []*Resource{
			{
				GVK:      gvk("test.example", "v1", "Widget"),
				Resource: "widgets",
				Namespaced: true,
				Update: func(ctx context.Context, ns, name string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
					obj.SetName(name)
					obj.SetNamespace(ns)
					return obj, nil
				},
			},
		},
	}
	s := NewAPI(g)

	body := `{"apiVersion":"test.example/v1","kind":"Widget","metadata":{"name":"updated"}}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/apis/test.example/v1/namespaces/default/widgets/my-widget", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestResource_DeleteHandler(t *testing.T) {
	var deleted string
	g := &Group{
		GroupVersion: gv("test.example", "v1"),
		Resources: []*Resource{
			{
				GVK:      gvk("test.example", "v1", "Widget"),
				Resource: "widgets",
				Namespaced: true,
				Delete: func(ctx context.Context, ns, name string) error {
					deleted = name
					return nil
				},
			},
		},
	}
	s := NewAPI(g)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/apis/test.example/v1/namespaces/default/widgets/my-widget", nil)
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rr.Code, rr.Body.String())
	}
	if deleted != "my-widget" {
		t.Fatalf("expected deleted name 'my-widget', got %q", deleted)
	}
}

func TestResource_PatchHandler(t *testing.T) {
	var gotPatchType types.PatchType
	g := &Group{
		GroupVersion: gv("test.example", "v1"),
		Resources: []*Resource{
			{
				GVK:      gvk("test.example", "v1", "Widget"),
				Resource: "widgets",
				Namespaced: true,
				Patch: func(ctx context.Context, ns, name string, pt types.PatchType, data []byte) (*unstructured.Unstructured, error) {
					gotPatchType = pt
					obj := &unstructured.Unstructured{}
					obj.SetName(name)
					return obj, nil
				},
			},
		},
	}
	s := NewAPI(g)

	patchBody := `{"spec":{"size":3}}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PATCH", "/apis/test.example/v1/namespaces/default/widgets/my-widget", strings.NewReader(patchBody))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if gotPatchType != types.MergePatchType {
		t.Fatalf("expected MergePatchType, got %v", gotPatchType)
	}
}

// ── 405 on unregistered verb ───────────────────────────────

func TestResource_UnregisteredVerb_405(t *testing.T) {
	g := &Group{
		GroupVersion: gv("test.example", "v1"),
		Resources: []*Resource{
			{
				GVK:      gvk("test.example", "v1", "Widget"),
				Resource: "widgets",
				Namespaced: true,
				// No Get handler registered.
			},
		},
	}
	s := NewAPI(g)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis/test.example/v1/namespaces/default/widgets/my-widget", nil)
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── RawHandler fall-through ─────────────────────────────────

func TestResource_RawHandlerFallthrough(t *testing.T) {
	var rawCalled bool
	g := &Group{
		GroupVersion: gv("test.example", "v1"),
		Resources: []*Resource{
			{
				GVK:      gvk("test.example", "v1", "Widget"),
				Resource: "widgets",
				Namespaced: true,
				// No Get handler, but RawHandler set.
				RawHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					rawCalled = true
					w.WriteHeader(http.StatusOK)
				}),
			},
		},
	}
	s := NewAPI(g)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis/test.example/v1/namespaces/default/widgets/my-widget", nil)
	s.ServeHTTP(rr, req)

	if !rawCalled {
		t.Fatal("RawHandler was not called")
	}
}

func TestResource_RawHandlerSkipsRegisteredVerb(t *testing.T) {
	var getCalled bool
	g := &Group{
		GroupVersion: gv("test.example", "v1"),
		Resources: []*Resource{
			{
				GVK:      gvk("test.example", "v1", "Widget"),
				Resource: "widgets",
				Namespaced: true,
				Get: func(ctx context.Context, ns, name string, opts metav1.GetOptions) (*unstructured.Unstructured, error) {
					getCalled = true
					return &unstructured.Unstructured{}, nil
				},
				RawHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					t.Fatal("RawHandler should not be called when a verb handler is set")
				}),
			},
		},
	}
	s := NewAPI(g)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis/test.example/v1/namespaces/default/widgets/my-widget", nil)
	s.ServeHTTP(rr, req)

	if !getCalled {
		t.Fatal("Get handler should have been called")
	}
	_ = rr
}

// ── Cluster-scoped resource ────────────────────────────────

func TestResource_ClusterScoped(t *testing.T) {
	var called bool
	g := &Group{
		GroupVersion: gv("test.example", "v1"),
		Resources: []*Resource{
			{
				GVK:      gvk("test.example", "v1", "ClusterWidget"),
				Resource: "clusterwidgets",
				Namespaced: false,
				Get: func(ctx context.Context, ns, name string, opts metav1.GetOptions) (*unstructured.Unstructured, error) {
					called = true
					if ns != "" {
						t.Fatalf("cluster-scoped resource should have empty namespace, got %q", ns)
					}
					return &unstructured.Unstructured{}, nil
				},
			},
		},
	}
	s := NewAPI(g)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis/test.example/v1/clusterwidgets/my-widget", nil)
	s.ServeHTTP(rr, req)

	if !called {
		t.Fatal("Get handler was not called")
	}
	_ = rr
}

// ── Subresource ────────────────────────────────────────────

func TestSubresource_Get(t *testing.T) {
	var subCalled bool
	g := &Group{
		GroupVersion: gv("test.example", "v1"),
		Resources: []*Resource{
			{
				GVK:      gvk("test.example", "v1", "Widget"),
				Resource: "widgets",
				Namespaced: true,
				Subresources: []*Subresource{
					{
						Name: "status",
						Get: func(ctx context.Context, ns, name string, opts metav1.GetOptions) (*unstructured.Unstructured, error) {
							subCalled = true
							return &unstructured.Unstructured{}, nil
						},
					},
				},
			},
		},
	}
	s := NewAPI(g)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis/test.example/v1/namespaces/default/widgets/my-widget/status", nil)
	s.ServeHTTP(rr, req)

	if !subCalled {
		t.Fatal("subresource Get handler was not called")
	}
	_ = rr
}

func TestSubresource_RawHandlerFallthrough(t *testing.T) {
	var rawCalled bool
	g := &Group{
		GroupVersion: gv("test.example", "v1"),
		Resources: []*Resource{
			{
				GVK:      gvk("test.example", "v1", "Widget"),
				Resource: "widgets",
				Namespaced: true,
				Subresources: []*Subresource{
					{
						Name: "proxy",
						RawHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							rawCalled = true
						}),
					},
				},
			},
		},
	}
	s := NewAPI(g)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis/test.example/v1/namespaces/default/widgets/my-widget/proxy", nil)
	s.ServeHTTP(rr, req)

	if !rawCalled {
		t.Fatal("subresource RawHandler was not called")
	}
	_ = rr
}

// ── Error handlers ─────────────────────────────────────────

func TestNotFound(t *testing.T) {
	err := NotFound("my-resource")
	if err.Error() != `"my-resource" not found` {
		t.Fatalf("unexpected message: %s", err.Error())
	}
	se, ok := err.(HTTPStatusError)
	if !ok {
		t.Fatal("NotFound should implement HTTPStatusError")
	}
	if se.HTTPStatus() != 404 {
		t.Fatalf("expected 404, got %d", se.HTTPStatus())
	}
}

func TestAlreadyExists(t *testing.T) {
	err := AlreadyExists("my-resource")
	se, ok := err.(HTTPStatusError)
	if !ok {
		t.Fatal("AlreadyExists should implement HTTPStatusError")
	}
	if se.HTTPStatus() != 409 {
		t.Fatalf("expected 409, got %d", se.HTTPStatus())
	}
}

func TestConflict(t *testing.T) {
	err := Conflict("resource version mismatch")
	se, ok := err.(HTTPStatusError)
	if !ok {
		t.Fatal("Conflict should implement HTTPStatusError")
	}
	if se.HTTPStatus() != 409 {
		t.Fatalf("expected 409, got %d", se.HTTPStatus())
	}
}

func TestForbidden(t *testing.T) {
	err := Forbidden("access denied")
	se, ok := err.(HTTPStatusError)
	if !ok {
		t.Fatal("Forbidden should implement HTTPStatusError")
	}
	if se.HTTPStatus() != 403 {
		t.Fatalf("expected 403, got %d", se.HTTPStatus())
	}
}

// ── OpenAPI ────────────────────────────────────────────────

func TestOpenAPI_Discovery(t *testing.T) {
	g := &Group{
		GroupVersion: gv("test.example", "v1"),
		Resources: []*Resource{
			{
				GVK:      gvk("test.example", "v1", "Widget"),
				Resource: "widgets",
			},
		},
	}
	s := NewAPI(g)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/openapi/v3", nil)
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var doc struct {
		Paths map[string]struct {
			ServerRelativeURL string `json:"serverRelativeURL"`
		} `json:"paths"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	key := "apis/test.example/v1"
	if _, ok := doc.Paths[key]; !ok {
		t.Fatalf("expected path %q in openapi discovery, got %+v", key, doc.Paths)
	}
}

func TestOpenAPI_GroupDoc(t *testing.T) {
	g := &Group{
		GroupVersion: gv("test.example", "v1"),
		Resources: []*Resource{
			{
				GVK:      gvk("test.example", "v1", "Widget"),
				Resource: "widgets",
			},
		},
	}
	s := NewAPI(g)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/openapi/v3/apis/test.example/v1", nil)
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var doc map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	paths, _ := doc["paths"].(map[string]any)
	if len(paths) == 0 {
		t.Fatal("expected at least one path in OpenAPI doc")
	}

	components, _ := doc["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	if len(schemas) == 0 {
		t.Fatal("expected at least one schema in OpenAPI doc")
	}
}

// ── APIService generation ───────────────────────────────────

func TestAPIServices(t *testing.T) {
	g := &Group{
		GroupVersion: gv("test.example", "v1"),
		Resources: []*Resource{
			{
				GVK:      gvk("test.example", "v1", "Widget"),
				Resource: "widgets",
			},
		},
	}
	s := NewAPI(g)

	count := 0
	for obj := range s.APIServices([]byte("dGVzdA=="), "kube-system", "my-api") {
		count++
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			t.Fatal("expected Unstructured")
		}
		if u.GetKind() != "APIService" {
			t.Fatalf("expected APIService, got %s", u.GetKind())
		}
	}
	if count != 1 {
		t.Fatalf("expected 1 APIService, got %d", count)
	}
}

// ── Parse patch type ───────────────────────────────────────

func TestParsePatchType(t *testing.T) {
	tests := []struct {
		ct  string
		expected types.PatchType
	}{
		{"application/json-patch+json", types.JSONPatchType},
		{"application/merge-patch+json", types.MergePatchType},
		{"application/strategic-merge-patch+json", types.StrategicMergePatchType},
		{"application/apply-patch+yaml", types.ApplyPatchType},
		{"application/json", types.MergePatchType},
		{"", types.MergePatchType},
	}

	for _, tt := range tests {
		got := parsePatchType(tt.ct)
		if got != tt.expected {
			t.Errorf("parsePatchType(%q) = %v, want %v", tt.ct, got, tt.expected)
		}
	}
}

// ── HTTP status error mapping ──────────────────────────────

func TestWriteError(t *testing.T) {
	tests := []struct {
		err      error
		expected int
	}{
		{NotFound("x"), 404},
		{Forbidden("nope"), 403},
		{Conflict("x"), 409},
		{fmt.Errorf("generic"), 500},
	}

	for _, tt := range tests {
		rr := httptest.NewRecorder()
		writeError(rr, tt.err)
		if rr.Code != tt.expected {
			t.Errorf("writeError(%v) = %d, want %d", tt.err, rr.Code, tt.expected)
		}
	}
}