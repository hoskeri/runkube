package kapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"maps"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Service represents a Kubernetes API service.
type Service struct {
	mux       *http.ServeMux
	apiGroups []*Group
}

// APIServices returns a sequence of APIService objects for the registered API groups.
func (s *Service) APIServices(caBundle []byte, namespace, name string) iter.Seq[runtime.Object] {
	return func(yield func(runtime.Object) bool) {
		for _, g := range s.apiGroups {
			if !yield(g.apiService(caBundle, namespace, name)) {
				return
			}
		}
	}
}

func encode(w http.ResponseWriter, from runtime.Object) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(from)
}

func writeStatus(w http.ResponseWriter, statusCode int, err error) {
	m := ""
	if err != nil {
		m = err.Error()
	}
	w.WriteHeader(statusCode)
	encode(w, &metav1.Status{
		Message: m,
		Code:    int32(statusCode),
		Reason:  metav1.StatusReasonBadRequest,
		Status:  metav1.StatusFailure,
	})
}

// NewAPI creates a new Service with the provided API groups.
func NewAPI(groups ...*Group) *Service {
	s := &Service{mux: http.NewServeMux(), apiGroups: groups}
	s.install()
	for _, g := range groups {
		g.Install(s.mux)
	}
	return s
}

func (s *Service) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	s.mux.ServeHTTP(w, req)
}

type Group struct {
	APIVersion schema.GroupVersion
	Resources  []Resource
}

type Key struct {
	Namespace string
	UID       string
	Version   string
}

type ResourceSpec struct {
	schema.GroupVersionKind
	Namespaced bool
	Resource   string
	Verbs      []string
}

func (r *ResourceSpec) NewList() *unstructured.UnstructuredList {
	u := &unstructured.UnstructuredList{}
	u.SetGroupVersionKind(r.GroupVersionKind)
	return u
}

func (r *ResourceSpec) New() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(r.GroupVersionKind)
	return u
}

var defaultVerbs = metav1.Verbs{
	"get",
	"list",
	"watch",

	"create",
	"patch",
	"update",
	"delete",

	"proxy",
}

func (r *ResourceSpec) Descriptor() metav1.APIResource {
	v := defaultVerbs
	if len(r.Verbs) > 0 {
		v = r.Verbs
	}
	return metav1.APIResource{
		Name:         r.Resource,
		Group:        r.Group,
		Version:      r.Version,
		SingularName: r.Kind,
		Kind:         r.Kind,
		Namespaced:   r.Namespaced,
		Verbs:        v,
	}
}

type Resource interface {
	Descriptor() metav1.APIResource
	New() *unstructured.Unstructured
	NewList() *unstructured.UnstructuredList
}

func (g *Group) openAPIv3Paths() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		for _, kr := range g.Resources {
			d := kr.Descriptor()
			k := path.Join("/apis", g.APIVersion.Group, g.APIVersion.Version, d.Name)
			v := map[string]any{
				"get": map[string]any{
					"x-kubernetes-group-version-kind": map[string]any{
						"group":   d.Group,
						"version": d.Version,
						"kind":    d.Kind,
					},
				},
			}

			if !yield(k, v) {
				break
			}
		}
	}
}

func (g *Group) openAPIv3ComponentsSchemas() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		for _, kr := range g.Resources {
			d := kr.Descriptor()
			k := strings.Join([]string{g.APIVersion.Group, g.APIVersion.Version, d.Kind}, ".")
			v := map[string]any{
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

			if !yield(k, v) {
				break
			}
		}
	}
}

func (g *Group) apiDiscovery() iter.Seq[metav1.APIResource] {
	return func(yield func(metav1.APIResource) bool) {
		for _, kr := range g.Resources {
			if !yield(kr.Descriptor()) {
				break
			}
		}
	}
}

func (g *Group) url(u string) string {
	parts := strings.FieldsFunc(u, func(c rune) bool { return c == '/' })
	fp := []string{"/"}
	for _, p := range parts {
		switch p {
		case "{group}":
			fp = append(fp, g.APIVersion.Group)
		case "{version}":
			fp = append(fp, g.APIVersion.Version)
		default:
			fp = append(fp, p)
		}
	}

	return "GET " + path.Clean(path.Join(fp...))
}

func (g *Group) rurl(r Resource, u string) string {
	d := r.Descriptor()
	parts := strings.FieldsFunc(u, func(c rune) bool { return c == '/' })
	fp := []string{"/"}
	for _, p := range parts {
		switch p {
		case "{group}":
			fp = append(fp, g.APIVersion.Group)
		case "{version}":
			fp = append(fp, g.APIVersion.Version)
		case "{resource}":
			fp = append(fp, d.Name)
		default:
			fp = append(fp, p)
		}
	}

	return "GET " + path.Clean(path.Join(fp...))
}

func (g *Group) apiService(caBundle []byte, namespace, name string) runtime.Object {
	spec := map[string]any{
		"caBundle":             base64.StdEncoding.EncodeToString(caBundle),
		"group":                g.APIVersion.Group,
		"version":              g.APIVersion.Version,
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
	u.SetName(strings.Join([]string{g.APIVersion.Version, g.APIVersion.Group}, "."))
	unstructured.SetNestedMap(u.Object, spec, "spec")

	return u
}

func (s *Service) install() {
	s.mux.HandleFunc("/{a...}", func(w http.ResponseWriter, req *http.Request) {
		slog.Info("apiservice not found", "method", req.Method, "req.header", req.Header)
		writeStatus(w, http.StatusNotFound, fmt.Errorf("%s not found", req.RequestURI))
	})

	s.mux.HandleFunc("/apis", func(w http.ResponseWriter, req *http.Request) {
		encode(w, &metav1.APIGroupList{})
	})

	type e struct {
		ServerRelativeURL string `json:"serverRelativeURL"`
	}
	type o struct {
		Paths map[string]e `json:"paths"`
	}

	ps := map[string]e{}
	for _, g := range s.apiGroups {
		ps[fmt.Sprintf("apis/%s/%s", g.APIVersion.Group, g.APIVersion.Version)] = e{
			ServerRelativeURL: path.Join("/openapi/v3/apis", g.APIVersion.Group, g.APIVersion.Version),
		}
	}

	s.mux.HandleFunc("GET /openapi/{version}", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(o{
			Paths: ps,
		})
	})
}

func (g *Group) Install(m *http.ServeMux) {
	m.HandleFunc(g.url("/openapi/v3/apis/{group}/{version}"), func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"paths": maps.Collect(g.openAPIv3Paths()),
			"components": map[string]any{
				"schemas": maps.Collect(g.openAPIv3ComponentsSchemas()),
			},
		})
	})

	m.HandleFunc(g.url("/apis/{group}/{version}"), func(w http.ResponseWriter, req *http.Request) {
		o := &metav1.APIResourceList{
			GroupVersion: g.APIVersion.String(),
			APIResources: slices.Collect(g.apiDiscovery()),
		}
		encode(w, o)
	})

	for _, r := range g.Resources {
		m.HandleFunc(g.rurl(r, "/apis/{group}/{version}/{resource}"), func(w http.ResponseWriter, req *http.Request) {
			op := metav1.ListOptions{}
			uv := req.URL.Query()
			metav1.Convert_url_Values_To_v1_ListOptions(&uv, &op, nil)

			if op.Watch {
				w.Header().Add("Content-Type", "application/json")
				d := int64(600)
				if op.TimeoutSeconds != nil && *op.TimeoutSeconds > 0 {
					d = *op.TimeoutSeconds
				}
				time.Sleep(time.Duration(d) * time.Second)
				return
			}

			o := r.NewList()
			encode(w, o)
		})

		if r.Descriptor().Namespaced {
			m.HandleFunc(g.rurl(r, "/apis/{group}/{version}/namespaces/{namespace}/{resource}"), func(w http.ResponseWriter, req *http.Request) {
				op := metav1.ListOptions{}
				uv := req.URL.Query()
				metav1.Convert_url_Values_To_v1_ListOptions(&uv, &op, nil)

				if op.Watch {
					w.Header().Add("Content-Type", "application/json")
					d := int64(600)
					if op.TimeoutSeconds != nil && *op.TimeoutSeconds > 0 {
						d = *op.TimeoutSeconds
					}
					time.Sleep(time.Duration(d) * time.Second)
					return
				}

				o := r.NewList()
				encode(w, o)
			})

			m.HandleFunc(g.rurl(r, "/apis/{group}/{version}/namespaces/{namespace}/{resources}/{name}"), func(w http.ResponseWriter, req *http.Request) {
				op := metav1.GetOptions{}
				uv := req.URL.Query()
				metav1.Convert_url_Values_To_v1_GetOptions(&uv, &op, nil)

				o := r.New()
				o.SetName(req.PathValue("name"))
				o.SetNamespace(req.PathValue("namespace"))
				o.SetCreationTimestamp(metav1.Now())

				encode(w, o)
			})
		} else {
			m.HandleFunc(g.rurl(r, "/apis/{group}/{version}/{resource}/{name}"), func(w http.ResponseWriter, req *http.Request) {
				op := metav1.GetOptions{}
				uv := req.URL.Query()
				metav1.Convert_url_Values_To_v1_GetOptions(&uv, &op, nil)

				o := r.New()
				o.SetName(req.PathValue("name"))
				o.SetCreationTimestamp(metav1.Now())

				encode(w, o)
			})
		}
	}
}
