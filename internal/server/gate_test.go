package server

import (
	"context"
	"strings"
	"testing"
)

func TestGateDisabledAlwaysAllows(t *testing.T) {
	g := newGate(0, 0)
	for i := 0; i < 100; i++ {
		if _, ok := g.acquire(context.Background(), "m"); !ok {
			t.Fatal("disabled gate must always allow")
		}
	}
}

func TestGateRejectsWhenFull(t *testing.T) {
	g := newGate(1, 0) // 1 slot, no queue → reject as soon as the slot is busy
	rel, ok := g.acquire(context.Background(), "m")
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	if _, ok := g.acquire(context.Background(), "m"); ok {
		t.Fatal("second acquire should be rejected (slot busy, queue=0)")
	}
	// A different model has its own slot.
	if _, ok := g.acquire(context.Background(), "other"); !ok {
		t.Fatal("different model should have its own slot")
	}
	// Release frees the slot.
	rel()
	if _, ok := g.acquire(context.Background(), "m"); !ok {
		t.Fatal("acquire should succeed after release")
	}
}

func TestGateContextCancelWhileQueued(t *testing.T) {
	g := newGate(1, 5) // 1 slot, room to queue
	if _, ok := g.acquire(context.Background(), "m"); !ok {
		t.Fatal("first acquire should succeed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already-cancelled → the queued waiter gives up
	if _, ok := g.acquire(ctx, "m"); ok {
		t.Fatal("cancelled context should not acquire")
	}
}

func TestGatePrometheus(t *testing.T) {
	g := newGate(2, 4)
	rel, _ := g.acquire(context.Background(), "m")
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
