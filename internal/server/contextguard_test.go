package server

import (
	"strings"
	"testing"
)

// estimated is the guardrail's default accuracy: the caller has no exact
// tokenizer, so it passes the character-based estimate. Mirrors what
// Server.admit does when precise context is off.
func estimated(body []byte, limit int) (bool, string) {
	return contextOverageDetail(body, limit, estimatePromptTokens(body), false)
}

func TestContextOverageExact(t *testing.T) {
	// max_tokens alone exceeds the window → exact overage.
	over, reason := estimated([]byte(`{"max_tokens":5000,"messages":[]}`), 4096)
	if !over || !strings.Contains(reason, "max_tokens 5000") {
		t.Fatalf("expected exact overage, got over=%v reason=%q", over, reason)
	}
	// max_completion_tokens is honored too.
	if over, _ := estimated([]byte(`{"max_completion_tokens":9000}`), 4096); !over {
		t.Fatal("max_completion_tokens overage not detected")
	}
}

func TestContextOverageEstimate(t *testing.T) {
	// ~4 chars/token: 4000 'a' chars ≈ 1000 tokens + overhead; with max_tokens
	// 3500 that is ~4504 > 4096 → estimated overage.
	big := strings.Repeat("a", 4000)
	over, reason := estimated([]byte(`{"max_tokens":3500,"messages":[{"role":"user","content":"`+big+`"}]}`), 4096)
	if !over || !strings.Contains(reason, "estimated prompt") {
		t.Fatalf("expected estimated overage, got over=%v reason=%q", over, reason)
	}
}

// With a real tokenizer behind it (exact=true) the rejection must not be
// labelled as a guess: no "estimated"/"~", and the exact count is reported.
func TestContextOverageExactPromptCountIsLabelledExact(t *testing.T) {
	body := []byte(`{"max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`)
	over, reason := contextOverageDetail(body, 4096, 4000, true)
	if !over {
		t.Fatalf("4000 exact prompt tokens + max_tokens 100 must not fit 4096")
	}
	if !strings.Contains(reason, "prompt (4000 tokens)") {
		t.Fatalf("exact reason should report the counted prompt, got %q", reason)
	}
	if strings.Contains(reason, "estimated") || strings.Contains(reason, "~") {
		t.Fatalf("exact reason must not be hedged as an estimate, got %q", reason)
	}
	// Same numbers, exact=false → the hedged wording, so the two are
	// distinguishable on the wire.
	_, est := contextOverageDetail(body, 4096, 4000, false)
	if !strings.Contains(est, "estimated prompt (~4000 tokens)") {
		t.Fatalf("estimated reason should be hedged, got %q", est)
	}
}

func TestContextWithinLimit(t *testing.T) {
	if over, _ := estimated([]byte(`{"max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`), 4096); over {
		t.Fatal("small request should fit")
	}
	// No max_tokens, small prompt → fits.
	if over, _ := estimated([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), 4096); over {
		t.Fatal("request without max_tokens and tiny prompt should fit")
	}
	// An exact count that fits is not an overage either.
	if over, _ := contextOverageDetail([]byte(`{"max_tokens":96}`), 4096, 4000, true); over {
		t.Fatal("4000 exact prompt tokens + max_tokens 96 fits 4096 exactly")
	}
}

func TestEstimatePromptTokens(t *testing.T) {
	// 40 chars ≈ 10 tokens + 4 overhead = 14.
	body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 40) + `"}]}`)
	if got := estimatePromptTokens(body); got != 14 {
		t.Fatalf("estimate = %d, want 14", got)
	}
	// Multimodal content array: only the text part counts (8 chars ≈ 2 tokens) + 4.
	multi := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"abcdefgh"},{"type":"image_url","image_url":{"url":"data:..."}}]}]}`)
	if got := estimatePromptTokens(multi); got != 6 {
		t.Fatalf("multimodal estimate = %d, want 6", got)
	}
	// Legacy completion prompt.
	if got := estimatePromptTokens([]byte(`{"prompt":"` + strings.Repeat("y", 20) + `"}`)); got != 5 {
		t.Fatalf("prompt estimate = %d, want 5", got)
	}
}

func TestEstimatePromptTokensSystemShapes(t *testing.T) {
	// Bare string system: 20 chars ≈ 5 tokens.
	if got := estimatePromptTokens([]byte(`{"system":"` + strings.Repeat("s", 20) + `"}`)); got != 5 {
		t.Fatalf("string system estimate = %d, want 5", got)
	}
	// Anthropic block-array system counts its text, exactly like the string form.
	blocks := []byte(`{"system":[{"type":"text","text":"` + strings.Repeat("s", 20) + `"}]}`)
	if got := estimatePromptTokens(blocks); got != 5 {
		t.Fatalf("block-array system estimate = %d, want 5", got)
	}
	// The critical case: a block-array system must not zero the *whole* request.
	// 40 chars of content ≈ 10 + 4 overhead, plus 5 for the system = 19.
	both := []byte(`{"system":[{"type":"text","text":"` + strings.Repeat("s", 20) + `"}],` +
		`"messages":[{"role":"user","content":"` + strings.Repeat("x", 40) + `"}]}`)
	if got := estimatePromptTokens(both); got != 19 {
		t.Fatalf("estimate with block-array system = %d, want 19 (0 means the guardrail failed open)", got)
	}
}
