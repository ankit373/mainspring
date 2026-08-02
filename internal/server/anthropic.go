package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/metrics"
)

// This file implements Anthropic Messages API compatibility (POST /v1/messages)
// by translating to/from the OpenAI chat-completions upstream. Text content is
// fully supported (string or text blocks, system prompt, temperature, top_p,
// stop_sequences, streaming). Tool use is translated in both directions:
// Anthropic tools/tool_choice ↔ OpenAI tools/tool_choice, assistant tool_use
// blocks ↔ OpenAI tool_calls, and user tool_result blocks ↔ OpenAI role:tool
// messages, including streaming (input_json_delta). Images are not yet translated.

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        json.RawMessage    `json:"system,omitempty"` // string or []{type,text}
	Messages      []anthropicMessage `json:"messages"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"` // {type:auto|any|tool, name?}
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []block
}

// anthropicTool is an Anthropic tool definition.
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// contentBlock is a superset of the Anthropic content-block shapes we read.
type contentBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // string or []block
	IsError   bool            `json:"is_error,omitempty"`
	// image
	Source *imageSource `json:"source,omitempty"`
}

// imageSource is an Anthropic image block's source (base64 or url).
type imageSource struct {
	Type      string `json:"type"` // "base64" | "url"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// imageSourceToURL renders an Anthropic image source as an OpenAI image_url
// value: a data: URI for base64 sources, or the URL passed through. Returns ""
// for an unusable source.
func imageSourceToURL(s *imageSource) string {
	if s == nil {
		return ""
	}
	switch s.Type {
	case "base64":
		if s.MediaType != "" && s.Data != "" {
			return "data:" + s.MediaType + ";base64," + s.Data
		}
	case "url":
		return s.URL
	}
	return ""
}

// messages handles POST /v1/messages (Anthropic Messages API).
func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
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
	// Resolve aliases. The upstream call and metrics key on the real id; the
	// client-facing response echoes the requested name (Anthropic convention).
	requested := req.Model
	model, ok := s.sched.Resolve(req.Model)
	if !ok {
		writeError(w, http.StatusNotFound, "model not found: "+req.Model)
		return
	}
	req.Model = model

	// Translate up front: the admission checks (clamp, context guardrail) and the
	// upstream call all work on the OpenAI-shaped body, and an untranslatable
	// request must be rejected before it consumes a gate slot or loads a model.
	oaiBody, err := toOpenAIRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	oaiBody, ok = s.admit(w, r.Context(), model, oaiBody)
	if !ok {
		return
	}

	tenant, _ := auth.FromContext(r.Context())
	if !s.auth.AllowTokens(tenant) {
		writeErr(w, codeTokenBudget, "token budget exceeded")
		return
	}
	release, queueWait, ok := s.gate.acquire(r.Context(), model)
	if !ok {
		w.Header().Set("Retry-After", "1")
		writeErr(w, codeServerBusy, "server busy: too many concurrent requests for "+model)
		return
	}
	defer release()

	if !s.breaker.Allow(model) {
		w.Header().Set("Retry-After", "5")
		writeErr(w, codeCircuitOpen, "circuit open: backend for "+model+" is unavailable")
		return
	}

	// Per-request timeout: bound total generation time (0 = unbounded).
	if d := s.timeoutFor(model); d > 0 {
		tctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		r = r.WithContext(tctx)
	}

	runner, err := s.sched.EnsureLoaded(r.Context(), model)
	if err != nil {
		s.breaker.OnResult(model, false)
		writeErr(w, codeBackendUnavailable, "load model "+model+": "+err.Error())
		return
	}
	for k, v := range s.failLoudHeaders(r.Context(), runner) {
		w.Header().Set(k, v)
	}

	start := time.Now()
	// Wrap the writer once so the recorded event reports what was actually written
	// (status, bytes, TTFT) instead of an assumed 200.
	cap := newCapture(w, start)
	var prompt, completion int64
	var exact, backendFailed bool
	// Mark the runner busy for the actual generation call so the scheduler's
	// idle timer and LRU eviction never pull it out from under a long-running
	// request (e.g. one that streams longer than KeepAlive).
	s.sched.MarkBusy(model)
	if req.Stream {
		prompt, completion, exact, backendFailed = s.messagesStream(cap, r.Context(), runner.BaseURL(), oaiBody, requested)
	} else {
		prompt, completion, exact, backendFailed = s.messagesJSON(cap, r.Context(), runner.BaseURL(), oaiBody, requested)
	}
	s.sched.MarkIdle(model)
	// A 5xx counts as a backend failure; 2xx/4xx are healthy (4xx is a client error,
	// not the backend's fault). backendFailed covers what the status cannot: once an
	// SSE stream is open the status is pinned at 200, so a stream that died mid-flight
	// would otherwise be recorded as a success. Without any of this the breaker never
	// learns that a /v1/messages request succeeded — and a half-open probe consumed by
	// this path would never resolve.
	s.breaker.OnResult(model, !backendFailed && cap.status < 500)

	charge := completion
	if exact {
		charge = prompt + completion
	}
	s.auth.AddTokens(tenant, charge)
	if s.metrics != nil {
		s.metrics.Record(metrics.Event{
			Time: start, RequestID: RequestID(r.Context()), TraceID: TraceID(r.Context()),
			Model: model, Tenant: auth.TenantOf(r.Context()),
			Status: cap.status, Stream: cap.stream,
			DurationMs:   float64(time.Since(start).Microseconds()) / 1000.0,
			TTFTMs:       cap.ttftMs(),
			Bytes:        cap.bytes,
			PromptTokens: prompt, TokensEst: completion, Exact: exact,
			CostUSD:     s.costFor(model, prompt, completion, exact),
			QueueWaitMs: float64(queueWait.Microseconds()) / 1000.0,
		})
	}
}

// toOpenAIRequest translates an Anthropic request into an OpenAI chat body.
func toOpenAIRequest(req anthropicRequest) ([]byte, error) {
	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	if sys := rawToText(req.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range req.Messages {
		converted, err := anthropicMessageToOpenAI(m)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, converted...)
	}
	oai := map[string]any{"model": req.Model, "messages": msgs, "stream": req.Stream}
	if req.Stream {
		// Ask the upstream for a final usage chunk so messagesStream can report
		// exact prompt/completion tokens instead of falling back to the SSE-frame
		// estimate. Only meaningful when streaming; omitted otherwise.
		oai["stream_options"] = map[string]bool{"include_usage": true}
	}
	if req.MaxTokens > 0 {
		oai["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		oai["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		oai["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		oai["stop"] = req.StopSequences
	}
	if len(req.Tools) > 0 {
		oai["tools"] = toolsToOpenAI(req.Tools)
	}
	if tc := toolChoiceToOpenAI(req.ToolChoice); tc != nil {
		oai["tool_choice"] = tc
	}
	return json.Marshal(oai)
}

// toolsToOpenAI maps Anthropic tool definitions to OpenAI function tools.
func toolsToOpenAI(tools []anthropicTool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if len(t.InputSchema) > 0 {
			fn["parameters"] = json.RawMessage(t.InputSchema)
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// toolChoiceToOpenAI maps Anthropic tool_choice to the OpenAI form. Returns nil
// when unset (upstream default applies).
func toolChoiceToOpenAI(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		if tc.Name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
		}
		return "required"
	default:
		return nil
	}
}

// anthropicMessageToOpenAI translates one Anthropic message into one or more
// OpenAI messages. A user turn carrying tool_result blocks expands into role:tool
// messages; an assistant turn carrying tool_use blocks becomes an assistant
// message with tool_calls.
func anthropicMessageToOpenAI(m anthropicMessage) ([]map[string]any, error) {
	// Simple string content: single message.
	var str string
	if json.Unmarshal(m.Content, &str) == nil {
		return []map[string]any{{"role": m.Role, "content": str}}, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("invalid message content: %v", err)
	}

	var out []map[string]any
	var text strings.Builder
	var toolCalls []map[string]any
	var parts []map[string]any // ordered text+image content parts (user turns)
	hasImage := false

	for _, b := range blocks {
		switch b.Type {
		case "text", "":
			text.WriteString(b.Text)
			if b.Text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": b.Text})
			}
		case "image":
			if url := imageSourceToURL(b.Source); url != "" {
				hasImage = true
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			}
		case "tool_use":
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   b.ID,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": args,
				},
			})
		case "tool_result":
			// Each tool_result becomes its own OpenAI tool message (text only).
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": b.ToolUseID,
				"content":      rawToText(b.Content),
			})
		}
	}

	// Assistant turn with text and/or tool calls (assistants don't send images).
	if m.Role == "assistant" {
		msg := map[string]any{"role": "assistant"}
		if text.Len() > 0 {
			msg["content"] = text.String()
		} else {
			msg["content"] = nil
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		// Prepend the assistant message before any (unlikely) tool blocks.
		return append([]map[string]any{msg}, out...), nil
	}

	// User turn carrying images: emit multimodal content parts (text + image_url,
	// in original order) instead of a collapsed string.
	if hasImage {
		out = append(out, map[string]any{"role": "user", "content": parts})
		return out, nil
	}

	// User turn: emit tool messages first (they answer a prior assistant turn),
	// then any trailing user text.
	if text.Len() > 0 {
		out = append(out, map[string]any{"role": "user", "content": text.String()})
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"role": m.Role, "content": ""})
	}
	return out, nil
}

// rawToText extracts text from an Anthropic content value that is either a JSON
// string or an array of content blocks (only text blocks are read).
func rawToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, bl := range blocks {
			if bl.Type == "text" || bl.Type == "" {
				b.WriteString(bl.Text)
			}
		}
		return b.String()
	}
	return ""
}

// mapStopReason maps an OpenAI finish_reason to an Anthropic stop_reason.
func mapStopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "stop", "":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// oaiToolCall is a tool call as returned by the OpenAI upstream.
type oaiToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// messagesJSON does a non-streaming upstream call and writes the Anthropic body.
// It returns the real prompt/completion token counts and whether the upstream
// supplied a usage object.
func (s *Server) messagesJSON(w http.ResponseWriter, ctx context.Context, baseURL string, oaiBody []byte, model string) (prompt, completion int64, exact, backendFailed bool) {
	resp, err := postJSON(ctx, baseURL+"/v1/chat/completions", oaiBody)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			writeErr(w, codeTimeout, "request timed out")
			return 0, 0, false, false
		}
		writeError(w, http.StatusBadGateway, "backend request failed: "+err.Error())
		return 0, 0, false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		// Fail loud: an upstream error body decodes into zero choices, which would
		// otherwise be served as a well-formed empty 200 Anthropic message.
		writeUpstreamFailure(w, resp)
		return 0, 0, false, false
	}
	var oai struct {
		Choices []struct {
			Message struct {
				Content   string        `json:"content"`
				ToolCalls []oaiToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRequestBody)).Decode(&oai); err != nil {
		writeError(w, http.StatusBadGateway, "decode backend response: "+err.Error())
		return 0, 0, false, false
	}
	content, finish := "", ""
	var toolCalls []oaiToolCall
	if len(oai.Choices) > 0 {
		content = oai.Choices[0].Message.Content
		finish = oai.Choices[0].FinishReason
		toolCalls = oai.Choices[0].Message.ToolCalls
	}

	blocks := make([]map[string]any, 0, 1+len(toolCalls))
	if content != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": content})
	}
	for _, tc := range toolCalls {
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Function.Name,
			"input": argsToInput(tc.Function.Arguments),
		})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}

	out := map[string]any{
		"id":            fmt.Sprintf("msg_%d", time.Now().UnixNano()),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   mapStopReason(finish),
		"stop_sequence": nil,
		"usage":         map[string]int64{"input_tokens": oai.Usage.PromptTokens, "output_tokens": oai.Usage.CompletionTokens},
	}
	exact = oai.Usage.CompletionTokens > 0 || oai.Usage.PromptTokens > 0
	if exact {
		// We own the writer here, so surface usage as headers too (set before body).
		w.Header().Set("X-Mainspring-Tokens-Input", strconv.FormatInt(oai.Usage.PromptTokens, 10))
		w.Header().Set("X-Mainspring-Tokens-Output", strconv.FormatInt(oai.Usage.CompletionTokens, 10))
	}
	writeJSON(w, http.StatusOK, out)
	return oai.Usage.PromptTokens, oai.Usage.CompletionTokens, exact, false
}

// argsToInput turns an OpenAI tool-call arguments JSON string into a JSON value
// suitable for the Anthropic tool_use.input field (object, not a string).
func argsToInput(args string) json.RawMessage {
	if strings.TrimSpace(args) == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(args)) {
		return json.RawMessage(args)
	}
	// Not valid JSON: wrap as a string value so the payload stays well-formed.
	b, _ := json.Marshal(args)
	return b
}

// messagesStream translates the upstream OpenAI SSE stream into Anthropic SSE
// events. It returns the prompt/completion token counts and whether they came
// from an upstream usage object (stream_options.include_usage); when absent,
// completion falls back to the number of streamed fragments.
func (s *Server) messagesStream(w http.ResponseWriter, ctx context.Context, baseURL string, oaiBody []byte, model string) (prompt, completion int64, exact, backendFailed bool) {
	resp, err := postJSON(ctx, baseURL+"/v1/chat/completions", oaiBody)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			writeErr(w, codeTimeout, "request timed out")
			return 0, 0, false, false
		}
		writeError(w, http.StatusBadGateway, "backend request failed: "+err.Error())
		return 0, 0, false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		// Fail loud before any SSE byte goes out: once the stream is open the status
		// can no longer be corrected.
		writeUpstreamFailure(w, resp)
		return 0, 0, false, false
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": fmt.Sprintf("msg_%d", time.Now().UnixNano()), "type": "message", "role": "assistant",
		"model": model, "content": []any{}, "stop_reason": nil,
		"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
	}})

	// The stream is a sequence of content blocks. Index 0 is reserved for text and
	// opened lazily on the first text delta; tool_use blocks follow.
	st := &anthropicStreamState{send: send}

	var deltas int64
	finish := "stop"
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string        `json:"content"`
					ToolCalls []oaiToolCall `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		// The include_usage final chunk carries usage with an empty choices list.
		if chunk.Usage != nil {
			prompt, completion, exact = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, true
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		if c := ch.Delta.Content; c != "" {
			deltas++
			st.textDelta(c)
		}
		for _, tc := range ch.Delta.ToolCalls {
			deltas++
			st.toolDelta(tc)
		}
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
	}

	// Prefer real usage from the upstream include_usage chunk; else the fragment
	// count is the best available output estimate.
	if !exact {
		completion = deltas
	}

	// Fail loud on a truncated stream: a cut connection or an expired deadline must
	// not be closed with a fabricated end_turn, which a client cannot tell apart
	// from a complete answer. Close any open block, then emit an Anthropic `error`
	// event instead of message_delta/message_stop. The status is already 200 on the
	// wire by now, so this error event is the only signal the client will get — and
	// the returned flag is the only signal the breaker will get.
	if err := sc.Err(); err != nil || ctx.Err() != nil {
		st.closeOpen()
		switch {
		case err != nil:
			streamError(send, codeUpstreamError, "upstream stream ended prematurely: "+err.Error())
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			streamError(send, codeTimeout, "stream exceeded the per-request timeout")
		default:
			streamError(send, codeInternal, "stream cancelled before completion")
		}
		// A broken upstream or an exceeded deadline is the backend's fault. A caller
		// that cancelled its own request is not — charging that to the breaker would
		// let clients trip the circuit for every tenant by disconnecting.
		return prompt, completion, exact, !errors.Is(ctx.Err(), context.Canceled)
	}

	if !st.started {
		// No content at all: emit an empty text block so the message is well-formed.
		st.textDelta("")
	}
	st.closeOpen()
	send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": mapStopReason(finish), "stop_sequence": nil},
		"usage": map[string]int64{"input_tokens": prompt, "output_tokens": completion}})
	send("message_stop", map[string]any{"type": "message_stop"})
	return prompt, completion, exact, false
}

// upstreamErrorDetail bounds how much of an upstream error body is echoed back.
const upstreamErrorDetail = 512

// writeUpstreamFailure reports a non-2xx upstream response as an error instead of
// letting it decode into an empty success. A 5xx becomes a 502 upstream_error (the
// backend broke); a 4xx keeps its own status (the request was bad). Both carry a
// bounded prefix of the upstream message.
func writeUpstreamFailure(w http.ResponseWriter, resp *http.Response) {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamErrorDetail))
	msg := fmt.Sprintf("backend returned %d", resp.StatusCode)
	if detail := strings.TrimSpace(string(b)); detail != "" {
		msg += ": " + detail
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		writeError(w, resp.StatusCode, msg)
		return
	}
	writeErr(w, codeUpstreamError, msg)
}

// streamError emits an Anthropic `error` SSE event carrying Mainspring's error
// taxonomy, so a stream that ends abnormally says so on the wire.
func streamError(send func(event string, data any), code errorCode, msg string) {
	m, ok := codeMeta[code]
	if !ok {
		m = codeMeta[codeInternal]
	}
	send("error", map[string]any{"type": "error", "error": map[string]string{
		"type": m.typ, "code": string(code), "message": msg,
	}})
}

// anthropicStreamState tracks which content block is currently open so text and
// tool_use fragments translate into the correct start/delta/stop event sequence.
type anthropicStreamState struct {
	send      func(event string, data any)
	nextIndex int  // next content-block index to allocate
	openKind  int  // 0 none, 1 text, 2 tool
	openIndex int  // index of the currently open block
	openTool  int  // OpenAI tool-call index mapped to the open tool block (-1 if none)
	started   bool // any block opened yet
}

func (st *anthropicStreamState) textDelta(text string) {
	if st.openKind != 1 {
		st.closeOpen()
		st.openIndex = st.nextIndex
		st.nextIndex++
		st.openKind = 1
		st.started = true
		st.send("content_block_start", map[string]any{"type": "content_block_start", "index": st.openIndex,
			"content_block": map[string]any{"type": "text", "text": ""}})
	}
	st.send("content_block_delta", map[string]any{"type": "content_block_delta", "index": st.openIndex,
		"delta": map[string]any{"type": "text_delta", "text": text}})
}

func (st *anthropicStreamState) toolDelta(tc oaiToolCall) {
	// A new tool_use block begins when the OpenAI tool index changes or a call id
	// arrives while no tool block is open.
	if st.openKind != 2 || st.openTool != tc.Index {
		st.closeOpen()
		st.openIndex = st.nextIndex
		st.nextIndex++
		st.openKind = 2
		st.openTool = tc.Index
		st.started = true
		st.send("content_block_start", map[string]any{"type": "content_block_start", "index": st.openIndex,
			"content_block": map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{}}})
	}
	if frag := tc.Function.Arguments; frag != "" {
		st.send("content_block_delta", map[string]any{"type": "content_block_delta", "index": st.openIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": frag}})
	}
}

func (st *anthropicStreamState) closeOpen() {
	if st.openKind == 0 {
		return
	}
	st.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": st.openIndex})
	st.openKind = 0
}

// postJSON POSTs body to url and returns the response (caller closes Body).
func postJSON(ctx context.Context, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if tp := outgoingTraceparent(ctx); tp != "" {
		req.Header.Set("traceparent", tp)
	}
	return proxyClient.Do(req)
}
