# CRI Model Redesign Proposal

Status: **proposal** — not yet implemented. Tracking item: `docs/TODO` → "CRI
model / Runtime, Sandbox, Image and Container State Machines".

## 1. Goals

Reach the TODO's description: a performant discrete-event simulation of a
runtime modelling sandbox/container/image lifecycles, where events are
mutations, objects track readiness and **simulated resource usage**, and
mutations are triggered either from the runtime API or by the passage of time
(a tick).

Specifically fix the three pain points with the current `pkg/cri/model`:

1. **No synchronous *or* asynchronous update API** — `Enqueue` is secretly
   synchronous (blocks the caller until the event fires) and cri.go contorts
   around it.
2. **No resource-usage modelling, no statistics** — stats fields exist on the
   objects but nothing ever populates them; `ListPodSandboxStats`,
   `ListPodSandboxMetrics`, `StreamPodSandboxMetrics` are all empty/`nil`.
3. **No event stream** — model mutations are silent, so `GetContainerEvents`
   cannot be implemented.

## 2. Analysis of the current implementation

Reference: `pkg/cri/model/model.go` (472 lines), consumed by
`pkg/cri/cri.go`.

### What exists

- `Runtime{mu, sandboxes, containers, images, tickRate, seq, events}` — a
  mutex-guarded store plus a tick queue.
- `Sandbox`/`Container`/`Image` — thin wrappers around cri-api protos
  (`PodSandbox`+`PodSandboxStatus`, `Container`+`ContainerStatus`,
  `ImageSpec`), deep-cloned with `proto.Clone` on every boundary crossing.
- `Enqueue(delayTicks, callback)` — registers `Event{Sequence, Callback}`.
- A background goroutine (`runSimulation`) ticks at `tickRate`, increments
  `seq`, linear-scans `events`, and dispatches due callbacks outside the lock.
- Add/Remove/Update/Get/List for each object kind; Get delegates to List;
  Match/filter helpers; cached-but-never-populated `stats` fields with
  `List*Stats`/`Get*Stats` accessors.

### Why it is hard to use

1. **`Enqueue` is synchronous** (model.go:88-101). It registers the event,
   then blocks on a per-event `done` channel until the ticker goroutine fires
   it. Every delayed transition in cri.go (sandbox → READY after 10 ticks,
   container → RUNNING, image pull) therefore stalls the RPC handler for
   real wall-clock time. Consequences:

   - `RunPodSandbox` returns only *after* the sandbox is already READY,
     contradicting the NOTREADY initial state the test asserts
     (cri_test.go:52-57).
   - `PullImage`'s unlock/select/relock dance (cri.go:499-519) is dead code:
     the completion channel is already closed when `Enqueue` returns.
   - There is no async path anywhere. Delays cannot be skipped or awaited
     selectively.

2. **Simulated time is wall time.** `seq` only advances when the real
   ticker fires; a "10 tick" delay is 1 second of real sleeping. Tests must
   `time.Sleep(1100ms)` (cri_test.go:75). No fast-forward, no determinism,
   no reproducibility, and no way to run "performant" bulk simulation.

3. **Resource usage is not modelled.** `Sandbox.stats` /
   `Container.stats` (`PodSandboxStats` / `ContainerStats`) are never
   written anywhere in the repo. There is no utilization, no cumulative
   counters, no rates, no per-state behavior, no aggregation into sandbox
   stats. `ListPodSandboxStats` / `ListContainerStats` always return empty;
   `ListPodSandboxMetrics` / `StreamPodSandboxMetrics` return `nil`; kubelet
   sees a runtime that never consumes a resource.

4. **No event stream.** Add/Update/Remove and the state flips inside
   callbacks are applied silently. Nothing notifies observers, so
   `GetContainerEvents` cannot be implemented; it currently
   `time.Sleep(30s)` and returns, closing the stream.

5. **Structural fragilities.**
   - *Dual-wired state:* every transition mutates both
     `PodSandbox.State`/`PodSandboxStatus.State` (and `Container`/`ContainerStatus`),
     with nothing enforcing consistency; a desync produces contradictory
     List vs Status responses.
   - *Get-modify-put everywhere:* callbacks clone an object out, mutate
     protobufs, clone back in. Correct but wasteful, and multi-object
     mutations (stop-sandbox cascades) commit as several separate locked
     operations with no atomicity.
   - *Lying signature:* `Enqueue` returns `<-chan struct{}` but always
     returns `nil` after blocking.
   - *Deadlock shape:* blocking `Enqueue` is called with `b.mu` held; safe
     only because callbacks today never touch `b.mu`. The contract is
     fragile and undocumented.
   - *O(n) dispatch:* linear scan of the event slice per tick.

## 3. Proposed design

### A. Engine: deterministic virtual time

- The runtime owns an explicit simulation clock: `now uint64` in ticks.
- Events are a priority queue (container/heap) keyed by due tick; equal due
  ticks run FIFO.
- `Advance(n)` fires all events with `due <= now+n`; when the queue is empty
  it is O(1) and jumps the clock directly. `Step()` = advance one tick.
- A background goroutine (wall-clock pacing) becomes an *optional* adapter
  that simply calls `Advance(1)` on a ticker; tests and callers can also
  drive the clock synchronously. No code path depends on real time.
- `Time() time.Time` derives wall time from sim time
  (`start + now*ticksPerTick`) so timestamps (CreatedAt, StartedAt, …) are
  deterministic and consistent.
- **Performant accounting:** resource accounting over an advance window
  integrates the load profile (`u·cpu·Δticks`) instead of iterating
  tick-by-tick, so `FastForward(n)` over a quiet system is O(events) +
  O(objects), not O(n·objects).

### B. Operations: synchronous and asynchronous

One primitive, two flavors, both funneling through a single-writer commit
loop:

- `Do(mutations ...Mutation) error` — commits immediately at `now`, returns
  when done.
- `Submit(delay Ticks, mutations ...Mutation) *Future` — commits at
  `now+delay`; `*Future` exposes `Done() <-chan struct{}`, `Wait(ctx) error`,
  `Cancel()`.
- The commit loop owns the store; mutations, callbacks, and event emission
  are strictly serialized. No two commits run concurrently; callbacks can
  submit further mutations or block on futures without deadlock.
- Multi-object mutations (e.g., stop-sandbox: sandbox → NOTREADY and all
  member containers → EXITED) are one atomic commit — observers see only the
  final state.
- Mutations are small immutable types (`SetSandboxState{id, state}`,
  `CreateContainer{...}`, `PullImage{spec}`, `UpdateResources{...}`,
  ...). The CRI layer composes them; the model owns the state machine
  transitions and their timing.

### C. Object model: single source of truth

- Internal state per object: `{id, spec, state, timestamps, usage}`. The
  cri-api protos become *derived views*: `PodSandbox()`,
  `PodSandboxStatus()`, `Container()`, `ContainerStatus()` are materialized
  from internal state on demand, so they can never disagree.
- `Clone()` is paid only when materializing outputs, not on every mutation.
- Match/filter helpers remain as-is.

### D. Resource usage model (centerpiece)

Per object, three layers:

1. **`ResourceSpec` (static)** — cpu (millicores) and memory (bytes) limits
   from the container's Linux resources when provided, sensible defaults
   otherwise. Optionally network/io capacity. Immutable per object (updates
   via `UpdateResources` mutation).
2. **Load profile (behavioral)** — how the object consumes resources in
   each state:
   - `idleUtil` — fraction of capacity used while CREATED/EXITED (small,
     e.g. 0.02).
   - `activeUtil` — fraction while RUNNING (e.g. 0.35, configurable).
   - Pluggable `LoadFn func(t Ticks) float64` for scripted workloads
     (sinusoid, steps, random walks) — the default is per-state constants.
   - Profile is per sandbox/container; sandboxes derive activity from their
     containers.
3. **Accounting (dynamic)** — updated in the commit loop on every advance:
   - *Cumulative counters* (what the CRI stats API wants):
     `usageCoreNanoSeconds += u·cpuMillis·nsPerTick·Δticks`, page faults,
     rss, network rx/tx bytes, io.
   - *Rates*: `usageNanoCores` (current utilization), EWMA over a sample
     window for smoothed values.
   - *Memory dynamics*: working set approaches `limit·u` with a rate limit
     (`ws += (target-ws)·α`), so memory visibly ramps up after start and
     drains after stop.
   - *Aggregation*: sandbox stats = sum of member container stats plus a
     network component scaled by activity. (The `PodSandboxStats` proto
     already carries per-container `ContainerStats` inside
     `LinuxPodSandboxStats`.)

Materialization:

- `ContainerStats` / `PodSandboxStats` protos are built at request time from
  internal `Usage` — no stale caches to keep in sync.
- A small **metric descriptor registry** maps internal counters to CRI
  `Metric{Name, MetricType: GAUGE|CUMULATIVE, Value, Timestamp}`.
  `ListPodSandboxMetrics` / `StreamPodSandboxMetrics` are served from it
  (per-sandbox `Metrics` + per-container `ContainerMetrics`). The metric
  names are provisional (see Decisions, D1).
- Expose a stats *snapshot* point-in-time view so `ContainerStats` RPCs
  reflect the current tick.

Optional but valuable: **limit enforcement.** When utilization is pinned at
100% past the limit, the model may emit a throttle/OOM event and drive the
container to EXITED with reason `OOMKilled`. Off by default; this is what
makes resource modelling observable in the *lifecycle*, not just the
numbers.

### E. Event stream

- Typed events emitted on every commit: `SandboxCreated/Removed`,
  `SandboxStateChanged{from,to}`, `ContainerCreated/Removed`,
  `ContainerStateChanged{from,to}`, `ImagePulled/Removed`, `ResourceEvent`.
- `Subscribe() *Subscription` with `Events() <-chan Event` and `Close()`.
  Fan-out happens **outside the engine lock** into bounded, non-blocking
  subscriber channels; a slow consumer drops events (or receives a
  disconnect signal) rather than stalling the engine. This is the one hard
  rule that keeps the sim performant with many listeners.
- `GetContainerEvents` (cri.go) subscribes, maps
  `ContainerStateChanged`/created/deleted to
  `ContainerEventResponse{ContainerId, ContainerEventType: CREATED|STARTED|STOPPED|DELETED, CreatedAt, PodSandboxStatus, ContainersStatuses}`
  (status snapshots come from the derived views), and streams to the RPC
  client; on client disconnect, close the subscription.
- `StreamPodSandboxMetrics` reuses the same pipeline, filtered to metric
  events (or a sampling subscription) — one event pipeline, two RPC faces.

### F. Compatibility and migration

- Keep the existing Add/Remove/Update/Get/List + Match surface as thin
  synchronous sugar over the new engine; cri.go migrates incrementally.
- `Enqueue`'s blocking semantics are deprecated in favor of
  `Schedule`/`Submit`; it is retained (and still tested) but cri.go no
  longer uses it.
- CRI layer: `RunPodSandbox`/`StartContainer`/etc. issue a synchronous
  create, then `Schedule` the delayed READY/RUNNING transition and block
  on `Future.Wait(ctx)` until it completes (via `Schedule`, whose callback
  may use the public API — `Submit` mutations run under the lock and must
  not re-enter). The lifecycle RPCs are therefore **synchronous**: the
  model keeps simulating the transition asynchronously, but
  `StartContainer` returns only once the container is RUNNING,
  `StopContainer` only once it is EXITED, and `RunPodSandbox` /
  `StopPodSandbox` once the sandbox reaches READY / NOTREADY (with child
  containers EXITED). `PullImage` is synchronous the same way. Callers
  that want to observe the intermediate states as they happen use
  `GetContainerEvents`, which streams the model's event feed.
- Keep `pkg/cri/model` fully encapsulated; cri.go still only sees public
  APIs. Update `pkg/cri/model/AGENTS.md` to match.

## 4. Implementation plan (phased)

Status: **all five phases implemented** (commits per phase, see git log).

1. ✅ **Engine** — virtual clock, priority queue, `Advance`/`Step`/`FastForward`,
   `Do`/`Submit`/`Schedule`/`Future`, optional wall-clock adapter.
2. ✅ **Object model** — state consistency: the object proto (`PodSandbox` /
   `Container`) is the single source of truth for `State`; every write path
   (Add/Update) reconciles the status proto's `State` to match, and `Match`
   filters use the authoritative state, so the dual protos can never
   disagree. (Deviates from the original "full derived views" plan: the
   in-place get-modify-put flow requires pointer-returning accessors, so
   reconciliation on write was chosen instead; cri.go was migrated to
   `Schedule` in Phase 5, and typed mutations can supersede the in-place
   flow later.)
3. ✅ **Resource usage** — `Options`/`New`, `ResourceSpec`, state-based load
   profile with `Hooks.Load` override, integrated per-advance accounting
   (cumulative counters + rates, memory ramp, network), sandbox
   aggregation, stats/metrics materialization, `Hooks.Duration`
   + `DelayConfig`, `LimitEnforcement` (OOM).
4. ✅ **Event stream** — typed events on every commit, `Subscribe()` with
   bounded non-blocking fan-out, close semantics; `GetContainerEvents`,
   `ListPodSandboxMetrics`, `StreamPodSandboxMetrics` in cri.go.
5. ✅ **Migration** — cri.go uses `Schedule` for the model transitions and
   blocks on `Future.Wait(ctx)` so the lifecycle RPCs are synchronous
   (`RunPodSandbox`/`StopPodSandbox`/`StartContainer`/`StopContainer`
   return only once the simulated transition completed; `PullImage` waits
   the same way); `Enqueue` retained but deprecated (unused by cri.go,
   still tested); cri_test.go rewritten to assert the post-call states
   directly and to cover context cancellation of the synchronous waits;
   `docs/cri-model-usage.md` written; `pkg/cri/model/AGENTS.md` updated.

Known follow-ups (not blocking): metric names remain provisional (D1);
`Hooks.Duration`/`Delays` are exercised by the CRI layer through
`runtime.Duration` — the `OpKind`/`DelayConfig` knobs exist and are
covered by tests but not yet user-configurable from config files.

## 5. Decisions (hardcoded answers to the open questions)

D1. **Metric names (provisional).** No authoritative name list exists in the
    CRI spec or containerd for `ListPodSandboxMetrics` (containerd returns
    `Unimplemented`). Use these reasonable names for now; revisit when
    real-kubelet integration lands:

    | name               | type       | unit    | source                     |
    |--------------------|------------|---------|----------------------------|
    | `cpu/usage`        | COUNTER    | ns      | usageCoreNanoSeconds       |
    | `cpu/usage_cores`  | GAUGE      | cores   | usageNanoCores             |
    | `memory/usage`     | GAUGE      | bytes   | usageBytes                 |
    | `memory/working_set`| GAUGE     | bytes   | workingSetBytes            |
    | `memory/rss`       | GAUGE      | bytes   | rssBytes                   |
    | `network/rx_bytes` | COUNTER    | bytes   | cumulative rx              |
    | `network/tx_bytes` | COUNTER    | bytes   | cumulative tx              |

    Implemented as a small registry (`Name → (MetricType, accessor)`);
    adding/renaming descriptors later is a one-place change.

D2. **Default load profile:** idle 0.02, active 0.35 (fraction of the
    object's `ResourceSpec`). Both overridable per runtime via
    `Options.Hooks.Load` (see Usage guide).

D3. **Wall-clock adapter:** `NewRuntime(tickRate > 0)` starts the adapter and
    is the default for real runs (cri.go keeps `100ms`, as today).
    `tickRate == 0` selects a manually driven clock (`Advance`/`Step`), used
    by all tests. `Close()` stops the adapter; the runtime stays usable with
    a manual clock.

D4. **Limit/OOM enforcement:** off by default; opt-in via
    `Options.LimitEnforcement`.

D5. **Delays:** the current hardcoded constants in cri.go (10 ticks ≈ 1s for
    sandbox start/stop, container start/stop, image pull) become a
    `DelayConfig` in `Options` with those defaults; cri.go switches to named
    constants during migration (Phase 5).

## 6. Usage guide: overriding model behavior

Plan (document lands at `docs/cri-model-usage.md`, written in Phase 5):
users customize the simulation by passing override functions in
`Options.Hooks`; the model always composes them with defaults via
`Hooks.WithDefaults()`.

```go
opts := model.Options{
    TickRate: 100 * time.Millisecond,
    Hooks: model.Hooks{
        // Utilization in [0,1] for object ref at simulated time now.
        // Default: state-based (idle 0.02 / active 0.35).
        Load: func(rt *model.Runtime, ref model.ObjectRef, now model.Ticks) float64 { ... },
        // Duration of a simulated operation, in ticks.
        // Default: Options.Delays table.
        Duration: func(kind model.OpKind) model.Ticks { ... },
    },
}
r := model.New(opts)
```

The guide documents, for each hook: the signature, the default, when it is
called, thread-safety rules (hooks run on the simulation driver; use the
public API for reads, `runtime.Do` for atomic writes), and worked examples
(scripted CPU ramp, instant pulls for tests, slow starts to exercise kubelet
timeouts, OOM by load function returning >1).

Future hooks are designed for but not yet implemented: memory dynamics,
network scaling, custom event sinks.
