package server

import (
	"context"
	"net/http"
	"time"
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
	limit := s.clampLimits[model] // 0 = no clamping configured for this model
	pol := s.ctxPolicies[model]   // zero Limit = no guardrail configured
	if limit <= 0 && pol.Limit <= 0 {
		return body, true
	}

	start := time.Now()
	// Count the prompt once and hand the same number to both checks. Counting it
	// twice is how a request the clamp just shrank to fit still gets rejected: the
	// clamp sizes the output against the char heuristic while the guardrail
	// re-counts with the model's tokenizer, and the two disagree.
	promptTok, exact := s.guardPromptTokens(ctx, model, body)

	// Graceful clamp: shrink an over-budget max_tokens to fit the window before
	// the guardrail gets a chance to reject the request.
	if limit > 0 {
		body = applyClamp(w, body, limit, promptTok)
	}
	if pol.Limit <= 0 {
		return body, true
	}

	// Context guardrail: reject (or warn) an over-context request before doing any
	// work, rather than letting the engine silently truncate it. Prompt tokens are
	// counted exactly when precise_context is on and the model is resident on a
	// tokenizing engine, otherwise estimated; the method is reported on a header.
	method := "estimated"
	if exact {
		method = "exact"
	}
	w.Header().Set("X-Mainspring-Context-Method", method)
	if over, reason := contextOverageDetail(body, pol.Limit, promptTok, exact); over {
		if pol.Enforce {
			writeErr(w, codeContextLength, reason)
			s.recordRejected(ctx, model, codeContextLength, start)
			return body, false
		}
		w.Header().Set("X-Mainspring-Context-Warning", reason)
	}
	return body, true
}
