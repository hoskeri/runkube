package kapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"net/http"
)

// ── Helper: a simple in-memory store for testing ───────────

type widget struct {
	Name      string
	Namespace string
	Spec      map[string]any
}

type widgetStore struct {
	items map[string]*widget // key: "ns/name" or just "name" for cluster-scoped
}

func newWidgetStore() *widgetStore {
	return &widgetStore{items: make(map[string]*widget)}
}

func (s *widgetStore) key(name, ns string) string {
	if ns == "" {
		return name
	}
	return ns + "/" + name
}

// ── Example 1: Cluster-scoped resource with Get, Create, Delete ─

func TestClusterScoped_GetCreateDelete(t *testing.T) {
	store := newWidgetStore()

	res := &Resource{
		GVK:        gvk("example.io", "v1", "Gadget"),
		Resource:   "gadgets",
		Namespaced: false,
		Getter: GetFunc(func(ctx context.Context, name string) (*unstructured.Unstructured, error) {
			w, ok := store.items[name]
			if !ok {
				return nil, NotFound(name)
			}
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(gvk("example.io", "v1", "Gadget"))
			obj.SetName(w.Name)
			return obj, nil
		}),
		Creater: CreateFunc(func(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
			name := obj.GetName()
			if name == "" {
				return nil, BadRequest("name is required")
			}
			store.items[name] = &widget{Name: name}
			obj.SetGroupVersionKind(gvk("example.io", "v1", "Gadget"))
			return obj, nil
		}),
		Deleter: DeleteFunc(func(ctx context.Context, name string) error {
			if _, ok := store.items[name]; !ok {
				return NotFound(name)
			}
			delete(store.items, name)
			return nil
		}),
	}

	g := &Group{GroupVersion: gv("example.io", "v1"), Resources: []*Resource{res}}
	s := NewAPI(g)

	// Create.
	body := `{"apiVersion":"example.io/v1","kind":"Gadget","metadata":{"name":"g1"}}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/apis/example.io/v1/gadgets", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Get.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/apis/example.io/v1/gadgets/g1", nil)
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", rr.Code)
	}

	// Get nonexistent.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/apis/example.io/v1/gadgets/nope", nil)
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("get missing: expected 404, got %d", rr.Code)
	}

	// Delete.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("DELETE", "/apis/example.io/v1/gadgets/g1", nil)
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d", rr.Code)
	}
}

// ── Example 2: Namespaced resource with List ───────────────

func TestNamespaced_List(t *testing.T) {
	store := newWidgetStore()
	store.items["ns1/a"] = &widget{Name: "a", Namespace: "ns1"}
	store.items["ns1/b"] = &widget{Name: "b", Namespace: "ns1"}

	res := &Resource{
		GVK:        gvk("example.io", "v1", "Widget"),
		Resource:   "widgets",
		Namespaced: true,
		Lister: &ListWatch{
			ListFunc: func(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
				l := &unstructured.UnstructuredList{}
				l.SetGroupVersionKind(gvk("example.io", "v1", "Widget"))
				// In a real implementation, namespace would come from context.
				// Here we just return all items for simplicity.
				for _, w := range store.items {
					obj := &unstructured.Unstructured{}
					obj.SetGroupVersionKind(gvk("example.io", "v1", "Widget"))
					obj.SetName(w.Name)
					l.Items = append(l.Items, *obj)
				}
				return l, nil
			},
			// No Watch support — WatchFunc is nil.
		},
	}

	g := &Group{GroupVersion: gv("example.io", "v1"), Resources: []*Resource{res}}
	s := NewAPI(g)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis/example.io/v1/namespaces/ns1/widgets", nil)
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var lst unstructured.UnstructuredList
	if err := json.NewDecoder(rr.Body).Decode(&lst); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(lst.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(lst.Items))
	}
}

// ── Example 3: PATCH with merge patch ──────────────────────

func TestPatchMergePatch(t *testing.T) {
	res := &Resource{
		GVK:        gvk("example.io", "v1", "Widget"),
		Resource:   "widgets",
		Namespaced: true,
		Patcher: PatchFunc(func(ctx context.Context, name string, pt types.PatchType, data []byte) (*unstructured.Unstructured, error) {
			if pt != types.MergePatchType {
				t.Errorf("expected MergePatchType, got %v", pt)
			}
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(gvk("example.io", "v1", "Widget"))
			obj.SetName(name)
			return obj, nil
		}),
	}

	g := &Group{GroupVersion: gv("example.io", "v1"), Resources: []*Resource{res}}
	s := NewAPI(g)

	body := `{"spec":{"size":3}}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PATCH", "/apis/example.io/v1/namespaces/default/widgets/w1",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/merge-patch+json")
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: expected 200, got %d", rr.Code)
	}
}

// ── Example 4: Update ──────────────────────────────────────

func TestUpdate(t *testing.T) {
	var updatedName string
	res := &Resource{
		GVK:        gvk("example.io", "v1", "Widget"),
		Resource:   "widgets",
		Namespaced: true,
		Updater: UpdateFunc(func(ctx context.Context, name string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
			updatedName = name
			obj.SetName(name)
			return obj, nil
		}),
	}

	g := &Group{GroupVersion: gv("example.io", "v1"), Resources: []*Resource{res}}
	s := NewAPI(g)

	body := `{"apiVersion":"example.io/v1","kind":"Widget","metadata":{"name":"w1"}}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/apis/example.io/v1/namespaces/default/widgets/w1",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d", rr.Code)
	}
	if updatedName != "w1" {
		t.Fatalf("expected name 'w1', got %q", updatedName)
	}
}

// ── Example 5: DeleteCollection ────────────────────────────

func TestDeleteCollection(t *testing.T) {
	var deleted bool
	res := &Resource{
		GVK:        gvk("example.io", "v1", "Widget"),
		Resource:   "widgets",
		Namespaced: true,
		CollectionDeleter: DeleteCollectionFunc(func(ctx context.Context, opts metav1.ListOptions) error {
			deleted = true
			return nil
		}),
	}

	g := &Group{GroupVersion: gv("example.io", "v1"), Resources: []*Resource{res}}
	s := NewAPI(g)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/apis/example.io/v1/namespaces/default/widgets", nil)
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("deletecollection: expected 204, got %d", rr.Code)
	}
	if !deleted {
		t.Fatal("DeleteCollection was not called")
	}
}

// ── Example 6: 405 for unimplemented operations ────────────

func TestUnimplementedOperation_405(t *testing.T) {
	res := &Resource{
		GVK:        gvk("example.io", "v1", "Widget"),
		Resource:   "widgets",
		Namespaced: true,
		// Only Getter is set — no Creater, Updater, etc.
		Getter: GetFunc(func(ctx context.Context, name string) (*unstructured.Unstructured, error) {
			return &unstructured.Unstructured{}, nil
		}),
	}

	g := &Group{GroupVersion: gv("example.io", "v1"), Resources: []*Resource{res}}
	s := NewAPI(g)

	// PUT (Update) without Updater → 405.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/apis/example.io/v1/namespaces/default/widgets/w1",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

// ── Example 7: RawHandler fall-through ─────────────────────

func TestRawHandler_Fallthrough(t *testing.T) {
	var rawCalled bool
	res := &Resource{
		GVK:        gvk("example.io", "v1", "Widget"),
		Resource:   "widgets",
		Namespaced: true,
		RawHandler: func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
			rawCalled = true
			w.WriteHeader(http.StatusOK)
		},
	}

	g := &Group{GroupVersion: gv("example.io", "v1"), Resources: []*Resource{res}}
	s := NewAPI(g)

	// GET without Getter → falls through to RawHandler.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis/example.io/v1/namespaces/default/widgets/w1", nil)
	s.ServeHTTP(rr, req)
	if !rawCalled {
		t.Fatal("RawHandler was not called")
	}
	_ = rr
}

// ── Example 8: RawHandler not called when operation is present ─

func TestRawHandler_NotCalledForRegisteredOp(t *testing.T) {
	res := &Resource{
		GVK:        gvk("example.io", "v1", "Widget"),
		Resource:   "widgets",
		Namespaced: true,
		Getter: GetFunc(func(ctx context.Context, name string) (*unstructured.Unstructured, error) {
			return &unstructured.Unstructured{}, nil
		}),
		RawHandler: func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
			t.Fatal("RawHandler should not be called when Getter is set")
		},
	}

	g := &Group{GroupVersion: gv("example.io", "v1"), Resources: []*Resource{res}}
	s := NewAPI(g)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis/example.io/v1/namespaces/default/widgets/w1", nil)
	s.ServeHTTP(rr, req)
	_ = rr
}

// ── Example 9: Subresource ─────────────────────────────────

func TestSubresource_StatusGet(t *testing.T) {
	var subCalled bool
	res := &Resource{
		GVK:        gvk("example.io", "v1", "Widget"),
		Resource:   "widgets",
		Namespaced: true,
		Subresources: []*Subresource{
			{
				Name: "status",
				Getter: GetFunc(func(ctx context.Context, name string) (*unstructured.Unstructured, error) {
					subCalled = true
					obj := &unstructured.Unstructured{}
					obj.SetGroupVersionKind(gvk("example.io", "v1", "Widget"))
					obj.SetName(name)
					return obj, nil
				}),
			},
		},
	}

	g := &Group{GroupVersion: gv("example.io", "v1"), Resources: []*Resource{res}}
	s := NewAPI(g)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis/example.io/v1/namespaces/default/widgets/w1/status", nil)
	s.ServeHTTP(rr, req)
	if !subCalled {
		t.Fatal("subresource Getter was not called")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

// ── Example 10: Subresource with RawHandler (proxy-like) ───

func TestSubresource_ProxyRawHandler(t *testing.T) {
	var rawCalled bool
	res := &Resource{
		GVK:        gvk("example.io", "v1", "Widget"),
		Resource:   "widgets",
		Namespaced: true,
		Subresources: []*Subresource{
			{
				Name: "proxy",
				RawHandler: func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
					rawCalled = true
					w.WriteHeader(http.StatusOK)
				},
			},
		},
	}

	g := &Group{GroupVersion: gv("example.io", "v1"), Resources: []*Resource{res}}
	s := NewAPI(g)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis/example.io/v1/namespaces/default/widgets/w1/proxy", nil)
	s.ServeHTTP(rr, req)
	if !rawCalled {
		t.Fatal("subresource RawHandler was not called")
	}
	_ = rr

	// Also test deeper path.
	rawCalled = false
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/apis/example.io/v1/namespaces/default/widgets/w1/proxy/deep/path", nil)
	s.ServeHTTP(rr, req)
	if !rawCalled {
		t.Fatal("subresource RawHandler was not called for deep path")
	}
	_ = rr
}

// ── Example 11: Watch via Lister ───────────────────────────

func TestListWatch(t *testing.T) {
	ch := make(chan watch.Event, 1)
	ch <- watch.Event{Type: watch.Added, Object: &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "example.io/v1",
			"kind":       "Widget",
			"metadata":   map[string]any{"name": "w1"},
		},
	}}
	close(ch)

	res := &Resource{
		GVK:        gvk("example.io", "v1", "Widget"),
		Resource:   "widgets",
		Namespaced: true,
		Lister: &ListWatch{
			ListFunc: func(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
				return &unstructured.UnstructuredList{}, nil
			},
			WatchFunc: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
				return watch.NewProxyWatcher(ch), nil
			},
		},
	}

	g := &Group{GroupVersion: gv("example.io", "v1"), Resources: []*Resource{res}}
	s := NewAPI(g)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/apis/example.io/v1/namespaces/default/widgets?watch=true", nil)
	s.ServeHTTP(rr, req)

	// Watch returns 200 and streams. Since the channel has one event,
	// the handler should stream it and return when channel closes.
	if rr.Code != http.StatusOK {
		t.Fatalf("watch: expected 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"type":"ADDED"`) {
		t.Fatalf("watch: expected ADDED event, got: %s", rr.Body.String())
	}
}

// ── Additional tests ───────────────────────────────────────

func TestNewAPI_Discovery(t *testing.T) {
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

func TestGroupDiscovery(t *testing.T) {
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

func TestErrorConstructors(t *testing.T) {
	tests := []struct {
		err      error
		expected int
	}{
		{NotFound("x"), 404},
		{Forbidden("nope"), 403},
		{Conflict("x"), 409},
		{AlreadyExists("x"), 409},
		{BadRequest("bad"), 400},
		{InternalError("oops"), 500},
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

func TestParsePatchType(t *testing.T) {
	tests := []struct {
		ct       string
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

func TestOpenAPIDiscovery(t *testing.T) {
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
	if _, ok := doc.Paths["apis/test.example/v1"]; !ok {
		t.Fatalf("expected path in openapi discovery, got %+v", doc.Paths)
	}
}

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

// ── Helpers ────────────────────────────────────────────────

func gvk(group, version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}

func gv(group, version string) schema.GroupVersion {
	return schema.GroupVersion{Group: group, Version: version}
}
