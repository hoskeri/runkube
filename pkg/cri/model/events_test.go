package model

import (
	"testing"
	"time"

	criv1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func idFor(i int) string { return "c" + string(rune('0'+i/26%10)) + string(rune('a'+i%26)) }

func nextEvent(t *testing.T, sub *Subscription) Event {
	t.Helper()
	select {
	case ev := <-sub.Events():
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

func TestSubscribeReceivesLifecycle(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	sub := r.Subscribe()
	defer sub.Close()

	r.AddContainer("c1", runningContainer("c1", "sb1"))
	ev := nextEvent(t, sub)
	if ev.Kind != EventContainerCreated || ev.ID != "c1" || ev.Sandbox != "sb1" {
		t.Fatalf("created event = %+v", ev)
	}

	c, _ := r.GetContainer("c1")
	c.Container().State = criv1.ContainerState_CONTAINER_EXITED
	r.UpdateContainer("c1", c)
	ev = nextEvent(t, sub)
	if ev.Kind != EventContainerStateChanged || ev.FromC != criv1.ContainerState_CONTAINER_RUNNING || ev.ToC != criv1.ContainerState_CONTAINER_EXITED {
		t.Fatalf("state change event = %+v", ev)
	}

	r.RemoveContainer("c1")
	ev = nextEvent(t, sub)
	if ev.Kind != EventContainerRemoved || ev.ID != "c1" {
		t.Fatalf("removed event = %+v", ev)
	}
}

func TestSandboxAndImageEvents(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	sub := r.Subscribe()
	defer sub.Close()

	r.AddSandbox("sb1", NewSandbox(
		&criv1.PodSandbox{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_NOTREADY},
		&criv1.PodSandboxStatus{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_NOTREADY},
	))
	if ev := nextEvent(t, sub); ev.Kind != EventSandboxCreated || ev.ID != "sb1" {
		t.Fatalf("sandbox created event = %+v", ev)
	}

	sb, _ := r.GetSandbox("sb1")
	sb.PodSandbox().State = criv1.PodSandboxState_SANDBOX_READY
	r.UpdateSandbox("sb1", sb)
	if ev := nextEvent(t, sub); ev.Kind != EventSandboxStateChanged || ev.ToSB != criv1.PodSandboxState_SANDBOX_READY {
		t.Fatalf("sandbox state event = %+v", ev)
	}

	r.AddImage("nginx", NewImage(&criv1.ImageSpec{Image: "nginx"}))
	if ev := nextEvent(t, sub); ev.Kind != EventImagePulled || ev.ID != "nginx" {
		t.Fatalf("image pulled event = %+v", ev)
	}
	r.RemoveImage("nginx")
	if ev := nextEvent(t, sub); ev.Kind != EventImageRemoved {
		t.Fatalf("image removed event = %+v", ev)
	}
}

func TestMultipleSubscribers(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	s1 := r.Subscribe()
	defer s1.Close()
	s2 := r.Subscribe()
	defer s2.Close()

	r.AddContainer("c1", runningContainer("c1", "sb1"))
	if ev := nextEvent(t, s1); ev.ID != "c1" {
		t.Fatalf("subscriber 1 event = %+v", ev)
	}
	if ev := nextEvent(t, s2); ev.ID != "c1" {
		t.Fatalf("subscriber 2 event = %+v", ev)
	}
}

func TestCloseStopsDelivery(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	sub := r.Subscribe()
	r.AddContainer("c1", runningContainer("c1", "sb1"))
	nextEvent(t, sub)
	sub.Close()
	r.AddContainer("c2", runningContainer("c2", "sb1"))
	select {
	case ev, ok := <-sub.Events():
		if ok {
			t.Fatalf("event after Close: %+v", ev)
		}
		// closed channel: expected.
	case <-time.After(100 * time.Millisecond):
	}
}

func TestEventsInCommitOrder(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	sub := r.Subscribe()
	defer sub.Close()

	r.AddContainer("c1", runningContainer("c1", "sb1"))
	r.AddContainer("c2", runningContainer("c2", "sb1"))
	r.RemoveContainer("c1")

	var kinds []EventKind
	var ids []string
	for i := 0; i < 3; i++ {
		ev := nextEvent(t, sub)
		kinds = append(kinds, ev.Kind)
		ids = append(ids, ev.ID)
	}
	wantKinds := []EventKind{EventContainerCreated, EventContainerCreated, EventContainerRemoved}
	wantIDs := []string{"c1", "c2", "c1"}
	for i := range wantKinds {
		if kinds[i] != wantKinds[i] || ids[i] != wantIDs[i] {
			t.Fatalf("order mismatch at %d: %v %v", i, kinds, ids)
		}
	}
}

// TestSlowConsumerDrops: the engine must never block on a subscriber that
// does not drain; excess events are dropped.
func TestSlowConsumerDrops(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	sub := r.Subscribe()
	defer sub.Close()

	total := subscriptionBuffer + 50
	for i := 0; i < total; i++ {
		r.AddContainer(idFor(i), runningContainer(idFor(i), "sb1"))
	}
	// Reading drains only what fit in the buffer.
	got := 0
	for {
		select {
		case <-sub.Events():
			got++
			if got > subscriptionBuffer {
				t.Fatalf("received %d events, buffer is %d", got, subscriptionBuffer)
			}
		case <-time.After(100 * time.Millisecond):
			if got != subscriptionBuffer {
				t.Fatalf("received %d events, want exactly %d (drop policy)", got, subscriptionBuffer)
			}
			return
		}
	}
}

func TestRuntimeCloseClosesSubscriptions(t *testing.T) {
	r := NewRuntime(0)
	sub := r.Subscribe()
	r.AddContainer("c1", runningContainer("c1", "sb1"))
	nextEvent(t, sub)
	r.Close()
	select {
	case _, ok := <-sub.Events():
		if ok {
			t.Fatal("subscription must be closed after runtime Close")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("subscription channel must close after runtime Close")
	}
}

func TestOOMEmitsStateChangedEvent(t *testing.T) {
	r := New(Options{
		Hooks: Hooks{
			Load: func(rt *Runtime, ref ObjectRef, now Ticks) float64 { return 2.0 },
		},
		LimitEnforcement: true,
	})
	defer r.Close()
	sub := r.Subscribe()
	defer sub.Close()

	r.AddContainer("c1", runningContainer("c1", "sb1"))
	nextEvent(t, sub) // created
	r.Advance(5)
	ev := nextEvent(t, sub)
	if ev.Kind != EventContainerStateChanged || ev.ToC != criv1.ContainerState_CONTAINER_EXITED || ev.Reason != "OOMKilled" {
		t.Fatalf("oom event = %+v", ev)
	}
}
