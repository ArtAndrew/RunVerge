// SPDX-License-Identifier: Elastic-2.0
package identity

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxProviderResponseBytes = 1024 * 1024

type endpointPolicy struct {
	origins       map[string]struct{}
	allowTestHTTP bool
}

func newProviderClient(cfg Config) (*http.Client, *http.Transport, endpointPolicy, error) {
	policy := endpointPolicy{origins: make(map[string]struct{}), allowTestHTTP: cfg.AllowLoopbackHTTPForTests}
	issuer, ok := checkedURL(cfg.IssuerURL, cfg.AllowLoopbackHTTPForTests)
	if !ok || issuer.RawQuery != "" || issuer.ForceQuery || strings.TrimSpace(cfg.IssuerURL) != cfg.IssuerURL {
		return nil, nil, policy, ErrInvalidConfig
	}
	policy.origins[origin(issuer)] = struct{}{}
	if len(cfg.AdditionalJWKSOrigins) > 8 {
		return nil, nil, policy, ErrInvalidConfig
	}
	for _, configured := range cfg.AdditionalJWKSOrigins {
		u, valid := checkedURL(configured, cfg.AllowLoopbackHTTPForTests)
		if !valid || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery {
			return nil, nil, policy, ErrInvalidConfig
		}
		policy.origins[origin(u)] = struct{}{}
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: cfg.RequestTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    cfg.RequestTimeout,
		ResponseHeaderTimeout:  cfg.RequestTimeout,
		IdleConnTimeout:        30 * time.Second,
		MaxConnsPerHost:        8,
		MaxIdleConns:           8,
		MaxIdleConnsPerHost:    2,
		MaxResponseHeaderBytes: 64 * 1024,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if cfg.RootCAs != nil {
		transport.TLSClientConfig.RootCAs = cfg.RootCAs.Clone()
	}
	client := &http.Client{
		Transport:     &restrictedTransport{base: transport, policy: policy},
		Timeout:       cfg.RequestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return ErrProviderUnavailable },
	}
	return client, transport, policy, nil
}

func checkedURL(raw string, allowTestHTTP bool) (*url.URL, bool) {
	if raw == "" || len(raw) > 2048 || !validText(raw, 2048, true) {
		return nil, false
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Opaque != "" || u.Hostname() == "" || u.User != nil || strings.Contains(raw, "#") {
		return nil, false
	}
	if u.Scheme == "https" {
		return u, true
	}
	// Literal IPs avoid relying on mutable DNS/hosts resolution for the test exception.
	ip := net.ParseIP(u.Hostname())
	return u, u.Scheme == "http" && allowTestHTTP && ip != nil && ip.IsLoopback()
}

func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return u.Scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

func (p endpointPolicy) allows(raw string) bool {
	u, ok := checkedURL(raw, p.allowTestHTTP)
	if !ok {
		return false
	}
	_, ok = p.origins[origin(u)]
	return ok
}

type restrictedTransport struct {
	base   *http.Transport
	policy endpointPolicy
}

func (t *restrictedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodGet || !t.policy.allows(request.URL.String()) ||
		request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
		return nil, ErrProviderUnavailable
	}
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	// Discovery and JWKS are small, public JSON documents. Bound both allocation
	// and body-read time; never propagate an upstream error body into our errors.
	if response.ContentLength > maxProviderResponseBytes {
		return nil, ErrProviderUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxProviderResponseBytes+1))
	if err != nil || len(body) > maxProviderResponseBytes {
		return nil, ErrProviderUnavailable
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}
