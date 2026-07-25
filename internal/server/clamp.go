package server

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// clamp_max_tokens turns an over-context request into a fitting one instead of
// rejecting it: when the requested output would overflow the model's context
// window, max_tokens is shrunk to the room left after the prompt, and the
// request proceeds. This is the graceful sibling of the guardrail (which
// rejects) — clamping runs first, so a request the operator wanted served is
// served, and only a request with no room left at all falls through to the
// guardrail.

// minClampRoom is the smallest output budget worth clamping to; below this there
// is effectively no room, so clamping is skipped and the guardrail decides.
const minClampRoom = 16

// SetClampLimits installs per-model context windows for max_tokens clamping
// (keyed by resolved model id; value is the window in tokens). Safe to call at
// startup.
func (s *Server) SetClampLimits(limits map[string]int) { s.clampLimits = limits }

// clampMaxTokens shrinks the request's max_tokens (or max_completion_tokens) so
// prompt + output fits limit. It returns the rewritten body and the from/to
// values when a clamp occurred. It no-ops (clamped=false) when no output field
// is set, when the request already fits, or when there is no meaningful room
// left (the guardrail then handles it).
func clampMaxTokens(body []byte, limit int) (out []byte, from, to int, clamped bool) {
	var req struct {
		MaxTokens           *int `json:"max_tokens"`
		MaxCompletionTokens *int `json:"max_completion_tokens"`
	}
	if json.Unmarshal(body, &req) != nil {
		return body, 0, 0, false
	}
	key := "max_tokens"
	var cur int
	switch {
	case req.MaxTokens != nil:
		key, cur = "max_tokens", *req.MaxTokens
	case req.MaxCompletionTokens != nil:
		key, cur = "max_completion_tokens", *req.MaxCompletionTokens
	default:
		return body, 0, 0, false // no explicit output budget to clamp
	}

	room := limit - estimatePromptTokens(body)
	if room < minClampRoom {
		return body, 0, 0, false // no room to clamp to; let the guardrail decide
	}
	if cur <= room {
		return body, 0, 0, false // already fits
	}
	rewritten := rewriteIntField(body, key, room)
	return rewritten, cur, room, true
}

// rewriteIntField sets a top-level integer field of a JSON object, returning the
// body unchanged on any parse failure.
func rewriteIntField(body []byte, key string, val int) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return body
	}
	m[key] = raw
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// applyClamp clamps max_tokens for model when clamping is configured, mutating
// body and setting the X-Mainspring-Clamped header. Returns the (possibly
// rewritten) body.
func (s *Server) applyClamp(w http.ResponseWriter, model string, body []byte) []byte {
	lim, ok := s.clampLimits[model]
	if !ok || lim <= 0 {
		return body
	}
	if newBody, from, to, clamped := clampMaxTokens(body, lim); clamped {
		w.Header().Set("X-Mainspring-Clamped", fmt.Sprintf("%d->%d", from, to))
		return newBody
	}
	return body
}
