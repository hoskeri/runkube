# Health checks

Runkube reports health for every control-plane component, and for the
components inside each node's sandbox, using procman's status API
(`process.Process.Status` / `process.Formation.Status`; see
`../procman/docs/STATUS.md` for the design).

## What is checked

Each probe is a cheap "is it still serving?" question, asked every
`HealthInterval` (5s) with a `probeTimeout` (3s) budget. Process *liveness* is
not a probe's job — procman already knows whether the process is running — so
the probes only answer "can it do its job?".

| Component | Probe | Healthy means |
| --- | --- | --- |
| `etcd` | gRPC `grpc.health.v1.Health/Check` over the etcd unix socket (TLS as the etcd client) | etcd answers that it is serving |
| `api` | `GET /livez` and `GET /readyz` on `https://[::1]:6443` with the system-admin certificate | the apiserver is up and willing to serve |
| `controller`, `scheduler` | `GET /livez` on the component's own secure port (`10257` / `10259`), verified with its generated serving certificate | the component answers for itself |
| `webhook` | `GET /` on the frontend's abstract unix socket | the CONNECT proxy in front of the admission/authentication/authorization backends is accepting |
| `kms`, `jwtsigner` | dial the gRPC server's abstract unix socket | the server is accepting |
| `node-a/kubelet` | `GET http://127.0.0.1:10248/healthz` (from inside the sandbox) | the kubelet reports itself healthy |
| `node-a/criserver` | dial `/run/containerd/containerd.sock` (from inside the sandbox) | the fake CRI server is accepting |

A probe that fails because nothing is listening yet (`ENOENT`, `ECONNREFUSED`,
or `Unavailable` from gRPC, which hides the errno) is reported as
**degraded**, not unhealthy: that is the normal state during startup. So is a
5xx from a `/readyz` endpoint, which is how a server says "up, not ready yet"
while it finishes starting. Anything else — including a 5xx from `/livez`, and
any 4xx — is **unhealthy**. All of these are recoverable, so no single probe
result can take the cluster down.

Probes reuse one cached HTTP transport (and therefore one cached TLS
handshake) per endpoint, so the 5s cadence costs a request rather than a new
TLS session every time.

Healthy statuses are logged at debug level, so `runkube --log-level debug`
shows the full picture; degraded and unhealthy are always visible.

## When does the cluster go down

`ControlPlane.FormationStatus` is the only thing allowed to end the formation
for a health reason. It reads the aggregate view procman attaches to its
context and applies two rules:

1. A component that reported **failed** is always critical. That state is an
   explicit "I am done" from a child that pushed status, and honoring it is the
   point of the vocabulary.
2. Otherwise only **etcd** and **api** escalate on their own. The control
   plane cannot function without its store or its API, whereas a flapping
   webhook, scheduler or node is a self-healing wobble.

A critical problem starts a grace clock (`--health-grace`, default 5m). The
cluster is reported as degraded and waited out; if the problem is still there
past the grace window, the policy returns a fatal `status.Failed` and procman
tears the formation down exactly as it would for a process exit. Recovery
clears the clock, so a later blip gets the full window again.

An exited process is never escalated here: procman's exit semantics already
bring the formation down, so health never double-reports it.

## Where to look

```console
$ runkube --log-level debug
...
          etcd/status | DEGRADED etcd is not serving yet: dial unix ...: connect: no such file or directory
            api/status | UNHEALTHY api is unhealthy: dial tcp [::1]:6443: connect: connection refused
msg="control plane degraded" components=api(unhealthy) grace=5m0s detail="etcd=healthy api=unhealthy ..."
```

The one-line `control plane status` summary is logged whenever it changes, and
`control plane degraded` when a critical component first goes bad.

## Adding a probe

Attach it where the process is described and return `nil` when healthy:

```go
func (c *ControlPlane) MyComponent() *process.Process {
	return c.withHealth(&process.Process{Tag: "mine", CmdArgs: ...}, func(ctx context.Context) error {
		// nil == healthy; anything else is classified (not serving yet ==
		// degraded) by withHealth.
	})
}
```

Probes must be side-effect free and must not return a `status.Failed`: process
statuses never end a formation on their own, and the formation policy is where
that decision belongs.