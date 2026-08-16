# Simulated Container Runtime Architecture and Model Design

This document describes the design and implementation of the encapsulated
simulated Kubernetes container runtime model in the `pkg/cri/model`
package. For user-facing extension docs see `docs/cri-model-usage.md`; for
the redesign rationale and the phased rollout see
`docs/cri-model-redesign.md`.

## 1. Architectural principles

The simulated container runtime is built around **strong encapsulation**,
**thread safety**, **immutability/cloning**, and **deterministic discrete
event simulation**. External callers (such as `pkg/cri/cri.go`) interact
strictly through public interfaces; all internal storage, the simulation
clock, and event delivery are encapsulated.

## 2. Models and encapsulation

* **`Sandbox`**: wraps `criv1.PodSandbox` and `criv1.PodSandboxStatus`,
  plus aggregated `Usage`.
* **`Container`**: wraps `criv1.Container` and `criv1.ContainerStatus`,
  plus a `ResourceSpec` and per-object `Usage`.
* **`Image`**: wraps `criv1.ImageSpec`.

### Single source of truth for state

`PodSandbox`/`Container` is the authoritative proto for `State`. Every
write path (`Add*`, `Update*`) reconciles the status proto's `State` to
match (`Sandbox.reconcile` / `Container.reconcile`), so the two protos can
never report different states. `Match()` state filters use the authoritative
state via `state()`. Status-only fields (Network, LogPath, FinishedAt, …)
pass through untouched.

### Immutability & corruption prevention

All model types provide `Clone()` (deep copies via `proto.Clone`, plus the
value fields). `Runtime` methods clone incoming objects on `Add*`/`Update*`
and clone outgoing objects on `Get*`/`List*`. Object accessors
(`PodSandbox()`, `PodSandboxStatus()`, …) return the object's internal
pointers, so the get-modify-put pattern (`Get` → mutate → `Update`) works;
the store's cloning guarantees the stored state cannot be corrupted by
callers.

## 3. Simulation engine (`engine.go`)

* **`Ticks`** (`uint64`) is the only unit of simulated time. The clock is
  owned by `Runtime` (`now`).
* **Event queue**: a `container/heap` priority queue keyed by `(due, order)`
  (FIFO at equal due ticks).
* **`Advance(n)` / `Step()` / `FastForward(n)`** move the clock and
  dispatch every event due by the target. Advance segments the window at
  event boundaries so a state change caused by an event is reflected in the
  accounting of later segments; events scheduled during dispatch with due ≤
  target run in the same Advance.
* **Wall-clock adapter**: `Options.TickRate > 0` starts a goroutine calling
  `Advance(1)` per tick; `tickRate == 0` selects a manually driven clock
  (used by all tests). `Close()` stops the adapter and closes
  subscriptions.

### Operations: Do, Submit, Schedule

* **`Do(muts ...Mutation)`** applies mutations immediately and atomically:
  the runtime lock is held for the whole list. Mutations must not call back
  into public methods.
* **`Submit(delay, muts ...Mutation)`** schedules an atomic commit at
  `now+delay`; the commit runs under the lock exactly like `Do`.
* **`Schedule(delay, fn)`** runs `fn` on the driver outside the lock; `fn`
  may use the public API and schedule further events.
* All three return `*Future` with `Done()`, `Wait(ctx)`, `Cancel()`.
* `Enqueue` remains as a deprecated blocking wrapper over `Schedule`.

## 4. Resource usage model (`resource.go`)

* **`Options`**: `TickRate`, `TickDuration` (100ms), `Delays`
  (10 ticks per op), `Hooks`, `LoadIdle`/`LoadActive` (0.02/0.35),
  `LimitEnforcement` (off). Zero values take defaults.
* **`ResourceSpec`**: CPU millis, memory bytes, network rates; assigned via
  the `SetContainerSpec` mutation; `DefaultResourceSpec` fallback.
* **Accounting**: `account(delta)` is invoked by Advance per segment. It
  snapshots object state under the lock, computes utilization per container
  (default state-based, or the `Hooks.Load` override, called outside the
  lock), then applies deltas under the lock:
  * CPU: `UsageCoreNanoSeconds` accumulates the integrated load
    (`u·millis·ns/tick·ticks`); `UsageNanoCores` is the instantaneous rate.
  * Memory: working set ramps exponentially toward `limit·u` and drains
    after stop; page faults accrue with growth.
  * Network: cumulative rx/tx bytes at the configured interface rates.
  * Sandbox usage is re-aggregated from member containers plus a 10%
    network overhead.
* **Stats materialization**: `Get*Stats`/`List*Stats` build CRI protos from
  internal `Usage` at request time — no stale caches. `PodSandboxMetrics`
  serves the provisional metric descriptors (`cpu/usage`,
  `cpu/usage_cores`, `memory/{usage,working_set,rss}`,
  `network/{rx,tx}_bytes`).
* **Limit enforcement**: with `LimitEnforcement`, a running container whose
  load reaches 1.0 transitions to EXITED (reason `OOMKilled`, exit 137) and
  emits a state-change event.

## 5. Event stream (`events.go`)

* Every commit emits a typed `Event` (created/removed/state-changed per
  object kind; `From`/`To` states; optional `Reason`; `At` tick).
* `Subscribe()` registers a bounded (128) non-blocking channel; a slow
  consumer drops events — the engine never blocks on delivery.
* `Subscription.Close()` and `Runtime.Close()` close the channel.
* `pkg/cri/cri.go` maps container events to `GetContainerEvents`
  (CREATED/STARTED/STOPPED/DELETED with status snapshots) and serves
  `ListPodSandboxMetrics` / `StreamPodSandboxMetrics` from the accounting.

## 6. Thread safety

* All public methods are safe for concurrent use.
* Hooks run on the driver outside the lock (public API reads OK).
* `Do`/`Submit` mutations run with the lock held and must not re-enter.
* Only one goroutine drives the clock at a time (the adapter, or the
  caller of `Advance` in manual mode).
