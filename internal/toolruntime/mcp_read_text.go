// SPDX-License-Identifier: Elastic-2.0
package toolruntime

import (
	"bytes"
	"encoding/json"

	"agentplatform.local/platform/internal/domain"
)

const maxReadTextBlocks = 16

// Read only explicitly present text fields from the validated original result.
// Uniform JSON-array encoding preserves empty strings and block boundaries; no
// structured value, annotation or peer role becomes host authority.
func readTextValue(raw []byte, max int) (string, bool, error) {
	if max < 4 || len(raw) > 6*max+8192 {
		return "", false, failed("response_limit")
	}
	value, err := strictJSON(raw, 16)
	if err != nil {
		return "", false, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return "", false, failed("invalid_read_result")
	}
	if kind, exists := object["resultType"]; exists && kind != "complete" {
		return "", false, failed("invalid_read_result")
	}
	for _, key := range []string{"inputRequests", "requestState"} {
		if _, exists := object[key]; exists {
			return "", false, failed("invalid_read_result")
		}
	}
	isError := false
	if flag, exists := object["isError"]; exists {
		var valid bool
		isError, valid = flag.(bool)
		if !valid {
			return "", false, failed("invalid_read_result")
		}
	}
	content, ok := object["content"].([]any)
	if !ok || len(content) < 1 || len(content) > maxReadTextBlocks {
		return "", false, failed("invalid_read_result")
	}
	texts := make([]string, 0, len(content))
	total := 0
	for _, block := range content {
		item, ok := block.(map[string]any)
		if !ok || item["type"] != "text" {
			return "", false, failed("invalid_read_result")
		}
		textValue, ok := item["text"].(string)
		if !ok || !text(textValue, max) {
			return "", false, failed("invalid_read_result")
		}
		total += len(textValue)
		if total > max {
			return "", false, failed("response_limit")
		}
		texts = append(texts, textValue)
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(texts) != nil {
		return "", false, failed("invalid_read_result")
	}
	result := bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'})
	if len(result) > max {
		return "", false, failed("response_limit")
	}
	return string(result), isError, nil
}

func decodeReadObservation(raw []byte, execution domain.ToolExecution, max int) (wireResult, error) {
	result, rejected, err := readTextValue(raw, max)
	if err != nil {
		return wireResult{}, err
	}
	outcome := "succeeded"
	if rejected {
		outcome = "rejected"
	}
	// This identifies our observation of this read, not an externally committed
	// business write or proof that the returned information is true/free.
	return wireResult{ExecutionID: execution.ID, ActionID: execution.ActionID,
		IntentHash: execution.IntentHash, Outcome: outcome,
		EffectReference: "mcp-read:" + execution.ActionID, Result: result}, nil
}
