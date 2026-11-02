// SPDX-License-Identifier: Elastic-2.0
package modelgateway

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const usageFixtureJSON = `{"data":{"id":"gen-fixture-123","model":"example/model","total_cost":0.000003,"is_byok":false,"upstream_inference_cost":null}}`

// Keep the production HTTP transport, URL, hostname verification and redirect
// policy. Only DNS/dial and trust roots select this owned TLS peer.
func usagePeer(t *testing.T, handler http.HandlerFunc, trust bool) (*OpenRouterUsageReader, *atomic.Int32) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"openrouter.ai"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	r, err := NewOpenRouterUsageReader(testKey, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	transport := r.client.Transport.(*http.Transport)
	if trust {
		roots := x509.NewCertPool()
		roots.AddCert(cert)
		transport.TLSClientConfig.RootCAs = roots
	}
	count := new(atomic.Int32)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		count.Add(1)
		if address != "openrouter.ai:443" {
			return nil, errors.New("unexpected destination")
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	return r, count
}

func assertUsageFailure(t *testing.T, observation UsageObservation, err error) {
	t.Helper()
	var usageErr *UsageReadError
	if !errors.As(err, &usageErr) || !reflect.DeepEqual(observation, UsageObservation{}) || errors.Unwrap(err) != nil {
		t.Fatal("failure must return no partial observation and a safe error")
	}
	for _, value := range []string{fmt.Sprint(err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		for _, forbidden := range []string{testKey, "private-provider-sentinel", "gen-fixture-123", "https://"} {
			if strings.Contains(value, forbidden) {
				t.Fatal("unsafe failure diagnostic")
			}
		}
	}
}

func TestOpenRouterUsageHTTPObservation(t *testing.T) {
	var requests, proxyRequests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxyRequests.Add(1) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	r, dials := usagePeer(t, func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		if req.Method != "GET" || req.Host != "openrouter.ai" || req.URL.Path != "/api/v1/generation" || req.URL.RawQuery != "id=gen-fixture-123" || req.Header.Get("Authorization") != "Bearer "+testKey || req.Header.Get("Idempotency-Key") != "" || req.ContentLength != 0 || req.ProtoMajor != 1 || req.TLS.ServerName != "openrouter.ai" {
			t.Error("metadata request escaped its fixed GET contract")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, usageFixtureJSON)
	}, true)
	start := time.Now()
	o, err := r.Observe(context.Background(), "gen-fixture-123")
	if err != nil {
		t.Fatal(err)
	}
	v := o.Record()
	hash := sha256.Sum256([]byte(usageFixtureJSON))
	if v.GenerationID != "gen-fixture-123" || v.TotalCostCredits != "0.000003" || v.ExactMicroUSD != "3" || v.MicroUSDStatus != "exact" || v.IsBYOK || v.SettlementState != "unverified" || v.ResponseSHA256 != hex.EncodeToString(hash[:]) || v.ObservedAt.Before(start) || v.ObservedAt.After(time.Now()) || !bytes.Equal(o.RawResponse(), []byte(usageFixtureJSON)) {
		t.Fatal("observation changed provider data or granted settlement")
	}
	copy := o.RawResponse()
	copy[0] = 'x'
	v.TotalCostCredits = "0"
	if o.RawResponse()[0] != '{' || o.Record().TotalCostCredits != "0.000003" {
		t.Fatal("observation was mutable through a copy")
	}
	if requests.Load() != 1 || dials.Load() != 1 || proxyRequests.Load() != 0 {
		t.Fatal("unexpected retry or proxy request")
	}
}

func TestOpenRouterUsageHTTPFailures(t *testing.T) {
	var redirectCalls atomic.Int32
	destination := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirectCalls.Add(1) }))
	defer destination.Close()
	cases := []struct {
		name    string
		handler http.HandlerFunc
		trust   bool
	}{
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
		}, true},
		{"not_found", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
			io.WriteString(w, "private-provider-sentinel "+testKey)
		}, true},
		{"rate_limit", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }, true},
		{"wrong_type", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, usageFixtureJSON)
		}, true},
		{"compression", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			io.WriteString(w, usageFixtureJSON)
		}, true},
		{"large_header", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Oversized", strings.Repeat("x", 32<<10))
			io.WriteString(w, usageFixtureJSON)
		}, true},
		{"large_body", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.(http.Flusher).Flush()
			io.WriteString(w, strings.Repeat(" ", maxUsageResponseBytes+1))
		}, true},
		{"bad_tls", func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS request reached handler") }, false},
		{"timeout", func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, dials := usagePeer(t, tc.handler, tc.trust)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			o, err := r.Observe(ctx, "gen-fixture-123")
			assertUsageFailure(t, o, err)
			if dials.Load() != 1 {
				t.Fatal("transport retried an observation")
			}
			if tc.name == "not_found" && err.(*UsageReadError).HTTPStatus != 404 {
				t.Fatal("lost status diagnostic")
			}
		})
	}
	if redirectCalls.Load() != 0 {
		t.Fatal("redirect could disclose credentials")
	}
}

func TestOpenRouterUsageRefusesInvalidAdmission(t *testing.T) {
	for _, key := range []string{"", "bad key", "key\r\nInjected: yes", strings.Repeat("x", 8193)} {
		if _, err := NewOpenRouterUsageReader(key, time.Second); err == nil {
			t.Fatal("invalid credential accepted")
		}
	}
	for _, timeout := range []time.Duration{0, -1, 31 * time.Second} {
		if _, err := NewOpenRouterUsageReader(testKey, timeout); err == nil {
			t.Fatal("unbounded timeout accepted")
		}
	}
	r, dials := usagePeer(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid request reached provider") }, true)
	for _, id := range []string{"", "gen-", "gen-hello?secret", "gen-hello/other", "gen-hello\n", "other", "gen-" + strings.Repeat("x", 125)} {
		o, err := r.Observe(context.Background(), id)
		assertUsageFailure(t, o, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o, err := r.Observe(ctx, "gen-fixture-123")
	assertUsageFailure(t, o, err)
	o, err = r.Observe(nil, "gen-fixture-123")
	assertUsageFailure(t, o, err)
	var absent *OpenRouterUsageReader
	o, err = absent.Observe(context.Background(), "gen-fixture-123")
	assertUsageFailure(t, o, err)
	if dials.Load() != 0 {
		t.Fatal("invalid observation attempted network access")
	}
}

func TestOpenRouterUsageJSONIntegrity(t *testing.T) {
	for name, raw := range map[string]string{
		"wrong_id":            strings.Replace(usageFixtureJSON, "gen-fixture-123", "gen-another", 1),
		"null_cost":           strings.Replace(usageFixtureJSON, "0.000003", "null", 1),
		"missing_cost":        strings.Replace(usageFixtureJSON, `"total_cost":0.000003,`, "", 1),
		"quoted_cost":         strings.Replace(usageFixtureJSON, "0.000003", `"0.000003"`, 1),
		"negative_cost":       strings.Replace(usageFixtureJSON, "0.000003", "-0.000003", 1),
		"missing_byok":        strings.Replace(usageFixtureJSON, `"is_byok":false,`, "", 1),
		"duplicate_cost":      strings.Replace(usageFixtureJSON, `"total_cost":`, `"total_cost":0,"total_cost":`, 1),
		"escaped_duplicate":   strings.Replace(usageFixtureJSON, `"total_cost":`, `"total_\u0063ost":0,"total_cost":`, 1),
		"invalid_unicode":     strings.Replace(usageFixtureJSON, "example/model", `\ud800`, 1),
		"trailing":            usageFixtureJSON + `{}`,
		"error":               strings.Replace(usageFixtureJSON, `{"data":`, `{"error":null,"data":`, 1),
		"secret_echo":         strings.Replace(usageFixtureJSON, `"model":`, `"extra":"`+testKey+`","model":`, 1),
		"escaped_secret_echo": strings.Replace(usageFixtureJSON, `"model":`, `"extra":"\u0073`+testKey[1:]+`","model":`, 1),
		"deep_unknown":        strings.Replace(usageFixtureJSON, `"model":`, `"extra":`+strings.Repeat("[", 33)+`0`+strings.Repeat("]", 33)+`,"model":`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			o, err := decodeOpenRouterUsage([]byte(raw), "gen-fixture-123", testKey)
			assertUsageFailure(t, o, err)
		})
	}
	raw := strings.Replace(strings.Replace(usageFixtureJSON, "0.000003", "0.0000001", 1), `"is_byok":false`, `"is_byok":true`, 1)
	o, err := decodeOpenRouterUsage([]byte(raw), "gen-fixture-123", testKey)
	if err != nil || !o.Record().IsBYOK || o.Record().MicroUSDStatus != "fractional" || o.Record().ExactMicroUSD != "" || o.Record().TotalCostCredits != "0.0000001" || o.Record().SettlementState != "unverified" {
		t.Fatal("BYOK/fractional observation conflated with settlement", err)
	}
}

func TestOpenRouterUsageExactDecimal(t *testing.T) {
	for _, tc := range []struct{ raw, micro, status string }{
		{"0", "0", "exact"}, {"0.0000000", "0", "exact"}, {"3e-6", "3", "exact"}, {"3E+1", "30000000", "exact"},
		{"0.0000001", "", "fractional"}, {"1e-1000", "", "fractional"}, {"0e1000", "0", "exact"},
		{"9223372036854.775807", "9223372036854775807", "exact"}, {"9223372036854.775808", "", "out_of_range"}, {"1e1000", "", "out_of_range"},
	} {
		micro, status, err := usageExactMicroUSD(tc.raw)
		if err != nil || micro != tc.micro || status != tc.status {
			t.Fatalf("decimal %s changed meaning", tc.raw)
		}
	}
	for _, raw := range []string{"-0", "-1", "+1", "01", "NaN", "Inf", "1/2", "1e1001", "1e-1001", "1e999999999999999999999999", strings.Repeat("1", 129)} {
		if _, _, err := usageExactMicroUSD(raw); err == nil {
			t.Fatal("unsupported number accepted")
		}
	}
}
