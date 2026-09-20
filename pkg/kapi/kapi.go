// Package kapi provides a lightweight framework for building Kubernetes
// extension API servers without the full k8s.io/apiserver dependency stack.
//
// It supplies:
//   - Standard Kubernetes API URL routing (/apis/{group}/{version}/...)
//   - Per-resource verb dispatch (list, get, create, update, delete, patch, watch)
//   - Sub-resources with independent handlers (status, scale, proxy, etc.)
//   - Discovery and APIService object generation
//   - Minimal OpenAPI v3 by default, enriched via per-resource hooks
//   - A RawHandler catch-all for verbs or paths not covered by specific handlers
//
// kapi does NOT provide persistence, admission webhooks, RBAC, or rich
// OpenAPI spec generation. Handlers are responsible for their own storage
// and business logic.
package kapi

import (
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"path"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ── Service ────────────────────────────────────────────────

// Service is an http.Handler that serves a set of API groups.
type Service struct {
	mux       *http.ServeMux
	apiGroups []*Group
}

// NewAPI constructs a Service from the provided groups.
func NewAPI(groups ...*Group) *Service {
	s := &Service{mux: http.NewServeMux(), apiGroups: groups}
	s.install()
	for _, g := range groups {
		g.install(s.mux)
	}
	return s
}

// ServeHTTP implements http.Handler by delegating to the internal mux.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// APIServices yields APIService objects (apiregistration.k8s.io/v1)
// for all registered groups, suitable for registering with the main
// Kubernetes API server.
func (s *Service) APIServices(caBundle []byte, namespace, name string) iter.Seq[runtime.Object] {
	return func(yield func(runtime.Object) bool) {
		for _, g := range s.apiGroups {
			if !yield(g.apiService(caBundle, namespace, name)) {
				return
			}
		}
	}
}

// install registers the global discovery endpoints and catch-all.
func (s *Service) install() {
	s.mux.HandleFunc("/{a...}", func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("kapi: no matching resource", "method", r.Method, "path", r.URL.Path)
		writeStatus(w, http.StatusNotFound, fmt.Errorf("%s not found", r.URL.Path))
	})

	s.mux.HandleFunc("GET /apis", func(w http.ResponseWriter, r *http.Request) {
		encode(w, &metav1.APIGroupList{})
	})

	// OpenAPI v3 root — lists per-group OpenAPI document URLs.
	type oaEntry struct {
		ServerRelativeURL string `json:"serverRelativeURL"`
	}

	ps := make(map[string]oaEntry)
	for _, g := range s.apiGroups {
		p := fmt.Sprintf("apis/%s/%s", g.GroupVersion.Group, g.GroupVersion.Version)
		ps[p] = oaEntry{
			ServerRelativeURL: path.Join("/openapi/v3", p),
		}
	}

	s.mux.HandleFunc("GET /openapi/{version}", func(w http.ResponseWriter, r *http.Request) {
		encode(w, map[string]any{"paths": ps})
	})
}

// ── Group ──────────────────────────────────────────────────

// Group collects resources under one group-version.
type Group struct {
	GroupVersion schema.GroupVersion
	Resources    []*Resource

	// OpenAPI, when non-nil, injects additional top-level entries
	// into the group's OpenAPI v3 document (paths, components, etc.).
	// The returned map is shallow-merged into the generated doc.
	OpenAPI func(gv schema.GroupVersion) map[string]any
}

// install registers group-level discovery and per-resource patterns.
func (g *Group) install(m *http.ServeMux) {
	// Group discovery — list resources under this group-version.
	m.HandleFunc("GET "+g.url("/apis/{group}/{version}"), func(w http.ResponseWriter, r *http.Request) {
		encode(w, &metav1.APIResourceList{
			GroupVersion: g.GroupVersion.String(),
			APIResources: slices.Collect(g.apiDiscovery()),
		})
	})

	// Per-group OpenAPI v3 document.
	m.HandleFunc("GET "+g.url("/openapi/v3/apis/{group}/{version}"), func(w http.ResponseWriter, r *http.Request) {
		doc := g.openAPIv3()
		encode(w, doc)
	})

	// Per-resource patterns.
	for _, res := range g.Resources {
		res.install(g, m)
	}
}

// apiDiscovery yields APIResource descriptors for all resources.
func (g *Group) apiDiscovery() iter.Seq[metav1.APIResource] {
	return func(yield func(metav1.APIResource) bool) {
		for _, r := range g.Resources {
			if !yield(r.descriptor()) {
				return
			}
		}
	}
}

// apiService builds an APIService unstructured object.
func (g *Group) apiService(caBundle []byte, namespace, name string) runtime.Object {
	spec := map[string]any{
		"caBundle":             encodeBase64(caBundle),
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

// url substitutes {group}/{version} placeholders with the group's values.
func (g *Group) url(tmpl string) string {
	return expandPath(tmpl, g.GroupVersion.Group, g.GroupVersion.Version, "")
}

// ── Response writing helpers ───────────────────────────────

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
		Status: metav1.StatusFailure,
		Code:   int32(code),
		Reason: reasonForCode(code),
		Message: msg,
	})
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

// encodeBase64 returns the standard-base64 encoding of data.
func encodeBase64(data []byte) string {
	const enc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	if len(data) == 0 {
		return ""
	}
	// Simple inline base64 — avoids importing encoding/base64 for this one call.
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