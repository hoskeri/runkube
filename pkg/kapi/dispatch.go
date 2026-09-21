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
)

// ── Resource installation ──────────────────────────────────

func (r *Resource) install(g *Group, m *http.ServeMux) {
	base := path.Join("/apis", g.GroupVersion.Group, g.GroupVersion.Version)
	resourcePath := path.Join(base, r.Resource)

	if r.Namespaced {
		nsCollection := path.Join(base, "namespaces", "{namespace}", r.Resource)
		r.installCollection(m, nsCollection, true)
		itemPattern := nsCollection + "/{name}"
		r.installItem(m, itemPattern, true)
		r.installSubresources(m, itemPattern)
	} else {
		r.installCollection(m, resourcePath, false)
		itemPattern := resourcePath + "/{name}"
		r.installItem(m, itemPattern, false)
		r.installSubresources(m, itemPattern)
	}
}

func (r *Resource) installCollection(m *http.ServeMux, pattern string, namespaced bool) {
	// Always register GET (list/watch). Handles dispatch internally.
	m.HandleFunc("GET "+pattern, dispatchList(r, namespaced))

	// Always register POST (create). The dispatch function checks Creater.
	m.HandleFunc("POST "+pattern, dispatchCreate(r, namespaced))

	// Always register DELETE (delete collection).
	m.HandleFunc("DELETE "+pattern, dispatchDeleteCollection(r, namespaced))
}

func (r *Resource) installItem(m *http.ServeMux, pattern string, namespaced bool) {
	// Register all method patterns. Each dispatches to the operation if set,
	// falls through to RawHandler if set, or returns 405.
	m.HandleFunc("GET "+pattern, dispatchGet(r, namespaced))
	m.HandleFunc("PUT "+pattern, dispatchPut(r, namespaced))
	m.HandleFunc("PATCH "+pattern, dispatchPatch(r, namespaced))
	m.HandleFunc("DELETE "+pattern, dispatchDelete(r, namespaced))
	// POST on an item path is unusual but possible with RawHandler.
	m.HandleFunc("POST "+pattern, dispatchPostItem(r, namespaced))

	// Sub-paths /{rest...} → handled by RawHandler if set.
	if r.RawHandler != nil {
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
			m.HandleFunc(method+" "+pattern+"/{rest...}", rawDispatchHandler(r.RawHandler))
		}
	}
}

func (r *Resource) installSubresources(m *http.ServeMux, itemPattern string) {
	for _, sub := range r.Subresources {
		subPattern := itemPattern + "/" + sub.Name

		if sub.Getter != nil {
			m.HandleFunc("GET "+subPattern, dispatchSubGet(sub))
		}
		if sub.Creater != nil {
			m.HandleFunc("POST "+subPattern, dispatchSubCreate(sub))
		}
		if sub.Updater != nil {
			m.HandleFunc("PUT "+subPattern, dispatchSubUpdate(sub))
		}
		if sub.Patcher != nil {
			m.HandleFunc("PATCH "+subPattern, dispatchSubPatch(sub))
		}
		if sub.Deleter != nil {
			m.HandleFunc("DELETE "+subPattern, dispatchSubDelete(sub))
		}

		if sub.RawHandler != nil {
			handled := map[string]bool{}
			if sub.Getter != nil {
				handled["GET"] = true
			}
			if sub.Creater != nil {
				handled["POST"] = true
			}
			if sub.Updater != nil {
				handled["PUT"] = true
			}
			if sub.Patcher != nil {
				handled["PATCH"] = true
			}
			if sub.Deleter != nil {
				handled["DELETE"] = true
			}
			for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
				if !handled[method] {
					m.HandleFunc(method+" "+subPattern, rawDispatchHandler(sub.RawHandler))
				}
				m.HandleFunc(method+" "+subPattern+"/{rest...}", rawDispatchHandler(sub.RawHandler))
			}
		}
	}
}

// ── Dispatch functions ─────────────────────────────────────

func dispatchList(r *Resource, namespaced bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		_ = nsOrEmpty(req, namespaced)

		opts := metav1.ListOptions{}
		if u := req.URL.Query(); u.Has("watch") {
			metav1.Convert_url_Values_To_v1_ListOptions(&u, &opts, nil)
		}

		if opts.Watch {
			if r.Lister != nil {
				watcher, err := r.Lister.Watch(context.Background(), opts)
				if err != nil {
					writeError(w, err)
					return
				}
				if watcher != nil {
					serveWatch(w, req, watcher)
					return
				}
			}
			writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("watch"))
			return
		}

		if r.Lister != nil {
			list, err := r.Lister.List(context.Background(), opts)
			if err != nil {
				writeError(w, err)
				return
			}
			encode(w, list)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("list"))
	}
}

func dispatchCreate(r *Resource, namespaced bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		_ = nsOrEmpty(req, namespaced)

		if r.Creater == nil {
			if r.RawHandler != nil {
				r.RawHandler(context.Background(), w, req)
				return
			}
			writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("create"))
			return
		}

		obj, err := decodeBody(req)
		if err != nil {
			writeStatus(w, http.StatusBadRequest, err)
			return
		}
		result, err := r.Creater.Create(context.Background(), obj)
		if err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusCreated)
		encode(w, result)
	}
}

func dispatchGet(r *Resource, namespaced bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		_ = nsOrEmpty(req, namespaced)
		name := req.PathValue("name")

		if r.Getter != nil {
			result, err := r.Getter.Get(context.Background(), name)
			if err != nil {
				writeError(w, err)
				return
			}
			encode(w, result)
			return
		}
		if r.RawHandler != nil {
			r.RawHandler(context.Background(), w, req)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("get"))
	}
}

func dispatchPut(r *Resource, namespaced bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		_ = nsOrEmpty(req, namespaced)
		name := req.PathValue("name")

		if r.Updater != nil {
			obj, err := decodeBody(req)
			if err != nil {
				writeStatus(w, http.StatusBadRequest, err)
				return
			}
			result, err := r.Updater.Update(context.Background(), name, obj)
			if err != nil {
				writeError(w, err)
				return
			}
			encode(w, result)
			return
		}
		if r.RawHandler != nil {
			r.RawHandler(context.Background(), w, req)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("update"))
	}
}

func dispatchPatch(r *Resource, namespaced bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		_ = nsOrEmpty(req, namespaced)
		name := req.PathValue("name")

		if r.Patcher != nil {
			ct := req.Header.Get("Content-Type")
			data, err := readBody(req)
			if err != nil {
				writeStatus(w, http.StatusBadRequest, err)
				return
			}
			result, err := r.Patcher.Patch(context.Background(), name, parsePatchType(ct), data)
			if err != nil {
				writeError(w, err)
				return
			}
			encode(w, result)
			return
		}
		if r.RawHandler != nil {
			r.RawHandler(context.Background(), w, req)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("patch"))
	}
}

func dispatchDelete(r *Resource, namespaced bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		_ = nsOrEmpty(req, namespaced)
		name := req.PathValue("name")

		if r.Deleter != nil {
			if err := r.Deleter.Delete(context.Background(), name); err != nil {
				writeError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.RawHandler != nil {
			r.RawHandler(context.Background(), w, req)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("delete"))
	}
}

func dispatchPostItem(r *Resource, namespaced bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		_ = nsOrEmpty(req, namespaced)
		if r.RawHandler != nil {
			r.RawHandler(context.Background(), w, req)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("post"))
	}
}

func dispatchDeleteCollection(r *Resource, namespaced bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		_ = nsOrEmpty(req, namespaced)

		if r.CollectionDeleter != nil {
			opts := metav1.ListOptions{}
			if u := req.URL.Query(); len(u) > 0 {
				metav1.Convert_url_Values_To_v1_ListOptions(&u, &opts, nil)
			}
			if err := r.CollectionDeleter.DeleteCollection(context.Background(), opts); err != nil {
				writeError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if r.RawHandler != nil {
			r.RawHandler(context.Background(), w, req)
			return
		}
		writeStatus(w, http.StatusMethodNotAllowed, errMethodNotAllowed("deletecollection"))
	}
}

// ── Subresource dispatch ───────────────────────────────────

func dispatchSubGet(sub *Subresource) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		name := req.PathValue("name")
		result, err := sub.Getter.Get(context.Background(), name)
		if err != nil {
			writeError(w, err)
			return
		}
		encode(w, result)
	}
}

func dispatchSubCreate(sub *Subresource) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		_ = req.PathValue("name")
		obj, err := decodeBody(req)
		if err != nil {
			writeStatus(w, http.StatusBadRequest, err)
			return
		}
		result, err := sub.Creater.Create(context.Background(), obj)
		if err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusCreated)
		encode(w, result)
	}
}

func dispatchSubUpdate(sub *Subresource) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		name := req.PathValue("name")
		obj, err := decodeBody(req)
		if err != nil {
			writeStatus(w, http.StatusBadRequest, err)
			return
		}
		result, err := sub.Updater.Update(context.Background(), name, obj)
		if err != nil {
			writeError(w, err)
			return
		}
		encode(w, result)
	}
}

func dispatchSubPatch(sub *Subresource) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		name := req.PathValue("name")
		ct := req.Header.Get("Content-Type")

		data, err := readBody(req)
		if err != nil {
			writeStatus(w, http.StatusBadRequest, err)
			return
		}
		result, err := sub.Patcher.Patch(context.Background(), name, parsePatchType(ct), data)
		if err != nil {
			writeError(w, err)
			return
		}
		encode(w, result)
	}
}

func dispatchSubDelete(sub *Subresource) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		name := req.PathValue("name")
		if err := sub.Deleter.Delete(context.Background(), name); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ── RawHandler dispatch ────────────────────────────────────

func rawDispatchHandler(rh RawHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		rh(context.Background(), w, req)
	}
}

// ── Helpers ────────────────────────────────────────────────

func nsOrEmpty(req *http.Request, namespaced bool) string {
	if namespaced {
		return req.PathValue("namespace")
	}
	return ""
}

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
		return types.MergePatchType
	}
}