package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Ankairis/CodexOne/internal/codex"
	"github.com/Ankairis/CodexOne/internal/config"
	appLog "github.com/Ankairis/CodexOne/internal/logging"
	"github.com/Ankairis/CodexOne/internal/proxy"
	"github.com/Ankairis/CodexOne/internal/session"
	"github.com/Ankairis/CodexOne/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
	cfg      config.Config
	database *store.Store
	sessions session.Store
	codex    *codex.Manager
	proxy    *proxy.Service
	logger   *slog.Logger
	logs     *appLog.Ring
	login    *loginLimiter
}

func New(cfg config.Config, database *store.Store, sessions session.Store, codexManager *codex.Manager, proxyService *proxy.Service, logger *slog.Logger, logs *appLog.Ring) http.Handler {
	server := &Server{
		cfg:      cfg,
		database: database,
		sessions: sessions,
		codex:    codexManager,
		proxy:    proxyService,
		logger:   logger,
		logs:     logs,
		login:    newLoginLimiter(),
	}
	router := chi.NewRouter()
	router.Use(middleware.Recoverer)
	router.Use(serverHeaders)

	router.Get("/healthz", server.health)
	router.Get("/api/auth/session", server.authSession)
	router.Post("/api/auth/login", server.loginHandler)

	router.Group(func(admin chi.Router) {
		admin.Use(server.requireAdmin)
		admin.Use(server.checkOrigin)
		admin.Post("/api/auth/logout", server.logoutHandler)
		admin.Put("/api/admin/password", server.changePassword)
		admin.Get("/api/admin/overview", server.overview)
		admin.Get("/api/admin/logs", server.applicationLogs)
		admin.Get("/api/admin/account", server.account)
		admin.Delete("/api/admin/account", server.deleteAccount)
		admin.Post("/api/admin/account/device", server.startDeviceLogin)
		admin.Get("/api/admin/account/device/{flowID}", server.pollDeviceLogin)
		admin.Post("/api/admin/account/import", server.importAccount)
		admin.Post("/api/admin/account/quota", server.refreshQuota)
		admin.Get("/api/admin/keys", server.listAPIKeys)
		admin.Post("/api/admin/keys", server.createAPIKey)
		admin.Delete("/api/admin/keys/{id}", server.revokeAPIKey)
	})

	router.Route("/v1", func(v1 chi.Router) {
		v1.Use(v1CORS)
		v1.Use(server.requireAPIKey)
		v1.Options("/*", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		v1.Get("/models", server.proxy.Models)
		v1.Post("/chat/completions", server.proxy.ChatCompletions)
		v1.Post("/responses", server.proxy.Responses)
		v1.Post("/responses/compact", server.proxy.Compact)
		v1.Post("/responses/input_tokens", server.proxy.InputTokens)
	})

	assetsRoot, err := fs.Sub(frontendAssets, "dist")
	if err != nil {
		panic(fmt.Sprintf("load embedded frontend: %v", err))
	}
	fileServer := http.FileServer(http.FS(assetsRoot))
	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/v1/") {
			writeError(w, http.StatusNotFound, "not_found", "route not found")
			return
		}
		if strings.Contains(strings.TrimPrefix(r.URL.Path, "/"), ".") {
			fileServer.ServeHTTP(w, r)
			return
		}
		index, readErr := fs.ReadFile(assetsRoot, "index.html")
		if readErr != nil {
			writeError(w, http.StatusInternalServerError, "frontend_unavailable", "frontend asset is unavailable")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
	return router
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "codexone"})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is invalid")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON value")
		return false
	}
	return true
}

func (s *Server) dayRange(raw string) (time.Time, time.Time, error) {
	if raw == "" {
		now := time.Now().In(s.cfg.Location)
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.cfg.Location)
		return start, start.AddDate(0, 0, 1), nil
	}
	start, err := time.ParseInLocation("2006-01-02", raw, s.cfg.Location)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("date must use YYYY-MM-DD")
	}
	return start, start.AddDate(0, 0, 1), nil
}

func backgroundContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Second)
}
