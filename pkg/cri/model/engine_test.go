package model

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDoAppliesImmediately(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	var got []string
	err := r.Do(
		func(*Runtime) error { got = append(got, "a"); return nil },
		func(*Runtime) error { got = append(got, "b"); return nil },
	)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("expected both mutations to run in order, got %v", got)
	}
	if r.Now() != 0 {
		t.Errorf("Do must not advance the clock; now = %d", r.Now())
	}
}

func TestDoStopsOnError(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	wantErr := errors.New("boom")
	var got []string
	err := r.Do(
		func(*Runtime) error { got = append(got, "a"); return nil },
		func(*Runtime) error { return wantErr },
		func(*Runtime) error { got = append(got, "c"); return nil },
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected %v, got %v", wantErr, err)
	}
	if len(got) != 1 {
		t.Errorf("mutations after the failing one must not run, got %v", got)
	}
}

func TestSubmitRunsAtDueTick(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	var ran []Ticks
	f := r.Submit(5, func(rt *Runtime) error { ran = append(ran, rt.now); return nil })
	r.Advance(4)
	if len(ran) != 0 {
		t.Fatalf("event must not run before its due tick, ran %v", ran)
	}
	r.Advance(1)
	if len(ran) != 1 || ran[0] != 5 {
		t.Fatalf("event must run at tick 5, got %v", ran)
	}
	select {
	case <-f.Done():
	default:
		t.Fatal("future must be done after its event ran")
	}
}

func TestFIFOAtEqualDue(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	var order []int
	for i := 0; i < 5; i++ {
		i := i
		r.Submit(3, func(*Runtime) error { order = append(order, i); return nil })
	}
	r.Advance(3)
	want := []int{0, 1, 2, 3, 4}
	if len(order) != len(want) {
		t.Fatalf("got %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestFastForwardEmptyClock(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	r.FastForward(1000)
	if r.Now() != 1000 {
		t.Fatalf("now = %d, want 1000", r.Now())
	}
}

func TestStep(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	r.Step()
	if r.Now() != 1 {
		t.Errorf("now = %d, want 1", r.Now())
	}
}

// TestDeterministicTrajectory runs the same scripted schedule twice and
// requires identical dispatch order.
func TestDeterministicTrajectory(t *testing.T) {
	run := func() []int {
		r := NewRuntime(0)
		defer r.Close()
		var out []int
		r.Schedule(2, func(rt *Runtime) {
			out = append(out, 2)
			rt.Submit(1, func(*Runtime) error { out = append(out, 3); return nil })
			rt.Schedule(0, func(*Runtime) { out = append(out, 4) })
		})
		r.Schedule(1, func(*Runtime) { out = append(out, 1) })
		r.Advance(10) // fires everything due by tick 10, including chained events
		return out
	}
	a, b := run(), run()
	want := []int{1, 2, 4, 3}
	if len(a) != len(want) {
		t.Fatalf("trajectory %v, want %v", a, want)
	}
	for i := range want {
		if a[i] != want[i] || b[i] != want[i] {
			t.Fatalf("trajectories differ at %d: %v vs %v", i, a, b)
		}
	}
}

func TestFutureWaitContext(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	f := r.Schedule(100, func(*Runtime) {})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := f.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
}

func TestFutureCancel(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	ran := false
	f := r.Schedule(5, func(*Runtime) { ran = true })
	if err := f.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	r.Advance(10)
	if ran {
		t.Error("canceled event must not run")
	}
	if err := f.Wait(context.Background()); !errors.Is(err, ErrCanceled) {
		t.Fatalf("expected ErrCanceled, got %v", err)
	}
	if err := f.Cancel(); !errors.Is(err, ErrAlreadyCompleted) {
		t.Fatalf("expected ErrAlreadyCompleted, got %v", err)
	}
}

func TestFutureCancelAfterRun(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	f := r.Schedule(1, func(*Runtime) {})
	r.Advance(1)
	if err := f.Cancel(); !errors.Is(err, ErrAlreadyCompleted) {
		t.Fatalf("expected ErrAlreadyCompleted, got %v", err)
	}
}

func TestScheduleDuringDispatch(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	var seq []int
	r.Schedule(1, func(rt *Runtime) {
		seq = append(seq, 1)
		rt.Schedule(0, func(*Runtime) { seq = append(seq, 2) })  // due == now, inside target
		rt.Schedule(10, func(*Runtime) { seq = append(seq, 3) }) // due beyond target
	})
	r.Advance(5)
	// The due-1 event and its chained due-1 event both run within the same
	// advance; the due-11 event waits.
	if len(seq) != 2 || seq[0] != 1 || seq[1] != 2 {
		t.Fatalf("got %v, want [1 2]", seq)
	}
	r.Advance(10)
	if len(seq) != 3 || seq[2] != 3 {
		t.Fatalf("got %v, want [1 2 3]", seq)
	}
}

func TestEnqueueCompat(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	done := make(chan struct{})
	go func() {
		ch := r.Enqueue(3, func(rt *Runtime) { close(done) })
		if ch != nil {
			t.Errorf("Enqueue must return a nil channel, got %v", ch)
		}
	}()
	select {
	case <-done:
		t.Fatal("Enqueue must block until its callback runs")
	case <-time.After(10 * time.Millisecond):
	}
	r.Advance(3)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Enqueue must unblock after its callback runs")
	}
}

func TestWallClockAdapter(t *testing.T) {
	r := NewRuntime(time.Millisecond)
	defer r.Close()
	deadline := time.Now().Add(100 * time.Millisecond)
	for r.Now() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if r.Now() == 0 {
		t.Fatal("wall-clock adapter must advance the clock")
	}
	r.Close()
	// Wait for any in-flight tick to land, then require the clock to freeze.
	deadline = time.Now().Add(100 * time.Millisecond)
	stopped := r.Now()
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		if r.Now() == stopped {
			break
		}
		stopped = r.Now()
	}
	time.Sleep(20 * time.Millisecond)
	if r.Now() != stopped {
		t.Errorf("clock must stop advancing after Close: %d -> %d", stopped, r.Now())
	}
}

func TestSubmitMutationsRunInOrder(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	var got []int
	r.Submit(2,
		func(*Runtime) error { got = append(got, 1); return nil },
		func(*Runtime) error { got = append(got, 2); return nil },
	)
	r.Advance(2)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("got %v, want [1 2]", got)
	}
}
