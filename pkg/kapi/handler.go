package kapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

// ── Handler function types ─────────────────────────────────

type ListHandler   func(ctx context.Context, namespace string, opts metav1.ListOptions) (*unstructured.UnstructuredList, error)
type GetHandler    func(ctx context.Context, namespace, name string, opts metav1.GetOptions) (*unstructured.Unstructured, error)
type CreateHandler func(ctx context.Context, namespace string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
type UpdateHandler func(ctx context.Context, namespace, name string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
type DeleteHandler func(ctx context.Context, namespace, name string) error
type PatchHandler  func(ctx context.Context, namespace, name string, pt types.PatchType, data []byte) (*unstructured.Unstructured, error)

// WatchHandler returns a watch.Interface (from k8s.io/apimachinery/pkg/watch).
// The library handles streaming the events as JSON lines and cleaning
// up when the client disconnects or the context is cancelled.
type WatchHandler func(ctx context.Context, namespace string, opts metav1.ListOptions) (watch.Interface, error)

// ── Resource installation ──────────────────────────────────

// install registers all mux patterns for this resource into m.
func (r *Resource) install(g *Group, m *http.ServeMux) {
	base := path.Join("/apis", g.GroupVersion.Group, g.GroupVersion.Version)
	resourcePath := path.Join(base, r.Resource)

	hasRaw := r.RawHandler != nil

	if r.Namespaced {
		r.installNamespaced(m, base, resourcePath, hasRaw)
	} else {
		r.installClusterScoped(m, base, resourcePath, hasRaw)
	}
}

func (r *Resource) installClusterScoped(m *http.ServeMux, base, resourcePath string, hasRaw bool) {
	// Collection endpoint.
	m.HandleFunc("GET "+resourcePath, dispatchCollection(r))
	m.HandleFunc("POST "+resourcePath, dispatchCreate(r))

	// Item endpoint.
	itemPattern := resourcePath + "/{name}"
	m.HandleFunc("GET "+itemPattern, dispatchGet(r))
	m.HandleFunc("PUT "+itemPattern, dispatchUpdate(r))
	m.HandleFunc("PATCH "+itemPattern, dispatchPatch(r))
	m.HandleFunc("DELETE "+itemPattern, dispatchDelete(r))

	// Subresources and catch-all.
	r.installSubresources(m, itemPattern, hasRaw)
}

func (r *Resource) installNamespaced(m *http.ServeMux, base, resourcePath string, hasRaw bool) {
	// Collection endpoint with namespace.
	nsCollection := path.Join(base, "namespaces", "{namespace}", r.Resource)
	m.HandleFunc("GET "+nsCollection, dispatchCollection(r))
	m.HandleFunc("POST "+nsCollection, dispatchCreate(r))

	// Item endpoint.
	itemPattern := nsCollection + "/{name}"
	m.HandleFunc("GET "+itemPattern, dispatchGet(r))
	m.HandleFunc("PUT "+itemPattern, dispatchUpdate(r))
	m.HandleFunc("PATCH "+itemPattern, dispatchPatch(r))
	m.HandleFunc("DELETE "+itemPattern, dispatchDelete(r))

	// Subresources and catch-all.
	r.installSubresources(m, itemPattern, hasRaw)
}

func (r *Resource) installSubresources(m *http.ServeMux, itemPattern string, hasRaw bool) {
	// Register specific subresource patterns.
	for _, sub := range r.Subresources {
		subPattern := itemPattern + "/" + sub.Name
		if sub.Get != nil {
			m.HandleFunc("GET "+subPattern, dispatchSubGet(sub))
		}
		if sub.Update != nil {
			m.HandleFunc("PUT "+subPattern, dispatchSubUpdate(sub))
		}
		if sub.Create != nil {
			m.HandleFunc("POST "+subPattern, dispatchSubCreate(sub))
		}
		if sub.Delete != nil {
			m.HandleFunc("DELETE "+subPattern, dispatchSubDelete(sub))
		}
		if sub.Patch != nil {
			m.HandleFunc("PATCH "+subPattern, dispatchSubPatch(sub))
		}

		// Subresource catch-all: register both the exact subresource path
		// and the /{rest...} pattern so RawHandler catches direct access
		// (e.g., GET /name/proxy) as well as deeper paths.
		if sub.RawHandler != nil {
			for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
				m.HandleFunc(method+" "+subPattern, sub.RawHandler.ServeHTTP)
				m.HandleFunc(method+" "+subPattern+"/{rest...}", sub.RawHandler.ServeHTTP)
			}
		}
	}

	// Catch-all for item/sub-paths that don't match any subresource.
	// RawHandler handles unrecognized sub-paths; without it, 404.
	if hasRaw {
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
			m.HandleFunc(method+" "+itemPattern+"/{rest...}", r.RawHandler.ServeHTTP)
		}
	}
}

// ── Dispatch helpers ───────────────────────────────────────

// dispatchCollection handles GET on a collection — delegates to Watch
// if ?watch=true, otherwise List.
func dispatchCollection(r *Resource) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ns := req.PathValue("namespace")

		// Check watch.
		op := metav1.ListOptions{}
		if u := req.URL.Query(); u.Has("watch") {
			metav1.Convert_url_Values_To_v1_ListOptions(&u, &op, nil)
		}
		if op.Watch {
			if r.Watch != nil {
				serveWatch(w, req, r.Watch, ns, op)
				return
			}
			if r.RawHandler != nil {
				r.RawHandler.ServeHTTP(w, req)
				return
			}
			writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("watch"))
			return
		}

		if r.List != nil {
			serveList(w, req, r.List, ns, op)
			return
		}
		if r.RawHandler != nil {
			r.RawHandler.ServeHTTP(w, req)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("list"))
	}
}

func dispatchCreate(r *Resource) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ns := req.PathValue("namespace")

		if r.Create != nil {
			serveCreate(w, req, r.Create, ns)
			return
		}
		if r.RawHandler != nil {
			r.RawHandler.ServeHTTP(w, req)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("create"))
	}
}

func dispatchGet(r *Resource) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ns := req.PathValue("namespace")
		name := req.PathValue("name")

		op := metav1.GetOptions{}
		if u := req.URL.Query(); len(u) > 0 {
			metav1.Convert_url_Values_To_v1_GetOptions(&u, &op, nil)
		}

		if r.Get != nil {
			serveGet(w, req, r.Get, ns, name, op)
			return
		}
		if r.RawHandler != nil {
			r.RawHandler.ServeHTTP(w, req)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("get"))
	}
}

func dispatchUpdate(r *Resource) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ns := req.PathValue("namespace")
		name := req.PathValue("name")

		if r.Update != nil {
			serveUpdate(w, req, r.Update, ns, name)
			return
		}
		if r.RawHandler != nil {
			r.RawHandler.ServeHTTP(w, req)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("update"))
	}
}

func dispatchDelete(r *Resource) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ns := req.PathValue("namespace")
		name := req.PathValue("name")

		if r.Delete != nil {
			serveDelete(w, req, r.Delete, ns, name)
			return
		}
		if r.RawHandler != nil {
			r.RawHandler.ServeHTTP(w, req)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("delete"))
	}
}

func dispatchPatch(r *Resource) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ns := req.PathValue("namespace")
		name := req.PathValue("name")
		ct := req.Header.Get("Content-Type")

		pt := parsePatchType(ct)

		if r.Patch != nil {
			servePatch(w, req, r.Patch, ns, name, pt)
			return
		}
		if r.RawHandler != nil {
			r.RawHandler.ServeHTTP(w, req)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("patch"))
	}
}

// ── Subresource dispatch ───────────────────────────────────

func dispatchSubGet(sub *Subresource) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ns := req.PathValue("namespace")
		name := req.PathValue("name")

		op := metav1.GetOptions{}
		if u := req.URL.Query(); len(u) > 0 {
			metav1.Convert_url_Values_To_v1_GetOptions(&u, &op, nil)
		}

		if sub.Get != nil {
			serveGet(w, req, sub.Get, ns, name, op)
			return
		}
		if sub.RawHandler != nil {
			sub.RawHandler.ServeHTTP(w, req)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("get"))
	}
}

func dispatchSubUpdate(sub *Subresource) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ns := req.PathValue("namespace")
		name := req.PathValue("name")

		if sub.Update != nil {
			serveUpdate(w, req, sub.Update, ns, name)
			return
		}
		if sub.RawHandler != nil {
			sub.RawHandler.ServeHTTP(w, req)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("update"))
	}
}

func dispatchSubCreate(sub *Subresource) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ns := req.PathValue("namespace")
		_ = req.PathValue("name")

		if sub.Create != nil {
			serveCreate(w, req, sub.Create, ns)
			return
		}
		if sub.RawHandler != nil {
			sub.RawHandler.ServeHTTP(w, req)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("create"))
	}
}

func dispatchSubDelete(sub *Subresource) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ns := req.PathValue("namespace")
		name := req.PathValue("name")

		if sub.Delete != nil {
			serveDelete(w, req, sub.Delete, ns, name)
			return
		}
		if sub.RawHandler != nil {
			sub.RawHandler.ServeHTTP(w, req)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("delete"))
	}
}

func dispatchSubPatch(sub *Subresource) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ns := req.PathValue("namespace")
		name := req.PathValue("name")
		ct := req.Header.Get("Content-Type")

		pt := parsePatchType(ct)

		if sub.Patch != nil {
			servePatch(w, req, sub.Patch, ns, name, pt)
			return
		}
		if sub.RawHandler != nil {
			sub.RawHandler.ServeHTTP(w, req)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("patch"))
	}
}

// ── Handler invokers ───────────────────────────────────────

func serveList(w http.ResponseWriter, r *http.Request, fn ListHandler, ns string, opts metav1.ListOptions) {
	obj, err := fn(context.Background(), ns, opts)
	if err != nil {
		writeError(w, err)
		return
	}
	encode(w, obj)
}

func serveGet(w http.ResponseWriter, r *http.Request, fn GetHandler, ns, name string, opts metav1.GetOptions) {
	obj, err := fn(context.Background(), ns, name, opts)
	if err != nil {
		writeError(w, err)
		return
	}
	encode(w, obj)
}

func serveCreate(w http.ResponseWriter, r *http.Request, fn CreateHandler, ns string) {
	obj, err := decodeBody(r)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, err)
		return
	}
	result, err := fn(context.Background(), ns, obj)
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	encode(w, result)
}

func serveUpdate(w http.ResponseWriter, r *http.Request, fn UpdateHandler, ns, name string) {
	obj, err := decodeBody(r)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, err)
		return
	}
	result, err := fn(context.Background(), ns, name, obj)
	if err != nil {
		writeError(w, err)
		return
	}
	encode(w, result)
}

func serveDelete(w http.ResponseWriter, r *http.Request, fn DeleteHandler, ns, name string) {
	_ = name
	err := fn(context.Background(), ns, name)
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func servePatch(w http.ResponseWriter, r *http.Request, fn PatchHandler, ns, name string, pt types.PatchType) {
	data, err := readBody(r)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, err)
		return
	}
	result, err := fn(context.Background(), ns, name, pt, data)
	if err != nil {
		writeError(w, err)
		return
	}
	encode(w, result)
}

// ── Helpers ────────────────────────────────────────────────

func decodeBody(r *http.Request) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	if err := json.NewDecoder(r.Body).Decode(obj); err != nil {
		return nil, fmt.Errorf("failed to decode request body: %w", err)
	}
	return obj, nil
}

func readBody(r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}
	return data, nil
}

func parsePatchType(ct string) types.PatchType {
	// Content-Type values: application/json-patch+json, application/merge-patch+json,
	// application/strategic-merge-patch+json, application/apply-patch+yaml
	// Order matters: check specific subtypes before general ones.
	switch {
	case strings.Contains(ct, "json-patch"):
		return types.JSONPatchType
	case strings.Contains(ct, "strategic-merge-patch"):
		return types.StrategicMergePatchType
	case strings.Contains(ct, "merge-patch"):
		return types.MergePatchType
	case strings.Contains(ct, "apply-patch"):
		return types.ApplyPatchType
	default:
		// Kubernetes defaults to merge patch when no specific type is indicated.
		return types.MergePatchType
	}
}

func errMethodNotAllowed(verb string) error {
	return &statusError{code: http.StatusMethodNotAllowed, message: fmt.Sprintf("%s is not supported", verb)}
}

func writeError(w http.ResponseWriter, err error) {
	if se, ok := err.(HTTPStatusError); ok {
		writeStatus(w, int(se.HTTPStatus()), err)
		return
	}
	writeStatus(w, http.StatusInternalServerError, err)
}