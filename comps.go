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

			"--cert-file", c.certRef("etcd-serving").certPath,
			"--key-file", c.certRef("etcd-serving").keyPath,
			"--peer-cert-file", c.certRef("etcd-serving").certPath,
			"--peer-key-file", c.certRef("etcd-serving").keyPath,
			"--peer-client-cert-file", c.certRef("etcd-peer-client").certPath,
			"--peer-client-key-file", c.certRef("etcd-peer-client").keyPath,
			"--trusted-ca-file", c.caRef(Etcd).certPath,
			"--peer-trusted-ca-file", c.caRef(Etcd).certPath,
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

			"--tls-cert-file", c.certRef("apiserver-serving").certPath,
			"--tls-private-key-file", c.certRef("apiserver-serving").keyPath,

			"--proxy-client-cert-file", c.certRef("apiserver-extension-client").certPath,
			"--proxy-client-key-file", c.certRef("apiserver-extension-client").keyPath,

			"--external-hostname", c.externalHostname(),
			"--advertise-address", c.kubernetesServiceAddress(),

			"--etcd-servers", c.unixSocketPath("etcd").String(),
			"--etcd-cafile", c.caRef(Etcd).certPath,
			"--etcd-certfile", c.certRef("apiserver-etcd-client").certPath,
			"--etcd-keyfile", c.certRef("apiserver-etcd-client").keyPath,
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
			"--client-ca-file", c.caRef(APIClient).certPath,
			"--requestheader-client-ca-file", c.caRef(RequestHeader).certPath,
			"--requestheader-extra-headers-prefix=X-Remote-Extra-",
			"--requestheader-group-headers=X-Remote-Group",
			"--requestheader-username-headers=X-Remote-User",
			"--requestheader-uid-headers=X-Remote-UID",

			"--service-account-lookup=false",
			"--service-account-issuer", "https://" + c.externalHostname(),
			"--service-account-max-token-expiration=1h",
			"--service-account-extend-token-expiration=false",
			"--service-account-signing-endpoint", c.unixSocketURL("jwtsigner").Opaque,

			"--kubelet-certificate-authority", c.caRef(KubeletServer).certPath,
			"--kubelet-client-certificate", c.certRef("apiserver-kubelet-client").certPath,
			"--kubelet-client-key", c.certRef("apiserver-kubelet-client").keyPath,
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
			"--tls-cert-file", c.certRef("controller-manager-server").certPath,
			"--tls-private-key-file", c.certRef("controller-manager-server").keyPath,

			"--kubeconfig", c.kubeConfigRef("controller-manager-client").Path,
			"--authentication-kubeconfig", c.kubeConfigRef("controller-manager-authnz").Path,
			"--authorization-kubeconfig", c.kubeConfigRef("controller-manager-authnz").Path,
			"--authentication-tolerate-lookup-failure=true",

			"--use-service-account-credentials=true",
			"--controllers", "*,-serviceaccount-token-controller,-legacy-serviceaccount-token-cleaner-controller",
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
			"--tls-cert-file", c.certRef("scheduler-server").certPath,
			"--tls-private-key-file", c.certRef("scheduler-server").keyPath,

			"--leader-elect=false",
			"--kubeconfig", c.kubeConfigRef("scheduler-client").Path,

			"--authentication-kubeconfig", c.kubeConfigRef("scheduler-authnz").Path,
			"--authorization-kubeconfig", c.kubeConfigRef("scheduler-authnz").Path,
			"--authentication-tolerate-lookup-failure=true",
		},
	}
}

// Webhook returns a process description for the internal webhook.
func (c *ControlPlane) Webhook() *process.Process {
	return &process.Process{
		Tag: "webhook",
		CmdArgs: []string{
			c.selfBinary(),
			"webhook",
		},
	}
}

// KMSPlugin returns a process description for the internal KMS plugin.
func (c *ControlPlane) KMSPlugin() *process.Process {
	return &process.Process{
		Tag: "kms",
		CmdArgs: []string{
			c.selfBinary(),
			"kms",
		},
	}
}

// JWTSigner returns a process description for the internal JWT signer.
func (c *ControlPlane) JWTSigner() *process.Process {
	return &process.Process{
		Tag: "jwtsigner",
		CmdArgs: []string{
			c.selfBinary(),
			"jwtsigner",
		},
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
		"--cacert", c.caRef(APIServer).certPath,
		"--cert", c.certRef("system-admin").certPath,
		"--key", c.certRef("system-admin").keyPath,
	}
	fa := append(a, args...)
	return syscall.Exec(p, fa, finalEnv)
}

// EtcdCtlExec executes etcdctl with the appropriate certificates and endpoint.
func (c *ControlPlane) EtcdCtlExec(env, args []string) error {
	finalEnv := append(makeSafeEnv(env),
		fmt.Sprintf("ETCDCTL_ENDPOINTS=%s", c.unixSocketPath("etcd").String()),
		fmt.Sprintf("ETCDCTL_CACERT=%s", c.caRef(Etcd).certPath),
		fmt.Sprintf("ETCDCTL_CERT=%s", c.certRef("apiserver-etcd-client").certPath),
		fmt.Sprintf("ETCDCTL_KEY=%s", c.certRef("apiserver-etcd-client").keyPath),
	)
	p, err := exec.LookPath("etcdctl")
	if err != nil {
		return err
	}
	return syscall.Exec(p, args, finalEnv)
}
