// SPDX-License-Identifier: Elastic-2.0
package storage

// Independent action approval tests use isolated real PostgreSQL schemas.
// A local model fixture creates the candidate; it supplies no authoritative bill.
// Direct trusted Store calls below test admission/state, not external delivery.
import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
	"agentplatform.local/platform/internal/toolruntime"
)

type independentToolFixture struct {
	*independentCollaborationFixture
	tool domain.ToolConnection
	peer *httptest.Server
}

func independentToolSetup(t *testing.T, distinct bool) *independentToolFixture {
	return independentToolSetupWithPeer(t, distinct, nil)
}
func independentToolSetupWithPeer(t *testing.T, distinct bool, handler http.HandlerFunc) *independentToolFixture {
	t.Helper()
	f := &independentToolFixture{independentCollaborationFixture: independentCollaborationSetup(t)}
	if handler == nil {
		handler = func(w http.ResponseWriter, _ *http.Request) {
			t.Error("state-only fixture unexpectedly sent an external tool request")
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
	peer := httptest.NewTLSServer(handler)
	f.peer = peer
	t.Cleanup(peer.Close)
	schema := `{"type":"object","properties":{},"additionalProperties":false}`
	c := domain.ToolConnection{Tool: domain.ToolInfo{
		ID: "independent-tool", OrganizationID: f.p.OrganizationID, Version: 1,
		Name: "Independent action fixture", Description: "State tests; no real external effect",
		Effect: "write", Destination: "Explicit local fixture", Provider: "independent-tool-account", PriceVersion: "fixture-price-v1",
		MaxChargeMicroUSD: 120, InputSchemaJSON: schema, InputSchemaDigest: Hash([]byte(schema)),
		RequireDistinctApprover: distinct, TimeoutSeconds: 3, MaxInputBytes: 4096, MaxOutputBytes: 4096, Active: true,
	}, Transport: "http_json", ResultContract: "agent_platform_action_v1", Endpoint: peer.URL,
		CredentialEnvironment: "PLATFORM_TOOL_KEY_INDEPENDENT", AllowPrivateNetwork: true}
	var e error
	f.tool, e = f.s.InstallToolConnection(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	return f
}

func (f *independentToolFixture) toolRuntime(t *testing.T, s toolruntime.Store) *toolruntime.Runtime {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(f.peer.Certificate())
	r, e := toolruntime.New(s, toolruntime.Config{Connection: f.tool, Credential: "independent-local-tool-key", TrustedRoots: roots, PersistenceTimeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func (f *independentToolFixture) prepareInput() domain.PrepareToolExecution {
	return domain.PrepareToolExecution{OccurrenceID: domain.NewID("occ"), ExpectedTaskVersion: f.current.Version,
		CandidateID: f.candidate.ID, ArtifactID: f.candidate.ArtifactID, ArtifactVersion: f.candidate.ArtifactVersion,
		ContentDigest: f.candidate.ContentDigest, ToolID: f.tool.Tool.ID, ToolVersion: f.tool.Tool.Version,
		ArgumentsJSON: `{}`, ExpiresInSeconds: 60}
}

func independentToolView(t *testing.T, raw json.RawMessage, e error) domain.ToolExecutionView {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
	var v domain.ToolExecutionView
	if e = json.Unmarshal(raw, &v); e != nil || v.Execution.ID == "" || v.Approval.ID == "" || v.Task.ID == "" {
		t.Fatalf("missing bound tool view: %v", e)
	}
	return v
}

func (f *independentToolFixture) prepare(t *testing.T) domain.ToolExecutionView {
	t.Helper()
	raw, e := f.s.PrepareToolExecution(context.Background(), f.p, f.task.ID, domain.NewID("key"), f.prepareInput())
	return independentToolView(t, raw, e)
}

func independentToolDecision(v domain.ToolExecutionView, decision string) domain.DecideToolExecution {
	return domain.DecideToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version,
		ExpectedApprovalVersion: v.Approval.Version, IntentHash: v.Execution.IntentHash, Decision: decision, Reason: "Independent explicit decision"}
}
func independentToolDispatch(v domain.ToolExecutionView) domain.DispatchToolExecution {
	return domain.DispatchToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version,
		ExpectedApprovalVersion: v.Approval.Version, IntentHash: v.Execution.IntentHash, AcknowledgeCost: true}
}
func (f *independentToolFixture) decide(t *testing.T, v domain.ToolExecutionView, p domain.Principal, decision string) domain.ToolExecutionView {
	t.Helper()
	raw, e := f.s.DecideToolExecution(context.Background(), p, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDecision(v, decision))
	return independentToolView(t, raw, e)
}
func (f *independentToolFixture) dispatch(t *testing.T, v domain.ToolExecutionView) domain.ToolExecutionView {
	t.Helper()
	raw, e := f.s.DispatchToolExecution(context.Background(), f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDispatch(v))
	return independentToolView(t, raw, e)
}
func independentToolDeny(t *testing.T, e error) {
	t.Helper()
	var de *domain.Error
	if !errors.As(e, &de) || de.Status < 400 || de.Status >= 500 {
		t.Fatalf("expected bounded domain rejection, got %v", e)
	}
}
func (f *independentToolFixture) balance(t *testing.T, reserve, spent int64) {
	t.Helper()
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, reserve, spent)
}

func TestIndependentToolConcurrentPrepareIsOneImmutableOccurrence(t *testing.T) {
	f := independentToolSetup(t, false)
	ctx := context.Background()
	in, key := f.prepareInput(), domain.NewID("key")
	const n = 12
	var wg sync.WaitGroup
	raws := make(chan json.RawMessage, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, e := f.s.PrepareToolExecution(ctx, f.p, f.task.ID, key, in)
			raws <- raw
			errs <- e
		}()
	}
	wg.Wait()
	close(raws)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var first domain.ToolExecutionView
	for raw := range raws {
		v := independentToolView(t, raw, nil)
		if first.Execution.ID == "" {
			first = v
		}
		if v.Execution.ID != first.Execution.ID || v.Execution.IntentHash != first.Execution.IntentHash {
			t.Fatal("concurrent replay created another intent")
		}
	}
	if first.Execution.Status != "awaiting_approval" || first.Task.Status != "waiting_approval" || first.Task.CandidateID != f.candidate.ID || first.Task.CurrentRunID != f.candidate.RunID {
		t.Fatal("prepare changed source identity or executed without approval")
	}
	if first.Execution.IntentHash != domain.ToolIntentDigest(first.Execution) {
		t.Fatal("persisted intent does not match exact public snapshot")
	}
	var jobs, actions, reserves, own, root int
	e := f.s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tool_executions WHERE organization_id=$1),
 (SELECT count(*) FROM actions WHERE organization_id=$1 AND id=$3),
 (SELECT count(*) FROM reservations WHERE organization_id=$1 AND category='tool'),own_executions,tree_executions
 FROM tool_execution_counts WHERE organization_id=$1 AND task_id=$2`, f.p.OrganizationID, f.task.ID, first.Execution.ActionID).Scan(&jobs, &actions, &reserves, &own, &root)
	if e != nil || jobs != 1 || actions != 1 || reserves != 1 || own != 1 || root != 1 {
		t.Fatalf("replay multiplicity: %d %d %d %d %d: %v", jobs, actions, reserves, own, root, e)
	}
	f.balance(t, 270, 0)
	in.OccurrenceID = "different-occurrence"
	_, e = f.s.PrepareToolExecution(ctx, f.p, f.task.ID, key, in)
	independentToolDeny(t, e)
	// Tenant scope is independently enforced even when all identifiers are known.
	other, _ := identity(t, f.s, 10000)
	_, e = f.s.GetToolExecution(ctx, other, f.task.ID, first.Execution.ID)
	independentToolDeny(t, e)
}

func TestIndependentToolRejectsCandidateAndIntentSubstitution(t *testing.T) {
	f := independentToolSetup(t, false)
	before := f.money(t)
	for _, test := range []struct {
		name   string
		change func(*domain.PrepareToolExecution)
	}{
		{"task_version", func(v *domain.PrepareToolExecution) { v.ExpectedTaskVersion-- }},
		{"candidate", func(v *domain.PrepareToolExecution) { v.CandidateID = domain.NewID("cand") }},
		{"artifact", func(v *domain.PrepareToolExecution) { v.ArtifactID = domain.NewID("art") }},
		{"artifact_version", func(v *domain.PrepareToolExecution) { v.ArtifactVersion++ }},
		{"content_digest", func(v *domain.PrepareToolExecution) { v.ContentDigest = strings.Repeat("0", 64) }},
		{"tool_version", func(v *domain.PrepareToolExecution) { v.ToolVersion++ }},
		{"tool_identity", func(v *domain.PrepareToolExecution) { v.ToolID = "unregistered-tool" }},
		{"occurrence_space", func(v *domain.PrepareToolExecution) { v.OccurrenceID = "bad occurrence" }},
		{"arguments_duplicate", func(v *domain.PrepareToolExecution) { v.ArgumentsJSON = `{"x":1,"x":2}` }},
		{"arguments_unicode", func(v *domain.PrepareToolExecution) { v.ArgumentsJSON = `{"x":"\ud800"}` }},
		{"arguments_null", func(v *domain.PrepareToolExecution) { v.ArgumentsJSON = `null` }},
		{"expiry_zero", func(v *domain.PrepareToolExecution) { v.ExpiresInSeconds = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := f.prepareInput()
			test.change(&in)
			_, e := f.s.PrepareToolExecution(context.Background(), f.p, f.task.ID, domain.NewID("key"), in)
			independentToolDeny(t, e)
		})
	}
	if f.money(t) != before {
		t.Fatal("invalid intent changed financial state")
	}
}

func TestIndependentToolApprovalSeparationAndCurrentApproverAuthority(t *testing.T) {
	f := independentToolSetup(t, true)
	ctx := context.Background()
	v := f.prepare(t)
	in := independentToolDecision(v, "approve")
	_, e := f.s.DecideToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), in)
	independentToolDeny(t, e)
	member := independentCollaborationActor(t, f.s, f.p.OrganizationID, "member")
	_, e = f.s.DecideToolExecution(ctx, member, f.task.ID, v.Execution.ID, domain.NewID("key"), in)
	independentToolDeny(t, e)
	admin := independentCollaborationActor(t, f.s, f.p.OrganizationID, "admin")
	v = f.decide(t, v, admin, "approve")
	if v.Execution.Status != "approved" || v.Task.Status != "waiting_approval" || v.Approval.DecidedBy != admin.ID {
		t.Fatal("approval sent a job or lost approver binding")
	}
	if _, e = f.s.Pool.Exec(ctx, "UPDATE memberships SET role='member' WHERE organization_id=$1 AND principal_id=$2", admin.OrganizationID, admin.ID); e != nil {
		t.Fatal(e)
	}
	_, e = f.s.DispatchToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDispatch(v))
	independentToolDeny(t, e)
	f.balance(t, 270, 0)
}

func TestIndependentToolConcurrentDecisionAndDispatchHaveSingleWinner(t *testing.T) {
	f := independentToolSetup(t, false)
	ctx := context.Background()
	v := f.prepare(t)
	const n = 10
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := f.s.DecideToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDecision(v, "approve"))
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for e := range results {
		if e == nil {
			wins++
		} else {
			independentToolDeny(t, e)
		}
	}
	if wins != 1 {
		t.Fatalf("approval winners=%d", wins)
	}
	v, e := f.s.GetToolExecution(ctx, f.p, f.task.ID, v.Execution.ID)
	if e != nil {
		t.Fatal(e)
	}
	results = make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := f.s.DispatchToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDispatch(v))
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	wins = 0
	for e := range results {
		if e == nil {
			wins++
		} else {
			independentToolDeny(t, e)
		}
	}
	if wins != 1 {
		t.Fatalf("dispatch queue winners=%d", wins)
	}
	lease, _, e := f.s.ClaimToolExecution(ctx, "independent-tool-worker", 30*time.Second)
	if e != nil || lease == nil || lease.Execution.ID != v.Execution.ID {
		t.Fatalf("first claim: %v", e)
	}
	second, _, e := f.s.ClaimToolExecution(ctx, "independent-tool-worker-2", 30*time.Second)
	if e != nil || second != nil {
		t.Fatalf("live job claimed twice: %v", e)
	}
	f.balance(t, 270, 0)
}

func TestIndependentToolReplayRechecksCurrentCommandPermission(t *testing.T) {
	for _, command := range []string{"prepare", "decide", "dispatch"} {
		t.Run(command, func(t *testing.T) {
			f := independentToolSetup(t, false)
			ctx := context.Background()
			key := domain.NewID("key")
			in := f.prepareInput()
			var replay func() error
			if command == "prepare" {
				_, e := f.s.PrepareToolExecution(ctx, f.p, f.task.ID, key, in)
				if e != nil {
					t.Fatal(e)
				}
				replay = func() error { _, e := f.s.PrepareToolExecution(ctx, f.p, f.task.ID, key, in); return e }
			} else {
				v := f.prepare(t)
				if command == "decide" {
					d := independentToolDecision(v, "approve")
					_, e := f.s.DecideToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, key, d)
					if e != nil {
						t.Fatal(e)
					}
					replay = func() error { _, e := f.s.DecideToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, key, d); return e }
				} else {
					v = f.decide(t, v, f.p, "approve")
					d := independentToolDispatch(v)
					_, e := f.s.DispatchToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, key, d)
					if e != nil {
						t.Fatal(e)
					}
					replay = func() error { _, e := f.s.DispatchToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, key, d); return e }
				}
			}
			before := f.money(t)
			if _, e := f.s.Pool.Exec(ctx, "UPDATE memberships SET role='viewer' WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, f.p.ID); e != nil {
				t.Fatal(e)
			}
			independentToolDeny(t, replay())
			if f.money(t) != before {
				t.Fatal("forbidden cached replay changed accounting")
			}
		})
	}
}

func TestIndependentToolDeniedOccurrenceCannotBeReusedOrResetQuota(t *testing.T) {
	f := independentToolSetup(t, false)
	ctx := context.Background()
	first := f.prepareInput()
	for i := 0; i < domain.MaxToolExecutionsPerTask; i++ {
		f.refresh(t)
		in := f.prepareInput()
		if i == 0 {
			first = in
		}
		raw, e := f.s.PrepareToolExecution(ctx, f.p, f.task.ID, domain.NewID("key"), in)
		v := independentToolView(t, raw, e)
		v = f.decide(t, v, f.p, "deny")
		if v.Execution.Status != "denied" || v.Task.Status != "waiting_input" || v.Task.CandidateID != f.candidate.ID {
			t.Fatal("denial lost original candidate or continued execution")
		}
		var runState string
		if e = f.s.Pool.QueryRow(ctx, "SELECT status FROM runs WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.candidate.RunID).Scan(&runState); e != nil || runState != "waiting_input" {
			t.Fatalf("denied action left run waiting for a nonexistent approval: %s %v", runState, e)
		}
		f.balance(t, 150, 0)
		if i == 0 {
			reused := first
			reused.ExpectedTaskVersion = v.Task.Version
			_, e = f.s.PrepareToolExecution(ctx, f.p, f.task.ID, domain.NewID("key"), reused)
			independentToolDeny(t, e) // occurrence identity is checked before the quota is exhausted.
		}
	}
	f.refresh(t)
	first.ExpectedTaskVersion = f.current.Version
	_, e := f.s.PrepareToolExecution(ctx, f.p, f.task.ID, domain.NewID("key"), first)
	independentToolDeny(t, e)
	_, e = f.s.PrepareToolExecution(ctx, f.p, f.task.ID, domain.NewID("key"), f.prepareInput())
	independentToolDeny(t, e)
	var count int
	if e = f.s.Pool.QueryRow(ctx, "SELECT own_executions FROM tool_execution_counts WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&count); e != nil || count != domain.MaxToolExecutionsPerTask {
		t.Fatalf("lifetime quota=%d: %v", count, e)
	}
	f.balance(t, 150, 0)
}

func TestIndependentToolCredentialExpiryWhileWaitingForTreeRollsBack(t *testing.T) {
	f := independentToolSetup(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	before := f.money(t)
	if _, e := f.s.Pool.Exec(ctx, "UPDATE access_tokens SET expires_at=clock_timestamp()+interval '400 milliseconds' WHERE token_hash=$1", f.p.TokenHash); e != nil {
		t.Fatal(e)
	}
	blocker, e := f.s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback(ctx)
	var pid int
	if e = blocker.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); e != nil {
		t.Fatal(e)
	}
	if _, e = blocker.Exec(ctx, "SELECT root_id FROM task_tree_controls WHERE organization_id=$1 AND root_id=$2 FOR NO KEY UPDATE", f.p.OrganizationID, f.task.ID); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() {
		_, e := f.s.PrepareToolExecution(ctx, f.p, f.task.ID, domain.NewID("key"), f.prepareInput())
		done <- e
	}()
	independentCollaborationWaiters(t, f.s, pid, 1)
	for {
		var expired bool
		if e = f.s.Pool.QueryRow(ctx, "SELECT expires_at<clock_timestamp() FROM access_tokens WHERE token_hash=$1", f.p.TokenHash).Scan(&expired); e != nil {
			t.Fatal(e)
		}
		if expired {
			break
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if e = blocker.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	code(t, <-done, "unauthenticated")
	if f.money(t) != before {
		t.Fatal("expired preparer committed a financial admission")
	}
}

func (f *independentToolFixture) claimed(t *testing.T) (domain.ToolExecutionView, *domain.ToolLease) {
	t.Helper()
	v := f.prepare(t)
	v = f.decide(t, v, f.p, "approve")
	v = f.dispatch(t, v)
	l, _, e := f.s.ClaimToolExecution(context.Background(), "independent-tool-worker", 30*time.Second)
	if e != nil || l == nil || l.Execution.ID != v.Execution.ID {
		t.Fatal("could not claim admitted job", e)
	}
	return v, l
}

func independentToolOutcome(l *domain.ToolLease, outcome string) domain.ToolOutcomeEvidence {
	return domain.ToolOutcomeEvidence{OrganizationID: l.Execution.OrganizationID, ExecutionID: l.Execution.ID,
		IntentHash: l.Execution.IntentHash, FencingToken: l.FencingToken, Outcome: outcome,
		Result: "Exact local observed effect; not task acceptance or billing", EffectReference: "independent-effect-reference",
		EvidenceDigest: Hash([]byte("independent registered response bytes"))}
}

func TestIndependentToolEffectUnknownIsIndependentFromFinancialConfirmation(t *testing.T) {
	f := independentToolSetup(t, false)
	ctx := context.Background()
	v, l := f.claimed(t)
	permit, e := f.s.PrepareToolDispatch(ctx, l)
	if e != nil || permit.IntentHash != l.Execution.IntentHash || permit.ExecutionID != l.Execution.ID || permit.FencingToken != l.FencingToken {
		t.Fatal("missing exact send permit", e)
	}
	if _, e = f.s.RecordToolOutcome(ctx, independentToolOutcome(l, "unknown")); e != nil {
		t.Fatal(e)
	}
	ev := independentEvidence(17)
	ev.Source = f.tool.Tool.Provider
	out, e := f.s.AccountCost(ctx, f.p.OrganizationID, v.Execution.ReservationID, ev)
	if e != nil || out.Status != "settled" {
		t.Fatal("trusted final bill not recorded", e)
	}
	v, e = f.s.GetToolExecution(ctx, f.p, f.task.ID, v.Execution.ID)
	if e != nil || v.Execution.Status != "unknown" || v.Receipt != nil {
		t.Fatal("financial confirmation invented effect success", e)
	}
	before := f.money(t)
	_, e = f.s.DispatchToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDispatch(v))
	independentToolDeny(t, e)
	input := domain.ReconcileToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version,
		IntentHash: v.Execution.IntentHash, Outcome: "succeeded", EffectReference: "operator-observed-effect", Result: "Explicit operator assertion",
		EvidenceDigest: Hash([]byte("operator reviewed external evidence")), Reason: "Independent operator attestation, not provider verification"}
	key := domain.NewID("key")
	raw, e := f.s.ReconcileToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, key, input)
	final := independentToolView(t, raw, e)
	if final.Receipt == nil || final.Receipt.Method != "operator_assertion" || final.Receipt.PrincipalID != f.p.ID || final.Execution.Status != "succeeded" || final.Task.Status != "waiting_input" || final.Task.CandidateID != f.candidate.ID || final.Task.Result != "" {
		t.Fatal("manual effect resolution accepted task or lost provenance")
	}
	if f.money(t) != before {
		t.Fatal("effect reconciliation mutated independently settled money")
	}
	f.balance(t, 150, 17)
	raw, e = f.s.ReconcileToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, key, input)
	replay := independentToolView(t, raw, e)
	if replay.Receipt == nil || replay.Receipt.ID != final.Receipt.ID {
		t.Fatal("reconcile replay created second observation")
	}
	// A contradictory late registered observation must survive the returned
	// conflict, without overwriting the operator's receipt or changing finance.
	conflict := independentToolOutcome(l, "rejected")
	for i := 0; i < 2; i++ {
		_, e = f.s.RecordToolOutcome(ctx, conflict)
		code(t, e, "conflict")
	}
	var conflicts, events int
	if e = f.s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tool_outcome_conflicts WHERE organization_id=$1 AND execution_id=$2),(SELECT count(*) FROM events WHERE organization_id=$1 AND type='tool.outcome_conflicted' AND data->>'execution_id'=$2)`, f.p.OrganizationID, v.Execution.ID).Scan(&conflicts, &events); e != nil || conflicts != 1 || events != 1 {
		t.Fatal("conflicting evidence was dropped or replayed twice", e)
	}
	for i := 0; i < 10; i++ {
		conflict.EffectReference = domain.NewID("effect")
		_, e = f.s.RecordToolOutcome(ctx, conflict)
		code(t, e, "conflict")
	}
	if e = f.s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tool_outcome_conflicts WHERE organization_id=$1 AND execution_id=$2),(SELECT count(*) FROM events WHERE organization_id=$1 AND type='tool.outcome_conflict_overflow' AND data->>'execution_id'=$2)`, f.p.OrganizationID, v.Execution.ID).Scan(&conflicts, &events); e != nil || conflicts != 8 || events != 1 {
		t.Fatal("conflict evidence bound or overflow event is not durable/idempotent", e)
	}
	unchanged, e := f.s.GetToolExecution(ctx, f.p, f.task.ID, v.Execution.ID)
	if e != nil || unchanged.Receipt == nil || unchanged.Receipt.ID != final.Receipt.ID || unchanged.Task.Version != final.Task.Version || unchanged.Execution.Status != "succeeded" || f.money(t) != before {
		t.Fatal("late contradiction changed accepted observation/task/finance", e)
	}
	if _, e = f.s.Pool.Exec(ctx, "UPDATE tool_outcome_receipts SET outcome='rejected' WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, final.Receipt.ID); e == nil {
		t.Fatal("effect receipt can be rewritten")
	}
}

func TestIndependentToolCancellationFencesJobButRetainsLateAdmittedObservation(t *testing.T) {
	for _, sent := range []bool{false, true} {
		name := "pending"
		if sent {
			name = "sent"
		}
		t.Run(name, func(t *testing.T) {
			f := independentToolSetup(t, false)
			ctx := context.Background()
			v, l := f.claimed(t)
			if sent {
				if _, e := f.s.PrepareToolDispatch(ctx, l); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := f.s.CancelTask(ctx, f.p, f.task.ID, domain.NewID("key")); e != nil {
				t.Fatal(e)
			}
			if e := f.s.RenewToolLease(ctx, l, 30*time.Second); e == nil {
				t.Fatal("cancelled job renewed stale lease")
			}
			if _, e := f.s.PrepareToolDispatch(ctx, l); e == nil {
				t.Fatal("cancelled job obtained another send permit")
			}
			if sent {
				var entries int
				var reservedDelta, spentDelta int64
				if err := f.s.Pool.QueryRow(ctx, "SELECT count(*),COALESCE(sum(delta_reserved_microusd),0),COALESCE(sum(delta_spent_microusd),0) FROM ledger_entries WHERE organization_id=$1 AND reservation_id=$2 AND event_key='tool.effect.unknown' AND kind='unknown'", f.p.OrganizationID, v.Execution.ReservationID).Scan(&entries, &reservedDelta, &spentDelta); err != nil || entries != 1 || reservedDelta != 0 || spentDelta != 0 {
					t.Fatal("cancellation did not preserve an exact zero-delta unknown ledger entry", err)
				}
				before := f.money(t)
				evidence := independentToolOutcome(l, "succeeded")
				receipt, e := f.s.RecordToolOutcome(ctx, evidence)
				if e != nil || receipt.Method != "registered_tool_response" || receipt.ExecutionID != v.Execution.ID {
					t.Fatal("late admitted observation was not preserved", e)
				}
				if f.money(t) != before {
					t.Fatal("late effect observation settled/released exposure")
				}
				replay, e := f.s.RecordToolOutcome(ctx, evidence)
				if e != nil || replay.ID != receipt.ID {
					t.Fatal("exact late observation replay duplicated receipt", e)
				}
				forged := evidence
				forged.FencingToken++
				if _, e = f.s.RecordToolOutcome(ctx, forged); e == nil {
					t.Fatal("forged fence reused receipt")
				}
				f.balance(t, 270, 0)
			} else {
				if _, e := f.s.RecordToolOutcome(ctx, independentToolOutcome(l, "succeeded")); e == nil {
					t.Fatal("unsent cancelled job invented an effect")
				}
				f.balance(t, 150, 0)
			}
			current, e := f.s.GetTask(ctx, f.p, f.task.ID)
			if e != nil || current.Status != "cancelled" || current.Result != "" {
				t.Fatal("late job revived cancelled task", e)
			}
		})
	}
}

func TestIndependentToolApprovalExpiryDuringGateWaitCannotAuthorizeSend(t *testing.T) {
	f := independentToolSetup(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	in := f.prepareInput()
	in.ExpiresInSeconds = 1
	raw, e := f.s.PrepareToolExecution(ctx, f.p, f.task.ID, domain.NewID("key"), in)
	v := independentToolView(t, raw, e)
	blocker, e := f.s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback(ctx)
	var pid int
	if e = blocker.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); e != nil {
		t.Fatal(e)
	}
	if _, e = blocker.Exec(ctx, "SELECT root_id FROM task_tree_controls WHERE organization_id=$1 AND root_id=$2 FOR NO KEY UPDATE", f.p.OrganizationID, f.task.ID); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() {
		_, e := f.s.DecideToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDecision(v, "approve"))
		done <- e
	}()
	independentCollaborationWaiters(t, f.s, pid, 1)
	for {
		var expired bool
		if e = f.s.Pool.QueryRow(ctx, "SELECT expires_at<=clock_timestamp() FROM approvals WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, v.Approval.ID).Scan(&expired); e != nil {
			t.Fatal(e)
		}
		if expired {
			break
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if e = blocker.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	independentToolDeny(t, <-done)
	var approved, dispatched bool
	if e = f.s.Pool.QueryRow(ctx, "SELECT a.status='approved',e.dispatch_issued_at IS NOT NULL FROM approvals a JOIN tool_executions e ON e.organization_id=a.organization_id AND e.id=a.execution_id WHERE a.organization_id=$1 AND a.id=$2", f.p.OrganizationID, v.Approval.ID).Scan(&approved, &dispatched); e != nil || approved || dispatched {
		t.Fatal("expired approval committed after blocked transaction", e)
	}
}

func TestIndependentToolSendRechecksRevokedApproverAndProfile(t *testing.T) {
	for _, revoke := range []string{"approver", "profile", "owner"} {
		t.Run(revoke, func(t *testing.T) {
			f := independentToolSetup(t, true)
			ctx := context.Background()
			admin := independentCollaborationActor(t, f.s, f.p.OrganizationID, "admin")
			v := f.prepare(t)
			v = f.decide(t, v, admin, "approve")
			v = f.dispatch(t, v)
			l, _, e := f.s.ClaimToolExecution(ctx, "independent-tool-worker", 30*time.Second)
			if e != nil || l == nil {
				t.Fatal("missing claimed job", e)
			}
			switch revoke {
			case "approver":
				_, e = f.s.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", admin.OrganizationID, admin.ID)
			case "profile":
				_, e = f.s.Pool.Exec(ctx, "UPDATE tool_connections SET active=false WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.tool.Tool.ID)
			case "owner":
				_, e = f.s.Pool.Exec(ctx, "UPDATE principals SET active=false WHERE id=$1", f.p.ID)
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e = f.s.PrepareToolDispatch(ctx, l); e == nil {
				t.Fatal("revoked authority obtained send permit")
			}
			var dispatched bool
			if e = f.s.Pool.QueryRow(ctx, "SELECT dispatch_issued_at IS NOT NULL FROM tool_executions WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, v.Execution.ID).Scan(&dispatched); e != nil || dispatched {
				t.Fatal("revoked execution persisted dispatch", e)
			}
		})
	}
}

func TestIndependentToolMetadataReadsReauthorizeHistoricalPrincipal(t *testing.T) {
	for _, revoked := range []string{"token", "membership", "principal", "organization"} {
		t.Run(revoked, func(t *testing.T) {
			f := independentToolSetup(t, false)
			ctx := context.Background()
			v := f.prepare(t)
			var e error
			switch revoked {
			case "token":
				_, e = f.s.Pool.Exec(ctx, "UPDATE access_tokens SET revoked=true WHERE token_hash=$1", f.p.TokenHash)
			case "membership":
				_, e = f.s.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, f.p.ID)
			case "principal":
				_, e = f.s.Pool.Exec(ctx, "UPDATE principals SET active=false WHERE id=$1", f.p.ID)
			case "organization":
				_, e = f.s.Pool.Exec(ctx, "UPDATE organizations SET active=false WHERE id=$1", f.p.OrganizationID)
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, _, e = f.s.Tools(ctx, f.p, "", 10); e == nil {
				t.Fatal("stale principal read tool registry")
			}
			if _, _, e = f.s.ToolExecutions(ctx, f.p, f.task.ID, "", 10); e == nil {
				t.Fatal("stale principal read task execution history")
			}
			if _, e = f.s.GetToolExecution(ctx, f.p, f.task.ID, v.Execution.ID); e == nil {
				t.Fatal("stale principal read exact arguments/approval")
			}
		})
	}
}

func TestIndependentToolAdminMayResolvePastEffectAfterOwnerRevocation(t *testing.T) {
	f := independentToolSetup(t, false)
	ctx := context.Background()
	v, l := f.claimed(t)
	if _, e := f.s.PrepareToolDispatch(ctx, l); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.RecordToolOutcome(ctx, independentToolOutcome(l, "unknown")); e != nil {
		t.Fatal(e)
	}
	var e error
	v, e = f.s.GetToolExecution(ctx, f.p, f.task.ID, v.Execution.ID)
	if e != nil {
		t.Fatal(e)
	}
	admin := independentCollaborationActor(t, f.s, f.p.OrganizationID, "admin")
	if _, e = f.s.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, f.p.ID); e != nil {
		t.Fatal(e)
	}
	before := f.money(t)
	in := domain.ReconcileToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, IntentHash: v.Execution.IntentHash,
		Outcome: "rejected", EffectReference: "admin-reviewed-prior-effect", Result: "", EvidenceDigest: Hash([]byte("independent administrative observation")), Reason: "Resolve past effect after owner revocation"}
	raw, e := f.s.ReconcileToolExecution(ctx, admin, f.task.ID, v.Execution.ID, domain.NewID("key"), in)
	final := independentToolView(t, raw, e)
	if final.Receipt == nil || final.Receipt.Method != "operator_assertion" || final.Receipt.PrincipalID != admin.ID || final.Execution.Status != "rejected" {
		t.Fatal("current admin could not record a past effect independently of revoked owner")
	}
	if f.money(t) != before {
		t.Fatal("admin observation of rejected effect released unknown financial exposure")
	}
	f.balance(t, 270, 0)
}

func TestIndependentToolDenyCanCleanUpUnsentIntentAfterAuthorityLoss(t *testing.T) {
	for _, change := range []string{"owner_revoked", "profile_retired", "distinct_self_denial"} {
		t.Run(change, func(t *testing.T) {
			f := independentToolSetup(t, true)
			ctx := context.Background()
			v := f.prepare(t)
			actor := f.p
			if change != "distinct_self_denial" {
				actor = independentCollaborationActor(t, f.s, f.p.OrganizationID, "admin")
			}
			var e error
			switch change {
			case "owner_revoked":
				_, e = f.s.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, f.p.ID)
			case "profile_retired":
				_, e = f.s.Pool.Exec(ctx, "UPDATE tool_connections SET active=false WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.tool.Tool.ID)
			}
			if e != nil {
				t.Fatal(e)
			}
			v = f.decide(t, v, actor, "deny")
			if v.Execution.Status != "denied" || v.Approval.Status != "denied" || v.Task.Status != "waiting_input" {
				t.Fatal("denial cleanup was treated as new execution admission")
			}
			f.balance(t, 150, 0)
		})
	}
}

func TestIndependentToolSchemaRejectsRoundedNonIntegerAssertions(t *testing.T) {
	f := independentToolSetup(t, false)
	for _, bound := range []string{`"maximum":1.0000000000000001`, `"const":9007199254740990.1`, `"multipleOf":1.00000000000000001`} {
		c := f.tool
		c.Tool.ID = domain.NewID("tool")
		c.Tool.Version = 1
		c.Tool.CreatedAt = time.Time{}
		c.Tool.InputSchemaJSON = `{"type":"object","properties":{"value":{"type":"number",` + bound + `}},"additionalProperties":false}`
		c.Tool.InputSchemaDigest = Hash([]byte(c.Tool.InputSchemaJSON))
		if _, e := f.s.InstallToolConnection(context.Background(), c); e == nil {
			t.Fatalf("schema admitted a fractional assertion rounded to an integer: %s", bound)
		}
	}
}

func TestIndependentToolManagedChildCannotBeConsumedWhileActionIsOpen(t *testing.T) {
	for _, cancelRoot := range []bool{false, true} {
		name := "deny_then_consume"
		if cancelRoot {
			name = "root_cancels_sent_child"
		}
		t.Run(name, func(t *testing.T) {
			base := independentToolSetup(t, false)
			ctx := context.Background()
			if _, e := base.s.CancelTask(ctx, base.p, base.task.ID, domain.NewID("key")); e != nil {
				t.Fatal(e)
			}
			leaf := domain.Agent{ID: base.task.AgentID, Version: base.task.AgentVersion}
			team := teamAgent(t, base.s, base.p, "parallel", "all", 1, teamMember("member", leaf))
			root := task(t, base.s, base.p, team)
			teamTick(t, base.s)
			l, _, e := base.s.ClaimModel(ctx, "independent-tool-team-model", 30*time.Second)
			if e != nil || l == nil || l.Task.RootID != root.ID {
				t.Fatal("missing managed child model job", e)
			}
			if out, e := base.runtime(t, base.s).Execute(ctx, l); e != nil || out.Status != modelruntime.CandidateWaitingInput {
				t.Fatal("child candidate missing", e)
			}
			child := &independentToolFixture{independentCollaborationFixture: &independentCollaborationFixture{independentModelFixture: &independentModelFixture{s: base.s, p: base.p, task: l.Task}}, tool: base.tool}
			child.refresh(t)
			v := child.prepare(t)
			teamTick(t, base.s)
			var selected bool
			if e = base.s.Pool.QueryRow(ctx, "SELECT selected_candidate_id IS NOT NULL FROM team_members WHERE organization_id=$1 AND parent_run_id=$2 AND child_task_id=$3", base.p.OrganizationID, root.CurrentRunID, child.task.ID).Scan(&selected); e != nil || selected {
				t.Fatal("coordinator consumed an action's frozen source before its outcome", e)
			}
			if cancelRoot {
				v = child.decide(t, v, child.p, "approve")
				v = child.dispatch(t, v)
				job, _, e := base.s.ClaimToolExecution(ctx, "independent-tool-team-worker", 30*time.Second)
				if e != nil || job == nil {
					t.Fatal("missing child tool job", e)
				}
				if _, e = base.s.PrepareToolDispatch(ctx, job); e != nil {
					t.Fatal(e)
				}
				if _, e = base.s.CancelTask(ctx, base.p, root.ID, domain.NewID("key")); e != nil {
					t.Fatal(e)
				}
				if e = base.s.RenewToolLease(ctx, job, 30*time.Second); e == nil {
					t.Fatal("root cancellation left child tool lease usable")
				}
				for _, id := range []string{root.ID, child.task.ID} {
					current, e := base.s.GetTask(ctx, base.p, id)
					if e != nil || current.Status != "cancelled" {
						t.Fatal("root cancellation missed tool descendant", e)
					}
				}
				var state string
				var amount int64
				if e = base.s.Pool.QueryRow(ctx, "SELECT status,amount_microusd FROM reservations WHERE organization_id=$1 AND id=$2", base.p.OrganizationID, v.Execution.ReservationID).Scan(&state, &amount); e != nil || state != "unknown" || amount != 120 {
					t.Fatal("root cancellation released possibly incurred tool cost", e)
				}
			} else {
				child.decide(t, v, child.p, "deny")
				teamTick(t, base.s)
				if e = base.s.Pool.QueryRow(ctx, "SELECT selected_candidate_id IS NOT NULL FROM team_members WHERE organization_id=$1 AND parent_run_id=$2 AND child_task_id=$3", base.p.OrganizationID, root.CurrentRunID, child.task.ID).Scan(&selected); e != nil || !selected {
					t.Fatal("restored source could not be consumed", e)
				}
				child.refresh(t)
				_, e = base.s.PrepareToolExecution(ctx, base.p, child.task.ID, domain.NewID("key"), child.prepareInput())
				independentToolDeny(t, e)
			}
		})
	}
}

func TestIndependentToolActualTLSOutcomesNeverBecomeFreeOrAutomaticReplay(t *testing.T) {
	for _, kind := range []string{"registered_success", "plain_200", "wrong_intent", "lost_ack"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int64
			f := independentToolSetupWithPeer(t, false, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer independent-local-tool-key" || r.Header.Get("Cookie") != "" {
					t.Error("wrong tool transport identity")
				}
				var request map[string]json.RawMessage
				if e := json.NewDecoder(r.Body).Decode(&request); e != nil {
					t.Error(e)
					w.WriteHeader(400)
					return
				}
				var execution, action, intent string
				_ = json.Unmarshal(request["execution_id"], &execution)
				_ = json.Unmarshal(request["action_id"], &action)
				_ = json.Unmarshal(request["intent_hash"], &intent)
				if execution == "" || action == "" || intent == "" || r.Header.Get("Idempotency-Key") != action || string(request["arguments"]) != "{}" {
					t.Error("actual request did not match exact approved payload")
				}
				if kind == "lost_ack" {
					h, ok := w.(http.Hijacker)
					if !ok {
						t.Error("fixture cannot drop response")
						return
					}
					conn, _, e := h.Hijack()
					if e != nil {
						t.Error(e)
						return
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if kind == "plain_200" {
					_, _ = w.Write([]byte(`{"ok":true}`))
					return
				}
				if kind == "wrong_intent" {
					intent = strings.Repeat("f", 64)
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"execution_id": execution, "action_id": action, "intent_hash": intent, "outcome": "succeeded", "effect_reference": "local-peer-observation", "result": "Local registered effect observation; unverified task result"})
			})
			v, l := f.claimed(t)
			ctx := context.Background()
			out, e := f.toolRuntime(t, f.s).Execute(ctx, l)
			if kind == "registered_success" {
				if e != nil || out.Status != "succeeded" || out.ReceiptID == "" {
					t.Fatal("valid registered response not observed", e)
				}
			} else if e == nil || out.Status != "unknown" {
				t.Fatal("ambiguous response treated as authoritative success", e)
			}
			if calls.Load() != 1 {
				t.Fatal("physical delivery did not occur exactly once in fixture")
			}
			f.balance(t, 270, 0)
			view, e := f.s.GetToolExecution(ctx, f.p, f.task.ID, v.Execution.ID)
			if e != nil {
				t.Fatal(e)
			}
			if kind == "registered_success" {
				if view.Receipt == nil || view.Task.Status != "waiting_input" || view.Task.Result != "" || view.Task.CandidateID != f.candidate.ID {
					t.Fatal("external success accepted or replaced original candidate")
				}
			} else if view.Execution.Status != "unknown" || view.Receipt != nil || view.Task.Status != "blocked" {
				t.Fatal("transport uncertainty was not durable")
			}
			// A fresh store/runtime instance cannot turn an old leased snapshot into a second physical action.
			reopened, e := Open(ctx, f.dsn)
			if e != nil {
				t.Fatal(e)
			}
			defer reopened.Close()
			_, _ = f.toolRuntime(t, reopened).Execute(ctx, l)
			if calls.Load() != 1 {
				t.Fatal("reopened runtime resent an already-admitted occurrence")
			}
			f.balance(t, 270, 0)
		})
	}
}

type independentDelayedToolStore struct{ *Store }

func (s independentDelayedToolStore) PrepareToolDispatch(ctx context.Context, l *domain.ToolLease) (domain.ToolDispatchPermit, error) {
	permit, e := s.Store.PrepareToolDispatch(ctx, l)
	if e == nil {
		time.Sleep(2200 * time.Millisecond)
	} // controlled post-commit delay, not a claim of an OS process crash.
	return permit, e
}
func TestIndependentToolExpiredPermitCannotBeginActualTLSRequest(t *testing.T) {
	var calls atomic.Int64
	f := independentToolSetupWithPeer(t, false, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(500) })
	v, l := f.claimed(t)
	out, e := f.toolRuntime(t, independentDelayedToolStore{f.s}).Execute(context.Background(), l)
	if e == nil || out.Status != "unknown" || calls.Load() != 0 {
		t.Fatal("expired permit started external I/O", e)
	}
	f.balance(t, 270, 0)
	current, e := f.s.GetToolExecution(context.Background(), f.p, f.task.ID, v.Execution.ID)
	if e != nil || current.Execution.Status != "unknown" || current.Receipt != nil {
		t.Fatal("unsent-after-admission uncertainty was silently released", e)
	}
}

// The UI consumes these hints; approving and refusing are different admissions.
// These tests were added after the hint fix, not claimed as a pre-fix execution.
func TestIndependentToolDecisionHintsMatchSafeDenialAuthority(t *testing.T) {
	for _, scenario := range []string{"active_owner", "distinct_approver", "distinct_self", "profile_retired", "owner_revoked", "creator_revoked", "member", "viewer", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			f := independentToolSetup(t, scenario != "active_owner")
			ctx := context.Background()
			creator := f.p
			if scenario == "creator_revoked" {
				creator = independentCollaborationActor(t, f.s, f.p.OrganizationID, "admin")
			}
			in := f.prepareInput()
			if scenario == "expired" {
				in.ExpiresInSeconds = 1
			}
			raw, err := f.s.PrepareToolExecution(ctx, creator, f.task.ID, domain.NewID("key"), in)
			v := independentToolView(t, raw, err)
			actor := f.p
			if scenario == "distinct_approver" || scenario == "profile_retired" || scenario == "owner_revoked" || scenario == "expired" {
				actor = independentCollaborationActor(t, f.s, f.p.OrganizationID, "admin")
			} else if scenario == "member" || scenario == "viewer" {
				actor = independentCollaborationActor(t, f.s, f.p.OrganizationID, scenario)
			}
			switch scenario {
			case "profile_retired":
				_, err = f.s.Pool.Exec(ctx, "UPDATE tool_connections SET active=false WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.tool.Tool.ID)
			case "owner_revoked":
				_, err = f.s.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, f.p.ID)
			case "creator_revoked":
				_, err = f.s.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, creator.ID)
			case "expired":
				time.Sleep(time.Until(v.Execution.ExpiresAt) + 40*time.Millisecond)
			}
			if err != nil {
				t.Fatal(err)
			}
			view, err := f.s.GetToolExecution(ctx, actor, f.task.ID, v.Execution.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]bool{}
			if scenario != "member" && scenario != "viewer" && scenario != "expired" {
				want["deny"] = true
			}
			if scenario == "active_owner" || scenario == "distinct_approver" {
				want["approve"] = true
			}
			if len(view.AvailableCommands) != len(want) {
				t.Fatalf("wrong commands for %s: %v", scenario, view.AvailableCommands)
			}
			for _, command := range view.AvailableCommands {
				if !want[command] {
					t.Fatalf("unexpected command %q for %s", command, scenario)
				}
			}
			if !want["approve"] {
				_, err = f.s.DecideToolExecution(ctx, actor, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDecision(v, "approve"))
				independentToolDeny(t, err)
			}
			if want["deny"] {
				denied := f.decide(t, v, actor, "deny")
				if denied.Execution.Status != "denied" || denied.Task.Status != "waiting_input" || len(denied.AvailableCommands) != 0 {
					t.Fatal("denial hint did not describe an executable safe cleanup")
				}
				f.balance(t, 150, 0)
			} else {
				_, err = f.s.DecideToolExecution(ctx, actor, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDecision(v, "deny"))
				independentToolDeny(t, err)
				f.balance(t, 270, 0)
			}
		})
	}
}

func TestIndependentToolApprovalSQLRequiresAttributionAndExactExpiry(t *testing.T) {
	f := independentToolSetup(t, false)
	ctx := context.Background()
	v := f.prepare(t)
	for _, scenario := range []string{"decider_missing", "timestamp_missing", "method_missing", "method_forged", "reason_empty", "expiry_changed"} {
		t.Run(scenario, func(t *testing.T) {
			decider, method, reason := any(f.p.ID), any("authorized_principal_decision"), "Independent attributed decision"
			at := any(time.Now().UTC())
			expires := v.Approval.ExpiresAt
			switch scenario {
			case "decider_missing":
				decider = nil
			case "timestamp_missing":
				at = nil
			case "method_missing":
				method = nil
			case "method_forged":
				method = "physical_human_presence"
			case "reason_empty":
				reason = ""
			case "expiry_changed":
				expires = expires.Add(time.Minute)
			}
			_, err := f.s.Pool.Exec(ctx, `UPDATE approvals SET status='approved',version=version+1,decided_by=$3,decided_at=$4,decision_method=$5,reason=$6,expires_at=$7 WHERE organization_id=$1 AND id=$2`, f.p.OrganizationID, v.Approval.ID, decider, at, method, reason, expires)
			if err == nil {
				t.Fatalf("database accepted %s", scenario)
			}
			view, err := f.s.GetToolExecution(ctx, f.p, f.task.ID, v.Execution.ID)
			if err != nil || view.Approval.Status != "pending" || view.Approval.Version != 1 || !view.Approval.ExpiresAt.Equal(v.Approval.ExpiresAt) {
				t.Fatal("failed SQL mutation changed the pending approval", err)
			}
		})
	}
	approved := f.decide(t, v, f.p, "approve")
	if approved.Approval.Status != "approved" || approved.Approval.DecidedBy != f.p.ID || approved.Approval.DecisionMethod != "authorized_principal_decision" || approved.Approval.DecidedAt == nil {
		t.Fatal("valid attributed decision rejected or unbound")
	}
	f.balance(t, 270, 0)
}
