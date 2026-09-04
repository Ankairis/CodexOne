package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Ankairis/CodexOne/internal/codex"
	"github.com/Ankairis/CodexOne/internal/security"
	"github.com/Ankairis/CodexOne/internal/store"
	"github.com/go-chi/chi/v5"
)

const adminPasswordSetting = "admin_password_hash"

func EnsureAdminPassword(ctx context.Context, database *store.Store, configured string) (string, bool, error) {
	if _, err := database.GetSetting(ctx, adminPasswordSetting); err == nil {
		return "", false, nil
	} else if !store.IsNotFound(err) {
		return "", false, err
	}
	password := configured
	generated := password == ""
	if generated {
		var err error
		password, err = security.RandomPassword()
		if err != nil {
			return "", false, err
		}
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return "", false, err
	}
	if err = database.SetSetting(ctx, adminPasswordSetting, hash); err != nil {
		return "", false, err
	}
	return password, generated, nil
}

func (s *Server) authSession(w http.ResponseWriter, r *http.Request) {
	authenticated := false
	if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
		value, getErr := s.sessions.Get(r.Context(), "admin:"+security.HashSecret(cookie.Value))
		authenticated = getErr == nil && value == "authenticated"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": authenticated,
		"base_url":      s.cfg.PublicURL + "/v1",
		"storage":       s.cfg.StorageDriver,
		"client":        "codex-tui/" + s.cfg.CodexClientVersion,
	})
}

func (s *Server) loginHandler(w http.ResponseWriter, r *http.Request) {
	address := clientAddress(r)
	if allowed, wait := s.login.Allow(address); !allowed {
		w.Header().Set("Retry-After", fmt.Sprintf("%.0f", wait.Seconds()))
		writeError(w, http.StatusTooManyRequests, "login_blocked", "too many failed attempts")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	hash, err := s.database.GetSetting(r.Context(), adminPasswordSetting)
	if err != nil || !security.CheckPassword(hash, body.Password) {
		s.login.Failure(address)
		time.Sleep(250 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "invalid_password", "password is incorrect")
		return
	}
	token, err := security.RandomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_failed", "could not create session")
		return
	}
	if err = s.sessions.Put(r.Context(), "admin:"+security.HashSecret(token), "authenticated", s.cfg.SessionTTL); err != nil {
		writeError(w, http.StatusInternalServerError, "session_failed", "could not store session")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: int(s.cfg.SessionTTL.Seconds()),
		HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode,
	})
	s.login.Success(address)
	s.logger.Info("admin signed in", "remote_addr", address)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
}

func (s *Server) logoutHandler(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		_ = s.sessions.Delete(r.Context(), "admin:"+security.HashSecret(cookie.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cfg.CookieSecure, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Current string `json:"current_password"`
		Next    string `json:"new_password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	currentHash, err := s.database.GetSetting(r.Context(), adminPasswordSetting)
	if err != nil || !security.CheckPassword(currentHash, body.Current) {
		writeError(w, http.StatusUnauthorized, "invalid_password", "current password is incorrect")
		return
	}
	nextHash, err := security.HashPassword(body.Next)
	if err != nil {
		writeError(w, http.StatusBadRequest, "weak_password", err.Error())
		return
	}
	if err = s.database.SetSetting(r.Context(), adminPasswordSetting, nextHash); err != nil {
		writeError(w, http.StatusInternalServerError, "password_update_failed", "could not update password")
		return
	}
	s.logger.Info("admin password changed")
	writeJSON(w, http.StatusOK, map[string]any{"updated": true})
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	start, end, err := s.dayRange(r.URL.Query().Get("date"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_date", err.Error())
		return
	}
	stats, err := s.database.TodayStats(r.Context(), start.UnixMilli(), end.UnixMilli())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "overview_failed", "could not load statistics")
		return
	}
	entries, err := s.database.ListRequestLogs(r.Context(), start.UnixMilli(), end.UnixMilli(), 150)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "overview_failed", "could not load request details")
		return
	}
	account, _ := s.codex.Account(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"date": start.Format("2006-01-02"), "stats": stats, "requests": entries,
		"base_url": s.cfg.PublicURL + "/v1", "account_connected": account.Connected,
	})
}

func (s *Server) applicationLogs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"logs": s.logs.List(300)})
}

func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	view, err := s.codex.Account(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "account_failed", "could not load account")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.codex.DeleteAccount(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "account_delete_failed", "could not disconnect account")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) startDeviceLogin(w http.ResponseWriter, r *http.Request) {
	flow, err := s.codex.StartDeviceFlow(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "device_login_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, flow)
}

func (s *Server) pollDeviceLogin(w http.ResponseWriter, r *http.Request) {
	flowID := chi.URLParam(r, "flowID")
	if len(flowID) < 16 || len(flowID) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_flow", "device login flow is invalid")
		return
	}
	status, err := s.codex.PollDeviceFlow(r.Context(), flowID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "device_login_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) startBrowserLogin(w http.ResponseWriter, r *http.Request) {
	flow, err := s.codex.StartBrowserFlow(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "browser_login_failed", "could not start browser login")
		return
	}
	writeJSON(w, http.StatusOK, flow)
}

func (s *Server) completeBrowserLogin(w http.ResponseWriter, r *http.Request) {
	flowID := chi.URLParam(r, "flowID")
	if len(flowID) < 16 || len(flowID) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_flow", "browser login flow is invalid")
		return
	}
	var body struct {
		CallbackURL string `json:"callback_url"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	view, err := s.codex.CompleteBrowserFlow(r.Context(), flowID, body.CallbackURL)
	if err != nil {
		if errors.Is(err, codex.ErrBrowserOAuthInput) {
			writeError(w, http.StatusBadRequest, "invalid_callback", strings.TrimPrefix(err.Error(), codex.ErrBrowserOAuthInput.Error()+": "))
			return
		}
		writeError(w, http.StatusBadGateway, "browser_login_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) importAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	view, err := s.codex.ImportAuthJSON(r.Context(), []byte(body.Content))
	if err != nil {
		writeError(w, http.StatusBadRequest, "import_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) refreshQuota(w http.ResponseWriter, r *http.Request) {
	quota, fetchedAt, err := s.codex.FetchQuota(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "quota_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"quota": quota, "fetched_at": fetchedAt})
}

func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.database.ListAPIKeys(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "keys_failed", "could not load API keys")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.Name) > 64 {
		writeError(w, http.StatusBadRequest, "invalid_name", "name must contain 1 to 64 characters")
		return
	}
	plain, prefix, hash, err := security.NewAPIKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "key_failed", "could not generate API key")
		return
	}
	id, err := security.RandomToken(16)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "key_failed", "could not generate API key")
		return
	}
	key := store.APIKey{ID: "key_" + id, Name: body.Name, Hash: hash, Prefix: prefix, CreatedAt: time.Now().UnixMilli()}
	if err = s.database.CreateAPIKey(r.Context(), key); err != nil {
		writeError(w, http.StatusInternalServerError, "key_failed", "could not save API key")
		return
	}
	s.logger.Info("API key created", "key_id", key.ID, "name", key.Name)
	writeJSON(w, http.StatusCreated, map[string]any{"key": key, "secret": plain})
}

func (s *Server) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !strings.HasPrefix(id, "key_") {
		writeError(w, http.StatusBadRequest, "invalid_key", "API key ID is invalid")
		return
	}
	revoked, err := s.database.RevokeAPIKey(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "key_failed", "could not revoke API key")
		return
	}
	if !revoked {
		writeError(w, http.StatusNotFound, "key_not_found", "API key was not found or already revoked")
		return
	}
	s.logger.Info("API key revoked", "key_id", id)
	w.WriteHeader(http.StatusNoContent)
}
