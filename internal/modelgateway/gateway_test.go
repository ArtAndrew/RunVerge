// SPDX-License-Identifier: Elastic-2.0
package modelgateway

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "synthetic-secret-key-never-log"

func fixtureConfig(endpoint string) Config {
	return Config{
		Endpoint: endpoint, APIKey: testKey, Model: "profile-fixture",
		MaxOutputTokens: 16, MaxInputBytes: 4096, Timeout: 2 * time.Second,
		AllowInsecureLoopback: true, TokenLimitField: "max_tokens",
	}
}

func fixtureRequest() Request {
	return Request{DispatchDeadline: time.Now().Add(2 * time.Second), ID: "operation-fixture-1", Instructions: "Return one text answer.", Goal: "Say hello."}
}

func gatewayFor(t *testing.T, config Config) *Gateway {
	t.Helper()
	gateway, err := New(config)
	if err != nil {
		t.Fatalf("construct gateway: %v", err)
	}
	return gateway
}

func assertOutcome(t *testing.T, err error, code string, isUnknown bool) {
	t.Helper()
	var outcome *OutcomeError
	if !errors.As(err, &outcome) {
		t.Fatalf("wanted OutcomeError, got %T", err)
	}
	if outcome.Code != code || outcome.Unknown != isUnknown || outcome.DefinitiveRejection == isUnknown {
		t.Fatalf("unexpected outcome: %+v", *outcome)
	}
	if errors.Unwrap(err) != nil {
		t.Fatal("unsafe underlying error retained")
	}
	for _, rendered := range []string{fmt.Sprint(err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		for _, forbidden := range []string{testKey, "provider-private-body", "url-password", "http://", "https://"} {
			if strings.Contains(rendered, forbidden) {
				t.Fatal("error exposed private transport or provider data")
			}
		}
	}
}

func successJSON() string {
	return `{"id":"body-request-1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"Hello"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`
}

func TestConfigSecurityAndExplicitLimits(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Config)
	}{
		{"no implicit HTTP", func(c *Config) { c.AllowInsecureLoopback = false }},
		{"no hostname exception", func(c *Config) { c.Endpoint = "http://localhost:8080/infer" }},
		{"no remote HTTP", func(c *Config) { c.Endpoint = "http://192.0.2.1/infer" }},
		{"no DNS alias exception", func(c *Config) { c.Endpoint = "http://127.0.0.1.example.com/infer" }},
		{"no user info", func(c *Config) { c.Endpoint = "https://user:url-password@example.com/infer" }},
		{"no query credentials", func(c *Config) { c.Endpoint = "https://example.com/infer?key=" + testKey }},
		{"no fragment", func(c *Config) { c.Endpoint = "https://example.com/infer#fragment" }},
		{"no other scheme", func(c *Config) { c.Endpoint = "file:///private/credentials" }},
		{"no zoned address", func(c *Config) { c.Endpoint = "http://[::1%25en0]:8080/infer" }},
		{"header injection", func(c *Config) { c.APIKey += "\r\nX-Injected: value" }},
		{"empty token field", func(c *Config) { c.TokenLimitField = "" }},
		{"unsupported token field", func(c *Config) { c.TokenLimitField = "num_predict" }},
		{"zero output bound", func(c *Config) { c.MaxOutputTokens = 0 }},
		{"excessive output bound", func(c *Config) { c.MaxOutputTokens = maxOutputTokens + 1 }},
		{"zero byte bound", func(c *Config) { c.MaxInputBytes = 0 }},
		{"excessive byte bound", func(c *Config) { c.MaxInputBytes = maxRequestBytes + 1 }},
		{"zero deadline", func(c *Config) { c.Timeout = 0 }},
		{"excessive deadline", func(c *Config) { c.Timeout = 11 * time.Minute }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := fixtureConfig("http://127.0.0.1:8080/infer")
			tc.edit(&config)
			_, err := New(config)
			var outcome *OutcomeError
			if !errors.As(err, &outcome) || outcome.Unknown || !outcome.DefinitiveRejection {
				t.Fatalf("expected preflight rejection, got %T", err)
			}
			if strings.Contains(fmt.Sprintf("%+v", err), testKey) || strings.Contains(fmt.Sprint(err), "url-password") {
				t.Fatal("configuration error leaked credentials")
			}
		})
	}
	for _, endpoint := range []string{"https://example.com/infer", "http://127.0.0.1:8080/infer", "http://[::1]:8080/infer"} {
		gatewayFor(t, fixtureConfig(endpoint))
	}
}

func TestExecuteSuccessWireProfileAndBothTokenFields(t *testing.T) {
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		t.Run(field, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/infer" || !r.Close {
					t.Error("unexpected method, path, or connection reuse")
				}
				if r.Header.Get("Authorization") != "Bearer "+testKey || r.Header.Get("X-Request-ID") != "operation-fixture-1" {
					t.Error("missing authorization or operation correlation")
				}
				if r.Header.Get("Idempotency-Key") != "" || r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("replay/compression policy was not applied")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error("invalid request JSON")
				}
				if body["model"] != "profile-fixture" || body["stream"] != false || body["n"] != float64(1) || body[field] != float64(16) || len(body) != 5 {
					t.Error("wire profile or explicit token bound changed")
				}
				messages, ok := body["messages"].([]any)
				if !ok || len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" || messages[1].(map[string]any)["content"] != "Say hello." {
					t.Error("unexpected instruction/goal messages")
				}
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("X-Request-ID", "provider-header-1")
				io.WriteString(w, successJSON())
			}))
			defer server.Close()
			config := fixtureConfig(server.URL + "/infer")
			config.TokenLimitField = field
			result, err := gatewayFor(t, config).Execute(context.Background(), fixtureRequest())
			if err != nil || result.Text != "Hello" || result.ProviderRequestID != "provider-header-1" || result.InputTokens != 7 || result.OutputTokens != 3 || calls.Load() != 1 {
				t.Fatalf("unexpected result: %+v, error: %v, requests: %d", result, err, calls.Load())
			}
		})
	}
}

func TestRequestRejectedBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	config := fixtureConfig(server.URL)
	config.MaxInputBytes = 256
	gateway := gatewayFor(t, config)
	_, err := gateway.Execute(context.Background(), Request{DispatchDeadline: time.Now().Add(2 * time.Second), ID: "id", Instructions: strings.Repeat("x", 100), Goal: strings.Repeat("y", 100)})
	assertOutcome(t, err, "request_too_large", false)
	_, err = gateway.Execute(context.Background(), Request{DispatchDeadline: time.Now().Add(2 * time.Second), ID: "id\r\nUnsafe: header", Instructions: "text", Goal: "text"})
	assertOutcome(t, err, "invalid_request", false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = gateway.Execute(ctx, fixtureRequest())
	assertOutcome(t, err, "cancelled_before_send", false)
	if calls.Load() != 0 {
		t.Fatal("preflight rejection dispatched HTTP")
	}
}

func TestTimeoutAfterAcceptanceIncludesBodyRead(t *testing.T) {
	var accepted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		accepted.Store(true)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	config := fixtureConfig(server.URL)
	config.Timeout = 100 * time.Millisecond
	start := time.Now()
	_, err := gatewayFor(t, config).Execute(context.Background(), fixtureRequest())
	assertOutcome(t, err, "response_read_unknown", true)
	if !accepted.Load() || time.Since(start) > time.Second {
		t.Fatal("request was not accepted or body read exceeded deadline")
	}
}

func TestLostResponseDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			connection.Close()
		}
	}))
	defer server.Close()
	_, err := gatewayFor(t, fixtureConfig(server.URL)).Execute(context.Background(), fixtureRequest())
	assertOutcome(t, err, "transport_outcome_unknown", true)
	if calls.Load() != 1 {
		t.Fatalf("expected one accepted request, got %d", calls.Load())
	}
}

func TestRejectRedirectWithoutForwardingCredentials(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL+"/"+testKey)
		w.WriteHeader(http.StatusTemporaryRedirect)
		io.WriteString(w, "provider-private-body")
	}))
	defer source.Close()
	_, err := gatewayFor(t, fixtureConfig(source.URL)).Execute(context.Background(), fixtureRequest())
	assertOutcome(t, err, "redirect_rejected", true)
	if targetCalls.Load() != 0 {
		t.Fatal("redirect target received a request")
	}
}

func TestExternalErrorStatusesNeverProveZeroCost(t *testing.T) {
	for _, status := range []int{400, 401, 403, 422, 429, 500, 502, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				io.WriteString(w, `provider-private-body `+testKey+` https://user:url-password@example.com`)
			}))
			defer server.Close()
			_, err := gatewayFor(t, fixtureConfig(server.URL)).Execute(context.Background(), fixtureRequest())
			code := "provider_status_unknown"
			if status == 429 {
				code = "provider_rate_limited"
			} else if status < 500 {
				code = "provider_rejected"
			}
			assertOutcome(t, err, code, true)
			var outcome *OutcomeError
			if !errors.As(err, &outcome) || outcome.HTTPStatus != status {
				t.Fatal("safe provider status lost")
			}
		})
	}
}

func TestInvalidUsageAndCompletionAreUnknown(t *testing.T) {
	cases := []struct{ name, body, code string }{
		{"missing usage", `{"choices":[]}`, "invalid_usage"},
		{"null usage", `{"usage":null}`, "invalid_usage"},
		{"missing count", `{"usage":{"prompt_tokens":3}}`, "invalid_usage"},
		{"negative", `{"usage":{"prompt_tokens":3,"completion_tokens":-1}}`, "invalid_usage"},
		{"fraction", `{"usage":{"prompt_tokens":3,"completion_tokens":1.5}}`, "invalid_usage"},
		{"exponent", `{"usage":{"prompt_tokens":3,"completion_tokens":1e1}}`, "invalid_usage"},
		{"bool", `{"usage":{"prompt_tokens":3,"completion_tokens":true}}`, "invalid_usage"},
		{"null count", `{"usage":{"prompt_tokens":3,"completion_tokens":null}}`, "invalid_usage"},
		{"string count", `{"usage":{"prompt_tokens":3,"completion_tokens":"1"}}`, "invalid_usage"},
		{"overflow", `{"usage":{"prompt_tokens":9223372036854775808,"completion_tokens":1}}`, "invalid_usage"},
		{"sum overflow", `{"usage":{"prompt_tokens":9223372036854775807,"completion_tokens":1}}`, "invalid_usage"},
		{"incorrect total", `{"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":5}}`, "invalid_usage"},
		{"output exceeds bound", `{"usage":{"prompt_tokens":3,"completion_tokens":17}}`, "output_token_limit_exceeded"},
		{"duplicate count", `{"usage":{"prompt_tokens":3,"completion_tokens":99,"completion_tokens":1}}`, "invalid_response_json"},
		{"duplicate usage", `{"usage":null,"usage":{"prompt_tokens":3,"completion_tokens":1}}`, "invalid_response_json"},
		{"invalid JSON", `provider-private-body ` + testKey, "invalid_response_json"},
		{"extra JSON", successJSON() + `{}`, "invalid_response_json"},
		{"bad UTF-8", "{\"data\":\"\xff\"}", "invalid_response_json"},
		{"multiple choices", `{"choices":[{},{}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`, "invalid_completion"},
		{"tool call", `{"choices":[{"message":{"content":"text","tool_calls":[{}]}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`, "unsupported_completion"},
		{"empty text", `{"choices":[{"message":{"content":""}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`, "invalid_completion"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			result, err := gatewayFor(t, fixtureConfig(server.URL)).Execute(context.Background(), fixtureRequest())
			assertOutcome(t, err, tc.code, true)
			if !reflect.DeepEqual(result, Result{}) {
				t.Fatal("error returned a partial successful result")
			}
		})
	}
}

func TestResponseBoundWithAndWithoutContentLength(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprint(chunked), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if chunked {
					w.(http.Flusher).Flush()
				} else {
					w.Header().Set("Content-Length", fmt.Sprint(maxResponseBytes+1))
				}
				io.WriteString(w, strings.Repeat("x", maxResponseBytes+1))
			}))
			defer server.Close()
			_, err := gatewayFor(t, fixtureConfig(server.URL)).Execute(context.Background(), fixtureRequest())
			assertOutcome(t, err, "response_too_large", true)
		})
	}
}

func TestCompressionAndWrongContentTypeAreRejected(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			if compressed {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Encoding", "gzip")
			}
			io.WriteString(w, successJSON())
		}))
		_, err := gatewayFor(t, fixtureConfig(server.URL)).Execute(context.Background(), fixtureRequest())
		server.Close()
		assertOutcome(t, err, "invalid_response_format", true)
	}
}

func TestRefusedConnectionAndUntrustedTLSAreNotDispatched(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := server.URL
	server.Close()
	_, err := gatewayFor(t, fixtureConfig(endpoint)).Execute(context.Background(), fixtureRequest())
	assertOutcome(t, err, "connection_not_established", false)
	var requests atomic.Int32
	tlsServer := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	tlsServer.Config.ErrorLog = log.New(io.Discard, "", 0)
	tlsServer.StartTLS()
	defer tlsServer.Close()
	_, err = gatewayFor(t, fixtureConfig(tlsServer.URL)).Execute(context.Background(), fixtureRequest())
	assertOutcome(t, err, "connection_not_established", false)
	if requests.Load() != 0 {
		t.Fatal("HTTP request passed untrusted TLS")
	}
}

func TestTLSWithFixtureTrustAndEnvironmentProxyDisabled(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://user:url-password@127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://user:url-password@127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://user:url-password@127.0.0.1:1")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			t.Error("fixture request did not use TLS")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, successJSON())
	}))
	defer server.Close()
	config := fixtureConfig(server.URL)
	config.AllowInsecureLoopback = false
	gateway := gatewayFor(t, config)
	transport := gateway.client.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("unsafe proxy or TLS verification configuration")
	}
	// Test-only trust anchor: production New exposes no skip-verification option.
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	transport.TLSClientConfig.RootCAs = pool
	result, err := gateway.Execute(context.Background(), fixtureRequest())
	if err != nil || result.ProviderRequestID != "body-request-1" {
		t.Fatalf("verified TLS fixture failed: %v", err)
	}
}
