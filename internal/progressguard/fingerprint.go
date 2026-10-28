// SPDX-License-Identifier: Elastic-2.0
// Package progressguard provides bounded deterministic signals of repetition.
// It does not establish whether a model output or an external claim is true.
package progressguard

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"sort"
	"strconv"
	"strings"

	"agentplatform.local/platform/internal/jsonwire"
)

const (
	MaxArgumentsBytes = 32768
	MaxJSONDepth      = 32
	MaxJSONNodes      = 4096
	MaxNumberBytes    = 1024
)

var (
	ErrInvalidJSON  = errors.New("progressguard: invalid arguments JSON")
	ErrComplexity   = errors.New("progressguard: input exceeds bounded work limits")
	ErrInvalidInput = errors.New("progressguard: invalid input")
)

type objectField struct {
	key   string
	value any
}
type jsonObject []objectField
type jsonArray []any
type decimalNumber string

// FingerprintArguments is a semantic, versioned JSON-object fingerprint. It
// equates object key order, JSON whitespace, escape spellings and exact decimal
// spellings (1, 1.0, 1e0), while preserving array order and arbitrary integers
// within the fixed lexical bounds. It does not normalize Unicode, trim string
// values, coerce types, apply defaults, or modify approved argument bytes.
//
// Bounds limit total work and allocation. Numeric exponents are represented as
// compact signed decimal exponents, never expanded or passed through floats.
func FingerprintArguments(ctx context.Context, raw string) (string, error) {
	value, err := readArguments(ctx, raw)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	writeFrame(h, "progressguard/json/v1")
	if err = hashValue(ctx, h, value); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// IntentFingerprint uses stable tool identity and semantic arguments. Runtime
// call IDs, operation IDs, attempts, profile versions and grant IDs are excluded
// so that a retry or profile refresh cannot hide a repeated business intent.
// The exact installed profile and approved bytes remain separate authorization
// and dispatch invariants enforced by the caller.
func IntentFingerprint(ctx context.Context, in Intent) (string, error) {
	if !validID(in.ToolID) || !validEffect(in.Effect) {
		return "", ErrInvalidInput
	}
	arguments, err := FingerprintArguments(ctx, in.ArgumentsJSON)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	writeFrame(h, "progressguard/intent/v1")
	writeFrame(h, in.ToolID)
	writeFrame(h, arguments)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func readArguments(ctx context.Context, raw string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(raw) > MaxArgumentsBytes {
		return nil, ErrComplexity
	}
	if len(raw) == 0 || !jsonwire.ValidUnicode([]byte(raw)) {
		return nil, ErrInvalidJSON
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	count := 0
	var visit func(int) (any, error)
	visit = func(depth int) (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count++
		if depth > MaxJSONDepth || count > MaxJSONNodes {
			return nil, ErrComplexity
		}
		token, err := dec.Token()
		if err != nil {
			return nil, ErrInvalidJSON
		}
		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{':
				fields := jsonObject{}
				seen := map[string]bool{}
				for dec.More() {
					keyToken, err := dec.Token()
					key, ok := keyToken.(string)
					if err != nil || !ok || seen[key] {
						return nil, ErrInvalidJSON
					}
					seen[key] = true
					child, err := visit(depth + 1)
					if err != nil {
						return nil, err
					}
					fields = append(fields, objectField{key: key, value: child})
				}
				if closing, err := dec.Token(); err != nil || closing != json.Delim('}') {
					return nil, ErrInvalidJSON
				}
				sort.Slice(fields, func(i, j int) bool { return fields[i].key < fields[j].key })
				return fields, nil
			case '[':
				items := jsonArray{}
				for dec.More() {
					child, err := visit(depth + 1)
					if err != nil {
						return nil, err
					}
					items = append(items, child)
				}
				if closing, err := dec.Token(); err != nil || closing != json.Delim(']') {
					return nil, ErrInvalidJSON
				}
				return items, nil
			default:
				return nil, ErrInvalidJSON
			}
		case json.Number:
			normalized, err := normalizeDecimal(string(value))
			return decimalNumber(normalized), err
		case string, bool, nil:
			return value, nil
		default:
			return nil, ErrInvalidJSON
		}
	}
	value, err := visit(0)
	if err != nil {
		return nil, err
	}
	if _, ok := value.(jsonObject); !ok {
		return nil, ErrInvalidJSON
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, ErrInvalidJSON
	}
	return value, nil
}

// normalizeDecimal receives an already grammar-validated JSON numeric token.
// Its compact coefficient/exponent representation avoids exponent expansion.
func normalizeDecimal(raw string) (string, error) {
	if len(raw) > MaxNumberBytes {
		return "", ErrComplexity
	}
	negative := strings.HasPrefix(raw, "-")
	if negative {
		raw = raw[1:]
	}
	exponent := int64(0)
	if index := strings.IndexAny(raw, "eE"); index >= 0 {
		var err error
		exponent, err = strconv.ParseInt(raw[index+1:], 10, 32)
		if err != nil {
			return "", ErrComplexity
		}
		raw = raw[:index]
	}
	if index := strings.IndexByte(raw, '.'); index >= 0 {
		exponent -= int64(len(raw) - index - 1)
		raw = raw[:index] + raw[index+1:]
	}
	raw = strings.TrimLeft(raw, "0")
	if raw == "" {
		return "0", nil
	}
	coefficient := strings.TrimRight(raw, "0")
	exponent += int64(len(raw) - len(coefficient))
	if negative {
		coefficient = "-" + coefficient
	}
	return coefficient + "e" + strconv.FormatInt(exponent, 10), nil
}

func writeFrame(h hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = io.WriteString(h, value)
}

func hashValue(ctx context.Context, h hash.Hash, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch value := value.(type) {
	case jsonObject:
		writeFrame(h, "object")
		writeFrame(h, strconv.Itoa(len(value)))
		for _, field := range value {
			writeFrame(h, field.key)
			if err := hashValue(ctx, h, field.value); err != nil {
				return err
			}
		}
	case jsonArray:
		writeFrame(h, "array")
		writeFrame(h, strconv.Itoa(len(value)))
		for _, child := range value {
			if err := hashValue(ctx, h, child); err != nil {
				return err
			}
		}
	case decimalNumber:
		writeFrame(h, "number")
		writeFrame(h, string(value))
	case string:
		writeFrame(h, "string")
		writeFrame(h, value)
	case bool:
		writeFrame(h, "boolean")
		writeFrame(h, strconv.FormatBool(value))
	case nil:
		writeFrame(h, "null")
	default:
		return ErrInvalidJSON
	}
	return nil
}
