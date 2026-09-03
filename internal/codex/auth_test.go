package codex

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Ankairis/CodexOne/internal/config"
)

func TestParseImportedTokensNestedAuthJSON(t *testing.T) {
	idToken := testJWT(t, map[string]any{
		"email": "person@example.com",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_123",
			"chatgpt_plan_type":  "plus",
		},
	})
	raw, _ := json.Marshal(map[string]any{"tokens": map[string]any{
		"access_token": "access", "refresh_token": "refresh", "id_token": idToken, "account_id": "acct_123",
	}})
	tokens, err := parseImportedTokens(raw)
	if err != nil {
		t.Fatalf("parseImportedTokens() error = %v", err)
	}
	credential, err := credentialFromTokens(tokens)
	if err != nil {
		t.Fatalf("credentialFromTokens() error = %v", err)
	}
	if credential.Email != "person@example.com" || credential.AccountID != "acct_123" || credential.PlanType != "plus" {
		t.Fatalf("credential = %#v", credential)
	}
}

func TestParseImportedTokensFlatCredential(t *testing.T) {
	raw := []byte(`{"access_token":"access","refresh_token":"refresh","account_id":"acct_flat","email":"flat@example.com","expires_at":2000000000}`)
	tokens, err := parseImportedTokens(raw)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := credentialFromTokens(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccountID != "acct_flat" || credential.Email != "flat@example.com" {
		t.Fatalf("credential = %#v", credential)
	}
}

func TestApplyCodexHeadersAlwaysCloaksIdentity(t *testing.T) {
	manager := &Manager{cfg: config.Config{CodexClientVersion: "0.146.0", CodexUserAgent: "codex-tui/0.146.0 (test)"}}
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	req.Header.Set("User-Agent", "downstream/1.0")
	req.Header.Set("Originator", "other")
	manager.ApplyCodexHeaders(req, Credential{AccessToken: "token", AccountID: "acct"}, "text/event-stream")
	if got := req.Header.Get("User-Agent"); got != "codex-tui/0.146.0 (test)" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := req.Header.Get("Originator"); got != "codex-tui" {
		t.Fatalf("Originator = %q", got)
	}
	if got := req.Header.Get("Chatgpt-Account-Id"); got != "acct" {
		t.Fatalf("Chatgpt-Account-Id = %q", got)
	}
}

func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(raw) + ".signature"
}
