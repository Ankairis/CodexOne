package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const defaultWebSocketReplayBytes = 32 << 20

type webSocketConversation struct {
	identity       *codexIdentity
	model          string
	instructions   json.RawMessage
	history        []json.RawMessage
	lastResponseID string
	lastGeneration uint64
	pendingCallIDs []string
	hasCompleted   bool
	maxReplayBytes int64
}

type webSocketTurnPlan struct {
	model        string
	reasoning    string
	incremental  []byte
	replay       []byte
	currentInput []json.RawMessage
	reset        bool
	continuation bool
	identity     *codexIdentity
}

func (c *webSocketConversation) prepare(r *http.Request, raw []byte, remap bool, planTypes ...string) (webSocketTurnPlan, error) {
	if !json.Valid(raw) {
		return webSocketTurnPlan{}, fmt.Errorf("WebSocket request must contain valid JSON")
	}
	object, err := decodeJSONObject(raw)
	if err != nil {
		return webSocketTurnPlan{}, err
	}
	requestType := jsonString(object["type"])
	if requestType != "response.create" && requestType != "response.append" {
		return webSocketTurnPlan{}, fmt.Errorf("unsupported WebSocket request type %q", requestType)
	}
	if requestType == "response.append" && !c.hasCompleted {
		return webSocketTurnPlan{}, fmt.Errorf("response.append requires a completed response on this connection")
	}
	if _, exists := object["input"]; !exists {
		return webSocketTurnPlan{}, fmt.Errorf("input is required")
	}
	if requestType == "response.append" {
		if _, ok := object["input"].([]any); !ok {
			return webSocketTurnPlan{}, fmt.Errorf("response.append requires an input array")
		}
	}

	previousResponseID := jsonString(object["previous_response_id"])
	if !c.hasCompleted && previousResponseID != "" {
		return webSocketTurnPlan{}, fmt.Errorf("previous_response_id is not available on this WebSocket")
	}
	continuation := requestType == "response.append" || previousResponseID != ""
	inputLooksComplete := webSocketInputLooksLikeTranscript(object["input"])
	reset := !continuation || inputLooksComplete
	if inputLooksComplete {
		continuation = false
		delete(object, "previous_response_id")
	}
	if continuation && previousResponseID != "" && previousResponseID != c.lastResponseID {
		return webSocketTurnPlan{}, fmt.Errorf("previous_response_id does not match the last response on this WebSocket")
	}
	if continuation && !webSocketInputSatisfiesCalls(object["input"], c.pendingCallIDs) {
		return webSocketTurnPlan{}, fmt.Errorf("input is missing one or more required tool outputs")
	}

	model := jsonString(object["model"])
	if model == "" {
		model = c.model
	}
	if model == "" {
		return webSocketTurnPlan{}, fmt.Errorf("model is required in the first response.create request")
	}
	object["model"] = model
	if continuation {
		if _, exists := object["instructions"]; !exists && len(c.instructions) > 0 {
			var instructions any
			if json.Unmarshal(c.instructions, &instructions) == nil {
				object["instructions"] = instructions
			}
		}
		if previousResponseID == "" {
			object["previous_response_id"] = c.lastResponseID
		}
	}
	object["type"] = "response.create"
	encoded, err := json.Marshal(object)
	if err != nil {
		return webSocketTurnPlan{}, fmt.Errorf("encode WebSocket request: %w", err)
	}
	normalized, _, _, err := normalizeResponsesRequest(encoded)
	if err != nil {
		return webSocketTurnPlan{}, err
	}
	planType := ""
	if len(planTypes) > 0 {
		planType = planTypes[0]
	}
	normalized, err = prepareCodexWebSocketBody(normalized, model, planType, r.Header)
	if err != nil {
		return webSocketTurnPlan{}, err
	}

	identity := c.identity
	if identity == nil {
		normalized, identity, err = prepareRequestIdentity(r.Context(), r.Header, normalized, remap)
		if err != nil {
			return webSocketTurnPlan{}, err
		}
	} else {
		normalized, err = identity.prepareBody(normalized, true)
		if err != nil {
			return webSocketTurnPlan{}, err
		}
	}
	normalizedObject, err := decodeJSONObject(normalized)
	if err != nil {
		return webSocketTurnPlan{}, err
	}
	normalizedObject["type"] = "response.create"
	incremental, err := json.Marshal(normalizedObject)
	if err != nil {
		return webSocketTurnPlan{}, fmt.Errorf("encode incremental WebSocket request: %w", err)
	}
	currentInput, err := rawMessageArray(normalizedObject["input"])
	if err != nil {
		return webSocketTurnPlan{}, fmt.Errorf("decode normalized input: %w", err)
	}

	replayInput := currentInput
	if !reset {
		replayInput = mergeWebSocketItems(c.history, currentInput)
	}
	replayObject := cloneJSONObject(normalizedObject)
	replayObject["input"] = rawMessagesAsAny(replayInput)
	delete(replayObject, "previous_response_id")
	replayObject["type"] = "response.create"
	replay, err := json.Marshal(replayObject)
	if err != nil {
		return webSocketTurnPlan{}, fmt.Errorf("encode replay WebSocket request: %w", err)
	}
	if int64(len(replay)) > c.replayLimit() {
		return webSocketTurnPlan{}, c.replayLimitError()
	}

	return webSocketTurnPlan{
		model:        model,
		reasoning:    reasoningEffortFromBody(normalized),
		incremental:  incremental,
		replay:       replay,
		currentInput: currentInput,
		reset:        reset,
		continuation: continuation,
		identity:     identity,
	}, nil
}

func (c *webSocketConversation) bodyFor(plan webSocketTurnPlan, generation uint64, httpFallback bool) []byte {
	body := plan.replay
	if !httpFallback && plan.continuation && generation != 0 && generation == c.lastGeneration {
		body = plan.incremental
	}
	return body
}

func (c *webSocketConversation) commit(plan webSocketTurnPlan, terminal []byte, generation uint64) error {
	var event struct {
		Type     string `json:"type"`
		Response struct {
			ID           string            `json:"id"`
			Output       []json.RawMessage `json:"output"`
			Instructions json.RawMessage   `json:"instructions"`
		} `json:"response"`
	}
	if err := json.Unmarshal(terminal, &event); err != nil {
		return fmt.Errorf("decode terminal WebSocket response: %w", err)
	}
	if strings.TrimSpace(event.Response.ID) == "" {
		return fmt.Errorf("terminal WebSocket response is missing response.id")
	}
	history := cloneRawMessages(plan.currentInput)
	if !plan.reset {
		history = mergeWebSocketItems(c.history, plan.currentInput)
	}
	history = mergeWebSocketItems(history, event.Response.Output)
	replayObject, err := decodeJSONObject(plan.replay)
	if err != nil {
		return fmt.Errorf("decode replay WebSocket request: %w", err)
	}
	replayObject["input"] = rawMessagesAsAny(history)
	replay, err := json.Marshal(replayObject)
	if err != nil {
		return fmt.Errorf("encode replay WebSocket request: %w", err)
	}
	if int64(len(replay)) > c.replayLimit() {
		return c.replayLimitError()
	}
	c.history = history
	c.identity = plan.identity
	c.model = plan.model
	c.lastResponseID = strings.TrimSpace(event.Response.ID)
	c.lastGeneration = generation
	c.pendingCallIDs = pendingWebSocketCallIDs(event.Response.Output)
	c.hasCompleted = true

	requestObject, _ := decodeJSONObject(plan.incremental)
	if raw, exists := requestObject["instructions"]; exists {
		if encoded, err := json.Marshal(raw); err == nil {
			c.instructions = encoded
		}
	} else if len(event.Response.Instructions) > 0 {
		c.instructions = bytes.Clone(event.Response.Instructions)
	}
	return nil
}

func (c *webSocketConversation) replayLimit() int64 {
	if c != nil && c.maxReplayBytes > 0 {
		return c.maxReplayBytes
	}
	return defaultWebSocketReplayBytes
}

func (c *webSocketConversation) replayLimitError() error {
	limit := c.replayLimit()
	return fmt.Errorf("conversation replay exceeds %d MiB; send a compact response.create transcript", limit>>20)
}

func (c *webSocketConversation) identityFor(plan webSocketTurnPlan) *codexIdentity {
	if c.identity != nil {
		return c.identity
	}
	return plan.identity
}

func webSocketInputLooksLikeTranscript(value any) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		kind := jsonString(item["type"])
		switch kind {
		case "function_call", "custom_tool_call", "compaction", "compaction_trigger":
			return true
		case "message":
			if jsonString(item["role"]) == "assistant" {
				return true
			}
		}
	}
	return false
}

func webSocketInputSatisfiesCalls(value any, pending []string) bool {
	if len(pending) == 0 {
		return true
	}
	items, ok := value.([]any)
	if !ok {
		return false
	}
	outputs := make(map[string]struct{}, len(items))
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		switch jsonString(item["type"]) {
		case "function_call_output", "custom_tool_call_output":
			if callID := jsonString(item["call_id"]); callID != "" {
				outputs[callID] = struct{}{}
			}
		}
	}
	for _, callID := range pending {
		if _, exists := outputs[callID]; !exists {
			return false
		}
	}
	return true
}

func pendingWebSocketCallIDs(items []json.RawMessage) []string {
	seen := make(map[string]struct{})
	var pending []string
	for _, raw := range items {
		var item map[string]any
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		switch jsonString(item["type"]) {
		case "function_call", "custom_tool_call":
			if callID := jsonString(item["call_id"]); callID != "" {
				if _, exists := seen[callID]; !exists {
					seen[callID] = struct{}{}
					pending = append(pending, callID)
				}
			}
		}
	}
	return pending
}

type webSocketItemMetadata struct {
	id     string
	kind   string
	callID string
}

func mergeWebSocketItems(groups ...[]json.RawMessage) []json.RawMessage {
	items := make([]json.RawMessage, 0)
	for _, group := range groups {
		items = append(items, cloneRawMessages(group)...)
	}
	if len(items) < 2 {
		return items
	}
	metadata := make([]webSocketItemMetadata, len(items))
	for index, raw := range items {
		var object map[string]any
		if json.Unmarshal(raw, &object) != nil {
			continue
		}
		metadata[index] = webSocketItemMetadata{
			id:     jsonString(object["id"]),
			kind:   jsonString(object["type"]),
			callID: jsonString(object["call_id"]),
		}
	}
	seenCalls := make(map[string]struct{})
	callDeduped := make([]json.RawMessage, 0, len(items))
	callMetadata := make([]webSocketItemMetadata, 0, len(items))
	for index, item := range items {
		meta := metadata[index]
		if isWebSocketToolCall(meta.kind) && meta.callID != "" {
			if _, exists := seenCalls[meta.callID]; exists {
				continue
			}
			seenCalls[meta.callID] = struct{}{}
		}
		callDeduped = append(callDeduped, item)
		callMetadata = append(callMetadata, meta)
	}

	referencedCalls := make(map[string]struct{})
	for _, meta := range callMetadata {
		if (meta.kind == "function_call_output" || meta.kind == "custom_tool_call_output") && meta.callID != "" {
			referencedCalls[meta.callID] = struct{}{}
		}
	}
	keepByID := make(map[string]int)
	keepReferenced := make(map[string]bool)
	for index, meta := range callMetadata {
		if meta.id == "" {
			continue
		}
		_, referenced := referencedCalls[meta.callID]
		referenced = referenced && meta.callID != ""
		if _, exists := keepByID[meta.id]; !exists || referenced || !keepReferenced[meta.id] {
			keepByID[meta.id] = index
			keepReferenced[meta.id] = referenced
		}
	}
	out := make([]json.RawMessage, 0, len(callDeduped))
	for index, item := range callDeduped {
		if id := callMetadata[index].id; id != "" && keepByID[id] != index {
			continue
		}
		out = append(out, item)
	}
	return out
}

func isWebSocketToolCall(kind string) bool {
	return kind == "function_call" || kind == "custom_tool_call"
}

func rawMessageArray(value any) ([]json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var items []json.RawMessage
	if err = json.Unmarshal(encoded, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func rawMessagesAsAny(items []json.RawMessage) []any {
	out := make([]any, 0, len(items))
	for _, raw := range items {
		var value any
		if json.Unmarshal(raw, &value) == nil {
			out = append(out, value)
		}
	}
	return out
}

func cloneRawMessages(items []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, len(items))
	for index := range items {
		out[index] = bytes.Clone(items[index])
	}
	return out
}

func cloneJSONObject(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func removeWebSocketType(body []byte) ([]byte, error) {
	object, err := decodeJSONObject(body)
	if err != nil {
		return nil, err
	}
	delete(object, "type")
	return json.Marshal(object)
}
