package model

import (
	criv1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// subscriptionBuffer is the per-subscriber event queue depth. A subscriber
// that falls behind drops events (see Subscription); the engine never
// blocks on delivery.
const subscriptionBuffer = 128

// EventKind identifies the kind of a model event.
type EventKind int

const (
	EventSandboxCreated EventKind = iota
	EventSandboxRemoved
	EventSandboxStateChanged
	EventContainerCreated
	EventContainerRemoved
	EventContainerStateChanged
	EventImagePulled
	EventImageRemoved
)

// Event is a model mutation notification, delivered to subscribers in
// commit order. Fields are populated per kind: sandbox kinds fill FromSB /
// ToSB, container kinds fill FromC / ToC plus Sandbox (the owning sandbox),
// and image kinds carry only ID.
type Event struct {
	Kind EventKind
	ID   string
	// Sandbox is the owning sandbox id for container events.
	Sandbox string
	// FromSB/ToSB are set for sandbox state changes; FromC/ToC for
	// container state changes.
	FromSB criv1.PodSandboxState
	ToSB   criv1.PodSandboxState
	FromC  criv1.ContainerState
	ToC    criv1.ContainerState
	// Reason is set for abnormal transitions (e.g. OOMKilled).
	Reason string
	// At is the simulated time of the mutation.
	At Ticks
}

// Subscription is a live feed of model events. Events are delivered in
// commit order; a consumer that does not drain the buffer causes events to
// be dropped (the engine never blocks). Close releases the subscription.
type Subscription struct {
	rt *Runtime
	id uint64
	ch chan Event
}

// Events returns the event channel. It is closed when the subscription is
// closed or the runtime is closed.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Close unregisters the subscription. It is safe to call multiple times.
func (s *Subscription) Close() {
	s.rt.removeSub(s.id)
}

// Subscribe registers a new event subscription. Events are delivered
// asynchronously with a bounded buffer; see Subscription.
func (r *Runtime) Subscribe() *Subscription {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subSeq++
	sub := &Subscription{rt: r, id: r.subSeq, ch: make(chan Event, subscriptionBuffer)}
	r.subs[sub.id] = sub
	return sub
}

// removeSub unregisters and closes a subscription. It is idempotent.
func (r *Runtime) removeSub(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.subs[id]; ok {
		delete(r.subs, id)
		close(s.ch)
	}
}

// emit delivers an event to all subscribers without blocking: a subscriber
// whose buffer is full drops the event. Call with the runtime lock held.
func (r *Runtime) emit(ev Event) {
	for _, s := range r.subs {
		select {
		case s.ch <- ev:
		default:
			// slow consumer: drop.
		}
	}
}
