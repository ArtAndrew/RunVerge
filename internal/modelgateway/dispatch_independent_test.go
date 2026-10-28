// SPDX-License-Identifier: Elastic-2.0
package modelgateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync/atomic"
	"testing"
	"time"
)

const independentCompletion = `{"id":"independent-request","choices":[{"message":{"role":"assistant","content":"unverified answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`

func independentTLSGateway(t *testing.T, server *httptest.Server) *Gateway {
	t.Helper()
	cfg := fixtureConfig(server.URL)
	cfg.Timeout = time.Second
	g, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	g.client.Transport.(*http.Transport).TLSClientConfig.RootCAs = pool
	return g
}
func TestIndependentGatewayExpiredOrMissingPermitDoesNotDial(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	g, e := New(fixtureConfig(server.URL))
	if e != nil {
		t.Fatal(e)
	}
	for _, deadline := range []time.Time{{}, time.Now().Add(-time.Second)} {
		req := fixtureRequest()
		req.DispatchDeadline = deadline
		_, e := g.Execute(context.Background(), req)
		var oe *OutcomeError
		if !errors.As(e, &oe) || !oe.DefinitiveRejection {
			t.Fatalf("expired admission not rejected: %v", e)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("expired admission reached provider")
	}
}
func TestIndependentGatewayPostConnectPauseCannotWriteExpiredPermit(t *testing.T) {
	for _, secure := range []bool{false, true} {
		name := "http"
		if secure {
			name = "tls"
		}
		t.Run(name, func(t *testing.T) {
			var calls, connections atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, independentCompletion)
			}))
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			if secure {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			var g *Gateway
			if secure {
				g = independentTLSGateway(t, server)
			} else {
				var e error
				cfg := fixtureConfig(server.URL)
				cfg.Timeout = time.Second
				g, e = New(cfg)
				if e != nil {
					t.Fatal(e)
				}
			}
			req := fixtureRequest()
			req.DispatchDeadline = time.Now().Add(100 * time.Millisecond)
			ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) {
				connections.Add(1)
				time.Sleep(time.Until(req.DispatchDeadline) + 40*time.Millisecond)
			}})
			_, e := g.Execute(ctx, req)
			if e == nil {
				t.Fatal("expired application write succeeded")
			}
			if connections.Load() != 1 || calls.Load() != 0 {
				t.Fatalf("post-connect pause: connections=%d inference=%d", connections.Load(), calls.Load())
			}
		})
	}
}
func TestIndependentGatewayStalledTLSCannotConsumeInferencePermit(t *testing.T) {
	var calls atomic.Int32
	handshake := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { close(handshake); <-release; return nil, nil }}
	server.StartTLS()
	defer server.Close()
	defer close(release)
	g := independentTLSGateway(t, server)
	req := fixtureRequest()
	req.DispatchDeadline = time.Now().Add(100 * time.Millisecond)
	done := make(chan error, 1)
	go func() { _, e := g.Execute(context.Background(), req); done <- e }()
	select {
	case <-handshake:
	case <-time.After(time.Second):
		t.Fatal("TLS fixture never received ClientHello")
	}
	select {
	case e := <-done:
		var oe *OutcomeError
		if !errors.As(e, &oe) || !oe.DefinitiveRejection {
			t.Fatalf("stalled TLS classified as request dispatch: %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("TLS handshake ignored permit expiry")
	}
	if calls.Load() != 0 {
		t.Fatal("stalled TLS produced inference request")
	}
}
func TestIndependentGatewayStartedRequestKeepsSeparateResponseTimeout(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(180 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, independentCompletion)
	}))
	defer server.Close()
	cfg := fixtureConfig(server.URL)
	cfg.Timeout = time.Second
	g, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	req := fixtureRequest()
	req.DispatchDeadline = time.Now().Add(100 * time.Millisecond)
	result, e := g.Execute(context.Background(), req)
	if e != nil || result.Text != "unverified answer" || calls.Load() != 1 {
		t.Fatalf("dispatch-start deadline incorrectly cancelled admitted response: %+v %v", result, e)
	}
}
