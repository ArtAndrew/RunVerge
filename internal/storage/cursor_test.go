// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestCursorRejectsIncompleteAmbiguousAndMalformedPositions(t *testing.T) {
	for _, input := range []string{
		`{}`, `null`, `[]`, `{"id":"x"}`, `{"created_at":"2026-10-23T10:00:00Z"}`,
		`{"created_at":null,"id":"x"}`, `{"created_at":"2026-10-23T10:00:00Z","id":null}`,
		`{"created_at":"2026-10-23T10:00:00Z","id":123}`,
		`{"Created_At":"2026-10-23T10:00:00Z","id":"x"}`,
		`{"created_at":"2026-10-23T10:00:00Z","ID":"x"}`,
		`{"created_at":"2026-10-23T10:00:00Z","id":"x","id":"y"}`,
		`{"created_at":"2026-10-23T10:00:00Z","id":"x","extra":true}`,
		`{"created_at":"2026-10-23T10:00:00Z","id":"x"} {}`,
		`{"created_at":"2026-10-23T10:00:00Z","id":"\u0000"}`,
		"{\"created_at\":\"2026-10-23T10:00:00Z\",\"id\":\"\xff\"}",
	} {
		encoded := base64.RawURLEncoding.EncodeToString([]byte(input))
		if _, e := decodeCursor(encoded); e == nil {
			t.Errorf("accepted malformed cursor %q", input)
		}
	}
	for _, encoded := range []string{"not-json", strings.Repeat("a", 513)} {
		if _, e := decodeCursor(encoded); e == nil {
			t.Error("accepted invalid encoded cursor")
		}
	}
}

func TestCursorEmptyPageAndCanonicalRoundTrip(t *testing.T) {
	first, e := decodeCursor("")
	if e != nil || first.CreatedAt.Year() != 9999 || first.ID != "~" {
		t.Fatal("empty cursor must select the initial page")
	}
	when := time.Date(2026, 10, 23, 10, 0, 0, 123456000, time.UTC)
	position, e := decodeCursor(encodeCursor(when, "tsk_test"))
	if e != nil || !position.CreatedAt.Equal(when) || position.ID != "tsk_test" {
		t.Fatal("valid cursor lost its exact position")
	}
}
