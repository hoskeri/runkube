package runkube

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"slices"

	"github.com/hoskeri/runkube/pkg/frontend"
	"github.com/hoskeri/runkube/pkg/kapi"
	"github.com/hoskeri/runkube/pkg/webhook"
	admv1 "k8s.io/api/admission/v1"
	radmv1 "k8s.io/api/admissionregistration/v1"
	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func builtinAuthorizer() webhook.Authorizer {
	return func(ctx context.Context, spec *authzv1.SubjectAccessReviewSpec) (*authzv1.SubjectAccessReviewStatus, error) {
		var allowed bool
		var denied bool
		switch spec.User {
		case "system:apiserver":
			allowed = slices.Contains(spec.Groups, "system:masters")
		case "system:controller-manager-authnz", "system:scheduler-authnz":
			if spec.ResourceAttributes == nil {
				break
			}

			r := spec.ResourceAttributes
			var mismatched bool
			for _, m := range []bool{
				r.Verb == "get" || r.Verb == "watch" || r.Verb == "list",
				r.Name == "extension-apiserver-authentication",
				r.Resource == "configmaps",
				r.Group == "",
				r.Subresource == "",
			} {
				if !m {
					mismatched = true
				}
			}
			allowed = !mismatched
		}
		return &authzv1.SubjectAccessReviewStatus{
			Allowed: allowed,
			Denied:  denied,
			Reason:  "by-builtin",
		}, nil
	}
}

func builtinAuthenticator() webhook.Authenticator {
	return func(_ context.Context, spec *authnv1.TokenReviewSpec) (*authnv1.TokenReviewStatus, error) {
		return &authnv1.TokenReviewStatus{}, nil
	}
}

func builtinAuditor() webhook.Auditor {
	return func(_ context.Context, ev runtime.Object) error {
		content := ev.(*unstructured.Unstructured)
		slog.Debug("audit", "ev", content)
		return nil
	}
}

func builtinAdmitter(mutating bool) webhook.Admitter {
	return func(ctx context.Context, req *admv1.AdmissionRequest) (*admv1.AdmissionResponse, error) {
		allowed := true
		reason := metav1.Status{}
		var p []byte
		var pt *admv1.PatchType

		return &admv1.AdmissionResponse{
			UID:       req.UID,
			Allowed:   allowed,
			Result:    &reason,
			Patch:     p,
			PatchType: pt,
		}, nil
	}
}

func (c *ControlPlane) makeAdmissionWebhook() error {
	caBundle, err := os.ReadFile(c.caRef(WebhookServer).certPath)
	if err != nil {
		return err
	}

	rules := []radmv1.RuleWithOperations{
		{
			Operations: []radmv1.OperationType{radmv1.OperationAll},
			Rule: radmv1.Rule{
				APIGroups:   []string{"*"},
				APIVersions: []string{"*"},
				Resources:   []string{"*/*"},
				Scope:       new(radmv1.AllScopes),
			},
		},
	}

	r := &radmv1.ValidatingWebhookConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: radmv1.SchemeGroupVersion.String(),
			Kind:       "ValidatingWebhookConfiguration",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: c.hostname("validating-webhook") + ".static.k8s.io",
		},
		Webhooks: []radmv1.ValidatingWebhook{
			{
				Name: c.hostname("validating-webhook"),
				ClientConfig: radmv1.WebhookClientConfig{
					URL:      new(c.internalURL("validating-webhook").String()),
					CABundle: caBundle,
				},
				FailurePolicy:           new(radmv1.Fail),
				MatchPolicy:             new(radmv1.Equivalent),
				SideEffects:             new(radmv1.SideEffectClassNone),
				AdmissionReviewVersions: []string{"v1"},
				Rules:                   rules,
			},
		},
	}

	r2 := &radmv1.MutatingWebhookConfiguration{
		TypeMeta: metav1.TypeMeta{
			APIVersion: radmv1.SchemeGroupVersion.String(),
			Kind:       "MutatingWebhookConfiguration",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: c.hostname("mutating-webhook") + ".static.k8s.io",
		},
		Webhooks: []radmv1.MutatingWebhook{
			{
				Name: c.hostname("mutating-webhook"),
				ClientConfig: radmv1.WebhookClientConfig{
					URL:      new(c.internalURL("mutating-webhook").String()),
					CABundle: caBundle,
				},
				FailurePolicy:           new(radmv1.Fail),
				ReinvocationPolicy:      new(radmv1.IfNeededReinvocationPolicy),
				MatchPolicy:             new(radmv1.Equivalent),
				SideEffects:             new(radmv1.SideEffectClassNone),
				AdmissionReviewVersions: []string{"v1"},
				Rules:                   rules,
			},
		},
	}

	if err := c.configRef("validating/static.json").WriteObject(r); err != nil {
		return err
	}

	if err := c.configRef("mutating/static.json").WriteObject(r2); err != nil {
		return err
	}

	return nil
}

// RunWebhook starts the internal webhook server.
func (c *ControlPlane) RunWebhook(ctx context.Context) error {
	ff := frontend.New()
	for _, o := range []frontend.BackendServer{
		{
			ServerName: &url.URL{Scheme: "https", Host: "api-runkube-internal.kube-system.svc"},
			Handler:    kapi.NewAPI(defaultAPIGroups...),
		},
		{
			ServerName: c.internalURL("authentication-webhook"),
			Handler:    builtinAuthenticator(),
		},
		{
			ServerName: c.internalURL("authorization-webhook"),
			Handler:    builtinAuthorizer(),
		},
		{
			ServerName: c.internalURL("audit-webhook"),
			Handler:    builtinAuditor(),
		},
		{
			ServerName: c.internalURL("validating-webhook"),
			Handler:    builtinAdmitter(false),
		},
		{
			ServerName: c.internalURL("mutating-webhook"),
			Handler:    builtinAdmitter(true),
		},
	} {
		o.ServerCert = c.certRef("webhook-server").TLSCertificate()
		if err := ff.Register(o); err != nil {
			return err
		}
	}

	return ff.Run(ctx, c.unixSocketURL("frontend").Opaque)
}
