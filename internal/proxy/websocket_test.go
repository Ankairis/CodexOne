package proxy

import (
	"encoding/json"
	"net/http/httptest"
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

func mustJSON(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}
