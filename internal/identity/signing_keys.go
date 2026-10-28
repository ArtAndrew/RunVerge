// SPDX-License-Identifier: Elastic-2.0
package identity

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// signingKeyTransport applies JWK metadata restrictions before go-oidc caches
// keys and delegates signature verification to go-jose. Each instance belongs
// to exactly one administrator-allowed signature algorithm. Its base transport
// still enforces origin, TLS, size, redirect, proxy and timeout restrictions.
type signingKeyTransport struct {
	base                      http.RoundTripper
	algorithm                 string
	allowUnspecifiedAlgorithm bool
}

func (t *signingKeyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil || response.StatusCode != http.StatusOK {
		return response, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxProviderResponseBytes+1))
	if err != nil || len(body) > maxProviderResponseBytes {
		return nil, ErrProviderUnavailable
	}
	var document struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if json.Unmarshal(body, &document) != nil || document.Keys == nil {
		return nil, ErrProviderUnavailable
	}
	accepted := make([]json.RawMessage, 0, len(document.Keys))
	for _, raw := range document.Keys {
		if t.permitsKey(raw) {
			accepted = append(accepted, raw)
		}
	}
	document.Keys = accepted
	body, err = json.Marshal(document)
	if err != nil {
		return nil, ErrProviderUnavailable
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	response.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return response, nil
}

func (t *signingKeyTransport) permitsKey(raw json.RawMessage) bool {
	var key map[string]json.RawMessage
	if json.Unmarshal(raw, &key) != nil || key == nil {
		return false
	}
	if value, present := key["use"]; present {
		var use string
		if json.Unmarshal(value, &use) != nil || use != "sig" {
			return false
		}
	}
	if value, present := key["alg"]; present {
		var algorithm string
		if json.Unmarshal(value, &algorithm) != nil || algorithm != t.algorithm {
			return false
		}
	} else if !t.allowUnspecifiedAlgorithm {
		// Without alg, a single configured algorithm supplies the binding.
		// An ambiguous key must not be reused under multiple algorithms.
		return false
	}
	if value, present := key["key_ops"]; present {
		var operations []string
		if json.Unmarshal(value, &operations) != nil || len(operations) == 0 {
			return false
		}
		seen := make(map[string]bool, len(operations))
		for _, operation := range operations {
			if seen[operation] || (operation != "verify" && operation != "sign") {
				return false
			}
			seen[operation] = true
		}
		if !seen["verify"] {
			return false
		}
	}
	return true
}
