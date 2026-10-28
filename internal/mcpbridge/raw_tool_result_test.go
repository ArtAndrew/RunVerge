// SPDX-License-Identifier: Elastic-2.0
package mcpbridge

import (
	"encoding/json"
	"testing"
)

func TestRawToolResultExactIDAndOriginalBytes(t *testing.T) {
	const result = `{ "content": [{"type":"text","text":"data"}], "structuredContent":{"large":9007199254740993} }`
	for _, test := range []struct {
		name, request, reply string
		valid                bool
	}{
		{"large_match", `9007199254740993`, `9007199254740993`, true},
		{"no_float_collision", `9007199254740993`, `9007199254740992`, false},
		{"type_not_coerced", `1`, `"1"`, false},
		{"escaped_string", `"one"`, `"\u006fne"`, true},
		{"null", `1`, `null`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			ok := validToolResultResponse([]byte(`{"jsonrpc":"2.0","id":`+test.reply+`,"result":`+result+`}`), "application/json", json.RawMessage(test.request), func(raw json.RawMessage) error {
				calls++
				if string(raw) != result {
					t.Errorf("original result changed: %s", raw)
				}
				return nil
			})
			if ok != test.valid || (test.valid && calls != 1) || (!test.valid && calls != 0) {
				t.Fatalf("wrong result binding %v calls%d", ok, calls)
			}
		})
	}
}
