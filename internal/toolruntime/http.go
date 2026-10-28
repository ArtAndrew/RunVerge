// SPDX-License-Identifier: Elastic-2.0
package toolruntime

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

func (r *Runtime) httpExecute(ctx context.Context, payload []byte, key string, deadline time.Time) ([]byte, error) {
	c := r.config.Connection
	u, _ := url.Parse(c.Endpoint)
	port := u.Port()
	if port == "" {
		port = "443"
	}
	address := net.JoinHostPort(u.Hostname(), port)
	timeout := time.Duration(c.Tool.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	tr := &http.Transport{Proxy: nil, Protocols: protocols, DisableKeepAlives: true, DisableCompression: true, MaxResponseHeaderBytes: 16384, TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout}
	tr.DialTLSContext = func(call context.Context, network, target string) (net.Conn, error) {
		if target != address || !time.Now().Before(deadline) {
			return nil, failed("dispatch_expired")
		}
		bounded, stop := context.WithDeadline(call, deadline)
		defer stop()
		addresses, e := net.DefaultResolver.LookupNetIP(bounded, "ip", u.Hostname())
		if e != nil || len(addresses) == 0 {
			return nil, failed("endpoint_unavailable")
		}
		for _, ip := range addresses {
			if !allowedAddress(ip, c.AllowPrivateNetwork) {
				return nil, failed("network_policy")
			}
		}
		dialer := &net.Dialer{Timeout: timeout}
		conn, e := dialer.DialContext(bounded, network, net.JoinHostPort(addresses[0].String(), port))
		if e != nil {
			return nil, failed("endpoint_unavailable")
		}
		secured := tls.Client(conn, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname(), RootCAs: r.config.TrustedRoots})
		if e = secured.HandshakeContext(bounded); e != nil {
			conn.Close()
			return nil, failed("tls_failed")
		}
		if !time.Now().Before(deadline) {
			secured.Close()
			return nil, failed("dispatch_expired")
		}
		return &startBoundConnection{Conn: secured, deadline: deadline}, nil
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, e := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, io.NopCloser(bytes.NewReader(payload)))
	if e != nil {
		return nil, failed("invalid_request")
	}
	request.ContentLength = int64(len(payload))
	request.GetBody = nil
	request.Close = true
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("Idempotency-Key", key)
	if r.config.Credential != "" {
		request.Header.Set("Authorization", "Bearer "+r.config.Credential)
	}
	response, e := client.Do(request)
	if e != nil {
		return nil, failed("transport_outcome_unknown")
	}
	defer response.Body.Close()
	media, _, e := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if e != nil || media != "application/json" || response.StatusCode < 200 || response.StatusCode > 299 || (response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity") {
		return nil, failed("response_unknown")
	}
	// JSON may escape one decoded byte as six wire bytes (for example \u003c).
	limit := int64(6*c.Tool.MaxOutputBytes + 8192)
	if response.ContentLength > limit {
		return nil, failed("response_limit")
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if e != nil || int64(len(raw)) > limit {
		return nil, failed("response_limit")
	}
	return raw, nil
}
func allowedAddress(ip netip.Addr, private bool) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip.IsPrivate() || ip.IsLoopback() {
		return private
	}
	return ip.IsGlobalUnicast()
}

type startBoundConnection struct {
	net.Conn
	deadline time.Time
	started  bool
}

func (c *startBoundConnection) Write(p []byte) (int, error) {
	if c.started {
		return c.Conn.Write(p)
	}
	if !time.Now().Before(c.deadline) {
		return 0, failed("dispatch_expired")
	}
	if e := c.Conn.SetWriteDeadline(c.deadline); e != nil {
		return 0, e
	}
	n, e := c.Conn.Write(p)
	if n > 0 {
		c.started = true
		_ = c.Conn.SetWriteDeadline(time.Time{})
	}
	return n, e
}
