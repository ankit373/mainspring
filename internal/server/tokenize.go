package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/ankit373/mainspring/internal/backend"
)

// residentTokenCounter returns the TokenCounter for a model when it is currently
// resident and its runner implements exact tokenization, so callers can count
// tokens without forcing a load. ok=false falls back to the estimate.
func (s *Server) residentTokenCounter(model string) (backend.TokenCounter, bool) {
	for _, ri := range s.sched.Loaded() {
		if ri.ID != model {
			continue
		}
		tc, ok := ri.Runner.(backend.TokenCounter)
		return tc, ok
	}
	return nil, false
}

// countTokens returns the token count for text against a model. It uses the
// model's real tokenizer when the model is resident and supports it (exact=true)
// and otherwise the character-based estimate (exact=false).
func (s *Server) countTokens(ctx context.Context, model, text string) (int, bool) {
	if tc, ok := s.residentTokenCounter(model); ok {
		if n, err := tc.CountTokens(ctx, text); err == nil {
			return n, true
		}
	}
	return charTokens(utf8.RuneCountInString(text)), false
}

// precisePromptTokens counts a request's prompt tokens exactly using the model's
// real tokenizer, when the model is resident and supports it (ok=true). It sums
// the tokenized text of each message (plus the fixed per-message chat-template
// overhead), the system prompt, and any legacy completion prompt. ok=false (no
// forced load, or a tokenizer error) tells the caller to fall back to the
// estimate.
func (s *Server) precisePromptTokens(ctx context.Context, model string, body []byte) (int, bool) {
	tc, ok := s.residentTokenCounter(model)
	if !ok {
		return 0, false
	}
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Prompt json.RawMessage `json:"prompt"`
		System string          `json:"system"`
	}
	if json.Unmarshal(body, &req) != nil {
		return 0, false
	}
	total := 0
	count := func(text string) bool {
		if text == "" {
			return true
		}
		n, err := tc.CountTokens(ctx, text)
		if err != nil {
			return false
		}
		total += n
		return true
	}
	if !count(req.System) {
		return 0, false
	}
	for _, m := range req.Messages {
		total += perMessageOverhead
		if !count(contentText(m.Content)) {
			return 0, false
		}
	}
	if !count(contentText(req.Prompt)) {
		return 0, false
	}
	return total, true
}

// tokenize implements POST /v1/tokenize — a utility that counts tokens for a
// piece of text against a model, exact when the engine can tokenize, estimated
// otherwise. Body: {"model": "...", "input": "..."}.
func (s *Server) tokenize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, codeMethodNotAllowed, "method not allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	var req struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "missing required field: model")
		return
	}
	model, ok := s.sched.Resolve(req.Model)
	if !ok {
		writeError(w, http.StatusNotFound, "model not found: "+req.Model)
		return
	}
	tokens, exact := s.countTokens(r.Context(), model, req.Input)
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "tokenization",
		"model":  model,
		"tokens": tokens,
		"exact":  exact,
	})
}
