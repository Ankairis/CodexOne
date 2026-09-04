package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ankairis/CodexOne/internal/config"
)

func TestOpenMigratesLegacyRequestTelemetryColumns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE request_logs (
		id TEXT PRIMARY KEY, request_id TEXT NOT NULL, api_key_id TEXT, method TEXT NOT NULL, path TEXT NOT NULL,
		model TEXT NOT NULL DEFAULT '', status INTEGER NOT NULL, duration_ms BIGINT NOT NULL,
		input_tokens BIGINT NOT NULL DEFAULT 0, output_tokens BIGINT NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT '', created_at BIGINT NOT NULL
	)`)
	if closeErr := legacy.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	database, err := Open(ctx, config.Config{StorageDriver: "sqlite", SQLitePath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rows, err := database.db.QueryContext(ctx, `PRAGMA table_info(request_logs)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := map[string]bool{
		"reasoning_tokens": false, "reasoning_effort": false, "upstream_reasoning_effort": false,
		"first_reasoning_ms": false, "first_output_ms": false,
	}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue sql.NullString
		if err = rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if _, exists := want[name]; exists {
			want[name] = true
		}
	}
	for column, found := range want {
		if !found {
			t.Errorf("legacy migration did not add %s", column)
		}
	}
}

func TestSaveAccountReplacesTheOnlyRow(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, config.Config{StorageDriver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	first := Account{Email: "one@example.com", ChatGPTAccountID: "acct_one", AccessTokenEncrypted: "a", RefreshTokenEncrypted: "r", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	if err = database.SaveAccount(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err = database.SaveQuota(ctx, QuotaSnapshot{Payload: `{}`, FetchedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	second := Account{Email: "two@example.com", ChatGPTAccountID: "acct_two", AccessTokenEncrypted: "b", RefreshTokenEncrypted: "s", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	if err = database.SaveAccount(ctx, second); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ChatGPTAccountID != "acct_two" || stored.Email != "two@example.com" {
		t.Fatalf("stored account = %#v", stored)
	}
	var count int
	if err = database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM codex_account`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("account count = %d, want 1", count)
	}
	if _, err = database.GetQuota(ctx); !IsNotFound(err) {
		t.Fatalf("old account quota should be removed, error = %v", err)
	}
}

func TestAPIKeyLifecycleAndStats(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, config.Config{StorageDriver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UnixMilli()
	key := APIKey{ID: "key_one", Name: "test", Hash: "hash", Prefix: "sk-codexone-test", CreatedAt: now}
	if err = database.CreateAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err = database.FindActiveAPIKeyByHash(ctx, "hash"); err != nil {
		t.Fatal(err)
	}
	if err = database.InsertRequestLog(ctx, RequestLog{ID: "log_one", RequestID: "req_one", APIKeyID: key.ID, Method: "POST", Path: "/v1/responses", Model: "gpt-test", Status: 200, DurationMS: 120, InputTokens: 10, OutputTokens: 5, ReasoningTokens: 3, ReasoningEffort: "max", UpstreamReasoningEffort: "max", FirstReasoningMS: 50, FirstOutputMS: 100, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	stats, err := database.TodayStats(ctx, now-1000, now+1000)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Requests != 1 || stats.SuccessRate != 100 || stats.InputTokens != 10 || stats.OutputTokens != 5 || stats.ReasoningTokens != 3 {
		t.Fatalf("stats = %#v", stats)
	}
	logs, err := database.ListRequestLogs(ctx, now-1000, now+1000, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].ReasoningEffort != "max" || logs[0].UpstreamReasoningEffort != "max" ||
		logs[0].ReasoningTokens != 3 || logs[0].FirstReasoningMS != 50 || logs[0].FirstOutputMS != 100 {
		t.Fatalf("request telemetry = %#v", logs)
	}
	if revoked, err := database.RevokeAPIKey(ctx, key.ID); err != nil || !revoked {
		t.Fatalf("revoke = %v, %v", revoked, err)
	}
	if _, err = database.FindActiveAPIKeyByHash(ctx, "hash"); !IsNotFound(err) {
		t.Fatalf("revoked key lookup error = %v", err)
	}
}
