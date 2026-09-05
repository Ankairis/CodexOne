package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Ankairis/CodexOne/internal/store"
)

func TestPrepareCodexHTTPBodyMatchesUpstreamPolicy(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.6-sol",
		"input":[
			{"type":"message","id":"client-message","role":"user","content":"hello"},
			{"type":"reasoning","id":"orphan-reasoning"}
		],
		"stream":true,
		"store":false,
		"parallel_tool_calls":true,
		"previous_response_id":"resp_old",
		"generate":true,
		"safety_identifier":"legacy",
		"stream_options":{"include_usage":true}
	}`)

	result, err := prepareCodexHTTPBody(raw, "gpt-5.6-sol", "plus", nil)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(result, &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"previous_response_id", "generate", "safety_identifier", "stream_options"} {
		if _, exists := body[field]; exists {
			t.Fatalf("HTTP-only field %q was forwarded: %s", field, result)
		}
	}
	tools := body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "image_generation" || tools[0].(map[string]any)["output_format"] != "png" {
		t.Fatalf("image tool = %#v", tools)
	}
	input := body["input"].([]any)
	if got := input[0].(map[string]any)["id"]; got != "msg_client-message" {
		t.Fatalf("message id = %#v", got)
	}
	if _, exists := input[1].(map[string]any)["id"]; exists {
		t.Fatalf("orphan reasoning id was forwarded: %#v", input[1])
	}
}

func TestCodexImageGenerationToolPolicy(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		plan     string
		headers  http.Header
		body     string
		wantTool bool
	}{
		{name: "paid", model: "gpt-5.6-sol", plan: "plus", body: `{"model":"gpt-5.6-sol","input":[]}`, wantTool: true},
		{name: "free", model: "gpt-5.6-sol", plan: "free", body: `{"model":"gpt-5.6-sol","input":[]}`},
		{name: "spark", model: "gpt-5.3-codex-spark", plan: "plus", body: `{"model":"gpt-5.3-codex-spark","input":[]}`},
		{name: "responses lite header", model: "gpt-5.6-luna", plan: "plus", headers: http.Header{codexResponsesLiteHeader: {"true"}}, body: `{"model":"gpt-5.6-luna","input":[]}`},
		{name: "responses lite metadata", model: "gpt-5.6-luna", plan: "plus", body: `{"model":"gpt-5.6-luna","client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":true},"input":[]}`},
		{name: "existing native tool", model: "gpt-5.6-sol", plan: "plus", body: `{"model":"gpt-5.6-sol","tools":[{"type":"image_generation"}],"input":[]}`, wantTool: true},
		{name: "existing function tool", model: "gpt-5.6-sol", plan: "plus", body: `{"model":"gpt-5.6-sol","tools":[{"type":"function","name":"image_gen.imagegen"}],"input":[]}`, wantTool: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object, err := decodeJSONObject([]byte(test.body))
			if err != nil {
				t.Fatal(err)
			}
			ensureCodexImageGenerationTool(object, test.model, test.plan, test.headers)
			tools, _ := object["tools"].([]any)
			if got := len(tools) > 0; got != test.wantTool {
				t.Fatalf("has image tool = %v, want %v: %#v", got, test.wantTool, tools)
			}
			if len(tools) > 1 {
				t.Fatalf("image tool was duplicated: %#v", tools)
			}
		})
	}
}

func TestPrepareCodexResponsesLiteDisablesParallelTools(t *testing.T) {
	headers := http.Header{codexResponsesLiteHeader: {"true"}}
	result, err := prepareCodexHTTPBody([]byte(`{"model":"gpt-5.6-luna","input":[],"parallel_tool_calls":true}`), "gpt-5.6-luna", "plus", headers)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(result, &body); err != nil {
		t.Fatal(err)
	}
	if body["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %#v", body["parallel_tool_calls"])
	}
	if _, exists := body["tools"]; exists {
		t.Fatalf("responses-lite request gained tools: %s", result)
	}
}

func TestPrepareCodexHTTPBodyRepairsNonArrayTools(t *testing.T) {
	body, err := prepareCodexHTTPBody([]byte(`{"model":"gpt-5.4","input":"hello","tools":"invalid"}`), "gpt-5.4", "plus", nil)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	tools, ok := decoded["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v, want one repaired image_generation tool", decoded["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "image_generation" || tool["output_format"] != "png" {
		t.Fatalf("tool = %#v", tool)
	}
}

func TestPrepareCodexChatBodyDoesNotInjectImageTool(t *testing.T) {
	body, err := prepareCodexChatBody([]byte(`{
		"model":"gpt-5.6-sol",
		"input":[{"type":"message","id":"client-message","role":"user","content":"hello"}],
		"previous_response_id":"resp_old"
	}`), "gpt-5.6-sol", "plus", nil)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err = json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, exists := decoded["tools"]; exists {
		t.Fatalf("Chat Completions request gained an image tool: %s", body)
	}
	if _, exists := decoded["previous_response_id"]; exists {
		t.Fatalf("Chat Completions request retained HTTP state: %s", body)
	}
	input := decoded["input"].([]any)
	if got := input[0].(map[string]any)["id"]; got != "msg_client-message" {
		t.Fatalf("message id = %#v", got)
	}
}

func TestPrepareCodexWebSocketBodyPreservesIncrementalState(t *testing.T) {
	result, err := prepareCodexWebSocketBody([]byte(`{
		"model":"gpt-5.6-sol",
		"type":"response.create",
		"previous_response_id":"resp_1",
		"safety_identifier":"legacy",
		"parallel_tool_calls":true,
		"input":[]
	}`), "gpt-5.6-sol", "free", nil)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(result, &body); err != nil {
		t.Fatal(err)
	}
	if body["previous_response_id"] != "resp_1" || body["type"] != "response.create" {
		t.Fatalf("incremental state was changed: %s", result)
	}
	if _, exists := body["safety_identifier"]; exists {
		t.Fatalf("safety_identifier was forwarded: %s", result)
	}
	if body["parallel_tool_calls"] != true {
		t.Fatalf("websocket parallel_tool_calls = %#v", body["parallel_tool_calls"])
	}
}

func TestSanitizeCodexReasoningAndInputIDs(t *testing.T) {
	validSignatureBytes := make([]byte, 73)
	validSignatureBytes[0] = 0x80
	validSignature := base64.RawURLEncoding.EncodeToString(validSignatureBytes)
	longID := "foreign-" + strings.Repeat("x", 80)
	object := map[string]any{
		"store": false,
		"input": []any{
			map[string]any{"type": "reasoning", "id": "invalid", "encrypted_content": "not-a-signature"},
			map[string]any{"type": "reasoning", "id": "rs_valid", "encrypted_content": validSignature},
			map[string]any{"type": "reasoning", "id": strings.Repeat("r", 65), "encrypted_content": validSignature},
			map[string]any{"type": "custom_tool_call", "id": longID},
		},
	}

	sanitizeCodexInputItems(object)
	items := object["input"].([]any)
	if len(items) != 3 {
		t.Fatalf("input count = %d, want 3: %#v", len(items), items)
	}
	invalid := items[0].(map[string]any)
	if _, exists := invalid["id"]; exists {
		t.Fatalf("invalid reasoning id remained: %#v", invalid)
	}
	if _, exists := invalid["encrypted_content"]; exists {
		t.Fatalf("invalid encrypted_content remained: %#v", invalid)
	}
	valid := items[1].(map[string]any)
	if valid["id"] != "rs_valid" || valid["encrypted_content"] != validSignature {
		t.Fatalf("valid reasoning changed: %#v", valid)
	}
	customID, _ := items[2].(map[string]any)["id"].(string)
	if !strings.HasPrefix(customID, "ctc_") || len([]rune(customID)) > codexInputItemIDLimit {
		t.Fatalf("custom tool id = %q", customID)
	}
}

func TestSanitizeCodexInputIDsArePrivateAndReversible(t *testing.T) {
	ctx := WithAPIKey(context.Background(), store.APIKey{ID: "key_private"})
	body := []byte(`{"model":"gpt-test","input":[{"type":"message","id":"tenant/item-42","role":"user","content":"hello"}]}`)
	prepared, identity, err := prepareRequestIdentity(ctx, nil, body, true)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err = prepareCodexHTTPBody(prepared, "gpt-test", "free", nil, identity)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Input []struct {
			ID string `json:"id"`
		} `json:"input"`
	}
	if err = json.Unmarshal(prepared, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Input) != 1 || !strings.HasPrefix(request.Input[0].ID, "msg_") || strings.Contains(request.Input[0].ID, "tenant") {
		t.Fatalf("normalized item ID = %#v", request.Input)
	}
	exposed := identity.exposePayload([]byte(`{"item_id":"` + request.Input[0].ID + `"}`))
	if string(exposed) != `{"item_id":"tenant/item-42"}` {
		t.Fatalf("restored item ID = %s", exposed)
	}
}

func TestSanitizeCodexOverlongCanonicalIDIsReversible(t *testing.T) {
	ctx := WithAPIKey(context.Background(), store.APIKey{ID: "key_private"})
	clientID := "msg_" + strings.Repeat("x", 80)
	body := []byte(fmt.Sprintf(`{"model":"gpt-test","input":[{"type":"message","id":%q,"role":"user","content":"hello"}]}`, clientID))
	prepared, identity, err := prepareRequestIdentity(ctx, nil, body, true)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err = prepareCodexHTTPBody(prepared, "gpt-test", "free", nil, identity)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Input []struct {
			ID string `json:"id"`
		} `json:"input"`
	}
	if err = json.Unmarshal(prepared, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Input) != 1 || len([]rune(request.Input[0].ID)) > codexInputItemIDLimit || request.Input[0].ID == clientID {
		t.Fatalf("normalized overlong ID = %#v", request.Input)
	}
	exposed := identity.exposePayload([]byte(`{"item_id":"` + request.Input[0].ID + `"}`))
	if string(exposed) != `{"item_id":"`+clientID+`"}` {
		t.Fatalf("restored overlong item ID = %s", exposed)
	}
}
