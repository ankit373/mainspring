package server

import (
	"strings"
	"testing"
)

// clampEstimated is the pre-#164 call shape: clamp against the char heuristic.
func clampEstimated(body string, limit int) (out []byte, from, to int, clamped bool) {
	return clampMaxTokens([]byte(body), limit, estimatePromptTokens([]byte(body)))
}

func TestClampMaxTokens(t *testing.T) {
	// Tiny prompt, oversized max_tokens → clamp to the remaining room.
	out, from, to, clamped := clampEstimated(`{"model":"m1","max_tokens":99999,"messages":[{"role":"user","content":"hi"}]}`, 4096)
	if !clamped {
		t.Fatal("expected a clamp")
	}
	if from != 99999 || to <= 0 || to >= 99999 {
		t.Fatalf("from=%d to=%d, want from=99999 and a smaller positive to", from, to)
	}
	if !strings.Contains(string(out), `"max_tokens":`) || strings.Contains(string(out), "99999") {
		t.Fatalf("rewritten body should carry the reduced max_tokens: %s", out)
	}

	// Already fits → no clamp.
	if _, _, _, c := clampEstimated(`{"max_tokens":100,"messages":[]}`, 4096); c {
		t.Fatal("request that fits must not be clamped")
	}
	// No output field → nothing to clamp.
	if _, _, _, c := clampEstimated(`{"messages":[]}`, 4096); c {
		t.Fatal("request without max_tokens must not be clamped")
	}
	// max_completion_tokens is honored.
	if _, _, _, c := clampEstimated(`{"max_completion_tokens":9000,"messages":[]}`, 4096); !c {
		t.Fatal("max_completion_tokens overage should clamp")
	}
}

func TestClampNoRoom(t *testing.T) {
	// Prompt alone (huge) leaves < minClampRoom → defer to the guardrail.
	big := strings.Repeat("x", 40000) // ~10000 tokens, well over a 4096 window
	if _, _, _, c := clampEstimated(`{"max_tokens":500,"messages":[{"role":"user","content":"`+big+`"}]}`, 4096); c {
		t.Fatal("no room left → should not clamp (guardrail decides)")
	}
}

// TestClampUsesSuppliedPromptCount proves the clamp no longer counts the prompt
// itself: a caller that counted 4000 tokens where the char heuristic sees ~1
// gets a clamp sized to the room the *caller's* count leaves. This is what stops
// the guardrail from rejecting a request the clamp just made fit.
func TestClampUsesSuppliedPromptCount(t *testing.T) {
	body := []byte(`{"max_tokens":900,"messages":[{"role":"user","content":"hi"}]}`)
	_, _, to, clamped := clampMaxTokens(body, 4096, 4000)
	if !clamped {
		t.Fatal("expected a clamp against the supplied prompt count")
	}
	if to != 96 { // 4096 - 4000
		t.Fatalf("clamped to %d, want 96 (limit minus the supplied prompt count)", to)
	}
}

func TestRewriteIntField(t *testing.T) {
	out := rewriteIntField([]byte(`{"a":1,"max_tokens":10}`), "max_tokens", 7)
	if !strings.Contains(string(out), `"max_tokens":7`) {
		t.Fatalf("field not rewritten: %s", out)
	}
}
