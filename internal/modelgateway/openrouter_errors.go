// SPDX-License-Identifier: Elastic-2.0
package modelgateway

import (
	"bytes"
	"encoding/json"
	"io"
)

// Only a known structural discriminator may leave this boundary. Provider
// messages, raw errors, headers and unknown metadata are never retained. This
// classification changes diagnostics only, never dispatch or billing policy.
func openRouterLimitCode(source io.Reader) string {
	const limit = 16 << 10
	body, err := io.ReadAll(io.LimitReader(source, limit+1))
	if err != nil || len(body) > limit {
		return ""
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	value, err := decodeValueBounded(decoder, 0, 8)
	if err != nil {
		return ""
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ""
	}
	root, _ := value.(map[string]any)
	failure, _ := root["error"].(map[string]any)
	metadata, _ := failure["metadata"].(map[string]any)
	if metadata["limit_source"] == "upstream_provider_shared_pool" {
		return "provider_shared_pool_limited"
	}
	return ""
}
