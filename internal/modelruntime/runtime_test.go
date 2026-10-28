// SPDX-License-Identifier: Elastic-2.0
package modelruntime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelgateway"
	"agentplatform.local/platform/internal/modelruntime"
)

// This fault store models the agreed transactional boundary. It is not a
// substitute for PostgreSQL tests of the bridge, locks or durable crash recovery.
type faultStore struct {
	mu                  sync.Mutex
	reservation         domain.Reservation
	intent              domain.ReserveCostRequest
	action              string
	fence               int64
	authorized          bool
	cancelled           bool
	prepareCommitError  bool
	reserveCommitError  bool
	unknownError        bool
	completeCommitError bool
	artifact            domain.Artifact
	candidate           domain.ModelCandidate
	pauseReason         string
	steps               []string
	contextsValid       bool
}

func newStore() *faultStore { return &faultStore{fence: 1, authorized: true, contextsValid: true} }
func (s *faultStore) authority(l *domain.Lease) bool {
	return s.authorized && !s.cancelled && l.Token == s.fence
}
func (s *faultStore) ReserveCost(ctx context.Context, l *domain.Lease, in domain.ReserveCostRequest) (domain.Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, "reserve")
	if ctx.Err() != nil || !s.authority(l) {
		return domain.Reservation{}, errors.New("secret storage authority reason")
	}
	if s.reservation.ID != "" {
		if s.intent != in {
			return domain.Reservation{}, errors.New("immutable attempt conflict")
		}
		return s.reservation, nil
	}
	s.intent = in
	s.reservation = domain.Reservation{ID: "res_test", OrganizationID: l.OrganizationID, TaskID: l.TaskID, OperationID: l.OperationID, ActionID: "act_test", ChargeKey: in.ChargeKey, Category: in.Category, Provider: in.Provider, PriceVersion: in.PriceVersion, AmountMicroUSD: in.AmountMicroUSD, Status: "reserved", Revision: 1}
	s.action = "pending"
	if s.reserveCommitError {
		return domain.Reservation{}, errors.New("secret lost reservation acknowledgment")
	}
	return s.reservation, nil
}
func (s *faultStore) PrepareCostDispatch(ctx context.Context, l *domain.Lease, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, "prepare")
	if ctx.Err() != nil || !s.authority(l) || s.reservation.ID != id || s.action != "pending" || s.reservation.Status != "reserved" {
		return errors.New("secret dispatch refusal")
	}
	s.action = "sent"
	if s.prepareCommitError {
		return errors.New("secret ambiguous commit")
	}
	return nil
}
func (s *faultStore) PrepareModelDispatch(ctx context.Context, l *domain.Lease, id string) (domain.DispatchPermit, error) {
	if e := s.PrepareCostDispatch(ctx, l, id); e != nil {
		return domain.DispatchPermit{}, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	issued := time.Now()
	return domain.DispatchPermit{OrganizationID: l.OrganizationID, TaskID: l.TaskID, OperationID: l.OperationID, ActionID: s.reservation.ActionID, ReservationID: id, IntentHash: s.intent.IntentHash, FencingToken: l.Token, IssuedAt: issued, NotAfter: issued.Add(2 * time.Second), ValidFor: 2 * time.Second}, nil
}

func (s *faultStore) MarkCostUnknown(ctx context.Context, org, id, key, reason string) (domain.Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, "unknown")
	_, bounded := ctx.Deadline()
	s.contextsValid = s.contextsValid && ctx.Err() == nil && bounded
	if s.unknownError || ctx.Err() != nil || org != s.reservation.OrganizationID || id != s.reservation.ID || key != "model:accounting-pending" || reason != modelruntime.PauseAccountingPending {
		return domain.Reservation{}, errors.New("secret accounting failure")
	}
	if s.reservation.Status == "settled" || s.reservation.Status == "released" {
		return domain.Reservation{}, errors.New("final record already exists")
	}
	s.reservation.Status = "unknown"
	if s.action == "sent" {
		s.action = "unknown"
	}
	return s.reservation, nil
}
func (s *faultStore) CompleteModelCandidate(ctx context.Context, l *domain.Lease, id string, c domain.ModelCandidate) (domain.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, "candidate")
	_, bounded := ctx.Deadline()
	s.contextsValid = s.contextsValid && ctx.Err() == nil && bounded
	if ctx.Err() != nil || !s.authority(l) || s.reservation.ID != id || s.reservation.Status != "unknown" {
		return domain.Artifact{}, errors.New("secret stale publication")
	}
	s.candidate = c
	s.artifact = domain.Artifact{ID: "art_candidate", OrganizationID: l.OrganizationID, TaskID: l.TaskID, Content: c.Text, Provenance: map[string]any{"verified": false}}
	if s.completeCommitError {
		return domain.Artifact{}, errors.New("secret candidate commit acknowledgment")
	}
	return s.artifact, nil
}
func (s *faultStore) PauseModelAttempt(ctx context.Context, l *domain.Lease, id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, "pause")
	_, bounded := ctx.Deadline()
	s.contextsValid = s.contextsValid && ctx.Err() == nil && bounded
	if ctx.Err() != nil || !s.authority(l) || s.reservation.ID != id {
		return errors.New("secret stale pause")
	}
	s.pauseReason = reason
	return nil
}

func testLease() *domain.Lease {
	return &domain.Lease{OrganizationID: "org_test", OperationID: "op_test", TaskID: "tsk_test", RunID: "run_test", OwnerID: "worker_one", Token: 1,
		Task:  domain.Task{ID: "tsk_test", OrganizationID: "org_test", OwnerID: "usr_test", AgentID: "agt_test", AgentVersion: 2, Goal: "Summarize this bounded test input", Status: "running"},
		Agent: domain.Agent{ID: "agt_test", OrganizationID: "org_test", Version: 2, Published: true, Model: "local-http-test-text", Instructions: "Return one short plain text answer", Tools: []string{}},
	}
}
func testConfig(endpoint string) modelruntime.Config {
	return modelruntime.Config{Gateway: modelgateway.Config{Endpoint: endpoint, APIKey: "test-only-not-a-provider-credential", Model: "local-http-test-text", MaxOutputTokens: 16, MaxInputBytes: 4096, Timeout: time.Second, AllowInsecureLoopback: true, TokenLimitField: "max_tokens"},
		Price: modelruntime.PriceProfile{ProviderAccount: "test-http-account", Version: "test-price-v1", Model: "local-http-test-text", BoundAssumptions: "Local test assumption: only stated input/output tokens and fixed fee, no hidden charges; not a real provider invoice", MaxChargeMicroUSD: 150, MaxInputTokens: 100, InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 2_000_000, FixedMicroUSD: 7}, MaxOutputBytes: 1024, PersistenceTimeout: time.Second}
}
func newRuntime(t *testing.T, s *faultStore, c modelruntime.Config) *modelruntime.Runtime {
	t.Helper()
	r, e := modelruntime.New(s, c)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func success(w http.ResponseWriter, text string, input, output int64) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "provider-test-request", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": input, "completion_tokens": output}})
}
func checkCode(t *testing.T, err error, code string, unknown bool) {
	t.Helper()
	var e *modelruntime.ExecutionError
	if !errors.As(err, &e) || e.Code != code || e.Unknown != unknown {
		t.Fatalf("error = %v, want %s unknown=%v", err, code, unknown)
	}
}

func TestSuccessfulResponseIsCandidateAndUnknownCost(t *testing.T) {
	s := newStore()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		s.mu.Lock()
		sent := s.action == "sent"
		s.steps = append(s.steps, "provider")
		s.mu.Unlock()
		if !sent {
			t.Error("provider reached before durable dispatch")
		}
		if r.Header.Get("Authorization") != "Bearer test-only-not-a-provider-credential" || r.Header.Get("X-Request-ID") != "model:op_test" {
			t.Error("wrong private credential/request binding")
		}
		var wire map[string]any
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			t.Error(err)
		}
		if wire["max_tokens"] != float64(16) || wire["stream"] != false || wire["model"] != "local-http-test-text" {
			t.Error("wire policy lost")
		}
		success(w, "candidate only", 3, 2)
	}))
	defer server.Close()
	r := newRuntime(t, s, testConfig(server.URL))
	out, err := r.Execute(context.Background(), testLease())
	if err != nil || out.Status != modelruntime.CandidateWaitingInput || out.CostStatus != "unknown" || out.ArtifactID != "art_candidate" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if calls.Load() != 1 || s.reservation.Status != "unknown" || s.reservation.ActualMicroUSD != nil || s.reservation.AmountMicroUSD != 150 || s.candidate.EstimatedMicroUSD != 14 || s.candidate.Text != "candidate only" || s.artifact.Provenance["verified"] != false {
		t.Fatal("candidate was lost, billed or accepted")
	}
	if !reflect.DeepEqual(s.steps, []string{"reserve", "prepare", "provider", "unknown", "candidate"}) || !s.contextsValid {
		t.Fatalf("order/contexts: %v", s.steps)
	}
	if len(s.candidate.IntentDigest) != 64 || s.candidate.IntentDigest != s.intent.IntentHash {
		t.Fatal("candidate intent binding lost")
	}
	// A recreated runtime and a new lease reuse the persisted physical attempt.
	s.fence = 2
	l := testLease()
	l.Token = 2
	l.OwnerID = "worker_two"
	out, err = newRuntime(t, s, testConfig(server.URL)).Execute(context.Background(), l)
	if err != nil || out.Status != modelruntime.AlreadyAttempted || calls.Load() != 1 {
		t.Fatalf("reclaimed attempt resent: %+v %v calls=%d", out, err, calls.Load())
	}
}

func TestInvalidProviderResponsesRetainHoldAndPause(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		code    string
	}{
		{"invalid_usage", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"content":"answer"}}],"usage":{"prompt_tokens":-1,"completion_tokens":1}}`)
		}, "provider_outcome_unknown"},
		{"input_contract_exceeded", func(w http.ResponseWriter, r *http.Request) { success(w, "answer", 101, 2) }, "model_output_rejected"},
		{"output_limit", func(w http.ResponseWriter, r *http.Request) { success(w, "answer", 3, 17) }, "provider_outcome_unknown"},
		{"nul_output", func(w http.ResponseWriter, r *http.Request) { success(w, "before\x00after", 3, 2) }, "model_output_rejected"},
		{"oversized_candidate", func(w http.ResponseWriter, r *http.Request) { success(w, strings.Repeat("x", 1025), 3, 2) }, "model_output_rejected"},
		{"rate_limit", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(429)
			fmt.Fprint(w, "private provider body")
		}, "provider_outcome_unknown"},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "http://127.0.0.1:1/secret")
			w.WriteHeader(307)
		}, "provider_outcome_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); tc.handler(w, r) }))
			defer server.Close()
			r := newRuntime(t, s, testConfig(server.URL))
			out, err := r.Execute(context.Background(), testLease())
			checkCode(t, err, tc.code, true)
			if out.Status != modelruntime.OutcomeUnknown || s.reservation.Status != "unknown" || s.artifact.ID != "" || s.pauseReason == "" || calls.Load() != 1 {
				t.Fatal("invalid response was accepted or hold lost")
			}
			_, err = r.Execute(context.Background(), testLease())
			if err != nil || calls.Load() != 1 {
				t.Fatal("unknown response retried")
			}
		})
	}
}

func TestTimeoutAfterAcceptanceRetainsExposureWithoutRetry(t *testing.T) {
	s := newStore()
	var calls atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	c := testConfig(server.URL)
	c.Gateway.Timeout = 50 * time.Millisecond
	r := newRuntime(t, s, c)
	_, err := r.Execute(context.Background(), testLease())
	checkCode(t, err, "provider_outcome_unknown", true)
	if s.reservation.Status != "unknown" || s.pauseReason != modelruntime.PauseOutcomeUnknown || !s.contextsValid {
		t.Fatal("timeout released exposure or cancelled accounting")
	}
	_, err = r.Execute(context.Background(), testLease())
	if err != nil || calls.Load() != 1 {
		t.Fatal("timeout was retried")
	}
}

func TestCancellationAndRevocationSuppressPublicationButKeepAccounting(t *testing.T) {
	for _, mode := range []string{"cancel_context", "cancel_task", "revoke", "stale_lease"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				s.mu.Lock()
				switch mode {
				case "cancel_task":
					s.cancelled = true
				case "revoke":
					s.authorized = false
				case "stale_lease":
					s.fence = 2
				}
				s.mu.Unlock()
				if mode == "cancel_context" {
					cancel()
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				success(w, "must not publish", 3, 2)
			}))
			defer server.Close()
			defer close(release)
			_, err := newRuntime(t, s, testConfig(server.URL)).Execute(ctx, testLease())
			if mode == "cancel_context" {
				checkCode(t, err, "provider_outcome_unknown", true)
			} else {
				checkCode(t, err, "candidate_not_committed", true)
			}
			if s.reservation.Status != "unknown" || s.artifact.ID != "" || !s.contextsValid {
				t.Fatal("authority loss erased cost or published output")
			}
		})
	}
}

func TestCrashBoundariesAndAmbiguousCommitsCannotResend(t *testing.T) {
	for _, mode := range []string{"reserve_commit", "prepare_commit", "sent_before_response", "unknown_persistence", "candidate_commit"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); success(w, "candidate", 3, 2) }))
			defer server.Close()
			c := testConfig(server.URL)
			switch mode {
			case "reserve_commit":
				s.reserveCommitError = true
			case "prepare_commit":
				s.prepareCommitError = true
			case "unknown_persistence":
				s.unknownError = true
			case "candidate_commit":
				s.completeCommitError = true
			case "sent_before_response":
				// Inject the durable state left by a process that disappeared after
				// dispatch. No test assumes that its network outcome is knowable.
				s.prepareCommitError = true
			}
			_, err := newRuntime(t, s, c).Execute(context.Background(), testLease())
			if err == nil {
				t.Fatal("fault not injected")
			}
			firstCalls := calls.Load()
			s.reserveCommitError = false
			s.prepareCommitError = false
			s.unknownError = false
			s.completeCommitError = false
			s.fence = 2
			l := testLease()
			l.Token = 2
			l.OwnerID = "replacement_worker"
			_, _ = newRuntime(t, s, c).Execute(context.Background(), l)
			if mode == "reserve_commit" {
				if calls.Load() != 1 || firstCalls != 0 {
					t.Fatal("unsent reserve was not safely admitted once")
				}
			} else if calls.Load() != firstCalls {
				t.Fatal("uncertain committed dispatch was resent")
			}
			if s.reservation.Status != "unknown" {
				t.Fatal("attempt exposure did not survive restart")
			}
		})
	}
}

func TestConcurrentSameAttemptHasAtMostOneExternalCall(t *testing.T) {
	s := newStore()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); success(w, "candidate", 3, 2) }))
	defer server.Close()
	r := newRuntime(t, s, testConfig(server.URL))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, _ = r.Execute(context.Background(), testLease()) }()
	}
	close(start)
	wg.Wait()
	if calls.Load() > 1 || s.reservation.Status != "unknown" {
		t.Fatalf("calls=%d status=%s", calls.Load(), s.reservation.Status)
	}
	// Races can conservatively suppress dispatch; they must never multiply it.
}

func TestChangedPinnedIntentOrPriceCannotReuseAttempt(t *testing.T) {
	s := newStore()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); success(w, "candidate", 3, 2) }))
	defer server.Close()
	c := testConfig(server.URL)
	_, err := newRuntime(t, s, c).Execute(context.Background(), testLease())
	if err != nil {
		t.Fatal(err)
	}
	l := testLease()
	l.Agent.Instructions = "changed instruction"
	_, err = newRuntime(t, s, c).Execute(context.Background(), l)
	checkCode(t, err, "reservation_not_admitted", false)
	c.Price.Version = "new-price"
	_, err = newRuntime(t, s, c).Execute(context.Background(), testLease())
	checkCode(t, err, "reservation_not_admitted", false)
	if calls.Load() != 1 {
		t.Fatal("changed intent re-executed paid attempt")
	}
}

func TestPreflightRejectsUnsupportedAndOversizedRequestsBeforeReserve(t *testing.T) {
	for _, mode := range []string{"tools", "wrong_model", "wrong_tenant", "unpublished", "wrong_version", "input_bytes", "cancelled_context"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore()
			c := testConfig("http://127.0.0.1:1/text")
			l := testLease()
			ctx := context.Background()
			switch mode {
			case "tools":
				l.Agent.Tools = []string{"shell"}
			case "wrong_model":
				l.Agent.Model = "another"
			case "wrong_tenant":
				l.Task.OrganizationID = "other"
			case "unpublished":
				l.Agent.Published = false
			case "wrong_version":
				l.Task.AgentVersion++
			case "input_bytes":
				l.Task.Goal = strings.Repeat("\n", 3000)
			case "cancelled_context":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			_, err := newRuntime(t, s, c).Execute(ctx, l)
			if err == nil || len(s.steps) != 0 || s.reservation.ID != "" {
				t.Fatal("preflight reached storage/network")
			}
		})
	}
}

func TestConfigBoundsAndSafeErrors(t *testing.T) {
	for _, change := range []func(*modelruntime.Config){
		func(c *modelruntime.Config) { c.Price.MaxChargeMicroUSD = 138 },
		func(c *modelruntime.Config) { c.Price.BoundAssumptions = "" },
		func(c *modelruntime.Config) { c.Price.Model = "mismatch" },
		func(c *modelruntime.Config) { c.Price.MaxInputTokens = 1_000_000_001 },
		func(c *modelruntime.Config) {
			c.Gateway.Model = domain.FixtureModel
			c.Price.Model = domain.FixtureModel
		},
		func(c *modelruntime.Config) { c.MaxOutputBytes = 0 },
		func(c *modelruntime.Config) { c.PersistenceTimeout = 31 * time.Second },
		func(c *modelruntime.Config) {
			c.Gateway.Endpoint = "https://user:secret@example.invalid/text?api_key=private"
		},
	} {
		c := testConfig("http://127.0.0.1:1/text")
		change(&c)
		_, err := modelruntime.New(newStore(), c)
		if err == nil {
			t.Fatal("invalid config accepted")
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "example.invalid") {
			t.Fatal("configuration leaked")
		}
	}
	c := testConfig("http://127.0.0.1:1/text")
	s := newStore()
	s.authorized = false
	r := newRuntime(t, s, c)
	_, err := r.Execute(context.Background(), testLease())
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("storage error leaked")
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(format, c), c.Gateway.APIKey) || strings.Contains(fmt.Sprintf(format, r), c.Gateway.APIKey) {
			t.Fatal("formatted config leaked credential")
		}
	}
}

func TestIntegerTariffRoundingAndOverflow(t *testing.T) {
	for _, tc := range []struct{ input, output, rateIn, rateOut, fixed, want int64 }{
		{1, 1, 1, 1, 0, 2}, {0, 0, 999, 999, 7, 7}, {1_000_001, 1, 1, 1_000_001, 3, 7},
		{1_000_000_000, 0, 10_000_000_000, 0, 0, 10_000_000_000_000},
	} {
		p := modelruntime.PriceProfile{InputMicroUSDPerMillion: tc.rateIn, OutputMicroUSDPerMillion: tc.rateOut, FixedMicroUSD: tc.fixed}
		got, err := p.Estimate(tc.input, tc.output)
		if err != nil || got != tc.want {
			t.Fatalf("estimate=%d err=%v want=%d", got, err, tc.want)
		}
	}
	p := modelruntime.PriceProfile{InputMicroUSDPerMillion: math.MaxInt64, OutputMicroUSDPerMillion: math.MaxInt64}
	if _, err := p.Estimate(math.MaxInt64, math.MaxInt64); err == nil {
		t.Fatal("overflow accepted")
	}
	if _, err := p.Estimate(-1, 1); err == nil {
		t.Fatal("negative usage accepted")
	}
}
