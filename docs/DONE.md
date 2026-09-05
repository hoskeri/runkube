# DONE Items

Items from docs/TODO.md should be moved here.

## Sandbox

### Dynamic network interface

Status: **done** — implemented 2026-08-30; derivation lives in the new
`pkg/randid` package.

The sandbox's default interface (`eth0`, a dummy device created in the
sandbox network namespace) is configured with a MAC address, a private IPv4
address and a ULA IPv6 address derived deterministically from the machine id
(`randid.MAC`, `randid.IPv4`, `randid.IPv6`): a machine id that is exactly 16
bytes (32 hex chars)
is used as-is, anything else is SHA-256 hashed. The MAC is always unicast +
locally administered (can never collide with a real OUI), IPv4 is a 10/8
address, IPv6 is a `fd00::/8` ULA. Loopback retains its kernel-assigned
127.0.0.1/8 and ::1/128; `Config.IPAddress` remains as an optional IPv4
override.

The hardcoded 10.0.0.1 is gone everywhere: the node/kubelet address and
clusterDNS derive from the machine id (`pkg/nodes`), the dead `Address` field
was removed from `nodes.Node`, the `isolated` tool no longer sets a fixed IP,
and the CRI pod sandbox IP is derived from the sandbox id (each emulated pod
gets its own deterministic address). Verified live inside the sandbox:
`ip link/addr` shows MAC `32:e8:25:d5:70:1d`, IPv4 `10.224.107.116/24` and
IPv6 `fd00:32e8:25d5:701d:11f0:8f96:6c02:e06b` for machine id
`32e825d5701d11f08f966c02e06b743e`.

### Dynamic file injection

Status: **done** — implemented 2026-08-30; see
`docs/experience/dynamic-file-injection.md`.

The child passes an fd on the container rootfs to the parent in the setup
response. The parent writes files into the running container through it with
rootfs-relative *at syscalls (Rootfs.Write/MkdirAll, atomic temp+rename), so
the system model can create and update files at runtime, not just at container
start.

The model-rendered /sys/devices tree and write-type payloads are rendered once
(Sandbox.bootFiles) and materialized both by the child's boot path
(deterministic startup) and the parent's rootfs-fd injection at response time.
bind payloads stay child-side (they are real mounts).

### Single Roundtrip

Status: **done** — implemented 2026-08-30 as a single "setup" RPC; see
`docs/experience/single-roundtrip.md`.

Instead of multiple fdchan roundtrips, lets just perform one "setup" RPC. the
request passes metadata and file descriptors to the child, and the child
responds with other metadata and file descriptors to the parent.

## Runkube

### Log level per component

Status: **done** — implemented 2026-09-05.

Component log output is configurable instead of always silent:

- New `--log-level` flag on the `runkube` tool: a bare level
  (`error|warn|info|debug`, default `error`) sets the default for all
  components; comma-separated `tag=level` pairs override per component, e.g.
  `--log-level=info,api=debug,node-a=error`. Tags are `etcd`, `api`,
  `controller`, `scheduler`, `webhook`, `kms`, `jwtsigner` and the node names.
- `ControlPlane.logLevelFor(tag)` resolves the effective level (override or
  default) from the config and feeds the per-process `process.Process.LogLevel`
  used by procman's termhandler to filter each component's output.
- The level propagates to nodes through the existing `RUNKUBE_NODE`
  serialization: `nodes.Node` gained a `LogLevel` field, the node process
  carries it, and the in-sandbox node formation uses it as its sink level, so
  `kubelet`/`criserver` output is filtered at the node's level too.
- Config parsing and roundtrips are covered by `pkg/config`/`pkg/nodes` tests.

## CRI model

### Runtime, Sandbox, Image and Container State Machines

Status: **implemented** — all five phases (engine, object model, resource
usage, event stream, migration); see `docs/cri-model-redesign.md`.

Design a performant discrete event simulation of a runtime, modelling the
lifecycle of sandboxes, containers, and images. Events are mutations of
sandboxes, containers and images. The model keeps track of readiness state and
simulated resource usage for each object.  Mutations can be triggered from
runtime api, or by the passage of time (modelled as a tick).
