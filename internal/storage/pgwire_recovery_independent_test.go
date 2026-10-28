// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
	"agentplatform.local/platform/internal/toolruntime"
)

func independentWireModelLease(t *testing.T, f *independentNativeFixture) (*domain.Lease, domain.ModelConnection) {
	t.Helper()
	l, c, err := f.s.ClaimModel(context.Background(), "independent-wire-model", 30*time.Second)
	if err != nil || l == nil {
		t.Fatal("model lease control unavailable", err)
	}
	return l, c
}
func independentWireToolLease(t *testing.T, f *independentNativeFixture) (domain.ToolExecutionView, *domain.ToolLease, domain.ToolConnection) {
	t.Helper()
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	proposal := f.proposal(t)
	view := f.decide(t, f.prepare(t, proposal), "approve")
	raw, err := f.s.DispatchToolExecution(context.Background(), f.p, f.task.ID, view.Execution.ID, domain.NewID("key"), independentToolDispatch(view))
	view = independentToolView(t, raw, err)
	l, c, err := f.s.ClaimToolExecution(context.Background(), "independent-wire-tool", 30*time.Second)
	if err != nil || l == nil || l.Execution.ID != view.Execution.ID {
		t.Fatal("tool lease control unavailable", err)
	}
	return view, l, c
}
func independentWireToolRuntime(t *testing.T, f *independentNativeFixture, s *Store, c domain.ToolConnection) *toolruntime.Runtime {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(f.toolPeer.Certificate())
	r, err := toolruntime.New(s, toolruntime.Config{Connection: c, Credential: "independent-native-tool-key", TrustedRoots: roots, PersistenceTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func independentWireOutbox(t *testing.T, s *Store, org string) {
	t.Helper()
	var missing, orphan int
	err := s.Pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM events e LEFT JOIN outbox o ON o.sequence=e.sequence AND o.organization_id=e.organization_id WHERE e.organization_id=$1 AND o.sequence IS NULL),(SELECT count(*) FROM outbox o LEFT JOIN events e ON e.sequence=o.sequence AND e.organization_id=o.organization_id WHERE o.organization_id=$1 AND e.sequence IS NULL)`, org).Scan(&missing, &orphan)
	if err != nil || missing != 0 || orphan != 0 {
		t.Fatal("commit cut separated durable state event and outbox", missing, orphan, err)
	}
}
func TestIndependentPgwireCommandAcceptanceRetainsScopedIdempotency(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			f := independentNativeSetup(t, nil)
			ctx := context.Background()
			if _, err := f.s.CancelTask(ctx, f.p, f.task.ID, domain.NewID("key")); err != nil {
				t.Fatal(err)
			}
			victim, proxy := independentCommitProxyStore(t, f.dsn)
			plan := proxy.arm(t, "insert into idempotency_keys", mode)
			key := domain.NewID("key")
			in := domain.CreateTask{AgentID: f.agent.ID, Goal: "Wire acknowledgement must not duplicate accepted native work", BudgetMicroUSD: 10000}
			_, err := victim.CreateTask(ctx, f.p, key, in)
			if err == nil {
				t.Fatal("command unexpectedly acknowledged through broken commit connection")
			}
			proxy.wait(t, plan)
			var durable int
			if err = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM tasks WHERE organization_id=$1 AND goal=$2", f.p.OrganizationID, in.Goal).Scan(&durable); err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode == "after_commit" {
				want = 1
			}
			if durable != want {
				t.Fatal("command commit observation disagrees with actual durable acceptance", durable, want)
			}
			const callers = 8
			var wg sync.WaitGroup
			values := make(chan json.RawMessage, callers)
			errs := make(chan error, callers)
			for i := 0; i < callers; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); raw, err := f.s.CreateTask(ctx, f.p, key, in); values <- raw; errs <- err }()
			}
			wg.Wait()
			close(values)
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal("same-key replay failed after wire recovery", err)
				}
			}
			var first domain.Task
			for raw := range values {
				var task domain.Task
				if json.Unmarshal(raw, &task) != nil || task.ID == "" {
					t.Fatal("replayed accepted task missing")
				}
				if first.ID == "" {
					first = task
				}
				if task.ID != first.ID || task.CurrentRunID != first.CurrentRunID || task.Version != first.Version {
					t.Fatal("same key replay created a second accepted Run")
				}
			}
			var tasks, runs, operations, reservations, keys int
			err = f.s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tasks WHERE organization_id=$1 AND goal=$2),(SELECT count(*) FROM runs WHERE organization_id=$1 AND task_id=$3),(SELECT count(*) FROM operations WHERE organization_id=$1 AND task_id=$3),(SELECT count(*) FROM reservations WHERE organization_id=$1 AND task_id=$3),(SELECT count(*) FROM idempotency_keys WHERE organization_id=$1 AND key=$4)`, f.p.OrganizationID, in.Goal, first.ID, key).Scan(&tasks, &runs, &operations, &reservations, &keys)
			if err != nil || tasks != 1 || runs != 1 || operations != 1 || reservations != 0 || keys != 1 || f.modelCalls.Load() != 0 || f.toolCalls.Load() != 0 {
				t.Fatal("ambiguous acceptance duplicated logical work, money or provider delivery", tasks, runs, operations, reservations, keys, err)
			}
			independentWireOutbox(t, f.s, f.p.OrganizationID)
		})
	}
}
func TestIndependentPgwireModelReserveCommitLossCleansOnlyProvenUnsent(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			f := independentNativeSetup(t, nil)
			l, c := independentWireModelLease(t, f)
			victim, proxy := independentCommitProxyStore(t, f.dsn)
			plan := proxy.arm(t, "insert into reservations", mode)
			runtime, err := modelruntime.New(victim, RuntimeModelConfig(c, "independent-native-model-key"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := runtime.Execute(context.Background(), l)
			if err == nil || out.Status != modelruntime.NotDispatched {
				t.Fatal("lost reservation acknowledgement dispatched a model", out.Status, err)
			}
			proxy.wait(t, plan)
			if err = f.s.PauseModelAfterRuntimeError(context.Background(), l, "model_attempt_requires_attention"); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				did, err := f.s.ReconcileModelUnsent(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if !did {
					break
				}
			}
			var count, marked, unreleased int
			err = f.s.Pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM reservations WHERE organization_id=$1 AND operation_id=$2),(SELECT count(*) FROM actions WHERE organization_id=$1 AND operation_id=$2 AND dispatch_issued_at IS NOT NULL),(SELECT count(*) FROM reservations WHERE organization_id=$1 AND operation_id=$2 AND status<>'released')`, f.p.OrganizationID, l.OperationID).Scan(&count, &marked, &unreleased)
			want := 0
			if mode == "after_commit" {
				want = 1
			}
			if err != nil || count != want || marked != 0 || unreleased != 0 || f.modelCalls.Load() != 0 {
				t.Fatal("reservation commit loss leaked or falsely spent unsent cost", count, marked, unreleased, err)
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 0, 0)
			independentWireOutbox(t, f.s, f.p.OrganizationID)
			if next, _, err := f.s.ClaimModel(context.Background(), "independent-after-reserve-cut", time.Second); err != nil || next != nil {
				t.Fatal("reservation failure caused an implicit model retry", err)
			}
		})
	}
}
func TestIndependentPgwireModelDispatchAcknowledgementUsesDurableMarker(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			f := independentNativeSetup(t, nil)
			l, c := independentWireModelLease(t, f)
			victim, proxy := independentCommitProxyStore(t, f.dsn)
			plan := proxy.arm(t, "dispatch_fencing_token=x.fencing_token", mode)
			runtime, err := modelruntime.New(victim, RuntimeModelConfig(c, "independent-native-model-key"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := runtime.Execute(context.Background(), l)
			if err == nil || out.Status != modelruntime.OutcomeUnknown {
				t.Fatal("lost dispatch acknowledgement did not retain uncertainty", out.Status, err)
			}
			proxy.wait(t, plan)
			if err = f.s.PauseModelAfterRuntimeError(context.Background(), l, "model_attempt_requires_attention"); err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.ReconcileModelUnsent(context.Background()); err != nil {
				t.Fatal(err)
			}
			var marked bool
			var status string
			if err = f.s.Pool.QueryRow(context.Background(), `SELECT a.dispatch_issued_at IS NOT NULL,r.status FROM reservations r JOIN actions a ON a.organization_id=r.organization_id AND a.id=r.action_id WHERE r.organization_id=$1 AND r.operation_id=$2`, f.p.OrganizationID, l.OperationID).Scan(&marked, &status); err != nil {
				t.Fatal(err)
			}
			held := int64(0)
			wantStatus := "released"
			if mode == "after_commit" {
				held = 300
				wantStatus = "unknown"
			}
			if marked != (mode == "after_commit") || status != wantStatus || f.modelCalls.Load() != 0 {
				t.Fatal("network error was mistaken for a durable dispatch decision", marked, status)
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, held, 0)
			independentWireOutbox(t, f.s, f.p.OrganizationID)
			if next, _, err := f.s.ClaimModel(context.Background(), "independent-after-dispatch-cut", time.Second); err != nil || next != nil {
				t.Fatal("uncertain dispatch was automatically repeated", err)
			}
		})
	}
}
func TestIndependentPgwireToolDispatchAcknowledgementUsesDurableMarker(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			f := independentNativeSetup(t, nil)
			_, lease, c := independentWireToolLease(t, f)
			victim, proxy := independentCommitProxyStore(t, f.dsn)
			plan := proxy.arm(t, "update tool_executions j set status='sent'", mode)
			out, err := independentWireToolRuntime(t, f, victim, c).Execute(context.Background(), lease)
			if err == nil || out.Status != "unknown" {
				t.Fatal("lost tool dispatch acknowledgement was presented as success", out.Status, err)
			}
			proxy.wait(t, plan)
			if err = f.s.PauseToolExecution(context.Background(), lease, "independent_wire_dispatch_uncertain"); err != nil {
				t.Fatal(err)
			}
			view, err := f.s.GetToolExecution(context.Background(), f.p, f.task.ID, lease.Execution.ID)
			if err != nil {
				t.Fatal(err)
			}
			held := int64(300)
			want := "cancelled"
			if mode == "after_commit" {
				held = 420
				want = "unknown"
			}
			if view.Execution.Status != want || view.Receipt != nil || f.toolCalls.Load() != 0 || f.modelCalls.Load() != 1 {
				t.Fatal("tool cleanup discarded actual marker state or invented a delivery", view.Execution.Status, want)
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, held, 0)
			independentWireOutbox(t, f.s, f.p.OrganizationID)
			if next, _, err := f.s.ClaimToolExecution(context.Background(), "independent-after-tool-cut", time.Second); err != nil || next != nil {
				t.Fatal("tool dispatch uncertainty was resent", err)
			}
		})
	}
}
func TestIndependentPgwireModelCandidateCommitLossPreservesDurablePublication(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			f := independentNativeSetup(t, nil)
			l, c := independentWireModelLease(t, f)
			victim, proxy := independentCommitProxyStore(t, f.dsn)
			plan := proxy.arm(t, "insert into task_candidates", mode)
			runtime, err := modelruntime.New(victim, RuntimeModelConfig(c, "independent-native-model-key"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := runtime.Execute(context.Background(), l)
			if err == nil || out.Status != modelruntime.OutcomeUnknown {
				t.Fatal("candidate commit acknowledgement unexpectedly arrived", out.Status, err)
			}
			proxy.wait(t, plan)
			if err = f.s.PauseModelAfterRuntimeError(context.Background(), l, "model_attempt_requires_attention"); err != nil {
				t.Fatal(err)
			}
			task := f.current(t)
			var candidates, proposals, createdEvents int
			err = f.s.Pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM task_candidates WHERE organization_id=$1 AND task_id=$2),(SELECT count(*) FROM model_tool_proposals WHERE organization_id=$1 AND task_id=$2),(SELECT count(*) FROM events WHERE organization_id=$1 AND task_id=$2 AND type='model.candidate_created')`, f.p.OrganizationID, f.task.ID).Scan(&candidates, &proposals, &createdEvents)
			want := 0
			if mode == "after_commit" {
				want = 1
			}
			if err != nil || candidates != want || proposals != want || createdEvents != want || f.modelCalls.Load() != 1 || f.toolCalls.Load() != 0 || task.AcceptedDecisionID != "" {
				t.Fatal("candidate ACK loss duplicated/lost durable publication or falsely accepted it", candidates, proposals, createdEvents, err)
			}
			if mode == "after_commit" {
				if task.Status != "waiting_input" || task.CandidateID == "" {
					t.Fatal("late cleanup erased committed candidate")
				}
			} else if task.Status != "blocked" || task.CandidateID != "" {
				t.Fatal("rolled-back candidate was advertised as published")
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 300, 0)
			independentWireOutbox(t, f.s, f.p.OrganizationID)
			if next, _, err := f.s.ClaimModel(context.Background(), "independent-after-candidate-cut", time.Second); err != nil || next != nil {
				t.Fatal("candidate acknowledgement failure triggered another provider request", err)
			}
		})
	}
}
func TestIndependentPgwireToolReceiptCommitLossPreservesDurableObservation(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			f := independentNativeSetup(t, nil)
			_, lease, c := independentWireToolLease(t, f)
			victim, proxy := independentCommitProxyStore(t, f.dsn)
			plan := proxy.arm(t, "insert into tool_outcome_receipts", mode)
			out, err := independentWireToolRuntime(t, f, victim, c).Execute(context.Background(), lease)
			if err == nil || out.Status != "unknown" {
				t.Fatal("receipt commit acknowledgement unexpectedly arrived", out.Status, err)
			}
			proxy.wait(t, plan)
			if err = f.s.PauseToolExecution(context.Background(), lease, "independent_wire_receipt_uncertain"); err != nil {
				t.Fatal(err)
			}
			view, err := f.s.GetToolExecution(context.Background(), f.p, f.task.ID, lease.Execution.ID)
			if err != nil {
				t.Fatal(err)
			}
			var receipts, observedEvents int
			err = f.s.Pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM tool_outcome_receipts WHERE organization_id=$1 AND execution_id=$2),(SELECT count(*) FROM events WHERE organization_id=$1 AND task_id=$3 AND type='tool.effect_observed')`, f.p.OrganizationID, lease.Execution.ID, f.task.ID).Scan(&receipts, &observedEvents)
			want := 0
			status := "unknown"
			if mode == "after_commit" {
				want = 1
				status = "succeeded"
			}
			if err != nil || receipts != want || observedEvents != want || view.Execution.Status != status || (view.Receipt != nil) != (want == 1) || f.modelCalls.Load() != 1 || f.toolCalls.Load() != 1 {
				t.Fatal("wire loss erased/duplicated receipt or external call", receipts, observedEvents, view.Execution.Status, err)
			}
			if want == 1 {
				again, err := f.s.RecordToolOutcome(context.Background(), domain.ToolOutcomeEvidence{OrganizationID: f.p.OrganizationID, ExecutionID: lease.Execution.ID, IntentHash: lease.Execution.IntentHash, FencingToken: lease.FencingToken, Outcome: "unknown"})
				if err != nil || again.ID != view.Receipt.ID {
					t.Fatal("late uncertainty overwrote committed exact receipt", err)
				}
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
			independentWireOutbox(t, f.s, f.p.OrganizationID)
			if next, _, err := f.s.ClaimToolExecution(context.Background(), "independent-after-receipt-cut", time.Second); err != nil || next != nil {
				t.Fatal("receipt ACK loss caused an unsafe resend", err)
			}
		})
	}
}

func TestIndependentPgwireAutomaticPrepareAckLossDoesNotDuplicateAllocation(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			f := independentAutomationSetup(t, nil)
			f.executeModel(t)
			grant := f.grant(t, nil)
			victim, proxy := independentCommitProxyStore(t, f.dsn)
			plan := proxy.arm(t, "insert into tool_executions", mode)
			_, err := victim.ReconcileTaskAutomation(context.Background())
			if err == nil {
				t.Fatal("automatic prepare unexpectedly acknowledged its cut commit")
			}
			proxy.wait(t, plan)
			jobs := f.executions(t)
			want := 0
			if mode == "after_commit" {
				want = 1
			}
			if len(jobs) != want {
				t.Fatal("automatic prepare wire boundary did not match durable job", len(jobs), want)
			}
			const callers = 8
			var wg sync.WaitGroup
			errs := make(chan error, callers)
			for i := 0; i < callers; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); _, err := f.s.ReconcileTaskAutomation(context.Background()); errs <- err }()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			f.reconcile(t)
			jobs = f.executions(t)
			state, err := f.s.Automation(context.Background(), f.p, f.task.ID)
			if err != nil || len(jobs) != 1 || jobs[0].Status != "queued" || jobs[0].AutomationGrantID != grant.Grant.ID || state.Usage.ModelTurns != 0 || state.Usage.ToolExecutions != 1 || state.Usage.AllocatedMicroUSD != 120 || f.modelCalls.Load() != 1 || f.readCalls.Load() != 0 {
				t.Fatal("coordinator commit replay duplicated authority/allocation or dispatched inline", jobs, state, err)
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
			independentWireOutbox(t, f.s, f.p.OrganizationID)
		})
	}
}
func TestIndependentPgwireContinuationAckLossKeepsOneNewTurnAndRun(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			f := independentNativeSetup(t, nil)
			ctx := context.Background()
			if _, err := f.executeModel(t, f.s); err != nil {
				t.Fatal(err)
			}
			proposal := f.proposal(t)
			view := f.dispatchAndObserve(t, f.prepare(t, proposal))
			in := independentNativeContinue(proposal, view)
			victim, proxy := independentCommitProxyStore(t, f.dsn)
			plan := proxy.arm(t, "insert into model_turns", mode)
			key := domain.NewID("key")
			_, err := victim.ContinueModel(ctx, f.p, f.task.ID, key, in)
			if err == nil {
				t.Fatal("continuation unexpectedly acknowledged through a broken commit")
			}
			proxy.wait(t, plan)
			var turns int
			if err = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM model_turns WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&turns); err != nil {
				t.Fatal(err)
			}
			want := 1
			if mode == "after_commit" {
				want = 2
			}
			if turns != want {
				t.Fatal("continuation wire boundary did not match durable turn", turns, want)
			}
			const callers = 8
			var wg sync.WaitGroup
			errs := make(chan error, callers)
			values := make(chan json.RawMessage, callers)
			for i := 0; i < callers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					raw, err := f.s.ContinueModel(ctx, f.p, f.task.ID, key, in)
					errs <- err
					values <- raw
				}()
			}
			wg.Wait()
			close(errs)
			close(values)
			for err := range errs {
				if err != nil {
					t.Fatal("stable-key continuation replay failed", err)
				}
			}
			for raw := range values {
				var next domain.Task
				if json.Unmarshal(raw, &next) != nil || next.ID != f.task.ID || next.Status != "queued" || next.CurrentRunID != view.Task.CurrentRunID {
					t.Fatal("continuation replay changed source task/run")
				}
			}
			var operations, own int
			if err = f.s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM model_turns WHERE organization_id=$1 AND task_id=$2),(SELECT count(*) FROM operations WHERE organization_id=$1 AND task_id=$2),(SELECT own_turns FROM model_turn_counts WHERE organization_id=$1 AND task_id=$2)`, f.p.OrganizationID, f.task.ID).Scan(&turns, &operations, &own); err != nil || turns != 2 || operations != 2 || own != 2 || f.modelCalls.Load() != 1 || f.toolCalls.Load() != 1 {
				t.Fatal("ambiguous continuation reset or duplicated durable turn/run counters", turns, operations, own, err)
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
			if _, err = f.executeModel(t, f.s); err != nil {
				t.Fatal(err)
			}
			final := f.current(t)
			if final.Status != "waiting_input" || final.AcceptedDecisionID != "" || f.modelCalls.Load() != 2 || f.toolCalls.Load() != 1 {
				t.Fatal("replayed continuation duplicated provider work or accepted unverified final answer")
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 720, 0)
			independentWireOutbox(t, f.s, f.p.OrganizationID)
		})
	}
}
