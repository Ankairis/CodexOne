package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Ankairis/CodexOne/internal/store"
	"github.com/google/uuid"
)

func TestPrepareRequestIdentityUsesStableAPIKeyFallback(t *testing.T) {
	ctx := WithAPIKey(context.Background(), store.APIKey{ID: "key_stable"})
	first, firstIdentity, err := prepareRequestIdentity(ctx, nil, []byte(`{"model":"gpt-test","input":"hello"}`), true)
	if err != nil {
		t.Fatal(err)
	}
	second, secondIdentity, err := prepareRequestIdentity(ctx, nil, []byte(`{"model":"gpt-test","input":"again"}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if firstIdentity.sessionID() == "" || firstIdentity.sessionID() != secondIdentity.sessionID() {
		t.Fatalf("generated sessions differ: %q and %q", firstIdentity.sessionID(), secondIdentity.sessionID())
	}
	if _, err = uuid.Parse(firstIdentity.sessionID()); err != nil {
		t.Fatalf("generated session is not a UUID: %q", firstIdentity.sessionID())
	}
	for _, body := range [][]byte{first, second} {
		var payload map[string]any
		if err = json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["prompt_cache_key"] != firstIdentity.sessionID() {
			t.Fatalf("prompt_cache_key = %#v, want %q", payload["prompt_cache_key"], firstIdentity.sessionID())
		}
	}
}

func TestPrepareRequestIdentityBodyWinsWithoutRemap(t *testing.T) {
	headers := http.Header{"Session-Id": {"header-session"}}
	body, identity, err := prepareRequestIdentity(
		WithAPIKey(context.Background(), store.APIKey{ID: "key_one"}),
		headers,
		[]byte(`{"model":"gpt-test","input":"hello","prompt_cache_key":"body-session"}`),
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if identity.sessionID() != "body-session" {
		t.Fatalf("session = %q", identity.sessionID())
	}
	var payload map[string]any
	if err = json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["prompt_cache_key"] != "body-session" {
		t.Fatalf("prompt_cache_key = %#v", payload["prompt_cache_key"])
	}
}

func TestIdentityRemapIsStableSeparatedAndReversible(t *testing.T) {
	ctx := WithAPIKey(context.Background(), store.APIKey{ID: "key_private"})
	headers := http.Header{
		"Session-Id":            {"client-session"},
		"Thread-Id":             {"client-thread"},
		"X-Client-Request-Id":   {"client-request"},
		"X-Codex-Window-Id":     {"client-session:0"},
		"X-Codex-Turn-Metadata": {`{"prompt_cache_key":"client-session","turn_id":"client-turn","window_id":"client-session:0"}`},
	}
	body := []byte(`{"model":"gpt-test","input":"hello","client_metadata":{"x-codex-installation-id":"client-installation","x-client-request-id":"body-request","x-codex-window-id":"client-session:0","x-codex-turn-metadata":"{\"turn_id\":\"body-turn\"}"}}`)
	prepared, identity, err := prepareRequestIdentity(ctx, headers, body, true)
	if err != nil {
		t.Fatal(err)
	}
	if identity.sessionID() == "client-session" {
		t.Fatal("session identity was not remapped")
	}
	if _, err = uuid.Parse(identity.sessionID()); err != nil {
		t.Fatalf("mapped session is not a UUID: %q", identity.sessionID())
	}

	upstreamHeaders := headers.Clone()
	identity.applyHeaders(upstreamHeaders, headers, true)
	mapped := []string{
		identity.sessionID(),
		upstreamHeaders.Get("Thread-Id"),
		upstreamHeaders.Get("X-Client-Request-Id"),
		upstreamHeaders.Get("X-Codex-Window-Id"),
	}
	seen := make(map[string]bool)
	for _, value := range mapped {
		if value == "" {
			t.Fatalf("missing mapped identity: %#v", upstreamHeaders)
		}
		if seen[value] {
			t.Fatalf("distinct identity dimensions collapsed onto %q", value)
		}
		seen[value] = true
	}
	if got := headerValueFold(upstreamHeaders, "session_id"); got != identity.sessionID() {
		t.Fatalf("session_id = %q, want %q", got, identity.sessionID())
	}

	var object map[string]any
	if err = json.Unmarshal(prepared, &object); err != nil {
		t.Fatal(err)
	}
	metadata := object["client_metadata"].(map[string]any)
	if metadata["x-codex-installation-id"] == "client-installation" || metadata["x-client-request-id"] == "body-request" {
		t.Fatalf("client metadata was not remapped: %#v", metadata)
	}

	exposed := identity.exposePayload([]byte(`{"session":"` + identity.sessionID() + `","thread":"` + upstreamHeaders.Get("Thread-Id") + `"}`))
	if string(exposed) != `{"session":"client-session","thread":"client-thread"}` {
		t.Fatalf("exposed payload = %s", exposed)
	}

	_, repeated, err := prepareRequestIdentity(ctx, headers, body, true)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.sessionID() != identity.sessionID() {
		t.Fatalf("mapping is not stable: %q != %q", repeated.sessionID(), identity.sessionID())
	}
	otherHeaders := headers.Clone()
	otherHeaders.Set("Session-Id", "other-session")
	_, other, err := prepareRequestIdentity(ctx, otherHeaders, body, true)
	if err != nil {
		t.Fatal(err)
	}
	if other.sessionID() == identity.sessionID() {
		t.Fatal("different sessions collapsed onto the same upstream identity")
	}
}

func TestWebSocketIdentityRejectsSessionChange(t *testing.T) {
	ctx := WithAPIKey(context.Background(), store.APIKey{ID: "key_ws"})
	_, identity, err := prepareRequestIdentity(ctx, nil, []byte(`{"model":"gpt-test","input":"hello","prompt_cache_key":"first"}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = identity.prepareBody([]byte(`{"model":"gpt-test","input":"again","prompt_cache_key":"second"}`), true); err == nil {
		t.Fatal("active WebSocket accepted a prompt_cache_key change")
	}
}

func TestExposePayloadEscapesRestoredIdentityForJSON(t *testing.T) {
	upstreamID := "3b5b6900-5e7a-5eb8-a62a-96e79b4f9e31"
	clientID := "client-\"quote\\slash\ttab"
	identity := &codexIdentity{
		remap:   true,
		reverse: map[string]string{upstreamID: clientID},
	}
	exposed := identity.exposePayload([]byte(`{"id":"prefix-` + upstreamID + `-suffix"}`))
	if !json.Valid(exposed) {
		t.Fatalf("restored payload is invalid JSON: %s", exposed)
	}
	var decoded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(exposed, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ID != "prefix-"+clientID+"-suffix" {
		t.Fatalf("restored identity = %q", decoded.ID)
	}
}
