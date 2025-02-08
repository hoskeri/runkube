package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"strings"

	admv1 "k8s.io/api/admission/v1"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()
)

// Authorizer is a function that performs authorization.
type Authorizer func(context.Context, *authzv1.SubjectAccessReviewSpec) (*authzv1.SubjectAccessReviewStatus, error)

// Authenticator is a function that performs authentication.
type Authenticator func(context.Context, *authnv1.TokenReviewSpec) (*authnv1.TokenReviewStatus, error)

// Admitter is a function that performs admission control.
type Admitter func(context.Context, *admv1.AdmissionRequest) (*admv1.AdmissionResponse, error)

// Auditor is a function that performs auditing.
type Auditor func(context.Context, runtime.Object) error

func writeStatus(w http.ResponseWriter, statusCode int, err error) {
	w.WriteHeader(statusCode)
	encode(w, &metav1.Status{
		Message: err.Error(),
		Code:    int32(statusCode),
		Reason:  metav1.StatusReasonBadRequest,
	})
}

func decode(req *http.Request, into runtime.Object) error {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return err
	}
	_, _, err = deserializer.Decode(body, nil, into)
	return err
}

func encode(w http.ResponseWriter, from runtime.Object) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(from)
}

var (
	sarResultAllowed   = slog.StringValue("allowed")
	sarResultDenied    = slog.StringValue("denied")
	sarResultNoOpinion = slog.StringValue("noopinion")
	sarResultInvalid   = slog.StringValue("invalid")
)

func userInfoLogValue(a *authnv1.UserInfo) slog.Value {
	if p, ok := strings.CutPrefix(a.Username, "system:"); ok && p != "" {
		switch p {
		case "apiserver":
			if slices.Contains(a.Groups, "system:masters") {
				return slog.StringValue(a.Username)
			}
		case "kube-scheduler", "kube-controller-manager":
			return slog.StringValue(a.Username)
		}

		if n, ok := strings.CutPrefix(p, "node:"); ok && n != "" {
			if slices.Contains(a.Groups, "system:nodes") {
				return slog.StringValue(a.Username)
			}
		}

		if n, ok := strings.CutPrefix(p, "serviceaccount:"); ok && n != "" {
			if slices.Contains(a.Groups, "system:serviceaccounts") {
				return slog.StringValue(a.Username)
			}
		}
	}

	return slog.StringValue(a.String())
}

func sarLogValue(s *authzv1.SubjectAccessReviewSpec, st *authzv1.SubjectAccessReviewStatus) slog.Value {
	var rg, sg slog.Value

	switch {
	case s.ResourceAttributes != nil:
		r := s.ResourceAttributes
		rg = slog.Group("", "verb", r.Verb, "g", r.Group, "v", r.Version, "r", r.Resource, "ns", r.Namespace, "n", r.Name).Value
	case s.NonResourceAttributes != nil:
		r := s.NonResourceAttributes
		rg = slog.Group("", "verb", r.Verb, "path", r.Path).Value
	default:
		rg = slog.StringValue("unknown-resource").Resolve()
	}

	switch {
	case st.Denied && !st.Allowed:
		sg = sarResultDenied.Resolve()
	case st.Allowed && !st.Denied:
		sg = sarResultAllowed.Resolve()
	case !st.Allowed && !st.Denied:
		sg = sarResultNoOpinion.Resolve()
	default:
		sg = sarResultInvalid.Resolve()
	}

	return slog.Group("", "st", sg, "user", userInfoLogValue(&authnv1.UserInfo{
		Username: s.User,
		Groups:   s.Groups,
		UID:      s.UID,
	}), "res", rg).Value
}

func urlFor(g metav1.GroupVersionResource, namespace, name, subresource string) string {
	s := []string{"/"}
	switch g.Group {
	case "":
		s = append(s, "api", g.Version)
	default:
		s = append(s, "apis", g.Group, g.Version)
	}

	switch namespace {
	case "":
		s = append(s, g.Resource, name, subresource)
	default:
		s = append(s, "namespaces", namespace, g.Resource, name, subresource)
	}
	return path.Join(s...)
}

func optionsLogValue(raw []byte) string {
	o := map[string]string{}
	if err := json.Unmarshal(raw, &o); err != nil {
		return string(raw)
	}
	delete(o, "kind")
	delete(o, "apiVersion")
	if len(o) == 0 {
		return "{}"
	}

	x, _ := json.Marshal(o)
	return string(x)
}

func admResponseLogValue(a *admv1.AdmissionResponse) slog.Value {
	if a.Allowed {
		return slog.Group("",
			"patched", len(a.Patch) > 0,
		).Value
	} else {
		return slog.Group("",
			"denied", true,
		).Value
	}
}

func admLogValue(a *admv1.AdmissionRequest) slog.Value {
	return slog.Group("",
		"operation", a.Operation,
		"user", userInfoLogValue(&a.UserInfo),
		"url", urlFor(a.Resource, a.Namespace, a.Name, a.SubResource),
		"options", optionsLogValue(a.Options.Raw),
		"uid", a.UID,
	).Value
}

func (admitter Admitter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	admReq := &admv1.AdmissionReview{}
	if err := decode(req, admReq); err != nil {
		writeStatus(w, http.StatusBadRequest, err)
		return
	}

	lvl := slog.LevelInfo
	if admReq.Request.Resource.Group == "coordination.k8s.io" {
		lvl = slog.LevelDebug
	}

	admResponse, err := admitter(ctx, admReq.Request)
	if err != nil {
		slog.Log(req.Context(), lvl, "admission.error", "", admLogValue(admReq.Request), "", admResponseLogValue(admReq.Response))
		writeStatus(w, http.StatusInternalServerError, err)
		return
	}
	slog.Log(req.Context(), lvl, "admission", "", admLogValue(admReq.Request), "", admResponseLogValue(admResponse))

	admResponse.UID = admReq.Request.UID
	encode(w, &admv1.AdmissionReview{
		TypeMeta: admReq.TypeMeta,
		Response: admResponse,
	})
}

func (auditor Auditor) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	evl := &unstructured.UnstructuredList{}
	if err := decode(req, evl); err != nil {
		writeStatus(w, http.StatusBadRequest, err)
		return
	}

	for _, ev := range evl.Items {
		err := auditor(ctx, &ev)
		if err != nil {
			writeStatus(w, http.StatusBadRequest, err)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (authenticator Authenticator) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	tkr := &authnv1.TokenReview{}
	if err := decode(req, tkr); err != nil {
		writeStatus(w, http.StatusBadRequest, err)
		return
	}

	tkrStatus, err := authenticator(req.Context(), &tkr.Spec)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, err)
		return
	}
	encode(w, &authnv1.TokenReview{
		Status: *tkrStatus,
	})
}

func (authorizer Authorizer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	sarReq := &authzv1.SubjectAccessReview{}
	if err := decode(req, sarReq); err != nil {
		http.Error(w, "decoding request body", http.StatusBadRequest)
		return
	}
	sarStatus, err := authorizer(ctx, &sarReq.Spec)
	if err != nil {
		writeStatus(w, http.StatusInternalServerError, err)
		return
	}
	slog.Debug("authorize", "", sarLogValue(&sarReq.Spec, sarStatus))
	encode(w, &authzv1.SubjectAccessReview{
		Status: *sarStatus,
	})
}

// Attributes represents the attributes of an admission request.
type Attributes interface {
	UID() string
	UserInfo() authnv1.UserInfo
	Operation() admv1.Operation
	DryRun() bool
	Options() runtime.Object

	Kind() metav1.GroupVersionKind
	Resource() metav1.GroupVersionResource
	Subresource() string

	Namespace() string
	Name() string

	OldObject() runtime.Unstructured
	Object() runtime.Unstructured
}
