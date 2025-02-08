# Simulated Container Runtime Architecture and Model Design

This document describes the design, implementation, and capabilities of the
encapsulated simulated Kubernetes container runtime model in the
`pkg/cri/model` package.

## 1. Architectural Principles

The simulated container runtime is designed around **strong encapsulation**,
**thread safety**, **immutability/cloning**, and **event-driven simulation**.
The package abstracts all internal storage and lifecycle states so that
external callers (such as `pkg/cri/cri.go`) interact strictly through clean
public interfaces.

## 2. Models and Encapsulation

All primary models in `pkg/cri/model` enforce complete field encapsulation:
* **`Sandbox`**: Wraps `criv1.PodSandbox` and `criv1.PodSandboxStatus`.
* **`Container`**: Wraps `criv1.Container` and `criv1.ContainerStatus`.
* **`Image`**: Wraps `criv1.ImageSpec`.

### Stats as Properties of Respective Objects

Following object-oriented domain modeling principles, **statistics are
represented directly as properties of their respective parent objects** rather
than being tracked in disjoint collections:
* `Sandbox` holds a private `stats *criv1.PodSandboxStats` property, accessible
  via `PodSandboxStats()` and `SetPodSandboxStats()`.
* `Container` holds a private `stats *criv1.ContainerStats` property,
  accessible via `ContainerStats()` and `SetContainerStats()`.

This ensures stats automatically share the lifecycle and identification
attributes of their parent objects.

### Immutability & Corruption Prevention

To prevent concurrent modification and state corruption, **all model structures
support deep copy cloning**:

* Every model type provides a `Clone()` method that duplicates internal
  protobuf fields using `proto.Clone`.
* `Runtime` methods clone incoming objects upon `Add*`/`Update*` operations and
  clone outgoing objects during `Get*`/`List*` queries. This guarantees that
  mutated copies outside the model package cannot corrupt the internal store.

## 3. Runtime API Design

The `model.Runtime` struct represents the active simulated container runtime
state. It completely encapsulates state maps and synchronization, exposing
state manipulation exclusively via standard public operations:

* **Unified API Design**: `Runtime` exposes `Add*`, `Remove*`, `Update*`,
  `Get*`, and `List*` methods for Sandboxes, Containers, and Images.
* **Get as a Special Case of List**: To ensure maximum code reuse and
  consistent filtering/cloning, all `Get*` APIs are implemented internally as
  special cases of `List*` (delegating with matching filters):

  ```go
  func (r *Runtime) GetSandbox(id string) (*Sandbox, bool) {
      list := r.ListSandboxes(&criv1.PodSandboxFilter{Id: id})
      if len(list) == 0 {
          return nil, false
      }
      return list[0], true
  }
  ```

## 4. Model-Wide Discrete Event Simulation Queue

The discrete event simulation mechanism is built directly as an internal
property of `model.Runtime`, modeling asynchronous operations (e.g. image
pulling, sandbox starting, container launching) as deterministic sequences of
ticks.

### The Event Queue

* **Event Structure**:

  ```go
    type Event struct {
        Sequence uint64
        Callback func(r *Runtime)
    }
  ```

* **Active Queue**: `events []Event` tracks scheduled simulation tasks.
* **Sequence Counter**: `seq uint64` tracks the elapsed simulation time
  (ticks).
* **Tick Rate**: `tickRate time.Duration` determines the real-world duration of
  a single simulation tick (controlling simulation speed).

### Execution Mechanism

1. **Background Goroutine**: A package-internal background goroutine ticks at
   the defined `tickRate` interval.
2. **Tick Progression**: Every tick, the goroutine locks `Runtime`, increments
   `seq`, collects all scheduled events whose target `Sequence <= seq`, updates
   the active queue to remove them, and releases the lock.
3. **Safe Dispatch**: Collected event callbacks are dispatched **outside of the
   internal lock** (`r.mu`). This ensures that event callbacks can freely lock
   `Runtime` to read states, update models, or schedule subsequent events
   without deadlocking the server.
