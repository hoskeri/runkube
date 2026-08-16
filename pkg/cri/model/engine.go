package model

import (
	"container/heap"
	"context"
	"errors"
	"time"
)

// Ticks is the unit of simulated time. The simulation clock advances in
// whole ticks; one tick corresponds to tickRate of wall-clock time when the
// wall-clock adapter is running (see NewRuntime). All delays inside the
// model are expressed in ticks.
type Ticks uint64

// Sentinel errors for futures.
var (
	// ErrCanceled reports that a future's event was canceled before it ran.
	ErrCanceled = errors.New("model: future canceled")
	// ErrAlreadyCompleted reports that a future's event already ran (or is
	// currently being dispatched).
	ErrAlreadyCompleted = errors.New("model: future already completed")
)

// Mutation is a single state change applied by the engine. Mutations passed
// to Do (or submitted with Submit) run with the runtime lock held, so they
// must not call back into public Runtime methods — they may use the
// package's internal helpers only. Use Schedule for callbacks that need the
// public API.
type Mutation func(*Runtime) error

// Future is a handle to an event scheduled with Submit or Schedule.
type Future struct {
	rt *Runtime
	ev *scheduledEvent
}

// Done returns a channel that closes when the event has run or was
// canceled.
func (f *Future) Done() <-chan struct{} { return f.ev.done }

// Wait blocks until the event runs (or is canceled), or until ctx is done.
// It returns the event's error, or ctx.Err().
func (f *Future) Wait(ctx context.Context) error {
	select {
	case <-f.ev.done:
		return f.ev.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Cancel removes the event from the queue if it has not started running.
// Once an event is being dispatched it completes normally; Cancel then
// returns ErrAlreadyCompleted. Waiters on a canceled event observe
// ErrCanceled.
func (f *Future) Cancel() error {
	r := f.rt
	r.mu.Lock()
	defer r.mu.Unlock()
	ev := f.ev
	if ev.completed || ev.index < 0 {
		return ErrAlreadyCompleted
	}
	heap.Remove(&r.queue, ev.index)
	ev.index = -1
	ev.err = ErrCanceled
	ev.completed = true
	close(ev.done)
	return nil
}

// Do applies mutations immediately and atomically. The runtime lock is held
// for the whole list, so no other model activity (including scheduled
// events) can interleave. Mutations must not call back into public Runtime
// methods.
func (r *Runtime) Do(muts ...Mutation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range muts {
		if err := m(r); err != nil {
			return err
		}
	}
	return nil
}

// Submit schedules mutations to be committed after delay ticks. The commit
// runs under the runtime lock exactly like Do, so it is atomic with respect
// to all other model activity.
func (r *Runtime) Submit(delay Ticks, muts ...Mutation) *Future {
	r.mu.Lock()
	defer r.mu.Unlock()
	ev := &scheduledEvent{
		due:   r.now + delay,
		order: r.nextOrder(),
		muts:  muts,
		done:  make(chan struct{}),
	}
	heap.Push(&r.queue, ev)
	return &Future{rt: r, ev: ev}
}

// Schedule runs fn on the simulation driver after delay ticks, outside the
// runtime lock. fn may use the public Runtime API and may schedule further
// events; events due at or before the current advance's target run within
// the same Advance. Schedule is the asynchronous replacement for Enqueue.
func (r *Runtime) Schedule(delay Ticks, fn func(rt *Runtime)) *Future {
	r.mu.Lock()
	defer r.mu.Unlock()
	ev := &scheduledEvent{
		due:   r.now + delay,
		order: r.nextOrder(),
		fn:    fn,
		done:  make(chan struct{}),
	}
	heap.Push(&r.queue, ev)
	return &Future{rt: r, ev: ev}
}

// Advance advances the simulation clock by n ticks, dispatching every event
// due at or before the new time. Events are dispatched in (due, order)
// sequence. Resource accounting is applied per segment between dispatches,
// so a state change caused by an event (e.g. a container starting) is
// reflected in the accounting of subsequent segments. Advance is the only
// way the clock moves when the wall-clock adapter is not running
// (Options.TickRate == 0); tests drive the clock with it for determinism.
func (r *Runtime) Advance(n Ticks) {
	if n == 0 {
		return
	}
	r.mu.Lock()
	target := r.now + n
	for {
		if r.queue.Len() > 0 && r.queue[0].due <= target {
			next := r.queue[0].due
			if next > r.now {
				r.now = next
				r.mu.Unlock()
				r.account(next - r.now)
				r.mu.Lock()
			}
			ev := heap.Pop(&r.queue).(*scheduledEvent)
			ev.index = -1
			r.mu.Unlock()
			r.dispatch(ev)
			r.mu.Lock()
			continue
		}
		break
	}
	if target > r.now {
		delta := target - r.now
		r.now = target
		r.mu.Unlock()
		r.account(delta)
		r.mu.Lock()
	}
	r.mu.Unlock()
}

// Step advances the clock by one tick.
func (r *Runtime) Step() { r.Advance(1) }

// FastForward advances the clock by n ticks. When no events are scheduled
// this is O(1); with events it dispatches everything due by the new time.
func (r *Runtime) FastForward(n Ticks) { r.Advance(n) }

// Now returns the current simulated time in ticks.
func (r *Runtime) Now() Ticks {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

// Close stops the wall-clock adapter if one is running and closes all
// event subscriptions. The runtime remains usable with a manually driven
// clock. Close is idempotent.
func (r *Runtime) Close() {
	r.closeOnce.Do(func() {
		if r.stopCh != nil {
			close(r.stopCh)
		}
		r.mu.Lock()
		for id, s := range r.subs {
			delete(r.subs, id)
			close(s.ch)
		}
		r.mu.Unlock()
	})
}

// dispatch runs a single popped event to completion. Schedule callbacks run
// outside the lock (they may use the public API); Submit mutations run under
// the lock (atomic commit).
func (r *Runtime) dispatch(ev *scheduledEvent) {
	if ev.fn != nil {
		ev.fn(r)
		r.mu.Lock()
		ev.completed = true
		close(ev.done)
		r.mu.Unlock()
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range ev.muts {
		if err := m(r); err != nil {
			ev.err = err
			break
		}
	}
	ev.completed = true
	close(ev.done)
}

func (r *Runtime) nextOrder() uint64 {
	r.order++
	return r.order
}

// startWallClock runs the wall-clock adapter: Advance(1) every TickRate.
func (r *Runtime) startWallClock() {
	r.stopCh = make(chan struct{})
	go func() {
		ticker := time.NewTicker(r.opts.TickRate)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r.Advance(1)
			case <-r.stopCh:
				return
			}
		}
	}()
}

// scheduledEvent is a queued simulation event.
type scheduledEvent struct {
	due   Ticks
	order uint64
	muts  []Mutation
	fn    func(rt *Runtime)
	done  chan struct{}

	index     int
	completed bool
	err       error
}

// eventHeap is a priority queue of scheduled events ordered by (due, order).
type eventHeap []*scheduledEvent

func (h eventHeap) Len() int { return len(h) }
func (h eventHeap) Less(i, j int) bool {
	if h[i].due != h[j].due {
		return h[i].due < h[j].due
	}
	return h[i].order < h[j].order
}
func (h eventHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}
func (h *eventHeap) Push(x any) {
	n := len(*h)
	ev := x.(*scheduledEvent)
	ev.index = n
	*h = append(*h, ev)
}
func (h *eventHeap) Pop() any {
	old := *h
	n := len(old)
	ev := old[n-1]
	old[n-1] = nil
	ev.index = -1
	*h = old[:n-1]
	return ev
}
