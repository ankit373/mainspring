package server

import (
	"testing"
	"time"
)

func TestTimeoutFor(t *testing.T) {
	s := &Server{}
	s.SetTimeouts(2*time.Second, map[string]time.Duration{"slow": 30 * time.Second})
	if got := s.timeoutFor("slow"); got != 30*time.Second {
		t.Fatalf("per-model override = %v, want 30s", got)
	}
	if got := s.timeoutFor("other"); got != 2*time.Second {
		t.Fatalf("default = %v, want 2s", got)
	}
	// No timeouts configured => unbounded (0).
	s2 := &Server{}
	if s2.timeoutFor("x") != 0 {
		t.Fatal("unset timeout should be 0 (unbounded)")
	}
}
