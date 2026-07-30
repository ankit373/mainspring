package server

import (
	"strings"
	"testing"
)

func TestClampMaxTokens(t *testing.T) {
	// Tiny prompt, oversized max_tokens → clamp to the remaining room.
	body := []byte(`{"model":"m1","max_tokens":99999,"messages":[{"role":"user","content":"hi"}]}`)
	out, from, to, clamped := clampMaxTokens(body, 4096)
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
	if _, _, _, c := clampMaxTokens([]byte(`{"max_tokens":100,"messages":[]}`), 4096); c {
		t.Fatal("request that fits must not be clamped")
	}
	// No output field → nothing to clamp.
	if _, _, _, c := clampMaxTokens([]byte(`{"messages":[]}`), 4096); c {
		t.Fatal("request without max_tokens must not be clamped")
	}
	// max_completion_tokens is honored.
	if _, _, _, c := clampMaxTokens([]byte(`{"max_completion_tokens":9000,"messages":[]}`), 4096); !c {
		t.Fatal("max_completion_tokens overage should clamp")
	}
}

func TestClampNoRoom(t *testing.T) {
	// Prompt alone (huge) leaves < minClampRoom → defer to the guardrail.
	big := strings.Repeat("x", 40000) // ~10000 tokens, well over a 4096 window
	body := []byte(`{"max_tokens":500,"messages":[{"role":"user","content":"` + big + `"}]}`)
	if _, _, _, c := clampMaxTokens(body, 4096); c {
		t.Fatal("no room left → should not clamp (guardrail decides)")
	}
}

func TestRewriteIntField(t *testing.T) {
	out := rewriteIntField([]byte(`{"a":1,"max_tokens":10}`), "max_tokens", 7)
	if !strings.Contains(string(out), `"max_tokens":7`) {
		t.Fatalf("field not rewritten: %s", out)
	}
}
