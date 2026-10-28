// SPDX-License-Identifier: Elastic-2.0
package modelgateway

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strings"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/jsonwire"
)

func decodeResult(body []byte, outputLimit int) (Result, error) {
	return decodeResultWithTools(body, outputLimit, nil, nil)
}

func decodeResultWithTools(body []byte, outputLimit int, tools []domain.ModelToolDefinition, history []domain.ModelMessage) (Result, error) {
	return decodeProfileResult(body, outputLimit, tools, history, "")
}

func decodeProfileResult(body []byte, outputLimit int, tools []domain.ModelToolDefinition, history []domain.ModelMessage, expectedModel string) (Result, error) {
	if !jsonwire.ValidUnicode(body) {
		return Result{}, unknown("invalid_response_json")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	value, err := decodeValue(decoder, 0)
	if err != nil {
		return Result{}, unknown("invalid_response_json")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return Result{}, unknown("invalid_response_json")
	}
	root, ok := value.(map[string]any)
	if !ok {
		return Result{}, unknown("invalid_response_json")
	}
	if expectedModel != "" {
		id, validID := root["id"].(string)
		_, hasError := root["error"]
		if root["model"] != expectedModel || !validID || !safeASCII(id, 256) || hasError {
			return Result{}, unknown("openrouter_identity_unknown")
		}
	}
	usage, ok := root["usage"].(map[string]any)
	if !ok {
		return Result{}, unknown("invalid_usage")
	}
	inputTokens, validInput := tokenCount(usage["prompt_tokens"])
	outputTokens, validOutput := tokenCount(usage["completion_tokens"])
	if !validInput || !validOutput || inputTokens > math.MaxInt64-outputTokens {
		return Result{}, unknown("invalid_usage")
	}
	if total, exists := usage["total_tokens"]; exists {
		totalTokens, valid := tokenCount(total)
		if !valid || totalTokens != inputTokens+outputTokens {
			return Result{}, unknown("invalid_usage")
		}
	}
	if outputTokens > int64(outputLimit) {
		return Result{}, unknown("output_token_limit_exceeded")
	}
	choices, ok := root["choices"].([]any)
	if !ok || len(choices) != 1 {
		return Result{}, unknown("invalid_completion")
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return Result{}, unknown("invalid_completion")
	}
	if reason, exists := choice["finish_reason"]; exists && reason != "stop" && reason != "length" && reason != "tool_calls" {
		return Result{}, unknown("unsupported_completion")
	}
	message, ok := choice["message"].(map[string]any)
	if !ok {
		return Result{}, unknown("invalid_completion")
	}
	if role, exists := message["role"]; exists && role != "assistant" {
		return Result{}, unknown("unsupported_completion")
	}
	if function, exists := message["function_call"]; exists && function != nil {
		return Result{}, unknown("unsupported_completion")
	}
	var calls []domain.ModelToolCall
	if rawCalls, exists := message["tool_calls"]; exists && rawCalls != nil {
		array, ok := rawCalls.([]any)
		if !ok {
			return Result{}, unknown("unsupported_completion")
		}
		if len(array) != 0 {
			if len(tools) == 0 {
				return Result{}, unknown("unsupported_completion")
			}
			if len(array) > maxToolCalls || choice["finish_reason"] != "tool_calls" {
				return Result{}, unknown("invalid_tool_calls")
			}
			allowed, seen := toolNames(tools), map[string]bool{}
			for _, message := range history {
				for _, call := range message.ToolCalls {
					seen[call.ID] = true
				}
			}
			for _, value := range array {
				object, ok := value.(map[string]any)
				if !ok || len(object) != 3 || object["type"] != "function" {
					return Result{}, unknown("invalid_tool_calls")
				}
				function, ok := object["function"].(map[string]any)
				if !ok || len(function) != 2 {
					return Result{}, unknown("invalid_tool_calls")
				}
				id, idOK := object["id"].(string)
				name, nameOK := function["name"].(string)
				arguments, argsOK := function["arguments"].(string)
				call := domain.ModelToolCall{ID: id, Name: name, ArgumentsJSON: arguments}
				if !idOK || !nameOK || !argsOK || !validCall(call, allowed) || seen[id] {
					return Result{}, unknown("invalid_tool_calls")
				}
				seen[id] = true
				calls = append(calls, call)
			}
		}
	}
	text, textOK := message["content"].(string)
	if len(calls) == 0 {
		if expectedModel != "" && (!textOK || strings.TrimSpace(text) == "") && choice["finish_reason"] == "length" {
			return Result{}, unknown("output_budget_exhausted")
		}
		if !textOK || strings.TrimSpace(text) == "" || choice["finish_reason"] == "tool_calls" {
			return Result{}, unknown("invalid_completion")
		}
	} else if message["content"] != nil && !textOK {
		return Result{}, unknown("invalid_tool_calls")
	}
	if len(calls) != 0 && !contentText(text, maxResponseBytes) {
		return Result{}, unknown("invalid_completion")
	}
	result := Result{Text: text, InputTokens: inputTokens, OutputTokens: outputTokens, ToolCalls: calls}
	if id, ok := root["id"].(string); ok && safeASCII(id, 256) {
		result.ProviderRequestID = id
	}
	return result, nil
}

func tokenCount(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	count, err := number.Int64()
	return count, err == nil && count >= 0
}

// decodeValue rejects duplicate keys, excessive nesting, and non-JSON values.
// UseNumber preserves integer precision before token-count validation.
func decodeValue(decoder *json.Decoder, depth int) (any, error) {
	return decodeValueBounded(decoder, depth, 32)
}

func decodeValueBounded(decoder *json.Decoder, depth, maxDepth int) (any, error) {
	if depth > maxDepth {
		return nil, unknown("invalid_response_json")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, unknown("invalid_response_json")
			}
			if _, duplicate := object[name]; duplicate {
				return nil, unknown("invalid_response_json")
			}
			value, err := decodeValueBounded(decoder, depth+1, maxDepth)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		close, err := decoder.Token()
		if err != nil || close != json.Delim('}') {
			return nil, unknown("invalid_response_json")
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			value, err := decodeValueBounded(decoder, depth+1, maxDepth)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		close, err := decoder.Token()
		if err != nil || close != json.Delim(']') {
			return nil, unknown("invalid_response_json")
		}
		return array, nil
	default:
		return nil, unknown("invalid_response_json")
	}
}
