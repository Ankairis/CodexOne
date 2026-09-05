package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ankairis/CodexOne/internal/codex"
	"github.com/Ankairis/CodexOne/internal/config"
	"github.com/Ankairis/CodexOne/internal/cryptox"
	appLog "github.com/Ankairis/CodexOne/internal/logging"
	"github.com/Ankairis/CodexOne/internal/proxy"
	"github.com/Ankairis/CodexOne/internal/security"
	"github.com/Ankairis/CodexOne/internal/session"
	"github.com/Ankairis/CodexOne/internal/store"
	"github.com/gorilla/websocket"
)

func TestV1ResponsesUsesAPIKeyAndFixedCodexIdentity(t *testing.T) {
	var upstreamBody map[string]any
	var upstreamSessionID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Errorf("upstream path = %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-access" {
			t.Errorf("upstream Authorization = %q", got)
		}
		if got := r.Header.Get("Chatgpt-Account-Id"); got != "acct_single" {
			t.Errorf("upstream account ID = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "codex-tui/0.146.0 (integration-test)" {
			t.Errorf("upstream User-Agent = %q", got)
		}
		if got := r.Header.Get("Originator"); got != "codex-tui" {
			t.Errorf("upstream Originator = %q", got)
		}
		if got := r.Header.Get("Version"); got != "0.146.0" {
			t.Errorf("upstream Version = %q", got)
		}
		upstreamSessionID = r.Header.Get("Session-Id")
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Primary-Used-Percent", "12")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-test\",\"reasoning\":{\"effort\":\"max\"},\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5,\"output_tokens_details\":{\"reasoning_tokens\":1}}}}\n\n")
	}))
	defer upstream.Close()

	handler, database, apiKey, cleanup := testApplication(t, upstream.URL)
	defer cleanup()

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hello"}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hello","stream":false,"store":true,"reasoning_effort":"max"}`))
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("User-Agent", "downstream-client/9.9")
	request.Header.Set("Originator", "untrusted-client")
	request.Header.Set("Session-Id", "session-from-client")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("response status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("X-Codex-Primary-Used-Percent"); got != "12" {
		t.Fatalf("quota header = %q", got)
	}
	var final map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &final); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if final["id"] != "resp_1" {
		t.Fatalf("response body = %s", response.Body.String())
	}
	if upstreamBody["stream"] != true || upstreamBody["store"] != false {
		t.Fatalf("upstream body was not normalized: %#v", upstreamBody)
	}
	reasoning, _ := upstreamBody["reasoning"].(map[string]any)
	if reasoning["effort"] != "max" || reasoning["summary"] != "auto" {
		t.Fatalf("upstream reasoning = %#v", reasoning)
	}
	include, _ := upstreamBody["include"].([]any)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("upstream include = %#v", upstreamBody["include"])
	}
	if upstreamBody["prompt_cache_key"] != upstreamSessionID || upstreamSessionID == "" || upstreamSessionID == "session-from-client" {
		t.Fatalf("upstream identity was not consistently remapped: header=%q body=%#v", upstreamSessionID, upstreamBody["prompt_cache_key"])
	}

	now := time.Now()
	logs, err := database.ListRequestLogs(context.Background(), now.Add(-time.Minute).UnixMilli(), now.Add(time.Minute).UnixMilli(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].InputTokens != 3 || logs[0].OutputTokens != 2 || logs[0].APIKeyID == "" ||
		logs[0].ReasoningEffort != "max" || logs[0].UpstreamReasoningEffort != "max" || logs[0].ReasoningTokens != 1 || logs[0].FirstOutputMS != 0 {
		t.Fatalf("request logs = %#v", logs)
	}
}

func TestV1ResponsesGeneratesStablePromptCacheAndSessionIdentity(t *testing.T) {
	identities := make(chan [2]string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PromptCacheKey string `json:"prompt_cache_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		identities <- [2]string{body.PromptCacheKey, r.Header.Get("Session-Id")}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_identity\",\"model\":\"gpt-test\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	defer upstream.Close()

	handler, _, apiKey, cleanup := testApplication(t, upstream.URL)
	defer cleanup()
	for range 2 {
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hello","stream":false}`))
		request.Header.Set("Authorization", "Bearer "+apiKey)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("response status = %d, body = %s", response.Code, response.Body.String())
		}
	}
	first, second := <-identities, <-identities
	if first[0] == "" || first[0] != first[1] || first != second {
		t.Fatalf("generated identities = %#v and %#v", first, second)
	}
}

func TestV1ResponsesWebSocketRelaysAndAddsIdentity(t *testing.T) {
	type observation struct {
		Authorization string
		AccountID     string
		Beta          string
		SessionID     string
		CacheKey      string
		Type          string
		Model         string
		PreviousID    string
		Stream        bool
		Store         bool
	}
	observed := make(chan observation, 2)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for index, responseID := range []string{"resp_ws", "resp_ws_2"} {
			_, payload, readErr := connection.ReadMessage()
			if readErr != nil {
				return
			}
			var request struct {
				Type           string `json:"type"`
				Model          string `json:"model"`
				PreviousID     string `json:"previous_response_id"`
				PromptCacheKey string `json:"prompt_cache_key"`
				Stream         bool   `json:"stream"`
				Store          bool   `json:"store"`
			}
			if err = json.Unmarshal(payload, &request); err != nil {
				return
			}
			observed <- observation{
				Authorization: r.Header.Get("Authorization"),
				AccountID:     r.Header.Get("Chatgpt-Account-Id"),
				Beta:          r.Header.Get("OpenAI-Beta"),
				SessionID:     r.Header.Get("session_id"),
				CacheKey:      request.PromptCacheKey,
				Type:          request.Type,
				Model:         request.Model,
				PreviousID:    request.PreviousID,
				Stream:        request.Stream,
				Store:         request.Store,
			}
			_ = connection.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"model":"gpt-test","output":[],"usage":{"input_tokens":%d,"output_tokens":1}}}`, responseID, index+2)))
		}
	}))
	defer upstream.Close()

	handler, database, apiKey, cleanup := testApplication(t, upstream.URL)
	defer cleanup()
	server := httptest.NewServer(handler)
	defer server.Close()
	webSocketURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"

	unauthorized, unauthorizedResponse, unauthorizedErr := websocket.DefaultDialer.Dial(webSocketURL, nil)
	if unauthorized != nil {
		_ = unauthorized.Close()
	}
	if unauthorizedErr == nil || unauthorizedResponse == nil || unauthorizedResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized WebSocket response = %#v, error = %v", unauthorizedResponse, unauthorizedErr)
	}
	if unauthorizedResponse.Body != nil {
		_ = unauthorizedResponse.Body.Close()
	}

	headers := http.Header{"Authorization": {"Bearer " + apiKey}}
	connection, response, err := websocket.DefaultDialer.Dial(webSocketURL, headers)
	if err != nil {
		if response != nil && response.Body != nil {
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			t.Fatalf("dial WebSocket: %v: %s", err, body)
		}
		t.Fatal(err)
	}
	defer connection.Close()
	if err = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-test","input":"hello","reasoning":{"effort":"max"}}`)); err != nil {
		t.Fatal(err)
	}
	_, payload, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var event struct {
		Type string `json:"type"`
	}
	if err = json.Unmarshal(payload, &event); err != nil || event.Type != "response.completed" {
		t.Fatalf("event = %s, error = %v", payload, err)
	}
	if err = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.append","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}]}`)); err != nil {
		t.Fatal(err)
	}
	_, payload, err = connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(payload, &event); err != nil || event.Type != "response.completed" {
		t.Fatalf("continuation event = %s, error = %v", payload, err)
	}

	for index := range 2 {
		select {
		case got := <-observed:
			if got.Authorization != "Bearer upstream-access" || got.AccountID != "acct_single" {
				t.Fatalf("upstream auth = %#v", got)
			}
			if got.Beta != codexResponsesWebSocketBetaForTest || got.SessionID == "" || got.SessionID != got.CacheKey {
				t.Fatalf("upstream identity = %#v", got)
			}
			if got.Type != "response.create" || got.Model != "gpt-test" {
				t.Fatalf("upstream WebSocket request = %#v", got)
			}
			if index == 1 && got.PreviousID != "resp_ws" {
				t.Fatalf("continuation request = %#v", got)
			}
			if !got.Stream || got.Store {
				t.Fatalf("upstream request normalization = %#v", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for upstream WebSocket request")
		}
	}
	_ = connection.Close()
	deadline := time.Now().Add(time.Second)
	for {
		logs, logErr := database.ListRequestLogs(context.Background(), time.Now().Add(-time.Minute).UnixMilli(), time.Now().Add(time.Minute).UnixMilli(), 10)
		if logErr != nil {
			t.Fatal(logErr)
		}
		if len(logs) >= 2 {
			if logs[0].Method != http.MethodGet || logs[1].Method != http.MethodGet {
				t.Fatalf("WebSocket turn logs = %#v", logs[:2])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("WebSocket produced %d turn logs, want 2", len(logs))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestV1ResponsesWebSocketRejectsTurnAfterAPIKeyRevocation(t *testing.T) {
	requests := make(chan struct{}, 2)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			if _, _, err = connection.ReadMessage(); err != nil {
				return
			}
			requests <- struct{}{}
			if err = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_ws","output":[]}}`)); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	handler, database, apiKey, cleanup := testApplication(t, upstream.URL)
	defer cleanup()
	server := httptest.NewServer(handler)
	defer server.Close()
	connection, response, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses",
		http.Header{"Authorization": {"Bearer " + apiKey}},
	)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatal(err)
	}
	defer connection.Close()

	if err = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-test","input":"first"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = connection.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("first turn did not reach upstream")
	}
	if revoked, revokeErr := database.RevokeAPIKey(context.Background(), "key_fixture"); revokeErr != nil || !revoked {
		t.Fatalf("revoke API key: revoked=%v err=%v", revoked, revokeErr)
	}

	if err = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.append","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}]}`)); err != nil {
		t.Fatal(err)
	}
	_, payload, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"code":"invalid_api_key"`) || !strings.Contains(string(payload), `"status":401`) {
		t.Fatalf("revoked-key response = %s", payload)
	}
	if _, _, err = connection.ReadMessage(); err == nil {
		t.Fatal("WebSocket remained open after its API key was revoked")
	} else if closeError, ok := err.(*websocket.CloseError); !ok || closeError.Code != websocket.ClosePolicyViolation {
		t.Fatalf("revoked-key close error = %v", err)
	}
	select {
	case <-requests:
		t.Fatal("revoked WebSocket turn reached upstream")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestV1ResponsesWebSocketRebindsWhenCodexAccountChanges(t *testing.T) {
	type observation struct {
		connection int
		accountID  string
	}
	observed := make(chan observation, 3)
	var connectionMu sync.Mutex
	connectionCount := 0
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		connectionMu.Lock()
		connectionCount++
		currentConnection := connectionCount
		connectionMu.Unlock()
		for turn := 0; ; turn++ {
			if _, _, err = connection.ReadMessage(); err != nil {
				return
			}
			observed <- observation{connection: currentConnection, accountID: r.Header.Get("Chatgpt-Account-Id")}
			responseID := fmt.Sprintf("resp_%d_%d", currentConnection, turn)
			if err = connection.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"output":[]}}`, responseID))); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	handler, database, apiKey, cleanup := testApplication(t, upstream.URL)
	defer cleanup()
	server := httptest.NewServer(handler)
	defer server.Close()
	connection, response, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses",
		http.Header{"Authorization": {"Bearer " + apiKey}},
	)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatal(err)
	}
	defer connection.Close()
	nextObservation := func() observation {
		select {
		case value := <-observed:
			return value
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for upstream account binding")
			return observation{}
		}
	}

	if err = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-test","input":"first"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = connection.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	first := nextObservation()
	if first.connection != 1 || first.accountID != "acct_single" {
		t.Fatalf("first upstream binding = %#v", first)
	}

	account, err := database.GetAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	account.ChatGPTAccountID = "acct_replacement"
	account.UpdatedAt++
	if err = database.SaveAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if err = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.append","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, payload, readErr := connection.ReadMessage(); readErr != nil {
		err = readErr
		t.Fatal(err)
	} else if strings.Contains(string(payload), `"type":"error"`) {
		t.Fatalf("replacement turn failed: %s", payload)
	}
	second := nextObservation()
	if second.connection != 2 || second.accountID != "acct_replacement" {
		t.Fatalf("replacement upstream binding = %#v", second)
	}

	if err = database.DeleteAccount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.append","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"third"}]}]}`)); err != nil {
		t.Fatal(err)
	}
	_, payload, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"code":"account_unavailable"`) || !strings.Contains(string(payload), `"status":503`) {
		t.Fatalf("disconnected-account response = %s", payload)
	}
	select {
	case third := <-observed:
		t.Fatalf("disconnected account reached upstream: %#v", third)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestV1ResponsesWebSocketReplaysTranscriptAfterUpstreamReconnect(t *testing.T) {
	type upstreamRequest struct {
		Connection         int
		PreviousResponseID string
		Input              []json.RawMessage
	}
	observed := make(chan upstreamRequest, 2)
	firstClosed := make(chan struct{})
	var connectionNumber int
	var connectionMu sync.Mutex
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connectionMu.Lock()
		connectionNumber++
		current := connectionNumber
		connectionMu.Unlock()
		defer connection.Close()
		_, payload, err := connection.ReadMessage()
		if err != nil {
			return
		}
		var request struct {
			PreviousResponseID string            `json:"previous_response_id"`
			Input              []json.RawMessage `json:"input"`
		}
		if json.Unmarshal(payload, &request) != nil {
			return
		}
		observed <- upstreamRequest{Connection: current, PreviousResponseID: request.PreviousResponseID, Input: request.Input}
		responseID := "resp_first"
		outputID := "msg_first"
		if current == 2 {
			responseID, outputID = "resp_second", "msg_second"
		}
		_ = connection.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"output":[{"type":"message","id":%q,"role":"assistant","content":[]}]}}`, responseID, outputID)))
		if current == 1 {
			_ = connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "idle recycle"), time.Now().Add(time.Second))
			close(firstClosed)
		}
	}))
	defer upstream.Close()

	handler, _, apiKey, cleanup := testApplication(t, upstream.URL)
	defer cleanup()
	server := httptest.NewServer(handler)
	defer server.Close()
	connection, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer " + apiKey}})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatal(err)
	}
	defer connection.Close()
	if err = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-test","input":"first"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err = connection.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstClosed:
	case <-time.After(time.Second):
		t.Fatal("upstream did not close the first socket")
	}
	time.Sleep(30 * time.Millisecond)
	if err = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.append","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, payload, readErr := connection.ReadMessage(); readErr != nil {
		t.Fatal(readErr)
	} else if !strings.Contains(string(payload), `"id":"resp_second"`) {
		t.Fatalf("second response = %s", payload)
	}

	first, second := <-observed, <-observed
	if first.Connection != 1 || len(first.Input) != 1 {
		t.Fatalf("first upstream request = %#v", first)
	}
	if second.Connection != 2 || second.PreviousResponseID != "" || len(second.Input) != 3 {
		t.Fatalf("reconnected request did not replay the transcript: %#v", second)
	}
}

func TestV1ResponsesWebSocketFallsBackToHTTPBeforeSubmission(t *testing.T) {
	var webSocketAttempts int
	var httpRequests int
	var requestMu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		defer requestMu.Unlock()
		if websocket.IsWebSocketUpgrade(r) {
			webSocketAttempts++
			http.Error(w, "websocket disabled", http.StatusUpgradeRequired)
			return
		}
		httpRequests++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode fallback body: %v", err)
		}
		if _, exists := body["type"]; exists {
			t.Errorf("HTTP fallback retained WebSocket type: %#v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_http\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
	}))
	defer upstream.Close()

	handler, database, apiKey, cleanup := testApplication(t, upstream.URL)
	defer cleanup()
	server := httptest.NewServer(handler)
	defer server.Close()
	connection, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer " + apiKey}})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatal(err)
	}
	defer connection.Close()
	if err = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-test","input":"fallback"}`)); err != nil {
		t.Fatal(err)
	}
	_, payload, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"id":"resp_http"`) {
		t.Fatalf("fallback response = %s", payload)
	}
	requestMu.Lock()
	gotWS, gotHTTP := webSocketAttempts, httpRequests
	requestMu.Unlock()
	if gotWS != 1 || gotHTTP != 1 {
		t.Fatalf("upstream attempts: websocket=%d http=%d", gotWS, gotHTTP)
	}
	deadline := time.Now().Add(time.Second)
	for {
		logs, logErr := database.ListRequestLogs(context.Background(), time.Now().Add(-time.Minute).UnixMilli(), time.Now().Add(time.Minute).UnixMilli(), 10)
		if logErr != nil {
			t.Fatal(logErr)
		}
		if len(logs) > 0 {
			if logs[0].Status != http.StatusOK || logs[0].InputTokens != 2 || logs[0].OutputTokens != 1 {
				t.Fatalf("fallback request log = %#v", logs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fallback turn was not logged")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const codexResponsesWebSocketBetaForTest = "responses_websockets=2026-02-06"

func TestNonStreamingEndpointsPreserveResponseFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"status\":400,\"error\":{\"type\":\"invalid_request_error\",\"code\":\"context_length_exceeded\",\"message\":\"context is too long\"}}}\n\n")
	}))
	defer upstream.Close()

	handler, _, apiKey, cleanup := testApplication(t, upstream.URL)
	defer cleanup()
	tests := []struct {
		path string
		body string
	}{
		{path: "/v1/responses", body: `{"model":"gpt-test","input":"hello","stream":false}`},
		{path: "/v1/chat/completions", body: `{"model":"gpt-test","messages":[{"role":"user","content":"hello"}],"stream":false}`},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer "+apiKey)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			var payload struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Error.Message != "context is too long" || payload.Error.Type != "invalid_request_error" || payload.Error.Code != "context_length_exceeded" {
				t.Fatalf("error payload = %#v", payload.Error)
			}
		})
	}
}

func TestAccountFailureDoesNotExposeCredentialDetails(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	handler, database, apiKey, cleanup := testApplication(t, upstream.URL)
	defer cleanup()
	if err := database.DeleteAccount(context.Background()); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hello"}`))
	request.Header.Set("Authorization", "Bearer "+apiKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "no Codex account") || !strings.Contains(response.Body.String(), "reconnect it in the admin dashboard") {
		t.Fatalf("unsafe account error = %s", response.Body.String())
	}
}

func TestDecodeJSONReportsOversizedBody(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/test", strings.NewReader(`{"value":"`+strings.Repeat("x", int(adminJSONBodyLimit))+`"}`))
	response := httptest.NewRecorder()
	var target map[string]any
	if decodeJSON(response, request, &target) {
		t.Fatal("decodeJSON() accepted an oversized body")
	}
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), `"code":"request_too_large"`) {
		t.Fatalf("oversized response = %d %s", response.Code, response.Body.String())
	}
}

func TestAdminLoginAndAPIKeyLifecycle(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	handler, _, _, cleanup := testApplication(t, upstream.URL)
	defer cleanup()

	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"correct horse battery"}`))
	handler.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookie || !cookies[0].HttpOnly {
		t.Fatalf("session cookies = %#v", cookies)
	}

	create := httptest.NewRecorder()
	createRequest := httptest.NewRequest(http.MethodPost, "/api/admin/keys", strings.NewReader(`{"name":"desktop"}`))
	createRequest.Header.Set("Origin", "http://codexone.test")
	createRequest.AddCookie(cookies[0])
	handler.ServeHTTP(create, createRequest)
	if create.Code != http.StatusCreated {
		t.Fatalf("create key status = %d, body = %s", create.Code, create.Body.String())
	}
	var created struct {
		Key    store.APIKey `json:"key"`
		Secret string       `json:"secret"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Secret, "sk-codexone-") || created.Key.Hash != "" {
		t.Fatalf("created key response = %#v", created)
	}

	revoke := httptest.NewRecorder()
	revokeRequest := httptest.NewRequest(http.MethodDelete, "/api/admin/keys/"+created.Key.ID, nil)
	revokeRequest.Header.Set("Origin", "http://codexone.test")
	revokeRequest.AddCookie(cookies[0])
	handler.ServeHTTP(revoke, revokeRequest)
	if revoke.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, body = %s", revoke.Code, revoke.Body.String())
	}

	useRevoked := httptest.NewRecorder()
	useRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	useRequest.Header.Set("Authorization", "Bearer "+created.Secret)
	handler.ServeHTTP(useRevoked, useRequest)
	if useRevoked.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key status = %d", useRevoked.Code)
	}
}

func TestPasswordChangeInvalidatesEveryExistingSession(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	handler, _, _, cleanup := testApplication(t, upstream.URL)
	defer cleanup()

	login := func(password string) (*http.Cookie, int) {
		t.Helper()
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(fmt.Sprintf(`{"password":%q}`, password)))
		handler.ServeHTTP(response, request)
		cookies := response.Result().Cookies()
		if response.Code != http.StatusOK {
			return nil, response.Code
		}
		if len(cookies) != 1 {
			t.Fatalf("login cookies = %#v", cookies)
		}
		return cookies[0], response.Code
	}

	first, status := login("correct horse battery")
	if status != http.StatusOK {
		t.Fatalf("first login status = %d", status)
	}
	second, status := login("correct horse battery")
	if status != http.StatusOK {
		t.Fatalf("second login status = %d", status)
	}

	change := httptest.NewRecorder()
	changeRequest := httptest.NewRequest(http.MethodPut, "/api/admin/password", strings.NewReader(`{"current_password":"correct horse battery","new_password":"new correct horse battery"}`))
	changeRequest.Header.Set("Origin", "http://codexone.test")
	changeRequest.AddCookie(first)
	handler.ServeHTTP(change, changeRequest)
	if change.Code != http.StatusOK {
		t.Fatalf("password change status = %d, body = %s", change.Code, change.Body.String())
	}

	for index, cookie := range []*http.Cookie{first, second} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/admin/account", nil)
		request.AddCookie(cookie)
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("old session %d status = %d, body = %s", index, response.Code, response.Body.String())
		}
	}

	if _, status = login("correct horse battery"); status != http.StatusUnauthorized {
		t.Fatalf("old password login status = %d", status)
	}
	if _, status = login("new correct horse battery"); status != http.StatusOK {
		t.Fatalf("new password login status = %d", status)
	}
}

func TestClientAddressOnlyTrustsConfiguredProxyChain(t *testing.T) {
	server := &Server{cfg: config.Config{TrustedProxyCIDRs: []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
	}}}

	tests := []struct {
		name       string
		remoteAddr string
		forwarded  string
		want       string
	}{
		{name: "untrusted peer ignores header", remoteAddr: "203.0.113.8:4444", forwarded: "198.51.100.9", want: "203.0.113.8"},
		{name: "trusted peer supplies client", remoteAddr: "10.0.0.2:4444", forwarded: "198.51.100.9", want: "198.51.100.9"},
		{name: "trusted chain walks from right", remoteAddr: "10.0.0.2:4444", forwarded: "192.0.2.7, 10.0.0.3", want: "192.0.2.7"},
		{name: "invalid chain falls back to peer", remoteAddr: "10.0.0.2:4444", forwarded: "not-an-ip", want: "10.0.0.2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
			request.RemoteAddr = test.remoteAddr
			request.Header.Set("X-Forwarded-For", test.forwarded)
			if got := server.clientAddress(request); got != test.want {
				t.Fatalf("clientAddress() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAPIKeyDatabaseFailureIsNotReportedAsInvalidKey(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	handler, database, apiKey, cleanup := testApplication(t, upstream.URL)
	defer cleanup()
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+apiKey)
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"database_unavailable"`) {
		t.Fatalf("database failure status = %d, body = %s", response.Code, response.Body.String())
	}

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusServiceUnavailable || !strings.Contains(health.Body.String(), `"code":"database_unavailable"`) {
		t.Fatalf("health status = %d, body = %s", health.Code, health.Body.String())
	}
}

func TestEnsureAdminPasswordGeneratesOnlyOnce(t *testing.T) {
	cfg := config.Config{StorageDriver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "codexone.db")}
	database, err := store.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	password, generated, err := EnsureAdminPassword(context.Background(), database, "")
	if err != nil {
		t.Fatal(err)
	}
	if !generated || len(password) < 12 {
		t.Fatalf("generated = %v, password length = %d", generated, len(password))
	}
	hash, err := database.GetSetting(context.Background(), adminPasswordSetting)
	if err != nil || !security.CheckPassword(hash, password) {
		t.Fatalf("generated password was not persisted correctly: %v", err)
	}
	secondPassword, generatedAgain, err := EnsureAdminPassword(context.Background(), database, "different-password")
	if err != nil {
		t.Fatal(err)
	}
	if generatedAgain || secondPassword != "" {
		t.Fatalf("password was initialized twice: generated = %v, password = %q", generatedAgain, secondPassword)
	}
}

func testApplication(t *testing.T, upstreamURL string) (http.Handler, *store.Store, string, func()) {
	t.Helper()
	temp := t.TempDir()
	cfg := config.Config{
		PublicURL:           "http://codexone.test",
		StorageDriver:       "sqlite",
		SQLitePath:          filepath.Join(temp, "codexone.db"),
		MasterKeyFile:       filepath.Join(temp, "master.key"),
		LogPath:             filepath.Join(temp, "codexone.log"),
		CodexClientVersion:  "0.146.0",
		CodexUserAgent:      "codex-tui/0.146.0 (integration-test)",
		CodexIdentityRemap:  true,
		CodexWSHTTPFallback: true,
		UpstreamBaseURL:     upstreamURL,
		MaxRequestBytes:     2 << 20,
		SessionTTL:          time.Hour,
		Location:            time.UTC,
	}
	ctx := context.Background()
	database, err := store.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.New(ctx, cfg)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	cipher, err := cryptox.New(cfg)
	if err != nil {
		sessions.Close()
		database.Close()
		t.Fatal(err)
	}
	logger, ring, logCloser, err := appLog.New(cfg.LogPath)
	if err != nil {
		sessions.Close()
		database.Close()
		t.Fatal(err)
	}
	if _, _, err = EnsureAdminPassword(ctx, database, "correct horse battery"); err != nil {
		logCloser.Close()
		sessions.Close()
		database.Close()
		t.Fatal(err)
	}
	manager := codex.NewManager(cfg, database, cipher, sessions, logger)
	authJSON := fmt.Sprintf(`{"access_token":"upstream-access","refresh_token":"upstream-refresh","account_id":"acct_single","email":"owner@example.com","expires_at":%d}`, time.Now().Add(time.Hour).Unix())
	if _, err = manager.ImportAuthJSON(ctx, []byte(authJSON)); err != nil {
		logCloser.Close()
		sessions.Close()
		database.Close()
		t.Fatal(err)
	}
	plain, prefix, hash, err := security.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	key := store.APIKey{ID: "key_fixture", Name: "fixture", Hash: hash, Prefix: prefix, CreatedAt: time.Now().UnixMilli()}
	if err = database.CreateAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	proxyService := proxy.New(cfg, manager, database, logger)
	handler := New(cfg, database, sessions, manager, proxyService, logger, ring)
	cleanup := func() {
		_ = logCloser.Close()
		_ = sessions.Close()
		_ = database.Close()
	}
	return handler, database, plain, cleanup
}
