// SPDX-License-Identifier: Elastic-2.0
package progressguard_test

// Independent tests use exact rational comparisons only for small, explicitly
// bounded reference numbers. The production fingerprint must never expand an
// attacker-controlled exponent or convert an integer through float64.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/progressguard"
)

func independentFingerprint(t *testing.T, raw string) string {
	t.Helper()
	got, err := progressguard.FingerprintArguments(context.Background(), raw)
	if err != nil {
		t.Fatalf("valid bounded JSON rejected: %s: %v", raw, err)
	}
	return got
}
func independentDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
func independentHasSignal(d progressguard.Decision, code string) bool {
	for _, s := range d.Signals {
		if s.Code == code {
			return true
		}
	}
	return false
}

func TestIndependentProgressGuardNumbersAgreeWithBoundedExactReference(t *testing.T) {
	// This covers exact decimal equivalence and separation independently of the
	// implementation's coefficient/exponent representation. Values near 2^53
	// must not alias, including negative numbers and decimal spellings.
	values := []string{"0", "-0", "0.000e12", "1", "1.0", "10e-1", "0.01e2", "-1", "-10.0e-1", "1000", "1e3", "0.0001", "1e-4", "9007199254740992", "9007199254740993", "9007199254740993.0", "90071992547409930e-1", "-9007199254740992", "-9007199254740993", "123456789012345678901234567890.125", "123456789012345678901234567890125e-3"}
	for _, n := range []int64{7, 1001, 99999, 9007199254740993} {
		for _, e := range []int{-5, -1, 0, 1, 5} {
			values = append(values, fmt.Sprintf("%de%d", n, e), fmt.Sprintf("%d0e%d", n, e-1))
		}
	}
	fingerprints := make([]string, len(values))
	rationals := make([]*big.Rat, len(values))
	for i, v := range values {
		var ok bool
		rationals[i], ok = new(big.Rat).SetString(v)
		if !ok {
			t.Fatalf("bad independently bounded reference %s", v)
		}
		fingerprints[i] = independentFingerprint(t, `{"nested":{"b":[true,null,"text"],"a":`+v+`}}`)
	}
	for i := range values {
		for j := range values {
			equal := rationals[i].Cmp(rationals[j]) == 0
			if (fingerprints[i] == fingerprints[j]) != equal {
				t.Fatalf("numeric equivalence mismatch: %s / %s", values[i], values[j])
			}
		}
	}
}

func TestIndependentProgressGuardSemanticBoundaries(t *testing.T) {
	equivalent := []string{`{"b":[1,true,null],"a":"😀"}`, ` { "\u0061":"\ud83d\ude00", "b" : [1.000e0,true,null]} `}
	if independentFingerprint(t, equivalent[0]) != independentFingerprint(t, equivalent[1]) {
		t.Fatal("JSON escape or object order changed semantic identity")
	}
	distinct := []string{`{"a":null}`, `{"a":"null"}`, `{"a":false}`, `{"a":0}`, `{"a":"0"}`, `{"a":[]}`, `{"a":{}}`, `{"a":[1,2]}`, `{"a":[2,1]}`, `{"a":"é"}`, `{"a":"e\u0301"}`, `{"a":"x"}`, `{"a":" x"}`}
	seen := map[string]string{}
	for _, raw := range distinct {
		fp := independentFingerprint(t, raw)
		if old, ok := seen[fp]; ok {
			t.Fatalf("unsupported coercion: %s / %s", old, raw)
		}
		seen[fp] = raw
	}
	for _, raw := range []string{`{"a":1,"\u0061":2}`, `{"x":"\ud800"}`, `{"x":"\udc00"}`, `{"x":1} {}`, `[{}]`, "{\"x\":\"\xff\"}"} {
		if _, err := progressguard.FingerprintArguments(context.Background(), raw); err == nil {
			t.Fatalf("ambiguous/malformed input admitted: %q", raw)
		}
	}
}

func TestIndependentProgressGuardExponentAndComplexityStayCompact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, raw := range []string{`{"x":1e2147483647}`, `{"x":10e2147483647}`, `{"x":1.1e-2147483648}`, `{"x":-0e2147483647}`} {
		if _, err := progressguard.FingerprintArguments(ctx, raw); err != nil {
			t.Fatalf("compact bounded exponent rejected: %v", err)
		}
	}
	for _, raw := range []string{`{"x":1e2147483648}`, `{"x":1e-2147483649}`, `{"x":1e9999999999999999999999999}`, `{"x":` + strings.Repeat("1", progressguard.MaxNumberBytes+1) + `}`, `{"x":"` + strings.Repeat("x", progressguard.MaxArgumentsBytes) + `"}`, `{"x":` + strings.Repeat("[", progressguard.MaxJSONDepth+1) + `0` + strings.Repeat("]", progressguard.MaxJSONDepth+1) + `}`} {
		if _, err := progressguard.FingerprintArguments(ctx, raw); !errors.Is(err, progressguard.ErrComplexity) {
			t.Fatalf("bounded resource input returned %v", err)
		}
	}
	many := `{"x":[` + strings.Repeat("0,", progressguard.MaxJSONNodes) + `0]}`
	if _, err := progressguard.FingerprintArguments(ctx, many); !errors.Is(err, progressguard.ErrComplexity) {
		t.Fatalf("node budget not enforced: %v", err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("small compact test exceeded its work budget: %v", err)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := progressguard.FingerprintArguments(cancelled, `{}`); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled guard ignored cancellation", err)
	}
}

func TestIndependentProgressGuardCausalHistoryAndBeforeEffectStops(t *testing.T) {
	ctx := context.Background()
	policy := progressguard.DefaultPolicy()
	a := progressguard.Intent{ToolID: "stable-tool", Effect: "read", ArgumentsJSON: `{"cursor":"a","n":9007199254740993}`}
	b := progressguard.Intent{ToolID: "stable-tool", Effect: "read", ArgumentsJSON: `{"cursor":"b","n":9007199254740993}`}
	afp, err := progressguard.IntentFingerprint(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	bfp, err := progressguard.IntentFingerprint(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	a.ArgumentsJSON = ` {"n":90071992547409930e-1,"cursor":"a"}`
	history := []progressguard.Observation{{ID: "old-run-action-a", IntentFingerprint: afp, Effect: "read", Outcome: "succeeded"}, {ID: "old-run-action-b", IntentFingerprint: bfp, Effect: "read", Outcome: "succeeded"}, {ID: "new-run-action-a", IntentFingerprint: afp, Effect: "read", Outcome: "succeeded"}}
	d, err := progressguard.Evaluate(ctx, policy, history, &b)
	if err != nil || !d.Stop || !independentHasSignal(d, "alternating_intents") {
		t.Fatal("fourth alternating request not stopped before effect", d, err)
	}
	for _, s := range d.Signals {
		if s.Code == "alternating_intents" && len(s.EvidenceIDs) != 3 {
			t.Fatal("proposed request falsely recorded as completed evidence", s)
		}
	}
	// A fresh grant/progress marker cannot erase an unknown or a successful
	// side effect; the pure input projection resolves outcomes in-place.
	history = []progressguard.Observation{{ID: "old-execution", IntentFingerprint: afp, Effect: "write", Outcome: "succeeded", ProgressDigest: independentDigest("before")}, {ID: "new-grant-plan", Outcome: "output", OutputDigest: independentDigest("different-plan"), ProgressDigest: independentDigest("after"), UnverifiedOutput: true}}
	d, err = progressguard.Evaluate(ctx, policy, history, &a)
	if err != nil || !d.Stop || !independentHasSignal(d, "repeated_effect_intent") {
		t.Fatal("grant/progress reset successful effect guard", d, err)
	}
	history[0].Outcome = "unknown"
	d, err = progressguard.Evaluate(ctx, policy, history, &b)
	if err != nil || !d.Stop || !independentHasSignal(d, "unknown_effect") {
		t.Fatal("unknown effect bypassed by different intent", d, err)
	}
	history[0].Outcome = "rejected"
	d, err = progressguard.Evaluate(ctx, policy, history, &b)
	if err != nil || d.Stop {
		t.Fatal("resolved rejected action incorrectly remained unknown", d, err)
	}
	history = append(history, history[0])
	if _, err = progressguard.Evaluate(ctx, policy, history, nil); !errors.Is(err, progressguard.ErrInvalidInput) {
		t.Fatal("duplicate lifecycle projection counted as separate evidence", err)
	}
}
