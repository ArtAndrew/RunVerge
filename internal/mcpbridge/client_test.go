// SPDX-License-Identifier: Elastic-2.0
package mcpbridge_test

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agentplatform.local/platform/internal/mcpbridge"
)

type peerRequest struct {
	ID     json.RawMessage            `json:"id"`
	Method string                     `json:"method"`
	Params map[string]json.RawMessage `json:"params"`
}

func peerResponse(req peerRequest) map[string]any {
	result := map[string]any{"resultType": "complete", "_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "independent-stdlib-peer", "version": "1"}}}
	switch req.Method {
	case "server/discover":
		result["supportedVersions"] = []string{mcpbridge.ProtocolVersion}
		result["capabilities"] = map[string]any{"tools": map[string]any{}}
	case "tools/list":
		result["tools"] = []any{map[string]any{"name": "fixture_echo", "description": "Untrusted local fixture", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}}}
	case "tools/call":
		result["content"] = []any{map[string]any{"type": "text", "text": "local fixture"}}
		result["structuredContent"] = map[string]any{"fixture": true, "platform_environment_inherited": os.Getenv("PLATFORM_DEPLOYMENT_KEY") != ""}
	default:
		return map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "unsupported method"}}
	}
	return map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
}

// The child peer deliberately uses only encoding/json and standard streams,
// independently of the adapter and of the official SDK's server implementation.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_PEER_MODE") == "raw-stdio" {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 4096), mcpbridge.MaxMessageBytes)
		for scanner.Scan() {
			var req peerRequest
			if json.Unmarshal(scanner.Bytes(), &req) != nil {
				os.Exit(2)
			}
			if req.Method == "notifications/cancelled" {
				continue
			}
			if json.NewEncoder(os.Stdout).Encode(peerResponse(req)) != nil {
				os.Exit(3)
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func tlsConfig(server *httptest.Server) mcpbridge.ClientConfig {
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	return mcpbridge.ClientConfig{Endpoint: server.URL, TrustedRoots: roots, AllowPrivateNetwork: true, Timeout: 2 * time.Second}
}
func fixtureGate(context.Context, string, json.RawMessage) error { return nil }
func TestClientIndependentTLSJSONAndSSEPeers(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprint("sse=", sse), func(t *testing.T) {
			var mu sync.Mutex
			calls := []string{}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Error("unexpected legacy HTTP method")
				}
				if r.Header.Get("Authorization") != "Bearer registered-peer-only" || r.Header.Get("Cookie") != "" {
					t.Error("wrong endpoint credential")
				}
				var req peerRequest
				if json.NewDecoder(r.Body).Decode(&req) != nil {
					t.Error("invalid JSON request")
					return
				}
				var meta map[string]json.RawMessage
				_ = json.Unmarshal(req.Params["_meta"], &meta)
				if string(meta["io.modelcontextprotocol/protocolVersion"]) != `"2026-07-28"` || string(meta["io.modelcontextprotocol/clientCapabilities"]) != "{}" {
					t.Errorf("client overclaimed capability or version: %s", req.Params["_meta"])
				}
				mu.Lock()
				calls = append(calls, req.Method)
				mu.Unlock()
				body, _ := json.Marshal(peerResponse(req))
				if sse {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.Write(body)
				}
			}))
			defer server.Close()
			config := tlsConfig(server)
			config.AccessToken = "registered-peer-only"
			config.AuthorizeCall = fixtureGate
			client, e := mcpbridge.Connect(context.Background(), config)
			if e != nil {
				t.Fatal(e)
			}
			defer client.Close()
			tools, e := client.ListTools(context.Background(), "")
			if e != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "fixture_echo" {
				t.Fatalf("discovery/list failed: %v", e)
			}
			result, e := client.CallTool(context.Background(), "fixture_echo", json.RawMessage(`{"text":"small input"}`))
			if e != nil || result.IsError {
				t.Fatalf("call failed: %v", e)
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(calls, ",") != "server/discover,tools/list,tools/call" {
				t.Fatalf("unexpected protocol loop: %v", calls)
			}
		})
	}
}
func TestClientRealStdioProcessDoesNotInheritSecrets(t *testing.T) {
	t.Setenv("PLATFORM_DEPLOYMENT_KEY", "must-not-enter-child")
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	client, e := mcpbridge.Connect(context.Background(), mcpbridge.ClientConfig{Command: executable, Environment: []string{"MCP_PEER_MODE=raw-stdio"}, AuthorizeCall: fixtureGate, Timeout: 2 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	tools, e := client.ListTools(context.Background(), "")
	if e != nil || len(tools.Tools) != 1 {
		t.Fatal("stdio list failed")
	}
	result, e := client.CallTool(context.Background(), "fixture_echo", json.RawMessage(`{}`))
	if e != nil {
		t.Fatal(e)
	}
	body, _ := json.Marshal(result.StructuredContent)
	if !strings.Contains(string(body), `"platform_environment_inherited":false`) {
		t.Fatal("child inherited a platform secret")
	}
}
func TestClientNoLegacyFallbackNoContinuationNoResend(t *testing.T) {
	for _, mode := range []string{"legacy", "input_required", "lost_reply"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			calls := []string{}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req peerRequest
				if json.NewDecoder(r.Body).Decode(&req) != nil {
					t.Error("bad request")
					return
				}
				mu.Lock()
				calls = append(calls, req.Method)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if mode == "legacy" {
					json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "initialize first"}})
					return
				}
				if req.Method == "tools/call" {
					if mode == "lost_reply" {
						conn, _, e := w.(http.Hijacker).Hijack()
						if e != nil {
							t.Error(e)
							return
						}
						conn.Close()
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"resultType": "input_required", "requestState": "untrusted-state", "inputRequests": map[string]any{"input": map[string]any{"method": "elicitation/create", "params": map[string]any{"message": "continue", "requestedSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}}}})
					return
				}
				json.NewEncoder(w).Encode(peerResponse(req))
			}))
			defer server.Close()
			config := tlsConfig(server)
			config.AuthorizeCall = fixtureGate
			client, e := mcpbridge.Connect(context.Background(), config)
			if mode == "legacy" {
				if e == nil {
					client.Close()
					t.Fatal("legacy accepted")
				}
			} else {
				if e != nil {
					t.Fatal(e)
				}
				defer client.Close()
				if _, e = client.CallTool(context.Background(), "fixture_echo", json.RawMessage(`{}`)); e == nil {
					t.Fatal("uncertain/continuation call succeeded")
				}
			}
			mu.Lock()
			defer mu.Unlock()
			want := 1
			if mode != "legacy" {
				want = 2
			}
			if len(calls) != want {
				t.Fatalf("automatic fallback/retry/continuation: %v", calls)
			}
		})
	}
}
func TestClientNetworkAndGatewayDenyByDefault(t *testing.T) {
	var requests int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var req peerRequest
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(peerResponse(req))
	}))
	defer server.Close()
	config := tlsConfig(server)
	config.AllowPrivateNetwork = false
	if c, e := mcpbridge.Connect(context.Background(), config); e == nil {
		c.Close()
		t.Fatal("private endpoint accepted by default")
	}
	config = tlsConfig(server)
	client, e := mcpbridge.Connect(context.Background(), config)
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	if _, e = client.CallTool(context.Background(), "fixture_echo", json.RawMessage(`{}`)); e == nil {
		t.Fatal("tool ran without gateway")
	}
	if requests != 1 {
		t.Fatal("denied call reached peer")
	}
}
func TestClientRedirectDoesNotForwardCredential(t *testing.T) {
	var targetCalls int
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++; io.WriteString(w, "unexpected") }))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	config := tlsConfig(source)
	config.AccessToken = "source-only-token"
	if c, e := mcpbridge.Connect(context.Background(), config); e == nil {
		c.Close()
		t.Fatal("redirect followed")
	}
	if targetCalls != 0 {
		t.Fatal("credential forwarded to redirected endpoint")
	}
}

func TestClientBoundsUntrustedHTTPResponse(t *testing.T) {
	for _, mode := range []string{"content_length", "chunked_json", "sse_event", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req peerRequest
				json.NewDecoder(r.Body).Decode(&req)
				if req.Method == "server/discover" {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(peerResponse(req))
					return
				}
				switch mode {
				case "content_length":
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Content-Length", fmt.Sprint(mcpbridge.MaxResponseBytes+1))
					w.WriteHeader(200)
				case "chunked_json":
					w.Header().Set("Content-Type", "application/json")
					w.(http.Flusher).Flush()
					io.WriteString(w, strings.Repeat(" ", mcpbridge.MaxResponseBytes+1))
				case "sse_event":
					w.Header().Set("Content-Type", "text/event-stream")
					w.(http.Flusher).Flush()
					io.WriteString(w, "event: message\ndata: "+strings.Repeat("x", mcpbridge.MaxResponseBytes+1)+"\n\n")
				case "timeout":
					<-r.Context().Done()
				}
			}))
			defer server.Close()
			config := tlsConfig(server)
			config.Timeout = 150 * time.Millisecond
			client, e := mcpbridge.Connect(context.Background(), config)
			if e != nil {
				t.Fatal(e)
			}
			defer client.Close()
			if _, e = client.ListTools(context.Background(), ""); e == nil {
				t.Fatal("untrusted response exceeded bound without error")
			}
		})
	}
}
