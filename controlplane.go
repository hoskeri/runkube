package runkube

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hoskeri/procman/pkg/process"
	"github.com/hoskeri/procman/pkg/termhandler"
	"github.com/hoskeri/runkube/pkg/config"
	"github.com/hoskeri/runkube/pkg/kapi"
	"github.com/hoskeri/runkube/pkg/nodes"
	"github.com/hoskeri/runkube/pkg/ref"
	"github.com/hoskeri/runkube/pkg/syncer"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// ControlPlane represents the Kubernetes control plane.
type ControlPlane struct {
	config.Config

	caMap        map[CAName]*CA
	syncer       *syncer.Syncer
	unixListener bool
}

// Run starts the control plane components.
func (c *ControlPlane) Run(ctx context.Context) error {
	fm := &process.Formation{
		Sink: slog.New(termhandler.New(os.Stdout, &termhandler.Options{
			Level: slog.LevelInfo,
		})),
		Processes: slices.Concat([]*process.Process{
			c.Etcd(),
			c.APIServer(),
			c.Controller(),
			c.Scheduler(),
			c.Webhook(),
			c.KMSPlugin(),
			c.JWTSigner(),
		}, c.nodeProcesses()),
		Workdir: c.Root,
	}

	return fm.Run(ctx)
}

func (c *ControlPlane) hostname(tag string) string {
	return fmt.Sprintf("%s-%s.internal.runkube", c.Name, tag)
}

func (c *ControlPlane) internalURL(tag string) *url.URL {
	return &url.URL{
		Scheme: "https",
		Host:   c.hostname(tag),
	}
}

func (c *ControlPlane) externalHostname() string {
	return c.ExternalHostname
}

func (c *ControlPlane) internalListenAddr() netip.Addr {
	return netip.MustParseAddr("::").Next()
}

func (c *ControlPlane) unixSocketURL(tag string) *url.URL {
	return &url.URL{
		Scheme: "unixs",
		Opaque: fmt.Sprintf("@%s-%s", c.Name, tag),
	}
}

func (c *ControlPlane) unixSocketPath(tag string) *url.URL {
	return &url.URL{
		Scheme: "unixs",
		Path:   filepath.Join(c.Root, fmt.Sprintf("run/%s/%s-%s.sock", tag, c.Name, tag)),
	}
}

func (c *ControlPlane) apiserverURL() string {
	if c.unixListener {
		return c.unixSocketURL("apiserver").String()
	}

	return c.internalListenURL(6443, "")
}

func (c *ControlPlane) internalListenURL(port uint16, path string) string {
	return (&url.URL{
		Scheme: "https",
		Host:   netip.AddrPortFrom(c.internalListenAddr(), port).String(),
		Path:   path,
	}).String()
}

func (c *ControlPlane) caRef(caName CAName) *pkiRef {
	return &pkiRef{
		certPath: filepath.Join(c.Root, "etc/kubernetes/pki", "ca", string(caName)+"-ca.crt"),
		keyPath:  filepath.Join(c.Root, "etc/kubernetes/pki", "ca", string(caName)+"-ca.key"),
	}
}

func (c *ControlPlane) certRef(certName string) *pkiRef {
	return &pkiRef{
		certPath: filepath.Join(c.Root, "etc/kubernetes/pki", "leaf", certName+".crt"),
		keyPath:  filepath.Join(c.Root, "etc/kubernetes/pki", "leaf", certName+".key"),
	}
}

func (c *ControlPlane) keySetRef(name string) *jwksRef {
	return &jwksRef{
		path: filepath.Join(c.Root, "etc/kubernetes/keys", name+".jwks"),
	}
}

func (c *ControlPlane) configRef(s string) *ref.Config {
	return &ref.Config{
		Path: filepath.Join(c.Root, "etc/kubernetes/confgen", s),
	}
}

func (c *ControlPlane) kubeConfigRef(s string) *ref.KubeConfig {
	return &ref.KubeConfig{
		Path: filepath.Join(c.Root, "etc/kubernetes/kubeconfig", s+".kubeconfig"),
	}
}

func (c *ControlPlane) etcdDir() string {
	return filepath.Join(c.Root, "var/lib/etcd", c.Name)
}

func (c *ControlPlane) kubernetesServiceAddress() string {
	return c.ServiceAddressRange.Addr().Next().String()
}

func (c *ControlPlane) serviceAddressRange() string {
	return c.ServiceAddressRange.String()
}

// Init initializes the control plane by generating certificates and configuration files.
func (c *ControlPlane) Init(ctx context.Context) error {
	c.syncer, _ = syncer.NewSyncer()
	reqs := []*Request{
		{
			name:   "etcd-serving",
			signer: Etcd,
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
			cn:     c.hostname("etcd-serving"),
			san:    []string{path.Base(c.unixSocketPath("etcd").Path)},
		},
		{
			name:   "etcd-peer-client",
			signer: Etcd,
			cn:     c.hostname("etcd-peer-client"),
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		},
		{
			name:   "apiserver-etcd-client",
			signer: Etcd,
			cn:     c.hostname("etcd-client"),
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		},
		{
			name:   "webhook-server",
			signer: WebhookServer,
			cn:     c.hostname("webhook"),
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			ip:     []netip.Addr{c.internalListenAddr()},
			san: []string{
				"api-runkube-internal.kube-system.svc",
				c.hostname("authentication-webhook"),
				c.hostname("authorization-webhook"),
				c.hostname("mutating-webhook"),
				c.hostname("validating-webhook"),
				c.hostname("audit-webhook"),
			},
		},
		{
			name:   "apiserver-serving",
			signer: APIServer,
			cn:     c.hostname("apiserver"),
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			san:    []string{c.hostname("apiserver"), c.externalHostname()},
			ip:     []netip.Addr{c.internalListenAddr()},
		},
		{
			name:   "apiserver-kubelet-client",
			signer: APIClient,
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			cn:     "system:kubelet-apiserver-client",
		},
		{
			name:   "apiserver-extension-client",
			signer: ExtensionAPIServer,
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			cn:     "system:apiserver-extension-client",
		},
		{
			name:   "requestheader-client",
			signer: RequestHeader,
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			cn:     "system:requestheader-client",
		},
		{
			name:   "kubelet-server-unused",
			signer: KubeletServer,
			cn:     "kubelet-server-unused",
		},
		{
			name:   "system-admin",
			signer: APIClient,
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			cn:     "system:system-admin",
			o:      []string{"system:masters"},
		},
		{
			name:   "controller-manager-client",
			signer: APIClient,
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			cn:     "system:kube-controller-manager",
		},
		{
			name:   "controller-manager-authnz",
			signer: APIClient,
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			cn:     "system:controller-manager-authnz",
		},
		{
			name:   "controller-manager-server",
			signer: APIClient,
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			cn:     "system:kube-controller-manager",
			san:    []string{c.hostname("controller-manager-server")},
			ip:     []netip.Addr{c.internalListenAddr()},
		},
		{
			name:   "scheduler-client",
			signer: APIClient,
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			cn:     "system:kube-scheduler",
		},
		{
			name:   "scheduler-server",
			signer: APIClient,
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			cn:     "system:kube-scheduler",
			san:    []string{c.hostname("scheduler-server")},
			ip:     []netip.Addr{c.internalListenAddr()},
		},
		{
			name:   "scheduler-authnz",
			signer: APIClient,
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			cn:     "system:scheduler-authnz",
		},
	}

	for _, r := range reqs {
		if err := c.CA(r.signer).Certificate(c.certRef(r.name), r); err != nil {
			return err
		}

		if r.signer == APIClient {
			if err := c.makeAPIKubeconfig(r.name, c.certRef(r.name)); err != nil {
				return err
			}
		}
	}

	for _, err := range []error{
		c.makeExtensionKubeconfig("authentication-token-webhook", c.internalURL("authentication-webhook")),
		c.makeExtensionKubeconfig("authorization-webhook", c.internalURL("authorization-webhook")),
		c.makeExtensionKubeconfig("audit-webhook", c.internalURL("audit-webhook")),
		c.makeEgressSelectorConfig(),
		c.makeAuthenticationConfig(),
		c.makeAuthorizationConfig(),
		c.makeAdmissionConfig(),
		c.makeAuditPolicy(),
		c.makeAdmissionWebhook(),
		c.makeEncryptionProviderConfig(),
		c.makeAPIService(defaultAPIGroups...),
		c.keySetRef("encryption").Write("default", "encryption"),
		c.keySetRef("serviceaccount").Write("default", "signing"),
		os.MkdirAll(path.Dir(c.unixSocketPath("etcd").Path), 0700),
		os.MkdirAll(path.Dir(c.unixSocketPath("etcd-peer").Path), 0700),
		os.MkdirAll(path.Dir(c.unixSocketPath("etcd-http").Path), 0700),
	} {
		if err != nil {
			return err
		}
	}

	for _, n := range c.Nodes {
		if err := c.setupNode(n); err != nil {
			return err
		}
	}

	return c.configRef("syncer.objects").Write(c.syncer.AsYAML())
}

func (c *ControlPlane) setupNode(nodeName string) error {
	r := &Request{
		name:   nodeName,
		signer: APIClient,
		usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		cn:     fmt.Sprintf("system:node:%s", nodeName),
		o:      []string{"system:nodes"},
	}

	if err := c.CA(r.signer).Certificate(c.certRef(nodeName), r); err != nil {
		return err
	}

	if err := c.makeNodeKubeconfig(nodeName, c.certRef(nodeName)); err != nil {
		return err
	}

	return nil
}

func (c *ControlPlane) nodeProcesses() []*process.Process {
	ps := []*process.Process{}
	for _, n := range c.Nodes {
		ps = append(ps, &process.Process{
			Tag:     n,
			CmdArgs: []string{c.selfBinary(), "node", n},
		})
	}

	return ps
}

// RunNode runs a Kubernetes node.
func (c *ControlPlane) RunNode(ctx context.Context, nodeName string) error {
	kp, err := exec.LookPath("kubelet")
	if err != nil {
		panic(err)
	}
	h := crypto.SHA224.New()
	h.Write([]byte(nodeName))
	node := &nodes.Node{
		Name:          nodeName,
		Kubeconfig:    c.kubeConfigRef(nodeName).Path,
		KubeletBinary: kp,
		MachineID:     hex.EncodeToString(h.Sum(nil)[0:16]),
		Address:       "10.0.0.1",
	}

	return node.RunSandbox(ctx)
}

func (c *ControlPlane) makeExtensionKubeconfig(name string, srv *url.URL) error {
	kc := fmt.Sprintf(`
apiVersion: v1
kind: Config
current-context: default
clusters:
- name: default
  cluster:
    server: %q
    certificate-authority-data: %q
contexts:
- name: default
  context:
    cluster: default
    user: default
users:
- name: default
  user:
    client-certificate-data: %q
    client-key-data: %q
`,
		srv.String(),
		c.caRef(WebhookServer).CertBase64String(),
		c.certRef("apiserver-extension-client").CertBase64String(),
		c.certRef("apiserver-extension-client").KeyBase64String(),
	)

	return c.kubeConfigRef(name).Write([]byte(kc))
}

func (c *ControlPlane) makeAPIKubeconfig(name string, p *pkiRef) error {
	kc := fmt.Sprintf(`
apiVersion: v1
kind: Config
current-context: default
clusters:
- name: default
  cluster:
    server: %q
    tls-server-name: %q
    certificate-authority-data: %q
contexts:
- name: default
  context:
    cluster: default
    user: default
users:
- name: default
  user:
    client-certificate-data: %q
    client-key-data: %q
`,
		c.apiserverURL(),
		c.externalHostname(),
		c.caRef(APIServer).CertBase64String(),
		p.CertBase64String(),
		p.KeyBase64String(),
	)
	return c.kubeConfigRef(name).Write([]byte(kc))
}

func (c *ControlPlane) makeNodeKubeconfig(nodeName string, p *pkiRef) error {
	kc := fmt.Sprintf(`
apiVersion: v1
kind: Config
current-context: default
clusters:
- name: default
  cluster:
    server: %q
    certificate-authority-data: %q
contexts:
- name: default
  context:
    cluster: default
    user: default
users:
- name: default
  user:
    client-certificate-data: %q
    client-key-data: %q
`,
		(&url.URL{Scheme: "https", Host: c.hostname("apiserver")}).String(),
		c.caRef(APIServer).CertBase64String(),
		p.CertBase64String(),
		p.KeyBase64String(),
	)
	return c.kubeConfigRef(nodeName).Write([]byte(kc))
}

func (c *ControlPlane) makeEgressSelectorConfig() error {
	kc := fmt.Sprintf(`
apiVersion: apiserver.k8s.io/v1beta1
kind: EgressSelectorConfiguration
egressSelections:
- name: cluster
  connection:
    proxyProtocol: HTTPConnect
		transport:
			uds:
				udsName: %q
- name: controlplane
  connection:
    proxyProtocol: HTTPConnect
		transport:
			uds:
				udsName: %q
`,
		c.unixSocketURL("frontend").Opaque,
		c.unixSocketURL("frontend").Opaque,
	)
	return c.configRef("egress-selector-config").Write([]byte(kc))
}

func (c *ControlPlane) makeAuthorizationConfig() error {
	kc := fmt.Sprintf(`
kind: AuthorizationConfiguration
apiVersion: apiserver.config.k8s.io/v1beta1
authorizers:
- name: authorizer-internal.runkube
  type: Webhook
  webhook:
    failurePolicy: Deny
    timeout: 3s
    subjectAccessReviewVersion: v1
    matchConditionSubjectAccessReviewVersion: v1
    connectionInfo:
      type: KubeConfigFile
      kubeConfigFile: %q
- name: node
  type: Node
- name: rbac
  type: RBAC
`, c.kubeConfigRef("authorization-webhook").Path)
	return c.configRef("authorization-config").Write([]byte(kc))
}

func (c *ControlPlane) makeAuthenticationConfig() error {
	kc := `
apiVersion: apiserver.config.k8s.io/v1beta1
kind: AuthenticationConfiguration
`
	return c.configRef("authentication-config").Write([]byte(kc))
}

func (c *ControlPlane) makeAdmissionConfig() error {
	kc := fmt.Sprintf(`
apiVersion: apiserver.config.k8s.io/v1
kind: AdmissionConfiguration
plugins:
- name: ValidatingAdmissionWebhook
  configuration:
		apiVersion: apiserver.config.k8s.io/v1
		kind: WebhookAdmissionConfiguration
		staticManifestsDir: %q
- name: MutatingAdmissionWebhook
	configuration:
		apiVersion: apiserver.config.k8s.io/v1
		kind: WebhookAdmissionConfiguration
		staticManifestsDir: %q
`,
		filepath.Join(c.Root, "/etc/kubernetes/confgen/validating"),
		filepath.Join(c.Root, "/etc/kubernetes/confgen/mutating"),
	)
	return c.configRef("admission-config").Write([]byte(kc))
}

func (c *ControlPlane) makeEncryptionProviderConfig() error {
	kc := fmt.Sprintf(`
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
- resources:
  - '*.*'
  providers:
  - kms:
      name: kms
      apiVersion: v2
			endpoint: unix:/%s
  - identity: {}
`, c.unixSocketURL("kms").Opaque)
	return c.configRef("encryption-provider-config").Write([]byte(kc))
}

func (c *ControlPlane) makeAuditPolicy() error {
	kc := `
apiVersion: audit.k8s.io/v1
kind: Policy
omitManagedFields: true
omitStages:
  - RequestReceived
  - ResponseStarted
rules:
  - level: RequestResponse
`
	return c.configRef("audit-policy").Write([]byte(kc))
}

func (c *ControlPlane) makeAPIService(g ...*kapi.Group) error {
	namespace, name := "kube-system", "api-runkube-internal"
	caBundle, err := os.ReadFile(c.caRef(WebhookServer).certPath)
	if err != nil {
		return err
	}

	ksvc := &unstructured.Unstructured{}
	ksvc.SetKind("Service")
	ksvc.SetAPIVersion("v1")
	ksvc.SetName(name)
	ksvc.SetNamespace(namespace)
	spec := map[string]any{
		"type":         "ExternalName",
		"externalName": strings.Join([]string{name, namespace, "svc"}, "."),
		"ports": []any{
			map[string]any{
				"name":       "api",
				"protocol":   "TCP",
				"port":       int64(443),
				"targetPort": int64(443),
			},
		},
	}
	unstructured.SetNestedMap(ksvc.Object, spec, "spec")
	objs := []runtime.Object{ksvc}

	api := kapi.NewAPI(g...)
	objs = slices.AppendSeq(objs, api.APIServices(caBundle, namespace, name))

	return c.syncer.Add(objs...)
}
