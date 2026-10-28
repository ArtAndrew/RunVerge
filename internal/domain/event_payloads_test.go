// SPDX-License-Identifier: Elastic-2.0
package domain

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestPublicEventConformanceCorpus(t *testing.T) {
	raw, err := os.ReadFile("../../contracts/event-conformance.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Cases []struct {
			Name  string          `json:"name"`
			Valid bool            `json:"valid"`
			Event json.RawMessage `json:"event"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.Cases) < 64 {
		t.Fatal("event corpus does not cover registered kinds and wire failures")
	}
	for _, c := range corpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			_, err := DecodeEventWire(c.Event)
			if (err == nil) != c.Valid {
				t.Fatalf("valid=%v err=%v", c.Valid, err)
			}
		})
	}
}
func TestEventProducerRejectsDuplicateOptionalAndUnicodeFields(t *testing.T) {
	for _, raw := range []string{
		`{"reason":"one","reason":"two"}`,
		`{"reason":"\ud800"}`,
		`{"reason":"\u0000"}`,
		`{"reason":"` + strings.Repeat("x", MaxEventPayloadBytes) + `"}`,
	} {
		if _, err := NormalizeEventPayload("task.blocked", json.RawMessage(raw)); err == nil {
			t.Fatal("invalid producer payload admitted")
		}
	}
	valid := `{"material_id":"mat_x","version":2,"base_version":1,"content_digest":"` + strings.Repeat("a", 64) + `","principal_id":"usr_x","draft_only":true,"costs_unchanged":true,"merge_base_version":null}`
	raw, err := NormalizeEventPayload("material.revised", json.RawMessage(valid))
	if err != nil || strings.Contains(string(raw), "merge_base_version") {
		t.Fatal("absent optional merge base normalization failed", err)
	}
	duplicate := strings.TrimSuffix(valid, "}") + `,"merge_base_version":null}`
	if _, err = NormalizeEventPayload("material.revised", json.RawMessage(duplicate)); err == nil {
		t.Fatal("duplicate nil optional field bypassed validation")
	}
}
