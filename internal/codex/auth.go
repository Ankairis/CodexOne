package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Ankairis/CodexOne/internal/config"
	"github.com/Ankairis/CodexOne/internal/cryptox"
	"github.com/Ankairis/CodexOne/internal/security"
	"github.com/Ankairis/CodexOne/internal/session"
	"github.com/Ankairis/CodexOne/internal/store"
)

const (
	clientID                    = "app_EMoamEEZ73f0CkXaXp7hrann"
	authTokenURL                = "https://auth.openai.com/oauth/token"
	deviceUserCodeURL           = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	deviceTokenURL              = "https://auth.openai.com/api/accounts/deviceauth/token"
	deviceVerificationURL       = "https://auth.openai.com/codex/device"
	deviceTokenExchangeRedirect = "https://auth.openai.com/deviceauth/callback"
	deviceFlowTTL               = 15 * time.Minute
)

type Credential struct {
	Email        string
	AccountID    string
	PlanType     string
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    time.Time
	UpdatedAt    time.Time
}

type AccountView struct {
	Connected  bool            `json:"connected"`
	Email      string          `json:"email,omitempty"`
	AccountID  string          `json:"account_id,omitempty"`
	PlanType   string          `json:"plan_type,omitempty"`
	ExpiresAt  int64           `json:"expires_at,omitempty"`
	UpdatedAt  int64           `json:"updated_at,omitempty"`
	Quota      json.RawMessage `json:"quota,omitempty"`
	QuotaAt    int64           `json:"quota_fetched_at,omitempty"`
	ClientName string          `json:"client_name"`
}

type DeviceStart struct {
	FlowID          string `json:"flow_id"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	ExpiresAt       int64  `json:"expires_at"`
	PollInterval    int    `json:"poll_interval"`
}

type DeviceStatus struct {
	Status  string      `json:"status"`
	Account AccountView `json:"account,omitempty"`
}

type Manager struct {
	cfg      config.Config
	store    *store.Store
	cipher   *cryptox.Cipher
	sessions session.Store
	client   *http.Client
	logger   *slog.Logger
	refresh  sync.Mutex
}

func NewManager(cfg config.Config, database *store.Store, cipher *cryptox.Cipher, sessions session.Store, logger *slog.Logger) *Manager {
	return &Manager{
		cfg:      cfg,
		store:    database,
		cipher:   cipher,
		sessions: sessions,
		client:   &http.Client{Timeout: 30 * time.Second},
		logger:   logger,
	}
}

func (m *Manager) StartDeviceFlow(ctx context.Context) (DeviceStart, error) {
	body, err := json.Marshal(map[string]string{"client_id": clientID})
	if err != nil {
		return DeviceStart{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceUserCodeURL, bytes.NewReader(body))
	if err != nil {
		return DeviceStart{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return DeviceStart{}, fmt.Errorf("request device code: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return DeviceStart{}, fmt.Errorf("read device code response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return DeviceStart{}, upstreamError("device code request", resp.StatusCode, raw)
	}
	var payload struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		UserCodeAlt  string          `json:"usercode"`
		Interval     json.RawMessage `json:"interval"`
	}
	if err = json.Unmarshal(raw, &payload); err != nil {
		return DeviceStart{}, fmt.Errorf("decode device code response: %w", err)
	}
	if payload.UserCode == "" {
		payload.UserCode = payload.UserCodeAlt
	}
	if strings.TrimSpace(payload.DeviceAuthID) == "" || strings.TrimSpace(payload.UserCode) == "" {
		return DeviceStart{}, fmt.Errorf("device flow response is missing required fields")
	}
	interval := parsePollInterval(payload.Interval)
	flowID, err := security.RandomToken(24)
	if err != nil {
		return DeviceStart{}, fmt.Errorf("generate device flow ID: %w", err)
	}
	flow := deviceFlow{
		DeviceAuthID: payload.DeviceAuthID,
		UserCode:     payload.UserCode,
		ExpiresAt:    time.Now().Add(deviceFlowTTL).UnixMilli(),
		Interval:     interval,
	}
	encoded, _ := json.Marshal(flow)
	if err = m.sessions.Put(ctx, "oauth:"+flowID, string(encoded), deviceFlowTTL); err != nil {
		return DeviceStart{}, fmt.Errorf("store device flow: %w", err)
	}
	m.logger.Info("codex device login started", "flow_id", flowID[:8])
	return DeviceStart{
		FlowID:          flowID,
		UserCode:        payload.UserCode,
		VerificationURL: deviceVerificationURL,
		ExpiresAt:       flow.ExpiresAt,
		PollInterval:    interval,
	}, nil
}

func (m *Manager) PollDeviceFlow(ctx context.Context, flowID string) (DeviceStatus, error) {
	rawFlow, err := m.sessions.Get(ctx, "oauth:"+flowID)
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			return DeviceStatus{}, fmt.Errorf("device login expired or was not found")
		}
		return DeviceStatus{}, err
	}
	var flow deviceFlow
	if err = json.Unmarshal([]byte(rawFlow), &flow); err != nil {
		return DeviceStatus{}, fmt.Errorf("decode device flow: %w", err)
	}
	if time.Now().UnixMilli() >= flow.ExpiresAt {
		_ = m.sessions.Delete(ctx, "oauth:"+flowID)
		return DeviceStatus{}, fmt.Errorf("device login expired")
	}

	body, _ := json.Marshal(map[string]string{"device_auth_id": flow.DeviceAuthID, "user_code": flow.UserCode})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceTokenURL, bytes.NewReader(body))
	if err != nil {
		return DeviceStatus{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return DeviceStatus{}, fmt.Errorf("poll device login: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return DeviceStatus{}, fmt.Errorf("read device login response: %w", err)
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return DeviceStatus{Status: "pending"}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return DeviceStatus{}, upstreamError("device login polling", resp.StatusCode, raw)
	}
	var tokenCode struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
		CodeChallenge     string `json:"code_challenge"`
	}
	if err = json.Unmarshal(raw, &tokenCode); err != nil {
		return DeviceStatus{}, fmt.Errorf("decode device login response: %w", err)
	}
	if tokenCode.AuthorizationCode == "" || tokenCode.CodeVerifier == "" {
		return DeviceStatus{}, fmt.Errorf("device login response is incomplete")
	}
	tokens, err := m.exchangeCode(ctx, tokenCode.AuthorizationCode, tokenCode.CodeVerifier)
	if err != nil {
		return DeviceStatus{}, err
	}
	credential, err := credentialFromTokens(tokens)
	if err != nil {
		return DeviceStatus{}, err
	}
	if err = m.save(ctx, credential); err != nil {
		return DeviceStatus{}, err
	}
	_ = m.sessions.Delete(ctx, "oauth:"+flowID)
	m.logger.Info("codex account connected", "email", credential.Email, "plan", credential.PlanType)
	view, err := m.Account(ctx)
	return DeviceStatus{Status: "complete", Account: view}, err
}

func (m *Manager) ImportAuthJSON(ctx context.Context, raw []byte) (AccountView, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return AccountView{}, fmt.Errorf("auth.json is not valid JSON")
	}
	tokens, err := parseImportedTokens(raw)
	if err != nil {
		return AccountView{}, err
	}
	credential, err := credentialFromTokens(tokens)
	if err != nil {
		return AccountView{}, err
	}
	if err = m.save(ctx, credential); err != nil {
		return AccountView{}, err
	}
	m.logger.Info("codex account imported", "email", credential.Email, "plan", credential.PlanType)
	return m.Account(ctx)
}

func (m *Manager) Account(ctx context.Context) (AccountView, error) {
	account, err := m.store.GetAccount(ctx)
	if err != nil {
		if store.IsNotFound(err) {
			return AccountView{Connected: false, ClientName: m.clientName()}, nil
		}
		return AccountView{}, err
	}
	view := AccountView{
		Connected:  true,
		Email:      account.Email,
		AccountID:  account.ChatGPTAccountID,
		PlanType:   account.PlanType,
		ExpiresAt:  account.ExpiresAt,
		UpdatedAt:  account.UpdatedAt,
		ClientName: m.clientName(),
	}
	if quota, quotaErr := m.store.GetQuota(ctx); quotaErr == nil && json.Valid([]byte(quota.Payload)) {
		view.Quota = json.RawMessage(quota.Payload)
		view.QuotaAt = quota.FetchedAt
	}
	return view, nil
}

func (m *Manager) DeleteAccount(ctx context.Context) error {
	if err := m.store.DeleteAccount(ctx); err != nil {
		return err
	}
	m.logger.Info("codex account disconnected")
	return nil
}

func (m *Manager) Credential(ctx context.Context) (Credential, error) {
	account, err := m.store.GetAccount(ctx)
	if err != nil {
		if store.IsNotFound(err) {
			return Credential{}, fmt.Errorf("no Codex account is connected")
		}
		return Credential{}, err
	}
	accessToken, err := m.cipher.Decrypt(account.AccessTokenEncrypted)
	if err != nil {
		return Credential{}, err
	}
	refreshToken, err := m.cipher.Decrypt(account.RefreshTokenEncrypted)
	if err != nil {
		return Credential{}, err
	}
	idToken, err := m.cipher.Decrypt(account.IDTokenEncrypted)
	if err != nil {
		return Credential{}, err
	}
	return Credential{
		Email:        account.Email,
		AccountID:    account.ChatGPTAccountID,
		PlanType:     account.PlanType,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		IDToken:      idToken,
		ExpiresAt:    time.UnixMilli(account.ExpiresAt),
		UpdatedAt:    time.UnixMilli(account.UpdatedAt),
	}, nil
}

func (m *Manager) FreshCredential(ctx context.Context) (Credential, error) {
	credential, err := m.Credential(ctx)
	if err != nil {
		return Credential{}, err
	}
	if credential.ExpiresAt.After(time.Now().Add(2 * time.Minute)) {
		return credential, nil
	}

	m.refresh.Lock()
	defer m.refresh.Unlock()
	credential, err = m.Credential(ctx)
	if err != nil {
		return Credential{}, err
	}
	if credential.ExpiresAt.After(time.Now().Add(2 * time.Minute)) {
		return credential, nil
	}
	if credential.RefreshToken == "" {
		return Credential{}, fmt.Errorf("Codex refresh token is missing; sign in again")
	}
	tokens, err := m.refreshTokens(ctx, credential.RefreshToken)
	if err != nil {
		return Credential{}, err
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = credential.RefreshToken
	}
	if tokens.IDToken == "" {
		tokens.IDToken = credential.IDToken
	}
	refreshed, err := credentialFromTokens(tokens)
	if err != nil {
		return Credential{}, err
	}
	if refreshed.AccountID == "" {
		refreshed.AccountID = credential.AccountID
	}
	if refreshed.Email == "" {
		refreshed.Email = credential.Email
	}
	if refreshed.PlanType == "" {
		refreshed.PlanType = credential.PlanType
	}
	if err = m.save(ctx, refreshed); err != nil {
		return Credential{}, err
	}
	m.logger.Info("codex access token refreshed", "email", refreshed.Email)
	return refreshed, nil
}

func (m *Manager) FetchQuota(ctx context.Context) (json.RawMessage, int64, error) {
	credential, err := m.FreshCredential(ctx)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cfg.UpstreamBaseURL+"/backend-api/wham/usage", nil)
	if err != nil {
		return nil, 0, err
	}
	m.ApplyCodexHeaders(req, credential, "application/json")
	req.Header.Set("OpenAI-Beta", "codex-1")
	req.Header.Set("OAI-Language", "zh-CN")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-Mode", "no-cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Priority", "u=4, i")
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request Codex quota: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("read Codex quota: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, 0, upstreamError("Codex quota request", resp.StatusCode, raw)
	}
	if !json.Valid(raw) {
		return nil, 0, fmt.Errorf("Codex quota response was not valid JSON")
	}
	fetchedAt := time.Now().UnixMilli()
	if err = m.store.SaveQuota(ctx, store.QuotaSnapshot{Payload: string(raw), FetchedAt: fetchedAt}); err != nil {
		return nil, 0, fmt.Errorf("save quota snapshot: %w", err)
	}
	m.logger.Info("codex quota refreshed")
	return json.RawMessage(raw), fetchedAt, nil
}

func (m *Manager) ApplyCodexHeaders(req *http.Request, credential Credential, accept string) {
	req.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	req.Header.Set("Chatgpt-Account-Id", credential.AccountID)
	req.Header.Set("User-Agent", m.cfg.CodexUserAgent)
	req.Header.Set("Originator", "codex-tui")
	req.Header.Set("Version", m.cfg.CodexClientVersion)
	req.Header.Set("Accept", accept)
	req.Header.Set("Connection", "Keep-Alive")
}

func (m *Manager) clientName() string {
	return "codex-tui/" + m.cfg.CodexClientVersion
}

func (m *Manager) save(ctx context.Context, credential Credential) error {
	if credential.AccountID == "" || credential.AccessToken == "" {
		return fmt.Errorf("Codex credential is missing account_id or access_token")
	}
	access, err := m.cipher.Encrypt(credential.AccessToken)
	if err != nil {
		return err
	}
	refresh, err := m.cipher.Encrypt(credential.RefreshToken)
	if err != nil {
		return err
	}
	idToken, err := m.cipher.Encrypt(credential.IDToken)
	if err != nil {
		return err
	}
	return m.store.SaveAccount(ctx, store.Account{
		Email:                 credential.Email,
		ChatGPTAccountID:      credential.AccountID,
		PlanType:              credential.PlanType,
		AccessTokenEncrypted:  access,
		RefreshTokenEncrypted: refresh,
		IDTokenEncrypted:      idToken,
		ExpiresAt:             credential.ExpiresAt.UnixMilli(),
	})
}

func (m *Manager) exchangeCode(ctx context.Context, code, verifier string) (tokenSet, error) {
	values := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"redirect_uri":  {deviceTokenExchangeRedirect},
		"code_verifier": {verifier},
	}
	return m.requestTokens(ctx, values, "exchange Codex authorization code")
}

func (m *Manager) refreshTokens(ctx context.Context, refreshToken string) (tokenSet, error) {
	values := url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {"openid profile email"},
	}
	return m.requestTokens(ctx, values, "refresh Codex token")
}

func (m *Manager) requestTokens(ctx context.Context, values url.Values, action string) (tokenSet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authTokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return tokenSet{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return tokenSet{}, fmt.Errorf("%s: %w", action, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenSet{}, fmt.Errorf("%s: read response: %w", action, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tokenSet{}, upstreamError(action, resp.StatusCode, raw)
	}
	var tokens tokenSet
	if err = json.Unmarshal(raw, &tokens); err != nil {
		return tokenSet{}, fmt.Errorf("%s: decode response: %w", action, err)
	}
	return tokens, nil
}

type deviceFlow struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
	ExpiresAt    int64  `json:"expires_at"`
	Interval     int    `json:"interval"`
}

type tokenSet struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
	Email        string `json:"email"`
	PlanType     string `json:"plan_type"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiresAt    int64  `json:"expires_at"`
	Expired      string `json:"expired"`
}

func parseImportedTokens(raw []byte) (tokenSet, error) {
	var topLevel tokenSet
	if err := json.Unmarshal(raw, &topLevel); err != nil {
		return tokenSet{}, err
	}
	var nested struct {
		Tokens tokenSet `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &nested); err != nil {
		return tokenSet{}, err
	}
	tokens := nested.Tokens
	if tokens.AccessToken == "" {
		tokens = topLevel
	}
	if tokens.AccessToken == "" {
		return tokenSet{}, fmt.Errorf("auth.json does not contain an access_token")
	}
	if tokens.RefreshToken == "" {
		return tokenSet{}, fmt.Errorf("auth.json does not contain a refresh_token")
	}
	return tokens, nil
}

func credentialFromTokens(tokens tokenSet) (Credential, error) {
	claims := parseJWTClaims(tokens.IDToken)
	if len(claims) == 0 {
		claims = parseJWTClaims(tokens.AccessToken)
	}
	authClaims := nestedMap(claims, "https://api.openai.com/auth")
	profileClaims := nestedMap(claims, "https://api.openai.com/profile")
	accountID := firstNonEmpty(tokens.AccountID, stringValue(authClaims, "chatgpt_account_id"), stringValue(claims, "chatgpt_account_id"), stringValue(claims, "account_id"))
	email := firstNonEmpty(tokens.Email, stringValue(claims, "email"), stringValue(profileClaims, "email"))
	plan := firstNonEmpty(tokens.PlanType, stringValue(authClaims, "chatgpt_plan_type"), stringValue(claims, "chatgpt_plan_type"))
	if tokens.AccessToken == "" || accountID == "" {
		return Credential{}, fmt.Errorf("Codex token is missing access_token or ChatGPT account ID")
	}
	expiresAt := tokenExpiry(tokens, claims)
	return Credential{
		Email:        email,
		AccountID:    accountID,
		PlanType:     plan,
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		IDToken:      tokens.IDToken,
		ExpiresAt:    expiresAt,
		UpdatedAt:    time.Now(),
	}, nil
}

func tokenExpiry(tokens tokenSet, claims map[string]any) time.Time {
	if tokens.ExpiresAt > 0 {
		if tokens.ExpiresAt > 10_000_000_000 {
			return time.UnixMilli(tokens.ExpiresAt)
		}
		return time.Unix(tokens.ExpiresAt, 0)
	}
	if tokens.Expired != "" {
		if parsed, err := time.Parse(time.RFC3339, tokens.Expired); err == nil {
			return parsed
		}
	}
	if tokens.ExpiresIn > 0 {
		return time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	}
	if exp, ok := numberValue(claims, "exp"); ok {
		return time.Unix(exp, 0)
	}
	return time.Now().Add(time.Hour)
}

func parseJWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return claims
}

func nestedMap(values map[string]any, key string) map[string]any {
	if nested, ok := values[key].(map[string]any); ok {
		return nested
	}
	return nil
}

func stringValue(values map[string]any, key string) string {
	if value, ok := values[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func numberValue(values map[string]any, key string) (int64, bool) {
	switch value := values[key].(type) {
	case float64:
		return int64(value), true
	case json.Number:
		number, err := value.Int64()
		return number, err == nil
	}
	return 0, false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func parsePollInterval(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 5
	}
	var number int
	if json.Unmarshal(raw, &number) == nil && number > 0 {
		return number
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if parsed, err := strconv.Atoi(strings.TrimSpace(text)); err == nil && parsed > 0 {
			return parsed
		}
	}
	return 5
}

func upstreamError(action string, status int, body []byte) error {
	text := strings.TrimSpace(string(body))
	if len(text) > 500 {
		text = text[:500]
	}
	if text == "" {
		text = http.StatusText(status)
	}
	return fmt.Errorf("%s failed (%d): %s", action, status, text)
}
