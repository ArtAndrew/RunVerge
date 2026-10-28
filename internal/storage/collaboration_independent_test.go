// SPDX-License-Identifier: Elastic-2.0
package storage

// Independent 0.4 review/retry checks. A real local HTTP provider fixture and
// PostgreSQL create the candidate; usage remains untrusted billing evidence.
// These tests do not replace the author's API, coordinator or runtime suites.
import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
	"github.com/jackc/pgx/v5/pgconn"
)

type independentCollaborationFixture struct {
	*independentModelFixture
	candidate domain.TaskCandidate
	current   domain.Task
}

func independentCollaborationSetup(t *testing.T) *independentCollaborationFixture {
	t.Helper()
	model := independentModelSetup(t, true, nil)
	out, e := model.runtime(t, model.s).Execute(context.Background(), model.lease)
	if e != nil || out.Status != modelruntime.CandidateWaitingInput || model.calls.Load() != 1 {
		t.Fatalf("candidate fixture did not execute once: %v", e)
	}
	f := &independentCollaborationFixture{independentModelFixture: model}
	f.refresh(t)
	if f.current.CurrentRunID != f.candidate.RunID || f.current.CandidateID != f.candidate.ID {
		t.Fatal("candidate does not belong to current run")
	}
	return f
}
func (f *independentCollaborationFixture) refresh(t *testing.T) {
	t.Helper()
	var e error
	f.current, e = f.s.GetTask(context.Background(), f.p, f.task.ID)
	if e != nil {
		t.Fatal(e)
	}
	candidates, _, e := f.s.Candidates(context.Background(), f.p, f.task.ID, "", 100)
	if e != nil {
		t.Fatal(e)
	}
	for _, candidate := range candidates {
		if candidate.ID == f.current.CandidateID {
			f.candidate = candidate
			return
		}
	}
	t.Fatal("current candidate not present in inspection")
}
func (f *independentCollaborationFixture) review(decision string) domain.ReviewTaskRequest {
	return domain.ReviewTaskRequest{ExpectedVersion: f.current.Version, CandidateID: f.candidate.ID, ArtifactID: f.candidate.ArtifactID, ArtifactVersion: f.candidate.ArtifactVersion, ContentDigest: f.candidate.ContentDigest, Decision: decision, Reason: "independent explicit decision"}
}
func (f *independentCollaborationFixture) money(t *testing.T) string {
	t.Helper()
	var value []byte
	e := f.s.Pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
 'organization',(SELECT jsonb_build_array(reserved_microusd,spent_microusd,financial_hold) FROM organizations WHERE id=$1),
 'task',(SELECT jsonb_build_array(reserved_microusd,spent_microusd,financial_hold) FROM tasks WHERE organization_id=$1 AND id=$2),
 'reservations',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM reservations r WHERE organization_id=$1),
 'actions',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM actions a WHERE organization_id=$1),
 'entries',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM ledger_entries e WHERE organization_id=$1),
 'evidence',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM cost_evidence c WHERE organization_id=$1),
 'incidents',(SELECT jsonb_agg(to_jsonb(i) ORDER BY id) FROM financial_incidents i WHERE organization_id=$1),
 'quarantines',(SELECT jsonb_agg(to_jsonb(q) ORDER BY id) FROM cost_quarantines q WHERE organization_id=$1))`, f.p.OrganizationID, f.task.ID).Scan(&value)
	if e != nil {
		t.Fatal(e)
	}
	return string(value)
}
func independentCollaborationActor(t *testing.T, s *Store, org, role string) domain.Principal {
	t.Helper()
	ctx := context.Background()
	id := domain.NewID("usr")
	token := domain.NewID("token")
	if _, e := s.Pool.Exec(ctx, "INSERT INTO principals(id,name) VALUES($1,'independent reviewer')", id); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Pool.Exec(ctx, "INSERT INTO memberships(organization_id,principal_id,role) VALUES($1,$2,$3)", org, id, role); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Pool.Exec(ctx, "INSERT INTO access_tokens(token_hash,principal_id) VALUES($1,$2)", Hash([]byte(token)), id); e != nil {
		t.Fatal(e)
	}
	p, e := s.Authenticate(ctx, token, org)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func TestIndependentCollaborationDecisionPreservesFinancialExposure(t *testing.T) {
	for _, decision := range []string{"accept", "reject", "request_changes"} {
		t.Run(decision, func(t *testing.T) {
			f := independentCollaborationSetup(t)
			ctx := context.Background()
			// A financial hold must stop paid admission, not decisions on existing work.
			if _, e := f.s.Pool.Exec(ctx, "UPDATE organizations SET financial_hold=true WHERE id=$1", f.p.OrganizationID); e != nil {
				t.Fatal(e)
			}
			if _, e := f.s.Pool.Exec(ctx, "UPDATE tasks SET financial_hold=true WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.task.ID); e != nil {
				t.Fatal(e)
			}
			before := f.money(t)
			request := f.review(decision)
			if _, e := f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), request); e != nil {
				t.Fatal(e)
			}
			if after := f.money(t); before != after {
				t.Fatal("result decision changed financial facts/exposure/action")
			}
			current, e := f.s.GetTask(ctx, f.p, f.task.ID)
			if e != nil {
				t.Fatal(e)
			}
			wanted := map[string]string{"accept": "succeeded", "reject": "failed", "request_changes": "blocked"}[decision]
			if current.Status != wanted || current.Version != f.current.Version+1 {
				t.Fatalf("decision state=%s version=%d", current.Status, current.Version)
			}
			if decision == "accept" {
				var content string
				if e = f.s.Pool.QueryRow(ctx, "SELECT content FROM artifacts WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.candidate.ArtifactID).Scan(&content); e != nil || current.Result != content {
					t.Fatal("accepted bytes differ from candidate")
				}
			} else if current.Result != "" {
				t.Fatal("unaccepted candidate became result")
			}
			if decision == "request_changes" && current.WaitingReason != "candidate_changes_requested" {
				t.Fatal("request_changes not independently retryable state")
			}
			decisions, _, e := f.s.Decisions(ctx, f.p, f.task.ID, "", 100)
			if e != nil || len(decisions) != 1 {
				t.Fatal("missing unique decision receipt")
			}
			d := decisions[0]
			if d.CandidateID != f.candidate.ID || d.RunID != f.candidate.RunID || d.ArtifactID != f.candidate.ArtifactID || d.ArtifactVersion != f.candidate.ArtifactVersion || d.ContentDigest != f.candidate.ContentDigest || d.Decision != decision || d.PrincipalID != f.p.ID || d.Method != "authorized_user_decision" {
				t.Fatal("decision receipt is not bound to authorized exact snapshot")
			}
			if f.calls.Load() != 1 {
				t.Fatal("decision caused another provider call")
			}
		})
	}
}
func TestIndependentCollaborationRejectsSnapshotSubstitution(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx := context.Background()
	before := f.money(t)
	mutations := []struct {
		name   string
		change func(*domain.ReviewTaskRequest)
	}{
		{"candidate", func(r *domain.ReviewTaskRequest) { r.CandidateID = domain.NewID("cand") }},
		{"artifact", func(r *domain.ReviewTaskRequest) { r.ArtifactID = domain.NewID("art") }},
		{"version", func(r *domain.ReviewTaskRequest) { r.ArtifactVersion++ }},
		{"digest", func(r *domain.ReviewTaskRequest) { r.ContentDigest = Hash([]byte("not reviewed bytes")) }},
		{"task_version", func(r *domain.ReviewTaskRequest) { r.ExpectedVersion-- }},
	}
	for _, item := range mutations {
		t.Run(item.name, func(t *testing.T) {
			request := f.review("accept")
			item.change(&request)
			if _, e := f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), request); e == nil {
				t.Fatal("snapshot substitution accepted")
			}
		})
	}
	if f.money(t) != before {
		t.Fatal("rejected snapshot changed financial exposure")
	}
	var count int
	if e := f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM task_decisions WHERE organization_id=$1", f.p.OrganizationID).Scan(&count); e != nil || count != 0 {
		t.Fatal("rejected snapshot persisted a receipt")
	}
	// Ordinary SQL cannot mutate a snapshot after it became a candidate.
	if _, e := f.s.Pool.Exec(ctx, "UPDATE artifacts SET content='changed' WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.candidate.ArtifactID); e == nil {
		t.Fatal("candidate bytes mutable")
	}
}
func TestIndependentCollaborationReplayRechecksCommandPermission(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx := context.Background()
	admin := independentCollaborationActor(t, f.s, f.p.OrganizationID, "admin")
	request := f.review("accept")
	key := domain.NewID("key")
	if _, e := f.s.ReviewTask(ctx, admin, f.task.ID, key, request); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.Pool.Exec(ctx, "UPDATE memberships SET role='member' WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, admin.ID); e != nil {
		t.Fatal(e)
	}
	_, e := f.s.ReviewTask(ctx, admin, f.task.ID, key, request)
	code(t, e, "forbidden")
}
func TestIndependentCollaborationConcurrentReviewHasOneReceipt(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx := context.Background()
	request := f.review("accept")
	key := domain.NewID("key")
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := f.s.ReviewTask(ctx, f.p, f.task.ID, key, request); results <- e }()
	}
	wg.Wait()
	close(results)
	for e := range results {
		if e != nil {
			t.Fatal(e)
		}
	}
	var count int
	if e := f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM task_decisions WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&count); e != nil || count != 1 {
		t.Fatalf("review receipts=%d err=%v", count, e)
	}
	changed := request
	changed.Decision = "reject"
	if _, e := f.s.ReviewTask(ctx, f.p, f.task.ID, key, changed); e == nil {
		t.Fatal("changed review payload reused committed idempotency key")
	}
	var decisionID string
	if e := f.s.Pool.QueryRow(ctx, "SELECT id FROM task_decisions WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&decisionID); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.Pool.Exec(ctx, "DELETE FROM task_decisions WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, decisionID); e == nil {
		t.Fatal("decision receipt deletable")
	}
}
func TestIndependentCollaborationRetryUsesNewRunWithoutResettingCosts(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx := context.Background()
	if _, e := f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), f.review("request_changes")); e != nil {
		t.Fatal(e)
	}
	current, e := f.s.GetTask(ctx, f.p, f.task.ID)
	if e != nil {
		t.Fatal(e)
	}
	before := f.money(t)
	request := domain.RetryTaskRequest{ExpectedVersion: current.Version, Reason: "explicit new attempt", Feedback: "Use a precise, shorter explanation.", AcknowledgeNewCost: true}
	key := domain.NewID("key")
	if _, e = f.s.RetryTask(ctx, f.p, f.task.ID, key, request); e != nil {
		t.Fatal(e)
	}
	if f.money(t) != before {
		t.Fatal("retry changed earlier cost/action facts")
	}
	after, e := f.s.GetTask(ctx, f.p, f.task.ID)
	if e != nil {
		t.Fatal(e)
	}
	if after.CurrentRunID == current.CurrentRunID || after.AttemptCount != current.AttemptCount+1 || after.RootID != current.RootID || after.BudgetMicroUSD != current.BudgetMicroUSD || after.CandidateID != "" || after.Result != "" {
		t.Fatal("retry did not create a clean current attempt under original task budget")
	}
	var input, digest, feedback string
	if e = f.s.Pool.QueryRow(ctx, "SELECT input_context,input_digest,feedback FROM runs WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, after.CurrentRunID).Scan(&input, &digest, &feedback); e != nil {
		t.Fatal(e)
	}
	if feedback != request.Feedback || Hash([]byte(input)) != digest || !strings.Contains(input, request.Feedback) {
		t.Fatal("retry feedback not in immutable input digest")
	}
	var snapshot domain.RunInput
	if json.Unmarshal([]byte(input), &snapshot) != nil || snapshot.OriginalGoal != f.task.Goal || snapshot.Feedback != request.Feedback {
		t.Fatal("retry input silently replaced original goal")
	}
	if _, e = f.s.Pool.Exec(ctx, "UPDATE runs SET feedback='silent edit' WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, after.CurrentRunID); e == nil {
		t.Fatal("retry feedback mutable")
	}
	if _, e = f.s.RetryTask(ctx, f.p, f.task.ID, key, request); e != nil {
		t.Fatal(e)
	}
	replay, e := f.s.GetTask(ctx, f.p, f.task.ID)
	if e != nil || replay.AttemptCount != after.AttemptCount || replay.CurrentRunID != after.CurrentRunID {
		t.Fatal("retry replay incremented lifetime count")
	}
	var oldState string
	if e = f.s.Pool.QueryRow(ctx, "SELECT status FROM operations WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.candidate.OperationID).Scan(&oldState); e != nil || oldState == "queued" || oldState == "leased" {
		t.Fatal("old paid operation is redispatchable")
	}
	if f.calls.Load() != 1 {
		t.Fatal("retry endpoint itself called a provider")
	}
}

func TestIndependentCollaborationRetryCapIsLifetimeAndReplayDoesNotSpendSlot(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx := context.Background()
	if f.current.MaxRuns != 3 {
		t.Fatal("fixture default max_runs changed; test needs explicit contract update")
	}
	for expectedAttempt := 1; expectedAttempt <= 3; expectedAttempt++ {
		if f.current.AttemptCount != expectedAttempt {
			t.Fatal("attempt count reset or skipped")
		}
		if _, e := f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), f.review("request_changes")); e != nil {
			t.Fatal(e)
		}
		current, e := f.s.GetTask(ctx, f.p, f.task.ID)
		if e != nil {
			t.Fatal(e)
		}
		retry := domain.RetryTaskRequest{ExpectedVersion: current.Version, Reason: "bounded distinct attempt", Feedback: "Make the correction explicit.", AcknowledgeNewCost: true}
		key := domain.NewID("key")
		before := f.money(t)
		_, e = f.s.RetryTask(ctx, f.p, f.task.ID, key, retry)
		if expectedAttempt == 3 {
			if e == nil {
				t.Fatal("fourth task-lifetime attempt admitted")
			}
			if f.money(t) != before {
				t.Fatal("attempt-cap rejection changed financial facts")
			}
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.s.RetryTask(ctx, f.p, f.task.ID, key, retry); e != nil {
			t.Fatal(e)
		}
		var runCount int
		if e = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM runs WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&runCount); e != nil || runCount != expectedAttempt+1 {
			t.Fatal("replay created another run")
		}
		lease, connection, e := f.s.ClaimModel(ctx, "independent-correction-worker", 30*time.Second)
		if e != nil || lease == nil || connection.ID != f.c.ID {
			t.Fatal("new current attempt not claimable")
		}
		f.lease = lease
		out, e := f.runtime(t, f.s).Execute(ctx, lease)
		if e != nil || out.Status != modelruntime.CandidateWaitingInput {
			t.Fatalf("new attempt candidate: %v", e)
		}
		f.refresh(t)
	}
	var taskCount, attempts int
	if e := f.s.Pool.QueryRow(ctx, "SELECT used_tasks,used_attempts FROM task_tree_controls WHERE organization_id=$1 AND root_id=$2", f.p.OrganizationID, f.task.RootID).Scan(&taskCount, &attempts); e != nil || taskCount != 1 || attempts != 3 {
		t.Fatalf("lifetime tree counters tasks=%d attempts=%d err=%v", taskCount, attempts, e)
	}
	if f.calls.Load() != 3 {
		t.Fatal("retry cap did not bound provider fixture calls")
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 450, 0)
}

func TestIndependentCollaborationTerminalAndUnacknowledgedRetryAreRejected(t *testing.T) {
	for _, decision := range []string{"accept", "reject", "cancel", "request_changes"} {
		t.Run(decision, func(t *testing.T) {
			f := independentCollaborationSetup(t)
			ctx := context.Background()
			if decision == "cancel" {
				if _, e := f.s.CancelTask(ctx, f.p, f.task.ID, domain.NewID("key")); e != nil {
					t.Fatal(e)
				}
			} else {
				if _, e := f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), f.review(decision)); e != nil {
					t.Fatal(e)
				}
			}
			current, e := f.s.GetTask(ctx, f.p, f.task.ID)
			if e != nil {
				t.Fatal(e)
			}
			before := f.money(t)
			retry := domain.RetryTaskRequest{ExpectedVersion: current.Version, Reason: "must not start", AcknowledgeNewCost: decision != "request_changes"}
			if _, e = f.s.RetryTask(ctx, f.p, f.task.ID, domain.NewID("key"), retry); e == nil {
				t.Fatal("terminal or unacknowledged retry accepted")
			}
			after, e := f.s.GetTask(ctx, f.p, f.task.ID)
			if e != nil || after.AttemptCount != current.AttemptCount || after.CurrentRunID != current.CurrentRunID || after.Status != current.Status {
				t.Fatal("rejected retry changed current attempt")
			}
			if f.money(t) != before || f.calls.Load() != 1 {
				t.Fatal("rejected retry changed external or financial state")
			}
		})
	}
}

func TestIndependentCollaborationConcurrentRetryCreatesOneCurrentRun(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx := context.Background()
	if _, e := f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), f.review("request_changes")); e != nil {
		t.Fatal(e)
	}
	current, e := f.s.GetTask(ctx, f.p, f.task.ID)
	if e != nil {
		t.Fatal(e)
	}
	request := domain.RetryTaskRequest{ExpectedVersion: current.Version, Reason: "one explicit retry", Feedback: "one correction", AcknowledgeNewCost: true}
	key := domain.NewID("key")
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := f.s.RetryTask(ctx, f.p, f.task.ID, key, request); results <- e }()
	}
	wg.Wait()
	close(results)
	for e := range results {
		if e != nil {
			t.Fatal(e)
		}
	}
	var runs, queued, attempts int
	if e = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM runs WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&runs); e != nil {
		t.Fatal(e)
	}
	if e = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM operations WHERE organization_id=$1 AND task_id=$2 AND status='queued'", f.p.OrganizationID, f.task.ID).Scan(&queued); e != nil {
		t.Fatal(e)
	}
	if e = f.s.Pool.QueryRow(ctx, "SELECT used_attempts FROM task_tree_controls WHERE organization_id=$1 AND root_id=$2", f.p.OrganizationID, f.task.RootID).Scan(&attempts); e != nil {
		t.Fatal(e)
	}
	if runs != 2 || queued != 1 || attempts != 2 {
		t.Fatalf("concurrent retry multiplied work: runs=%d queued=%d attempts=%d", runs, queued, attempts)
	}
	if f.calls.Load() != 1 {
		t.Fatal("retry endpoint performed paid call")
	}
}

func TestIndependentCollaborationReplayRechecksExpiryAfterIdempotencyWait(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := f.review("accept")
	key := domain.NewID("key")
	if _, e := f.s.ReviewTask(ctx, f.p, f.task.ID, key, request); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.Pool.Exec(ctx, "UPDATE access_tokens SET expires_at=clock_timestamp()+interval '400 milliseconds' WHERE token_hash=$1", f.p.TokenHash); e != nil {
		t.Fatal(e)
	}
	blocker, e := f.s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback(ctx)
	lock := strings.Join([]string{f.p.OrganizationID, f.p.ID, "POST /tasks/" + f.task.ID + "/review", key}, "\x1f")
	if _, e = blocker.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", lock); e != nil {
		t.Fatal(e)
	}
	replay := make(chan error, 1)
	go func() { _, e := f.s.ReviewTask(ctx, f.p, f.task.ID, key, request); replay <- e }()
	waitForBlockedQuery(t, f.s, "SELECT pg_advisory_xact_lock(hashtextextended(")
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
	e = <-replay
	code(t, e, "unauthenticated")
}

func independentCollaborationWaiters(t *testing.T, s *Store, blocker int, wanted int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var n int
		e := s.Pool.QueryRow(ctx, `WITH RECURSIVE waiters AS MATERIALIZED (SELECT pid,pg_blocking_pids(pid) AS blockers FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT max_tasks,used_tasks,max_total_attempts,used_attempts,max_depth FROM task_tree_controls%'), blocked AS (SELECT pid FROM waiters WHERE $1=ANY(blockers) UNION SELECT w.pid FROM waiters w JOIN blocked b ON b.pid=ANY(w.blockers)) SELECT count(*) FROM blocked`, blocker).Scan(&n)
		if e != nil {
			t.Fatal(e)
		}
		if n >= wanted {
			return
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatalf("expected %d tree gate waiters, saw %d", wanted, n)
		}
	}
}
func TestIndependentCollaborationReviewCancelForcedGateOrder(t *testing.T) {
	for _, first := range []string{"cancel", "review"} {
		t.Run(first, func(t *testing.T) {
			f := independentCollaborationSetup(t)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			blocker, e := f.s.Pool.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer blocker.Rollback(ctx)
			var blockerPID int
			if e = blocker.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); e != nil {
				t.Fatal(e)
			}
			if _, e = blocker.Exec(ctx, "SELECT root_id FROM task_tree_controls WHERE organization_id=$1 AND root_id=$2 FOR UPDATE", f.p.OrganizationID, f.task.RootID); e != nil {
				t.Fatal(e)
			}
			review := make(chan error, 1)
			cancelled := make(chan error, 1)
			request := f.review("accept")
			before := f.money(t)
			doReview := func() { _, e := f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), request); review <- e }
			doCancel := func() { _, e := f.s.CancelTask(ctx, f.p, f.task.ID, domain.NewID("key")); cancelled <- e }
			if first == "cancel" {
				go doCancel()
			} else {
				go doReview()
			}
			independentCollaborationWaiters(t, f.s, blockerPID, 1)
			if first == "cancel" {
				go doReview()
			} else {
				go doCancel()
			}
			independentCollaborationWaiters(t, f.s, blockerPID, 2)
			if e = blocker.Commit(ctx); e != nil {
				t.Fatal(e)
			}
			reviewError, cancelError := <-review, <-cancelled
			if cancelError != nil {
				t.Fatal(cancelError)
			}
			current, e := f.s.GetTask(ctx, f.p, f.task.ID)
			if e != nil {
				t.Fatal(e)
			}
			var count int
			if e = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM task_decisions WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&count); e != nil {
				t.Fatal(e)
			}
			if first == "cancel" {
				if reviewError == nil || current.Status != "cancelled" || current.Result != "" || count != 0 {
					t.Fatal("review resurrected cancellation that acquired tree gate first")
				}
			} else {
				if reviewError != nil || current.Status != "succeeded" || current.Result == "" || count != 1 {
					t.Fatal("completed review corrupted by later terminal cancellation")
				}
			}
			if f.money(t) != before {
				t.Fatal("review/cancel ordering altered unknown financial facts")
			}
		})
	}
}
func TestIndependentCollaborationDecisionTupleEnforcedByDatabase(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx := context.Background()
	for _, kind := range []string{"artifact", "version", "digest"} {
		t.Run(kind, func(t *testing.T) {
			artifact, version, digest := f.candidate.ArtifactID, f.candidate.ArtifactVersion, f.candidate.ContentDigest
			switch kind {
			case "artifact":
				artifact = domain.NewID("art")
				if _, e := f.s.Pool.Exec(ctx, "INSERT INTO artifacts(organization_id,id,task_id,name,media_type,content,provenance) VALUES($1,$2,$3,'other.txt','text/plain','other snapshot','{}')", f.p.OrganizationID, artifact, f.task.ID); e != nil {
					t.Fatal(e)
				}
			case "version":
				version++
			case "digest":
				digest = Hash([]byte("different exact content"))
			}
			_, e := f.s.Pool.Exec(ctx, `INSERT INTO task_decisions(organization_id,id,task_id,run_id,candidate_id,artifact_id,artifact_version,content_digest,decision,reason,principal_id,method,task_version_before,task_version_after)
  VALUES($1,$2,$3,$4,$5,$6,$7,$8,'accept','must reject tuple',$9,'authorized_user_decision',$10,$11)`, f.p.OrganizationID, domain.NewID("decision"), f.task.ID, f.current.CurrentRunID, f.candidate.ID, artifact, version, digest, f.p.ID, f.current.Version, f.current.Version+1)
			var pgError *pgconn.PgError
			if !errors.As(e, &pgError) || pgError.Code != "23503" {
				t.Fatalf("exact candidate snapshot FK expected, got %v", e)
			}
		})
	}
}
func TestIndependentCollaborationOutboxFailureRollsBackReview(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx := context.Background()
	before := f.money(t)
	if _, e := f.s.Pool.Exec(ctx, `CREATE FUNCTION independent_review_fail_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'independent injected outbox failure'; END $$`); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.Pool.Exec(ctx, "CREATE TRIGGER independent_review_outbox_failure BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION independent_review_fail_outbox()"); e != nil {
		t.Fatal(e)
	}
	request := f.review("accept")
	key := domain.NewID("key")
	if _, e := f.s.ReviewTask(ctx, f.p, f.task.ID, key, request); e == nil {
		t.Fatal("outbox fault did not abort decision")
	}
	current, e := f.s.GetTask(ctx, f.p, f.task.ID)
	if e != nil || current.Status != f.current.Status || current.Version != f.current.Version || current.Result != "" {
		t.Fatal("failed commit leaked result transition")
	}
	var count int
	if e = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM task_decisions WHERE organization_id=$1", f.p.OrganizationID).Scan(&count); e != nil || count != 0 {
		t.Fatal("failed commit leaked immutable decision")
	}
	if before != f.money(t) {
		t.Fatal("failed review altered money")
	}
	if _, e = f.s.Pool.Exec(ctx, "DROP TRIGGER independent_review_outbox_failure ON outbox"); e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.ReviewTask(ctx, f.p, f.task.ID, key, request); e != nil {
		t.Fatal("rolled-back key cannot be retried:", e)
	}
}

func TestIndependentCollaborationReviewAfterSettlementOverageOrQuarantine(t *testing.T) {
	for _, kind := range []string{"settled", "overage", "quarantine"} {
		t.Run(kind, func(t *testing.T) {
			f := independentCollaborationSetup(t)
			ctx := context.Background()
			var reservationID string
			if e := f.s.Pool.QueryRow(ctx, "SELECT id FROM reservations WHERE organization_id=$1 AND operation_id=$2", f.p.OrganizationID, f.candidate.OperationID).Scan(&reservationID); e != nil {
				t.Fatal(e)
			}
			actual := int64(25)
			if kind == "overage" {
				actual = 2000
			}
			if kind == "quarantine" {
				// Both reservations exist before a real checked counter reaches int64 max.
				// The second is controlled internal financial test evidence, not an invoice.
				lease := independentLease(t, f.s, f.p, f.lease.Agent, "", 1000)
				other := independentReserve(t, f.s, lease, 1)
				outcome, e := f.s.AccountCost(ctx, f.p.OrganizationID, other.ID, independentEvidence(math.MaxInt64))
				if e != nil || outcome.Status != "settled" {
					t.Fatalf("overflow setup failed: %v", e)
				}
				actual = 1
			}
			evidence := independentEvidence(actual)
			evidence.Source = f.c.ProviderAccount // Exact configured provider-account binding, not the generic ledger fixture.
			outcome, e := f.s.AccountCost(ctx, f.p.OrganizationID, reservationID, evidence)
			if e != nil {
				t.Fatal(e)
			}
			expected := "settled"
			if kind == "quarantine" {
				expected = "quarantined"
			}
			if outcome.Status != expected {
				t.Fatalf("accounting outcome=%s want=%s", outcome.Status, expected)
			}
			before := f.money(t)
			// Accounting advances Task.Version; stale decisions must conflict even
			// when the immutable candidate bytes have not changed.
			_, e = f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), f.review("accept"))
			code(t, e, "conflict")
			f.refresh(t)
			if _, e = f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), f.review("accept")); e != nil {
				t.Fatal("existing result could not be reviewed after accounting:", e)
			}
			if f.money(t) != before {
				t.Fatal("result acceptance changed settled/quarantined financial facts")
			}
			if kind != "settled" {
				current, e := f.s.GetTask(ctx, f.p, f.task.ID)
				if e != nil || !current.FinancialHold {
					t.Fatal("acceptance cleared financial hold")
				}
			}
		})
	}
}

func TestIndependentCollaborationRetryDeadlineAfterProfileLockWait(t *testing.T) {
	model := independentModelSetup(t, false, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, e := model.s.CancelTask(ctx, model.p, model.task.ID, domain.NewID("key")); e != nil {
		t.Fatal(e)
	}
	seconds := 2
	raw, e := model.s.CreateTask(ctx, model.p, domain.NewID("key"), domain.CreateTask{Goal: "short deadline independent retry", AgentID: model.task.AgentID, BudgetMicroUSD: 1000, ExecutionTimeoutSeconds: &seconds})
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &model.task); e != nil {
		t.Fatal(e)
	}
	model.lease, _, e = model.s.ClaimModel(ctx, "independent-deadline-worker", 30*time.Second)
	if e != nil || model.lease == nil {
		t.Fatal("short task not claimed")
	}
	out, e := model.runtime(t, model.s).Execute(ctx, model.lease)
	if e != nil || out.Status != modelruntime.CandidateWaitingInput {
		t.Fatalf("short task candidate failed: %v", e)
	}
	f := &independentCollaborationFixture{independentModelFixture: model}
	f.refresh(t)
	if _, e = f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), f.review("request_changes")); e != nil {
		t.Fatal(e)
	}
	current, e := f.s.GetTask(ctx, f.p, f.task.ID)
	if e != nil {
		t.Fatal(e)
	}
	blocker, e := f.s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback(ctx)
	if _, e = blocker.Exec(ctx, "SELECT id FROM model_connections WHERE organization_id=$1 AND id=$2 AND version=$3 FOR UPDATE", f.p.OrganizationID, f.c.ID, f.c.Version); e != nil {
		t.Fatal(e)
	}
	retry := make(chan error, 1)
	go func() {
		_, e := f.s.RetryTask(ctx, f.p, f.task.ID, domain.NewID("key"), domain.RetryTaskRequest{ExpectedVersion: current.Version, Reason: "deadline must still apply", Feedback: "small correction", AcknowledgeNewCost: true})
		retry <- e
	}()
	waitForBlockedQuery(t, f.s, "SELECT profile,active,created_at FROM model_connections WHERE organization_id=")
	for {
		var expired bool
		if e = f.s.Pool.QueryRow(ctx, "SELECT execution_deadline<clock_timestamp() FROM tasks WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.task.ID).Scan(&expired); e != nil {
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
	if e = <-retry; e == nil {
		t.Fatal("new run admitted after execution deadline expired during profile lock wait")
	}
	after, e := f.s.GetTask(ctx, f.p, f.task.ID)
	if e != nil || after.AttemptCount != current.AttemptCount || after.CurrentRunID != current.CurrentRunID {
		t.Fatal("expired admission consumed an attempt slot")
	}
}
