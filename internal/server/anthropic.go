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

	"github.com/ankit373/mainspring/internal/apierr"
	"github.com/ankit373/mainspring/internal/auth"
	"github.com/ankit373/mainspring/internal/cache"
	"github.com/ankit373/mainspring/internal/metrics"
)

// This file implements Anthropic Messages API compatibility (POST /v1/messages)
// by translating to/from the OpenAI chat-completions upstream. Text content is
// fully supported (string or text blocks, system prompt, temperature, top_p,
// top_k, stop_sequences, streaming). Tool use is translated in both directions:
// Anthropic tools/tool_choice ↔ OpenAI tools/tool_choice, assistant tool_use
// blocks ↔ OpenAI tool_calls, and user tool_result blocks ↔ OpenAI role:tool
// messages, including streaming (input_json_delta). Image blocks are translated
// to OpenAI image_url parts (a base64 source becomes a data: URI). The one
// content caveat that is real: a tool_result's body is read as text only.

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        json.RawMessage    `json:"system,omitempty"` // string or []{type,text}
	Messages      []anthropicMessage `json:"messages"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"` // forwarded; llama.cpp/MLX accept it
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
	// Taken at entry so a rejection reports the time the caller actually waited.
	// The generation path keeps its own later `start`, so TTFT stays comparable.
	recvd := time.Now()
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
		s.recordRejected(r.Context(), model, codeTokenBudget, recvd)
		return
	}
	// Response cache + coalescing, on the translated body so two Anthropic requests
	// that mean the same thing share an entry. cache.Key includes the request path,
	// so what is stored here is the Anthropic-shaped response and can never be
	// confused with an OpenAI one.
	sh, doneSharing, answered := s.beginShare(w, r, model, tenant, oaiBody, recvd)
	if answered {
		return
	}
	defer doneSharing()

	// Same selection the OpenAI path uses: the requested model, then its fallback
	// chain, with gate saturation, an open circuit and a load failure each reported
	// as themselves.
	runner, release, served, queueWait, servable := s.selectRunner(w, r, model, recvd)
	if !servable {
		return
	}
	defer release()

	// Per-request timeout for the model that will actually serve (0 = unbounded).
	if d := s.timeoutFor(served); d > 0 {
		tctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		r = r.WithContext(tctx)
	}

	for k, v := range s.failLoudHeaders(r.Context(), runner) {
		w.Header().Set(k, v)
	}
	// If a fallback answered, point the upstream body at it and tell the client.
	if served != model {
		oaiBody = rewriteModelField(oaiBody, served)
		w.Header().Set("X-Mainspring-Served-Model", served)
	}

	start := time.Now()
	// Wrap the writer once so the recorded event reports what was actually written
	// (status, bytes, TTFT) instead of an assumed 200.
	cap := newCapture(w, start)
	if sh.wanted() && served == model {
		cap.recordFor(cacheBodyCap)
	}
	// Mark the runner busy for the actual generation call so the scheduler's
	// idle timer and LRU eviction never pull it out from under a long-running
	// request (e.g. one that streams longer than KeepAlive).
	s.sched.MarkBusy(served)
	var gen messagesResult
	if req.Stream {
		gen = s.messagesStream(cap, r.Context(), runner.BaseURL(), oaiBody, requested, req.StopSequences)
	} else {
		gen = s.messagesJSON(cap, r.Context(), runner.BaseURL(), oaiBody, requested, req.StopSequences)
	}
	prompt, completion, exact, backendFailed := gen.prompt, gen.completion, gen.exact, gen.backendFailed
	s.sched.MarkIdle(served)
	// A 5xx counts as a backend failure; 2xx/4xx are healthy (4xx is a client error,
	// not the backend's fault). backendFailed covers what the status cannot: once an
	// SSE stream is open the status is pinned at 200, so a stream that died mid-flight
	// would otherwise be recorded as a success. Without any of this the breaker never
	// learns that a /v1/messages request succeeded — and a half-open probe consumed by
	// this path would never resolve.
	//
	// A caller that abandoned its own request learned nothing about the backend, so
	// it votes neither way — see Group.OnAbandoned.
	if clientGone(r.Context()) {
		s.breaker.OnAbandoned(served)
	} else {
		s.breaker.OnResult(served, !backendFailed && cap.status < 500)
	}

	// Share a successful, complete, non-streaming response with later callers. The
	// same bar as the OpenAI path: a truncated or failed body is never stored, or
	// one severed answer is replayed for the life of the entry.
	if !backendFailed && sh.wanted() && served == model && !cap.stream && !cap.bodyOver &&
		cap.status >= 200 && cap.status < 300 && cap.body != nil {
		sh.store(cache.Value{
			Status:     cap.status,
			Header:     cap.snapHeader,
			Body:       cap.body,
			Prompt:     prompt,
			Completion: completion,
			Exact:      exact,
		})
	}

	charge := completion
	if exact {
		charge = prompt + completion
	}
	s.auth.AddTokens(tenant, charge)
	if s.metrics != nil {
		s.metrics.Record(metrics.Event{
			Time: start, RequestID: RequestID(r.Context()), TraceID: TraceID(r.Context()),
			Model: served, Tenant: auth.TenantOf(r.Context()),
			Status: cap.status, Stream: cap.stream,
			DurationMs:   float64(time.Since(start).Microseconds()) / 1000.0,
			TTFTMs:       cap.ttftMs(),
			Bytes:        cap.bytes,
			PromptTokens: prompt, TokensEst: completion, Exact: exact,
			Retries:     gen.retries,
			Fallback:    served != model,
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
	// top_k is not in the OpenAI schema but every engine Mainspring drives
	// (llama.cpp, MLX, and the adopted local daemons) accepts it, so it is
	// forwarded rather than silently dropped.
	if req.TopK != nil {
		oai["top_k"] = *req.TopK
	}
	// stop is carried on the translated body even though the engine never sees it
	// (ownedStopBody strips it just before the upstream call — see stopWatch for
	// why Mainspring matches the sequences itself). It belongs here so that the
	// cache key, which is computed over this body, still tells two requests that
	// differ only in their stop sequences apart.
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

// ownedStopBody prepares the OpenAI body actually sent to the engine when
// Mainspring owns stop-sequence detection. It drops `stop`, so the engine cannot
// stop-and-erase the match before we see it, and — for a request the client asked
// for non-streaming — promotes the upstream call to a stream, so generation can be
// cut off at the match instead of running on to max_tokens. Without that promotion
// owning the stop would trade a wrong stop_reason for a far worse bug: a local
// engine grinding out thousands of tokens the caller asked it not to produce.
//
// A body that will not parse is passed through untouched: a malformed request is
// the upstream's to report, not ours to guess at.
func ownedStopBody(body []byte, promoteToStream bool) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	delete(m, "stop")
	if promoteToStream {
		m["stream"] = json.RawMessage("true")
		m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// writeUpstreamCallFailure reports a failed upstream POST, distinguishing the
// three cases that must not be conflated: a caller that vanished (say nothing —
// there is no one to tell, and a 502 here would record a backend failure that did
// not happen), an expired deadline, and a genuine backend error.
func writeUpstreamCallFailure(w http.ResponseWriter, ctx context.Context, err error) {
	switch {
	case clientGone(ctx):
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		writeErr(w, codeTimeout, "request timed out")
	default:
		writeError(w, http.StatusBadGateway, "backend request failed: "+err.Error())
	}
}

// oaiStreamChunk is one parsed chunk of an OpenAI chat-completions SSE stream.
type oaiStreamChunk struct {
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

// scanOAIStream reads an OpenAI SSE stream, calling on for each parsed chunk.
// Returning false from on stops the read early and reports stopped=true; the
// caller closing the response body is what tears the upstream connection down and
// tells the engine to stop generating. Unparseable frames are skipped rather than
// aborting the stream — a chunk we cannot read is not a broken connection.
func scanOAIStream(r io.Reader, on func(*oaiStreamChunk) bool) (stopped bool, err error) {
	sc := bufio.NewScanner(r)
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
		var chunk oaiStreamChunk
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if !on(&chunk) {
			return true, nil
		}
	}
	return false, sc.Err()
}

// oaiAccum reassembles a streamed OpenAI response into the whole-message shape the
// non-streaming Anthropic body needs. Tool calls arrive as indexed fragments whose
// arguments are concatenated across chunks.
type oaiAccum struct {
	text  strings.Builder
	tools []oaiToolCall
	index map[int]int // OpenAI tool-call index → position in tools
}

func (a *oaiAccum) tool(tc oaiToolCall) {
	pos, ok := a.index[tc.Index]
	if !ok {
		if a.index == nil {
			a.index = map[int]int{}
		}
		pos = len(a.tools)
		a.index[tc.Index] = pos
		a.tools = append(a.tools, oaiToolCall{Index: tc.Index})
	}
	t := &a.tools[pos]
	if tc.ID != "" {
		t.ID = tc.ID
	}
	if tc.Type != "" {
		t.Type = tc.Type
	}
	if tc.Function.Name != "" {
		t.Function.Name = tc.Function.Name
	}
	t.Function.Arguments += tc.Function.Arguments
}

// writeAnthropicMessage writes the non-streaming Anthropic message body. Both
// non-streaming paths — the plain upstream call and the internally-streamed one
// used when Mainspring owns the stop sequences — end here, so the wire shape is
// defined exactly once.
func writeAnthropicMessage(w http.ResponseWriter, model, content string, toolCalls []oaiToolCall,
	finish, hit string, prompt, completion int64, exact bool) {
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
	if exact {
		// We own the writer here, so surface usage as headers too (set before body).
		w.Header().Set("X-Mainspring-Tokens-Input", strconv.FormatInt(prompt, 10))
		w.Header().Set("X-Mainspring-Tokens-Output", strconv.FormatInt(completion, 10))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            fmt.Sprintf("msg_%d", time.Now().UnixNano()),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   stopReasonFor(finish, hit),
		"stop_sequence": stopSequenceField(hit),
		"usage":         map[string]int64{"input_tokens": prompt, "output_tokens": completion},
	})
}

// messagesJSON does a non-streaming upstream call and writes the Anthropic body.
// It returns the real prompt/completion token counts and whether the upstream
// supplied a usage object.
func (s *Server) messagesJSON(w http.ResponseWriter, ctx context.Context, baseURL string, oaiBody []byte, model string, stops []string) messagesResult {
	if sw := newStopWatch(stops); sw != nil {
		return s.messagesJSONStreamed(w, ctx, baseURL, ownedStopBody(oaiBody, true), model, sw)
	}
	var exact bool
	resp, retries, err := s.postUpstreamRetrying(ctx, baseURL+"/v1/chat/completions", oaiBody)
	if err != nil {
		writeUpstreamCallFailure(w, ctx, err)
		return messagesResult{retries: retries}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		// Fail loud: an upstream error body decodes into zero choices, which would
		// otherwise be served as a well-formed empty 200 Anthropic message.
		writeUpstreamFailure(w, resp)
		return messagesResult{retries: retries}
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
		return messagesResult{retries: retries}
	}
	content, finish := "", ""
	var toolCalls []oaiToolCall
	if len(oai.Choices) > 0 {
		content = oai.Choices[0].Message.Content
		finish = oai.Choices[0].FinishReason
		toolCalls = oai.Choices[0].Message.ToolCalls
	}
	// No stop sequences on this path (messagesJSON routes those to the streamed
	// variant), so nothing can have matched: hit is empty.
	exact = oai.Usage.CompletionTokens > 0 || oai.Usage.PromptTokens > 0
	writeAnthropicMessage(w, model, content, toolCalls, finish, "",
		oai.Usage.PromptTokens, oai.Usage.CompletionTokens, exact)
	return messagesResult{prompt: oai.Usage.PromptTokens, completion: oai.Usage.CompletionTokens, exact: exact, retries: retries}
}

// messagesJSONStreamed answers a non-streaming /v1/messages request by streaming
// from the upstream and buffering the result. It exists for one reason: when the
// caller supplied stop_sequences, Mainspring — not the engine — decides where the
// generation ends (see stopWatch), and only a stream can be cut off at the match.
// Because nothing is written until the whole message is in hand, this path can
// still fail loud with a real status when the stream breaks, which the SSE path
// cannot.
func (s *Server) messagesJSONStreamed(w http.ResponseWriter, ctx context.Context, baseURL string, oaiBody []byte, model string, sw *stopWatch) messagesResult {
	resp, retries, err := s.postUpstreamRetrying(ctx, baseURL+"/v1/chat/completions", oaiBody)
	if err != nil {
		writeUpstreamCallFailure(w, ctx, err)
		return messagesResult{retries: retries}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		writeUpstreamFailure(w, resp)
		return messagesResult{retries: retries}
	}

	var acc oaiAccum
	var prompt, completion, deltas int64
	var exact bool
	finish := "stop"
	stopped, scanErr := scanOAIStream(resp.Body, func(c *oaiStreamChunk) bool {
		if c.Usage != nil {
			prompt, completion, exact = c.Usage.PromptTokens, c.Usage.CompletionTokens, true
		}
		if len(c.Choices) == 0 {
			return true
		}
		ch := c.Choices[0]
		if txt := ch.Delta.Content; txt != "" {
			deltas++
			emit, matched := sw.feed(txt)
			acc.text.WriteString(emit)
			if matched {
				return false
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			deltas++
			acc.tool(tc)
		}
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
		return true
	})
	if !stopped {
		// Nothing matched, so text held back as a possible partial match is real
		// output and must not be dropped.
		acc.text.WriteString(sw.flush())
		if scanErr != nil || ctx.Err() != nil {
			switch {
			case clientGone(ctx):
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				writeErr(w, codeTimeout, "request timed out")
			case scanErr != nil:
				writeErr(w, codeUpstreamError, "upstream stream ended prematurely: "+scanErr.Error())
			default:
				writeErr(w, codeInternal, "stream cancelled before completion")
			}
			return messagesResult{retries: retries, backendFailed: !clientGone(ctx)}
		}
	}
	if !exact {
		completion = deltas
		prompt = stoppedPromptTokens(prompt, sw, oaiBody)
	}
	writeAnthropicMessage(w, model, acc.text.String(), acc.tools, finish, sw.hitSeq(), prompt, completion, exact)
	return messagesResult{prompt: prompt, completion: completion, exact: exact, retries: retries}
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
func (s *Server) messagesStream(w http.ResponseWriter, ctx context.Context, baseURL string, oaiBody []byte, model string, stops []string) messagesResult {
	var prompt, completion int64
	var exact bool
	sw := newStopWatch(stops)
	if sw != nil {
		// Already a stream, so nothing to promote — just keep the stop set away from
		// the engine so the match survives long enough for us to see it.
		oaiBody = ownedStopBody(oaiBody, false)
	}
	resp, retries, err := s.postUpstreamRetrying(ctx, baseURL+"/v1/chat/completions", oaiBody)
	if err != nil {
		writeUpstreamCallFailure(w, ctx, err)
		return messagesResult{retries: retries}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		// Fail loud before any SSE byte goes out: once the stream is open the status
		// can no longer be corrected.
		writeUpstreamFailure(w, resp)
		return messagesResult{retries: retries}
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
	stopped, scanErr := scanOAIStream(resp.Body, func(c *oaiStreamChunk) bool {
		// The include_usage final chunk carries usage with an empty choices list.
		if c.Usage != nil {
			prompt, completion, exact = c.Usage.PromptTokens, c.Usage.CompletionTokens, true
		}
		if len(c.Choices) == 0 {
			return true
		}
		ch := c.Choices[0]
		if txt := ch.Delta.Content; txt != "" {
			deltas++
			// Only the part that cannot be the start of a stop sequence goes out; the
			// rest is held until the next fragment resolves it.
			emit, matched := sw.feed(txt)
			if emit != "" {
				st.textDelta(emit)
			}
			if matched {
				return false
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			deltas++
			st.toolDelta(tc)
		}
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
		return true
	})
	if !stopped {
		// Nothing matched, so text held back as a possible partial match is real
		// output and must not be dropped.
		if tail := sw.flush(); tail != "" {
			st.textDelta(tail)
		}
	}

	// Prefer real usage from the upstream include_usage chunk; else the fragment
	// count is the best available output estimate.
	if !exact {
		completion = deltas
		prompt = stoppedPromptTokens(prompt, sw, oaiBody)
	}

	// Fail loud on a truncated stream: a cut connection or an expired deadline must
	// not be closed with a fabricated end_turn, which a client cannot tell apart
	// from a complete answer. Close any open block, then emit an Anthropic `error`
	// event instead of message_delta/message_stop. The status is already 200 on the
	// wire by now, so this error event is the only signal the client will get — and
	// the returned flag is the only signal the breaker will get.
	//
	// `stopped` excludes the one case that looks identical from here but is not a
	// failure at all: we closed the stream ourselves on a stop-sequence match.
	if !stopped && (scanErr != nil || ctx.Err() != nil) {
		st.closeOpen()
		switch {
		case scanErr != nil:
			streamError(send, codeUpstreamError, "upstream stream ended prematurely: "+scanErr.Error())
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			streamError(send, codeTimeout, "stream exceeded the per-request timeout")
		default:
			streamError(send, codeInternal, "stream cancelled before completion")
		}
		// A broken upstream or an exceeded deadline is the backend's fault. A caller
		// that cancelled its own request is not — charging that to the breaker would
		// let clients trip the circuit for every tenant by disconnecting.
		return messagesResult{prompt: prompt, completion: completion, exact: exact,
			backendFailed: !errors.Is(ctx.Err(), context.Canceled), retries: retries}
	}

	if !st.started {
		// No content at all: emit an empty text block so the message is well-formed.
		st.textDelta("")
	}
	st.closeOpen()
	hit := sw.hitSeq()
	send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stopReasonFor(finish, hit), "stop_sequence": stopSequenceField(hit)},
		"usage": map[string]int64{"input_tokens": prompt, "output_tokens": completion}})
	send("message_stop", map[string]any{"type": "message_stop"})
	return messagesResult{prompt: prompt, completion: completion, exact: exact, retries: retries}
}

// messagesResult reports what a /v1/messages generation did, beyond what the
// status can express — the Anthropic counterpart to proxyResult.
type messagesResult struct {
	prompt, completion int64
	exact              bool
	backendFailed      bool // the break was the backend's fault, not a caller's abort
	retries            int  // extra upstream attempts incurred
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
	_, typ := apierr.Meta(code)
	send("error", map[string]any{"type": "error", "error": map[string]string{
		"type": typ, "code": string(code), "message": msg,
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
// postUpstreamRetrying sends body to url with the same bounded retry policy the
// OpenAI path uses: transient connection errors and retryable statuses (502/503/
// 504) are retried with exponential backoff, up to s.retryMax extra attempts.
//
// Retry is only safe because it is decided BEFORE any byte reaches the client,
// and both callers check resp.StatusCode before writing anything — including the
// streaming one, which has not yet emitted message_start. Retrying after a frame
// has gone out would corrupt the stream, so this must never be called once a
// response is committed. It returns the number of extra attempts incurred.
func (s *Server) postUpstreamRetrying(ctx context.Context, url string, body []byte) (*http.Response, int, error) {
	attempts := s.retryMax + 1
	for attempt := 0; ; attempt++ {
		if attempt > 0 && !sleepBackoff(ctx, s.retryBackoff, attempt) {
			return nil, attempt, ctx.Err()
		}
		last := attempt == attempts-1

		resp, err := postJSON(ctx, url, body)
		if err != nil {
			// A dead or cancelled caller is not a transient upstream blip.
			if last || ctx.Err() != nil {
				return nil, attempt, err
			}
			continue
		}
		if !last && retryableStatus(resp.StatusCode) {
			// Drain a bounded amount so the connection can be reused, then retry.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			continue
		}
		return resp, attempt, nil
	}
}

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
