package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ankairis/CodexOne/internal/store"
	"github.com/gorilla/websocket"
)

func TestWebSocketConversationUsesIncrementalTurnsAndReplayAfterReconnect(t *testing.T) {
	request := httptest.NewRequest("GET", "/v1/responses", nil)
	request = request.WithContext(WithAPIKey(request.Context(), store.APIKey{ID: "key_ws"}))
	conversation := webSocketConversation{}

	first, err := conversation.prepare(request, []byte(`{"type":"response.create","model":"gpt-test","instructions":"be concise","input":"hello"}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if err = conversation.commit(first, []byte(`{"type":"response.completed","response":{"id":"resp_first","output":[{"type":"message","id":"msg_first","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}}`), 1); err != nil {
		t.Fatal(err)
	}

	second, err := conversation.prepare(request, []byte(`{"type":"response.append","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}]}`), true)
	if err != nil {
		t.Fatal(err)
	}
	var incremental struct {
		Type               string            `json:"type"`
		Model              string            `json:"model"`
		Instructions       string            `json:"instructions"`
		PreviousResponseID string            `json:"previous_response_id"`
		PromptCacheKey     string            `json:"prompt_cache_key"`
		Input              []json.RawMessage `json:"input"`
	}
	if err = json.Unmarshal(conversation.bodyFor(second, 1, false), &incremental); err != nil {
		t.Fatal(err)
	}
	if incremental.Type != "response.create" || incremental.Model != "gpt-test" || incremental.Instructions != "be concise" || incremental.PreviousResponseID != "resp_first" {
		t.Fatalf("incremental request = %#v", incremental)
	}
	if len(incremental.Input) != 1 || incremental.PromptCacheKey == "" {
		t.Fatalf("incremental input/cache = %#v", incremental)
	}

	var replay struct {
		PreviousResponseID string            `json:"previous_response_id"`
		Input              []json.RawMessage `json:"input"`
	}
	if err = json.Unmarshal(conversation.bodyFor(second, 2, false), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.PreviousResponseID != "" || len(replay.Input) != 3 {
		t.Fatalf("reconnect replay = %#v", replay)
	}
	var kinds []string
	for _, raw := range replay.Input {
		var item struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		_ = json.Unmarshal(raw, &item)
		kinds = append(kinds, item.Type+":"+item.Role)
	}
	if kinds[0] != "message:user" || kinds[1] != "message:assistant" || kinds[2] != "message:user" {
		t.Fatalf("replay order = %v", kinds)
	}
}

func TestWebSocketConversationReplacesFullTranscriptAfterCompaction(t *testing.T) {
	request := httptest.NewRequest("GET", "/v1/responses", nil)
	request = request.WithContext(WithAPIKey(request.Context(), store.APIKey{ID: "key_ws"}))
	conversation := webSocketConversation{}
	first, err := conversation.prepare(request, []byte(`{"type":"response.create","model":"gpt-test","input":"old"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = conversation.commit(first, []byte(`{"type":"response.completed","response":{"id":"resp_old","output":[{"type":"message","id":"old-answer","role":"assistant","content":[]}]}}`), 1); err != nil {
		t.Fatal(err)
	}
	compact, err := conversation.prepare(request, []byte(`{"type":"response.create","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"compacted history"}]},{"type":"message","id":"compact-answer","role":"assistant","content":[]}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if !compact.reset || compact.continuation {
		t.Fatalf("compacted transcript was not treated as a reset: %#v", compact)
	}
	var payload struct {
		Input []json.RawMessage `json:"input"`
	}
	if err = json.Unmarshal(compact.replay, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Input) != 2 {
		t.Fatalf("compacted replay retained stale history: %s", compact.replay)
	}
}

func TestWebSocketConversationRequiresPendingToolOutputs(t *testing.T) {
	request := httptest.NewRequest("GET", "/v1/responses", nil)
	request = request.WithContext(WithAPIKey(request.Context(), store.APIKey{ID: "key_ws"}))
	conversation := webSocketConversation{}
	first, err := conversation.prepare(request, []byte(`{"type":"response.create","model":"gpt-test","input":"run"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = conversation.commit(first, []byte(`{"type":"response.completed","response":{"id":"resp_tool","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"run","arguments":"{}"}]}}`), 1); err != nil {
		t.Fatal(err)
	}
	if _, err = conversation.prepare(request, []byte(`{"type":"response.append","input":[{"type":"message","role":"user","content":"skip tool"}]}`), false); err == nil {
		t.Fatal("continuation accepted input without the pending tool result")
	}
	if _, err = conversation.prepare(request, []byte(`{"type":"response.append","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`), false); err != nil {
		t.Fatalf("valid tool continuation failed: %v", err)
	}
}

func TestWebSocketConversationCarriesToolsIntoAppend(t *testing.T) {
	request := httptest.NewRequest("GET", "/v1/responses", nil)
	request = request.WithContext(WithAPIKey(request.Context(), store.APIKey{ID: "key_ws"}))
	conversation := webSocketConversation{}
	first, err := conversation.prepare(request, []byte(`{
		"type":"response.create",
		"model":"gpt-test",
		"input":"hello",
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"tool_choice":"required",
		"parallel_tool_calls":true
	}`), false, "free")
	if err != nil {
		t.Fatal(err)
	}
	if err = conversation.commit(first, []byte(`{"type":"response.completed","response":{"id":"resp_first","output":[{"type":"message","id":"msg_first","role":"assistant","content":[]}]}}`), 1); err != nil {
		t.Fatal(err)
	}
	second, err := conversation.prepare(request, []byte(`{"type":"response.append","input":[{"type":"message","role":"user","content":"again"}]}`), false, "free")
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Tools             []json.RawMessage `json:"tools"`
		ToolChoice        string            `json:"tool_choice"`
		ParallelToolCalls bool              `json:"parallel_tool_calls"`
	}
	if err = json.Unmarshal(second.incremental, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Tools) != 1 || payload.ToolChoice != "required" || !payload.ParallelToolCalls {
		t.Fatalf("append lost tool settings: %s", second.incremental)
	}
}

func TestWebSocketAppendWithTranscriptItemsRemainsIncremental(t *testing.T) {
	request := httptest.NewRequest("GET", "/v1/responses", nil)
	request = request.WithContext(WithAPIKey(request.Context(), store.APIKey{ID: "key_ws"}))
	conversation := webSocketConversation{}
	first, err := conversation.prepare(request, []byte(`{"type":"response.create","model":"gpt-test","input":"hello"}`), false, "free")
	if err != nil {
		t.Fatal(err)
	}
	if err = conversation.commit(first, []byte(`{"type":"response.completed","response":{"id":"resp_first","output":[{"type":"message","id":"msg_first","role":"assistant","content":[]}]}}`), 1); err != nil {
		t.Fatal(err)
	}
	if _, err = conversation.prepare(request, []byte(`{"type":"response.append","previous_response_id":"wrong","input":[{"type":"message","role":"assistant","content":[]}]}`), false, "free"); err == nil {
		t.Fatal("assistant append bypassed previous_response_id validation")
	}
	appendPlan, err := conversation.prepare(request, []byte(`{"type":"response.append","input":[{"type":"message","role":"assistant","content":[]}]}`), false, "free")
	if err != nil {
		t.Fatal(err)
	}
	if !appendPlan.continuation || appendPlan.reset {
		t.Fatalf("response.append was treated as a reset: %#v", appendPlan)
	}
	var payload struct {
		PreviousResponseID string `json:"previous_response_id"`
	}
	if err = json.Unmarshal(appendPlan.incremental, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.PreviousResponseID != "resp_first" {
		t.Fatalf("previous_response_id = %q", payload.PreviousResponseID)
	}
}

func TestWebSocketConversationDoesNotRetainInjectedImageTool(t *testing.T) {
	request := httptest.NewRequest("GET", "/v1/responses", nil)
	request = request.WithContext(WithAPIKey(request.Context(), store.APIKey{ID: "key_ws"}))
	conversation := webSocketConversation{}
	first, err := conversation.prepare(request, []byte(`{"type":"response.create","model":"gpt-test","input":"hello"}`), false, "plus")
	if err != nil {
		t.Fatal(err)
	}
	var paid struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err = json.Unmarshal(first.incremental, &paid); err != nil {
		t.Fatal(err)
	}
	if len(paid.Tools) != 1 {
		t.Fatalf("paid turn did not receive image tool: %s", first.incremental)
	}
	if err = conversation.commit(first, []byte(`{"type":"response.completed","response":{"id":"resp_first","output":[{"type":"message","id":"msg_first","role":"assistant","content":[]}]}}`), 1); err != nil {
		t.Fatal(err)
	}
	second, err := conversation.prepare(request, []byte(`{"type":"response.append","model":"gpt-5.3-codex-spark","input":[{"type":"message","role":"user","content":"again"}]}`), false, "plus")
	if err != nil {
		t.Fatal(err)
	}
	var spark map[string]any
	if err = json.Unmarshal(second.incremental, &spark); err != nil {
		t.Fatal(err)
	}
	if _, exists := spark["tools"]; exists {
		t.Fatalf("Spark append retained an injected image tool: %s", second.incremental)
	}
}

func TestWebSocketConversationClearsInstructionsOnReset(t *testing.T) {
	request := httptest.NewRequest("GET", "/v1/responses", nil)
	request = request.WithContext(WithAPIKey(request.Context(), store.APIKey{ID: "key_ws"}))
	conversation := webSocketConversation{}
	first, err := conversation.prepare(request, []byte(`{"type":"response.create","model":"gpt-test","instructions":"old instructions","input":"hello"}`), false, "free")
	if err != nil {
		t.Fatal(err)
	}
	if err = conversation.commit(first, []byte(`{"type":"response.completed","response":{"id":"resp_first","output":[]}}`), 1); err != nil {
		t.Fatal(err)
	}
	reset, err := conversation.prepare(request, []byte(`{"type":"response.create","model":"gpt-test","input":"new conversation"}`), false, "free")
	if err != nil {
		t.Fatal(err)
	}
	if err = conversation.commit(reset, []byte(`{"type":"response.completed","response":{"id":"resp_reset","output":[]}}`), 1); err != nil {
		t.Fatal(err)
	}
	if len(conversation.instructions) != 0 {
		t.Fatalf("reset retained stale instructions: %s", conversation.instructions)
	}
	appendPlan, err := conversation.prepare(request, []byte(`{"type":"response.append","input":[{"type":"message","role":"user","content":"continue"}]}`), false, "free")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(appendPlan.incremental, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["instructions"] == "old instructions" {
		t.Fatalf("append restored stale instructions: %s", appendPlan.incremental)
	}
}

func TestWebSocketConversationPersistsExplicitInstructionClear(t *testing.T) {
	request := httptest.NewRequest("GET", "/v1/responses", nil)
	request = request.WithContext(WithAPIKey(request.Context(), store.APIKey{ID: "key_ws"}))
	conversation := webSocketConversation{}
	first, err := conversation.prepare(request, []byte(`{"type":"response.create","model":"gpt-test","instructions":"old instructions","input":"hello"}`), false, "free")
	if err != nil {
		t.Fatal(err)
	}
	if err = conversation.commit(first, []byte(`{"type":"response.completed","response":{"id":"resp_first","output":[]}}`), 1); err != nil {
		t.Fatal(err)
	}
	clearPlan, err := conversation.prepare(request, []byte(`{"type":"response.append","instructions":"","input":[{"type":"message","role":"user","content":"clear"}]}`), false, "free")
	if err != nil {
		t.Fatal(err)
	}
	if err = conversation.commit(clearPlan, []byte(`{"type":"response.completed","response":{"id":"resp_clear","output":[]}}`), 1); err != nil {
		t.Fatal(err)
	}
	if len(conversation.instructions) != 0 {
		t.Fatalf("explicit clear retained instructions: %s", conversation.instructions)
	}
	appendPlan, err := conversation.prepare(request, []byte(`{"type":"response.append","input":[{"type":"message","role":"user","content":"continue"}]}`), false, "free")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(appendPlan.incremental, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["instructions"] == "old instructions" {
		t.Fatalf("append restored explicitly cleared instructions: %s", appendPlan.incremental)
	}
}

func TestMergeWebSocketBetaHeaderReplacesStaleVersion(t *testing.T) {
	tests := map[string]string{
		"":                                codexResponsesWebSocketBeta,
		"foo=bar":                         "foo=bar," + codexResponsesWebSocketBeta,
		"responses_websockets=2025-01-01": codexResponsesWebSocketBeta,
		"foo=bar, responses_websockets=2025-01-01,other=baz": "foo=bar," + codexResponsesWebSocketBeta + ",other=baz",
		codexResponsesWebSocketBeta:                          codexResponsesWebSocketBeta,
	}
	for input, want := range tests {
		if got := mergeWebSocketBetaHeader(input); got != want {
			t.Errorf("mergeWebSocketBetaHeader(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWebSocketCompressionIsDisabledForBoundedReads(t *testing.T) {
	if responsesWebSocketUpgrader.EnableCompression {
		t.Fatal("downstream WebSocket compression bypasses the decompressed message limit")
	}
	if newCodexWebSocketDialer().EnableCompression {
		t.Fatal("upstream WebSocket compression bypasses the decompressed message limit")
	}
}

func TestMergeWebSocketItemsDeduplicatesIDsAndToolCalls(t *testing.T) {
	items := mergeWebSocketItems(
		[]json.RawMessage{json.RawMessage(`{"type":"function_call","id":"fc_old","call_id":"call_1"}`), json.RawMessage(`{"type":"message","id":"same","role":"assistant","content":"old"}`)},
		[]json.RawMessage{json.RawMessage(`{"type":"function_call","id":"fc_new","call_id":"call_1"}`), json.RawMessage(`{"type":"message","id":"same","role":"assistant","content":"new"}`)},
	)
	if len(items) != 2 || string(items[0]) != `{"type":"function_call","id":"fc_old","call_id":"call_1"}` || string(items[1]) != `{"type":"message","id":"same","role":"assistant","content":"new"}` {
		t.Fatalf("deduplicated items = %s", mustJSON(items))
	}
}

func TestWebSocketRegistryRejectsConnectionsAfterShutdown(t *testing.T) {
	registry := websocketRegistry{}
	registry.closeAll()
	if registry.add(&websocket.Conn{}) {
		t.Fatal("registry accepted a WebSocket after shutdown")
	}
}

func TestDownstreamWebSocketInboxBoundsQueuedBytes(t *testing.T) {
	inbox := &downstreamWebSocketInbox{maxQueuedBytes: 10}
	first := downstreamWebSocketMessage{payload: []byte("123456")}
	second := downstreamWebSocketMessage{payload: []byte("12345")}
	if !inbox.reserve(first) {
		t.Fatal("first message did not fit within the byte budget")
	}
	if inbox.reserve(second) {
		t.Fatal("aggregate queue exceeded its byte budget")
	}
	if got := inbox.queuedBytes.Load(); got != 6 {
		t.Fatalf("queued bytes = %d, want 6", got)
	}
	inbox.release(first)
	if !inbox.reserve(second) {
		t.Fatal("released capacity was not reusable")
	}
	inbox.release(second)
	if got := inbox.queuedBytes.Load(); got != 0 {
		t.Fatalf("queued bytes after release = %d, want 0", got)
	}
}

func TestUpstreamWebSocketBoundsQueuedBytes(t *testing.T) {
	upstream := &upstreamWebSocket{maxQueuedBytes: 10}
	first := upstreamWebSocketFrame{payload: []byte("123456")}
	second := upstreamWebSocketFrame{payload: []byte("12345")}
	if !upstream.reserve(first) {
		t.Fatal("first event did not fit within the byte budget")
	}
	if upstream.reserve(second) {
		t.Fatal("aggregate upstream queue exceeded its byte budget")
	}
	if got := upstream.queuedBytes.Load(); got != 6 {
		t.Fatalf("queued bytes = %d, want 6", got)
	}
	upstream.release(first)
	if !upstream.reserve(second) {
		t.Fatal("released upstream capacity was not reusable")
	}
	upstream.release(second)
	if got := upstream.queuedBytes.Load(); got != 0 {
		t.Fatalf("queued bytes after release = %d, want 0", got)
	}
}

func TestWebSocketConversationRejectsTerminalOutputBeyondReplayBudget(t *testing.T) {
	request := httptest.NewRequest("GET", "/v1/responses", nil)
	request = request.WithContext(WithAPIKey(request.Context(), store.APIKey{ID: "key_ws"}))
	conversation := webSocketConversation{maxReplayBytes: 2 << 10}
	plan, err := conversation.prepare(request, []byte(`{"type":"response.create","model":"gpt-test","input":"hello"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := json.Marshal(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": "resp_large",
			"output": []any{map[string]any{
				"type": "message", "id": "msg_large", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": strings.Repeat("x", 3<<10)}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = conversation.commit(plan, terminal, 1); err == nil {
		t.Fatal("oversized terminal output was retained")
	}
	if conversation.hasCompleted || len(conversation.history) != 0 {
		t.Fatalf("oversized response mutated conversation state: %#v", conversation)
	}
}

func mustJSON(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}
