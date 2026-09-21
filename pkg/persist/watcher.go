package persist

import (
	"bytes"
	"context"
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
)

// ── Watcher ────────────────────────────────────────────────

// WatchOptions controls the behaviour of a watch subscription.
type WatchOptions struct{}

// Watcher manages active watch subscriptions and notifies them when
// the DB is mutated. Created lazily by DB on the first Watch call.
type Watcher struct {
	mu      sync.Mutex
	nextID  int64
	subs    map[int64]*sub
}

type watchEvent struct {
	key      []byte
	typ      watch.EventType
	staleObj runtime.Object // set only for Deleted (pre-deletion snapshot)
}

type sub struct {
	id       int64
	matcher  *Matcher
	events   chan watchEvent // receives from write-tx OnCommit
	result   chan watch.Event
	stop     chan struct{} // closed by Stop()
	stopped  chan struct{} // closed when goroutine exits
}

// NewWatcher creates an empty Watcher.
func NewWatcher() *Watcher {
	return &Watcher{
		subs: make(map[int64]*sub),
	}
}

// subscribe registers a new subscription and returns it.
func (w *Watcher) subscribe(m *Matcher, _ WatchOptions) *sub {
	w.mu.Lock()
	defer w.mu.Unlock()
	id := w.nextID
	w.nextID++
	s := &sub{
		id:      id,
		matcher: m,
		events:  make(chan watchEvent, 100),
		result:  make(chan watch.Event, 100),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	w.subs[id] = s
	return s
}

// unsubscribe removes a subscription so no more events are fanned out to it.
func (w *Watcher) unsubscribe(id int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.subs, id)
}

// fanOut is called synchronously inside the write tx's OnCommit callback.
// It must be fast (µs-scale) — it does a non-blocking send to each
// subscription's events channel. Slow consumers will miss events.
func (w *Watcher) fanOut(evt watchEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, s := range w.subs {
		select {
		case s.events <- evt:
		default:
			// drop — slow consumer
		}
	}
}

// ── DB.Watch ──────────────────────────────────────────────

// Watch returns a watch.Interface that streams mutations matching the
// given matcher. It first performs a bookmark scan (existing matching
// objects sent as Added), then streams real-time changes.
//
// Call Stop() on the returned watcher to tear down the subscription.
func (p *DB) Watch(ctx context.Context, m *Matcher, opts WatchOptions) (watch.Interface, error) {
	w := p.getWatcher()
	s := w.subscribe(m, opts)

	// Start the streaming goroutine.
	resultCh := s.result
	eventsCh := s.events
	stopCh := s.stop
	stoppedCh := s.stopped
	subID := s.id

	go func() {
		defer close(resultCh)
		defer close(stoppedCh)
		defer w.unsubscribe(subID)

		// ── Bookmark scan ──────────────────────────────────────
		//
		// We're already subscribed (so real-time events won't be
		// missed), but we must also send the current state.
		refs, err := p.Query(m)
		if err == nil {
			for _, ref := range refs {
				obj, err := p.Get(ref)
				if err != nil {
					continue
				}
				// Non-blocking send: if the result channel is full,
				// drop the bookmark event. The caller can re-list.
				select {
				case resultCh <- watch.Event{Type: watch.Added, Object: obj}:
				default:
				}
			}
		}

		// ── Streaming loop ─────────────────────────────────────
		for {
			select {
			case evt, ok := <-eventsCh:
				if !ok {
					return
				}
				// Build a reference to check against the matcher.
				ref := &Reference{key: evt.key}
				// Re-parse the key to populate raw[]. We need
				// raw to call Matcher.Matches.
				if c := bytes.Split(evt.key, refSeparator); len(c) == 8 {
					ref.raw = c[0:7]
				} else {
					continue // malformed key
				}

				if m.Matches(ref) != 0 {
					continue // not for us
				}

				var obj runtime.Object
				if evt.staleObj != nil {
					obj = evt.staleObj // Deleted — already captured
				} else {
					// Fetch the current version from the DB.
					obj, _ = p.Get(ref)
				}
				if obj == nil {
					continue
				}

				select {
				case resultCh <- watch.Event{Type: evt.typ, Object: obj}:
				default:
					// result channel full — drop
				}

			case <-stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	return &dbWatcher{
		result: resultCh,
		stop: func() {
			close(stopCh)
		},
	}, nil
}

// getWatcher lazily initialises the DB's Watcher.
func (p *DB) getWatcher() *Watcher {
	return p.getNotifier()
}

// ── dbWatcher ──────────────────────────────────────────────

// dbWatcher implements watch.Interface.
type dbWatcher struct {
	result chan watch.Event
	stop   func()
}

func (w *dbWatcher) ResultChan() <-chan watch.Event {
	return w.result
}

func (w *dbWatcher) Stop() {
	w.stop()
}

// pkg-level init isn't needed; the Watcher is created lazily.