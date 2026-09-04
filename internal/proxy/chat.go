package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func (s *Service) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !s.beginRequest(w) {
		return
	}
	defer s.endRequest()
	started := time.Now()
	requestID := newID("req")
	status := http.StatusOK
	errText, model := "", ""
	var inputTokens, outputTokens int64
	telemetry := requestTelemetry{}
	defer func() { s.record(r, requestID, model, status, started, inputTokens, outputTokens, telemetry, errText) }()

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes))
	if err != nil {
		status, errText = http.StatusBadRequest, "request body is too large or unreadable"
		writeError(w, status, "invalid_request", errText, requestID)
		return
	}
	body, stream, includeUsage, parsedModel, err := convertChatRequest(raw)
	model = parsedModel
	if err != nil {
		status, errText = http.StatusBadRequest, err.Error()
		writeError(w, status, "invalid_request", errText, requestID)
		return
	}
	telemetry.ReasoningEffort = reasoningEffortFromBody(body)
	credential, err := s.auth.FreshCredential(r.Context())
	if err != nil {
		status, errText = http.StatusServiceUnavailable, err.Error()
		writeError(w, status, "account_unavailable", accountUnavailableClientMessage, requestID)
		return
	}
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.cfg.UpstreamBaseURL+"/backend-api/codex/responses", bytes.NewReader(body))
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
	if stream {
		var usage tokenUsage
		var observed requestTelemetry
		status, usage, observed, errText = streamChatResponse(w, resp.Body, requestID, model, includeUsage, started)
		inputTokens, outputTokens = usage.InputTokens, usage.OutputTokens
		telemetry.mergeResponse(observed, usage)
		return
	}
	final, usage, err := collectResponse(resp.Body)
	if err != nil {
		var failure *upstreamResponseError
		if errors.As(err, &failure) {
			status, errText = failure.HTTPStatus(), failure.Error()
			writeUpstreamResponseFailure(w, failure, requestID)
		} else {
			status, errText = http.StatusBadGateway, err.Error()
			writeError(w, status, "upstream_stream_failed", errText, requestID)
		}
		return
	}
	inputTokens, outputTokens = usage.InputTokens, usage.OutputTokens
	telemetry.mergeResponse(requestTelemetry{}, usage)
	converted, err := convertChatResponse(final)
	if err != nil {
		status, errText = http.StatusBadGateway, err.Error()
		writeError(w, status, "invalid_upstream_response", errText, requestID)
		return
	}
	writeJSONBytes(w, http.StatusOK, converted, requestID)
}

func convertChatRequest(raw []byte) ([]byte, bool, bool, string, error) {
	if !json.Valid(raw) {
		return nil, false, false, "", fmt.Errorf("invalid Chat Completions request: body must contain one JSON value")
	}
	var request struct {
		Model             string           `json:"model"`
		Messages          []map[string]any `json:"messages"`
		Tools             []map[string]any `json:"tools"`
		ToolChoice        any              `json:"tool_choice"`
		Stream            bool             `json:"stream"`
		StreamOptions     map[string]any   `json:"stream_options"`
		ParallelToolCalls *bool            `json:"parallel_tool_calls"`
		ReasoningEffort   string           `json:"reasoning_effort"`
		Reasoning         struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&request); err != nil {
		return nil, false, false, "", fmt.Errorf("invalid Chat Completions request: %w", err)
	}
	if strings.TrimSpace(request.Model) == "" {
		return nil, false, false, "", fmt.Errorf("model is required")
	}
	if len(request.Messages) == 0 {
		return nil, false, false, request.Model, fmt.Errorf("messages is required")
	}
	responses := map[string]any{"model": request.Model, "stream": request.Stream}
	input := make([]any, 0, len(request.Messages))
	instructions := make([]string, 0)
	for _, message := range request.Messages {
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer":
			if text := flattenTextContent(message["content"]); text != "" {
				instructions = append(instructions, text)
			}
		case "user":
			content := convertUserContent(message["content"])
			if len(content) > 0 {
				input = append(input, map[string]any{"type": "message", "role": "user", "content": content})
			}
		case "assistant":
			if text := flattenTextContent(message["content"]); text != "" {
				input = append(input, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}})
			}
			if calls, ok := message["tool_calls"].([]any); ok {
				for _, callValue := range calls {
					call, _ := callValue.(map[string]any)
					function, _ := call["function"].(map[string]any)
					name, _ := function["name"].(string)
					arguments, _ := function["arguments"].(string)
					callID, _ := call["id"].(string)
					if name != "" && callID != "" {
						input = append(input, map[string]any{"type": "function_call", "call_id": callID, "name": name, "arguments": arguments})
					}
				}
			}
		case "tool":
			callID, _ := message["tool_call_id"].(string)
			if callID == "" {
				return nil, false, false, request.Model, fmt.Errorf("tool message is missing tool_call_id")
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": callID, "output": flattenTextContent(message["content"])})
		default:
			return nil, false, false, request.Model, fmt.Errorf("unsupported message role %q", role)
		}
	}
	if len(input) == 0 {
		return nil, false, false, request.Model, fmt.Errorf("messages did not contain any usable input")
	}
	responses["input"] = input
	responses["instructions"] = strings.Join(instructions, "\n\n")
	if len(request.Tools) > 0 {
		tools := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			if tool["type"] != "function" {
				tools = append(tools, tool)
				continue
			}
			function, _ := tool["function"].(map[string]any)
			name, _ := function["name"].(string)
			if name == "" {
				continue
			}
			converted := map[string]any{"type": "function", "name": name}
			for _, field := range []string{"description", "parameters", "strict"} {
				if value, exists := function[field]; exists {
					converted[field] = value
				}
			}
			tools = append(tools, converted)
		}
		responses["tools"] = tools
	}
	if request.ToolChoice != nil {
		responses["tool_choice"] = convertToolChoice(request.ToolChoice)
	}
	if request.ParallelToolCalls != nil {
		responses["parallel_tool_calls"] = *request.ParallelToolCalls
	}
	reasoningEffort := canonicalReasoningEffort(request.ReasoningEffort)
	if reasoningEffort == "" {
		reasoningEffort = canonicalReasoningEffort(request.Reasoning.Effort)
	}
	if reasoningEffort == "" || reasoningEffort == "auto" {
		reasoningEffort = "medium"
	}
	reasoning := map[string]any{"effort": reasoningEffort}
	if reasoningEffort != "none" {
		reasoning["summary"] = "auto"
	}
	responses["reasoning"] = reasoning
	if request.PromptCacheKey != "" {
		responses["prompt_cache_key"] = request.PromptCacheKey
	}
	includeUsage, _ := request.StreamOptions["include_usage"].(bool)
	encoded, err := json.Marshal(responses)
	if err != nil {
		return nil, false, false, request.Model, err
	}
	normalized, _, _, err := normalizeResponsesRequest(encoded)
	return normalized, request.Stream, includeUsage, request.Model, err
}

func flattenTextContent(value any) string {
	switch content := value.(type) {
	case string:
		return content
	case nil:
		return ""
	case []any:
		parts := make([]string, 0, len(content))
		for _, item := range content {
			part, _ := item.(map[string]any)
			if kind, _ := part["type"].(string); kind == "text" || kind == "input_text" || kind == "output_text" {
				if text, _ := part["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		raw, _ := json.Marshal(content)
		return string(raw)
	}
}

func convertUserContent(value any) []any {
	if text, ok := value.(string); ok {
		return []any{map[string]any{"type": "input_text", "text": text}}
	}
	items, _ := value.([]any)
	result := make([]any, 0, len(items))
	for _, item := range items {
		part, _ := item.(map[string]any)
		switch part["type"] {
		case "text", "input_text":
			if text, _ := part["text"].(string); text != "" {
				result = append(result, map[string]any{"type": "input_text", "text": text})
			}
		case "image_url":
			image, _ := part["image_url"].(map[string]any)
			if location, _ := image["url"].(string); location != "" {
				converted := map[string]any{"type": "input_image", "image_url": location}
				if detail, _ := image["detail"].(string); detail != "" {
					converted["detail"] = detail
				}
				result = append(result, converted)
			}
		case "input_image":
			result = append(result, part)
		}
	}
	return result
}

func convertToolChoice(value any) any {
	choice, ok := value.(map[string]any)
	if !ok || choice["type"] != "function" {
		return value
	}
	function, _ := choice["function"].(map[string]any)
	if name, _ := function["name"].(string); name != "" {
		return map[string]any{"type": "function", "name": name}
	}
	return value
}

func convertChatResponse(raw []byte) ([]byte, error) {
	var response struct {
		ID        string            `json:"id"`
		Model     string            `json:"model"`
		CreatedAt int64             `json:"created_at"`
		Output    []json.RawMessage `json:"output"`
		Usage     struct {
			InputTokens        int64 `json:"input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
			TotalTokens        int64 `json:"total_tokens"`
			OutputTokenDetails struct {
				ReasoningTokens *int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode completed response: %w", err)
	}
	message := map[string]any{"role": "assistant", "content": ""}
	textParts := make([]string, 0)
	reasoningParts := make([]string, 0)
	toolCalls := make([]any, 0)
	for _, itemRaw := range response.Output {
		var item map[string]any
		if json.Unmarshal(itemRaw, &item) != nil {
			continue
		}
		switch item["type"] {
		case "reasoning":
			if summary, ok := item["summary"].([]any); ok {
				for _, partValue := range summary {
					part, _ := partValue.(map[string]any)
					if part["type"] == "summary_text" {
						if text, _ := part["text"].(string); text != "" {
							reasoningParts = append(reasoningParts, text)
						}
					}
				}
			}
			if content, ok := item["content"].([]any); ok {
				for _, partValue := range content {
					part, _ := partValue.(map[string]any)
					if part["type"] == "reasoning_text" {
						if text, _ := part["text"].(string); text != "" {
							reasoningParts = append(reasoningParts, text)
						}
					}
				}
			}
		case "message":
			switch content := item["content"].(type) {
			case string:
				if content != "" {
					textParts = append(textParts, content)
				}
			case []any:
				for _, partValue := range content {
					part, _ := partValue.(map[string]any)
					if text, _ := part["text"].(string); text != "" {
						textParts = append(textParts, text)
					}
				}
			}
		case "function_call":
			toolCalls = append(toolCalls, map[string]any{
				"id": item["call_id"], "type": "function",
				"function": map[string]any{"name": item["name"], "arguments": item["arguments"]},
			})
		}
	}
	message["content"] = strings.Join(textParts, "")
	if len(reasoningParts) > 0 {
		message["reasoning_content"] = strings.Join(reasoningParts, "")
	}
	finishReason := "stop"
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finishReason = "tool_calls"
	}
	created := response.CreatedAt
	if created == 0 {
		created = time.Now().Unix()
	}
	usage := map[string]any{"prompt_tokens": response.Usage.InputTokens, "completion_tokens": response.Usage.OutputTokens, "total_tokens": response.Usage.TotalTokens}
	if response.Usage.OutputTokenDetails.ReasoningTokens != nil {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": *response.Usage.OutputTokenDetails.ReasoningTokens}
	}
	result := map[string]any{
		"id": response.ID, "object": "chat.completion", "created": created, "model": response.Model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}},
		"usage":   usage,
	}
	return json.Marshal(result)
}

type chatStreamState struct {
	id           string
	model        string
	created      int64
	roleSent     bool
	terminalSent bool
	tools        map[int]int
	nextTool     int
	usage        tokenUsage
	telemetry    requestTelemetry
	started      time.Time
}

func streamChatResponse(w http.ResponseWriter, reader io.Reader, requestID, fallbackModel string, includeUsage bool, started time.Time) (int, tokenUsage, requestTelemetry, string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unsupported", "server does not support streaming", requestID)
		return http.StatusInternalServerError, tokenUsage{}, requestTelemetry{}, "streaming unsupported"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Request-Id", requestID)
	w.WriteHeader(http.StatusOK)
	state := &chatStreamState{model: fallbackModel, created: time.Now().Unix(), tools: make(map[int]int), started: started}
	buffered := bufio.NewReader(reader)
	for {
		line, err := buffered.ReadBytes('\n')
		if payload := ssePayload(line); len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
			if writeErr := translateChatEvent(w, flusher, payload, state, includeUsage); writeErr != nil {
				return 499, state.usage, state.telemetry, "client disconnected"
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if !state.terminalSent {
					_ = writeChatDone(w, flusher)
				}
				return http.StatusOK, state.usage, state.telemetry, ""
			}
			return http.StatusBadGateway, state.usage, state.telemetry, err.Error()
		}
	}
}

func translateChatEvent(w io.Writer, flusher http.Flusher, payload []byte, state *chatStreamState, includeUsage bool) error {
	var event struct {
		Type        string          `json:"type"`
		Delta       string          `json:"delta"`
		OutputIndex int             `json:"output_index"`
		Response    json.RawMessage `json:"response"`
		Item        struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"item"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil
	}
	if len(event.Response) > 0 {
		var metadata struct {
			ID        string `json:"id"`
			Model     string `json:"model"`
			CreatedAt int64  `json:"created_at"`
		}
		_ = json.Unmarshal(event.Response, &metadata)
		if metadata.ID != "" {
			state.id = metadata.ID
		}
		if metadata.Model != "" {
			state.model = metadata.Model
		}
		if metadata.CreatedAt != 0 {
			state.created = metadata.CreatedAt
		}
		parseResponseUsage(event.Response, &state.usage)
	}
	send := func(delta map[string]any, finish any, usage any) error {
		chunk := map[string]any{"id": state.id, "object": "chat.completion.chunk", "created": state.created, "model": state.model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		if usage != nil {
			chunk["usage"] = usage
		}
		return writeChatData(w, flusher, chunk)
	}
	sendUsage := func(usage map[string]any) error {
		chunk := map[string]any{"id": state.id, "object": "chat.completion.chunk", "created": state.created, "model": state.model, "choices": []any{}, "usage": usage}
		return writeChatData(w, flusher, chunk)
	}
	ensureRole := func() error {
		if state.roleSent {
			return nil
		}
		state.roleSent = true
		return send(map[string]any{"role": "assistant", "content": ""}, nil, nil)
	}
	switch event.Type {
	case "response.created":
		return ensureRole()
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if event.Delta == "" {
			return nil
		}
		if state.telemetry.FirstReasoningMS == 0 {
			state.telemetry.FirstReasoningMS = elapsedMS(state.started)
		}
		if err := ensureRole(); err != nil {
			return err
		}
		return send(map[string]any{"reasoning_content": event.Delta}, nil, nil)
	case "response.reasoning_summary_text.done", "response.reasoning_text.done":
		return nil
	case "response.output_text.delta":
		if event.Delta != "" && state.telemetry.FirstOutputMS == 0 {
			state.telemetry.FirstOutputMS = elapsedMS(state.started)
		}
		if err := ensureRole(); err != nil {
			return err
		}
		return send(map[string]any{"content": event.Delta}, nil, nil)
	case "response.output_item.added":
		if event.Item.Type != "function_call" {
			return nil
		}
		if err := ensureRole(); err != nil {
			return err
		}
		index := state.nextTool
		state.nextTool++
		state.tools[event.OutputIndex] = index
		return send(map[string]any{"tool_calls": []any{map[string]any{"index": index, "id": event.Item.CallID, "type": "function", "function": map[string]any{"name": event.Item.Name, "arguments": ""}}}}, nil, nil)
	case "response.function_call_arguments.delta":
		index, exists := state.tools[event.OutputIndex]
		if !exists {
			index = state.nextTool
			state.nextTool++
			state.tools[event.OutputIndex] = index
		}
		return send(map[string]any{"tool_calls": []any{map[string]any{"index": index, "function": map[string]any{"arguments": event.Delta}}}}, nil, nil)
	case "response.completed", "response.incomplete":
		if err := ensureRole(); err != nil {
			return err
		}
		finish := "stop"
		if len(state.tools) > 0 {
			finish = "tool_calls"
		}
		if err := send(map[string]any{}, finish, nil); err != nil {
			return err
		}
		if includeUsage {
			usage := map[string]any{"prompt_tokens": state.usage.InputTokens, "completion_tokens": state.usage.OutputTokens, "total_tokens": state.usage.InputTokens + state.usage.OutputTokens}
			if state.usage.HasReasoningTokens {
				usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": state.usage.ReasoningTokens}
			}
			if err := sendUsage(usage); err != nil {
				return err
			}
		}
		state.terminalSent = true
		return writeChatDone(w, flusher)
	case "error", "response.failed":
		message := responseFailureMessage(event.Error.Message, event.Response)
		if err := writeChatData(w, flusher, map[string]any{"error": map[string]any{"message": message, "type": "upstream_error"}}); err != nil {
			return err
		}
		state.terminalSent = true
		return writeChatDone(w, flusher)
	}
	return nil
}

func writeChatData(w io.Writer, flusher http.Flusher, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeChatDone(w io.Writer, flusher http.Flusher) error {
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
