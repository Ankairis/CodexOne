package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Ankairis/CodexOne/internal/codex"
	"github.com/Ankairis/CodexOne/internal/store"
	"github.com/gorilla/websocket"
)

const (
	codexResponsesWebSocketBeta   = "responses_websockets=2026-02-06"
	webSocketHandshakeTimeout     = 30 * time.Second
	webSocketFirstMessageTimeout  = 30 * time.Second
	webSocketClientIdleTimeout    = 10 * time.Minute
	webSocketUpstreamIdleTimeout  = 5 * time.Minute
	webSocketPingInterval         = 30 * time.Second
	webSocketWriteTimeout         = 30 * time.Second
	webSocketFallbackRetryDelay   = 20 * time.Second
	defaultWebSocketReadLimit     = 32 << 20
	upstreamWebSocketReadLimit    = 64 << 20
	webSocketClientQueueSize      = 16
	webSocketUpstreamEventBacklog = 256
)

var responsesWebSocketUpgrader = websocket.Upgrader{
	ReadBufferSize:    32 << 10,
	WriteBufferSize:   32 << 10,
	EnableCompression: true,
	CheckOrigin: func(*http.Request) bool {
		// The same API-key middleware as HTTP Responses authenticates this route.
		return true
	},
}

// ResponsesWebSocket runs a serialized Responses turn state machine over one
// downstream WebSocket. It reuses a healthy upstream socket, reconstructs a
// complete transcript after an idle disconnect, and can safely fall back to
// HTTP/SSE when the WebSocket handshake fails before a request is submitted.
func (s *Service) ResponsesWebSocket(w http.ResponseWriter, r *http.Request) {
	if !s.beginRequest(w) {
		return
	}
	defer s.endRequest()

	connectionRequestID := newID("req")
	if !websocket.IsWebSocketUpgrade(r) {
		started := time.Now()
		w.Header().Set("Upgrade", "websocket")
		writeError(w, http.StatusUpgradeRequired, "websocket_upgrade_required", "WebSocket upgrade required", connectionRequestID)
		s.record(r, connectionRequestID, "", http.StatusUpgradeRequired, started, 0, 0, requestTelemetry{}, "WebSocket upgrade required")
		return
	}

	downstreamConn, err := responsesWebSocketUpgrader.Upgrade(w, r, http.Header{"X-Request-Id": {connectionRequestID}})
	if err != nil {
		return
	}
	if !s.websockets.add(downstreamConn) {
		_ = downstreamConn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "CodexOne is shutting down"), time.Now().Add(time.Second))
		_ = downstreamConn.Close()
		return
	}
	defer func() {
		s.websockets.remove(downstreamConn)
		_ = downstreamConn.Close()
	}()

	readLimit := s.cfg.MaxRequestBytes
	if readLimit <= 0 {
		readLimit = defaultWebSocketReadLimit
	}
	downstream := newWebSocketWriter(downstreamConn)
	inbox := newDownstreamWebSocketInbox(downstreamConn, readLimit)
	sessionCtx, cancelSession := context.WithCancel(r.Context())
	defer cancelSession()
	go func() {
		select {
		case <-inbox.done:
			cancelSession()
		case <-sessionCtx.Done():
		}
	}()
	go downstream.pingLoop(sessionCtx, webSocketPingInterval)

	session := &responsesWebSocketSession{
		service:    s,
		request:    r,
		downstream: downstream,
		inbox:      inbox,
		conversation: webSocketConversation{
			maxReplayBytes: readLimit,
		},
	}
	defer session.closeUpstream()

	for {
		select {
		case <-sessionCtx.Done():
			return
		case message := <-inbox.messages:
			inbox.release(message)
			result := session.handleTurn(sessionCtx, message.payload)
			s.record(r, result.requestID, result.model, result.status, result.started,
				result.usage.InputTokens, result.usage.OutputTokens, result.telemetry, result.errText)
			if result.fatal {
				if result.closeCode == 0 {
					result.closeCode = websocket.CloseServiceRestart
				}
				downstream.close(result.closeCode, result.closeReason)
				return
			}
		case <-inbox.done:
			return
		}
	}
}

type webSocketTurnResult struct {
	requestID   string
	started     time.Time
	model       string
	status      int
	usage       tokenUsage
	telemetry   requestTelemetry
	errText     string
	fatal       bool
	closeCode   int
	closeReason string
}

type responsesWebSocketSession struct {
	service    *Service
	request    *http.Request
	downstream *webSocketWriter
	inbox      *downstreamWebSocketInbox

	conversation     webSocketConversation
	upstream         *upstreamWebSocket
	nextGeneration   uint64
	webSocketRetryAt time.Time
}

func (s *responsesWebSocketSession) handleTurn(ctx context.Context, raw []byte) webSocketTurnResult {
	result := webSocketTurnResult{
		requestID: newID("req"),
		started:   time.Now(),
		status:    http.StatusOK,
	}
	if !s.authorizeTurn(ctx, &result) {
		return result
	}
	credential, err := s.service.auth.FreshCredential(ctx)
	if err != nil {
		s.closeUpstream()
		result.status, result.errText = http.StatusServiceUnavailable, err.Error()
		_ = s.downstream.writeError("account_unavailable", accountUnavailableClientMessage, result.requestID, result.status)
		return result
	}
	plan, err := s.conversation.prepare(s.request, raw, s.service.cfg.CodexIdentityRemap, credential.PlanType)
	if err != nil {
		result.status, result.errText = http.StatusBadRequest, err.Error()
		_ = s.downstream.writeError("invalid_request", result.errText, result.requestID, result.status)
		return result
	}
	result.model = plan.model
	result.telemetry.ReasoningEffort = plan.reasoning

	upstream, dialFailure := s.ensureUpstream(ctx, plan.identity, credential)
	if upstream == nil {
		if s.service.cfg.CodexWSHTTPFallback {
			return s.executeHTTPFallback(ctx, plan, result, dialFailure, credential)
		}
		result.status = firstPositive(dialFailure.status, http.StatusBadGateway)
		result.errText = firstNonEmpty(dialFailure.message, "upstream WebSocket is unavailable")
		_ = s.downstream.writeError("upstream_unavailable", result.errText, result.requestID, result.status)
		return result
	}

	body := s.conversation.bodyFor(plan, upstream.generation, false)
	if err = upstream.write(body); err != nil {
		s.invalidateUpstream()
		result.status = http.StatusBadGateway
		result.errText = "upstream WebSocket disconnected while submitting the request; delivery outcome is unknown"
		result.fatal = true
		result.closeCode = websocket.CloseServiceRestart
		result.closeReason = "upstream outcome unknown; reconnect and replay the full turn"
		_ = s.downstream.writeError("upstream_outcome_unknown", result.errText, result.requestID, result.status)
		return result
	}
	return s.forwardWebSocketTurn(ctx, upstream, plan, result)
}

func (s *responsesWebSocketSession) authorizeTurn(ctx context.Context, result *webSocketTurnResult) bool {
	key, ok := APIKeyFromContext(s.request.Context())
	if !ok || strings.TrimSpace(key.ID) == "" {
		result.status = http.StatusUnauthorized
		result.errText = "API key is missing from the WebSocket session"
		result.fatal = true
		result.closeCode = websocket.ClosePolicyViolation
		result.closeReason = "API key is invalid or revoked"
		_ = s.downstream.writeError("invalid_api_key", result.closeReason, result.requestID, result.status)
		return false
	}
	active, err := s.service.store.FindActiveAPIKeyByID(ctx, key.ID)
	if err != nil {
		if store.IsNotFound(err) {
			result.status = http.StatusUnauthorized
			result.errText = "API key is invalid or revoked"
			result.fatal = true
			result.closeCode = websocket.ClosePolicyViolation
			result.closeReason = result.errText
			_ = s.downstream.writeError("invalid_api_key", result.errText, result.requestID, result.status)
			return false
		}
		result.status = http.StatusServiceUnavailable
		result.errText = err.Error()
		_ = s.downstream.writeError("database_unavailable", "API key storage is unavailable", result.requestID, result.status)
		return false
	}
	_ = s.service.store.TouchAPIKey(ctx, active.ID, time.Now().UnixMilli())
	return true
}

func (s *responsesWebSocketSession) forwardWebSocketTurn(ctx context.Context, upstream *upstreamWebSocket, plan webSocketTurnPlan, result webSocketTurnResult) webSocketTurnResult {
	identity := s.conversation.identityFor(plan)
	for {
		select {
		case <-ctx.Done():
			result.status, result.errText = 499, "client disconnected"
			return result
		case <-s.inbox.done:
			result.status, result.errText = 499, "client disconnected"
			return result
		case frame, ok := <-upstream.events:
			if !ok {
				err := upstream.readError()
				result.status = http.StatusBadGateway
				result.errText = firstNonEmpty(errorText(err), "upstream WebSocket closed before a terminal response")
				result.fatal = true
				result.closeCode, result.closeReason = webSocketCloseForUpstreamError(err)
				if result.closeCode == websocket.CloseMessageTooBig {
					_ = s.downstream.writeError("message_too_big", "upstream WebSocket message is too large", result.requestID, http.StatusRequestEntityTooLarge)
					result.status = http.StatusRequestEntityTooLarge
				} else {
					_ = s.downstream.writeError("upstream_disconnected", result.errText, result.requestID, result.status)
				}
				s.invalidateUpstream()
				return result
			}
			upstream.release(frame)
			payload := normalizeWebSocketTerminal(frame.payload)
			observeResponsePayload(payload, &result.usage, &result.telemetry, result.started)
			if err := s.downstream.write(identity.exposePayload(payload)); err != nil {
				result.status, result.errText = 499, "client disconnected"
				result.fatal = true
				result.closeCode = websocket.CloseGoingAway
				return result
			}
			switch webSocketEventType(payload) {
			case "response.completed", "response.incomplete":
				if err := s.conversation.commit(plan, payload, upstream.generation); err != nil {
					result.status, result.errText = http.StatusBadGateway, err.Error()
					result.fatal = true
					_ = s.downstream.writeError("invalid_upstream_response", result.errText, result.requestID, result.status)
				}
				result.telemetry.mergeResponse(requestTelemetry{}, result.usage)
				return result
			case "response.failed", "error":
				failure := parseUpstreamResponseError(payload)
				result.status, result.errText = failure.HTTPStatus(), failure.Error()
				result.telemetry.mergeResponse(requestTelemetry{}, result.usage)
				s.invalidateUpstream()
				return result
			}
		}
	}
}

func (s *responsesWebSocketSession) executeHTTPFallback(ctx context.Context, plan webSocketTurnPlan, result webSocketTurnResult, dialFailure upstreamDialFailure, credential codex.Credential) webSocketTurnResult {
	body, err := removeWebSocketType(s.conversation.bodyFor(plan, 0, true))
	if err != nil {
		result.status, result.errText = http.StatusInternalServerError, err.Error()
		_ = s.downstream.writeError("request_failed", result.errText, result.requestID, result.status)
		return result
	}
	body, err = prepareCodexHTTPBody(body, plan.model, credential.PlanType, s.request.Header, plan.identity)
	if err != nil {
		result.status, result.errText = http.StatusInternalServerError, err.Error()
		_ = s.downstream.writeError("request_failed", result.errText, result.requestID, result.status)
		return result
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.service.cfg.UpstreamBaseURL+"/backend-api/codex/responses", bytes.NewReader(body))
	if err != nil {
		result.status, result.errText = http.StatusInternalServerError, err.Error()
		_ = s.downstream.writeError("request_failed", result.errText, result.requestID, result.status)
		return result
	}
	req.Header.Set("Content-Type", "application/json")
	copyCodexContextHeaders(req.Header, s.request.Header)
	if err = plan.identity.applyHeaders(req.Header, s.request.Header, false); err != nil {
		result.status, result.errText = http.StatusBadRequest, err.Error()
		_ = s.downstream.writeError("invalid_request", result.errText, result.requestID, result.status)
		return result
	}
	s.service.auth.ApplyCodexHeaders(req, credential, "text/event-stream")
	response, err := s.service.client.Do(req)
	if err != nil {
		result.status, result.errText = http.StatusBadGateway, err.Error()
		if dialFailure.message != "" {
			result.errText = "WebSocket unavailable (" + dialFailure.message + "); HTTP fallback failed: " + result.errText
		}
		_ = s.downstream.writeError("upstream_unavailable", result.errText, result.requestID, result.status)
		return result
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		rawError, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		rawError = plan.identity.exposePayload(rawError)
		result.status, result.errText = response.StatusCode, compactError(rawError)
		_ = s.downstream.writeError("upstream_rejected", firstNonEmpty(result.errText, http.StatusText(result.status)), result.requestID, result.status)
		return result
	}

	reader := bufio.NewReader(response.Body)
	for {
		payload, readErr := readSSEData(reader, upstreamWebSocketReadLimit)
		if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
			payload = normalizeWebSocketTerminal(payload)
			observeResponsePayload(payload, &result.usage, &result.telemetry, result.started)
			if err = s.downstream.write(plan.identity.exposePayload(payload)); err != nil {
				result.status, result.errText = 499, "client disconnected"
				return result
			}
			switch webSocketEventType(payload) {
			case "response.completed", "response.incomplete":
				if err = s.conversation.commit(plan, payload, 0); err != nil {
					result.status, result.errText = http.StatusBadGateway, err.Error()
					_ = s.downstream.writeError("invalid_upstream_response", result.errText, result.requestID, result.status)
				}
				result.telemetry.mergeResponse(requestTelemetry{}, result.usage)
				return result
			case "response.failed", "error":
				failure := parseUpstreamResponseError(payload)
				result.status, result.errText = failure.HTTPStatus(), failure.Error()
				result.telemetry.mergeResponse(requestTelemetry{}, result.usage)
				return result
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				result.status, result.errText = http.StatusBadGateway, "HTTP fallback stream ended before a terminal response"
			} else {
				result.status, result.errText = http.StatusBadGateway, readErr.Error()
			}
			_ = s.downstream.writeError("upstream_stream_failed", result.errText, result.requestID, result.status)
			return result
		}
	}
}

func (s *responsesWebSocketSession) ensureUpstream(ctx context.Context, identity *codexIdentity, credential codex.Credential) (*upstreamWebSocket, upstreamDialFailure) {
	if s.upstream != nil && s.upstream.accountID == credential.AccountID && s.upstream.alive() {
		return s.upstream, upstreamDialFailure{}
	}
	s.closeUpstream()
	if s.service.cfg.CodexWSHTTPFallback && time.Now().Before(s.webSocketRetryAt) {
		return nil, upstreamDialFailure{message: "WebSocket retry cooldown is active"}
	}

	upstreamURL, err := codexWebSocketURL(s.service.cfg.UpstreamBaseURL + "/backend-api/codex/responses")
	if err != nil {
		return nil, upstreamDialFailure{status: http.StatusInternalServerError, message: err.Error()}
	}
	headers := make(http.Header)
	copyCodexContextHeaders(headers, s.request.Header)
	if err = identity.applyHeaders(headers, s.request.Header, true); err != nil {
		return nil, upstreamDialFailure{status: http.StatusBadRequest, message: err.Error()}
	}
	headerRequest := &http.Request{Header: headers}
	s.service.auth.ApplyCodexHeaders(headerRequest, credential, "application/json")
	headers.Set("OpenAI-Beta", mergeWebSocketBetaHeader(s.request.Header.Get("OpenAI-Beta")))

	dialer := websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  webSocketHandshakeTimeout,
		EnableCompression: true,
	}
	connection, handshake, err := dialer.DialContext(ctx, upstreamURL, headers)
	if err != nil {
		failure := upstreamDialFailure{status: http.StatusBadGateway, message: err.Error()}
		if handshake != nil {
			failure.status = handshake.StatusCode
			if handshake.Body != nil {
				raw, _ := io.ReadAll(io.LimitReader(handshake.Body, 2<<20))
				_ = handshake.Body.Close()
				if text := compactError(raw); text != "" {
					failure.message = text
				}
			}
		}
		s.webSocketRetryAt = time.Now().Add(webSocketFallbackRetryDelay)
		return nil, failure
	}
	s.nextGeneration++
	s.upstream = newUpstreamWebSocket(connection, s.nextGeneration, credential.AccountID)
	s.webSocketRetryAt = time.Time{}
	return s.upstream, upstreamDialFailure{}
}

func (s *responsesWebSocketSession) invalidateUpstream() {
	if s.upstream == nil {
		return
	}
	s.upstream.close()
	s.upstream = nil
}

func (s *responsesWebSocketSession) closeUpstream() {
	if s.upstream != nil {
		s.upstream.close()
		s.upstream = nil
	}
}

type upstreamDialFailure struct {
	status  int
	message string
}

type upstreamWebSocketFrame struct {
	payload []byte
}

type upstreamWebSocket struct {
	conn           *websocket.Conn
	generation     uint64
	accountID      string
	events         chan upstreamWebSocketFrame
	done           chan struct{}
	stop           chan struct{}
	maxQueuedBytes int64
	queuedBytes    atomic.Int64
	writeMu        sync.Mutex
	closeOnce      sync.Once
	errMu          sync.RWMutex
	err            error
}

func newUpstreamWebSocket(connection *websocket.Conn, generation uint64, accountID string) *upstreamWebSocket {
	socket := &upstreamWebSocket{
		conn:           connection,
		generation:     generation,
		accountID:      accountID,
		events:         make(chan upstreamWebSocketFrame, webSocketUpstreamEventBacklog),
		done:           make(chan struct{}),
		stop:           make(chan struct{}),
		maxQueuedBytes: upstreamWebSocketReadLimit,
	}
	connection.SetReadLimit(upstreamWebSocketReadLimit)
	connection.EnableWriteCompression(false)
	_ = connection.SetReadDeadline(time.Now().Add(webSocketUpstreamIdleTimeout))
	connection.SetPongHandler(func(string) error {
		return connection.SetReadDeadline(time.Now().Add(webSocketUpstreamIdleTimeout))
	})
	go socket.readLoop()
	go socket.pingLoop()
	return socket
}

func (s *upstreamWebSocket) readLoop() {
	defer func() {
		_ = s.conn.Close()
		close(s.events)
		close(s.done)
	}()
	for {
		messageType, payload, err := s.conn.ReadMessage()
		if err != nil {
			s.setError(err)
			return
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(webSocketUpstreamIdleTimeout))
		if messageType != websocket.TextMessage {
			if messageType == websocket.BinaryMessage {
				s.setError(fmt.Errorf("upstream WebSocket sent an unexpected binary message"))
				return
			}
			continue
		}
		frame := upstreamWebSocketFrame{payload: payload}
		if !s.reserve(frame) {
			s.setError(fmt.Errorf("queued upstream WebSocket events exceed %d bytes", s.maxQueuedBytes))
			return
		}
		select {
		case s.events <- frame:
		case <-s.stop:
			s.release(frame)
			return
		}
	}
}

func (s *upstreamWebSocket) reserve(frame upstreamWebSocketFrame) bool {
	if s == nil || s.maxQueuedBytes <= 0 {
		return false
	}
	size := int64(len(frame.payload))
	if size > s.maxQueuedBytes {
		return false
	}
	if total := s.queuedBytes.Add(size); total > s.maxQueuedBytes {
		s.queuedBytes.Add(-size)
		return false
	}
	return true
}

func (s *upstreamWebSocket) release(frame upstreamWebSocketFrame) {
	if s == nil {
		return
	}
	s.queuedBytes.Add(-int64(len(frame.payload)))
}

func (s *upstreamWebSocket) pingLoop() {
	ticker := time.NewTicker(webSocketPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-s.done:
			return
		case <-ticker.C:
			if err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(webSocketWriteTimeout)); err != nil {
				s.setError(err)
				s.close()
				return
			}
		}
	}
}

func (s *upstreamWebSocket) write(payload []byte) error {
	if !s.alive() {
		return firstError(s.readError(), errors.New("upstream WebSocket is closed"))
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(webSocketWriteTimeout))
	err := s.conn.WriteMessage(websocket.TextMessage, payload)
	_ = s.conn.SetWriteDeadline(time.Time{})
	return err
}

func (s *upstreamWebSocket) alive() bool {
	if s == nil {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

func (s *upstreamWebSocket) close() {
	if s == nil || s.conn == nil {
		return
	}
	s.closeOnce.Do(func() {
		close(s.stop)
		_ = s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = s.conn.Close()
	})
}

func (s *upstreamWebSocket) setError(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
}

func (s *upstreamWebSocket) readError() error {
	if s == nil {
		return nil
	}
	s.errMu.RLock()
	defer s.errMu.RUnlock()
	return s.err
}

type downstreamWebSocketMessage struct {
	payload []byte
}

type downstreamWebSocketInbox struct {
	messages       chan downstreamWebSocketMessage
	done           chan struct{}
	maxQueuedBytes int64
	queuedBytes    atomic.Int64
	errMu          sync.RWMutex
	err            error
}

func newDownstreamWebSocketInbox(connection *websocket.Conn, readLimit int64) *downstreamWebSocketInbox {
	inbox := &downstreamWebSocketInbox{
		messages:       make(chan downstreamWebSocketMessage, webSocketClientQueueSize),
		done:           make(chan struct{}),
		maxQueuedBytes: readLimit,
	}
	connection.SetReadLimit(readLimit)
	_ = connection.SetReadDeadline(time.Now().Add(webSocketFirstMessageTimeout))
	connection.SetPongHandler(func(string) error {
		return connection.SetReadDeadline(time.Now().Add(webSocketClientIdleTimeout))
	})
	go func() {
		defer close(inbox.done)
		first := true
		for {
			messageType, payload, err := connection.ReadMessage()
			if err != nil {
				inbox.setError(err)
				return
			}
			if first {
				first = false
			}
			_ = connection.SetReadDeadline(time.Now().Add(webSocketClientIdleTimeout))
			if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
				continue
			}
			message := downstreamWebSocketMessage{payload: payload}
			if !inbox.reserve(message) {
				inbox.setError(fmt.Errorf("queued WebSocket requests exceed %d bytes", inbox.maxQueuedBytes))
				_ = connection.Close()
				return
			}
			select {
			case inbox.messages <- message:
			default:
				inbox.release(message)
				inbox.setError(fmt.Errorf("too many queued WebSocket requests"))
				_ = connection.Close()
				return
			}
		}
	}()
	return inbox
}

func (i *downstreamWebSocketInbox) reserve(message downstreamWebSocketMessage) bool {
	if i == nil || i.maxQueuedBytes <= 0 {
		return false
	}
	size := int64(len(message.payload))
	if size > i.maxQueuedBytes {
		return false
	}
	if total := i.queuedBytes.Add(size); total > i.maxQueuedBytes {
		i.queuedBytes.Add(-size)
		return false
	}
	return true
}

func (i *downstreamWebSocketInbox) release(message downstreamWebSocketMessage) {
	if i == nil {
		return
	}
	i.queuedBytes.Add(-int64(len(message.payload)))
}

func (i *downstreamWebSocketInbox) setError(err error) {
	i.errMu.Lock()
	if i.err == nil {
		i.err = err
	}
	i.errMu.Unlock()
}

type webSocketWriter struct {
	conn      *websocket.Conn
	writeMu   sync.Mutex
	closeOnce sync.Once
}

func newWebSocketWriter(connection *websocket.Conn) *webSocketWriter {
	return &webSocketWriter{conn: connection}
}

func (w *webSocketWriter) write(payload []byte) error {
	if w == nil || w.conn == nil {
		return errors.New("WebSocket is closed")
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(webSocketWriteTimeout))
	err := w.conn.WriteMessage(websocket.TextMessage, payload)
	_ = w.conn.SetWriteDeadline(time.Time{})
	return err
}

func (w *webSocketWriter) writeError(code, message, requestID string, status int) error {
	payload, _ := json.Marshal(map[string]any{
		"type":       "error",
		"status":     status,
		"request_id": requestID,
		"error": map[string]string{
			"type":    "invalid_request_error",
			"code":    code,
			"message": trimText(message, 1000),
		},
	})
	return w.write(payload)
}

func (w *webSocketWriter) pingLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if w == nil || w.conn == nil {
				return
			}
			if err := w.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(webSocketWriteTimeout)); err != nil {
				_ = w.conn.Close()
				return
			}
		}
	}
}

func (w *webSocketWriter) close(code int, reason string) {
	if w == nil || w.conn == nil {
		return
	}
	w.closeOnce.Do(func() {
		reason = truncateWebSocketCloseReason(reason, 123)
		_ = w.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
		_ = w.conn.Close()
	})
}

func codexWebSocketURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse Codex WebSocket URL: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported Codex WebSocket URL scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("Codex WebSocket URL host is empty")
	}
	return parsed.String(), nil
}

func mergeWebSocketBetaHeader(clientValue string) string {
	values := make([]string, 0, 4)
	found := false
	for _, value := range strings.Split(clientValue, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(value), "responses_websockets=") {
			if !found {
				values = append(values, codexResponsesWebSocketBeta)
				found = true
			}
			continue
		}
		values = append(values, value)
	}
	if !found {
		values = append(values, codexResponsesWebSocketBeta)
	}
	return strings.Join(values, ",")
}

func normalizeWebSocketTerminal(payload []byte) []byte {
	var event map[string]any
	if json.Unmarshal(payload, &event) == nil && jsonString(event["type"]) == "response.done" {
		event["type"] = "response.completed"
		if encoded, err := json.Marshal(event); err == nil {
			return encoded
		}
	}
	return payload
}

func webSocketEventType(payload []byte) string {
	var event struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(payload, &event)
	return strings.TrimSpace(event.Type)
}

func readSSEData(reader *bufio.Reader, maxBytes int64) ([]byte, error) {
	var data []byte
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			if len(line) == 0 {
				if len(data) > 0 {
					return data, nil
				}
			} else if bytes.HasPrefix(line, []byte("data:")) {
				part := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, part...)
				if int64(len(data)) > maxBytes {
					return nil, fmt.Errorf("SSE event exceeds %d bytes", maxBytes)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(data) > 0 {
				return data, nil
			}
			return nil, err
		}
	}
}

func webSocketCloseForUpstreamError(err error) (int, string) {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		if closeErr.Code == websocket.CloseMessageTooBig {
			return websocket.CloseMessageTooBig, firstNonEmpty(closeErr.Text, "upstream message too big")
		}
	}
	return websocket.CloseServiceRestart, "upstream disconnected; reconnect and replay the full turn"
}

func truncateWebSocketCloseReason(reason string, maxBytes int) string {
	reason = strings.TrimSpace(reason)
	if len(reason) <= maxBytes {
		return reason
	}
	for maxBytes > 0 && !utf8.ValidString(reason[:maxBytes]) {
		maxBytes--
	}
	return reason[:maxBytes]
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func firstError(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

type websocketRegistry struct {
	mu          sync.Mutex
	connections map[*websocket.Conn]struct{}
	closed      bool
}

func (r *websocketRegistry) add(connection *websocket.Conn) bool {
	if connection == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	if r.connections == nil {
		r.connections = make(map[*websocket.Conn]struct{})
	}
	r.connections[connection] = struct{}{}
	return true
}

func (r *websocketRegistry) remove(connection *websocket.Conn) {
	r.mu.Lock()
	delete(r.connections, connection)
	r.mu.Unlock()
}

func (r *websocketRegistry) closeAll() {
	r.mu.Lock()
	r.closed = true
	connections := make([]*websocket.Conn, 0, len(r.connections))
	for connection := range r.connections {
		connections = append(connections, connection)
	}
	r.mu.Unlock()
	for _, connection := range connections {
		_ = connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "CodexOne is shutting down"), time.Now().Add(time.Second))
		_ = connection.Close()
	}
}
