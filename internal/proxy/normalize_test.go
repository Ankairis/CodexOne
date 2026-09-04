package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestNormalizeResponsesReasoningEffortCompatibility(t *testing.T) {
	result, _, _, err := normalizeResponsesRequest([]byte(`{
		"model":"gpt-5.6-sol",
		"input":"hello",
		"reasoning_effort":"maximum"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(result, &body); err != nil {
		t.Fatal(err)
	}
	if _, exists := body["reasoning_effort"]; exists {
		t.Fatalf("flat reasoning_effort was forwarded: %s", result)
	}
	reasoning := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "max" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %#v", reasoning)
	}
	if got := reasoningEffortFromBody(result); got != "max" {
		t.Fatalf("reasoningEffortFromBody() = %q, want max", got)
	}
}

func TestNormalizeResponsesReasoningAliases(t *testing.T) {
	result, _, _, err := normalizeResponsesRequest([]byte(`{
		"model":"gpt-5.6-luna",
		"input":"hello",
		"reasoning":{"effort":"x-high","generate_summary":"detailed"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(result, &body); err != nil {
		t.Fatal(err)
	}
	reasoning := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "xhigh" || reasoning["summary"] != "detailed" {
		t.Fatalf("reasoning = %#v", reasoning)
	}
	if _, exists := reasoning["generate_summary"]; exists {
		t.Fatalf("legacy generate_summary was forwarded: %#v", reasoning)
	}
}

func TestConvertChatRequest(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-test",
		"reasoning_effort":"high",
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
	reasoning := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %#v", reasoning)
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

func TestConvertChatRequestDefaultsReasoningToMedium(t *testing.T) {
	result, _, _, _, err := convertChatRequest([]byte(`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(result, &body); err != nil {
		t.Fatal(err)
	}
	reasoning := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "medium" || reasoning["summary"] != "auto" {
		t.Fatalf("default reasoning = %#v", reasoning)
	}
}

func TestConvertChatResponse(t *testing.T) {
	raw := []byte(`{
		"id":"resp_1","model":"gpt-test","created_at":100,
		"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"checked the inputs"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}
		],
		"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"output_tokens_details":{"reasoning_tokens":3}}
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
	if message["reasoning_content"] != "checked the inputs" {
		t.Fatalf("reasoning_content = %#v", message["reasoning_content"])
	}
	usage := body["usage"].(map[string]any)
	details := usage["completion_tokens_details"].(map[string]any)
	if details["reasoning_tokens"] != float64(3) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestTranslateChatEventPreservesReasoningAndUsage(t *testing.T) {
	recorder := httptest.NewRecorder()
	state := &chatStreamState{model: "gpt-test", created: 100, tools: make(map[int]int), started: time.Now().Add(-time.Second)}
	events := []string{
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-test","created_at":100}}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"check inputs"}`,
		`{"type":"response.reasoning_summary_text.done"}`,
		`{"type":"response.output_text.delta","delta":"done"}`,
		`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-test","reasoning":{"effort":"max"},"usage":{"input_tokens":10,"output_tokens":4,"output_tokens_details":{"reasoning_tokens":3}}}}`,
	}
	for _, event := range events {
		if err := translateChatEvent(recorder, recorder, []byte(event), state, true); err != nil {
			t.Fatal(err)
		}
	}
	output := recorder.Body.String()
	for _, want := range []string{`"reasoning_content":"check inputs"`, `"reasoning_content":"\n\n"`, `"content":"done"`, `"reasoning_tokens":3`, "data: [DONE]"} {
		if !strings.Contains(output, want) {
			t.Fatalf("stream output does not contain %q:\n%s", want, output)
		}
	}
	if !state.usage.HasReasoningTokens || state.usage.ReasoningTokens != 3 {
		t.Fatalf("stream usage = %#v", state.usage)
	}
	if state.usage.EffectiveReasoningEffort != "max" || state.telemetry.FirstReasoningMS == 0 || state.telemetry.FirstOutputMS == 0 {
		t.Fatalf("stream telemetry = usage:%#v telemetry:%#v", state.usage, state.telemetry)
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
