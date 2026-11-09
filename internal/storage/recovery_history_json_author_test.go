// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type recoveryJSONFixture struct {
	ID      string    `json:"id"`
	Amount  int64     `json:"amount,string"`
	Mode    string    `json:"mode,omitempty"`
	Created time.Time `json:"created"`
	Nested  []struct {
		Name string `json:"name"`
	} `json:"nested,omitempty"`
	Raw json.RawMessage `json:"raw,omitempty"`
}

type RecoveryJSONRecursiveFixture struct{ *RecoveryJSONRecursiveFixture }

func TestRecoveryHistoryJSONAuthorLegacyAndAliases(t *testing.T) {
	valid := `{"id":"legacy","amount":"9007199254740993","created":"2026-10-28T00:00:00Z","raw":{"opaque":null}}`
	var result recoveryJSONFixture
	if err := decodeRecoveryHistoryJSON([]byte(valid), &result, 4096); err != nil || result.Amount != 9007199254740993 || result.Mode != "" || result.Nested != nil {
		t.Fatal("omitted legacy fields changed", err)
	}
	cases := map[string]string{
		"case_alias":        strings.Replace(valid, `"id"`, `"ID"`, 1),
		"unknown":           strings.Replace(valid, `"id":`, `"extra":1,"id":`, 1),
		"duplicate":         strings.Replace(valid, `"id":`, `"id":"first","id":`, 1),
		"escaped_duplicate": strings.Replace(valid, `"id":`, `"\u0069d":"first","id":`, 1),
		"nested_alias":      strings.Replace(valid, `"raw":`, `"nested":[{"Name":"wrong"}],"raw":`, 1),
		"raw_duplicate":     strings.Replace(valid, `"opaque":null`, `"opaque":null,"opaque":1`, 1),
		"leading_zero":      strings.Replace(valid, `"9007199254740993"`, `"01"`, 1),
		"plus_sign":         strings.Replace(valid, `"9007199254740993"`, `"+1"`, 1),
		"number_for_string": strings.Replace(valid, `"9007199254740993"`, `1`, 1),
		"overflow":          strings.Replace(valid, `"9007199254740993"`, `"9223372036854775808"`, 1),
		"unicode":           strings.Replace(valid, `"legacy"`, `"\ud800"`, 1),
		"trailing":          valid + `{}`,
		"depth":             strings.Replace(valid, `{"opaque":null}`, strings.Repeat("[", 66)+"0"+strings.Repeat("]", 66), 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var out recoveryJSONFixture
			if !errors.Is(decodeRecoveryHistoryJSON([]byte(raw), &out, 4096), ErrRecoveryHistoryJSON) {
				t.Fatal("ambiguous historical record accepted")
			}
		})
	}
	t.Run("bound", func(t *testing.T) {
		if decodeRecoveryHistoryJSON([]byte(valid), &result, len(valid)-1) == nil {
			t.Fatal("byte bound ignored")
		}
	})
	t.Run("nil", func(t *testing.T) {
		if decodeRecoveryHistoryJSON([]byte(valid), nil, 4096) == nil {
			t.Fatal("nil output accepted")
		}
	})
	t.Run("recursive_anonymous_type", func(t *testing.T) {
		var value RecoveryJSONRecursiveFixture
		if decodeRecoveryHistoryJSON([]byte(`{}`), &value, 4096) == nil {
			t.Fatal("unsupported recursive anonymous DTO accepted")
		}
	})
}
