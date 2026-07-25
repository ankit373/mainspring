package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	if !s.sched.Known(req.Model) {
		writeError(w, http.StatusNotFound, "model not found: "+req.Model)
		return
	}

	tenant, _ := auth.FromContext(r.Context())
	if !s.auth.AllowTokens(tenant) {
		writeError(w, http.StatusTooManyRequests, "token budget exceeded")
		return
	}
	release, ok := s.gate.acquire(r.Context(), req.Model)
	if !ok {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "server busy: too many concurrent requests for "+req.Model)
		return
	}
	defer release()

	runner, err := s.sched.EnsureLoaded(r.Context(), req.Model)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "load model "+req.Model+": "+err.Error())
		return
	}
	for k, v := range s.failLoudHeaders(r.Context(), runner) {
		w.Header().Set(k, v)
	}

	oaiBody, err := toOpenAIRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	start := time.Now()
	var tokens int64
	if req.Stream {
		tokens = s.messagesStream(w, r.Context(), runner.BaseURL(), oaiBody, req.Model)
	} else {
		tokens = s.messagesJSON(w, r.Context(), runner.BaseURL(), oaiBody, req.Model)
	}

	s.auth.AddTokens(tenant, tokens)
	if s.metrics != nil {
		s.metrics.Record(metrics.Event{
			Time: start, Model: req.Model, Tenant: auth.TenantOf(r.Context()),
			Status: http.StatusOK, Stream: req.Stream,
			DurationMs: float64(time.Since(start).Microseconds()) / 1000.0,
			TokensEst:  tokens,
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

	for _, b := range blocks {
		switch b.Type {
		case "text", "":
			text.WriteString(b.Text)
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
			// Each tool_result becomes its own OpenAI tool message.
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": b.ToolUseID,
				"content":      rawToText(b.Content),
			})
		}
	}

	// Assistant turn with text and/or tool calls.
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

// messagesJSON does a non-streaming upstream call and returns the Anthropic body.
func (s *Server) messagesJSON(w http.ResponseWriter, ctx context.Context, baseURL string, oaiBody []byte, model string) int64 {
	resp, err := postJSON(ctx, baseURL+"/v1/chat/completions", oaiBody)
	if err != nil {
		writeError(w, http.StatusBadGateway, "backend request failed: "+err.Error())
		return 0
	}
	defer resp.Body.Close()
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
		return 0
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
	writeJSON(w, http.StatusOK, out)
	if oai.Usage.CompletionTokens > 0 {
		return oai.Usage.CompletionTokens
	}
	return 0
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
// events and returns the number of output tokens (deltas + tool-call fragments).
func (s *Server) messagesStream(w http.ResponseWriter, ctx context.Context, baseURL string, oaiBody []byte, model string) int64 {
	resp, err := postJSON(ctx, baseURL+"/v1/chat/completions", oaiBody)
	if err != nil {
		writeError(w, http.StatusBadGateway, "backend request failed: "+err.Error())
		return 0
	}
	defer resp.Body.Close()

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
		}
		if json.Unmarshal([]byte(data), &chunk) != nil || len(chunk.Choices) == 0 {
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

	if !st.started {
		// No content at all: emit an empty text block so the message is well-formed.
		st.textDelta("")
	}
	st.closeOpen()
	send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": mapStopReason(finish), "stop_sequence": nil},
		"usage": map[string]int64{"output_tokens": deltas}})
	send("message_stop", map[string]any{"type": "message_stop"})
	return deltas
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
	return proxyClient.Do(req)
}
