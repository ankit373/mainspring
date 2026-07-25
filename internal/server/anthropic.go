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
// stop_sequences, streaming). Tool use and images are not yet translated.

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        json.RawMessage    `json:"system,omitempty"` // string or []{type,text}
	Messages      []anthropicMessage `json:"messages"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []{type,text}
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
		msgs = append(msgs, map[string]any{"role": m.Role, "content": rawToText(m.Content)})
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
	return json.Marshal(oai)
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
				Content string `json:"content"`
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
	if len(oai.Choices) > 0 {
		content = oai.Choices[0].Message.Content
		finish = oai.Choices[0].FinishReason
	}
	out := map[string]any{
		"id":            fmt.Sprintf("msg_%d", time.Now().UnixNano()),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []map[string]any{{"type": "text", "text": content}},
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

// messagesStream translates the upstream OpenAI SSE stream into Anthropic SSE
// events and returns the number of text deltas emitted.
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
	send("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""}})

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
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		if c := chunk.Choices[0].Delta.Content; c != "" {
			deltas++
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": c}})
		}
		if fr := chunk.Choices[0].FinishReason; fr != "" {
			finish = fr
		}
	}

	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": mapStopReason(finish), "stop_sequence": nil},
		"usage": map[string]int64{"output_tokens": deltas}})
	send("message_stop", map[string]any{"type": "message_stop"})
	return deltas
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
