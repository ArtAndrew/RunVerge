// SPDX-License-Identifier: Elastic-2.0
package webhookruntime

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// send returns only a response status or zero uncertainty. All transport text,
// response prose and headers are discarded; no credential/body echo is logged.
func (r *Runtime) send(ctx context.Context, body []byte, h SignatureHeaders, deadline time.Time) int {
	u, _ := url.Parse(r.profile.Endpoint)
	port := u.Port()
	if port == "" {
		port = "443"
	}
	address := net.JoinHostPort(u.Hostname(), port)
	timeout := time.Duration(r.profile.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var roots *x509.CertPool
	if r.profile.TrustedCAPEM != "" {
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(r.profile.TrustedCAPEM)) {
			return 0
		}
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	tr := &http.Transport{Proxy: nil, Protocols: protocols, DisableKeepAlives: true, DisableCompression: true, MaxResponseHeaderBytes: 16384, TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout}
	tr.DialTLSContext = func(call context.Context, network, target string) (net.Conn, error) {
		if target != address || !time.Now().Before(deadline) {
			return nil, failed("dispatch_expired")
		}
		bounded, stop := context.WithDeadline(call, deadline)
		defer stop()
		addresses, err := net.DefaultResolver.LookupNetIP(bounded, "ip", u.Hostname())
		if err != nil || len(addresses) == 0 {
			return nil, failed("endpoint_unavailable")
		}
		for _, ip := range addresses {
			if !AllowedReceiverAddress(ip, r.profile.AllowPrivateNetwork) {
				return nil, failed("network_policy")
			}
		}
		dialer := &net.Dialer{Timeout: timeout}
		conn, err := dialer.DialContext(bounded, network, net.JoinHostPort(addresses[0].String(), port))
		if err != nil {
			return nil, failed("endpoint_unavailable")
		}
		secured := tls.Client(conn, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname(), RootCAs: roots, NextProtos: []string{"http/1.1"}})
		if err = secured.HandshakeContext(bounded); err != nil {
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
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.profile.Endpoint, io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		return 0
	}
	request.ContentLength = int64(len(body))
	request.GetBody = nil
	request.Close = true
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("Webhook-Key-ID", h.KeyID)
	request.Header.Set("Webhook-ID", h.DeliveryID)
	request.Header.Set("Webhook-Attempt-ID", h.AttemptID)
	request.Header.Set("Webhook-Timestamp", h.Timestamp)
	request.Header.Set("Webhook-Signature", h.Signature)
	response, err := client.Do(request)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	if response.StatusCode < 100 || response.StatusCode > 599 {
		return 0
	}
	return response.StatusCode
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
	if err := c.Conn.SetWriteDeadline(c.deadline); err != nil {
		return 0, err
	}
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.started = true
		_ = c.Conn.SetWriteDeadline(time.Time{})
	}
	return n, err
}
