// SPDX-License-Identifier: Elastic-2.0
package identity

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderClientIgnoresProxyEnvironmentAndRejectsCredentials(t *testing.T) {
	var issuerHits, proxyHits atomic.Int64
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issuerHits.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer issuer.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	client, transport, _, err := newProviderClient(Config{
		IssuerURL: issuer.URL, ClientID: "test-client", RequestTimeout: time.Second,
		AllowLoopbackHTTPForTests: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	// Checking the hook matters: net/http otherwise bypasses proxies for loopback,
	// so a passing loopback request alone would not prove the deployment setting.
	if transport.Proxy != nil {
		t.Fatal("environment proxy callback enabled")
	}
	response, err := client.Get(issuer.URL)
	if err != nil {
		t.Fatal("provider request failed")
	}
	_ = response.Body.Close()
	if issuerHits.Load() != 1 || proxyHits.Load() != 0 {
		t.Fatal("request did not use direct provider connection")
	}
	const marker = "PRIVATE_HEADER_MARKER"
	for _, header := range []string{"Authorization", "Cookie"} {
		request, _ := http.NewRequest(http.MethodGet, issuer.URL, nil)
		request.Header.Set(header, marker)
		_, err := client.Do(request)
		if err == nil || strings.Contains(err.Error(), marker) {
			t.Fatal("unsafe credential forwarding or error")
		}
	}
	if issuerHits.Load() != 1 || proxyHits.Load() != 0 {
		t.Fatal("credential-bearing request reached network")
	}
}
