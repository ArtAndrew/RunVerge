// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"

	"agentplatform.local/platform/internal/jsonwire"
)

var ErrRecoveryHistoryJSON = errors.New("invalid historical recovery JSON")

// decodeRecoveryHistoryJSON decodes a known historical DTO without adding
// defaults or rewriting omitted legacy fields. Present struct keys must match
// their exact JSON spelling; unknown/case aliases and duplicate keys fail.
// Individual validators still check required fields and immutable bindings.
func decodeRecoveryHistoryJSON(raw []byte, out any, maximum int) error {
	value := reflect.ValueOf(out)
	if maximum < 1 || len(raw) == 0 || len(raw) > maximum || !jsonwire.ValidUnicode(raw) || value.Kind() != reflect.Pointer || value.IsNil() {
		return ErrRecoveryHistoryJSON
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if recoveryHistoryJSONValue(d, 0) != nil {
		return ErrRecoveryHistoryJSON
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrRecoveryHistoryJSON
	}
	if recoveryHistoryJSONKeys(raw, value.Type().Elem(), 0) != nil {
		return ErrRecoveryHistoryJSON
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	d.UseNumber()
	if d.Decode(out) != nil {
		return ErrRecoveryHistoryJSON
	}
	return nil
}

func recoveryHistoryJSONValue(d *json.Decoder, depth int) error {
	if depth > 64 {
		return ErrRecoveryHistoryJSON
	}
	token, err := d.Token()
	if err != nil {
		return ErrRecoveryHistoryJSON
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil // Null and scalar values retain their original DTO semantics.
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			token, err := d.Token()
			key, ok := token.(string)
			if err != nil || !ok || seen[key] {
				return ErrRecoveryHistoryJSON
			}
			seen[key] = true
			if recoveryHistoryJSONValue(d, depth+1) != nil {
				return ErrRecoveryHistoryJSON
			}
		}
		token, err = d.Token()
		if err != nil || token != json.Delim('}') {
			return ErrRecoveryHistoryJSON
		}
	case '[':
		for d.More() {
			if recoveryHistoryJSONValue(d, depth+1) != nil {
				return ErrRecoveryHistoryJSON
			}
		}
		token, err = d.Token()
		if err != nil || token != json.Delim(']') {
			return ErrRecoveryHistoryJSON
		}
	default:
		return ErrRecoveryHistoryJSON
	}
	return nil
}

var recoveryJSONUnmarshaler = reflect.TypeFor[json.Unmarshaler]()

func recoveryHistoryJSONKeys(raw []byte, kind reflect.Type, depth int) error {
	if depth > 64 {
		return ErrRecoveryHistoryJSON
	}
	if kind.Implements(recoveryJSONUnmarshaler) || (kind.Kind() != reflect.Pointer && reflect.PointerTo(kind).Implements(recoveryJSONUnmarshaler)) {
		return nil // time.Time and RawMessage have their own wire representations.
	}
	if kind.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil
		}
		return recoveryHistoryJSONKeys(raw, kind.Elem(), depth+1)
	}
	switch kind.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil || object == nil {
			return ErrRecoveryHistoryJSON
		}
		fields := map[string]reflect.StructField{}
		visited := map[reflect.Type]bool{}
		var collect func(reflect.Type) error
		collect = func(t reflect.Type) error {
			if visited[t] {
				return ErrRecoveryHistoryJSON
			}
			visited[t] = true
			for i := 0; i < t.NumField(); i++ {
				field := t.Field(i)
				if field.PkgPath != "" {
					continue
				}
				name := strings.Split(field.Tag.Get("json"), ",")[0]
				if name == "-" {
					continue
				}
				if field.Anonymous && name == "" {
					embedded := field.Type
					if embedded.Kind() == reflect.Pointer {
						embedded = embedded.Elem()
					}
					if embedded.Kind() != reflect.Struct || collect(embedded) != nil {
						return ErrRecoveryHistoryJSON
					}
					continue
				}
				if name == "" {
					name = field.Name
				}
				if _, exists := fields[name]; exists {
					return ErrRecoveryHistoryJSON
				}
				fields[name] = field
			}
			return nil
		}
		if collect(kind) != nil {
			return ErrRecoveryHistoryJSON
		}
		for name, value := range object {
			field, exists := fields[name]
			if !exists {
				return ErrRecoveryHistoryJSON
			}
			if strings.Contains(field.Tag.Get("json"), ",string") {
				var text string
				if json.Unmarshal(value, &text) != nil {
					return ErrRecoveryHistoryJSON
				}
				t := field.Type
				if t.Kind() == reflect.Pointer {
					t = t.Elem()
				}
				switch t.Kind() {
				case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
					n, err := strconv.ParseInt(text, 10, t.Bits())
					if err != nil || strconv.FormatInt(n, 10) != text {
						return ErrRecoveryHistoryJSON
					}
				case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
					n, err := strconv.ParseUint(text, 10, t.Bits())
					if err != nil || strconv.FormatUint(n, 10) != text {
						return ErrRecoveryHistoryJSON
					}
				}
			}
			if recoveryHistoryJSONKeys(value, field.Type, depth+1) != nil {
				return ErrRecoveryHistoryJSON
			}
		}
	case reflect.Slice, reflect.Array:
		if kind.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return ErrRecoveryHistoryJSON
		}
		for _, value := range values {
			if recoveryHistoryJSONKeys(value, kind.Elem(), depth+1) != nil {
				return ErrRecoveryHistoryJSON
			}
		}
	case reflect.Map:
		if kind.Key().Kind() != reflect.String {
			return ErrRecoveryHistoryJSON
		}
		var values map[string]json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return ErrRecoveryHistoryJSON
		}
		for _, value := range values {
			if recoveryHistoryJSONKeys(value, kind.Elem(), depth+1) != nil {
				return ErrRecoveryHistoryJSON
			}
		}
	}
	return nil
}
