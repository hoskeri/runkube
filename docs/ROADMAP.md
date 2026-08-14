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

## Current state (baseline, as of writing)

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

Only 7 test files exist. Several packages are at **0% coverage**:

| Package | Coverage |
|---------|----------|
| `pkg/config` | 15% |
| `pkg/cri` | 0% (test file exists) |
| `pkg/sandbox` | 42.4% |
| `pkg/sandbox/fdchan` | 72.9% |
| `pkg/sandbox/seccomp` | 32.8% |
| `pkg/cri/model`, `frontend`, `grpcmiddleware`, `kapi`, `nodes`, `ref`, `seccomp/filter`, `seccomp/internal`, `seccomp/supervisor`, `server`, `syncer`, `webhook`, root `runkube`, all `tools/*` | 0% |

There is no CI and no coverage gate.

### Documentation

- `README.md` is 7 lines
- `docs/` is empty
- Good **internal** design docs exist inline: `pkg/sandbox/seccomp/AGENTS.md`,
  `pkg/sandbox/seccomp/MUX.md`, `pkg/sandbox/SYSTEM_MODEL.md`,
  `pkg/cri/model/AGENTS.md`
- No user-facing getting-started, CLI/config reference, architecture overview,
  extension guide, CONTRIBUTING, or CHANGELOG

### CRI simulation

- `pkg/cri/model` has a strong foundation: encapsulated models, deep-clone
  semantics, and a discrete-event simulation (tick) queue
- Large slice of the CRI surface returns `nil`: `ExecSync`, `Exec`, `Attach`,
  `PortForward`, `CheckpointContainer`, `ListPodSandboxMetrics`,
  `UpdatePodSandboxResources`, `UpdateContainerResources`, `ReopenContainerLog`
- `GetContainerEvents` just sleeps 30s instead of streaming real events

### Extension points

- `pkg/kapi` extension API server has **no real storage**: it returns empty
  lists and fabricates GETs with `metav1.Now()`. Advertised verbs
  (`create/patch/update/delete`) are unimplemented, so custom resources are not
  persistent or writable
- Webhooks are mostly inert: `builtinAuthenticator` returns empty,
  `builtinAdmitter` always allows with no patch logic. The report authorizer has
  real logic; the auditor only logs
- `kubelet-server-unused` certificate is generated but never used (dead config)
- Missing per-package tests across `kapi`, `webhook`, `ref`, `nodes`,
  `seccomp/*` adapters

### Housekeeping

- `Makefile` has a duplicated `all: $(TOOLS)` target
- Working tree had in-progress, uncommitted `logLevelFor` changes
- No git tags, no `CHANGELOG`, no release process
- Two remotes configured (`github`, `origin`) — single-source-of-truth target
  should be pinned in the release process

---

## Phase 0 — Baseline & housekeeping

*Goal: a clean, reproducible, verifiable starting point.*

- Commit (or otherwise settle) in-progress working-tree changes
- Fix the duplicated `Makefile` `all:` target; add `test`, `vet`, `coverage`
  targets
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

Order by risk/value:

1. **`pkg/kapi`** — storage/persistence is foundational; tests here shape the
   Phase 4 work. Cover discovery, openapi, list/get/create/patch/update/delete,
   and watch semantics
2. **`pkg/cri/model`** — the simulation engine. Cover encapsulation, deep-clone,
   event-queue ordering, and tick behavior
3. **`pkg/webhook`** — authn/authz/admission/audit handlers and the
   `attributes` interface
4. **`pkg/nodes`** — node/kubelet config generation and CRI server wiring
5. **`pkg/frontend`** + `pkg/server` — proxy/CONNECT routing, host matching,
   TLS
6. **`pkg/syncer`**, **`pkg/ref`** — config/file ref writing and sync
7. **`pkg/sandbox/seccomp/{filter,internal,supervisor}`** — adapters and
   handler dispatch
8. **`pkg/config`** (raise from 15%) — flag parsing, validation, defaults
9. **Root `runkube` package** — control-plane composition (cert generation,
   config file generation, webhook/KMS/JWT servers)
10. Raise `sandbox`/`seccomp`/`fdchan` toward the goal

**Definition of done:** CI passes with the agreed coverage bar; no `pkg/*`
package below the per-package floor.

---

## Phase 2 — Documentation

*Goal: a user can go from clone → run → extend without reading source.*

- Rewrite `README.md`: what it is, install, quickstart, the four main
  capabilities, link to docs/
- `docs/`:
  - `getting-started.md` / quickstart
  - `cli-and-config.md` — flags from `pkg/config` (`--name`, `--root`,
    `--external-hostname`, `--service-address-range`, `--node`), env
    variables, defaults
  - `architecture.md` — control plane + node + sandbox + extension points
  - `extending.md` — the four extension guides:
    - adding a custom API group/resource (`pkg/kapi`)
    - implementing a webhook (authn/authz/admission/audit)
    - extending the CRI simulation
    - registering a seccomp handler (`pkg/sandbox/seccomp`)
  - `CONTRIBUTING.md` — build, test, CI, coverage expectations
  - `CHANGELOG.md` — release notes process (start capturing from Phase 0+)
- Promote/align the existing internal design docs (`ARCHITECTURE`/`AGENTS.md`
  files) so they stay discoverable and current
- Add a CI doc check (e.g., verify internal links exist)

**Definition of done:** all headings above exist with accurate, buildable
examples; README links resolve.

---

## Phase 3 — Improved CRI simulation

*Goal: a credible fake runtime that exercises kubelet paths end-to-end.*

- `ExecSync` / `Exec` / `Attach` — simulate against the event model
- `PortForward` — model port forwarding semantics
- `CheckpointContainer` — status/transition handling
- `ListPodSandboxMetrics` / `UpdatePodSandboxResources` /
  `UpdateContainerResources` / `ReopenContainerLog` — return meaningful
  model-backed state instead of `nil`
- `GetContainerEvents` — stream events from the tick/event queue instead of a
  30s sleep
- Reconcile `kubelet-server-unused` certificate (use it or remove it)
- Raise `pkg/cri` and `pkg/cri/model` coverage in lock-step (see Phase 1)

**Definition of done:** no CRI RPC silently returns a bare `nil`; kubelet can
drive create/start/stop/exec against the fake runtime; changes are tested.

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
