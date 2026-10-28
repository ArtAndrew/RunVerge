// SPDX-License-Identifier: Elastic-2.0
package modelgateway

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

var errDispatchExpired = errors.New("dispatch-start deadline expired")

// startBoundConnection wraps the APPLICATION connection (after TLS), so a
// ClientHello cannot consume the inference-start permit. The first application
// write has an absolute socket deadline; resuming after expiry cannot create a
// fresh window. Subsequent request bytes remain bounded by request cancellation.
type startBoundConnection struct {
	net.Conn
	mu       sync.Mutex
	deadline time.Time
	started  bool
}

func (c *startBoundConnection) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return c.Conn.Write(p)
	}
	if !time.Now().Before(c.deadline) {
		return 0, errDispatchExpired
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

func (g *Gateway) dispatchClient(deadline time.Time) (*http.Client, func()) {
	base := g.client.Transport.(*http.Transport)
	transport := base.Clone()
	dialer := &net.Dialer{Timeout: g.config.Timeout}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if !time.Now().Before(deadline) {
			return nil, errDispatchExpired
		}
		bounded, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		connection, e := dialer.DialContext(bounded, network, address)
		if e != nil {
			return nil, e
		}
		if !time.Now().Before(deadline) {
			connection.Close()
			return nil, errDispatchExpired
		}
		return &startBoundConnection{Conn: connection, deadline: deadline}, nil
	}
	transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if !time.Now().Before(deadline) {
			return nil, errDispatchExpired
		}
		bounded, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: base.TLSClientConfig.Clone()}
		connection, e := tlsDialer.DialContext(bounded, network, address)
		if e != nil {
			return nil, e
		}
		if !time.Now().Before(deadline) {
			connection.Close()
			return nil, errDispatchExpired
		}
		return &startBoundConnection{Conn: connection, deadline: deadline}, nil
	}
	client := *g.client
	client.Transport = transport
	return &client, transport.CloseIdleConnections
}
