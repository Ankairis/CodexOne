package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	promptCacheNamespace    = "codexone:prompt-cache:"
	maxIdentityBytes        = 1024
	maxIdentityMappings     = 256
	maxIdentityMappingBytes = 256 << 10
)

// codexIdentity owns one downstream conversation's upstream identity. When
// remapping is enabled, every source identifier keeps its own deterministic
// one-to-one pseudonym. Unlike account-wide identity convergence, separate
// sessions, installations, windows, threads, requests, and turns never collapse
// onto a shared value.
type codexIdentity struct {
	remap             bool
	keyID             string
	clientSessionID   string
	upstreamSessionID string

	mu           sync.RWMutex
	reverse      map[string]string
	mappingBytes int
	mappingErr   error
	matcher      *regexp.Regexp
}

// prepareRequestIdentity makes the prompt cache key and transport session
// headers agree. A caller-provided prompt_cache_key wins, followed by session
// and Codex context headers, then a stable value derived from the local API key.
func prepareRequestIdentity(ctx context.Context, clientHeaders http.Header, body []byte, remap bool) ([]byte, *codexIdentity, error) {
	identity, err := newCodexIdentity(ctx, clientHeaders, body, remap)
	if err != nil {
		return nil, nil, err
	}
	body, err = identity.prepareBody(body, false)
	if err != nil {
		return nil, nil, err
	}
	return body, identity, nil
}

func newCodexIdentity(ctx context.Context, clientHeaders http.Header, body []byte, remap bool) (*codexIdentity, error) {
	object, err := decodeJSONObject(body)
	if err != nil {
		return nil, err
	}
	bodySessionID, err := identityStringField(object, "prompt_cache_key")
	if err != nil {
		return nil, err
	}

	keyID := ""
	if key, ok := APIKeyFromContext(ctx); ok {
		keyID = strings.TrimSpace(key.ID)
	}
	clientSessionID := firstNonEmpty(
		bodySessionID,
		firstSessionHeader(clientHeaders),
		promptCacheFromTurnMetadata(clientHeaders.Get("X-Codex-Turn-Metadata")),
		cleanIdentityValue(clientHeaders.Get("Thread-Id")),
		clientMetadataSession(object),
	)
	if clientSessionID != "" && !validIdentityValue(clientSessionID) {
		return nil, fmt.Errorf("session identity is invalid or too long")
	}

	identity := &codexIdentity{
		remap:           remap,
		keyID:           keyID,
		clientSessionID: clientSessionID,
		reverse:         make(map[string]string),
	}
	if clientSessionID != "" {
		identity.upstreamSessionID = identity.mapValue("prompt-cache", clientSessionID)
	} else if keyID != "" {
		identity.upstreamSessionID = stableSessionID(keyID)
	}
	if err := identity.mappingError(); err != nil {
		return nil, err
	}
	return identity, nil
}

// prepareBody applies this conversation identity to another request body. A
// persistent WebSocket cannot safely change its prompt-cache/session identity;
// attempts to do so are rejected instead of silently mixing conversations.
func (i *codexIdentity) prepareBody(body []byte, fixed bool) ([]byte, error) {
	object, err := decodeJSONObject(body)
	if err != nil {
		return nil, err
	}

	explicitSession, err := identityStringField(object, "prompt_cache_key")
	if err != nil {
		return nil, err
	}
	if fixed && explicitSession != "" && explicitSession != i.clientSessionID && explicitSession != i.upstreamSessionID {
		return nil, fmt.Errorf("prompt_cache_key cannot change on an active WebSocket")
	}
	if i.upstreamSessionID != "" {
		object["prompt_cache_key"] = i.upstreamSessionID
	}

	if metadata, ok := object["client_metadata"].(map[string]any); ok {
		i.remapClientMetadata(metadata)
	}
	if err = i.mappingError(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("encode request identity: %w", err)
	}
	return encoded, nil
}

func (i *codexIdentity) remapClientMetadata(metadata map[string]any) {
	if i == nil || !i.remap || metadata == nil {
		return
	}
	for _, field := range []struct {
		name string
		kind string
	}{
		{"x-codex-installation-id", "installation"},
		{"installation_id", "installation"},
		{"x-client-request-id", "request"},
		{"request_id", "request"},
		{"thread_id", "thread"},
		{"x-codex-thread-id", "thread"},
	} {
		if value, ok := metadata[field.name].(string); ok && validIdentityValue(value) {
			metadata[field.name] = i.mapValue(field.kind, value)
		}
	}
	for _, name := range []string{"x-codex-window-id", "window_id"} {
		if value, ok := metadata[name].(string); ok && validIdentityValue(value) {
			metadata[name] = i.mapWindow(value)
		}
	}
	for _, name := range []string{"session_id", "x-codex-session-id", "prompt_cache_key"} {
		if _, exists := metadata[name]; exists && i.upstreamSessionID != "" {
			metadata[name] = i.upstreamSessionID
		}
	}
	for _, name := range []string{"x-codex-turn-metadata", "turn_metadata"} {
		if value, ok := metadata[name].(string); ok {
			metadata[name] = i.remapTurnMetadata(value)
		}
	}
}

func (i *codexIdentity) applyHeaders(target, source http.Header, websocketTransport bool) error {
	if i == nil || target == nil {
		return nil
	}
	if i.upstreamSessionID != "" {
		deleteHeaderFold(target, "Session-Id")
		deleteHeaderFold(target, "Session_id")
		deleteHeaderFold(target, "session_id")
		deleteHeaderFold(target, "Conversation_id")
		if websocketTransport {
			target["session_id"] = []string{i.upstreamSessionID}
			target["Conversation_id"] = []string{i.upstreamSessionID}
		} else {
			target.Set("Session-Id", i.upstreamSessionID)
			if headerValueFold(source, "Conversation_id") != "" {
				target["Conversation_id"] = []string{i.upstreamSessionID}
			}
		}
	}
	if !i.remap || source == nil {
		return i.mappingError()
	}
	for _, field := range []struct {
		name string
		kind string
	}{
		{"X-Client-Request-Id", "request"},
		{"Thread-Id", "thread"},
	} {
		if value := cleanIdentityValue(source.Get(field.name)); value != "" {
			target.Set(field.name, i.mapValue(field.kind, value))
		}
	}
	if value := cleanIdentityValue(source.Get("X-Codex-Window-Id")); value != "" {
		target.Set("X-Codex-Window-Id", i.mapWindow(value))
	}
	if value := strings.TrimSpace(source.Get("X-Codex-Turn-Metadata")); value != "" {
		target.Set("X-Codex-Turn-Metadata", i.remapTurnMetadata(value))
	}
	return i.mappingError()
}

func (i *codexIdentity) remapTurnMetadata(raw string) string {
	if i == nil || !i.remap || strings.TrimSpace(raw) == "" || len(raw) > maxIdentityBytes*8 {
		return raw
	}
	var metadata map[string]any
	if json.Unmarshal([]byte(raw), &metadata) != nil || metadata == nil {
		return raw
	}
	if value, ok := metadata["prompt_cache_key"].(string); ok && value != "" && i.upstreamSessionID != "" {
		metadata["prompt_cache_key"] = i.upstreamSessionID
	}
	if value, ok := metadata["turn_id"].(string); ok && validIdentityValue(value) {
		metadata["turn_id"] = i.mapValue("turn", value)
	}
	if value, ok := metadata["thread_id"].(string); ok && validIdentityValue(value) {
		metadata["thread_id"] = i.mapValue("thread", value)
	}
	if value, ok := metadata["window_id"].(string); ok && validIdentityValue(value) {
		metadata["window_id"] = i.mapWindow(value)
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return raw
	}
	return string(encoded)
}

func (i *codexIdentity) mapWindow(value string) string {
	value = cleanIdentityValue(value)
	if value == "" || i == nil || !i.remap {
		return value
	}
	base, suffix := value, ""
	if index := strings.LastIndex(value, ":"); index > 0 {
		base, suffix = value[:index], value[index:]
	}
	mapped := ""
	if base == i.clientSessionID && i.upstreamSessionID != "" {
		mapped = i.upstreamSessionID
		i.remember(mapped+suffix, value)
	} else {
		mapped = i.mapValue("window", base)
	}
	return mapped + suffix
}

func (i *codexIdentity) mapValue(kind, value string) string {
	value = cleanIdentityValue(value)
	if value == "" || i == nil || !i.remap {
		return value
	}
	mapped := identityUUID(i.keyID, kind, value)
	i.remember(mapped, value)
	return mapped
}

func (i *codexIdentity) remember(upstream, client string) {
	if i == nil || upstream == "" || client == "" || upstream == client {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.mappingErr != nil {
		return
	}
	if i.reverse == nil {
		i.reverse = make(map[string]string)
	}
	if existing, exists := i.reverse[upstream]; exists {
		if existing != client {
			i.mappingErr = fmt.Errorf("identity mapping collision; start a new request")
		}
		return
	}
	addedBytes := len(upstream) + len(client)
	if len(i.reverse) >= maxIdentityMappings || i.mappingBytes+addedBytes > maxIdentityMappingBytes {
		i.mappingErr = fmt.Errorf("identity mapping budget exceeded; reconnect with a compact transcript")
		return
	}
	i.reverse[upstream] = client
	i.mappingBytes += addedBytes
	i.matcher = nil
}

func (i *codexIdentity) mappingError() error {
	if i == nil {
		return nil
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.mappingErr
}

// exposePayload restores identifiers that the client supplied. Replacements
// are UUID-sized upstream pseudonyms, so short user strings can never become
// broad substitutions in generated text.
func (i *codexIdentity) exposePayload(payload []byte) []byte {
	if i == nil || len(payload) == 0 {
		return payload
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.reverse) == 0 {
		return payload
	}
	keys := make([]string, 0, len(i.reverse))
	for key := range i.reverse {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(a, b int) bool { return len(keys[a]) > len(keys[b]) })
	if i.matcher == nil {
		patterns := make([]string, len(keys))
		for index, key := range keys {
			patterns[index] = regexp.QuoteMeta(key)
		}
		matcher, err := regexp.Compile(strings.Join(patterns, "|"))
		if err != nil {
			return payload
		}
		i.matcher = matcher
	}
	first := i.matcher.FindIndex(payload)
	if first == nil {
		return payload
	}
	out := make([]byte, 0, len(payload))
	start := 0
	match := first
	for match != nil {
		matchStart, matchEnd := start+match[0], start+match[1]
		out = append(out, payload[start:matchStart]...)
		client := i.reverse[string(payload[matchStart:matchEnd])]
		encoded, err := json.Marshal(client)
		if err != nil || len(encoded) < 2 {
			out = append(out, payload[matchStart:matchEnd]...)
		} else {
			out = append(out, encoded[1:len(encoded)-1]...)
		}
		start = matchEnd
		match = i.matcher.FindIndex(payload[start:])
	}
	out = append(out, payload[start:]...)
	return out
}

func (i *codexIdentity) sessionID() string {
	if i == nil {
		return ""
	}
	return i.upstreamSessionID
}

func stableSessionID(apiKeyID string) string {
	return identityUUID(strings.TrimSpace(apiKeyID), "default-session", promptCacheNamespace+strings.TrimSpace(apiKeyID))
}

func identityUUID(secret, kind, value string) string {
	key := []byte("codexone:identity:" + strings.TrimSpace(secret))
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(strings.TrimSpace(kind)))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(strings.TrimSpace(value)))
	digest := mac.Sum(nil)
	var raw [16]byte
	copy(raw[:], digest[:16])
	// RFC 9562 UUID layout with deterministic version/variant bits.
	raw[6] = (raw[6] & 0x0f) | 0x50
	raw[8] = (raw[8] & 0x3f) | 0x80
	return uuid.UUID(raw).String()
}

func decodeJSONObject(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	return object, nil
}

func jsonString(value any) string {
	text, _ := value.(string)
	return cleanIdentityValue(text)
}

func identityStringField(object map[string]any, name string) (string, error) {
	value, exists := object[name]
	if !exists || value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}
	if !validIdentityValue(text) {
		return "", fmt.Errorf("%s is invalid or too long", name)
	}
	return text, nil
}

func firstSessionHeader(headers http.Header) string {
	if headers == nil {
		return ""
	}
	for _, name := range []string{"Session-Id", "Session_id", "session_id", "Conversation_id"} {
		if value := cleanIdentityValue(headerValueFold(headers, name)); value != "" {
			return value
		}
	}
	return ""
}

func promptCacheFromTurnMetadata(raw string) string {
	if strings.TrimSpace(raw) == "" || len(raw) > maxIdentityBytes*8 {
		return ""
	}
	var metadata struct {
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	if json.Unmarshal([]byte(raw), &metadata) != nil {
		return ""
	}
	return cleanIdentityValue(metadata.PromptCacheKey)
}

func clientMetadataSession(object map[string]any) string {
	metadata, _ := object["client_metadata"].(map[string]any)
	for _, name := range []string{"session_id", "x-codex-session-id", "prompt_cache_key", "thread_id", "x-codex-window-id"} {
		if value := jsonString(metadata[name]); value != "" {
			return strings.TrimSuffix(value, ":0")
		}
	}
	return ""
}

func validIdentityValue(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= maxIdentityBytes && utf8.ValidString(value) && !strings.ContainsAny(value, "\r\n\x00")
}

func cleanIdentityValue(value string) string {
	value = strings.TrimSpace(value)
	if !validIdentityValue(value) {
		return ""
	}
	return value
}

func headerValueFold(headers http.Header, name string) string {
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if clean := cleanIdentityValue(value); clean != "" {
				return clean
			}
		}
	}
	return ""
}

func deleteHeaderFold(headers http.Header, name string) {
	for key := range headers {
		if strings.EqualFold(key, name) {
			delete(headers, key)
		}
	}
}
