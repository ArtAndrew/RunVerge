// SPDX-License-Identifier: Elastic-2.0
package mcpbridge

import (
	"bytes"
	"encoding/json"
)

// The SDK still performs JSON-RPC correlation. This additional policy must see
// only the one result for this actual outbound tools/call, never discovery,
// notifications, another ID or a typed result whose defaults hide wire defects.
func validToolResultResponse(raw []byte, contentType string, requestID json.RawMessage, validate func(json.RawMessage) error) bool {
	matched := 0
	ok := walkActionResponse(raw, contentType, func(body []byte) bool {
		var frame map[string]json.RawMessage
		if json.Unmarshal(body, &frame) != nil || frame == nil {
			return false
		}
		id, hasID := frame["id"]
		if !hasID || !sameRPCID(requestID, id) {
			return true
		}
		matched++
		if matched != 1 {
			return false
		}
		var version string
		if json.Unmarshal(frame["jsonrpc"], &version) != nil || version != "2.0" {
			return false
		}
		if _, exists := frame["method"]; exists {
			return false
		}
		if _, exists := frame["error"]; exists {
			return false
		}
		result, exists := frame["result"]
		return exists && validate(append(json.RawMessage(nil), result...)) == nil
	})
	return ok && matched == 1
}

func sameRPCID(expected, actual json.RawMessage) bool {
	a, b := bytes.TrimSpace(expected), bytes.TrimSpace(actual)
	if len(a) == 0 || len(b) == 0 || len(a) > 1024 || len(b) > 1024 {
		return false
	}
	if a[0] == '"' && b[0] == '"' {
		var left, right string
		return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && left == right
	}
	// SDK-generated numeric IDs are exact integer tokens. Never use float64
	// (large IDs must not collide), or equate a string ID with a numeric one.
	if (a[0] == '-' || a[0] >= '0' && a[0] <= '9') && (b[0] == '-' || b[0] >= '0' && b[0] <= '9') {
		return bytes.Equal(a, b)
	}
	return false
}
