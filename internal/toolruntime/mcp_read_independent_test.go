// SPDX-License-Identifier: Elastic-2.0
package toolruntime_test

import (
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
	"agentplatform.local/platform/internal/toolruntime"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func independentReadHash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// This observes gateway calls, not PostgreSQL durability or financial settlement.
type independentReadStore struct {
	mu           sync.Mutex
	issued       bool
	lostAck      bool
	wrongPermit  bool
	prepareDelay time.Duration
	permitAge    time.Duration
	prepares     int
	unknownCosts int
	evidence     []domain.ToolOutcomeEvidence
}

func (s *independentReadStore) PrepareToolDispatch(_ context.Context, l *domain.ToolLease) (domain.ToolDispatchPermit, error) {
	s.mu.Lock()
	s.prepares++
	if s.issued {
		s.mu.Unlock()
		return domain.ToolDispatchPermit{}, errors.New("already admitted")
	}
	s.issued = true
	lost, wrong, delay, age := s.lostAck, s.wrongPermit, s.prepareDelay, s.permitAge
	s.mu.Unlock()
	if lost {
		return domain.ToolDispatchPermit{}, errors.New("committed permit acknowledgement lost")
	}
	if age == 0 {
		age = 2 * time.Second
	}
	now := time.Now()
	p := domain.ToolDispatchPermit{ExecutionID: l.Execution.ID, ActionID: l.Execution.ActionID, ReservationID: l.Execution.ReservationID, IntentHash: l.Execution.IntentHash, FencingToken: l.FencingToken, IssuedAt: now, NotAfter: now.Add(age), MaxAge: age}
	if wrong {
		p.FencingToken++
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	return p, nil
}
func (s *independentReadStore) MarkCostUnknown(context.Context, string, string, string, string) (domain.Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unknownCosts++
	return domain.Reservation{}, nil
}
func (s *independentReadStore) RecordToolOutcome(_ context.Context, in domain.ToolOutcomeEvidence) (domain.ToolOutcomeReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evidence = append(s.evidence, in)
	return domain.ToolOutcomeReceipt{ID: "independent-receipt", Outcome: in.Outcome}, nil
}
func (s *independentReadStore) snapshot() (int, int, []domain.ToolOutcomeEvidence) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prepares, s.unknownCosts, append([]domain.ToolOutcomeEvidence(nil), s.evidence...)
}

type independentReadRequest struct {
	ID     json.RawMessage            `json:"id"`
	Method string                     `json:"method"`
	Params map[string]json.RawMessage `json:"params"`
}
type independentReadPeer struct {
	server   *httptest.Server
	calls    atomic.Int32
	requests atomic.Int32
}

func independentReadTLS(t *testing.T, respond func(http.ResponseWriter, *http.Request, independentReadRequest)) *independentReadPeer {
	t.Helper()
	p := &independentReadPeer{}
	p.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.requests.Add(1)
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer independent-read-only" || r.Header.Get("Cookie") != "" {
			t.Error("transport credential/method boundary changed")
		}
		var req independentReadRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			t.Error("unparseable outbound RPC")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "server/discover":
			if r.Header.Get("Idempotency-Key") != "" {
				t.Error("action identity attached to discovery")
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"resultType":"complete","supportedVersions":[%q],"capabilities":{"tools":{}},"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"independent-standard-peer","version":"1"}}}}`, req.ID, mcpbridge.ProtocolVersion)
		case "tools/call":
			p.calls.Add(1)
			if r.Header.Get("Idempotency-Key") != "independent-action" {
				t.Error("missing stable action header")
			}
			if string(req.Params["name"]) != `"independent_read"` {
				t.Error("wrong registered remote tool")
			}
			var args map[string]json.RawMessage
			if json.Unmarshal(req.Params["arguments"], &args) != nil || len(args) != 2 || string(args["n"]) != "9007199254740993" || string(args["text"]) != `"approved"` {
				t.Errorf("approved arguments changed or custom action wrapper sent: %s", req.Params["arguments"])
			}
			respond(w, r, req)
		default:
			t.Errorf("unexpected extra protocol call %s", req.Method)
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(p.server.Close)
	return p
}
func independentReadProfile(endpoint string) domain.ToolConnection {
	schema := `{"type":"object","properties":{"n":{"type":"integer"},"text":{"type":"string"}},"required":["n","text"],"additionalProperties":false}`
	return domain.ToolConnection{Tool: domain.ToolInfo{ID: "independent-read", OrganizationID: "independent-org", Version: 1, Name: "Independent read", Description: "Synthetic TLS read", Effect: "read", Destination: "Owned TLS peer", Provider: "independent-provider", PriceVersion: "independent-price", MaxChargeMicroUSD: 73, InputSchemaJSON: schema, InputSchemaDigest: independentReadHash([]byte(schema)), TimeoutSeconds: 1, MaxInputBytes: 4096, MaxOutputBytes: 1024, Active: true}, Transport: "mcp_https", ResultContract: domain.ToolResultContractMCPReadTextV1, Endpoint: endpoint, RemoteTool: "independent_read", CredentialEnvironment: "PLATFORM_TOOL_KEY_INDEPENDENT_READ", AllowPrivateNetwork: true}
}
func independentReadLease(c domain.ToolConnection) *domain.ToolLease {
	e := domain.ToolExecution{ID: "independent-execution", OrganizationID: c.Tool.OrganizationID, TaskID: "independent-task", RunID: "independent-run", OperationID: "independent-operation", ActionID: "independent-action", ReservationID: "independent-reservation", ApprovalID: "independent-approval", OccurrenceID: "independent-occurrence", ToolID: c.Tool.ID, ToolVersion: c.Tool.Version, ToolProfileDigest: domain.ToolConnectionDigest(c), CandidateID: "independent-candidate", ArtifactID: "independent-artifact", ArtifactVersion: 1, ContentDigest: independentReadHash([]byte("candidate")), SourceTaskVersion: 3, ArgumentsJSON: `{ "n": 9007199254740993, "text": "approved" }`, Provider: c.Tool.Provider, PriceVersion: c.Tool.PriceVersion, MaxChargeMicroUSD: c.Tool.MaxChargeMicroUSD, CreatedBy: "independent-owner", Status: "leased", Version: 1, ExpiresAt: time.Now().Add(time.Minute)}
	e.ArgumentsDigest = independentReadHash([]byte(e.ArgumentsJSON))
	e.IntentHash = domain.ToolIntentDigest(e)
	return &domain.ToolLease{Execution: e, LeaseOwner: "independent-worker", FencingToken: 7, LeaseUntil: time.Now().Add(time.Minute)}
}
func independentReadRuntime(t *testing.T, p *independentReadPeer, c domain.ToolConnection, s *independentReadStore, trust bool) *toolruntime.Runtime {
	t.Helper()
	var pool *x509.CertPool
	if trust {
		pool = x509.NewCertPool()
		pool.AddCert(p.server.Certificate())
	}
	r, err := toolruntime.New(s, toolruntime.Config{Connection: c, Credential: "independent-read-only", TrustedRoots: pool, PersistenceTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func independentReadFrame(id json.RawMessage, result string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, id, result)
}

func TestMCPReadIndependentStandardResultAndRawEvidence(t *testing.T) {
	cases := []struct {
		name, raw string
		texts     []string
		outcome   string
		sse       bool
	}{
		{"empty", `{"content":[{"type":"text","text":""}]}`, []string{""}, "succeeded", false},
		{"blocks", `{ "content": [{"type":"text","text":"ab"},{"type":"text","text":"c\n<&"}], "isError":false, "structuredContent":{"text":"DO NOT USE", "big":9007199254740993}, "_meta":{"role":"system"} }`, []string{"ab", "c\n<&"}, "succeeded", false},
		{"sse", `{"content":[{"type":"text","text":"line\r\nline"},{"type":"text","text":""}],"resultType":"complete"}`, []string{"line\r\nline", ""}, "succeeded", true},
		{"read_error", `{"content":[{"type":"text","text":"observed read failure"}],"isError":true}`, []string{"observed read failure"}, "rejected", false},
		{"sixteen", `{"content":[` + strings.TrimSuffix(strings.Repeat(`{"type":"text","text":""},`, 16), ",") + `]}`, make([]string, 16), "succeeded", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := independentReadTLS(t, func(w http.ResponseWriter, _ *http.Request, req independentReadRequest) {
				frame := independentReadFrame(req.ID, tc.raw)
				if tc.sse {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", frame)
				} else {
					io.WriteString(w, frame)
				}
			})
			c := independentReadProfile(p.server.URL)
			s := &independentReadStore{}
			r := independentReadRuntime(t, p, c, s, true)
			l := independentReadLease(c)
			out, err := r.Execute(context.Background(), l)
			_, cost, ev := s.snapshot()
			if err != nil || out.Status != tc.outcome || out.ReceiptID == "" || cost != 1 || p.calls.Load() != 1 || len(ev) != 1 {
				t.Fatalf("unexpected observation: %+v err=%v cost=%d calls=%d evidence=%+v", out, err, cost, p.calls.Load(), ev)
			}
			var texts []string
			if json.Unmarshal([]byte(ev[0].Result), &texts) != nil || !reflect.DeepEqual(texts, tc.texts) {
				t.Fatalf("text blocks lost or replaced: %q", ev[0].Result)
			}
			if ev[0].EvidenceDigest != independentReadHash([]byte(tc.raw)) || ev[0].IntentHash != l.Execution.IntentHash || ev[0].FencingToken != 7 || ev[0].Outcome != tc.outcome {
				t.Fatal("original raw evidence or execution binding lost")
			}
			if _, err = r.Execute(context.Background(), l); err == nil || p.calls.Load() != 1 {
				t.Fatal("same admitted occurrence was resent")
			}
		})
	}
}

func TestMCPReadIndependentMalformedWireIsUnknown(t *testing.T) {
	good := `{"type":"text","text":"ok"}`
	cases := map[string]string{
		"missing_content": `{"isError":false}`, "null_content": `{"content":null}`, "empty_content": `{"content":[]}`,
		"missing_text": `{"content":[{"type":"text"}]}`, "null_text": `{"content":[{"type":"text","text":null}]}`, "number_text": `{"content":[{"type":"text","text":1}]}`,
		"null_isError": `{"content":[` + good + `],"isError":null}`, "string_isError": `{"content":[` + good + `],"isError":"false"}`, "number_isError": `{"content":[` + good + `],"isError":0}`,
		"nontext": `{"content":[{"type":"image","data":"opaque","mimeType":"image/png"}]}`, "mixed": `{"content":[` + good + `,{"type":"resource_link","uri":"https://unused.invalid"}]}`,
		"duplicate_text": `{"content":[{"type":"text","text":"first","text":"last"}]}`, "duplicate_isError": `{"content":[` + good + `],"isError":false,"isError":true}`,
		"duplicate_ignored_meta": `{"content":[` + good + `],"_meta":{"a":1,"a":2}}`, "unpaired_surrogate": `{"content":[{"type":"text","text":"\ud800"}]}`,
		"nul": `{"content":[{"type":"text","text":"a\u0000b"}]}`, "invalid_utf8": "{\"content\":[{\"type\":\"text\",\"text\":\"" + string([]byte{0xff}) + "\"}]}",
		"structured_only": `{"structuredContent":{"content":[` + good + `]}}`, "null_block": `{"content":[null]}`, "seventeen": `{"content":[` + strings.TrimSuffix(strings.Repeat(good+",", 17), ",") + `]}`,
		"interactive": `{"content":[` + good + `],"resultType":"input_required","requestState":"opaque"}`, "hidden_input_request": `{"content":[` + good + `],"inputRequests":null}`,
		"oversized_text":   `{"content":[{"type":"text","text":"` + strings.Repeat("x", 1025) + `"}]}`,
		"aggregate_limit":  `{"content":[{"type":"text","text":"` + strings.Repeat("a", 600) + `"},{"type":"text","text":"` + strings.Repeat("b", 600) + `"}]}`,
		"serialized_limit": `{"content":[{"type":"text","text":"` + strings.Repeat(`\n`, 600) + `"}]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			p := independentReadTLS(t, func(w http.ResponseWriter, _ *http.Request, req independentReadRequest) {
				io.WriteString(w, independentReadFrame(req.ID, raw))
			})
			c := independentReadProfile(p.server.URL)
			s := &independentReadStore{}
			r := independentReadRuntime(t, p, c, s, true)
			l := independentReadLease(c)
			out, err := r.Execute(context.Background(), l)
			_, cost, ev := s.snapshot()
			if err == nil || out.Status != "unknown" || cost != 1 || len(ev) != 1 || ev[0].Outcome != "unknown" || p.calls.Load() != 1 {
				t.Fatalf("malformed result gained authority: %+v %v cost=%d evidence=%+v calls=%d", out, err, cost, ev, p.calls.Load())
			}
			_, _ = r.Execute(context.Background(), l)
			if p.calls.Load() != 1 {
				t.Fatal("unknown was resent")
			}
		})
	}
}

func TestMCPReadIndependentReplyCorrelation(t *testing.T) {
	result := `{"content":[{"type":"text","text":"not authoritative without correlation"}]}`
	for _, mode := range []string{"string_id", "large_id", "null_id", "fraction_id", "duplicate_id", "error_and_result", "bad_version", "method_and_result", "duplicate_sse", "foreign_sse_only", "bad_content_type", "array_batch", "dropped_reply"} {
		t.Run(mode, func(t *testing.T) {
			p := independentReadTLS(t, func(w http.ResponseWriter, _ *http.Request, req independentReadRequest) {
				frame := independentReadFrame(req.ID, result)
				switch mode {
				case "string_id":
					frame = independentReadFrame(json.RawMessage(fmt.Sprintf("%q", string(req.ID))), result)
				case "large_id":
					frame = independentReadFrame(json.RawMessage(`9007199254740993`), result)
				case "null_id":
					frame = independentReadFrame(json.RawMessage(`null`), result)
				case "fraction_id":
					frame = independentReadFrame(json.RawMessage(string(req.ID)+".0"), result)
				case "duplicate_id":
					frame = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"id":%s,"result":%s}`, req.ID, req.ID, result)
				case "error_and_result":
					frame = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s,"error":{"code":-1,"message":"error"}}`, req.ID, result)
				case "bad_version":
					frame = strings.Replace(frame, `"2.0"`, `"1.0"`, 1)
				case "method_and_result":
					frame = strings.Replace(frame, `"jsonrpc":`, `"method":"tools/call","jsonrpc":`, 1)
				case "duplicate_sse":
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", frame, frame)
					return
				case "foreign_sse_only":
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\n", independentReadFrame(json.RawMessage(`"foreign"`), result))
					return
				case "bad_content_type":
					w.Header().Set("Content-Type", "text/plain")
				case "array_batch":
					frame = "[" + frame + "]"
				case "dropped_reply":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
				io.WriteString(w, frame)
			})
			c := independentReadProfile(p.server.URL)
			s := &independentReadStore{}
			r := independentReadRuntime(t, p, c, s, true)
			l := independentReadLease(c)
			out, err := r.Execute(context.Background(), l)
			_, cost, ev := s.snapshot()
			if err == nil || out.Status != "unknown" || cost != 1 || len(ev) != 1 || ev[0].Outcome != "unknown" || p.calls.Load() != 1 {
				t.Fatalf("uncorrelated result accepted: %+v %v evidence=%+v calls=%d", out, err, ev, p.calls.Load())
			}
			_, _ = r.Execute(context.Background(), l)
			if p.calls.Load() != 1 {
				t.Fatal("uncertain RPC resent")
			}
		})
	}
}

func TestMCPReadIndependentNoIOOnAuthorityAndPermitFaults(t *testing.T) {
	for _, mode := range []string{"foreign_org", "stale_profile", "price", "charge", "arguments", "lost_permit_ack", "wrong_permit", "expired_permit", "private_network", "untrusted_ca"} {
		t.Run(mode, func(t *testing.T) {
			p := independentReadTLS(t, func(w http.ResponseWriter, _ *http.Request, req independentReadRequest) {
				io.WriteString(w, independentReadFrame(req.ID, `{"content":[{"type":"text","text":"must not call"}]}`))
			})
			c := independentReadProfile(p.server.URL)
			l := independentReadLease(c)
			s := &independentReadStore{}
			trust := true
			switch mode {
			case "foreign_org":
				l.Execution.OrganizationID = "foreign"
			case "stale_profile":
				l.Execution.ToolProfileDigest = strings.Repeat("0", 64)
			case "price":
				l.Execution.PriceVersion = "foreign-price"
			case "charge":
				l.Execution.MaxChargeMicroUSD++
			case "arguments":
				l.Execution.ArgumentsJSON = `{"n":1,"text":"changed"}`
			case "lost_permit_ack":
				s.lostAck = true
			case "wrong_permit":
				s.wrongPermit = true
			case "expired_permit":
				s.permitAge = time.Millisecond
				s.prepareDelay = 10 * time.Millisecond
			case "private_network":
				c.AllowPrivateNetwork = false
				l = independentReadLease(c)
			case "untrusted_ca":
				trust = false
			}
			r := independentReadRuntime(t, p, c, s, trust)
			if _, err := r.Execute(context.Background(), l); err == nil {
				t.Fatal("fault admitted execution")
			}
			if p.requests.Load() != 0 || p.calls.Load() != 0 {
				t.Fatal("authority/permit/TLS fault reached peer application")
			}
			_, _ = r.Execute(context.Background(), l)
			if p.calls.Load() != 0 {
				t.Fatal("failed admission resent")
			}
		})
	}
}

func TestMCPReadIndependentTrustedProfileCombinations(t *testing.T) {
	for _, mode := range []string{"write", "destructive", "http", "too_small", "unknown_contract"} {
		t.Run(mode, func(t *testing.T) {
			c := independentReadProfile("https://unused.invalid/mcp")
			switch mode {
			case "write":
				c.Tool.Effect = "write"
			case "destructive":
				c.Tool.Effect = "destructive"
				c.Tool.RequireDistinctApprover = true
			case "http":
				c.Transport = "http_json"
				c.RemoteTool = ""
			case "too_small":
				c.Tool.MaxOutputBytes = 3
			case "unknown_contract":
				c.ResultContract = "peer_says_read"
			}
			if _, err := toolruntime.New(&independentReadStore{}, toolruntime.Config{Connection: c, Credential: "independent-read-only", PersistenceTimeout: time.Second}); err == nil {
				t.Fatal("untrusted profile combination accepted")
			}
		})
	}
}
