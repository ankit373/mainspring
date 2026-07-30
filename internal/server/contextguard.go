package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The context guardrail turns Mainspring's fail-loud *warning* about a too-small
// effective context into an enforceable *limit*: rather than letting the engine
// silently truncate an over-context request, the guardrail rejects it (or warns)
// before any work is done. The requested-output check (max_tokens alone > ctx)
// is exact and always safe to hard-reject; the prompt-size check is a
// conservative token estimate and is labeled as such.

// ContextPolicy is the per-model guardrail configuration. Limit is the model's
// context window in tokens; Enforce=true rejects an over-context request, false
// only warns via a response header. A model absent from the policy map is never
// checked.
type ContextPolicy struct {
	Limit   int
	Enforce bool
}

// SetContextGuard installs per-model context policies (keyed by resolved model
// id). Safe to call at startup.
func (s *Server) SetContextGuard(policies map[string]ContextPolicy) {
	s.ctxPolicies = policies
}

// SetPreciseContext enables exact prompt tokenization for the guardrail on the
// given models (keyed by resolved model id). It refines enforce_context: when a
// model is precise AND resident on a tokenizing engine, the guardrail counts
// prompt tokens exactly instead of estimating. Safe to call at startup.
func (s *Server) SetPreciseContext(models map[string]bool) { s.preciseCtx = models }

// guardPromptTokens returns the prompt token count for the guardrail: exact via
// the model's tokenizer when precise is enabled and the model is resident on a
// supporting engine, otherwise the character-based estimate.
func (s *Server) guardPromptTokens(ctx context.Context, model string, body []byte) (int, bool) {
	if s.preciseCtx[model] {
		if n, ok := s.precisePromptTokens(ctx, model, body); ok {
			return n, true
		}
	}
	return estimatePromptTokens(body), false
}

// tokensPerCharDenom estimates tokens from characters: modern BPE tokenizers
// average ~4 characters per token for English/code, so chars/4 is a reasonable,
// slightly conservative proxy without shipping a tokenizer per model.
const tokensPerCharDenom = 4

// perMessageOverhead accounts for the role/formatting tokens a chat template
// adds around each message.
const perMessageOverhead = 4

// contextOverage reports whether a request cannot fit the model's context and a
// human-readable reason. maxTokens alone exceeding the limit is exact; the
// combined prompt-estimate + maxTokens check is approximate (see estimate note).
func contextOverage(body []byte, limit int) (bool, string) {
	return contextOverageDetail(body, limit, estimatePromptTokens(body), false)
}

// contextOverageDetail is the core check with the prompt-token count supplied by
// the caller: exact when counted with the model's real tokenizer, else an
// estimate. The reason text labels which was used so the client can tell an
// exact rejection from a heuristic one.
func contextOverageDetail(body []byte, limit, promptTokens int, exact bool) (bool, string) {
	maxTok := maxTokensRequested(body)

	// The requested output alone cannot exceed the whole window (always exact).
	if maxTok > limit {
		return true, fmt.Sprintf("max_tokens %d exceeds model context window of %d tokens", maxTok, limit)
	}

	// Prompt tokens + requested output must fit the window.
	if promptTokens+maxTok > limit {
		qual := "estimated prompt (~%d tokens)"
		if exact {
			qual = "prompt (%d tokens)"
		}
		return true, fmt.Sprintf(qual+" + max_tokens %d exceeds model context window of %d tokens", promptTokens, maxTok, limit)
	}
	return false, ""
}

// maxTokensRequested returns the requested output budget (max_tokens, else
// max_completion_tokens, else 0).
func maxTokensRequested(body []byte) int {
	var req struct {
		MaxTokens           *int `json:"max_tokens"`
		MaxCompletionTokens *int `json:"max_completion_tokens"`
	}
	_ = json.Unmarshal(body, &req)
	if req.MaxTokens != nil {
		return *req.MaxTokens
	}
	if req.MaxCompletionTokens != nil {
		return *req.MaxCompletionTokens
	}
	return 0
}

// estimatePromptTokens is a tokenizer-free, conservative estimate of the input
// token count. It handles the chat (messages[].content, string or multimodal
// array), legacy completion (prompt), and embeddings (input) shapes. Non-text
// parts (e.g. images) contribute only their fixed per-message overhead.
func estimatePromptTokens(body []byte) int {
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Prompt json.RawMessage `json:"prompt"`
		System string          `json:"system"`
		Input  json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &req) != nil {
		return 0
	}

	total := charTokens(len(req.System))
	for _, m := range req.Messages {
		total += perMessageOverhead + contentTokens(m.Content)
	}
	if len(req.Prompt) > 0 {
		total += contentTokens(req.Prompt)
	}
	for _, s := range inputStrings(req.Input) {
		total += charTokens(utf8.RuneCountInString(s))
	}
	return total
}

// inputStrings extracts an embeddings-style `input` field as a slice of
// strings: a bare string becomes a single-element slice, and an array of
// strings passes through. Other shapes (e.g. pre-tokenized integer arrays) are
// uncommon and yield nil, same as before this field was counted at all.
func inputStrings(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		return arr
	}
	return nil
}

// contentTokens estimates tokens for a message content field that may be a bare
// string or an array of typed parts (text/image_url/...).
func contentTokens(raw json.RawMessage) int {
	return charTokens(utf8.RuneCountInString(contentText(raw)))
}

// contentText extracts the plain text of a message content field (a bare string,
// or the concatenated text parts of a multimodal array). Non-text parts (e.g.
// images) contribute nothing.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

func charTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return (chars + tokensPerCharDenom - 1) / tokensPerCharDenom // ceil
}
