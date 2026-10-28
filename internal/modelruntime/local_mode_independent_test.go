// SPDX-License-Identifier: Elastic-2.0
package modelruntime_test

import (
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelgateway"
	"agentplatform.local/platform/internal/modelruntime"
	"context"
	"crypto/sha256"
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

// This independently models the trusted Store interface. It is not a SQL or
// daemon simulator and cannot establish real durable accounting/authority.
type localModeIndependentStore struct {
	mu               sync.Mutex
	reservation      domain.Reservation
	request          domain.ReserveCostRequest
	action           string
	authorized       bool
	fence            int64
	mode             string
	steps            []string
	candidate        domain.ModelCandidate
	artifact         domain.Artifact
	pause            string
	freshPersistence bool
}

func localModeIndependentNewStore() *localModeIndependentStore {
	return &localModeIndependentStore{authorized: true, fence: 11, freshPersistence: true}
}
func (s *localModeIndependentStore) ReserveCost(ctx context.Context, l *domain.Lease, in domain.ReserveCostRequest) (domain.Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, "reserve")
	if ctx.Err() != nil || !s.authorized || l.Token != s.fence {
		return domain.Reservation{}, errors.New("private authority refused")
	}
	if s.reservation.ID != "" {
		if in != s.request {
			return domain.Reservation{}, errors.New("immutable binding differs")
		}
		return s.reservation, nil
	}
	s.request = in
	s.action = "pending"
	s.reservation = domain.Reservation{ID: "independent-zero-reservation", OrganizationID: l.OrganizationID, TaskID: l.TaskID, OperationID: l.OperationID, ActionID: "independent-zero-action", ChargeKey: in.ChargeKey, Category: in.Category, Provider: in.Provider, PriceVersion: in.PriceVersion, AmountMicroUSD: in.AmountMicroUSD, Status: "reserved", Revision: 1}
	if s.mode == "revoke_after_reserve" {
		s.authorized = false
	}
	if s.mode == "reserve_ack_loss" {
		return domain.Reservation{}, errors.New("private reserve committed but reply lost")
	}
	return s.reservation, nil
}
func (s *localModeIndependentStore) PrepareModelDispatch(ctx context.Context, l *domain.Lease, id string) (domain.DispatchPermit, error) {
	s.mu.Lock()
	s.steps = append(s.steps, "prepare")
	if ctx.Err() != nil || !s.authorized || l.Token != s.fence || id != s.reservation.ID || s.reservation.Status != "reserved" || s.action != "pending" {
		s.mu.Unlock()
		return domain.DispatchPermit{}, errors.New("private dispatch denied")
	}
	s.action = "sent"
	mode := s.mode
	now := time.Now()
	age := 2 * time.Second
	if mode == "expired_permit" {
		age = time.Millisecond
	}
	p := domain.DispatchPermit{OrganizationID: l.OrganizationID, TaskID: l.TaskID, OperationID: l.OperationID, ActionID: s.reservation.ActionID, ReservationID: id, IntentHash: s.request.IntentHash, FencingToken: l.Token, IssuedAt: now, NotAfter: now.Add(age), ValidFor: age}
	s.mu.Unlock()
	if mode == "dispatch_ack_loss" {
		return domain.DispatchPermit{}, errors.New("private sent commit reply lost")
	}
	if mode == "wrong_fence" {
		p.FencingToken++
	}
	if mode == "wrong_intent" {
		p.IntentHash = strings.Repeat("0", 64)
	}
	if mode == "oversized_permit" {
		p.ValidFor = 3 * time.Second
		p.NotAfter = p.IssuedAt.Add(p.ValidFor)
	}
	if mode == "expired_permit" {
		time.Sleep(15 * time.Millisecond)
	}
	return p, nil
}
func (s *localModeIndependentStore) persistence(ctx context.Context) {
	_, ok := ctx.Deadline()
	s.freshPersistence = s.freshPersistence && ok && ctx.Err() == nil
}
func (s *localModeIndependentStore) MarkCostUnknown(ctx context.Context, org, id, key, reason string) (domain.Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, "unknown")
	s.persistence(ctx)
	if ctx.Err() != nil || org != s.reservation.OrganizationID || id != s.reservation.ID || key != "model:accounting-pending" || reason != modelruntime.PauseAccountingPending {
		return domain.Reservation{}, errors.New("private wrong accounting binding")
	}
	if s.mode == "unknown_write_failure" {
		return domain.Reservation{}, errors.New("private accounting unavailable")
	}
	s.reservation.Status = "unknown"
	if s.action == "sent" {
		s.action = "unknown"
	}
	return s.reservation, nil
}
func (s *localModeIndependentStore) CompleteModelCandidate(ctx context.Context, l *domain.Lease, id string, c domain.ModelCandidate) (domain.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, "candidate")
	s.persistence(ctx)
	if ctx.Err() != nil || !s.authorized || l.Token != s.fence || id != s.reservation.ID || s.reservation.Status != "unknown" {
		return domain.Artifact{}, errors.New("private publication authority refused")
	}
	s.candidate = c
	s.artifact = domain.Artifact{ID: "independent-zero-artifact", OrganizationID: l.OrganizationID, TaskID: l.TaskID, Content: c.Text, Provenance: map[string]any{"verified": false}}
	return s.artifact, nil
}
func (s *localModeIndependentStore) PauseModelAttempt(ctx context.Context, _ *domain.Lease, id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, "pause")
	s.persistence(ctx)
	if ctx.Err() != nil || id != s.reservation.ID {
		return errors.New("private bad pause binding")
	}
	s.pause = reason
	return nil
}
func (s *localModeIndependentStore) observe() (domain.Reservation, string, domain.ModelCandidate, domain.Artifact, []string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reservation, s.action, s.candidate, s.artifact, append([]string(nil), s.steps...), s.freshPersistence
}
func localModeIndependentLease() *domain.Lease {
	return &domain.Lease{OrganizationID: "independent-org", TaskID: "independent-task", RunID: "independent-run", OperationID: "independent-operation", OwnerID: "independent-worker", Token: 11,
		Task:  domain.Task{ID: "independent-task", OrganizationID: "independent-org", OwnerID: "independent-user", AgentID: "independent-agent", AgentVersion: 2, Goal: "Summarize a bounded sample", Status: "running"},
		Agent: domain.Agent{ID: "independent-agent", OrganizationID: "independent-org", Version: 2, Published: true, Model: "independent-local-model", Instructions: "Return one untrusted draft"}}
}
func localModeIndependentConfig(endpoint string) modelruntime.Config {
	return modelruntime.Config{Gateway: modelgateway.Config{Endpoint: endpoint, APIKey: "independent-local-placeholder", Model: "independent-local-model", MaxOutputTokens: 8, MaxInputBytes: 4096, Timeout: time.Second, AllowInsecureLoopback: true, TokenLimitField: "max_tokens"},
		Price: modelruntime.PriceProfile{AccountingMode: domain.ModelAccountingLocalUnmetered, ProviderAccount: "independent-local-account", Version: "local-zero-v1", Model: "independent-local-model", BoundAssumptions: "Declared zero API tariff; hardware unmeasured", MaxInputTokens: 50}, MaxOutputBytes: 1024, PersistenceTimeout: time.Second}
}
func localModeIndependentRuntime(t *testing.T, s *localModeIndependentStore, c modelruntime.Config) *modelruntime.Runtime {
	t.Helper()
	r, e := modelruntime.New(s, c)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func localModeIndependentHash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func localModeIndependentResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"id":"independent-local-response","choices":[{"message":{"role":"assistant","content":"Untrusted local draft"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
}

func TestLocalModeIndependentZeroResponseRetainsUnknownAndNoResend(t *testing.T) {
	s := localModeIndependentNewStore()
	var calls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		res, action, _, _, _, _ := s.observe()
		if res.ID == "" || res.AmountMicroUSD != 0 || action != "sent" {
			t.Error("zero call bypassed reserve/committed dispatch")
		}
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer independent-local-placeholder" || r.Header.Get("X-Request-ID") != "model:independent-operation" {
			t.Error("private request identity changed")
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || string(body["max_tokens"]) != "8" || string(body["stream"]) != "false" {
			t.Error("local mode lost finite request bounds")
		}
		localModeIndependentResponse(w)
	}))
	defer peer.Close()
	c := localModeIndependentConfig(peer.URL + "/v1/chat/completions")
	l := localModeIndependentLease()
	out, err := localModeIndependentRuntime(t, s, c).Execute(context.Background(), l)
	res, action, candidate, artifact, steps, fresh := s.observe()
	if err != nil || out.Status != modelruntime.CandidateWaitingInput || out.CostStatus != "unknown" || res.AmountMicroUSD != 0 || res.Status != "unknown" || res.ActualMicroUSD != nil || action != "unknown" || calls.Load() != 1 {
		t.Fatalf("zero call was skipped, settled or repeated: %+v %v %+v calls=%d", out, err, res, calls.Load())
	}
	if candidate.Text != "Untrusted local draft" || candidate.EstimatedMicroUSD != 0 || artifact.Provenance["verified"] != false || candidate.IntentDigest != s.request.IntentHash || !fresh || !reflect.DeepEqual(steps, []string{"reserve", "prepare", "unknown", "candidate"}) {
		t.Fatalf("zero changed observation/accounting order: %+v steps=%v", candidate, steps)
	}
	// New runtime and worker lease reuse the same unknown attempt, not a free retry.
	s.mu.Lock()
	s.fence = 12
	s.mu.Unlock()
	l.Token = 12
	l.OwnerID = "independent-replacement"
	out, err = localModeIndependentRuntime(t, s, c).Execute(context.Background(), l)
	if err != nil || out.Status != modelruntime.AlreadyAttempted || calls.Load() != 1 {
		t.Fatal("zero unknown was reissued")
	}
}

func TestLocalModeIndependentLostOrInvalidResponseStaysUnknown(t *testing.T) {
	for _, mode := range []string{"lost_reply", "malformed", "timeout", "oversized", "excess_usage", "revoked_after_io", "accounting_failure"} {
		t.Run(mode, func(t *testing.T) {
			s := localModeIndependentNewStore()
			var calls atomic.Int32
			release := make(chan struct{})
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch mode {
				case "lost_reply":
					conn, _, e := w.(http.Hijacker).Hijack()
					if e != nil {
						t.Error(e)
						return
					}
					conn.Close()
					return
				case "malformed":
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"choices":[]}`)
					return
				case "timeout":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				case "oversized":
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, strings.Repeat("x", 1025))
					return
				case "excess_usage":
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"choices":[{"message":{"content":"sample"}}],"usage":{"prompt_tokens":51,"completion_tokens":1}}`)
					return
				case "revoked_after_io":
					s.mu.Lock()
					s.authorized = false
					s.mu.Unlock()
				}
				localModeIndependentResponse(w)
			}))
			defer peer.Close()
			defer close(release)
			c := localModeIndependentConfig(peer.URL + "/v1/chat/completions")
			if mode == "timeout" {
				c.Gateway.Timeout = 40 * time.Millisecond
			}
			if mode == "accounting_failure" {
				s.mode = "unknown_write_failure"
			}
			l := localModeIndependentLease()
			out, err := localModeIndependentRuntime(t, s, c).Execute(context.Background(), l)
			res, action, _, artifact, _, fresh := s.observe()
			if err == nil || out.Status != modelruntime.OutcomeUnknown || artifact.ID != "" || res.ActualMicroUSD != nil || res.AmountMicroUSD != 0 || calls.Load() != 1 || !fresh {
				t.Fatalf("uncertain zero result published or billed: %+v %v %+v calls=%d", out, err, res, calls.Load())
			}
			if mode != "accounting_failure" && (res.Status != "unknown" || action != "unknown") {
				t.Fatal("zero accounting uncertainty lost")
			}
			if mode == "accounting_failure" && action != "sent" {
				t.Fatal("unacknowledged accounting erased sent marker")
			}
			_, _ = localModeIndependentRuntime(t, s, c).Execute(context.Background(), l)
			if calls.Load() != 1 {
				t.Fatal("uncertain zero outcome resent")
			}
		})
	}
}

func TestLocalModeIndependentAuthorityPermitAndCommitFaultsBeforeIO(t *testing.T) {
	for _, mode := range []string{"reserve_authority", "revoke_after_reserve", "stale_fence", "foreign_task", "cancelled_context", "reserve_ack_loss", "dispatch_ack_loss", "wrong_fence", "wrong_intent", "oversized_permit", "expired_permit"} {
		t.Run(mode, func(t *testing.T) {
			s := localModeIndependentNewStore()
			s.mode = mode
			var calls atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); localModeIndependentResponse(w) }))
			defer peer.Close()
			c := localModeIndependentConfig(peer.URL + "/v1/chat/completions")
			l := localModeIndependentLease()
			ctx := context.Background()
			switch mode {
			case "reserve_authority":
				s.authorized = false
			case "stale_fence":
				l.Token--
			case "foreign_task":
				l.Task.OrganizationID = "foreign"
			case "cancelled_context":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			_, err := localModeIndependentRuntime(t, s, c).Execute(ctx, l)
			if err == nil || calls.Load() != 0 {
				t.Fatal("zero mode bypassed authority/ambiguity/permit refusal")
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatal("underlying storage failure leaked")
			}
			res, action, _, artifact, _, _ := s.observe()
			if artifact.ID != "" || res.ActualMicroUSD != nil {
				t.Fatal("failed admission created result/final bill")
			}
			if mode == "dispatch_ack_loss" || mode == "wrong_fence" || mode == "wrong_intent" || mode == "oversized_permit" || mode == "expired_permit" {
				if action != "unknown" || res.Status != "unknown" {
					t.Fatal("committed marker/zero uncertainty disappeared")
				}
				_, _ = localModeIndependentRuntime(t, s, c).Execute(context.Background(), l)
				if calls.Load() != 0 {
					t.Fatal("ambiguous committed marker reissued")
				}
			}
		})
	}
}

func TestLocalModeIndependentExplicitModeAndCanonicalEndpoint(t *testing.T) {
	base := "http://127.0.0.1:12345/v1/chat/completions"
	changes := map[string]func(*modelruntime.Config){
		"legacy_zero": func(c *modelruntime.Config) { c.Price.AccountingMode = "" }, "metered_zero": func(c *modelruntime.Config) { c.Price.AccountingMode = domain.ModelAccountingMetered },
		"unknown_mode": func(c *modelruntime.Config) { c.Price.AccountingMode = "free" }, "case_alias": func(c *modelruntime.Config) { c.Price.AccountingMode = "OPERATOR_UNMETERED_LOCAL" }, "space_alias": func(c *modelruntime.Config) { c.Price.AccountingMode += " " },
		"input_rate": func(c *modelruntime.Config) { c.Price.InputMicroUSDPerMillion = 1 }, "output_rate": func(c *modelruntime.Config) { c.Price.OutputMicroUSDPerMillion = 1 }, "fixed": func(c *modelruntime.Config) { c.Price.FixedMicroUSD = 1 }, "positive_local_bound": func(c *modelruntime.Config) { c.Price.MaxChargeMicroUSD = 1 }, "negative_bound": func(c *modelruntime.Config) { c.Price.MaxChargeMicroUSD = -1 }, "missing_loopback_optin": func(c *modelruntime.Config) { c.Gateway.AllowInsecureLoopback = false },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			c := localModeIndependentConfig(base)
			change(&c)
			if _, err := modelruntime.New(localModeIndependentNewStore(), c); err == nil {
				t.Fatal("contradictory accounting profile accepted")
			}
		})
	}
	for _, endpoint := range []string{"https://api.example.invalid/v1/chat/completions", "https://127.0.0.1:12345/v1/chat/completions", "http://localhost:12345/v1/chat/completions", "http://[::1]:12345/v1/chat/completions", "http://127.0.0.2:12345/v1/chat/completions", "http://127.0.0.1:012345/v1/chat/completions", base + "?", base + "#", base + "?forward=cloud", "http://@127.0.0.1:12345/v1/chat/completions", "http://user:private@127.0.0.1:12345/v1/chat/completions", base + "/", "http://127.0.0.1:12345/v1/%63hat/completions", " " + base, "http://127.0.0.1:0/v1/chat/completions", "http://127.0.0.1:65536/v1/chat/completions", "http://127.0.0.1/v1/chat/completions"} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := modelruntime.New(localModeIndependentNewStore(), localModeIndependentConfig(endpoint)); err == nil {
				t.Fatal("noncanonical local endpoint accepted")
			}
		})
	}
	for _, port := range []string{"1", "65535"} {
		t.Run("canonical_port_"+port, func(t *testing.T) {
			if _, err := modelruntime.New(localModeIndependentNewStore(), localModeIndependentConfig("http://127.0.0.1:"+port+"/v1/chat/completions")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLocalModeIndependentLegacyReferenceVectors(t *testing.T) {
	// Python stdlib json.dumps(separators=(',',':')) + hashlib SHA-256 reference
	// vectors use the historical published field order/shape, not runtime output.
	c := localModeIndependentConfig("http://127.0.0.1:12345/v1/chat/completions")
	c.Price = modelruntime.PriceProfile{ProviderAccount: "independent-local-account", Version: "legacy-price-v1", Model: "independent-local-model", BoundAssumptions: "Independent fixture; no real provider invoice", MaxChargeMicroUSD: 75, MaxInputTokens: 50, InputMicroUSDPerMillion: 1000000, OutputMicroUSDPerMillion: 2000000, FixedMicroUSD: 7}
	price, err := json.Marshal(c.Price)
	if err != nil {
		t.Fatal(err)
	}
	if localModeIndependentHash(price) != "f77a919b87ce4bf963f59cff4f16adca4f5d284e83f3ee950702d325a1f438a5" || strings.Contains(string(price), "accounting_mode") {
		t.Fatalf("legacy price shape changed: %s", price)
	}
	r := localModeIndependentRuntime(t, localModeIndependentNewStore(), c)
	key, intent, err := r.DescribeIntent(*localModeIndependentLease())
	if err != nil || key != "model:independent-operation" || intent != "1114f51a6ec48ac4c14d1a965505e6daef43ddc7a7ae3b76f0d4f54333c0edca" {
		t.Fatalf("legacy immutable intent changed: %s %s %v", key, intent, err)
	}
	if domain.EffectiveModelAccountingMode(c.Price.AccountingMode) != domain.ModelAccountingMetered || c.Price.AccountingMode != "" {
		t.Fatal("display accessor normalized raw immutable mode")
	}
	connection := domain.ModelConnection{ID: "legacy-model", OrganizationID: "independent-org", Version: 1, Model: c.Price.Model, ProviderAccount: c.Price.ProviderAccount, PriceVersion: c.Price.Version, Endpoint: c.Gateway.Endpoint, CredentialEnvironment: "PLATFORM_MODEL_KEY_INDEPENDENT_LOCAL", MaxChargeMicroUSD: 75, MaxInputTokens: 50, InputMicroUSDPerMillion: 1000000, OutputMicroUSDPerMillion: 2000000, FixedMicroUSD: 7, MaxOutputTokens: 8, MaxInputBytes: 4096, MaxOutputBytes: 1024, TimeoutMilliseconds: 1000, BoundAssumptions: c.Price.BoundAssumptions, TokenLimitField: "max_tokens", AllowInsecureLoopback: true}
	b, err := json.Marshal(connection)
	if err != nil || localModeIndependentHash(b) != "699b52f541b5d49122a6f64e1dbc8f3ac0335f1b2cce733041a6050db15c8995" {
		t.Fatalf("legacy private profile shape changed: %s %v", b, err)
	}
	c.Price.AccountingMode = domain.ModelAccountingMetered
	_, explicit, err := localModeIndependentRuntime(t, localModeIndependentNewStore(), c).DescribeIntent(*localModeIndependentLease())
	if err != nil || explicit == intent {
		t.Fatal("explicit new mode did not bind new identity")
	}
}
