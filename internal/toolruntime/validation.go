// SPDX-License-Identifier: Elastic-2.0
package toolruntime

import (
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/jsonwire"
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"github.com/google/jsonschema-go/jsonschema"
	"io"
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var credentialReference = regexp.MustCompile(`^PLATFORM_TOOL_KEY_[A-Z0-9_]{1,64}$`)
var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

func text(v string, n int) bool {
	return len(v) <= n && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}
func digest(v []byte) string { sum := sha256.Sum256(v); return hex.EncodeToString(sum[:]) }

func ValidateConnection(c domain.ToolConnection) error {
	t := c.Tool
	if !identifier.MatchString(t.ID) || !identifier.MatchString(t.OrganizationID) || t.Version < 1 || !text(t.Name, 120) || t.Name == "" || !text(t.Description, 4096) || !text(t.Destination, 256) || t.Destination == "" || !text(t.Provider, 128) || t.Provider == "" || !text(t.PriceVersion, 128) || t.PriceVersion == "" || t.MaxChargeMicroUSD < 0 || t.MaxChargeMicroUSD > domain.MaxMoney || t.TimeoutSeconds < 1 || t.TimeoutSeconds > 30 || t.MaxInputBytes < 2 || t.MaxInputBytes > domain.MaxToolArgumentsBytes || t.MaxOutputBytes < 1 || t.MaxOutputBytes > 262144 {
		return failed("invalid_profile")
	}
	if t.Effect != "read" && t.Effect != "write" && t.Effect != "destructive" {
		return failed("invalid_effect")
	}
	if t.Effect == "destructive" && !t.RequireDistinctApprover {
		return failed("distinct_approver_required")
	}
	if c.ResultContract != domain.ToolResultContractActionV1 && c.ResultContract != domain.ToolResultContractMCPReadTextV1 || (c.Transport != "http_json" && c.Transport != "mcp_https") || (c.CredentialEnvironment != "" && !credentialReference.MatchString(c.CredentialEnvironment)) {
		return failed("invalid_profile")
	}
	if c.ResultContract == domain.ToolResultContractMCPReadTextV1 && (c.Transport != "mcp_https" || t.Effect != "read" || t.MaxOutputBytes < 4) {
		return failed("invalid_read_profile")
	}
	u, e := url.Parse(c.Endpoint)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(c.Endpoint, "\x00\r\n") {
		return failed("invalid_endpoint")
	}
	if p := u.Port(); p != "" {
		n, e := strconv.Atoi(p)
		if e != nil || n < 1 || n > 65535 {
			return failed("invalid_endpoint")
		}
	}
	if (c.Transport == "mcp_https" && !identifier.MatchString(c.RemoteTool)) || (c.Transport == "http_json" && c.RemoteTool != "") {
		return failed("invalid_remote_tool")
	}
	if t.InputSchemaDigest != digest([]byte(t.InputSchemaJSON)) {
		return failed("invalid_schema_digest")
	}
	if _, e := connectionRoots(c.TrustedCAPEM); e != nil {
		return e
	}
	_, e = resolveSchema(t.InputSchemaJSON)
	return e
}

func connectionRoots(raw string) (*x509.CertPool, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 65536 {
		return nil, failed("invalid_trust_roots")
	}
	pool := x509.NewCertPool()
	rest := []byte(raw)
	count := 0
	for len(bytes.TrimSpace(rest)) > 0 {
		rest = bytes.TrimSpace(rest)
		if !bytes.HasPrefix(rest, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, failed("invalid_trust_roots")
		}
		block, tail := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) > 0 {
			return nil, failed("invalid_trust_roots")
		}
		cert, e := x509.ParseCertificate(block.Bytes)
		if e != nil {
			return nil, failed("invalid_trust_roots")
		}
		pool.AddCert(cert)
		count++
		rest = tail
	}
	if count == 0 {
		return nil, failed("invalid_trust_roots")
	}
	return pool, nil
}

// CanonicalArguments validates but intentionally returns the original bytes:
// whitespace and large integers remain part of the approved immutable intent.
func CanonicalArguments(c domain.ToolConnection, raw string) (string, error) {
	if len(raw) > c.Tool.MaxInputBytes || len(raw) > domain.MaxToolArgumentsBytes {
		return "", failed("arguments_too_large")
	}
	value, e := strictJSON([]byte(raw), 16)
	if e != nil {
		return "", e
	}
	if _, ok := value.(map[string]any); !ok {
		return "", failed("arguments_must_be_object")
	}
	if _, e = resolveSchema(c.Tool.InputSchemaJSON); e != nil {
		return "", e
	}
	schema, e := strictJSON([]byte(c.Tool.InputSchemaJSON), 16)
	if e != nil || !matchesSchema(schema, value) {
		return "", failed("arguments_schema_mismatch")
	}
	return raw, nil
}

func strictJSON(raw []byte, depth int) (any, error) {
	if !jsonwire.ValidUnicode(raw) || len(raw) == 0 {
		return nil, failed("invalid_json")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	count := 0
	var visit func(int) (any, error)
	visit = func(d int) (any, error) {
		count++
		if d > depth || count > 16384 {
			return nil, failed("json_limits")
		}
		tok, e := dec.Token()
		if e != nil {
			return nil, failed("invalid_json")
		}
		switch x := tok.(type) {
		case json.Delim:
			if x == '{' {
				m := map[string]any{}
				for dec.More() {
					k, e := dec.Token()
					key, ok := k.(string)
					if e != nil || !ok || !text(key, 4096) {
						return nil, failed("invalid_json")
					}
					if _, ok = m[key]; ok {
						return nil, failed("duplicate_json_field")
					}
					v, e := visit(d + 1)
					if e != nil {
						return nil, e
					}
					m[key] = v
				}
				end, e := dec.Token()
				if e != nil || end != json.Delim('}') {
					return nil, failed("invalid_json")
				}
				return m, nil
			}
			if x == '[' {
				a := []any{}
				for dec.More() {
					v, e := visit(d + 1)
					if e != nil {
						return nil, e
					}
					a = append(a, v)
				}
				end, e := dec.Token()
				if e != nil || end != json.Delim(']') {
					return nil, failed("invalid_json")
				}
				return a, nil
			}
			return nil, failed("invalid_json")
		case string:
			if !text(x, len(raw)) {
				return nil, failed("invalid_json")
			}
		case json.Number:
			if len(x) > 128 {
				return nil, failed("json_number_limit")
			}
			if at := strings.IndexAny(string(x), "eE"); at >= 0 {
				exp, e := strconv.Atoi(string(x)[at+1:])
				if e != nil || exp < -308 || exp > 308 {
					return nil, failed("json_number_limit")
				}
			}
		}
		return tok, nil
	}
	value, e := visit(0)
	if e != nil {
		return nil, e
	}
	if _, e = dec.Token(); e != io.EOF {
		return nil, failed("invalid_json")
	}
	return value, nil
}

// A deliberately bounded non-recursive schema profile. Unsupported assertions
// are rejected, rather than silently treated as annotations. No remote loader,
// references, defaults, combinatorial applicators or format coercion is used.
func resolveSchema(raw string) (*jsonschema.Resolved, error) {
	if len(raw) > 32768 {
		return nil, failed("schema_too_large")
	}
	v, e := strictJSON([]byte(raw), 16)
	if e != nil {
		return nil, e
	}
	m, ok := v.(map[string]any)
	if !ok || m["type"] != "object" {
		return nil, failed("schema_must_be_object")
	}
	if schemaShape(m) != nil {
		return nil, failed("unsupported_schema")
	}
	var s jsonschema.Schema
	if json.Unmarshal([]byte(raw), &s) != nil {
		return nil, failed("invalid_schema")
	}
	resolved, e := s.Resolve(nil)
	if e != nil {
		return nil, failed("invalid_schema")
	}
	return resolved, nil
}
func schemaShape(v any) error {
	if _, ok := v.(bool); ok {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return failed("invalid_schema")
	}
	if m["uniqueItems"] == true {
		n, ok := m["maxItems"].(json.Number)
		if !ok || !safeSchemaInteger(n) {
			return failed("unique_items_requires_bound")
		}
		limit, _ := strconv.Atoi(string(n))
		if limit < 0 || limit > 256 {
			return failed("unique_items_requires_bound")
		}
	}
	for k, v := range m {
		switch k {
		case "type", "title", "description", "$schema", "required":
		case "properties":
			p, ok := v.(map[string]any)
			if !ok || len(p) > 128 {
				return failed("invalid_schema")
			}
			for _, s := range p {
				if e := schemaShape(s); e != nil {
					return e
				}
			}
		case "items", "additionalProperties":
			if e := schemaShape(v); e != nil {
				return e
			}
		case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties":
			n, ok := v.(json.Number)
			if !ok {
				return failed("invalid_schema")
			}
			if !safeSchemaInteger(n) {
				return failed("schema_numeric_bound_requires_safe_integer")
			}
		case "enum", "const":
			if !schemaLiteralSafe(v) {
				return failed("invalid_schema_literal")
			}
		case "pattern", "uniqueItems":
		default:
			return failed("unsupported_schema_keyword")
		}
	}
	return nil
}
func schemaLiteralSafe(v any) bool {
	switch x := v.(type) {
	case json.Number:
		return safeSchemaInteger(x)
	case []any:
		for _, v := range x {
			if !schemaLiteralSafe(v) {
				return false
			}
		}
	case map[string]any:
		for _, v := range x {
			if !schemaLiteralSafe(v) {
				return false
			}
		}
	}
	return true
}

func safeSchemaInteger(n json.Number) bool {
	r, ok := new(big.Rat).SetString(string(n))
	if !ok || !r.IsInt() {
		return false
	}
	return new(big.Int).Abs(r.Num()).Cmp(big.NewInt(9007199254740991)) <= 0
}

// v0.4.3 treats json.Number as a string in its type assertion. Never compensate
// by converting user values to float64. This bounded profile evaluates data
// assertions with exact rationals; the library still validates the schema.
func matchesSchema(schema, value any) bool {
	if b, ok := schema.(bool); ok {
		return b
	}
	s, ok := schema.(map[string]any)
	if !ok {
		return false
	}
	kind := "null"
	var number *big.Rat
	switch v := value.(type) {
	case map[string]any:
		kind = "object"
	case []any:
		kind = "array"
	case string:
		kind = "string"
	case bool:
		kind = "boolean"
	case json.Number:
		var ok bool
		number, ok = new(big.Rat).SetString(string(v))
		if !ok {
			return false
		}
		kind = "number"
		if number.IsInt() {
			kind = "integer"
		}
	}
	if want, exists := s["type"]; exists {
		accept := func(v any) bool { return v == kind || (v == "number" && kind == "integer") }
		good := accept(want)
		if list, ok := want.([]any); ok {
			for _, item := range list {
				good = good || accept(item)
			}
		}
		if !good {
			return false
		}
	}
	if expected, exists := s["const"]; exists && !jsonschema.Equal(expected, value) {
		return false
	}
	if choices, exists := s["enum"]; exists {
		found := false
		for _, choice := range choices.([]any) {
			if jsonschema.Equal(choice, value) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if number != nil {
		for _, key := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"} {
			if v, exists := s[key]; exists {
				bound, ok := new(big.Rat).SetString(string(v.(json.Number)))
				if !ok {
					return false
				}
				cmp := number.Cmp(bound)
				if key == "minimum" && cmp < 0 || key == "maximum" && cmp > 0 || key == "exclusiveMinimum" && cmp <= 0 || key == "exclusiveMaximum" && cmp >= 0 {
					return false
				}
				if key == "multipleOf" && (bound.Sign() <= 0 || !new(big.Rat).Quo(number, bound).IsInt()) {
					return false
				}
			}
		}
	}
	bound := func(key string, n int, minimum bool) bool {
		v, ok := s[key]
		if !ok {
			return true
		}
		b, _ := new(big.Rat).SetString(string(v.(json.Number)))
		cmp := big.NewRat(int64(n), 1).Cmp(b)
		return minimum && cmp >= 0 || !minimum && cmp <= 0
	}
	switch v := value.(type) {
	case string:
		if !bound("minLength", utf8.RuneCountInString(v), true) || !bound("maxLength", utf8.RuneCountInString(v), false) {
			return false
		}
		if pattern, ok := s["pattern"]; ok {
			r, e := regexp.Compile(pattern.(string))
			if e != nil || !r.MatchString(v) {
				return false
			}
		}
	case []any:
		if !bound("minItems", len(v), true) || !bound("maxItems", len(v), false) {
			return false
		}
		if item, ok := s["items"]; ok {
			for _, entry := range v {
				if !matchesSchema(item, entry) {
					return false
				}
			}
		}
		if s["uniqueItems"] == true {
			for i := range v {
				for j := 0; j < i; j++ {
					if jsonschema.Equal(v[i], v[j]) {
						return false
					}
				}
			}
		}
	case map[string]any:
		if !bound("minProperties", len(v), true) || !bound("maxProperties", len(v), false) {
			return false
		}
		if required, ok := s["required"].([]any); ok {
			for _, entry := range required {
				if _, ok := v[entry.(string)]; !ok {
					return false
				}
			}
		}
		properties, _ := s["properties"].(map[string]any)
		for key, entry := range v {
			if property, ok := properties[key]; ok {
				if !matchesSchema(property, entry) {
					return false
				}
			} else if additional, ok := s["additionalProperties"]; ok && !matchesSchema(additional, entry) {
				return false
			}
		}
	}
	return true
}
