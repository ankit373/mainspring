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
		if _, _, res := g.acquire(context.Background(), "m"); res != gateAcquired {
			t.Fatal("disabled gate must always allow")
		}
	}
}

func TestGateRejectsWhenFull(t *testing.T) {
	g := newGate(1, 0) // 1 slot, no queue → reject as soon as the slot is busy
	rel, _, res := g.acquire(context.Background(), "m")
	if res != gateAcquired {
		t.Fatal("first acquire should succeed")
	}
	if _, _, res := g.acquire(context.Background(), "m"); res != gateFull {
		t.Fatalf("second acquire should report gateFull (slot busy, queue=0), got %v", res)
	}
	// A different model has its own slot.
	if _, _, res := g.acquire(context.Background(), "other"); res != gateAcquired {
		t.Fatal("different model should have its own slot")
	}
	// Release frees the slot.
	rel()
	if _, _, res := g.acquire(context.Background(), "m"); res != gateAcquired {
		t.Fatal("acquire should succeed after release")
	}
}

func TestGateContextCancelWhileQueued(t *testing.T) {
	g := newGate(1, 5) // 1 slot, room to queue
	if _, _, res := g.acquire(context.Background(), "m"); res != gateAcquired {
		t.Fatal("first acquire should succeed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already-cancelled → the queued waiter gives up
	// gateCancelled, not gateFull: the queue had room, the caller went away. The
	// handler must be able to tell those apart or it answers a dead connection
	// with a 503 that misreports why the request ended.
	if _, _, res := g.acquire(ctx, "m"); res != gateCancelled {
		t.Fatalf("cancelled context should report gateCancelled, got %v", res)
	}
}

func TestGateAcquireImmediateWaitIsNegligible(t *testing.T) {
	g := newGate(2, 4) // free slot available
	rel, wait, res := g.acquire(context.Background(), "m")
	if res != gateAcquired {
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
	rel, _, res := g.acquire(context.Background(), "m")
	if res != gateAcquired {
		t.Fatal("first acquire should succeed")
	}

	const holdFor = 80 * time.Millisecond
	var wg sync.WaitGroup
	var waiterWait time.Duration
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, w, res := g.acquire(context.Background(), "m")
		if res != gateAcquired {
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

// TestGateDisabledStillCountsOccupancy pins the fix: gating is off by default,
// and an occupancy counter that only runs when gating is on reports a permanent
// zero on /v1/quality and /metrics for the default configuration. Only the
// admission decision depends on maxInflight.
func TestGateDisabledStillCountsOccupancy(t *testing.T) {
	g := newGate(0, 0)
	rel, _, res := g.acquire(context.Background(), "m")
	if res != gateAcquired {
		t.Fatal("disabled gate must always allow")
	}
	if in, q := g.stat("m"); in != 1 || q != 0 {
		t.Fatalf("stat = (%d,%d), want (1,0) — in-flight is real, there is no queue", in, q)
	}
	var sb strings.Builder
	g.writePrometheus(&sb)
	if !strings.Contains(sb.String(), `mainspring_inflight_requests{model="m"} 1`) {
		t.Fatalf("disabled gate must still export occupancy:\n%s", sb.String())
	}
	rel()
	if in, _ := g.stat("m"); in != 0 {
		t.Fatalf("in-flight = %d after release, want 0", in)
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

// A model id containing a quote and a backslash must be escaped exactly once.
// Wrapping the escaper in %q double-escaped it, corrupting the label.
func TestGatePrometheusEscapesLabelValueOnce(t *testing.T) {
	g := newGate(1, 1)
	rel, _, res := g.acquire(context.Background(), `we"ird\model`)
	if res != gateAcquired {
		t.Fatal("setup: acquire should succeed")
	}
	defer rel()
	var sb strings.Builder
	g.writePrometheus(&sb)
	out := sb.String()
	for _, want := range []string{
		`mainspring_inflight_requests{model="we\"ird\\model"} 1`,
		`mainspring_queued_requests{model="we\"ird\\model"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
