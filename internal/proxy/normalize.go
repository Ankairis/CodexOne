package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

func normalizeResponsesRequest(raw []byte) ([]byte, bool, string, error) {
	if !json.Valid(raw) {
		return nil, false, "", fmt.Errorf("invalid Responses request: body must contain one JSON value")
	}
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		return nil, false, "", fmt.Errorf("invalid Responses request: %w", err)
	}
	requestedStream, _ := body["stream"].(bool)
	model, _ := body["model"].(string)
	if model == "" {
		return nil, false, "", fmt.Errorf("model is required")
	}

	input, hasInput := body["input"]
	if !hasInput || input == nil {
		return nil, false, "", fmt.Errorf("input is required")
	}
	if input, ok := input.(string); ok {
		body["input"] = []any{map[string]any{
			"type":    "message",
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": input}},
		}}
	}
	if body["instructions"] == nil {
		body["instructions"] = ""
	}
	if body["tools"] == nil {
		delete(body, "tools")
	}
	if body["reasoning"] == nil {
		delete(body, "reasoning")
	}
	body["stream"] = true
	body["store"] = false
	body["parallel_tool_calls"] = true
	body["include"] = []string{"reasoning.encrypted_content"}
	normalizeReasoningConfig(body)

	for _, field := range []string{
		"max_output_tokens", "max_completion_tokens", "temperature", "top_p", "truncation",
		"prompt_cache_options", "prompt_cache_retention", "context_management", "user",
	} {
		delete(body, field)
	}
	if tier, ok := body["service_tier"].(string); ok && tier != "priority" {
		delete(body, "service_tier")
	}
	normalizeInput(body["input"])
	normalizeTools(body["tools"])
	if choice, ok := body["tool_choice"].(map[string]any); ok {
		normalizeToolType(choice)
		normalizeTools(choice["tools"])
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, false, "", fmt.Errorf("encode Responses request: %w", err)
	}
	return encoded, requestedStream, model, nil
}

func normalizeReasoningConfig(body map[string]any) {
	reasoning, _ := body["reasoning"].(map[string]any)
	flatEffort, _ := body["reasoning_effort"].(string)
	delete(body, "reasoning_effort")

	if reasoning == nil && strings.TrimSpace(flatEffort) != "" {
		reasoning = make(map[string]any)
		body["reasoning"] = reasoning
	}
	if reasoning == nil {
		return
	}
	effort, _ := reasoning["effort"].(string)
	if strings.TrimSpace(effort) == "" {
		effort = flatEffort
	}
	effort = canonicalReasoningEffort(effort)
	if effort == "auto" {
		effort = "medium"
	}
	if effort != "" {
		reasoning["effort"] = effort
	}
	if _, exists := reasoning["summary"]; !exists {
		if legacy, ok := reasoning["generate_summary"].(string); ok && strings.TrimSpace(legacy) != "" {
			reasoning["summary"] = strings.ToLower(strings.TrimSpace(legacy))
		} else if effort != "" && effort != "none" {
			reasoning["summary"] = "auto"
		}
	}
	delete(reasoning, "generate_summary")
}

func canonicalReasoningEffort(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	compact := strings.NewReplacer("-", "", "_", "", " ", "").Replace(normalized)
	switch compact {
	case "off", "disabled":
		return "none"
	case "xhigh", "extrahigh":
		return "xhigh"
	case "maximum":
		return "max"
	default:
		return normalized
	}
}

func reasoningEffortFromBody(raw []byte) string {
	var body struct {
		ReasoningEffort string `json:"reasoning_effort"`
		Reasoning       struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	if effort := canonicalReasoningEffort(body.Reasoning.Effort); effort != "" {
		return effort
	}
	return canonicalReasoningEffort(body.ReasoningEffort)
}

func normalizeInput(value any) {
	items, ok := value.([]any)
	if !ok {
		return
	}
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if object["role"] == "system" {
			object["role"] = "developer"
		}
		content, ok := object["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range content {
			if object, ok := part.(map[string]any); ok {
				delete(object, "prompt_cache_breakpoint")
			}
		}
	}
}

func normalizeTools(value any) {
	tools, ok := value.([]any)
	if !ok {
		return
	}
	for _, tool := range tools {
		if object, ok := tool.(map[string]any); ok {
			normalizeToolType(object)
		}
	}
}

func normalizeToolType(tool map[string]any) {
	if kind, ok := tool["type"].(string); ok && (kind == "web_search_preview" || kind == "web_search_preview_2025_03_11") {
		tool["type"] = "web_search"
	}
}
