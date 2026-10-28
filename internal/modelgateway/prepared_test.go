// SPDX-License-Identifier: Elastic-2.0
package modelgateway

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func preparedFixture(t *testing.T, delay time.Duration) (*Gateway, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"gen-fixture","model":"example/fixed-model","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"Hello"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`))
	}))
	t.Cleanup(server.Close)
	config := routerConfig()
	config.Timeout = 5 * time.Second
	g := gatewayFor(t, config)
	base := g.client.Transport.(*http.Transport)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	base.TLSClientConfig.RootCAs, base.TLSClientConfig.ServerName = roots, "example.com"
	base.DialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
		if address != "openrouter.ai:443" {
			t.Error("unexpected endpoint")
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	return g, &requests
}

func TestPreparedTLSPrecedesFreshPermitAndCannotBeReplayed(t *testing.T) {
	g, requests := preparedFixture(t, 2100*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	prepared, err := g.PrepareConnection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if requests.Load() != 0 {
		t.Fatal("preparation sent HTTP")
	}
	copyHandle := *prepared
	if _, err := g.ExecutePrepared(ctx, fixtureRequest(), prepared); err != nil {
		t.Fatal(err)
	}
	_, err = g.ExecutePrepared(ctx, fixtureRequest(), &copyHandle)
	assertOutcome(t, err, "invalid_prepared_connection", false)
	if requests.Load() != 1 {
		t.Fatal("unexpected HTTP replay")
	}
}

func TestPreparedTLSRejectsStalePermitAndWrongOwner(t *testing.T) {
	g, requests := preparedFixture(t, 0)
	prepared, err := g.PrepareConnection(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	other := gatewayFor(t, routerConfig())
	_, err = other.ExecutePrepared(context.Background(), fixtureRequest(), prepared)
	assertOutcome(t, err, "invalid_prepared_connection", false)
	request := fixtureRequest()
	request.DispatchDeadline = time.Now().Add(-time.Millisecond)
	_, err = g.ExecutePrepared(context.Background(), request, prepared)
	assertOutcome(t, err, "dispatch_permit_expired", false)
	if requests.Load() != 0 {
		t.Fatal("HTTP sent without fresh authority")
	}
}

func TestPreparedConnectionCancellationPreventsHTTP(t *testing.T) {
	g, requests := preparedFixture(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	prepared, err := g.PrepareConnection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	cancel()
	_, err = g.ExecutePrepared(ctx, fixtureRequest(), prepared)
	if err == nil || requests.Load() != 0 {
		t.Fatal("cancelled socket sent HTTP")
	}
}
