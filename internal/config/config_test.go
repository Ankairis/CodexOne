package config

import (
	"path/filepath"
	"testing"
)

func TestLoadDefaultSQLiteConfigurationIncludesTimezoneData(t *testing.T) {
	for _, name := range []string{
		"CODEX_CLIENT_VERSION", "CODEX_USER_AGENT", "LISTEN_ADDR", "PUBLIC_URL", "STORAGE_DRIVER",
		"CODEX_CHROME_TLS", "CODEX_IDENTITY_REMAP", "CODEX_WEBSOCKET_HTTP_FALLBACK",
		"DATABASE_URL", "REDIS_URL", "MASTER_KEY", "ADMIN_PASSWORD", "OPENAI_UPSTREAM_BASE_URL",
		"REQUEST_RETENTION_DAYS", "MAX_REQUEST_MIB", "SESSION_TTL_HOURS", "APP_TIMEZONE", "TRUSTED_PROXY_CIDRS",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("SQLITE_PATH", filepath.Join(t.TempDir(), "codexone.db"))
	t.Setenv("MASTER_KEY_FILE", filepath.Join(t.TempDir(), "master.key"))
	t.Setenv("LOG_PATH", filepath.Join(t.TempDir(), "codexone.log"))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.StorageDriver != "sqlite" || cfg.Location.String() != "Asia/Shanghai" {
		t.Fatalf("configuration = %#v", cfg)
	}
	if cfg.CodexUserAgent == "" || cfg.CodexClientVersion != "0.146.0" || !cfg.CodexChromeTLS ||
		!cfg.CodexIdentityRemap || !cfg.CodexWSHTTPFallback {
		t.Fatalf("Codex identity was not initialized: %#v", cfg)
	}
}

func TestLoadCanDisableChromeTLS(t *testing.T) {
	t.Setenv("PUBLIC_URL", "http://localhost:8080")
	t.Setenv("CODEX_CHROME_TLS", "false")
	t.Setenv("SQLITE_PATH", filepath.Join(t.TempDir(), "codexone.db"))
	t.Setenv("MASTER_KEY_FILE", filepath.Join(t.TempDir(), "master.key"))
	t.Setenv("LOG_PATH", filepath.Join(t.TempDir(), "codexone.log"))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CodexChromeTLS {
		t.Fatal("CODEX_CHROME_TLS=false was ignored")
	}
}

func TestLoadCanDisableIdentityRemapAndWebSocketFallback(t *testing.T) {
	t.Setenv("PUBLIC_URL", "http://localhost:8080")
	t.Setenv("CODEX_IDENTITY_REMAP", "false")
	t.Setenv("CODEX_WEBSOCKET_HTTP_FALLBACK", "false")
	t.Setenv("SQLITE_PATH", filepath.Join(t.TempDir(), "codexone.db"))
	t.Setenv("MASTER_KEY_FILE", filepath.Join(t.TempDir(), "master.key"))
	t.Setenv("LOG_PATH", filepath.Join(t.TempDir(), "codexone.log"))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CodexIdentityRemap || cfg.CodexWSHTTPFallback {
		t.Fatalf("optional safeguards were not disabled: %#v", cfg)
	}
}

func TestParseTrustedProxyCIDRs(t *testing.T) {
	prefixes, err := parseTrustedProxyCIDRs("10.0.0.5, 192.168.0.0/16, 2001:db8::/32")
	if err != nil {
		t.Fatal(err)
	}
	if len(prefixes) != 3 || prefixes[0].String() != "10.0.0.5/32" || prefixes[1].String() != "192.168.0.0/16" || prefixes[2].String() != "2001:db8::/32" {
		t.Fatalf("prefixes = %v", prefixes)
	}
	if _, err = parseTrustedProxyCIDRs("10.0.0.0/not-a-prefix"); err == nil {
		t.Fatal("invalid trusted proxy entry was accepted")
	}
}

func TestLoadRejectsExamplePublicURL(t *testing.T) {
	t.Setenv("PUBLIC_URL", "https://xxx.xxx.com")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted the example PUBLIC_URL")
	}
}

func TestLoadPostgresRequiresRedisAndMasterKey(t *testing.T) {
	t.Setenv("STORAGE_DRIVER", "postgres")
	t.Setenv("PUBLIC_URL", "https://codexone.example")
	t.Setenv("DATABASE_URL", "postgres://localhost/codexone")
	t.Setenv("REDIS_URL", "")
	t.Setenv("MASTER_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted postgres mode without Redis or a master key")
	}
}
