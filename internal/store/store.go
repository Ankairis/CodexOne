package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ankairis/CodexOne/internal/config"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

var ErrNotFound = sql.ErrNoRows

type Store struct {
	db       *sql.DB
	postgres bool
}

func Open(ctx context.Context, cfg config.Config) (*Store, error) {
	var driver, dsn string
	postgres := cfg.StorageDriver == "postgres"
	if postgres {
		driver, dsn = "pgx", cfg.DatabaseURL
	} else {
		if err := os.MkdirAll(filepath.Dir(cfg.SQLitePath), 0o700); err != nil {
			return nil, fmt.Errorf("create sqlite directory: %w", err)
		}
		driver = "sqlite"
		dsn = "file:" + filepath.ToSlash(cfg.SQLitePath) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	}

	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if postgres {
		db.SetMaxOpenConns(20)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(30 * time.Minute)
	} else {
		db.SetMaxOpenConns(8)
		db.SetMaxIdleConns(4)
	}
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	s := &Store{db: db, postgres: postgres}
	if err = s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS codex_account (
			id INTEGER PRIMARY KEY CHECK (id = 1), email TEXT NOT NULL DEFAULT '', chatgpt_account_id TEXT NOT NULL,
			plan_type TEXT NOT NULL DEFAULT '', access_token_enc TEXT NOT NULL, refresh_token_enc TEXT NOT NULL,
			id_token_enc TEXT NOT NULL DEFAULT '', expires_at BIGINT NOT NULL, updated_at BIGINT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, key_hash TEXT NOT NULL UNIQUE, prefix TEXT NOT NULL,
			created_at BIGINT NOT NULL, last_used_at BIGINT, revoked_at BIGINT
		)`,
		`CREATE TABLE IF NOT EXISTS request_logs (
			id TEXT PRIMARY KEY, request_id TEXT NOT NULL, api_key_id TEXT, method TEXT NOT NULL, path TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT '', status INTEGER NOT NULL, duration_ms BIGINT NOT NULL,
			input_tokens BIGINT NOT NULL DEFAULT 0, output_tokens BIGINT NOT NULL DEFAULT 0,
			reasoning_tokens BIGINT NOT NULL DEFAULT 0, reasoning_effort TEXT NOT NULL DEFAULT '',
			upstream_reasoning_effort TEXT NOT NULL DEFAULT '', first_reasoning_ms BIGINT NOT NULL DEFAULT 0,
			first_output_ms BIGINT NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '', created_at BIGINT NOT NULL,
			FOREIGN KEY (api_key_id) REFERENCES api_keys(id) ON DELETE SET NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_created_at ON request_logs(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_api_key ON request_logs(api_key_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS quota_snapshot (id INTEGER PRIMARY KEY CHECK (id = 1), payload TEXT NOT NULL, fetched_at BIGINT NOT NULL)`,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()
	for _, statement := range statements {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("run migration: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return s.ensureRequestLogTelemetryColumns(ctx)
}

func (s *Store) ensureRequestLogTelemetryColumns(ctx context.Context) error {
	columns := []struct {
		name       string
		definition string
	}{
		{"reasoning_tokens", "BIGINT NOT NULL DEFAULT 0"},
		{"reasoning_effort", "TEXT NOT NULL DEFAULT ''"},
		{"upstream_reasoning_effort", "TEXT NOT NULL DEFAULT ''"},
		{"first_reasoning_ms", "BIGINT NOT NULL DEFAULT 0"},
		{"first_output_ms", "BIGINT NOT NULL DEFAULT 0"},
	}
	if s.postgres {
		for _, column := range columns {
			statement := fmt.Sprintf("ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS %s %s", column.name, column.definition)
			if _, err := s.db.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("add request telemetry column %s: %w", column.name, err)
			}
		}
		return nil
	}

	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(request_logs)`)
	if err != nil {
		return fmt.Errorf("inspect request log columns: %w", err)
	}
	existing := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue sql.NullString
		if err = rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("scan request log column: %w", err)
		}
		existing[name] = true
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read request log columns: %w", err)
	}
	if err = rows.Close(); err != nil {
		return fmt.Errorf("close request log column scan: %w", err)
	}
	for _, column := range columns {
		if existing[column.name] {
			continue
		}
		statement := fmt.Sprintf("ALTER TABLE request_logs ADD COLUMN %s %s", column.name, column.definition)
		if _, err = s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("add request telemetry column %s: %w", column.name, err)
		}
	}
	return nil
}

func (s *Store) query(raw string) string {
	if !s.postgres {
		return raw
	}
	var b strings.Builder
	index := 1
	for _, char := range raw {
		if char == '?' {
			fmt.Fprintf(&b, "$%d", index)
			index++
		} else {
			b.WriteRune(char)
		}
	}
	return b.String()
}

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, s.query(`SELECT value FROM settings WHERE key = ?`), key).Scan(&value)
	return value, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, s.query(`INSERT INTO settings(key, value, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`), key, value, time.Now().UnixMilli())
	return err
}

func (s *Store) SaveAccount(ctx context.Context, account Account) error {
	account.UpdatedAt = time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previousAccountID string
	err = tx.QueryRowContext(ctx, `SELECT chatgpt_account_id FROM codex_account WHERE id = 1`).Scan(&previousAccountID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, s.query(`INSERT INTO codex_account(
		id, email, chatgpt_account_id, plan_type, access_token_enc, refresh_token_enc, id_token_enc, expires_at, updated_at
	) VALUES(1, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET email = excluded.email, chatgpt_account_id = excluded.chatgpt_account_id,
		plan_type = excluded.plan_type, access_token_enc = excluded.access_token_enc,
		refresh_token_enc = excluded.refresh_token_enc, id_token_enc = excluded.id_token_enc,
		expires_at = excluded.expires_at, updated_at = excluded.updated_at`),
		account.Email, account.ChatGPTAccountID, account.PlanType, account.AccessTokenEncrypted,
		account.RefreshTokenEncrypted, account.IDTokenEncrypted, account.ExpiresAt, account.UpdatedAt)
	if err != nil {
		return err
	}
	if previousAccountID != "" && previousAccountID != account.ChatGPTAccountID {
		if _, err = tx.ExecContext(ctx, `DELETE FROM quota_snapshot WHERE id = 1`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetAccount(ctx context.Context) (Account, error) {
	var account Account
	err := s.db.QueryRowContext(ctx, `SELECT email, chatgpt_account_id, plan_type, access_token_enc,
		refresh_token_enc, id_token_enc, expires_at, updated_at FROM codex_account WHERE id = 1`).Scan(
		&account.Email, &account.ChatGPTAccountID, &account.PlanType, &account.AccessTokenEncrypted,
		&account.RefreshTokenEncrypted, &account.IDTokenEncrypted, &account.ExpiresAt, &account.UpdatedAt,
	)
	return account, err
}

func (s *Store) DeleteAccount(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM quota_snapshot WHERE id = 1`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM codex_account WHERE id = 1`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateAPIKey(ctx context.Context, key APIKey) error {
	_, err := s.db.ExecContext(ctx, s.query(`INSERT INTO api_keys(id, name, key_hash, prefix, created_at) VALUES(?, ?, ?, ?, ?)`),
		key.ID, key.Name, key.Hash, key.Prefix, key.CreatedAt)
	return err
}

func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, prefix, created_at, last_used_at, revoked_at FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make([]APIKey, 0)
	for rows.Next() {
		var key APIKey
		var lastUsed, revoked sql.NullInt64
		if err = rows.Scan(&key.ID, &key.Name, &key.Prefix, &key.CreatedAt, &lastUsed, &revoked); err != nil {
			return nil, err
		}
		key.LastUsedAt = nullableInt(lastUsed)
		key.RevokedAt = nullableInt(revoked)
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) FindActiveAPIKeyByHash(ctx context.Context, hash string) (APIKey, error) {
	var key APIKey
	var lastUsed sql.NullInt64
	err := s.db.QueryRowContext(ctx, s.query(`SELECT id, name, prefix, created_at, last_used_at FROM api_keys WHERE key_hash = ? AND revoked_at IS NULL`), hash).Scan(
		&key.ID, &key.Name, &key.Prefix, &key.CreatedAt, &lastUsed,
	)
	key.LastUsedAt = nullableInt(lastUsed)
	return key, err
}

func (s *Store) FindActiveAPIKeyByID(ctx context.Context, id string) (APIKey, error) {
	var key APIKey
	var lastUsed sql.NullInt64
	err := s.db.QueryRowContext(ctx, s.query(`SELECT id, name, prefix, created_at, last_used_at FROM api_keys WHERE id = ? AND revoked_at IS NULL`), id).Scan(
		&key.ID, &key.Name, &key.Prefix, &key.CreatedAt, &lastUsed,
	)
	key.LastUsedAt = nullableInt(lastUsed)
	return key, err
}

func (s *Store) TouchAPIKey(ctx context.Context, id string, at int64) error {
	_, err := s.db.ExecContext(ctx, s.query(`UPDATE api_keys SET last_used_at = ? WHERE id = ?`), at, id)
	return err
}

func (s *Store) RevokeAPIKey(ctx context.Context, id string) (bool, error) {
	result, err := s.db.ExecContext(ctx, s.query(`UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`), time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

func (s *Store) InsertRequestLog(ctx context.Context, entry RequestLog) error {
	_, err := s.db.ExecContext(ctx, s.query(`INSERT INTO request_logs(
		id, request_id, api_key_id, method, path, model, status, duration_ms, input_tokens, output_tokens,
		reasoning_tokens, reasoning_effort, upstream_reasoning_effort, first_reasoning_ms, first_output_ms, error, created_at
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`), entry.ID, entry.RequestID, nullString(entry.APIKeyID), entry.Method,
		entry.Path, entry.Model, entry.Status, entry.DurationMS, entry.InputTokens, entry.OutputTokens, entry.ReasoningTokens,
		entry.ReasoningEffort, entry.UpstreamReasoningEffort, entry.FirstReasoningMS, entry.FirstOutputMS, entry.Error, entry.CreatedAt)
	return err
}

func (s *Store) TodayStats(ctx context.Context, start, end int64) (TodayStats, error) {
	var stats TodayStats
	var average sql.NullFloat64
	err := s.db.QueryRowContext(ctx, s.query(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status >= 200 AND status < 300 THEN 1 ELSE 0 END), 0), AVG(duration_ms),
		COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(reasoning_tokens), 0)
		FROM request_logs WHERE created_at >= ? AND created_at < ?`), start, end).Scan(
		&stats.Requests, &stats.Successes, &average, &stats.InputTokens, &stats.OutputTokens, &stats.ReasoningTokens,
	)
	if err != nil {
		return stats, err
	}
	if average.Valid {
		stats.AverageMS = average.Float64
	}
	if stats.Requests > 0 {
		stats.SuccessRate = float64(stats.Successes) * 100 / float64(stats.Requests)
	}
	return stats, nil
}

func (s *Store) ListRequestLogs(ctx context.Context, start, end int64, limit int) ([]RequestLog, error) {
	rows, err := s.db.QueryContext(ctx, s.query(`SELECT l.id, l.request_id, COALESCE(l.api_key_id, ''), COALESCE(k.name, ''),
		l.method, l.path, l.model, l.status, l.duration_ms, l.input_tokens, l.output_tokens, l.reasoning_tokens,
		l.reasoning_effort, l.upstream_reasoning_effort, l.first_reasoning_ms, l.first_output_ms, l.error, l.created_at
		FROM request_logs l LEFT JOIN api_keys k ON l.api_key_id = k.id
		WHERE l.created_at >= ? AND l.created_at < ? ORDER BY l.created_at DESC LIMIT ?`), start, end, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]RequestLog, 0)
	for rows.Next() {
		var entry RequestLog
		if err = rows.Scan(&entry.ID, &entry.RequestID, &entry.APIKeyID, &entry.APIKeyName, &entry.Method, &entry.Path,
			&entry.Model, &entry.Status, &entry.DurationMS, &entry.InputTokens, &entry.OutputTokens, &entry.ReasoningTokens,
			&entry.ReasoningEffort, &entry.UpstreamReasoningEffort, &entry.FirstReasoningMS, &entry.FirstOutputMS,
			&entry.Error, &entry.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (s *Store) DeleteOldRequestLogs(ctx context.Context, before int64) (int64, error) {
	result, err := s.db.ExecContext(ctx, s.query(`DELETE FROM request_logs WHERE created_at < ?`), before)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) SaveQuota(ctx context.Context, snapshot QuotaSnapshot) error {
	_, err := s.db.ExecContext(ctx, s.query(`INSERT INTO quota_snapshot(id, payload, fetched_at) VALUES(1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET payload = excluded.payload, fetched_at = excluded.fetched_at`), snapshot.Payload, snapshot.FetchedAt)
	return err
}

func (s *Store) GetQuota(ctx context.Context) (QuotaSnapshot, error) {
	var snapshot QuotaSnapshot
	err := s.db.QueryRowContext(ctx, `SELECT payload, fetched_at FROM quota_snapshot WHERE id = 1`).Scan(&snapshot.Payload, &snapshot.FetchedAt)
	return snapshot, err
}

func nullableInt(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func IsNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) }
