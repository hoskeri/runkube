# CRI Model Usage Guide

How to use and extend the simulated container runtime model
(`pkg/cri/model`): the simulation clock, the synchronous/asynchronous
operation API, and the override functions that customize model behavior.

Design and rationale: `docs/cri-model-redesign.md`.

## Quick start

```go
r := model.NewRuntime(100 * time.Millisecond) // wall-clock paced
defer r.Close()

r.AddSandbox("sb1", model.NewSandbox(podSandbox, podSandboxStatus))
r.AddContainer("c1", model.NewContainer(container, containerStatus))
r.Advance(...) // only needed with a manual clock, see below
```

Two clock modes:

- **Wall-clock paced** — `Options.TickRate > 0` starts a goroutine that
  advances the clock one tick per TickRate. Used by the CRI server
  (`cri.go`). `Close()` stops it.
- **Manual** — `TickRate == 0` (the default for tests): the clock moves
  only when you call `Advance(n)` / `Step()` / `FastForward(n)`. Every test
  in the package uses this for determinism.

`Now()` returns the current simulated time in ticks. One tick is
`Options.TickDuration` (default 100ms) of simulated wall time, used to
derive timestamps and byte rates.

## Operations: Do, Submit, Schedule

There are three ways to mutate the model. **The distinction is
important — using the wrong one deadlocks.**

| API | When it runs | Runtime lock | May call public API |
|-----|--------------|--------------|---------------------|
| `Do(muts...)` | immediately, in the caller | held (atomic) | **no** |
| `Submit(delay, muts...)` | after `delay` ticks | held (atomic) | **no** |
| `Schedule(delay, fn)` | after `delay` ticks | released | **yes** |

- `Do` / `Submit` run `Mutation`s (`func(*Runtime) error`) **with the
  runtime lock held**, so the commit is atomic — but the mutation must not
  call back into public methods like `AddContainer` or `GetContainer`
  (it would deadlock). Use them for package-internal state changes, e.g.
  `Do(SetContainerSpec(id, spec))`.
- `Schedule` runs a plain callback **outside** the lock on the simulation
  driver. Callbacks may use the public API and may schedule further
  events. This is what the CRI layer uses for transitions
  (sandbox → READY, container → RUNNING) and for `PullImage`.

All three return a `*Future` with:

- `Done() <-chan struct{}` — closes when the event ran (or was canceled).
- `Wait(ctx) error` — blocks until the event runs; returns `ctx.Err()` on
  cancellation.
- `Cancel() error` — removes the event if it has not started.

Historical note: the old blocking `Enqueue` still exists, deprecated.
It blocks the caller until the callback runs — do not use it in new code.

### Rule of thumb

```
Need an atomic internal state change?        Do / Submit (no public API)
Need a delayed callback that reads the model? Schedule
```

## Options and override functions

```go
opts := model.Options{
    TickRate:   100 * time.Millisecond,
    TickDuration: 100 * time.Millisecond,
    Delays:     model.DelayConfig{ SandboxStart: 10, ... }, // default: 10 each
    Hooks: model.Hooks{
        Load:     func(rt *model.Runtime, ref model.ObjectRef, now model.Ticks) float64 { ... },
        Duration: func(kind model.OpKind) model.Ticks { ... },
    },
    LoadIdle:       0.02,  // default
    LoadActive:     0.35,  // default
    LimitEnforcement: false, // default: OOM simulation off
}
r := model.New(opts)
```

Zero-valued fields take the documented defaults. `Duration(kind)` on the
runtime resolves the effective duration of an operation: the `Hooks.Duration`
override if set, else the `Delays` table.

### Hook: `Load` — resource utilization

`Load(rt, ref, now)` returns the utilization fraction of a container at a
simulated time. It is consulted once per accounting segment (the engine
segments every `Advance` at event boundaries), **outside the runtime
lock** — use the public API for reads and `Do` for writes.

Default: `LoadActive` while RUNNING, `LoadIdle` otherwise. The value drives
CPU, memory (working set ramps toward `limit * u`), page faults, and
network byte accumulation.

Examples:

```go
// A workload that ramps CPU up over the first 100 ticks, then plateaus.
Load: func(rt *model.Runtime, ref model.ObjectRef, now model.Ticks) float64 {
    ramp := float64(now) / 100
    if ramp > 1 { ramp = 1 }
    return 0.1 + 0.7*ramp
},

// Sinusoidal load (a background job with a 200-tick period).
Load: func(rt *model.Runtime, ref model.ObjectRef, now model.Ticks) float64 {
    return 0.5 + 0.4*math.Sin(2*math.Pi*float64(now)/200)
},
```

### Hook: `Duration` — operation timing

`Duration(kind)` overrides how long a simulated operation takes, in ticks.
The CRI layer resolves delays through this (e.g. `PullImage` waits
`Duration(OpImagePull)` ticks).

Examples:

```go
// Make image pulls instant for tests.
Duration: func(kind model.OpKind) model.Ticks {
    if kind == model.OpImagePull { return 1 }
    return 0 // fall back to the Delays table
},
```

### OOM simulation

With `LimitEnforcement: true`, a RUNNING container whose `Load` returns
`>= 1.0` is transitioned to EXITED with reason `OOMKilled` and exit code
137, and a state-change event is emitted. Off by default; the default load
profile never exceeds 0.35, so you must supply a `Load` hook to trigger it:

```go
Load: func(rt *model.Runtime, ref model.ObjectRef, now model.Ticks) float64 {
    return 2.0 // over the limit: OOM when enforcement is on
},
```

## Resource profiles

Containers carry a `ResourceSpec` (CPU millicores, memory bytes, network
rates). The default is `DefaultResourceSpec()` (100m CPU, 128MiB memory,
1Mib/s each direction). Assign a profile with the atomic mutation:

```go
if err := r.Do(model.SetContainerSpec("c1", model.ResourceSpec{
    CPUMillis: 500,
    MemBytes:  1 << 30,
    NetRxBps:  1 << 20,
    NetTxBps:  1 << 20,
})); err != nil { ... }
```

## Reading state and statistics

- Objects: `GetSandbox` / `GetContainer` / `GetImage` (and `List*`) return
  deep clones; mutate them and write back with `Update*`. The object proto
  is the single source of truth for `State`; the status proto is reconciled
  to match on every write, so the two can never disagree.
- Stats: `GetContainerStats`, `GetPodSandboxStats`, `List*Stats`
  materialize CRI protos from the internal accounting at request time.
  `PodSandboxMetrics(id)` returns the metric descriptors (`cpu/usage`,
  `cpu/usage_cores`, `memory/usage`, `memory/working_set`, `memory/rss`,
  `network/rx_bytes`, `network/tx_bytes`; names are provisional).

## Events

`Subscribe()` returns a `*Subscription` whose `Events()` channel delivers
typed events in commit order: sandbox/container/image created, removed,
and state changed (with From/To and an optional Reason such as
`OOMKilled`). Delivery is non-blocking with a bounded buffer (128): a
consumer that does not drain causes events to be **dropped** — the engine
never blocks. Close the subscription (or the runtime) to stop delivery.

```go
sub := r.Subscribe()
defer sub.Close()
for ev := range sub.Events() {
    switch ev.Kind {
    case model.EventContainerStateChanged:
        log.Printf("container %s: %v -> %v", ev.ID, ev.FromC, ev.ToC)
    }
}
```

The CRI layer maps these to `GetContainerEvents` (container lifecycle
events) and the metrics RPCs are served from the same accounting.

## Thread safety

- All public methods are safe for concurrent use.
- Hooks run on the simulation driver outside the runtime lock: reads via
  the public API are safe; for atomic writes use `Do` from within the hook.
- `Do`/`Submit` mutations run with the lock held and must not call public
  methods (see the operations table above).
- `Close()` stops the wall-clock adapter and closes all subscriptions; the
  runtime remains usable with a manual clock.
