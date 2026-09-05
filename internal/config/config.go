package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

type Config struct {
	Addr                 string
	PublicURL            string
	StorageDriver        string
	SQLitePath           string
	DatabaseURL          string
	RedisURL             string
	MasterKey            string
	MasterKeyFile        string
	AdminPassword        string
	LogPath              string
	CodexClientVersion   string
	CodexUserAgent       string
	CodexChromeTLS       bool
	CodexIdentityRemap   bool
	CodexWSHTTPFallback  bool
	UpstreamBaseURL      string
	RequestRetentionDays int
	MaxRequestBytes      int64
	SessionTTL           time.Duration
	Location             *time.Location
	CookieSecure         bool
	TrustedProxyCIDRs    []netip.Prefix
}

func Load() (Config, error) {
	version := env("CODEX_CLIENT_VERSION", "0.146.0")
	sqlitePath := env("SQLITE_PATH", "./data/codexone.db")
	cfg := Config{
		Addr:                 env("LISTEN_ADDR", ":8080"),
		PublicURL:            strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/"),
		StorageDriver:        strings.ToLower(env("STORAGE_DRIVER", "sqlite")),
		SQLitePath:           sqlitePath,
		DatabaseURL:          strings.TrimSpace(os.Getenv("DATABASE_URL")),
		RedisURL:             strings.TrimSpace(os.Getenv("REDIS_URL")),
		MasterKey:            strings.TrimSpace(os.Getenv("MASTER_KEY")),
		MasterKeyFile:        env("MASTER_KEY_FILE", filepath.Join(filepath.Dir(sqlitePath), "master.key")),
		AdminPassword:        os.Getenv("ADMIN_PASSWORD"),
		LogPath:              env("LOG_PATH", "./data/codexone.log"),
		CodexClientVersion:   version,
		CodexUserAgent:       env("CODEX_USER_AGENT", fmt.Sprintf("codex-tui/%s (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; %s)", version, version)),
		CodexChromeTLS:       envBool("CODEX_CHROME_TLS", true),
		CodexIdentityRemap:   envBool("CODEX_IDENTITY_REMAP", true),
		CodexWSHTTPFallback:  envBool("CODEX_WEBSOCKET_HTTP_FALLBACK", true),
		UpstreamBaseURL:      strings.TrimRight(env("OPENAI_UPSTREAM_BASE_URL", "https://chatgpt.com"), "/"),
		RequestRetentionDays: envInt("REQUEST_RETENTION_DAYS", 30),
		MaxRequestBytes:      int64(envInt("MAX_REQUEST_MIB", 32)) << 20,
		SessionTTL:           time.Duration(envInt("SESSION_TTL_HOURS", 24)) * time.Hour,
	}

	publicURL, err := url.Parse(cfg.PublicURL)
	if err != nil || publicURL.Scheme == "" || publicURL.Host == "" {
		return Config{}, fmt.Errorf("PUBLIC_URL must be an absolute URL")
	}
	if strings.EqualFold(publicURL.Hostname(), "xxx.xxx.com") {
		return Config{}, fmt.Errorf("PUBLIC_URL still contains the example hostname xxx.xxx.com")
	}
	cfg.CookieSecure = strings.EqualFold(publicURL.Scheme, "https")
	cfg.TrustedProxyCIDRs, err = parseTrustedProxyCIDRs(os.Getenv("TRUSTED_PROXY_CIDRS"))
	if err != nil {
		return Config{}, err
	}

	upstreamURL, err := url.Parse(cfg.UpstreamBaseURL)
	if err != nil || upstreamURL.Scheme != "https" || upstreamURL.Host == "" {
		return Config{}, fmt.Errorf("OPENAI_UPSTREAM_BASE_URL must be an absolute HTTPS URL")
	}

	timezone := env("APP_TIMEZONE", "Asia/Shanghai")
	cfg.Location, err = time.LoadLocation(timezone)
	if err != nil {
		return Config{}, fmt.Errorf("invalid APP_TIMEZONE %q: %w", timezone, err)
	}

	switch cfg.StorageDriver {
	case "sqlite":
		if cfg.SQLitePath == "" {
			return Config{}, fmt.Errorf("SQLITE_PATH is required")
		}
	case "postgres":
		if cfg.DatabaseURL == "" {
			return Config{}, fmt.Errorf("DATABASE_URL is required in postgres mode")
		}
		if cfg.RedisURL == "" {
			return Config{}, fmt.Errorf("REDIS_URL is required in postgres mode")
		}
		if cfg.MasterKey == "" {
			return Config{}, fmt.Errorf("MASTER_KEY is required in postgres mode")
		}
	default:
		return Config{}, fmt.Errorf("STORAGE_DRIVER must be sqlite or postgres")
	}
	return cfg, nil
}

func parseTrustedProxyCIDRs(raw string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			address, addressErr := netip.ParseAddr(entry)
			if addressErr != nil {
				return nil, fmt.Errorf("invalid TRUSTED_PROXY_CIDRS entry %q", entry)
			}
			address = address.Unmap()
			prefix = netip.PrefixFrom(address, address.BitLen())
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func envBool(name string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return value
}
