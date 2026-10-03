package runkube

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"

	"github.com/hoskeri/procman/pkg/process"
	"github.com/hoskeri/runkube/pkg/pki"
)

func (c *ControlPlane) selfBinary() string {
	e, err := os.Executable()
	if err != nil {
		slog.Error("os.Executable", "err", err)
		os.Exit(1)
	}

	return e
}

// Etcd returns a process description for the etcd server.
func (c *ControlPlane) Etcd() *process.Process {
	etcdName := fmt.Sprintf("%s-etcd", c.Name)
	return &process.Process{
		Tag: "etcd",
		CmdArgs: []string{
			"etcd",
			"--name", etcdName,
			"--log-level", "warn",
			"--data-dir", c.etcdDir(),

			"--listen-client-urls", c.unixSocketPath("etcd").String(),
			"--listen-peer-urls", c.unixSocketPath("etcd-peer").String(),
			"--listen-client-http-urls", c.unixSocketPath("etcd-http").String(),

			"--initial-cluster", fmt.Sprintf("%s=%s", etcdName, c.internalListenURL(443, "")),
			"--advertise-client-urls", c.internalListenURL(443, ""),
			"--initial-advertise-peer-urls", c.internalListenURL(443, ""),

			"--cert-file", c.pkiStore().Leaf("etcd-serving").CertPath(),
			"--key-file", c.pkiStore().Leaf("etcd-serving").KeyPath(),
			"--peer-cert-file", c.pkiStore().Leaf("etcd-serving").CertPath(),
			"--peer-key-file", c.pkiStore().Leaf("etcd-serving").KeyPath(),
			"--peer-client-cert-file", c.pkiStore().Leaf("etcd-peer-client").CertPath(),
			"--peer-client-key-file", c.pkiStore().Leaf("etcd-peer-client").KeyPath(),
			"--trusted-ca-file", c.pkiStore().CACertPath(pki.Etcd),
			"--peer-trusted-ca-file", c.pkiStore().CACertPath(pki.Etcd),
		},
	}
}

// APIServer returns a process description for the kube-apiserver.
func (c *ControlPlane) APIServer() *process.Process {
	return &process.Process{
		Tag: "api",
		CmdArgs: []string{
			"kube-apiserver",
			"-v", "0",
			"--profiling=false",
			"--feature-gates=AllAlpha=true,AllBeta=true",
			"--logging-format=json",

			"--bind-address", c.internalListenAddr().String(),
			"--secure-port=6443",

			"--tls-cert-file", c.pkiStore().Leaf("apiserver-serving").CertPath(),
			"--tls-private-key-file", c.pkiStore().Leaf("apiserver-serving").KeyPath(),

			"--proxy-client-cert-file", c.pkiStore().Leaf("apiserver-extension-client").CertPath(),
			"--proxy-client-key-file", c.pkiStore().Leaf("apiserver-extension-client").KeyPath(),

			"--external-hostname", c.externalHostname(),
			"--advertise-address", c.kubernetesServiceAddress(),

			"--etcd-servers", c.unixSocketPath("etcd").String(),
			"--etcd-cafile", c.pkiStore().CACertPath(pki.Etcd),
			"--etcd-certfile", c.pkiStore().Leaf("apiserver-etcd-client").CertPath(),
			"--etcd-keyfile", c.pkiStore().Leaf("apiserver-etcd-client").KeyPath(),
			"--encryption-provider-config", c.configRef("encryption-provider-config").Path,
			"--runtime-config", "api/all=true",

			"--service-cluster-ip-range", c.serviceAddressRange(),

			"--audit-policy-file", c.configRef("audit-policy").Path,
			"--audit-webhook-config-file", c.kubeConfigRef("audit-webhook").Path,
			"--audit-webhook-batch-throttle-enable=false",

			"--authorization-config", c.configRef("authorization-config").Path,

			"--enable-admission-plugins", "NodeRestriction,DenyServiceExternalIPs",
			"--admission-control-config-file", c.configRef("admission-config").Path,
			"--allow-privileged=false",

			"--authentication-config", c.configRef("authentication-config").Path,
			"--anonymous-auth=true",
			"--enable-bootstrap-token-auth=false",
			"--authentication-token-webhook-config-file", c.kubeConfigRef("authentication-token-webhook").Path,
			"--client-ca-file", c.pkiStore().CACertPath(pki.APIClient),
			"--requestheader-client-ca-file", c.pkiStore().CACertPath(pki.RequestHeader),
			"--requestheader-extra-headers-prefix=X-Remote-Extra-",
			"--requestheader-group-headers=X-Remote-Group",
			"--requestheader-username-headers=X-Remote-User",
			"--requestheader-uid-headers=X-Remote-UID",

			"--service-account-lookup=false",
			"--service-account-issuer", "https://" + c.externalHostname(),
			"--service-account-max-token-expiration=1h",
			"--service-account-extend-token-expiration=false",
			"--service-account-signing-endpoint", c.unixSocketURL("jwtsigner").Opaque,

			"--kubelet-certificate-authority", c.pkiStore().CACertPath(pki.KubeletServer),
			"--kubelet-client-certificate", c.pkiStore().Leaf("apiserver-kubelet-client").CertPath(),
			"--kubelet-client-key", c.pkiStore().Leaf("apiserver-kubelet-client").KeyPath(),
			"--kubelet-preferred-address-types", "Hostname",

			"--egress-selector-config-file", c.configRef("egress-selector-config").Path,
		},
	}
}

// Controller returns a process description for the kube-controller-manager.
func (c *ControlPlane) Controller() *process.Process {
	return &process.Process{
		Tag: "controller",
		CmdArgs: []string{
			"kube-controller-manager",

			"-v", "0",
			"--feature-gates=AllAlpha=true,AllBeta=true",
			"--profiling=false",
			"--logging-format=json",

			"--leader-elect=false",
			"--bind-address", c.internalListenAddr().String(),
			"--tls-cert-file", c.pkiStore().Leaf("controller-manager-server").CertPath(),
			"--tls-private-key-file", c.pkiStore().Leaf("controller-manager-server").KeyPath(),

			"--kubeconfig", c.kubeConfigRef("controller-manager-client").Path,
			"--authentication-kubeconfig", c.kubeConfigRef("controller-manager-authnz").Path,
			"--authorization-kubeconfig", c.kubeConfigRef("controller-manager-authnz").Path,
			"--authentication-tolerate-lookup-failure=true",

			"--use-service-account-credentials=true",
			"--controllers", "*,-serviceaccount-token-controller,-legacy-serviceaccount-token-cleaner-controller,-certificatesigningrequest-signing-controller",
		},
	}
}

// Scheduler returns a process description for the kube-scheduler.
func (c *ControlPlane) Scheduler() *process.Process {
	return &process.Process{
		Tag: "scheduler",
		CmdArgs: []string{
			"kube-scheduler",

			"-v", "0",
			"--feature-gates=AllAlpha=true,AllBeta=true",
			"--profiling=false",
			"--logging-format=json",

			"--bind-address", c.internalListenAddr().String(),
			"--tls-cert-file", c.pkiStore().Leaf("scheduler-server").CertPath(),
			"--tls-private-key-file", c.pkiStore().Leaf("scheduler-server").KeyPath(),

			"--leader-elect=false",
			"--kubeconfig", c.kubeConfigRef("scheduler-client").Path,

			"--authentication-kubeconfig", c.kubeConfigRef("scheduler-authnz").Path,
			"--authorization-kubeconfig", c.kubeConfigRef("scheduler-authnz").Path,
			"--authentication-tolerate-lookup-failure=true",
		},
	}
}

// childConfigFlags returns the flags a re-executed runkube subcommand needs to
// reconstruct the parent's configuration. Subcommands whose whole lifetime is
// one CLI invocation (kms, jwtsigner, webhook) re-enter config.Parse from
// os.Args, so without these they silently fall back to cluster name "default"
// and the default root.
func (c *ControlPlane) childConfigFlags() []string {
	return []string{"--name", c.Name, "--root", c.Root}
}

// Webhook returns a process description for the internal webhook.
func (c *ControlPlane) Webhook() *process.Process {
	return &process.Process{
		Tag: "webhook",
		CmdArgs: append([]string{
			c.selfBinary(),
			"webhook",
		}, c.childConfigFlags()...),
	}
}

// KMSPlugin returns a process description for the internal KMS plugin.
func (c *ControlPlane) KMSPlugin() *process.Process {
	return &process.Process{
		Tag: "kms",
		CmdArgs: append([]string{
			c.selfBinary(),
			"kms",
		}, c.childConfigFlags()...),
	}
}

// JWTSigner returns a process description for the internal JWT signer.
func (c *ControlPlane) JWTSigner() *process.Process {
	return &process.Process{
		Tag: "jwtsigner",
		CmdArgs: append([]string{
			c.selfBinary(),
			"jwtsigner",
		}, c.childConfigFlags()...),
	}
}

func makeSafeEnv(env []string) []string {
	return slices.DeleteFunc(env, func(s string) bool {
		k, _, _ := strings.Cut(s, "=")
		switch k {
		case "PATH", "TERM", "COLORTERM", "HOME", "SHELL", "PWD", "OLDPWD", "USER", "USERNAME", "LANG":
			return false
		default:
			return true
		}
	})
}

// KubectlExec executes kubectl with the system-admin kubeconfig.
func (c *ControlPlane) KubectlExec(env, args []string) error {
	finalEnv := append(makeSafeEnv(env),
		fmt.Sprintf("KUBECONFIG=%s", c.kubeConfigRef("system-admin").Path),
	)
	p, err := exec.LookPath("kubectl")
	if err != nil {
		return err
	}
	return syscall.Exec(p, args, finalEnv)
}

// CurlExec executes curl with the system-admin certificate.
func (c *ControlPlane) CurlExec(env, args []string) error {
	finalEnv := makeSafeEnv(env)
	p, err := exec.LookPath("curl")
	if err != nil {
		return err
	}
	a := []string{p,
		"--fail-with-body",
		"-sSLvo-",
		"--cacert", c.pkiStore().CACertPath(pki.APIServer),
		"--cert", c.pkiStore().Leaf("system-admin").CertPath(),
		"--key", c.pkiStore().Leaf("system-admin").KeyPath(),
	}
	fa := append(a, args...)
	return syscall.Exec(p, fa, finalEnv)
}

// EtcdCtlExec executes etcdctl with the appropriate certificates and endpoint.
func (c *ControlPlane) EtcdCtlExec(env, args []string) error {
	finalEnv := append(makeSafeEnv(env),
		fmt.Sprintf("ETCDCTL_ENDPOINTS=%s", c.unixSocketPath("etcd").String()),
		fmt.Sprintf("ETCDCTL_CACERT=%s", c.pkiStore().CACertPath(pki.Etcd)),
		fmt.Sprintf("ETCDCTL_CERT=%s", c.pkiStore().Leaf("apiserver-etcd-client").CertPath()),
		fmt.Sprintf("ETCDCTL_KEY=%s", c.pkiStore().Leaf("apiserver-etcd-client").KeyPath()),
	)
	p, err := exec.LookPath("etcdctl")
	if err != nil {
		return err
	}
	return syscall.Exec(p, args, finalEnv)
}
