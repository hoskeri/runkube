# Runkube

Runkube is a collection of testing and experimentation tools for
[Kubernetes](https://k8s.io).

## runkube

`runkube` can be a simple replacement for [envtest][] or [kind][], while being
much easier to modify, with much fewer dependencies, and OS privileges.

[kind]: https://kind.sigs.k8s.io/
[envtest]: https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/envtest

Starting `runkube` will start a standard Kubernetes control plane and nodes.

- Authorizer, admission, audit webhooks
- Extension API Server
- KMS v2 Encryption
- Fake CRI service
- Sandbox and filesystem stubs to enable running kubelet
  without any privileges (unprivileged user namespaces required)

Every control-plane component (and every component inside a node's sandbox)
reports health, and the cluster is torn down only when a critical component
stays unhealthy past `--health-grace`. See [docs/HEALTH.md](docs/HEALTH.md).
