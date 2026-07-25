package server

import (
	"encoding/json"
	"testing"
)

// decodeOAI unmarshals a translated OpenAI request body for assertions.
func decodeOAI(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("translated body is not JSON: %v\n%s", err, b)
	}
	return m
}

func TestToOpenAIRequestTools(t *testing.T) {
	req := anthropicRequest{
		Model:     "m1",
		MaxTokens: 64,
		Messages:  []anthropicMessage{{Role: "user", Content: json.RawMessage(`"what is the weather"`)}},
		Tools: []anthropicTool{{
			Name:        "get_weather",
			Description: "Look up weather",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
		ToolChoice: json.RawMessage(`{"type":"tool","name":"get_weather"}`),
	}
	body, err := toOpenAIRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeOAI(t, body)

	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools not translated: %v", m["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("tool name = %v", fn["name"])
	}
	if _, ok := fn["parameters"]; !ok {
		t.Fatal("tool parameters (input_schema) dropped")
	}

	tc, ok := m["tool_choice"].(map[string]any)
	if !ok || tc["type"] != "function" {
		t.Fatalf("tool_choice not translated to function form: %v", m["tool_choice"])
	}
}

func TestToolChoiceMapping(t *testing.T) {
	if got := toolChoiceToOpenAI(json.RawMessage(`{"type":"auto"}`)); got != "auto" {
		t.Fatalf("auto => %v", got)
	}
	if got := toolChoiceToOpenAI(json.RawMessage(`{"type":"any"}`)); got != "required" {
		t.Fatalf("any => %v", got)
	}
	obj, ok := toolChoiceToOpenAI(json.RawMessage(`{"type":"tool","name":"lookup"}`)).(map[string]any)
	if !ok || obj["type"] != "function" {
		t.Fatalf("tool => %v, want function object", obj)
	}
	if fn, _ := obj["function"].(map[string]any); fn["name"] != "lookup" {
		t.Fatalf("tool name = %v", obj["function"])
	}
	if toolChoiceToOpenAI(nil) != nil {
		t.Fatal("empty tool_choice should map to nil")
	}
}

func TestAnthropicMessageToolResult(t *testing.T) {
	// Assistant turn with text + tool_use.
	asst := anthropicMessage{Role: "assistant", Content: json.RawMessage(
		`[{"type":"text","text":"let me check"},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"paris"}}]`)}
	got, err := anthropicMessageToOpenAI(asst)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("assistant turn => %d messages, want 1", len(got))
	}
	calls, ok := got[0]["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_use not mapped to tool_calls: %v", got[0])
	}
	if calls[0]["id"] != "toolu_1" {
		t.Fatalf("tool call id = %v", calls[0]["id"])
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"paris"}` {
		t.Fatalf("bad function payload: %v", fn)
	}

	// User turn carrying a tool_result.
	usr := anthropicMessage{Role: "user", Content: json.RawMessage(
		`[{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny, 24C"}]`)}
	got, err = anthropicMessageToOpenAI(usr)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["role"] != "tool" {
		t.Fatalf("tool_result => %v, want a role:tool message", got)
	}
	if got[0]["tool_call_id"] != "toolu_1" || got[0]["content"] != "sunny, 24C" {
		t.Fatalf("bad tool message: %v", got[0])
	}
}

func TestImageSourceToURL(t *testing.T) {
	if got := imageSourceToURL(&imageSource{Type: "base64", MediaType: "image/png", Data: "AAAA"}); got != "data:image/png;base64,AAAA" {
		t.Fatalf("base64 => %q", got)
	}
	if got := imageSourceToURL(&imageSource{Type: "url", URL: "https://x/i.png"}); got != "https://x/i.png" {
		t.Fatalf("url => %q", got)
	}
	if imageSourceToURL(nil) != "" || imageSourceToURL(&imageSource{Type: "base64"}) != "" {
		t.Fatal("incomplete/nil source should yield empty url")
	}
}

func TestAnthropicMessageImageContent(t *testing.T) {
	// A user turn with text + a base64 image becomes an OpenAI multimodal content
	// array, preserving order (text part first, then image_url).
	msg := anthropicMessage{Role: "user", Content: json.RawMessage(
		`[{"type":"text","text":"what is this?"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"Zm9v"}}]`)}
	got, err := anthropicMessageToOpenAI(msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["role"] != "user" {
		t.Fatalf("expected one user message, got %v", got)
	}
	parts, ok := got[0]["content"].([]map[string]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content should be a 2-part array: %v", got[0]["content"])
	}
	if parts[0]["type"] != "text" || parts[1]["type"] != "image_url" {
		t.Fatalf("parts out of order or wrong type: %v", parts)
	}
	iu := parts[1]["image_url"].(map[string]any)
	if iu["url"] != "data:image/jpeg;base64,Zm9v" {
		t.Fatalf("image_url = %v", iu["url"])
	}

	// A text-only user turn stays a plain string (backward compatible).
	txt := anthropicMessage{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)}
	g2, _ := anthropicMessageToOpenAI(txt)
	if _, isStr := g2[0]["content"].(string); !isStr {
		t.Fatalf("text-only content should stay a string, got %T", g2[0]["content"])
	}
}

func TestArgsToInput(t *testing.T) {
	if string(argsToInput("")) != "{}" {
		t.Fatal("empty args should become {}")
	}
	if string(argsToInput(`{"a":1}`)) != `{"a":1}` {
		t.Fatal("valid JSON args should pass through")
	}
	// Invalid JSON must not corrupt the payload — it is wrapped as a string.
	got := argsToInput(`not json`)
	if !json.Valid(got) {
		t.Fatalf("argsToInput produced invalid JSON: %s", got)
	}
}
