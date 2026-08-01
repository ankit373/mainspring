package auth

import "testing"

func TestRemainingRequestsIsAPureRead(t *testing.T) {
	l := NewLimiter()
	// Peeking must not itself consume a request: calling it repeatedly reports
	// the same full budget as long as AllowRequest is never called.
	for i := 0; i < 5; i++ {
		remaining, reset := l.RemainingRequests("k", 3)
		if remaining != 3 {
			t.Fatalf("peek %d: remaining = %d, want 3 (peeking must not consume)", i, remaining)
		}
		if reset <= 0 {
			t.Fatalf("peek %d: reset = %v, want > 0 for a fresh window", i, reset)
		}
	}
}

func TestRemainingRequestsReflectsConsumption(t *testing.T) {
	l := NewLimiter()
	l.AllowRequest("k", 3) // consume 1 of 3
	if remaining, _ := l.RemainingRequests("k", 3); remaining != 2 {
		t.Fatalf("remaining after 1 request = %d, want 2", remaining)
	}
	l.AllowRequest("k", 3)
	l.AllowRequest("k", 3) // now at 3/3
	if remaining, _ := l.RemainingRequests("k", 3); remaining != 0 {
		t.Fatalf("remaining at budget = %d, want 0", remaining)
	}
	// Denied attempts (over budget) must not push remaining negative.
	l.AllowRequest("k", 3)
	if remaining, _ := l.RemainingRequests("k", 3); remaining != 0 {
		t.Fatalf("remaining after over-budget attempt = %d, want 0 (never negative)", remaining)
	}
}

func TestRemainingTokensIsAPureRead(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < 5; i++ {
		remaining, reset := l.RemainingTokens("k", 1000, 60)
		if remaining != 1000 {
			t.Fatalf("peek %d: remaining = %d, want 1000 (peeking must not consume)", i, remaining)
		}
		if reset <= 0 {
			t.Fatalf("peek %d: reset = %v, want > 0 for a fresh window", i, reset)
		}
	}
}

func TestRemainingTokensReflectsConsumption(t *testing.T) {
	l := NewLimiter()
	l.AddTokens("k", 400, 60)
	if remaining, _ := l.RemainingTokens("k", 1000, 60); remaining != 600 {
		t.Fatalf("remaining after 400 spent = %d, want 600", remaining)
	}
	l.AddTokens("k", 700, 60) // overshoots the 1000 budget
	if remaining, _ := l.RemainingTokens("k", 1000, 60); remaining != 0 {
		t.Fatalf("remaining after overshoot = %d, want 0 (never negative)", remaining)
	}
}
