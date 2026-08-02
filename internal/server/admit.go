package server

import (
	"context"
	"net/http"
)

// admit runs the pre-flight admission checks that must happen before any backend
// work: the graceful max_tokens clamp, then the context guardrail. Both operate on
// an OpenAI-shaped request body, so the Anthropic /v1/messages path calls this
// with its translated body — `clamp_max_tokens` and `enforce_context` then mean
// the same thing on every path instead of protecting only the OpenAI endpoints.
//
// It returns the (possibly clamped) body, and ok=false when the guardrail rejected
// the request — in which case the error response has already been written.
func (s *Server) admit(w http.ResponseWriter, ctx context.Context, model string, body []byte) (out []byte, ok bool) {
	// Graceful clamp: shrink an over-budget max_tokens to fit the window before
	// the guardrail gets a chance to reject the request.
	body = s.applyClamp(w, model, body)

	// Context guardrail: reject (or warn) an over-context request before doing any
	// work, rather than letting the engine silently truncate it. Prompt tokens are
	// counted exactly when precise_context is on and the model is resident on a
	// tokenizing engine, otherwise estimated; the method is reported on a header.
	pol, on := s.ctxPolicies[model]
	if !on || pol.Limit <= 0 {
		return body, true
	}
	promptTok, exact := s.guardPromptTokens(ctx, model, body)
	method := "estimated"
	if exact {
		method = "exact"
	}
	w.Header().Set("X-Mainspring-Context-Method", method)
	if over, reason := contextOverageDetail(body, pol.Limit, promptTok, exact); over {
		if pol.Enforce {
			writeErr(w, codeContextLength, reason)
			return body, false
		}
		w.Header().Set("X-Mainspring-Context-Warning", reason)
	}
	return body, true
}
