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

func TestWritePrometheus(t *testing.T) {
	g, _ := newTestGroup(1, time.Minute)
	g.OnResult("m", false)
	var sb strings.Builder
	g.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `mainspring_breaker_open{model="m"} 1`) {
		t.Fatalf("expected open gauge, got:\n%s", sb.String())
	}
}
