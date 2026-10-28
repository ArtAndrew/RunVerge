// SPDX-License-Identifier: Elastic-2.0
package identity

import (
	"bytes"
	"io"
	"mime"
	"net/http"
)

// Token exchange permits only POST to the exact administrator-validated URL.
// The cloned base retains verified TLS, disabled proxy and bounded connections.
type loginTokenTransport struct {
	base     *http.Transport
	endpoint string
}

func (t *loginTokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodPost || r.URL.String() != t.endpoint || r.Header.Get("Cookie") != "" {
		return nil, ErrLoginRejected
	}
	response, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.ContentLength > maxProviderResponseBytes {
		return nil, ErrLoginRejected
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxProviderResponseBytes+1))
	if err != nil || len(body) > maxProviderResponseBytes {
		return nil, ErrLoginRejected
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			return nil, ErrLoginRejected
		}
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}
