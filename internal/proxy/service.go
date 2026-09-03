package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Ankairis/CodexOne/internal/codex"
	"github.com/Ankairis/CodexOne/internal/config"
	"github.com/Ankairis/CodexOne/internal/security"
	"github.com/Ankairis/CodexOne/internal/store"
)

type apiKeyContextKey struct{}

func WithAPIKey(ctx context.Context, key store.APIKey) context.Context {
	return context.WithValue(ctx, apiKeyContextKey{}, key)
}

func APIKeyFromContext(ctx context.Context) (store.APIKey, bool) {
	key, ok := ctx.Value(apiKeyContextKey{}).(store.APIKey)
	return key, ok
}

type Service struct {
	cfg      config.Config
	auth     *codex.Manager
	store    *store.Store
	logger   *slog.Logger
	client   *http.Client
	modelsMu sync.Mutex
	models   []byte
	modelsAt time.Time
}

func New(cfg config.Config, auth *codex.Manager, database *store.Store, logger *slog.Logger) *Service {
	return &Service{
		cfg:    cfg,
		auth:   auth,
		store:  database,
		logger: logger,
		client: &http.Client{},
	}
}

func (s *Service) Responses(w http.ResponseWriter, r *http.Request) {
	s.handleResponsesPath(w, r, "/backend-api/codex/responses")
}

func (s *Service) Compact(w http.ResponseWriter, r *http.Request) {
	s.handlePassthroughJSON(w, r, "/backend-api/codex/responses/compact")
}

func (s *Service) InputTokens(w http.ResponseWriter, r *http.Request) {
	s.handlePassthroughJSON(w, r, "/backend-api/codex/responses/input_tokens")
}

func (s *Service) Models(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID := newID("req")
	status := http.StatusOK
	errText := ""
	defer func() { s.record(r, requestID, "", status, started, 0, 0, errText) }()

	s.modelsMu.Lock()
	if len(s.models) > 0 && time.Since(s.modelsAt) < 10*time.Minute {
		cached := append([]byte(nil), s.models...)
		s.modelsMu.Unlock()
		writeJSONBytes(w, http.StatusOK, cached, requestID)
		return
	}
	s.modelsMu.Unlock()

	credential, err := s.auth.FreshCredential(r.Context())
	if err != nil {
		status, errText = http.StatusServiceUnavailable, err.Error()
		writeError(w, status, "account_unavailable", errText, requestID)
		return
	}
	endpoint, _ := url.Parse(s.cfg.UpstreamBaseURL + "/backend-api/codex/models")
	query := endpoint.Query()
	query.Set("client_version", s.cfg.CodexClientVersion)
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint.String(), nil)
	if err != nil {
		status, errText = http.StatusInternalServerError, err.Error()
		writeError(w, status, "request_failed", errText, requestID)
		return
	}
	s.auth.ApplyCodexHeaders(req, credential, "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		status, errText = http.StatusBadGateway, err.Error()
		writeError(w, status, "upstream_unavailable", errText, requestID)
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		status, errText = http.StatusBadGateway, err.Error()
		writeError(w, status, "upstream_read_failed", errText, requestID)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status, errText = resp.StatusCode, compactError(raw)
		writeUpstreamError(w, resp, raw, requestID)
		return
	}
	converted, err := convertModels(raw)
	if err != nil {
		status, errText = http.StatusBadGateway, err.Error()
		writeError(w, status, "invalid_upstream_response", errText, requestID)
		return
	}
	s.modelsMu.Lock()
	s.models, s.modelsAt = append([]byte(nil), converted...), time.Now()
	s.modelsMu.Unlock()
	writeJSONBytes(w, http.StatusOK, converted, requestID)
}

func (s *Service) handleResponsesPath(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	started := time.Now()
	requestID := newID("req")
	status := http.StatusOK
	errText, model := "", ""
	var inputTokens, outputTokens int64
	defer func() { s.record(r, requestID, model, status, started, inputTokens, outputTokens, errText) }()

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes))
	if err != nil {
		status, errText = http.StatusBadRequest, "request body is too large or unreadable"
		writeError(w, status, "invalid_request", errText, requestID)
		return
	}
	body, requestedStream, parsedModel, err := normalizeResponsesRequest(raw)
	model = parsedModel
	if err != nil {
		status, errText = http.StatusBadRequest, err.Error()
		writeError(w, status, "invalid_request", errText, requestID)
		return
	}
	credential, err := s.auth.FreshCredential(r.Context())
	if err != nil {
		status, errText = http.StatusServiceUnavailable, err.Error()
		writeError(w, status, "account_unavailable", errText, requestID)
		return
	}
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.cfg.UpstreamBaseURL+upstreamPath, bytes.NewReader(body))
	if err != nil {
		status, errText = http.StatusInternalServerError, err.Error()
		writeError(w, status, "request_failed", errText, requestID)
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	copyCodexContextHeaders(upstreamReq.Header, r.Header)
	s.auth.ApplyCodexHeaders(upstreamReq, credential, "text/event-stream")

	resp, err := s.client.Do(upstreamReq)
	if err != nil {
		status, errText = http.StatusBadGateway, err.Error()
		writeError(w, status, "upstream_unavailable", errText, requestID)
		return
	}
	defer resp.Body.Close()
	copyQuotaHeaders(w.Header(), resp.Header)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rawError, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		status, errText = resp.StatusCode, compactError(rawError)
		writeUpstreamError(w, resp, rawError, requestID)
		return
	}

	if requestedStream {
		status, inputTokens, outputTokens, errText = s.streamResponse(w, resp, requestID)
		return
	}
	final, usage, err := collectResponse(resp.Body)
	if err != nil {
		status, errText = http.StatusBadGateway, err.Error()
		writeError(w, status, "upstream_stream_failed", errText, requestID)
		return
	}
	inputTokens, outputTokens = usage.InputTokens, usage.OutputTokens
	writeJSONBytes(w, http.StatusOK, final, requestID)
}

func (s *Service) handlePassthroughJSON(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	started := time.Now()
	requestID := newID("req")
	status := http.StatusOK
	errText, model := "", ""
	defer func() { s.record(r, requestID, model, status, started, 0, 0, errText) }()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes))
	if err != nil || !json.Valid(body) {
		status, errText = http.StatusBadRequest, "request body must be valid JSON"
		writeError(w, status, "invalid_request", errText, requestID)
		return
	}
	var metadata struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &metadata)
	model = metadata.Model
	credential, err := s.auth.FreshCredential(r.Context())
	if err != nil {
		status, errText = http.StatusServiceUnavailable, err.Error()
		writeError(w, status, "account_unavailable", errText, requestID)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.cfg.UpstreamBaseURL+upstreamPath, bytes.NewReader(body))
	if err != nil {
		status, errText = http.StatusInternalServerError, err.Error()
		writeError(w, status, "request_failed", errText, requestID)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	copyCodexContextHeaders(req.Header, r.Header)
	s.auth.ApplyCodexHeaders(req, credential, "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		status, errText = http.StatusBadGateway, err.Error()
		writeError(w, status, "upstream_unavailable", errText, requestID)
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		status, errText = http.StatusBadGateway, err.Error()
		writeError(w, status, "upstream_read_failed", errText, requestID)
		return
	}
	copyQuotaHeaders(w.Header(), resp.Header)
	status = resp.StatusCode
	if status < 200 || status >= 300 {
		errText = compactError(raw)
	}
	w.Header().Set("Content-Type", firstNonEmpty(resp.Header.Get("Content-Type"), "application/json"))
	w.Header().Set("X-Request-Id", requestID)
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func (s *Service) streamResponse(w http.ResponseWriter, resp *http.Response, requestID string) (int, int64, int64, string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "server does not support streaming", requestID)
		return http.StatusInternalServerError, 0, 0, "streaming unsupported"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Request-Id", requestID)
	w.WriteHeader(http.StatusOK)

	reader := bufio.NewReader(resp.Body)
	usage := tokenUsage{}
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			parseSSEUsage(line, &usage)
			if _, writeErr := w.Write(line); writeErr != nil {
				return 499, usage.InputTokens, usage.OutputTokens, "client disconnected"
			}
			flusher.Flush()
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return http.StatusOK, usage.InputTokens, usage.OutputTokens, ""
			}
			return http.StatusBadGateway, usage.InputTokens, usage.OutputTokens, err.Error()
		}
	}
}

func (s *Service) record(r *http.Request, requestID, model string, status int, started time.Time, inputTokens, outputTokens int64, errText string) {
	key, _ := APIKeyFromContext(r.Context())
	entry := store.RequestLog{
		ID:           newID("log"),
		RequestID:    requestID,
		APIKeyID:     key.ID,
		Method:       r.Method,
		Path:         r.URL.Path,
		Model:        model,
		Status:       status,
		DurationMS:   time.Since(started).Milliseconds(),
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		Error:        trimText(errText, 500),
		CreatedAt:    time.Now().UnixMilli(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.store.InsertRequestLog(ctx, entry); err != nil {
		s.logger.Error("save request log", "error", err, "request_id", requestID)
	}
	s.logger.Info("proxy request", "request_id", requestID, "path", r.URL.Path, "model", model, "status", status, "duration_ms", entry.DurationMS)
}

type tokenUsage struct {
	InputTokens  int64
	OutputTokens int64
}

func collectResponse(reader io.Reader) ([]byte, tokenUsage, error) {
	buffered := bufio.NewReader(reader)
	var final []byte
	usage := tokenUsage{}
	var upstreamErr string
	for {
		line, err := buffered.ReadBytes('\n')
		if len(line) > 0 {
			payload := ssePayload(line)
			if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
				var event struct {
					Type     string          `json:"type"`
					Response json.RawMessage `json:"response"`
					Error    struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				if json.Unmarshal(payload, &event) == nil {
					parseResponseUsage(event.Response, &usage)
					if (event.Type == "response.completed" || event.Type == "response.incomplete") && len(event.Response) > 0 {
						final = append(final[:0], event.Response...)
					}
					if event.Type == "error" {
						upstreamErr = event.Error.Message
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, usage, fmt.Errorf("read upstream stream: %w", err)
		}
	}
	if upstreamErr != "" {
		return nil, usage, fmt.Errorf("upstream error: %s", upstreamErr)
	}
	if len(final) == 0 {
		return nil, usage, fmt.Errorf("upstream stream ended without a completed response")
	}
	return final, usage, nil
}

func parseSSEUsage(line []byte, usage *tokenUsage) {
	payload := ssePayload(line)
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	var event struct {
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(payload, &event) == nil {
		parseResponseUsage(event.Response, usage)
	}
}

func parseResponseUsage(response json.RawMessage, usage *tokenUsage) {
	if len(response) == 0 {
		return
	}
	var payload struct {
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(response, &payload) == nil {
		if payload.Usage.InputTokens > 0 {
			usage.InputTokens = payload.Usage.InputTokens
		}
		if payload.Usage.OutputTokens > 0 {
			usage.OutputTokens = payload.Usage.OutputTokens
		}
	}
}

func ssePayload(line []byte) []byte {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil
	}
	return bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
}

func copyCodexContextHeaders(target, source http.Header) {
	for _, name := range []string{
		"X-Codex-Beta-Features", "X-Codex-Turn-Metadata", "X-Client-Request-Id", "X-Codex-Window-Id",
		"Thread-Id", "Session-Id", "X-OpenAI-Internal-Codex-Responses-Lite",
	} {
		if value := strings.TrimSpace(source.Get(name)); value != "" && !strings.ContainsAny(value, "\r\n") {
			target.Set(name, value)
		}
	}
}

func copyQuotaHeaders(target, source http.Header) {
	for name, values := range source {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-codex-") || strings.HasPrefix(lower, "x-ratelimit-") ||
			lower == "retry-after" || lower == "openai-request-id" {
			for _, value := range values {
				target.Add(name, value)
			}
		}
	}
}

func convertModels(raw []byte) ([]byte, error) {
	var payload struct {
		Models []struct {
			Slug string `json:"slug"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode Codex model catalog: %w", err)
	}
	if payload.Models == nil {
		return nil, fmt.Errorf("Codex model catalog has no models array")
	}
	data := make([]map[string]any, 0, len(payload.Models))
	seen := make(map[string]struct{})
	for _, model := range payload.Models {
		id := firstNonEmpty(model.Slug, model.ID, model.Name)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		data = append(data, map[string]any{"id": id, "object": "model", "created": 0, "owned_by": "openai"})
	}
	return json.Marshal(map[string]any{"object": "list", "data": data})
}

func writeError(w http.ResponseWriter, status int, code, message, requestID string) {
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": code, "code": code}})
	writeJSONBytes(w, status, payload, requestID)
}

func writeUpstreamError(w http.ResponseWriter, resp *http.Response, body []byte, requestID string) {
	w.Header().Set("Content-Type", firstNonEmpty(resp.Header.Get("Content-Type"), "application/json"))
	w.Header().Set("X-Request-Id", requestID)
	copyQuotaHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func writeJSONBytes(w http.ResponseWriter, status int, payload []byte, requestID string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Request-Id", requestID)
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func compactError(body []byte) string {
	return trimText(strings.TrimSpace(string(body)), 500)
}

func trimText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func newID(prefix string) string {
	value, err := security.RandomToken(16)
	if err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + value
}
