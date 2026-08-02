package server

import (
	"encoding/json"
	"io"
	"net/http"
)

// This file implements POST /v1/messages/count_tokens — the endpoint the
// Anthropic SDK's client.messages.count_tokens() posts to, which agent
// frameworks use to pre-flight a context budget before spending a generation on
// it. The request is an Anthropic Messages request minus the generation fields
// ({model, messages, system?, tools?}) and the response body is exactly
// {"input_tokens": N}: that is the whole contract, so nothing else goes in it.
//
// It counts through the same path as everything else — the Anthropic request is
// translated with toOpenAIRequest and handed to precisePromptTokens (the
// resident tokenizer, as /v1/tokenize uses) with estimatePromptTokens as the
// fallback. A second tokenizer call site would be a second answer to the same
// question. Inheriting that path also inherits its one gap: neither counter
// reads tool *definitions*, so a request shipping a large tool schema reads low
// — the same way it already does for the context guardrail.

// messagesCountTokens implements POST /v1/messages/count_tokens.
func (s *Server) messagesCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "missing required field: model")
		return
	}
	// Same alias resolution as /v1/messages: the tokenizer is keyed by the real id.
	model, ok := s.sched.Resolve(req.Model)
	if !ok {
		writeError(w, http.StatusNotFound, "model not found: "+req.Model)
		return
	}
	req.Model = model
	oaiBody, err := toOpenAIRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Exact when the model is resident on a tokenizing engine — counting must not
	// force a load, since the point of the endpoint is to be cheap — estimated
	// otherwise. Which one it was is the difference between a budget a caller can
	// trust and a guess, so it is reported rather than left to be assumed.
	tokens, exact := s.precisePromptTokens(r.Context(), model, oaiBody)
	method := "estimated"
	if exact {
		method = "exact"
	} else {
		tokens = estimatePromptTokens(oaiBody)
	}
	w.Header().Set("X-Mainspring-Tokens-Method", method)
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": tokens})
}
