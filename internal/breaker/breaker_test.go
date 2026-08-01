package breaker

import (
	"strings"
	"testing"
	"time"
)

// fakeClock is a manually-advanced time source.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestGroup(threshold int, cooldown time.Duration) (*Group, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1700000000, 0)}
	g := NewGroup(threshold, cooldown)
	g.SetClock(clk.now)
	return g, clk
}

func TestOpensAfterThreshold(t *testing.T) {
	g, _ := newTestGroup(3, time.Minute)
	if !g.Allow("m") {
		t.Fatal("fresh breaker should allow")
	}
	g.OnResult("m", false)
	g.OnResult("m", false)
	if g.State("m") != Closed {
		t.Fatal("should stay closed below threshold")
	}
	g.OnResult("m", false) // 3rd failure
	if g.State("m") != Open {
		t.Fatal("should open at threshold")
	}
	if g.Allow("m") {
		t.Fatal("open breaker must fast-fail")
	}
}

func TestHalfOpenProbeCloses(t *testing.T) {
	g, clk := newTestGroup(1, time.Minute)
	g.OnResult("m", false) // opens
	if g.Allow("m") {
		t.Fatal("should be open before cooldown")
	}
	clk.add(time.Minute)
	if !g.Allow("m") {
		t.Fatal("after cooldown, one probe should be allowed")
	}
	if g.State("m") != HalfOpen {
		t.Fatal("state should be half-open during probe")
	}
	if g.Allow("m") {
		t.Fatal("second concurrent probe must be blocked")
	}
	g.OnResult("m", true) // probe succeeds
	if g.State("m") != Closed {
		t.Fatal("successful probe should close the breaker")
	}
	if !g.Allow("m") {
		t.Fatal("closed breaker should allow")
	}
}

func TestHalfOpenProbeFailureReopens(t *testing.T) {
	g, clk := newTestGroup(1, time.Minute)
	g.OnResult("m", false)
	clk.add(time.Minute)
	g.Allow("m") // half-open probe
	g.OnResult("m", false)
	if g.State("m") != Open {
		t.Fatal("failed probe should re-open")
	}
	// Cooldown restarts from the re-open moment.
	if g.Allow("m") {
		t.Fatal("should be open again immediately after failed probe")
	}
}

func TestSuccessResetsFailures(t *testing.T) {
	g, _ := newTestGroup(3, time.Minute)
	g.OnResult("m", false)
	g.OnResult("m", false)
	g.OnResult("m", true) // reset
	g.OnResult("m", false)
	g.OnResult("m", false)
	if g.State("m") != Closed {
		t.Fatal("a success must reset the failure count")
	}
}

func TestDisabledGroupAlwaysAllows(t *testing.T) {
	g := NewGroup(0, time.Minute)
	for i := 0; i < 10; i++ {
		g.OnResult("m", false)
	}
	if !g.Allow("m") || g.State("m") != Closed {
		t.Fatal("disabled breaker must always allow and stay closed")
	}
}

func TestPerKeyIsolation(t *testing.T) {
	g, _ := newTestGroup(1, time.Minute)
	g.OnResult("a", false) // open a
	if g.Allow("a") {
		t.Fatal("a should be open")
	}
	if !g.Allow("b") {
		t.Fatal("b must be unaffected by a's breaker")
	}
}

func TestReset(t *testing.T) {
	g, _ := newTestGroup(1, time.Hour) // long cooldown — Reset must bypass it
	g.OnResult("m", false)             // trip open
	if g.State("m") != Open {
		t.Fatal("should be open after one failure at threshold=1")
	}
	if g.Allow("m") {
		t.Fatal("open breaker must fast-fail before reset")
	}

	g.Reset("m")
	if g.State("m") != Closed {
		t.Fatal("should be closed immediately after Reset, without waiting the cooldown")
	}
	if !g.Allow("m") {
		t.Fatal("reset breaker must allow")
	}
	// A single subsequent failure must re-open it fresh (failure count cleared,
	// not fast-forwarded toward the threshold).
	g.OnResult("m", false)
	if g.State("m") != Open {
		t.Fatal("threshold=1: a single failure after reset must re-open")
	}
}

func TestResetNoopWhenDisabledOrUnseen(t *testing.T) {
	disabled := NewGroup(0, time.Minute)
	disabled.Reset("m") // must not panic
	if disabled.State("m") != Closed {
		t.Fatal("disabled group must stay closed")
	}

	g, _ := newTestGroup(1, time.Minute)
	g.Reset("never-seen") // must not panic on an unknown key
	if g.State("never-seen") != Closed {
		t.Fatal("unseen key must report closed")
	}
}

func TestWritePrometheus(t *testing.T) {
	g, _ := newTestGroup(1, time.Minute)
	g.OnResult("m", false)
	var sb strings.Builder
	g.WritePrometheus(&sb)
	out := sb.String()
	if !strings.Contains(out, `mainspring_breaker_open{model="m"} 1`) {
		t.Fatalf("expected open gauge, got:\n%s", out)
	}
	// Open (not yet half-open): the half-open gauge must read 0.
	if !strings.Contains(out, `mainspring_breaker_half_open{model="m"} 0`) {
		t.Fatalf("expected half_open=0 while merely open, got:\n%s", out)
	}
}

// TestWritePrometheusDistinguishesHalfOpen proves the fix: half-open (actively
// probing recovery) must be distinguishable from a plain open breaker, not
// just collapsed into the same "tripped" gauge.
func TestWritePrometheusDistinguishesHalfOpen(t *testing.T) {
	g, clk := newTestGroup(1, time.Minute)
	g.OnResult("m", false) // opens
	clk.add(time.Minute)
	g.Allow("m") // cooldown elapsed -> transitions to HalfOpen
	if g.State("m") != HalfOpen {
		t.Fatal("setup: expected half-open state")
	}

	var sb strings.Builder
	g.WritePrometheus(&sb)
	out := sb.String()
	// Existing dashboards keep working: still "tripped" (1) while half-open.
	if !strings.Contains(out, `mainspring_breaker_open{model="m"} 1`) {
		t.Fatalf("expected open=1 during half-open (back-compat), got:\n%s", out)
	}
	// New signal: specifically half-open.
	if !strings.Contains(out, `mainspring_breaker_half_open{model="m"} 1`) {
		t.Fatalf("expected half_open=1, got:\n%s", out)
	}
}
