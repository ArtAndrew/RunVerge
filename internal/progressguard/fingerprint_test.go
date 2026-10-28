// SPDX-License-Identifier: Elastic-2.0
package progressguard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func mustFingerprint(t *testing.T, raw string) string {
	t.Helper()
	value, err := FingerprintArguments(context.Background(), raw)
	if err != nil {
		t.Fatalf("fingerprint failed: %v", err)
	}
	return value
}

func TestExactSemanticJSONEquivalence(t *testing.T) {
	cases := map[string][]string{
		"order_whitespace":      {`{"x":1,"nested":{"b":2,"a":[true,null,"x"]}}`, " {\n\"nested\": {\"a\":[true,null,\"x\"], \"b\":2},\"x\":1 } "},
		"decimal":               {`{"n":1}`, `{"n":1.0}`, `{"n":10e-1}`, `{"n":0.001e3}`, `{"n":1e+00000}`},
		"negative_zero":         {`{"n":0}`, `{"n":-0}`, `{"n":-0.0e2147483647}`, `{"n":0e-2147483648}`},
		"large_integer":         {`{"n":9007199254740993}`, `{"n":9007199254740993.0}`, `{"n":900719925474099300e-2}`},
		"huge_exponent_compact": {`{"n":1e2147483647}`, `{"n":10e2147483646}`, `{"n":100e2147483645}`},
		"unicode_escapes":       {`{"a":"😀"}`, `{"\u0061":"\ud83d\ude00"}`},
		"control_scalar":        {`{"a":"\u0000\n\t"}`, `{"a":"\u0000\u000a\u0009"}`},
	}
	for name, variants := range cases {
		t.Run(name, func(t *testing.T) {
			want := mustFingerprint(t, variants[0])
			for _, variant := range variants[1:] {
				if got := mustFingerprint(t, variant); got != want {
					t.Fatalf("equivalent JSON got different fingerprint: %q", variant)
				}
			}
		})
	}
}

func TestSemanticFingerprintPreservesMeaningfulDifferences(t *testing.T) {
	cases := map[string][2]string{
		"adjacent_large_ints":   {`{"n":9007199254740992}`, `{"n":9007199254740993}`},
		"decimal_precision":     {`{"n":0.10000000000000000000000000000000000000000000000000001}`, `{"n":0.1}`},
		"exponent_precision":    {`{"n":1e308}`, `{"n":1e309}`},
		"type":                  {`{"n":1}`, `{"n":"1"}`},
		"null":                  {`{"n":null}`, `{}`},
		"array_order":           {`{"n":[1,2]}`, `{"n":[2,1]}`},
		"array_multiplicity":    {`{"n":[1]}`, `{"n":[1,1]}`},
		"whitespace_value":      {`{"n":" x"}`, `{"n":"x"}`},
		"unicode_normalization": {`{"n":"é"}`, `{"n":"e\u0301"}`},
		"case":                  {`{"a":1}`, `{"A":1}`},
		"frame_collision":       {`{"ab":"c"}`, `{"a":"bc"}`},
		"nested_frame":          {`{"a":{"b":1,"c":2}}`, `{"a":{"b":1},"c":2}`},
	}
	for name, pair := range cases {
		t.Run(name, func(t *testing.T) {
			if mustFingerprint(t, pair[0]) == mustFingerprint(t, pair[1]) {
				t.Fatal("different values received same semantic fingerprint")
			}
		})
	}
}

func TestFingerprintRejectsAmbiguousOrInvalidJSON(t *testing.T) {
	cases := []string{
		``, `null`, `[]`, `1`, `"object"`, `{"a":1,"a":2}`, `{"a":1,"\u0061":2}`,
		`{"nested":{"x":1,"x":2}}`, `{"n":NaN}`, `{"n":Infinity}`, `{"n":01}`, `{"n":1.}`, `{"n":.1}`,
		`{"n":1} {}`, `{"n":1,}`, `{"a":"\ud800"}`, `{"a":"\udc00"}`, `{"\ud800":1}`,
		`{"a":"\ud800\u0061"}`, string([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}),
	}
	for index, raw := range cases {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			if _, err := FingerprintArguments(context.Background(), raw); !errors.Is(err, ErrInvalidJSON) {
				t.Fatalf("wanted invalid JSON, got %v", err)
			}
		})
	}
}

func TestFingerprintBoundedWorkAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FingerprintArguments(ctx, `{}`); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled fingerprint: %v", err)
	}
	cases := map[string]string{
		"bytes":                      `{"x":"` + strings.Repeat("x", MaxArgumentsBytes) + `"}`,
		"depth":                      `{"x":` + strings.Repeat("[", MaxJSONDepth+1) + "0" + strings.Repeat("]", MaxJSONDepth+1) + "}",
		"nodes":                      `{"x":[` + strings.Repeat("0,", MaxJSONNodes) + "0]}",
		"number_bytes":               `{"x":` + strings.Repeat("1", MaxNumberBytes+1) + "}",
		"exponent_overflow":          `{"x":1e2147483648}`,
		"negative_exponent_overflow": `{"x":1e-2147483649}`,
		"exponent_lexeme_bomb":       `{"x":1e` + strings.Repeat("9", 500) + "}",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := FingerprintArguments(context.Background(), raw); !errors.Is(err, ErrComplexity) {
				t.Fatalf("wanted bounded-complexity rejection, got %v", err)
			}
		})
	}
	// These extremes stay compact and execute without exponent expansion.
	for _, raw := range []string{`{"x":1e2147483647}`, `{"x":1e-2147483648}`, `{"x":` + strings.Repeat("1", MaxNumberBytes) + "}"} {
		mustFingerprint(t, raw)
	}
}

func TestIntentUsesStableToolIdentityAndExactSemanticArguments(t *testing.T) {
	ctx := context.Background()
	a, err := IntentFingerprint(ctx, Intent{ToolID: "crm_create", Effect: "write", ArgumentsJSON: `{"name":"A","amount":9007199254740993}`})
	if err != nil {
		t.Fatal(err)
	}
	b, err := IntentFingerprint(ctx, Intent{ToolID: "crm_create", Effect: "destructive", ArgumentsJSON: ` {"amount":90071992547409930e-1,"name":"\u0041"}`})
	if err != nil || a != b {
		t.Fatalf("stable identity changed: %v", err)
	}
	c, err := IntentFingerprint(ctx, Intent{ToolID: "crm_update", Effect: "write", ArgumentsJSON: `{"name":"A","amount":9007199254740993}`})
	if err != nil || a == c {
		t.Fatalf("different tool identity aliased: %v", err)
	}
	for _, in := range []Intent{{}, {ToolID: " ", Effect: "read", ArgumentsJSON: `{}`}, {ToolID: "x", Effect: "unknown", ArgumentsJSON: `{}`}, {ToolID: strings.Repeat("x", 129), Effect: "read", ArgumentsJSON: `{}`}} {
		if _, err := IntentFingerprint(ctx, in); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("bad identity: %v", err)
		}
	}
}

func FuzzFingerprintArgumentsNeverPanics(f *testing.F) {
	for _, seed := range []string{`{}`, `{"n":9007199254740993}`, `{"x":"\ud800"}`, `{"x":1e2147483648}`, `{"x":[1,true,null]}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > MaxArgumentsBytes+1 {
			return
		}
		one, err := FingerprintArguments(context.Background(), raw)
		two, again := FingerprintArguments(context.Background(), raw)
		if (err == nil) != (again == nil) || one != two {
			t.Fatal("non-deterministic fingerprint")
		}
	})
}
