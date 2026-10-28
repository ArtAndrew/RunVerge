// SPDX-License-Identifier: Elastic-2.0
package jsonwire

import "unicode/utf8"

// ValidUnicode rejects inputs encoding/json would silently repair to U+FFFD.
// It checks Unicode scalar encoding only; callers still validate JSON grammar,
// duplicate fields, depth and the actual command schema separately.
func ValidUnicode(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	inside := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			inside = !inside
			continue
		}
		if !inside || raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		value, ok := hex4(raw[i+1 : i+5])
		if !ok {
			return false
		}
		i += 4
		if value >= 0xdc00 && value <= 0xdfff {
			return false
		}
		if value < 0xd800 || value > 0xdbff {
			continue
		}
		if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		low, ok := hex4(raw[i+3 : i+7])
		if !ok || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return !inside
}

func hex4(raw []byte) (uint16, bool) {
	var result uint16
	for _, value := range raw {
		result <<= 4
		switch {
		case value >= '0' && value <= '9':
			result |= uint16(value - '0')
		case value >= 'a' && value <= 'f':
			result |= uint16(value - 'a' + 10)
		case value >= 'A' && value <= 'F':
			result |= uint16(value - 'A' + 10)
		default:
			return 0, false
		}
	}
	return result, true
}
