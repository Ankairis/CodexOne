package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
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

	if input, ok := body["input"].(string); ok {
		body["input"] = []any{map[string]any{
			"type":    "message",
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": input}},
		}}
	}
	if _, ok := body["input"]; !ok {
		return nil, false, "", fmt.Errorf("input is required")
	}
	if body["instructions"] == nil {
		body["instructions"] = ""
	}
	body["stream"] = true
	body["store"] = false
	body["parallel_tool_calls"] = true
	body["include"] = []string{"reasoning.encrypted_content"}

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
