package proxy

import (
	"encoding/json"
	"testing"
)

func TestNormalizeResponsesRequest(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-test",
		"input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"rules","prompt_cache_breakpoint":{"type":"ephemeral"}}]}],
		"stream":false,
		"store":true,
		"max_output_tokens":100,
		"temperature":0.5,
		"user":"someone",
		"tools":[{"type":"web_search_preview"}]
	}`)
	result, stream, model, err := normalizeResponsesRequest(raw)
	if err != nil {
		t.Fatalf("normalizeResponsesRequest() error = %v", err)
	}
	if stream {
		t.Fatal("requested stream = true, want false")
	}
	if model != "gpt-test" {
		t.Fatalf("model = %q", model)
	}
	var body map[string]any
	if err = json.Unmarshal(result, &body); err != nil {
		t.Fatal(err)
	}
	if body["stream"] != true || body["store"] != false || body["parallel_tool_calls"] != true {
		t.Fatalf("required fields not normalized: %s", result)
	}
	for _, field := range []string{"max_output_tokens", "temperature", "user"} {
		if _, exists := body[field]; exists {
			t.Fatalf("unsupported field %q was forwarded", field)
		}
	}
	input := body["input"].([]any)[0].(map[string]any)
	if input["role"] != "developer" {
		t.Fatalf("system role = %v, want developer", input["role"])
	}
	content := input["content"].([]any)[0].(map[string]any)
	if _, exists := content["prompt_cache_breakpoint"]; exists {
		t.Fatal("prompt_cache_breakpoint was forwarded")
	}
	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "web_search" {
		t.Fatalf("tool type = %v, want web_search", tool["type"])
	}
}

func TestConvertChatRequest(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-test",
		"stream":true,
		"stream_options":{"include_usage":true},
		"messages":[
			{"role":"system","content":"Be concise."},
			{"role":"user","content":"hello"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"result"}
		],
		"tools":[{"type":"function","function":{"name":"lookup","description":"Lookup","parameters":{"type":"object"}}}]
	}`)
	result, stream, usage, model, err := convertChatRequest(raw)
	if err != nil {
		t.Fatalf("convertChatRequest() error = %v", err)
	}
	if !stream || !usage || model != "gpt-test" {
		t.Fatalf("metadata = stream:%v usage:%v model:%q", stream, usage, model)
	}
	var body map[string]any
	if err = json.Unmarshal(result, &body); err != nil {
		t.Fatal(err)
	}
	if body["instructions"] != "Be concise." {
		t.Fatalf("instructions = %v", body["instructions"])
	}
	input := body["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input count = %d, want 3", len(input))
	}
	if input[1].(map[string]any)["type"] != "function_call" || input[2].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("tool history was not converted: %s", result)
	}
	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "lookup" {
		t.Fatalf("tool = %#v", tool)
	}
}

func TestConvertChatResponse(t *testing.T) {
	raw := []byte(`{
		"id":"resp_1","model":"gpt-test","created_at":100,
		"output":[
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}
		],
		"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14}
	}`)
	result, err := convertChatResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(result, &body); err != nil {
		t.Fatal(err)
	}
	choice := body["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	if message["content"] != "done" || len(message["tool_calls"].([]any)) != 1 {
		t.Fatalf("message = %#v", message)
	}
}

func TestRequestConvertersRejectTrailingJSON(t *testing.T) {
	if _, _, _, err := normalizeResponsesRequest([]byte(`{"model":"gpt-test","input":"hello"}{}`)); err == nil {
		t.Fatal("normalizeResponsesRequest() accepted multiple JSON values")
	}
	if _, _, _, _, err := convertChatRequest([]byte(`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}{}`)); err == nil {
		t.Fatal("convertChatRequest() accepted multiple JSON values")
	}
}
