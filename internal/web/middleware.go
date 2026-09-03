package web

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Ankairis/CodexOne/internal/proxy"
	"github.com/Ankairis/CodexOne/internal/security"
)

const sessionCookie = "codexone_session"

func serverHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func v1CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, Session-Id, Thread-Id, X-Client-Request-Id")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id, OpenAI-Request-Id, Retry-After")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "sign in required")
			return
		}
		value, err := s.sessions.Get(r.Context(), "admin:"+security.HashSecret(cookie.Value))
		if err != nil || subtle.ConstantTimeCompare([]byte(value), []byte("authenticated")) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "session expired")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) checkOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		originURL, originErr := url.Parse(origin)
		publicURL, publicErr := url.Parse(s.cfg.PublicURL)
		if originErr != nil || publicErr != nil || !strings.EqualFold(originURL.Scheme, publicURL.Scheme) || !strings.EqualFold(originURL.Host, publicURL.Host) {
			writeError(w, http.StatusForbidden, "origin_rejected", "request origin is not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		provided := strings.TrimSpace(r.Header.Get("X-API-Key"))
		if authorization := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
			provided = strings.TrimSpace(authorization[7:])
		}
		if provided == "" {
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "API key is required")
			return
		}
		key, err := s.database.FindActiveAPIKeyByHash(r.Context(), security.HashSecret(provided))
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "API key is invalid or revoked")
			return
		}
		_ = s.database.TouchAPIKey(r.Context(), key.ID, time.Now().UnixMilli())
		next.ServeHTTP(w, r.WithContext(proxy.WithAPIKey(r.Context(), key)))
	})
}

func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

type loginAttempt struct {
	count       int
	windowStart time.Time
	blockedTill time.Time
}

type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]loginAttempt
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{attempts: make(map[string]loginAttempt)} }

func (l *loginLimiter) Allow(address string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	attempt := l.attempts[address]
	if now.Before(attempt.blockedTill) {
		return false, time.Until(attempt.blockedTill)
	}
	if attempt.windowStart.IsZero() || now.Sub(attempt.windowStart) > 15*time.Minute {
		delete(l.attempts, address)
	}
	return true, 0
}

func (l *loginLimiter) Failure(address string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	attempt := l.attempts[address]
	if attempt.windowStart.IsZero() || now.Sub(attempt.windowStart) > 15*time.Minute {
		attempt = loginAttempt{windowStart: now}
	}
	attempt.count++
	if attempt.count >= 5 {
		attempt.blockedTill = now.Add(30 * time.Minute)
		attempt.count = 0
	}
	l.attempts[address] = attempt
}

func (l *loginLimiter) Success(address string) {
	l.mu.Lock()
	delete(l.attempts, address)
	l.mu.Unlock()
}
