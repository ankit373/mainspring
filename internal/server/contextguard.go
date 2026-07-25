package server

import (
	"encoding/json"
	"fmt"
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
	var req struct {
		MaxTokens           *int `json:"max_tokens"`
		MaxCompletionTokens *int `json:"max_completion_tokens"`
	}
	_ = json.Unmarshal(body, &req)
	maxTok := 0
	if req.MaxTokens != nil {
		maxTok = *req.MaxTokens
	} else if req.MaxCompletionTokens != nil {
		maxTok = *req.MaxCompletionTokens
	}

	// Exact: the requested output alone cannot exceed the whole window.
	if maxTok > limit {
		return true, fmt.Sprintf("max_tokens %d exceeds model context window of %d tokens", maxTok, limit)
	}

	// Estimated: prompt tokens + requested output must fit the window.
	promptEst := estimatePromptTokens(body)
	if promptEst+maxTok > limit {
		return true, fmt.Sprintf("estimated prompt (~%d tokens) + max_tokens %d exceeds model context window of %d tokens", promptEst, maxTok, limit)
	}
	return false, ""
}

// estimatePromptTokens is a tokenizer-free, conservative estimate of the input
// token count. It handles both the chat (messages[].content, string or
// multimodal array) and legacy completion (prompt) shapes. Non-text parts (e.g.
// images) contribute only their fixed per-message overhead.
func estimatePromptTokens(body []byte) int {
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Prompt json.RawMessage `json:"prompt"`
		System string          `json:"system"`
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
	return total
}

// contentTokens estimates tokens for a message content field that may be a bare
// string or an array of typed parts (text/image_url/...).
func contentTokens(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return charTokens(utf8.RuneCountInString(s))
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		n := 0
		for _, p := range parts {
			n += charTokens(utf8.RuneCountInString(p.Text))
		}
		return n
	}
	return 0
}

func charTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return (chars + tokensPerCharDenom - 1) / tokensPerCharDenom // ceil
}
