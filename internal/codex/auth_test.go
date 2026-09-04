package codex

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ankairis/CodexOne/internal/config"
	"github.com/Ankairis/CodexOne/internal/cryptox"
	"github.com/Ankairis/CodexOne/internal/session"
	"github.com/Ankairis/CodexOne/internal/store"
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

func TestBrowserOAuthFlowUsesPKCEAndPastedCallback(t *testing.T) {
	ctx := context.Background()
	temp := t.TempDir()
	cfg := config.Config{
		StorageDriver: "sqlite",
		SQLitePath:    filepath.Join(temp, "codexone.db"),
		MasterKeyFile: filepath.Join(temp, "master.key"),
	}
	database, err := store.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sessions, err := session.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Close()
	cipher, err := cryptox.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(cfg, database, cipher, sessions, slog.New(slog.NewTextHandler(io.Discard, nil)))
	start, err := manager.StartBrowserFlow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	authorizationURL, err := url.Parse(start.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	query := authorizationURL.Query()
	if query.Get("redirect_uri") != browserRedirectURI || query.Get("code_challenge_method") != "S256" || query.Get("state") == "" || query.Get("code_challenge") == "" {
		t.Fatalf("authorization URL query = %#v", query)
	}

	tokenRequests := 0
	challenge := query.Get("code_challenge")
	idToken := testJWT(t, map[string]any{
		"email": "browser@example.com",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_browser",
			"chatgpt_plan_type":  "plus",
		},
	})
	tokenBody, _ := json.Marshal(map[string]any{"access_token": "access", "refresh_token": "refresh", "id_token": idToken, "expires_in": 3600})
	manager.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		tokenRequests++
		if request.URL.String() != authTokenURL {
			t.Errorf("token URL = %q", request.URL.String())
		}
		if err := request.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		if request.Form.Get("code") != "code_123" || request.Form.Get("redirect_uri") != browserRedirectURI {
			t.Errorf("token form = %#v", request.Form)
		}
		digest := sha256.Sum256([]byte(request.Form.Get("code_verifier")))
		if encoded := base64.RawURLEncoding.EncodeToString(digest[:]); encoded != challenge {
			t.Errorf("PKCE challenge = %q, want %q", encoded, challenge)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(tokenBody))), Request: request}, nil
	})}

	wrongCallback := start.RedirectURI + "?code=code_123&state=wrong"
	if _, err = manager.CompleteBrowserFlow(ctx, start.FlowID, wrongCallback); !errors.Is(err, ErrBrowserOAuthInput) {
		t.Fatalf("state mismatch error = %v", err)
	}
	if tokenRequests != 0 {
		t.Fatalf("token endpoint called for mismatched state")
	}
	callback := start.RedirectURI + "?code=code_123&state=" + url.QueryEscape(query.Get("state"))
	view, err := manager.CompleteBrowserFlow(ctx, start.FlowID, callback)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Connected || view.Email != "browser@example.com" || view.AccountID != "acct_browser" || tokenRequests != 1 {
		t.Fatalf("account view = %#v, token requests = %d", view, tokenRequests)
	}
	if _, err = manager.CompleteBrowserFlow(ctx, start.FlowID, callback); !errors.Is(err, ErrBrowserOAuthInput) {
		t.Fatalf("reused flow error = %v", err)
	}
}

func TestParseBrowserCallbackRejectsNonLocalURL(t *testing.T) {
	if _, err := parseBrowserCallback("https://example.com/auth/callback?code=x&state=y"); !errors.Is(err, ErrBrowserOAuthInput) {
		t.Fatalf("parseBrowserCallback() error = %v", err)
	}
	callback, err := parseBrowserCallback("code=x&state=y")
	if err != nil || callback.Code != "x" || callback.State != "y" {
		t.Fatalf("raw callback = %#v, error = %v", callback, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
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
