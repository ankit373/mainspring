package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGateDisabledAlwaysAllows(t *testing.T) {
	g := newGate(0, 0)
	for i := 0; i < 100; i++ {
		if _, _, ok := g.acquire(context.Background(), "m"); !ok {
			t.Fatal("disabled gate must always allow")
		}
	}
}

func TestGateRejectsWhenFull(t *testing.T) {
	g := newGate(1, 0) // 1 slot, no queue → reject as soon as the slot is busy
	rel, _, ok := g.acquire(context.Background(), "m")
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	if _, _, ok := g.acquire(context.Background(), "m"); ok {
		t.Fatal("second acquire should be rejected (slot busy, queue=0)")
	}
	// A different model has its own slot.
	if _, _, ok := g.acquire(context.Background(), "other"); !ok {
		t.Fatal("different model should have its own slot")
	}
	// Release frees the slot.
	rel()
	if _, _, ok := g.acquire(context.Background(), "m"); !ok {
		t.Fatal("acquire should succeed after release")
	}
}

func TestGateContextCancelWhileQueued(t *testing.T) {
	g := newGate(1, 5) // 1 slot, room to queue
	if _, _, ok := g.acquire(context.Background(), "m"); !ok {
		t.Fatal("first acquire should succeed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already-cancelled → the queued waiter gives up
	if _, _, ok := g.acquire(ctx, "m"); ok {
		t.Fatal("cancelled context should not acquire")
	}
}

func TestGateAcquireImmediateWaitIsNegligible(t *testing.T) {
	g := newGate(2, 4) // free slot available
	rel, wait, ok := g.acquire(context.Background(), "m")
	if !ok {
		t.Fatal("acquire should succeed")
	}
	defer rel()
	// Not literally 0 — even an uncontended acquire costs a mutex + channel
	// send — but must be negligible next to an actually-queued wait (below).
	if wait > 5*time.Millisecond {
		t.Fatalf("wait = %v, want a negligible duration (slot was immediately free)", wait)
	}
}

func TestGateDisabledWaitIsZero(t *testing.T) {
	g := newGate(0, 0)
	_, wait, _ := g.acquire(context.Background(), "m")
	if wait != 0 {
		t.Fatalf("wait = %v, want 0 (gating disabled)", wait)
	}
}

// TestGateAcquireMeasuresQueueWait proves the fix: a waiter that must queue
// behind the one occupied slot reports a wait time approximating how long it
// actually waited, not 0.
func TestGateAcquireMeasuresQueueWait(t *testing.T) {
	g := newGate(1, 1) // 1 slot, room for exactly one queued waiter
	rel, _, ok := g.acquire(context.Background(), "m")
	if !ok {
		t.Fatal("first acquire should succeed")
	}

	const holdFor = 80 * time.Millisecond
	var wg sync.WaitGroup
	var waiterWait time.Duration
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, w, ok := g.acquire(context.Background(), "m")
		if !ok {
			t.Error("queued waiter should eventually acquire")
			return
		}
		waiterWait = w
	}()

	time.Sleep(holdFor)
	rel() // free the slot; the queued waiter should unblock almost immediately after this
	wg.Wait()

	if waiterWait < holdFor/2 {
		t.Fatalf("waiter reported wait=%v, want roughly >= %v (it was actually queued)", waiterWait, holdFor)
	}
}

func TestGatePrometheus(t *testing.T) {
	g := newGate(2, 4)
	rel, _, _ := g.acquire(context.Background(), "m")
	defer rel()
	var sb strings.Builder
	g.writePrometheus(&sb)
	out := sb.String()
	if !strings.Contains(out, `mainspring_inflight_requests{model="m"} 1`) {
		t.Fatalf("expected inflight gauge, got:\n%s", out)
	}
	if !strings.Contains(out, "mainspring_queued_requests") {
		t.Fatalf("expected queued gauge, got:\n%s", out)
	}
}
