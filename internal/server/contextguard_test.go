package server

import (
	"strings"
	"testing"
)

func TestContextOverageExact(t *testing.T) {
	// max_tokens alone exceeds the window → exact overage.
	over, reason := contextOverage([]byte(`{"max_tokens":5000,"messages":[]}`), 4096)
	if !over || !strings.Contains(reason, "max_tokens 5000") {
		t.Fatalf("expected exact overage, got over=%v reason=%q", over, reason)
	}
	// max_completion_tokens is honored too.
	if over, _ := contextOverage([]byte(`{"max_completion_tokens":9000}`), 4096); !over {
		t.Fatal("max_completion_tokens overage not detected")
	}
}

func TestContextOverageEstimate(t *testing.T) {
	// ~4 chars/token: 4000 'a' chars ≈ 1000 tokens + overhead; with max_tokens
	// 3500 that is ~4504 > 4096 → estimated overage.
	big := strings.Repeat("a", 4000)
	over, reason := contextOverage([]byte(`{"max_tokens":3500,"messages":[{"role":"user","content":"`+big+`"}]}`), 4096)
	if !over || !strings.Contains(reason, "estimated prompt") {
		t.Fatalf("expected estimated overage, got over=%v reason=%q", over, reason)
	}
}

func TestContextWithinLimit(t *testing.T) {
	if over, _ := contextOverage([]byte(`{"max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`), 4096); over {
		t.Fatal("small request should fit")
	}
	// No max_tokens, small prompt → fits.
	if over, _ := contextOverage([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), 4096); over {
		t.Fatal("request without max_tokens and tiny prompt should fit")
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
