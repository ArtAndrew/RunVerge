// SPDX-License-Identifier: Elastic-2.0
package toolruntime_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/progressguard"
	"agentplatform.local/platform/internal/toolruntime"
)

func TestIndependentNumericAdmissionBoundsBeforeExactArithmetic(t *testing.T) {
	// Source review confirms strictJSON rejects >128-byte numeric lexemes and
	// exponents outside [-308,308] before any big.Rat call. Use only modest
	// bounded negative examples here; never allocate an exponent-sized number.
	c := domain.ToolConnection{Tool: domain.ToolInfo{MaxInputBytes: 4096, InputSchemaJSON: `{"type":"object","properties":{"value":{"type":"number"}},"required":["value"],"additionalProperties":false}`}}
	valid := []string{`{"value":9007199254740993}`, `{"value":1e308}`, `{"value":1e-308}`, `{"value":` + strings.Repeat("9", 128) + `}`}
	for _, raw := range valid {
		got, err := toolruntime.CanonicalArguments(c, raw)
		if err != nil || got != raw {
			t.Fatalf("bounded numeric admission changed approved bytes: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	invalid := []string{`{"value":1e309}`, `{"value":1e-309}`, `{"value":1e10000}`, `{"value":` + strings.Repeat("9", 129) + `}`}
	for _, raw := range invalid {
		if _, err := toolruntime.CanonicalArguments(c, raw); err == nil {
			t.Fatalf("numeric complexity profile admitted %s", raw)
		}
	}
	// Guard supports a wider compact exponent without making that value valid
	// for tool delivery: canonical loop identity is not execution admission.
	if _, err := progressguard.FingerprintArguments(ctx, `{"value":1e10000}`); err != nil {
		t.Fatal("safe compact fingerprint rejected bounded lexeme", err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatal("modest numeric boundary probes exceeded budget", err)
	}
}
