package kapi

import (
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// ── Service ────────────────────────────────────────────────

type Service struct {
	mux       *http.ServeMux
	apiGroups []*Group
}

func NewAPI(groups ...*Group) *Service {
	s := &Service{mux: http.NewServeMux(), apiGroups: groups}
	s.installGlobal()
	for _, g := range groups {
		g.install(s.mux)
	}
	return s
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Service) APIServices(caBundle []byte, namespace, name string) iter.Seq[runtime.Object] {
	return func(yield func(runtime.Object) bool) {
		for _, g := range s.apiGroups {
			if !yield(g.apiService(caBundle, namespace, name)) {
				return
			}
		}
	}
}

func (s *Service) installGlobal() {
	s.mux.HandleFunc("/{a...}", func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("kapi: no matching resource", "method", r.Method, "path", r.URL.Path)
		writeStatus(w, http.StatusNotFound, fmt.Errorf("%s not found", r.URL.Path))
	})

	s.mux.HandleFunc("GET /apis", func(w http.ResponseWriter, r *http.Request) {
		encode(w, &metav1.APIGroupList{})
	})

	// OpenAPI v3 root — lists per-group document URLs.
	type oaEntry struct {
		ServerRelativeURL string `json:"serverRelativeURL"`
	}
	ps := make(map[string]oaEntry)
	for _, g := range s.apiGroups {
		p := fmt.Sprintf("apis/%s/%s", g.GroupVersion.Group, g.GroupVersion.Version)
		ps[p] = oaEntry{ServerRelativeURL: path.Join("/openapi/v3", p)}
	}

	s.mux.HandleFunc("GET /openapi/{version}", func(w http.ResponseWriter, r *http.Request) {
		encode(w, map[string]any{"paths": ps})
	})
}

// ── Group ──────────────────────────────────────────────────

func (g *Group) install(m *http.ServeMux) {
	base := path.Join("/apis", g.GroupVersion.Group, g.GroupVersion.Version)

	// Group discovery.
	m.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		encode(w, &metav1.APIResourceList{
			GroupVersion: g.GroupVersion.String(),
			APIResources: slices.Collect(g.apiDiscovery()),
		})
	})

	// Per-group OpenAPI v3 document.
	m.HandleFunc("GET "+path.Join("/openapi/v3/apis", g.GroupVersion.Group, g.GroupVersion.Version),
		func(w http.ResponseWriter, r *http.Request) {
			encode(w, g.openAPIv3())
		})

	// Per-resource patterns.
	for _, res := range g.Resources {
		res.install(g, m)
	}
}

func (g *Group) apiDiscovery() iter.Seq[metav1.APIResource] {
	return func(yield func(metav1.APIResource) bool) {
		for _, r := range g.Resources {
			if !yield(r.descriptor()) {
				return
			}
		}
	}
}

func (g *Group) apiService(caBundle []byte, namespace, name string) runtime.Object {
	spec := map[string]any{
		"caBundle":             b64(caBundle),
		"group":                g.GroupVersion.Group,
		"version":              g.GroupVersion.Version,
		"groupPriorityMinimum": int64(1000),
		"versionPriority":      int64(15),
		"service": map[string]any{
			"name":      name,
			"namespace": namespace,
			"port":      int64(443),
		},
	}
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("apiregistration.k8s.io/v1")
	u.SetKind("APIService")
	u.SetName(g.GroupVersion.Version + "." + g.GroupVersion.Group)
	unstructured.SetNestedMap(u.Object, spec, "spec")
	return u
}

// ── OpenAPI v3 ─────────────────────────────────────────────

func (g *Group) openAPIv3() map[string]any {
	paths := make(map[string]any)
	schemas := make(map[string]any)

	for _, r := range g.Resources {
		d := r.descriptor()
		basePath := path.Join("/apis", g.GroupVersion.Group, g.GroupVersion.Version, d.Name)

		op := map[string]any{
			"get": map[string]any{
				"x-kubernetes-group-version-kind": map[string]any{
					"group":   d.Group,
					"version": d.Version,
					"kind":    d.Kind,
				},
			},
		}
		if r.OpenAPIOperation != nil {
			if enriched := r.OpenAPIOperation("list"); enriched != nil {
				mergeMap(op["get"].(map[string]any), enriched)
			}
		}
		paths[basePath] = op

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
		"paths":      paths,
		"components": map[string]any{"schemas": schemas},
	}
	if g.OpenAPI != nil {
		if injected := g.OpenAPI(g.GroupVersion); injected != nil {
			mergeMap(doc, injected)
		}
	}
	return doc
}

// ── descriptor ─────────────────────────────────────────────

var defaultVerbs = metav1.Verbs{"get", "list", "watch", "create", "update", "patch", "delete", "proxy"}

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

// ── Response helpers ───────────────────────────────────────

func encode(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeStatus(w http.ResponseWriter, code int, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	w.WriteHeader(code)
	encode(w, &metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    int32(code),
		Reason:  reasonForCode(code),
		Message: msg,
	})
}

func writeError(w http.ResponseWriter, err error) {
	if se, ok := err.(HTTPStatusError); ok {
		writeStatus(w, int(se.HTTPStatus()), err)
		return
	}
	writeStatus(w, http.StatusInternalServerError, err)
}

func reasonForCode(code int) metav1.StatusReason {
	switch code {
	case 400:
		return metav1.StatusReasonBadRequest
	case 401:
		return metav1.StatusReasonUnauthorized
	case 403:
		return metav1.StatusReasonForbidden
	case 404:
		return metav1.StatusReasonNotFound
	case 405:
		return metav1.StatusReasonMethodNotAllowed
	case 409:
		return metav1.StatusReasonConflict
	case 410:
		return metav1.StatusReasonGone
	case 422:
		return metav1.StatusReasonInvalid
	case 429:
		return metav1.StatusReasonTooManyRequests
	case 500:
		return metav1.StatusReasonInternalError
	case 503:
		return metav1.StatusReasonServiceUnavailable
	case 504:
		return metav1.StatusReasonTimeout
	default:
		return metav1.StatusReasonUnknown
	}
}

func errMethodNotAllowed(verb string) error {
	return &statusError{code: http.StatusMethodNotAllowed, message: fmt.Sprintf("%s is not supported", verb)}
}

// b64 returns the standard-base64 encoding of data.
func b64(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	const enc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	n := (len(data) + 2) / 3 * 4
	buf := make([]byte, n)
	j := 0
	for i := 0; i < len(data); i += 3 {
		var val uint32
		val |= uint32(data[i]) << 16
		if i+1 < len(data) {
			val |= uint32(data[i+1]) << 8
		}
		if i+2 < len(data) {
			val |= uint32(data[i+2])
		}
		buf[j] = enc[(val>>18)&0x3F]
		buf[j+1] = enc[(val>>12)&0x3F]
		if i+1 < len(data) {
			buf[j+2] = enc[(val>>6)&0x3F]
		} else {
			buf[j+2] = '='
		}
		if i+2 < len(data) {
			buf[j+3] = enc[val&0x3F]
		} else {
			buf[j+3] = '='
		}
		j += 4
	}
	return string(buf)
}

func mergeMap(dst, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}