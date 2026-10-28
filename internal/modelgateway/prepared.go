// SPDX-License-Identifier: Elastic-2.0
package modelgateway

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"agentplatform.local/platform/internal/domain"
)

var errPreparedUnavailable = errors.New("prepared connection unavailable")

// PreparedConnection is opaque and bound to one gateway. Copies share the same
// consumption and closure state. Close unused handles; the preparation context
// also closes the socket when cancelled or timed out.
type PreparedConnection struct {
	owner *Gateway
	state *preparedState
}

type preparedState struct {
	connection net.Conn
	used       atomic.Bool
	closed     sync.Once
	stop       func() bool
	cancel     context.CancelFunc
}

func (*PreparedConnection) String() string   { return "[prepared model connection]" }
func (*PreparedConnection) GoString() string { return "[prepared model connection]" }

func (p *PreparedConnection) Close() {
	if p == nil || p.state == nil {
		return
	}
	p.state.used.Store(true)
	p.state.closed.Do(func() {
		if p.state.stop != nil {
			p.state.stop()
		}
		p.state.cancel()
		_ = p.state.connection.Close()
	})
}

func (p *PreparedConnection) claim(g *Gateway) bool {
	return p != nil && g != nil && p.owner == g && p.state != nil && p.state.used.CompareAndSwap(false, true)
}

// PrepareConnection sends only DNS/TCP/TLS traffic to the fixed OpenRouter
// endpoint. It sends no HTTP request, authorization header, or prompt. Callers
// must reserve budget first and recheck authorization after this returns.
func (g *Gateway) PrepareConnection(ctx context.Context) (*PreparedConnection, error) {
	if g == nil || g.client == nil || ctx == nil || g.config.WireProfile != domain.ModelWireOpenRouterTextV1 || g.config.Endpoint != OpenRouterEndpoint {
		return nil, rejected("invalid_prepared_connection")
	}
	if ctx.Err() != nil {
		return nil, rejected("cancelled_before_send")
	}
	bounded, cancel := context.WithTimeout(ctx, g.config.Timeout)
	base := g.client.Transport.(*http.Transport)
	endpoint, _ := url.Parse(g.config.Endpoint)
	address := net.JoinHostPort(endpoint.Hostname(), "443")
	connection, err := base.DialContext(bounded, "tcp", address)
	if err != nil {
		cancel()
		return nil, rejected("connection_not_established")
	}
	config := base.TLSClientConfig.Clone()
	if config.ServerName == "" {
		config.ServerName = endpoint.Hostname()
	}
	config.NextProtos = []string{"http/1.1"}
	secure := tls.Client(connection, config)
	if err := secure.HandshakeContext(bounded); err != nil || bounded.Err() != nil {
		cancel()
		_ = secure.Close()
		return nil, rejected("connection_not_established")
	}
	state := &preparedState{connection: secure, cancel: cancel}
	// Capture only state, not the returned handle. The callback must not read
	// stop while it is being assigned when cancellation races preparation.
	state.stop = context.AfterFunc(bounded, func() {
		state.used.Store(true)
		_ = secure.Close()
	})
	return &PreparedConnection{owner: g, state: state}, nil
}

func (g *Gateway) preparedDispatchClient(deadline time.Time, p *PreparedConnection) (*http.Client, func()) {
	transport := g.client.Transport.(*http.Transport).Clone()
	// No new socket may be opened, including an internal transport retry.
	transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errPreparedUnavailable
	}
	var delivered atomic.Bool
	transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if ctx.Err() != nil || address != "openrouter.ai:443" || network != "tcp" || !delivered.CompareAndSwap(false, true) {
			return nil, errPreparedUnavailable
		}
		if !time.Now().Before(deadline) {
			return nil, errDispatchExpired
		}
		return &startBoundConnection{Conn: p.state.connection, deadline: deadline}, nil
	}
	client := *g.client
	client.Transport = transport
	return &client, transport.CloseIdleConnections
}
