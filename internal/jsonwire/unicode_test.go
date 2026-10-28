// SPDX-License-Identifier: Elastic-2.0
package jsonwire

import (
	"encoding/json"
	"testing"
)

func TestUnicodeScalarBoundaries(t *testing.T) {
	valid := []string{`{"text":"Пример 😀"}`, `{"text":"\ud83d\ude00"}`, `{"text":"\uD800\uDC00"}`, `{"text":"\\ud800"}`, `{"text":"\"\\\uFFFD"}`, `{"text":"�"}`, `{"\u0061":"ok"}`}
	for _, raw := range valid {
		if !json.Valid([]byte(raw)) || !ValidUnicode([]byte(raw)) {
			t.Errorf("valid Unicode rejected: %q", raw)
		}
	}
	invalid := [][]byte{[]byte(`{"text":"\ud800"}`), []byte(`{"text":"\udc00"}`), []byte(`{"text":"\ud800\u0061"}`), []byte(`{"text":"\udc00\ud800"}`), []byte(`{"text":"\ud800\\udc00"}`), []byte(`{"\ud800":"key"}`), {'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}, {'{', '"', 'x', '"', ':', '"', 0xed, 0xa0, 0x80, '"', '}'}}
	for _, raw := range invalid {
		if ValidUnicode(raw) {
			t.Errorf("invalid Unicode accepted: %q", raw)
		}
	}
}
