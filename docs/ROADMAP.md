# Runkube Roadmap — towards an initial release

This document captures the plan for reaching an initial (v0.1.0) release of
Runkube. It is intentionally a planning artifact: it describes **what** needs
to happen and **why**, not the finished code. Milestones are ordered by
dependency — earlier phases de-risk later ones.

**Release prerequisites (agreed):**

1. Reasonable test coverage
2. Documentation
3. Improved CRI simulation
4. Complete the remaining extension points

A release is considered "achievable" when all four prerequisites are met, the
build is reproducible, and there is a documented release process.

---

## Current state (baseline, as of 2026-08-30)

### What the project is

A local Kubernetes "control plane + nodes" simulator and experimentation
harness that targets being a lighter-weight, more modifiable alternative to
`kind`/`envtest`:

- Real control-plane binaries: `etcd`, `kube-apiserver`, `kube-controller-manager`, `kube-scheduler`
- One kubelet per node, configured against a fake, model-based CRI
- A seccomp-based sandbox (`pkg/sandbox`) that emulates hardware/filesystem for
  unprivileged kubelet execution
- Extension points: extension API server (`pkg/kapi`), authn/authz/admission/audit
  webhooks, KMS v2 encryption plugin, external JWT signer, seccomp handler mux

### Test coverage (weakest area)

13 test files exist. Measured `go test -cover` on `pkg/*` and `tools/*`:

| Package | Coverage |
|---------|----------|
| `pkg/cri/model` | 80.1% |
| `pkg/sandbox/fdchan` | 72.9% |
| `pkg/cri` | 48.1% |
| `pkg/sandbox` | 47.8% |
| `pkg/sandbox/seccomp` | 32.8% |
| `pkg/config` | 15.0% |
| `pkg/resolve` | 0% (test file exists, exercises no statements) |
| `pkg/frontend`, `grpcmiddleware`, `kapi`, `nodes`, `ref`, `server`, `syncer`, `webhook` | 0% |
| `pkg/sandbox/seccomp/{filter,internal,supervisor}` | 0% |
| root `runkube`, all `tools/*` | 0% |

There is no CI and no coverage gate.

### Documentation

- `README.md` is minimal — a short pitch plus a feature bullet list
- `docs/` exists and is maintained: `TODO.md`, `DONE.md`, the CRI model
  redesign/usage docs (`cri-model-redesign.md`, `cri-model-usage.md`), and
  `experience/` write-ups (`dynamic-file-injection.md`, `single-roundtrip.md`)
- Good **internal** design docs exist inline: `pkg/sandbox/seccomp/AGENTS.md`,
  `pkg/sandbox/seccomp/MUX.md`, `pkg/sandbox/SYSTEM_MODEL.md`,
  `pkg/cri/model/AGENTS.md`
- No user-facing getting-started, CLI/config reference, architecture overview,
  extension guide, CONTRIBUTING, or CHANGELOG

### CRI simulation

- `pkg/cri/model` is the strong foundation: encapsulated object model with
  deep-clone semantics, a discrete-event (tick) simulation engine, resource
  usage accounting, an event stream, and per-API migration helpers. It sits at
  **80.1% coverage** (engine, events, resource, and consistency tests)
- Model-backed CRI RPCs already wired: create/list/start/stop/remove for
  sandboxes, containers and images, `ContainerStats`/`ListContainerStats`,
  `PodSandboxStats`/`ListPodSandboxStats`, `ListPodSandboxMetrics`,
  `StreamPodSandboxMetrics`, and `GetContainerEvents` (streams container events
  from the model's event stream — no more 30s sleep)
- Still returns bare `nil`: `ExecSync`, `Exec`, `Attach`, `PortForward`,
  `CheckpointContainer`, `UpdatePodSandboxResources`, `UpdateContainerResources`,
  `ReopenContainerLog`

### Sandbox

- Single "setup" fdchan roundtrip (child passes an fd on the container rootfs
  to the parent) and parent-side **dynamic file injection** are implemented —
  the system model can create/update files in a running container
  (`pkg/sandbox/rootfs.go`, `Rootfs.Write`/`InjectFiles`)
- Open TODO items: dynamic network interface (predictable MAC/IPv6 derived from
  the machine id), file IO improvements (TRO/RW/RDWR emulation, rootfs-backed
  files, `/.emulated`), filesystem disk/inode usage modeling (see `docs/TODO.md`)

### Extension points

- `pkg/kapi` extension API server has **no real storage**: it returns empty
  lists and fabricates GETs with `metav1.Now()`. Advertised verbs
  (`create/patch/update/delete`) are unimplemented, so custom resources are not
  persistent or writable
- Webhooks are mostly inert: `builtinAuthenticator` returns an empty
  `TokenReviewStatus`, `builtinAdmitter` always allows with no patch logic. The
  report authorizer has real logic; the auditor only logs
- `kubelet-server-unused` certificate is generated but never used (dead config)
- Missing per-package tests across `kapi`, `webhook`, `ref`, `nodes`,
  `seccomp/*` adapters

### Housekeeping

- `Makefile` still has a duplicated `all: $(TOOLS)` target; a `test` target
  exists but there are no `vet`/`coverage` targets
- Working tree has in-progress `docs/TODO.md` grooming (new items: dynamic
  network interface, sandbox disk usage, CRI disk usage feeding `statfs`)
- No git tags, no `CHANGELOG`, no release process
- Two remotes configured (`github`, `origin`) — single-source-of-truth target
  should be pinned in the release process

---

## Phase 0 — Baseline & housekeeping

*Goal: a clean, reproducible, verifiable starting point.*

- Settle the in-progress `docs/TODO.md` grooming (commit or refine)
- Fix the duplicated `Makefile` `all:` target; add `vet` and `coverage`
  targets next to `test`
- Add CI (GitHub Actions or equivalent): `go build ./...`, `go vet ./...`,
  `go test ./...`, `go test -race ./...`, and a coverage report
- Define a coverage bar (proposal: **≥60% overall on `pkg/*`**, per-package
  goals in Phase 1)
- Enforce formatting/linting as a CI gate
- Confirm a single upstream remote for release pushes

---

## Phase 1 — Test coverage

*Goal: raise coverage across `pkg/*` to the agreed bar, prioritizing
highest-risk packages first.*

`pkg/cri/model` and `pkg/sandbox/fdchan` are already at/above the target bar —
focus shifts to the still-uncovered packages:

1. **`pkg/kapi`** — storage/persistence is foundational; tests here shape the
   Phase 4 work. Cover discovery, openapi, list/get/create/patch/update/delete,
   and watch semantics
2. **`pkg/webhook`** — authn/authz/admission/audit handlers and the
   `attributes` interface
3. **`pkg/nodes`** — node/kubelet config generation and CRI server wiring
4. **`pkg/frontend`** + `pkg/server` — proxy/CONNECT routing, host matching,
   TLS
5. **`pkg/syncer`**, **`pkg/ref`** — config/file ref writing and sync
6. **`pkg/sandbox/seccomp/{filter,internal,supervisor}`** — adapters and
   handler dispatch
7. **`pkg/config`** (raise from 15%) — flag parsing, validation, defaults
8. **`pkg/resolve`** — the resolver/value plumbing (test exists but is inert)
9. **Root `runkube` package** — control-plane composition (cert generation,
   config file generation, webhook/KMS/JWT servers)
10. Raise `cri` (48%), `sandbox` (48%), and `seccomp` (33%) toward the goal

**Definition of done:** CI passes with the agreed coverage bar; no `pkg/*`
package below the per-package floor.

---

## Phase 2 — Documentation

*Goal: a user can go from clone → run → extend without reading source.*

- Rewrite `README.md`: what it is, install, quickstart, the main capabilities,
  link to docs/
- `docs/` additions:
  - `getting-started.md` / quickstart
  - `cli-and-config.md` — flags from `pkg/config` (`--name`, `--root`,
    `--external-hostname`, `--service-address-range`, `--node`), env
    variables, defaults
  - `architecture.md` — control plane + node + sandbox + extension points
  - `extending.md` — the extension guides:
    - adding a custom API group/resource (`pkg/kapi`)
    - implementing a webhook (authn/authz/admission/audit)
    - extending the CRI simulation
    - registering a seccomp handler (`pkg/sandbox/seccomp`)
  - `CONTRIBUTING.md` — build, test, CI, coverage expectations
  - `CHANGELOG.md` — release notes process (start capturing from Phase 0+)
- Promote/align the existing docs so they stay discoverable and current:
  `cri-model-redesign.md`, `cri-model-usage.md`, the `experience/` write-ups,
  and the inline `AGENTS.md`/`SYSTEM_MODEL.md`/`MUX.md` design docs — the
  new user docs should link them, not duplicate them
- Add a CI doc check (e.g., verify internal links exist)

**Definition of done:** all headings above exist with accurate, buildable
examples; README links resolve.

---

## Phase 3 — Improved simulation (CRI + sandbox fidelity)

*Goal: a credible fake runtime and filesystem that exercise kubelet paths
end-to-end.*

Already done (kept honest in the roadmap):

- CRI model redesign: engine, object model, resource usage, event stream,
  migration (DONE.md; `cri-model-redesign.md`)
- `GetContainerEvents` streams from the model event stream instead of sleeping
- `ContainerStats`, `ListContainerStats`, `PodSandboxStats`,
  `ListPodSandboxStats`, `ListPodSandboxMetrics`, `StreamPodSandboxMetrics`
  are model-backed
- Sandbox single-roundtrip setup and dynamic file injection
- Dynamic network interface — the default interface (`eth0`) gets a MAC,
  IPv4 and IPv6 address derived deterministically from the machine id
  (`pkg/netid`); the hardcoded 10.0.0.1 is removed from node/kubelet/CRI
  addresses

Remaining CRI work:

- `ExecSync` / `Exec` / `Attach` — simulate against the event model
- `PortForward` — model port forwarding semantics
- `CheckpointContainer` — status/transition handling
- `UpdatePodSandboxResources` / `UpdateContainerResources` /
  `ReopenContainerLog` — return meaningful model-backed state instead of `nil`
- Container process lifecycle — model processes that exit (successfully or
  not) after a realistic number of ticks so kubelet has to restart them
  (see `docs/TODO.md`)

Sandbox fidelity (from `docs/TODO.md`):

- File IO improvements — emulate files as TRO/RW/RDWR, back files in the rootfs
  (or `/.emulated` for proc-like paths) on first open instead of fresh memfds
- Disk usage — model filesystem disk/inode usage for configured mount points,
  and container image/container/log space consumption feeding through
  `statfs` calls

Other:

- Reconcile `kubelet-server-unused` certificate (use it or remove it)
- Raise `pkg/cri` and `pkg/sandbox` coverage in lock-step (see Phase 1)

**Definition of done:** no CRI RPC silently returns a bare `nil`; kubelet can
drive create/start/stop/exec against the fake runtime and the sandbox emulates
network/filesystem behavior plausibly; changes are tested.

---

## Phase 4 — Complete the remaining extension points

*Goal: every advertised extension point actually works and is tested.*

- **`pkg/kapi` real storage**: implement in-memory (or etcd-backed) CRUD with
  watch so custom resources are persistent, writable, and watchable. Advertised
  verbs must behave as advertised
- **Webhooks**: wire the authenticator and admitter so they are exercised
  end-to-end; add patch/validation logic tests
- **KMS v2 plugin** and **external JWT signer**: are implemented — add tests and
  document the extension contract
- **seccomp handler mux**: finish handler-interface coverage and document
  adding new intercepted paths
- Remove or repurpose dead configuration (e.g. `kubelet-server-unused`)

**Definition of done:** a user can register a custom resource, write/read/watch
it through the extension API server, and each webhook/KMS/JWT/seccomp surface
has test coverage and a documented extension contract.

---

## Phase 5 — Initial release (v0.1.0)

*Goal: a repeatable, consumable first release.*

- Versioning scheme + tag convention (`v0.1.0`)
- Complete `CHANGELOG.md` for the release
- Reproducible build/artifact process and checksums
- Publish docs + install instructions; update README
- Final coverage/CI green check against release candidate

---

## Definition of done — overall

- All four prerequisites met (coverage bar, docs, CRI, extension points)
- CI is green on the release candidate (build, vet, race, coverage)
- `CHANGELOG.md` reflects the release
- One tagged `v0.1.0` with published artifacts and docs

---

## Notes / open questions

- Exact coverage bar and per-package floors (proposal above: ≥60% overall;
  no package below a per-package floor — tune during Phase 1)
- Whether `kapi` storage should be in-memory or etcd-backed (etcd is already
  present in the control plane; decide during Phase 4)
- Single source of truth for the release remote (`github` vs `origin`)
- Whether the Phase 3 sandbox-fidelity items (network interface, file IO,
  disk usage) are release blockers for v0.1.0 or can slip past it — kubelet
  currently runs against the sandbox; these items raise credibility, not
  viability
- Per-component log levels and lazy (re)initialization (`docs/TODO.md`) are
  open but not release prerequisites; fold in where they interact with Phase 0/3