# Watch support for pkg/persist (bbolt-backed object store)

## Background

The `persist` package (`pkg/persist/persist.go`) provides a bbolt-backed
Kubernetes-style object store with:

- `DB.Open/Create/Update/Get/Query/Single` — standard CRUD plus prefix-range
  queries via `Matcher`
- `Reference` — fixed 7-part key: `realm/scope/group/version/resource/namespace/name`
  separated by `\x00`
- `Matcher` — query builder with options for namespace, name, UID, resource
  version equality/minimum, and realm
- Resource version auto-increment via `bbolt.Tx.Bucket.NextSequence()`
- CBOR serialization via `k8s.io/apimachinery/pkg/runtime/serializer/cbor`

**What's missing:** a `Watch` method on `DB` that returns a
`k8s.io/apimachinery/pkg/watch.Interface` — the natural backend for
`kapi.Lister.Watch()`, which the `kapi` package already has full HTTP wire
support for (chunked JSON lines, `?watch=true` dispatch, `serveWatch`).

---

## Design constraints

1. **bbolt permits only one read-write transaction at a time.** A snapshot
   scan (list) in a read-only tx is safe, but to observe subsequent mutations
   we need to attach after the write tx commits. bbolt v1.5.0 offers
   `Tx.OnCommit()` — a callback invoked synchronously inside the write tx
   after the data is written but before the unlock. This is the natural hook.

2. **Resource version is monotonic but not contiguous.** `NextSequence()`
   increments atomically per write tx. Since `NextSequence()` is per-bucket
   and we're using a single `sticks.objects` bucket, RV ordering is total.
   But there is no "notify at exact RV" guarantee — multiple write txs could
   commit between two `Watch` reads; a watcher that joins after the fact
   still needs a bookmark scan to catch up.

3. **Watch must start with a bookmark (existing objects matching the filter),
   then stream real-time events.** This matches Kubernetes watch semantics:
   initial list of existing objects + incremental changes. The "bookmark"
   also handles the case where a mutation occurs between the initial scan and
   the subscription hook insertion.

4. **Event ordering matters.** bbolt serializes write txs, so `OnCommit`
   callbacks fire in that order. The watch channel must preserve this order.

---

## Proposed implementation: `pkg/persist/watcher.go`

### High-level architecture

```
┌─────────────────────┐         ┌───────────────┐
│    bbolt Write Tx    │ OnCommit(writeTx)    Event log
│    Create/Update     │ ───────→  forEach watcher
│    Delete            │            chan<- Event
│                     │         └───────────────┘
└─────────────────────┘
                              ┌───────────────────────┐
                              │  Watcher struct         │
                              │  · mu                   │
                              │  · watchers map[int64]  │
                              │    chan watch.Event     │
                              │  · nextID int64         │
                              │  · DB ref               │
                              └───────────────────────┘
```

### Key types

```go
// Watcher manages active watch subscriptions and receives notifications
// from DB write transactions.
//
// Each subscription is a goroutine that:
//   1. Scans existing objects matching the filter (bookmark scan)
//   2. Sends those as watch.Added events
//   3. Switches to streaming mode: reads from a shared notification chan
//      and filters against the subscription's Matcher
//   4. Closes the result chan on timeout / context cancellation / Stop()
type Watcher struct {
    db       *DB
    registry *watchRegistry
}

// watchRegistry holds active subscriptions and is the target of OnCommit
// callbacks registered on each write transaction.
type watchRegistry struct {
    mu        sync.Mutex
    watchers  map[int64]*subscription
    nextID    int64
}

type subscription struct {
    id      int64
    matcher *Matcher        // pre-built; events failing Matcher.Matches != 0 → skip
    result  chan watch.Event
    done    chan struct{}    // closed by watcher.Stop()
    rvMin   uint64          // 0 → no bookmark filter; >0 → skip events < rvMin
    timeout time.Duration   // 0 → no timeout; >0 → close after this duration
}
```

### Wire path through `DB`

```go
// In DB (persist.go additions):

// Watch returns a named watcher attached to this DB. Every write tx after
// this call will notify the returned subscription until Stop() is called
// or the context is cancelled.
//
// The watcher first performs a bookmark scan (all matching objects sent as
// watch.Added), then streams real-time changes (Added/Modified/Deleted).
func (p *DB) Watch(ctx context.Context, m *Matcher, opts WatchOptions) (watch.Interface, error)
```

Every `DB.Create` / `DB.Update` / `DB.Delete` must call `p.registry.notify()`
inside or after the write transaction. The simplest approach:

1. **Add a `watcher *Watcher` field to `DB`**, lazily initialized on first
   `DB.Watch()` call.
2. **Wrap each write path** (`txPut`, `txDelete`) to call `tx.OnCommit()`
   with a closure that extracts the changed `Reference` + event type + new
   object and fans it out to all active subscriptions.

### Bookmark scan

The subscription goroutine runs a `DB.Query(m)` to get all existing refs,
then walks each ref through `DB.Get(ref)`, sends as `watch.Added` (with the
current RV), then enters the real-time loop.

Race: a write tx commits between the bookmark scan and the subscription
being registered in the registry. **Solution:** register the subscription in
the registry *before* the bookmark scan. The notification chan buffers at
least 1; events that arrive during the bookmark scan will be ordered after
the Added events. Use `WithMinResourceVersion(rvAfterBookmark+1)` on the
subscription's internal filter to avoid double-sending an object that was
both bookmarked and then modified.

### Delete support

The current `persist` package has no `Delete` method. **For full watch
support, add `DB.Delete(k *Reference) error`**. The delete path:
- Fetch the object before deleting (to include in the `watch.Deleted` event)
- Call `tx.DeleteBucket(bucket, k.key)` (or `bbolt.Bucket.Delete()`)
- Register `tx.OnCommit` → notify with `watch.Deleted`

### Event filtering

Each subscription holds a `*Matcher`. On each incoming notification, the
subscription calls `m.Matches(ref)` — the existing `Matcher` already returns
`0` for a match, positive for the reason-for-non-match. Fast path: if the
matcher has no namespace/name/realm filters, match all (`code == 0` for
everything within the GVR scope).

### WatchOptions

```go
type WatchOptions struct {
    Timeout time.Duration  // 0 means no timeout (Kubernetes default: 30m with periodic resync)
    RV      uint64         // 0 means "from the start"; >0 means "skip events at or below this RV"
}
```

### Integration with `kapi.Lister`

A `Resource` backed by `persist.DB` would wire:

```go
resource := &kapi.Resource{
    Lister: &persistBackedLister{db: db, matcher: baseMatcher},
    ...
}

type persistBackedLister struct {
    db      *persist.DB
    matcher *persist.Matcher
}

func (l *persistBackedLister) List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
    refs, err := l.db.Query(l.matcher)
    // ... fetch each ref, build UnstructuredList
}

func (l *persistBackedLister) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
    m := l.matcher // optionally add namespace/name from opts
    return l.db.Watch(ctx, m, persist.WatchOptions{
        Timeout: /* from opts.TimeoutSeconds or default */,
        RV:      opts.ResourceVersion, // parse as uint64
    })
}
```

### Backpressure and channel sizing

- Notification channel per subscription: capacity 100 (or configurable).
- If a subscription's channel is full, drop the oldest event
  (non-blocking send with `select + default`). This mirrors the real
  Kubernetes behaviour: slow watchers miss events and must re-list.
- The `OnCommit` callback runs inside the write tx lock — it must be fast
  (µs-scale). It does only the fan-out send attempt; filtering and channel
  sends happen in the subscription goroutine.

### Lifecycle

```
Watcher creation:
  db.Watch() → registers subscription in registry → starts goroutine

Goroutine:
  1. Bookmark scan (Query + Get for each ref → Added events)
  2. Loop: select on notification chan / context.Done / timeout
     - notification chan → filter via Matcher → send to result chan
     - context.Done / timeout → close result chan → cleanup subscription

Watcher teardown:
  Stop() closes subscription.done → goroutine exits → result chan closed
  Or: context cancellation / timeout → same path
```

---

## Implementation steps

### Phase 1 — Foundation (new `pkg/persist/watcher.go`)

1. Add `Watcher` type with `watchRegistry` and subscription management.
2. Add `WatchOptions` type.
3. Add `DB.Watch(ctx, m, opts) (watch.Interface, error)` that sets up
   `p.watcher` lazily, creates a subscription, registers it, and starts
   the bookmark+streaming goroutine.
4. Wire `tx.OnCommit()` in `DB.txPut()` and (new) `DB.txDelete()` to call
   `p.watcher.registry.notify()`.
5. Add `DB.Delete(k *Reference) error` with `tx.OnCommit` notification.

### Phase 2 — Filtering and streaming

6. Implement the streaming loop in the subscription goroutine.
7. Implement `Matcher.Matches(ref)` fast path (already exists).
8. Handle resource version filtering (`rvMin` bookmark).

### Phase 3 — Integration test

9. Write an end-to-end test: open DB, start Watch, Create an object, verify
   Added event arrives on the watch channel, Update → Modified, Delete →
   Deleted.
10. Write a multiple-watcher test: two watchers with different matchers;
    verify each receives only matching events.
11. Write a backpressure test: slow consumer, verify events are dropped
    (oldest-first) but the DB remains consistent.

### Phase 4 — kapi integration

12. Build `persistBackedLister` / `persistBackedResource` as a bridge
    between `persist.DB` and `kapi.Resource` (maybe in `pkg/kapi/persist.go`).
13. Verify `?watch=true` against a live `kapi.Service` with a real
    `persist.DB` backend.
14. Test that the HTTP response stream respects client disconnection
    (already handled by `serveWatch` via `ctx.Done()`).

---

## Alternatives considered

### A. Polling goroutine (every N seconds, re-Query)

Rejected. bbolt has no built-in change notification, so polling would be the
only option without `OnCommit`. But `OnCommit` exists in bbolt v1.5.0 and is
strictly better: zero latency between write and notification, no wasted
re-scans, and perfect ordering.

### B. Channel-per-write-tx (one goroutine per commit)

Rejected. The subscription goroutine pattern is cleaner: one goroutine per
watcher, multiplexed over a shared notification fan-in. `OnCommit` callbacks
are synchronous in the write tx — they must be fast. The fan-out to each
subscription's channel is a non-blocking send in a loop over the registry;
the filtering happens in the subscription goroutine, not in the write callback.

### C. k8s.io/apiserver's `StorageDecorator` pattern

Rejected for the same reason `kapi` exists: `k8s.io/apiserver` pulls in
hundreds of dependencies and an opinionated etcd-centric storage abstraction.
`persist` is the minimal bbolt backend; the watch layer should be equally
minimal and self-contained.