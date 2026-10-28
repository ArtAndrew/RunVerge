// SPDX-License-Identifier: Elastic-2.0
package toolruntime

import (
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type memoryStore struct {
	prepareErr bool
	badPermit  bool
	delay      time.Duration
	age        time.Duration
	sent       atomic.Bool
	unknown    atomic.Int32
	receipts   atomic.Int32
	last       domain.ToolOutcomeEvidence
}

func (s *memoryStore) PrepareToolDispatch(_ context.Context, l *domain.ToolLease) (domain.ToolDispatchPermit, error) {
	if s.sent.Swap(true) || s.prepareErr {
		return domain.ToolDispatchPermit{}, errors.New("acknowledgement lost")
	}
	age := s.age
	if age == 0 {
		age = 2 * time.Second
	}
	now := time.Now()
	p := domain.ToolDispatchPermit{ExecutionID: l.Execution.ID, ActionID: l.Execution.ActionID, ReservationID: l.Execution.ReservationID, IntentHash: l.Execution.IntentHash, FencingToken: l.FencingToken, IssuedAt: now, NotAfter: now.Add(age), MaxAge: age}
	if s.badPermit {
		p.IntentHash = "wrong"
	}
	time.Sleep(s.delay)
	return p, nil
}
func (s *memoryStore) MarkCostUnknown(context.Context, string, string, string, string) (domain.Reservation, error) {
	s.unknown.Add(1)
	return domain.Reservation{}, nil
}
func (s *memoryStore) RecordToolOutcome(_ context.Context, in domain.ToolOutcomeEvidence) (domain.ToolOutcomeReceipt, error) {
	s.last = in
	s.receipts.Add(1)
	if in.Outcome == "unknown" {
		return domain.ToolOutcomeReceipt{}, nil
	}
	return domain.ToolOutcomeReceipt{ID: "receipt", Outcome: in.Outcome}, nil
}
func testProfile(endpoint, transport string) domain.ToolConnection {
	schema := `{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"],"additionalProperties":false}`
	c := domain.ToolConnection{Tool: domain.ToolInfo{ID: "tool", OrganizationID: "org", Version: 1, Name: "TLS fixture", Description: "No real provider", Effect: "write", Destination: "local test", Provider: "fixture", PriceVersion: "v1", MaxChargeMicroUSD: 100, InputSchemaJSON: schema, InputSchemaDigest: digest([]byte(schema)), TimeoutSeconds: 2, MaxInputBytes: 4096, MaxOutputBytes: 4096, Active: true}, Transport: transport, ResultContract: "agent_platform_action_v1", Endpoint: endpoint, CredentialEnvironment: "PLATFORM_TOOL_KEY_TEST", AllowPrivateNetwork: true}
	if transport == "mcp_https" {
		c.RemoteTool = "fixture_action"
	}
	return c
}
func testLease(c domain.ToolConnection) *domain.ToolLease {
	e := domain.ToolExecution{ID: "execution", OccurrenceID: "occurrence", OrganizationID: "org", TaskID: "task", RunID: "run", OperationID: "op", ActionID: "action", ReservationID: "reservation", ApprovalID: "approval", ToolID: c.Tool.ID, ToolVersion: c.Tool.Version, ToolProfileDigest: domain.ToolConnectionDigest(c), CandidateID: "candidate", ArtifactID: "artifact", ArtifactVersion: 1, ContentDigest: digest([]byte("candidate")), SourceTaskVersion: 7, ArgumentsJSON: `{ "value": 9007199254740993 }`, MaxChargeMicroUSD: 100, Provider: "fixture", PriceVersion: "v1", CreatedBy: "principal", Status: "leased", Version: 4, ExpiresAt: time.Now().Add(time.Minute)}
	e.ArgumentsDigest = digest([]byte(e.ArgumentsJSON))
	e.IntentHash = domain.ToolIntentDigest(e)
	return &domain.ToolLease{Execution: e, LeaseOwner: "worker", FencingToken: 3, LeaseUntil: time.Now().Add(time.Minute)}
}
func roots(server *httptest.Server) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(server.Certificate())
	return p
}
func responseFor(request wireRequest) wireResult {
	return wireResult{request.ExecutionID, request.ActionID, request.IntentHash, "succeeded", "fixture-reference", "bounded result"}
}

func TestHTTPActionReceiptAndNoReplay(t *testing.T) {
	var calls atomic.Int32
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer private-fixture" || r.Header.Get("Idempotency-Key") != "action" || r.Header.Get("Cookie") != "" {
			t.Error("credential/action binding")
		}
		var in wireRequest
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			t.Error("invalid request")
		}
		if !strings.Contains(string(in.Arguments), "9007199254740993") {
			t.Error("numeric argument changed")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(responseFor(in))
	}))
	defer peer.Close()
	c := testProfile(peer.URL, "http_json")
	store := &memoryStore{}
	runtime, err := New(store, Config{Connection: c, Credential: "private-fixture", TrustedRoots: roots(peer), PersistenceTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	l := testLease(c)
	out, err := runtime.Execute(context.Background(), l)
	if err != nil || out.Status != "succeeded" || out.ReceiptID != "receipt" || store.unknown.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("missing bounded receipt: %+v %v", out, err)
	}
	_, err = runtime.Execute(context.Background(), l)
	if err == nil || calls.Load() != 1 {
		t.Fatal("same occurrence sent twice")
	}
}
func TestActionNoSendOnPermitFault(t *testing.T) {
	for _, mode := range []string{"lost_ack", "wrong_binding", "expired_return", "tls_stall"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			peer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			if mode == "tls_stall" {
				peer.TLS = &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { time.Sleep(60 * time.Millisecond); return nil, nil }}
			}
			peer.StartTLS()
			defer peer.Close()
			c := testProfile(peer.URL, "http_json")
			s := &memoryStore{}
			switch mode {
			case "lost_ack":
				s.prepareErr = true
			case "wrong_binding":
				s.badPermit = true
			case "expired_return":
				s.age = 10 * time.Millisecond
				s.delay = 30 * time.Millisecond
			case "tls_stall":
				s.age = 20 * time.Millisecond
			}
			rt, e := New(s, Config{Connection: c, Credential: "private", TrustedRoots: roots(peer), PersistenceTimeout: time.Second})
			if e != nil {
				t.Fatal(e)
			}
			out, e := rt.Execute(context.Background(), testLease(c))
			if e == nil || out.Status != "unknown" || calls.Load() != 0 || s.unknown.Load() == 0 {
				t.Fatalf("unsafe permit path: %+v %v calls=%d", out, e, calls.Load())
			}
		})
	}
}
func TestHTTPUnknownResultsNeverCreateSuccessReceipt(t *testing.T) {
	for _, mode := range []string{"lost_reply", "redirect", "malformed", "wrong_intent", "duplicate", "wrong_case", "oversize", "generic_200", "rejected"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var in wireRequest
				_ = json.NewDecoder(r.Body).Decode(&in)
				w.Header().Set("Content-Type", "application/json")
				result := responseFor(in)
				switch mode {
				case "lost_reply":
					conn, _, _ := w.(http.Hijacker).Hijack()
					conn.Close()
					return
				case "redirect":
					w.Header().Set("Location", "https://unregistered.invalid")
					w.WriteHeader(307)
					return
				case "malformed":
					io.WriteString(w, `{"broken"`)
					return
				case "wrong_intent":
					result.IntentHash = "other"
				case "duplicate":
					body, _ := json.Marshal(result)
					io.WriteString(w, strings.TrimSuffix(string(body), "}")+`,"outcome":"succeeded"}`)
					return
				case "wrong_case":
					body, _ := json.Marshal(result)
					io.WriteString(w, strings.Replace(string(body), `"outcome"`, `"Outcome"`, 1))
					return
				case "oversize":
					io.WriteString(w, strings.Repeat("x", 20000))
					return
				case "generic_200":
					io.WriteString(w, `{"ok":true}`)
					return
				case "rejected":
					result.Outcome = "rejected"
				}
				_ = json.NewEncoder(w).Encode(result)
			}))
			defer peer.Close()
			c := testProfile(peer.URL, "http_json")
			s := &memoryStore{}
			rt, _ := New(s, Config{Connection: c, Credential: "private", TrustedRoots: roots(peer), PersistenceTimeout: time.Second})
			out, err := rt.Execute(context.Background(), testLease(c))
			if calls.Load() != 1 || s.unknown.Load() != 1 {
				t.Fatal("missing exposure or multiple attempts")
			}
			if mode == "rejected" {
				if err != nil || out.Status != "rejected" {
					t.Fatal(out, err)
				}
			} else if err == nil || out.Status != "unknown" || s.last.Outcome != "unknown" {
				t.Fatalf("unverified result accepted: %+v %v", out, err)
			}
		})
	}
}
func TestMCPActionStructuredReceiptAndDiscoveryCannotExtendPermit(t *testing.T) {
	for _, mode := range []string{"success", "sse_success", "slow_discovery", "generic", "is_error", "duplicate", "invalid_unicode"} {
		t.Run(mode, func(t *testing.T) {
			var effects atomic.Int32
			peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
					Params struct {
						Arguments json.RawMessage `json:"arguments"`
					} `json:"params"`
				}
				_ = json.NewDecoder(r.Body).Decode(&request)
				result := map[string]any{"resultType": "complete", "_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "stdlib-fixture", "version": "1"}}}
				if request.Method == "server/discover" {
					if mode == "slow_discovery" {
						time.Sleep(60 * time.Millisecond)
					}
					result["supportedVersions"] = []string{mcpbridge.ProtocolVersion}
					result["capabilities"] = map[string]any{"tools": map[string]any{}}
				} else if request.Method == "tools/call" {
					effects.Add(1)
					if r.Header.Get("Idempotency-Key") != "action" {
						t.Error("missing stable action key")
					}
					var in wireRequest
					_ = json.Unmarshal(request.Params.Arguments, &in)
					result["content"] = []any{}
					if mode == "generic" {
						result["structuredContent"] = map[string]any{"ok": true}
					} else {
						result["structuredContent"] = responseFor(in)
					}
					if mode == "is_error" {
						result["isError"] = true
					}
				} else {
					t.Errorf("unexpected MCP method %s", request.Method)
				}
				body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
				if mode == "duplicate" && request.Method == "tools/call" {
					body = []byte(strings.Replace(string(body), `"outcome":"succeeded"`, `"outcome":"rejected","outcome":"succeeded"`, 1))
				}
				if mode == "invalid_unicode" && request.Method == "tools/call" {
					body = []byte(strings.Replace(string(body), `"bounded result"`, `"\ud800"`, 1))
				}
				if mode == "sse_success" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: message\ndata: "+string(body)+"\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(body)
				}
			}))
			defer peer.Close()
			c := testProfile(peer.URL, "mcp_https")
			s := &memoryStore{}
			if mode == "slow_discovery" {
				s.age = 20 * time.Millisecond
			}
			rt, err := New(s, Config{Connection: c, Credential: "private", TrustedRoots: roots(peer), PersistenceTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			out, err := rt.Execute(context.Background(), testLease(c))
			if mode == "success" || mode == "sse_success" {
				if err != nil || out.Status != "succeeded" || effects.Load() != 1 {
					t.Fatalf("MCP action failed: %+v %v count%d", out, err, effects.Load())
				}
			} else {
				if err == nil || out.Status != "unknown" {
					t.Fatalf("MCP result incorrectly accepted: %+v %v", out, err)
				}
				if mode == "slow_discovery" && effects.Load() != 0 {
					t.Fatal("discovery reset permit")
				}
			}
		})
	}
}
func TestArgumentsExactNumbersAndSchemaBoundaries(t *testing.T) {
	c := testProfile("https://example.invalid/action", "http_json")
	raw := " {\"value\":9007199254740993} \n"
	got, err := CanonicalArguments(c, raw)
	if err != nil || got != raw {
		t.Fatal("argument bytes not preserved", err)
	}
	for _, raw := range []string{`{"value":1,"value":2}`, `{"value":"x"}`, `{"value":1,"extra":true}`, `{"value":1e9999}`, `{"value":1,"x":"\ud800"}`} {
		if _, err = CanonicalArguments(c, raw); err == nil {
			t.Fatal("invalid arguments accepted")
		}
	}
	for _, schema := range []string{`{"type":"object","properties":{"value":{"minimum":1.0000000000000001}}}`, `{"type":"object","properties":{"value":{"const":9007199254740990.1}}}`, `{"type":"object","$ref":"https://unregistered.invalid/schema"}`, `{"type":"object","properties":{"value":{"format":"email"}}}`} {
		c.Tool.InputSchemaJSON = schema
		c.Tool.InputSchemaDigest = digest([]byte(schema))
		if ValidateConnection(c) == nil {
			t.Fatal("unsupported schema accepted")
		}
	}
}
