package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ankairis/CodexOne/internal/config"
)

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
	if err = database.InsertRequestLog(ctx, RequestLog{ID: "log_one", RequestID: "req_one", APIKeyID: key.ID, Method: "POST", Path: "/v1/responses", Model: "gpt-test", Status: 200, DurationMS: 120, InputTokens: 10, OutputTokens: 5, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	stats, err := database.TodayStats(ctx, now-1000, now+1000)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Requests != 1 || stats.SuccessRate != 100 || stats.InputTokens != 10 || stats.OutputTokens != 5 {
		t.Fatalf("stats = %#v", stats)
	}
	if revoked, err := database.RevokeAPIKey(ctx, key.ID); err != nil || !revoked {
		t.Fatalf("revoke = %v, %v", revoked, err)
	}
	if _, err = database.FindActiveAPIKeyByHash(ctx, "hash"); !IsNotFound(err) {
		t.Fatalf("revoked key lookup error = %v", err)
	}
}
