package proxy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	codexInputItemIDLimit          = 64
	maxReasoningEncryptedBytes     = 32 << 20
	codexResponsesLiteHeader       = "X-OpenAI-Internal-Codex-Responses-Lite"
	codexResponsesLiteMetadataPath = "ws_request_header_x_openai_internal_codex_responses_lite"
)

func prepareCodexHTTPBody(body []byte, model, planType string, headers http.Header, identities ...*codexIdentity) ([]byte, error) {
	return prepareCodexHTTPBodyWithImageTool(body, model, planType, headers, true, identities...)
}

func prepareCodexChatBody(body []byte, model, planType string, headers http.Header, identities ...*codexIdentity) ([]byte, error) {
	return prepareCodexHTTPBodyWithImageTool(body, model, planType, headers, false, identities...)
}

func prepareCodexHTTPBodyWithImageTool(body []byte, model, planType string, headers http.Header, includeImageTool bool, identities ...*codexIdentity) ([]byte, error) {
	object, err := decodeJSONObject(body)
	if err != nil {
		return nil, err
	}
	for _, field := range []string{"previous_response_id", "generate", "safety_identifier", "stream_options"} {
		delete(object, field)
	}
	if err = prepareCodexInferenceObject(object, model, planType, headers, false, includeImageTool, identities...); err != nil {
		return nil, err
	}
	return encodeCodexObject(object)
}

func prepareCodexWebSocketBody(body []byte, model, planType string, headers http.Header, identities ...*codexIdentity) ([]byte, error) {
	object, err := decodeJSONObject(body)
	if err != nil {
		return nil, err
	}
	delete(object, "safety_identifier")
	if err = prepareCodexInferenceObject(object, model, planType, headers, true, true, identities...); err != nil {
		return nil, err
	}
	return encodeCodexObject(object)
}

func prepareCodexInferenceObject(object map[string]any, model, planType string, headers http.Header, websocketTransport, includeImageTool bool, identities ...*codexIdentity) error {
	if includeImageTool {
		ensureCodexImageGenerationTool(object, model, planType, headers)
	}
	var identity *codexIdentity
	if len(identities) > 0 {
		identity = identities[0]
	}
	if err := sanitizeCodexInputItems(object, identity); err != nil {
		return err
	}
	normalizeCodexParallelToolCalls(object, headers, websocketTransport)
	return nil
}

func encodeCodexObject(object map[string]any) ([]byte, error) {
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("encode Codex upstream request: %w", err)
	}
	return encoded, nil
}

func ensureCodexImageGenerationTool(object map[string]any, model, planType string, headers http.Header) {
	if object == nil || isCodexResponsesLiteRequest(object, headers) ||
		strings.HasSuffix(strings.ToLower(strings.TrimSpace(model)), "spark") ||
		strings.EqualFold(strings.TrimSpace(planType), "free") {
		return
	}

	tools, exists := object["tools"]
	if !exists {
		object["tools"] = []any{codexImageGenerationTool()}
		return
	}
	items, ok := tools.([]any)
	if !ok {
		object["tools"] = []any{codexImageGenerationTool()}
		return
	}
	for _, value := range items {
		tool, _ := value.(map[string]any)
		if codexToolIncludesImageGeneration(tool) {
			return
		}
	}
	object["tools"] = append(items, codexImageGenerationTool())
}

func codexImageGenerationTool() map[string]any {
	return map[string]any{"type": "image_generation", "output_format": "png"}
}

func codexToolIncludesImageGeneration(tool map[string]any) bool {
	if tool == nil {
		return false
	}
	kind, _ := tool["type"].(string)
	if kind == "image_generation" {
		return true
	}
	if kind == "function" {
		name, _ := tool["name"].(string)
		return name == "image_gen.imagegen"
	}
	if kind != "namespace" {
		return false
	}
	name, _ := tool["name"].(string)
	if name != "image_gen" {
		return false
	}
	nested, _ := tool["tools"].([]any)
	for _, value := range nested {
		function, _ := value.(map[string]any)
		functionType, _ := function["type"].(string)
		functionName, _ := function["name"].(string)
		if functionType == "function" && functionName == "imagegen" {
			return true
		}
	}
	return false
}

func isCodexResponsesLiteRequest(object map[string]any, headers http.Header) bool {
	if strings.EqualFold(strings.TrimSpace(headerValueFold(headers, codexResponsesLiteHeader)), "true") {
		return true
	}
	metadata, _ := object["client_metadata"].(map[string]any)
	value, exists := metadata[codexResponsesLiteMetadataPath]
	if !exists {
		return false
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

func normalizeCodexParallelToolCalls(object map[string]any, headers http.Header, websocketTransport bool) {
	if object == nil {
		return
	}
	if isCodexResponsesLiteRequest(object, headers) {
		object["parallel_tool_calls"] = false
		return
	}
	if websocketTransport {
		return
	}
	tools, _ := object["tools"].([]any)
	if len(tools) == 0 {
		delete(object, "parallel_tool_calls")
	}
}

func sanitizeCodexInputItems(object map[string]any, identities ...*codexIdentity) error {
	items, ok := object["input"].([]any)
	if !ok {
		return nil
	}
	var identity *codexIdentity
	if len(identities) > 0 {
		identity = identities[0]
	}
	store, _ := object["store"].(bool)
	reserved := make(map[string]struct{}, len(items))
	for _, value := range items {
		item, _ := value.(map[string]any)
		itemID, _ := item["id"].(string)
		if itemID != "" && len([]rune(itemID)) <= codexInputItemIDLimit && canonicalCodexInputItemID(item, itemID) == itemID {
			reserved[itemID] = struct{}{}
		}
	}

	sanitized := make([]any, 0, len(items))
	used := make(map[string]struct{}, len(items))
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok {
			sanitized = append(sanitized, value)
			continue
		}
		kind, _ := item["type"].(string)
		if kind == "reasoning" {
			if !sanitizeCodexReasoningItem(item, store) {
				continue
			}
		}
		itemID, _ := item["id"].(string)
		if itemID != "" {
			normalized := canonicalCodexInputItemID(item, itemID)
			if identity != nil && identity.remap && (normalized != itemID || len([]rune(normalized)) > codexInputItemIDLimit) {
				normalized = mappedCodexInputItemID(identity, item, itemID)
			}
			if _, collides := reserved[normalized]; collides && normalized != itemID {
				normalized = uniqueCodexInputItemID(normalized, reserved, used)
			}
			if len([]rune(normalized)) > codexInputItemIDLimit {
				normalized = uniqueCodexInputItemID(normalized, reserved, used)
			}
			if normalized != itemID {
				item["id"] = normalized
				if identity != nil {
					identity.remember(normalized, itemID)
				}
			}
			used[normalized] = struct{}{}
		}
		sanitized = append(sanitized, item)
	}
	object["input"] = sanitized
	if identity != nil {
		return identity.mappingError()
	}
	return nil
}

func mappedCodexInputItemID(identity *codexIdentity, item map[string]any, itemID string) string {
	prefix := codexInputItemPrefix(item)
	if prefix == "" {
		prefix = "item"
	}
	return prefix + "_" + identityUUID(identity.keyID, "input-item:"+prefix, itemID)
}

func sanitizeCodexReasoningItem(item map[string]any, store bool) bool {
	value, exists := item["encrypted_content"]
	if !exists {
		if !store {
			delete(item, "id")
		}
		return true
	}
	encrypted, ok := value.(string)
	if !ok || !validCodexReasoningEncryptedContent(encrypted) {
		delete(item, "encrypted_content")
		if !store {
			delete(item, "id")
		}
		return true
	}
	itemID, _ := item["id"].(string)
	return len([]rune(itemID)) <= codexInputItemIDLimit
}

func validCodexReasoningEncryptedContent(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxReasoningEncryptedBytes || !strings.HasPrefix(value, "gAAAA") {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(value)
	}
	if err != nil || len(decoded) < 73 || decoded[0] != 0x80 {
		return false
	}
	ciphertextLength := len(decoded) - 1 - 8 - 16 - 32
	return ciphertextLength > 0 && ciphertextLength%16 == 0
}

func canonicalCodexInputItemID(item map[string]any, itemID string) string {
	prefix := codexInputItemPrefix(item)
	if prefix == "" || itemID == "" || strings.HasPrefix(itemID, prefix) {
		return itemID
	}
	return prefix + "_" + itemID
}

func codexInputItemPrefix(item map[string]any) string {
	kind, _ := item["type"].(string)
	switch kind {
	case "message":
		return "msg"
	case "reasoning":
		return "rs"
	case "function_call":
		return "fc"
	case "custom_tool_call":
		return "ctc"
	case "custom_tool_call_output":
		return "ctco"
	}
	return ""
}

func uniqueCodexInputItemID(itemID string, reserved, used map[string]struct{}) string {
	for attempt := 0; ; attempt++ {
		candidate := shortenCodexInputItemID(itemID, attempt)
		if _, exists := reserved[candidate]; exists {
			continue
		}
		if _, exists := used[candidate]; exists {
			continue
		}
		return candidate
	}
}

func shortenCodexInputItemID(itemID string, attempt int) string {
	runes := []rune(itemID)
	hashInput := itemID
	if attempt > 0 {
		hashInput += "\x00" + strconv.Itoa(attempt)
	}
	digest := sha256.Sum256([]byte(hashInput))
	suffix := "_" + hex.EncodeToString(digest[:8])
	prefixLength := codexInputItemIDLimit - len(suffix)
	if prefixLength > len(runes) {
		prefixLength = len(runes)
	}
	return string(runes[:prefixLength]) + suffix
}
