// SPDX-License-Identifier: Elastic-2.0
package storage

// Independent tests combine the real PostgreSQL bridge and actual localhost
// HTTP requests. The provider is an explicit test fixture; its usage is not a
// final bill. Fault injection is not represented as a real process/power loss.
import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
	"github.com/jackc/pgx/v5"
)

type independentModelFixture struct {
	s      *Store
	dsn    string
	p      domain.Principal
	c      domain.ModelConnection
	task   domain.Task
	lease  *domain.Lease
	calls  atomic.Int64
	server *httptest.Server
}

func independentModelReply(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"independent-provider-request","choices":[{"index":0,"message":{"role":"assistant","content":"Unverified candidate from local provider fixture"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
}

func independentModelSetup(t *testing.T, claim bool, handler http.HandlerFunc) *independentModelFixture {
	t.Helper()
	f := &independentModelFixture{}
	f.s, f.dsn = testStore(t)
	f.p, _ = identity(t, f.s, 100_000)
	if handler == nil {
		handler = independentModelReply
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer independent-local-test-key" {
			t.Error("provider fixture received an unexpected method or credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(f.server.Close)
	f.c = domain.ModelConnection{
		ID: "independent-local-model", OrganizationID: f.p.OrganizationID, Version: 1,
		Model: "independent-http-fixture", ProviderAccount: "independent-test-account", PriceVersion: "local-test-price-v1",
		Endpoint: f.server.URL, CredentialEnvironment: "PLATFORM_MODEL_KEY_INDEPENDENT",
		MaxChargeMicroUSD: 150, MaxInputTokens: 100, MaxOutputTokens: 16,
		InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 2_000_000, FixedMicroUSD: 7,
		MaxInputBytes: 4096, MaxOutputBytes: 1024, TimeoutMilliseconds: 2000,
		BoundAssumptions: "Local test fixture only; input token bound is an assumption, not measured from bytes; usage does not authorize billing settlement.",
		TokenLimitField:  "max_tokens", AllowInsecureLoopback: true, Active: true,
	}
	var err error
	f.c, err = f.s.InstallModelConnection(context.Background(), f.c)
	if err != nil {
		t.Fatal(err)
	}
	budget := int64(10_000)
	raw, err := f.s.CreateAgent(context.Background(), f.p, domain.NewID("key"), domain.CreateAgent{Name: "Independent model agent", Instructions: "Return a test candidate", Model: f.c.Model, BudgetMicroUSD: &budget})
	if err != nil {
		t.Fatal(err)
	}
	var agent domain.Agent
	if err = json.Unmarshal(raw, &agent); err != nil {
		t.Fatal(err)
	}
	raw, err = f.s.PublishAgent(context.Background(), f.p, agent.ID, domain.NewID("key"), agent.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &agent); err != nil {
		t.Fatal(err)
	}
	f.task = task(t, f.s, f.p, agent)
	if f.task.ModelConnectionID != f.c.ID || f.task.ModelConnectionVersion != 1 {
		t.Fatal("task did not pin the admitted immutable model profile")
	}
	if claim {
		var c domain.ModelConnection
		f.lease, c, err = f.s.ClaimModel(context.Background(), "independent-model-worker", 30*time.Second)
		if err != nil || f.lease == nil {
			t.Fatalf("claim: lease=%v err=%v", f.lease != nil, err)
		}
		if c.ID != f.c.ID || c.Version != f.c.Version {
			t.Fatal("claimed profile does not match task pin")
		}
	}
	return f
}

func (f *independentModelFixture) runtime(t *testing.T, store modelruntime.Store) *modelruntime.Runtime {
	t.Helper()
	r, err := modelruntime.New(store, RuntimeModelConfig(f.c, "independent-local-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *independentModelFixture) reserve(t *testing.T) domain.Reservation {
	t.Helper()
	key, digest, err := f.runtime(t, f.s).DescribeIntent(*f.lease)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.s.ReserveCost(context.Background(), f.lease, domain.ReserveCostRequest{ChargeKey: key, IntentHash: digest, Category: "model", Provider: f.c.ProviderAccount, PriceVersion: f.c.PriceVersion, AmountMicroUSD: f.c.MaxChargeMicroUSD})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func independentModelError(t *testing.T, err error, want string, unknown bool) {
	t.Helper()
	var runtimeErr *modelruntime.ExecutionError
	if !errors.As(err, &runtimeErr) || runtimeErr.Code != want || runtimeErr.Unknown != unknown {
		t.Fatalf("wanted runtime error %s unknown=%v, got %v", want, unknown, err)
	}
}

func (f *independentModelFixture) artifacts(t *testing.T, want int) []domain.Artifact {
	t.Helper()
	values, err := f.s.Artifacts(context.Background(), f.p, f.task.ID)
	if err != nil || len(values) != want {
		t.Fatalf("artifacts=%d want=%d err=%v", len(values), want, err)
	}
	return values
}

func (f *independentModelFixture) balances(t *testing.T, held int64) {
	t.Helper()
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, held, 0)
}

func (f *independentModelFixture) states(t *testing.T, taskState, reason, operationState string) {
	t.Helper()
	v, err := f.s.GetTask(context.Background(), f.p, f.task.ID)
	if err != nil || v.Status != taskState || v.WaitingReason != reason {
		t.Fatalf("task state %s/%s, want %s/%s; err=%v", v.Status, v.WaitingReason, taskState, reason, err)
	}
	var op string
	if err = f.s.Pool.QueryRow(context.Background(), "SELECT status FROM operations WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&op); err != nil || op != operationState {
		t.Fatalf("operation=%s want=%s err=%v", op, operationState, err)
	}
}

func (f *independentModelFixture) reservationState(t *testing.T, id, reservation, action string) {
	t.Helper()
	var rs, as string
	if err := f.s.Pool.QueryRow(context.Background(), "SELECT r.status,a.status FROM reservations r JOIN actions a ON a.organization_id=r.organization_id AND a.id=r.action_id WHERE r.organization_id=$1 AND r.id=$2", f.p.OrganizationID, id).Scan(&rs, &as); err != nil || rs != reservation || as != action {
		t.Fatalf("reservation/action=%s/%s want=%s/%s err=%v", rs, as, reservation, action, err)
	}
}

func TestIndependentModelCandidateIsDurableUnverifiedAndUnsettled(t *testing.T) {
	f := independentModelSetup(t, true, nil)
	out, err := f.runtime(t, f.s).Execute(context.Background(), f.lease)
	if err != nil || out.Status != modelruntime.CandidateWaitingInput || out.CostStatus != "unknown" || out.ArtifactID == "" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("HTTP calls=%d", f.calls.Load())
	}
	f.states(t, "waiting_input", "model_candidate_pending", "blocked")
	f.reservationState(t, out.ReservationID, "unknown", "unknown")
	f.balances(t, 150)
	values := f.artifacts(t, 1)
	p := values[0].Provenance
	if values[0].ID != out.ArtifactID || p["verified"] != false || p["verification"] != "awaiting_human" || p["estimated_microusd"] != "14" || p["cost_status"] != "unknown" || p["model_connection_id"] != f.c.ID || p["provider_request_id"] != "independent-provider-request" {
		t.Fatalf("incorrect candidate provenance: %+v", p)
	}
	reopened, err := Open(context.Background(), f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.GetTask(context.Background(), f.p, f.task.ID)
	if err != nil || persisted.Status != "waiting_input" || persisted.ReservedMicroUSD != 150 || persisted.SpentMicroUSD != 0 {
		t.Fatalf("reopened task state=%s held=%d spent=%d err=%v", persisted.Status, persisted.ReservedMicroUSD, persisted.SpentMicroUSD, err)
	}
	lease, _, err := reopened.ClaimModel(context.Background(), "replacement-worker", 30*time.Second)
	if err != nil || lease != nil || f.calls.Load() != 1 {
		t.Fatal("candidate waiting for human acceptance became automatically runnable")
	}
}

func TestIndependentModelRejectsForgedCanonicalIntentBeforeHTTP(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*domain.Lease, *modelruntime.Config)
	}{
		{"goal", func(l *domain.Lease, c *modelruntime.Config) { l.Task.Goal = "A forged goal" }},
		{"instructions", func(l *domain.Lease, c *modelruntime.Config) { l.Agent.Instructions = "A forged system instruction" }},
		{"endpoint", func(l *domain.Lease, c *modelruntime.Config) { c.Gateway.Endpoint += "/forged" }},
		{"input_bound", func(l *domain.Lease, c *modelruntime.Config) { c.Gateway.MaxInputBytes++ }},
		{"output_bound", func(l *domain.Lease, c *modelruntime.Config) { c.MaxOutputBytes++ }},
		{"price", func(l *domain.Lease, c *modelruntime.Config) { c.Price.FixedMicroUSD++ }},
		{"assumptions", func(l *domain.Lease, c *modelruntime.Config) { c.Price.BoundAssumptions = "A forged assumption" }},
		{"timeout", func(l *domain.Lease, c *modelruntime.Config) { c.Gateway.Timeout += time.Millisecond }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := independentModelSetup(t, true, nil)
			l := *f.lease
			config := RuntimeModelConfig(f.c, "independent-local-test-key")
			tt.mutate(&l, &config)
			r, err := modelruntime.New(f.s, config)
			if err != nil {
				t.Fatal("test mutation should be locally valid:", err)
			}
			out, err := r.Execute(context.Background(), &l)
			independentModelError(t, err, "reservation_not_admitted", false)
			if out.Status != modelruntime.NotDispatched || f.calls.Load() != 0 {
				t.Fatalf("forged intent sent: %+v calls=%d", out, f.calls.Load())
			}
			f.balances(t, 0)
			f.artifacts(t, 0)
		})
	}
}

func independentModelWait(t *testing.T, predicate func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for " + description)
}

func (f *independentModelFixture) shortenLease(t *testing.T, interval string) {
	t.Helper()
	if _, err := f.s.Pool.Exec(context.Background(), "UPDATE operations SET lease_until=clock_timestamp()+$3::interval WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.lease.OperationID, interval); err != nil {
		t.Fatal(err)
	}
}

func (f *independentModelFixture) lockAction(t *testing.T, id string) (pgx.Tx, int) {
	t.Helper()
	tx, err := f.s.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var pid int
	if err = tx.QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(context.Background(), "SELECT id FROM actions WHERE organization_id=$1 AND id=$2 FOR UPDATE", f.p.OrganizationID, id); err != nil {
		t.Fatal(err)
	}
	return tx, pid
}

func (f *independentModelFixture) waitBlockedAndExpired(t *testing.T, pid int) {
	t.Helper()
	independentModelWait(t, func() bool {
		var blocked bool
		if err := f.s.Pool.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1::int=ANY(pg_blocking_pids(pid)))", pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		return blocked
	}, "PostgreSQL action-row lock wait")
	independentModelWait(t, func() bool {
		var expired bool
		if err := f.s.Pool.QueryRow(context.Background(), "SELECT lease_until<=clock_timestamp() FROM operations WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.lease.OperationID).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		return expired
	}, "current fencing token lease to expire")
}

func TestIndependentModelPrepareRejectsCurrentTokenExpiredDuringLockWait(t *testing.T) {
	f := independentModelSetup(t, true, nil)
	reservation := f.reserve(t)
	f.shortenLease(t, "500 milliseconds")
	lock, pid := f.lockAction(t, reservation.ActionID)
	result := make(chan error, 1)
	go func() {
		_, err := f.s.PrepareModelDispatch(context.Background(), f.lease, reservation.ID)
		result <- err
	}()
	f.waitBlockedAndExpired(t, pid)
	if err := lock.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	code(t, <-result, "stale_lease")
	f.reservationState(t, reservation.ID, "reserved", "pending")
	f.balances(t, 150)
	f.artifacts(t, 0)
	if f.calls.Load() != 0 {
		t.Fatal("provider called without dispatch authority")
	}
	var token int64
	var dispatched bool
	if err := f.s.Pool.QueryRow(context.Background(), "SELECT o.fencing_token,a.dispatch_issued_at IS NOT NULL FROM operations o JOIN actions a ON a.organization_id=o.organization_id AND a.operation_id=o.id WHERE a.organization_id=$1 AND a.id=$2", f.p.OrganizationID, reservation.ActionID).Scan(&token, &dispatched); err != nil {
		t.Fatal(err)
	}
	if token != f.lease.Token || dispatched {
		t.Fatal("expired current token authorized a persisted send permit")
	}
	code(t, f.s.RenewModelLease(context.Background(), f.lease, 30*time.Second), "stale_lease")
}

type independentModelCompletion struct {
	reservationID string
	candidate     domain.ModelCandidate
}
type independentModelCompletionGate struct {
	*Store
	entered chan independentModelCompletion
	proceed chan struct{}
}

func (s *independentModelCompletionGate) CompleteModelCandidate(ctx context.Context, l *domain.Lease, id string, c domain.ModelCandidate) (domain.Artifact, error) {
	s.entered <- independentModelCompletion{id, c}
	<-s.proceed
	return s.Store.CompleteModelCandidate(ctx, l, id, c)
}

type independentModelResult struct {
	out modelruntime.Outcome
	err error
}

func TestIndependentModelCompleteRejectsCurrentTokenExpiredDuringLockWait(t *testing.T) {
	f := independentModelSetup(t, true, nil)
	gate := &independentModelCompletionGate{Store: f.s, entered: make(chan independentModelCompletion, 1), proceed: make(chan struct{})}
	result := make(chan independentModelResult, 1)
	runtime := f.runtime(t, gate)
	go func() {
		out, err := runtime.Execute(context.Background(), f.lease)
		result <- independentModelResult{out, err}
	}()
	var completion independentModelCompletion
	select {
	case completion = <-gate.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not reach candidate completion")
	}
	var actionID string
	if err := f.s.Pool.QueryRow(context.Background(), "SELECT action_id FROM reservations WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, completion.reservationID).Scan(&actionID); err != nil {
		t.Fatal(err)
	}
	f.shortenLease(t, "500 milliseconds")
	lock, pid := f.lockAction(t, actionID)
	close(gate.proceed)
	f.waitBlockedAndExpired(t, pid)
	if err := lock.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := <-result
	independentModelError(t, got.err, "candidate_not_committed", true)
	if got.out.Status != modelruntime.OutcomeUnknown || f.calls.Load() != 1 {
		t.Fatalf("out=%+v calls=%d", got.out, f.calls.Load())
	}
	f.artifacts(t, 0)
	f.balances(t, 150)
	f.reservationState(t, completion.reservationID, "unknown", "unknown")
	code(t, f.s.RenewModelLease(context.Background(), f.lease, 30*time.Second), "stale_lease")
	recovered, _, err := f.s.ClaimModel(context.Background(), "after-expiry-worker", 30*time.Second)
	if err != nil || recovered != nil {
		t.Fatalf("uncertain execution reclaimed: %v", err)
	}
	f.states(t, "blocked", "model_outcome_unknown", "blocked")
	if f.calls.Load() != 1 {
		t.Fatal("uncertain attempt resent")
	}
}

type independentModelDelayedPermit struct{ *Store }

func (s *independentModelDelayedPermit) PrepareModelDispatch(ctx context.Context, l *domain.Lease, id string) (domain.DispatchPermit, error) {
	permit, err := s.Store.PrepareModelDispatch(ctx, l, id)
	if err == nil {
		time.Sleep(permit.ValidFor + 20*time.Millisecond)
	}
	return permit, err
}

func TestIndependentModelExpiredCommittedPermitNeverStartsHTTP(t *testing.T) {
	f := independentModelSetup(t, true, nil)
	f.shortenLease(t, "500 milliseconds")
	out, err := f.runtime(t, &independentModelDelayedPermit{f.s}).Execute(context.Background(), f.lease)
	independentModelError(t, err, "dispatch_permit_rejected", true)
	if out.Status != modelruntime.OutcomeUnknown || f.calls.Load() != 0 {
		t.Fatalf("expired permit sent HTTP: %+v calls=%d", out, f.calls.Load())
	}
	f.reservationState(t, out.ReservationID, "unknown", "unknown")
	f.balances(t, 150)
	f.artifacts(t, 0)
	lease, _, err := f.s.ClaimModel(context.Background(), "after-permit-expiry", 30*time.Second)
	if err != nil || lease != nil {
		t.Fatalf("expired committed permit retried: %v", err)
	}
	f.states(t, "blocked", "model_outcome_unknown", "blocked")
}

type independentModelAccountingFailure struct{ *Store }

func (*independentModelAccountingFailure) MarkCostUnknown(context.Context, string, string, string, string) (domain.Reservation, error) {
	return domain.Reservation{}, errors.New("injected persistence outage after provider response")
}

func TestIndependentModelRestartAfterSentPersistenceFailureDoesNotResend(t *testing.T) {
	f := independentModelSetup(t, true, nil)
	out, err := f.runtime(t, &independentModelAccountingFailure{f.s}).Execute(context.Background(), f.lease)
	independentModelError(t, err, "accounting_record_failed", true)
	if f.calls.Load() != 1 {
		t.Fatalf("HTTP calls=%d", f.calls.Load())
	}
	f.reservationState(t, out.ReservationID, "reserved", "sent")
	f.artifacts(t, 0)
	f.balances(t, 150)
	f.shortenLease(t, "-1 second")
	reopened, err := Open(context.Background(), f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	l, _, err := reopened.ClaimModel(context.Background(), "new-store-new-worker", 30*time.Second)
	if err != nil || l != nil {
		t.Fatalf("sent action was reclaimed for resend: %v", err)
	}
	f.reservationState(t, out.ReservationID, "unknown", "unknown")
	f.balances(t, 150)
	f.states(t, "blocked", "model_outcome_unknown", "blocked")
	_, err = f.runtime(t, reopened).Execute(context.Background(), f.lease)
	independentModelError(t, err, "reservation_not_admitted", false)
	if f.calls.Load() != 1 {
		t.Fatal("reopening the store or replaying a stale lease sent a second request")
	}
}

func (f *independentModelFixture) replaceProfile(t *testing.T) {
	t.Helper()
	c := f.c
	c.Version++
	c.PriceVersion = "local-test-price-v2"
	if _, err := f.s.InstallModelConnection(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	models, _, err := f.s.Models(context.Background(), f.p, "", 100)
	if err != nil || len(models) != 1 || models[0].Version != 2 {
		t.Fatal("active replacement model missing from safe catalog")
	}
	persisted, err := f.s.GetTask(context.Background(), f.p, f.task.ID)
	if err != nil || persisted.ModelConnectionVersion != 1 {
		t.Fatal("accepted task silently changed model connection version")
	}
}

func TestIndependentModelDeactivatedPinCannotFallForward(t *testing.T) {
	t.Run("before_claim", func(t *testing.T) {
		f := independentModelSetup(t, false, nil)
		f.replaceProfile(t)
		lease, _, err := f.s.ClaimModel(context.Background(), "claim-retired-profile", 30*time.Second)
		if err != nil || lease != nil {
			t.Fatalf("retired pin claimed: %v", err)
		}
		f.states(t, "blocked", "model_profile_unavailable", "blocked")
		f.balances(t, 0)
		f.artifacts(t, 0)
		if f.calls.Load() != 0 {
			t.Fatal("retired task sent a model request")
		}
	})
	t.Run("after_claim_before_reserve", func(t *testing.T) {
		f := independentModelSetup(t, true, nil)
		f.replaceProfile(t)
		_, err := f.runtime(t, f.s).Execute(context.Background(), f.lease)
		independentModelError(t, err, "reservation_not_admitted", false)
		f.balances(t, 0)
		f.artifacts(t, 0)
		if f.calls.Load() != 0 {
			t.Fatal("retired task sent a model request")
		}
	})
	t.Run("after_provider_accepts", func(t *testing.T) {
		accepted := make(chan struct{})
		release := make(chan struct{})
		f := independentModelSetup(t, true, func(w http.ResponseWriter, r *http.Request) { close(accepted); <-release; independentModelReply(w, r) })
		runtime := f.runtime(t, f.s)
		result := make(chan independentModelResult, 1)
		go func() {
			out, err := runtime.Execute(context.Background(), f.lease)
			result <- independentModelResult{out, err}
		}()
		select {
		case <-accepted:
		case <-time.After(3 * time.Second):
			t.Fatal("provider never accepted request")
		}
		f.replaceProfile(t)
		close(release)
		got := <-result
		independentModelError(t, got.err, "candidate_not_committed", true)
		if f.calls.Load() != 1 {
			t.Fatalf("HTTP calls=%d", f.calls.Load())
		}
		f.reservationState(t, got.out.ReservationID, "unknown", "unknown")
		f.balances(t, 150)
		f.artifacts(t, 0)
	})
}

func TestIndependentModelRejectedOutputsKeepUnknownExposure(t *testing.T) {
	tests := []struct {
		name, text    string
		input, output int64
		wantCode      string
	}{
		{"input_assumption_exceeded", "Candidate", 101, 2, "model_output_rejected"},
		{"output_limit_exceeded", "Candidate", 3, 17, "provider_outcome_unknown"},
		{"nul_not_storable", "a\x00b", 3, 2, "model_output_rejected"},
		{"artifact_byte_limit", strings.Repeat("x", 1025), 3, 2, "model_output_rejected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := independentModelSetup(t, true, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "rejected-local-fixture", "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": tt.text}, "finish_reason": "stop"}}, "usage": map[string]int64{"prompt_tokens": tt.input, "completion_tokens": tt.output, "total_tokens": tt.input + tt.output}})
			})
			out, err := f.runtime(t, f.s).Execute(context.Background(), f.lease)
			independentModelError(t, err, tt.wantCode, true)
			if f.calls.Load() != 1 || out.CostStatus != "unknown" {
				t.Fatalf("calls=%d out=%+v", f.calls.Load(), out)
			}
			f.reservationState(t, out.ReservationID, "unknown", "unknown")
			f.balances(t, 150)
			f.artifacts(t, 0)
			reason := "model_output_rejected"
			if tt.wantCode == "provider_outcome_unknown" {
				reason = "model_outcome_unknown"
			}
			f.states(t, "blocked", reason, "blocked")
		})
	}
}

func TestIndependentModelGoalAndIssuedPermitCannotBeRewritten(t *testing.T) {
	f := independentModelSetup(t, true, nil)
	reservation := f.reserve(t)
	if _, err := f.s.Pool.Exec(context.Background(), "UPDATE tasks SET goal='Rewritten after admission' WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.task.ID); err == nil {
		t.Fatal("task goal changed after immutable intent admission")
	}
	current, err := f.s.GetTask(context.Background(), f.p, f.task.ID)
	if err != nil || current.Goal != f.task.Goal {
		t.Fatal("original task goal was not preserved")
	}
	permit, err := f.s.PrepareModelDispatch(context.Background(), f.lease, reservation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if permit.ValidFor <= 0 || permit.ValidFor > 2*time.Second || permit.FencingToken != f.lease.Token {
		t.Fatal("invalid original dispatch receipt")
	}
	// Each statement stays inside the table's generic 2-second duration bound;
	// the immutable receipt trigger must reject even an otherwise valid rewrite.
	for _, mutation := range []string{
		"dispatch_issued_at=dispatch_issued_at+interval '1 millisecond',dispatch_not_after=dispatch_not_after+interval '1 millisecond'",
		"dispatch_not_after=dispatch_not_after-interval '1 millisecond'",
		"dispatch_fencing_token=dispatch_fencing_token+1",
		"dispatch_issued_at=NULL,dispatch_not_after=NULL,dispatch_fencing_token=NULL",
	} {
		if _, err = f.s.Pool.Exec(context.Background(), "UPDATE actions SET "+mutation+" WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, reservation.ActionID); err == nil {
			t.Fatalf("issued permit rewrite accepted: %s", mutation)
		}
	}
	if err = f.s.RenewModelLease(context.Background(), f.lease, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.MarkCostUnknown(context.Background(), f.p.OrganizationID, reservation.ID, "independent-no-bill", "receipt-preservation-test"); err != nil {
		t.Fatal(err)
	}
	var issued, expires time.Time
	var token int64
	if err = f.s.Pool.QueryRow(context.Background(), "SELECT dispatch_issued_at,dispatch_not_after,dispatch_fencing_token FROM actions WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, reservation.ActionID).Scan(&issued, &expires, &token); err != nil {
		t.Fatal(err)
	}
	if !issued.Equal(permit.IssuedAt) || !expires.Equal(permit.NotAfter) || token != permit.FencingToken {
		t.Fatal("lease renewal or unknown classification replaced original send permit")
	}
	f.reservationState(t, reservation.ID, "unknown", "unknown")
	f.balances(t, 150)
	f.artifacts(t, 0)
	if f.calls.Load() != 0 {
		t.Fatal("structural receipt test unexpectedly called provider")
	}
}
