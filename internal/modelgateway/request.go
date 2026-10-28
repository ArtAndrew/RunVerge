// SPDX-License-Identifier: Elastic-2.0
package modelgateway

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/jsonwire"
)

const maxTools = domain.MaxModelTools
const maxToolCalls = domain.MaxModelToolCalls
const maxMessages = 2 + (domain.MaxModelTurnsPerTask-1)*(domain.MaxModelToolCalls+1)

var functionName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// BuildRequest is the shared, network-free admission and wire encoder. The
// executor hashes the same frozen messages and definitions which Execute sends;
// dispatch timing and private credentials never enter the message body.
func (g *Gateway) BuildRequest(input Request) ([]byte, error) {
	if g == nil || !safeASCII(input.ID, 128) || !validText(input.Instructions, g.config.MaxInputBytes) ||
		!validText(input.Goal, g.config.MaxInputBytes) || len(input.Instructions)+len(input.Goal) > g.config.MaxInputBytes {
		return nil, rejected("invalid_request")
	}
	tools, err := wireTools(input.Tools)
	if err != nil {
		return nil, err
	}
	messages := []map[string]any{{"role": "system", "content": input.Instructions}, {"role": "user", "content": input.Goal}}
	if len(input.Messages) != 0 {
		messages, err = wireMessages(input.Messages, input.Tools, input.Instructions, input.Goal, g.config.MaxInputBytes)
		if err != nil {
			return nil, err
		}
	}
	payload := map[string]any{
		"model": g.config.Model, "messages": messages, "stream": false, "n": 1,
		g.config.TokenLimitField: g.config.MaxOutputTokens,
	}
	if len(tools) != 0 {
		payload["tools"], payload["tool_choice"], payload["parallel_tool_calls"] = tools, "auto", true
	}
	body, err := json.Marshal(payload)
	if err != nil || len(body) > g.config.MaxInputBytes {
		return nil, rejected("request_too_large")
	}
	return body, nil
}

func wireTools(definitions []domain.ModelToolDefinition) ([]map[string]any, error) {
	if len(definitions) > maxTools {
		return nil, rejected("invalid_tools")
	}
	tools := make([]map[string]any, 0, len(definitions))
	names, ids := map[string]bool{}, map[string]bool{}
	for _, d := range definitions {
		if !functionName.MatchString(d.Name) || names[d.Name] || !safeASCII(d.ToolID, 128) || ids[d.ToolID] ||
			d.ToolVersion < 1 || !validDigest(d.ToolProfileDigest) || !contentText(d.Description, 4096) || len(d.InputSchemaJSON) > 8192 {
			return nil, rejected("invalid_tools")
		}
		schema, err := strictObject(d.InputSchemaJSON)
		if err != nil || schema["type"] != "object" {
			return nil, rejected("invalid_tools")
		}
		names[d.Name], ids[d.ToolID] = true, true
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name": d.Name, "description": d.Description, "parameters": json.RawMessage(d.InputSchemaJSON),
		}})
	}
	return tools, nil
}

func wireMessages(messages []domain.ModelMessage, tools []domain.ModelToolDefinition, instructions, goal string, maxBytes int) ([]map[string]any, error) {
	if len(messages) < 2 || len(messages) > maxMessages || messages[0].Role != "system" || messages[0].Content != instructions ||
		messages[1].Role != "user" || messages[1].Content != goal {
		return nil, rejected("invalid_messages")
	}
	allowed, seen, pending := toolNames(tools), map[string]bool{}, map[string]bool{}
	result := make([]map[string]any, 0, len(messages))
	for i, message := range messages {
		if !contentText(message.Content, maxBytes) {
			return nil, rejected("invalid_messages")
		}
		wire := map[string]any{"role": message.Role, "content": message.Content}
		switch message.Role {
		case "system", "user":
			if i > 1 || message.ToolCallID != "" || len(message.ToolCalls) != 0 {
				return nil, rejected("invalid_messages")
			}
		case "assistant":
			if i < 2 || len(pending) != 0 || message.ToolCallID != "" || len(message.ToolCalls) == 0 || len(message.ToolCalls) > maxToolCalls {
				return nil, rejected("invalid_messages")
			}
			calls := make([]map[string]any, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				if !validCall(call, allowed) || seen[call.ID] {
					return nil, rejected("invalid_messages")
				}
				seen[call.ID], pending[call.ID] = true, true
				calls = append(calls, wireCall(call))
			}
			wire["tool_calls"] = calls
		case "tool":
			if i < 2 || !pending[message.ToolCallID] || len(message.ToolCalls) != 0 || message.Content == "" {
				return nil, rejected("invalid_messages")
			}
			delete(pending, message.ToolCallID)
			wire["tool_call_id"] = message.ToolCallID
		default:
			return nil, rejected("invalid_messages")
		}
		result = append(result, wire)
	}
	if len(pending) != 0 {
		return nil, rejected("invalid_messages")
	}
	return result, nil
}

func toolNames(tools []domain.ModelToolDefinition) map[string]bool {
	names := make(map[string]bool, len(tools))
	for _, tool := range tools {
		names[tool.Name] = true
	}
	return names
}

func wireCall(call domain.ModelToolCall) map[string]any {
	return map[string]any{"id": call.ID, "type": "function", "function": map[string]string{"name": call.Name, "arguments": call.ArgumentsJSON}}
}

func validCall(call domain.ModelToolCall, allowed map[string]bool) bool {
	if !functionName.MatchString(call.ID) || !allowed[call.Name] || len(call.ArgumentsJSON) > domain.MaxToolArgumentsBytes {
		return false
	}
	_, err := strictObject(call.ArgumentsJSON)
	return err == nil
}

// strictObject checks the JSON syntax without rewriting the argument string.
// In particular no float conversion, default, sorting or whitespace removal is
// permitted before the exact bytes become part of an approved tool intent.
func strictObject(raw string) (map[string]any, error) {
	if len(raw) == 0 || !jsonwire.ValidUnicode([]byte(raw)) || strings.ContainsRune(raw, '\x00') {
		return nil, rejected("invalid_tool_json")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.UseNumber()
	value, err := decodeValueBounded(decoder, 0, 16)
	if err != nil {
		return nil, rejected("invalid_tool_json")
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, rejected("invalid_tool_json")
	}
	object, ok := value.(map[string]any)
	if !ok || !validObjectStrings(object) {
		return nil, rejected("invalid_tool_json")
	}
	return object, nil
}

func validObjectStrings(value any) bool {
	switch v := value.(type) {
	case string:
		return contentText(v, maxRequestBytes)
	case map[string]any:
		for key, item := range v {
			if !contentText(key, 4096) || !validObjectStrings(item) {
				return false
			}
		}
	case []any:
		for _, item := range v {
			if !validObjectStrings(item) {
				return false
			}
		}
	}
	return true
}

func contentText(value string, maxBytes int) bool {
	return len(value) <= maxBytes && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}
