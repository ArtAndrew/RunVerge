// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
	"agentplatform.local/platform/internal/toolruntime"
)

// A real PostgreSQL commit can succeed while its acknowledgement is lost. The
// fixture changes only the returned error; authoritative rows remain untouched.
type authorToolAdmissionAckLoss struct {
	*Store
	commit bool
}

func (s authorToolAdmissionAckLoss) PrepareToolDispatch(ctx context.Context, l *domain.ToolLease) (domain.ToolDispatchPermit, error) {
	if s.commit {
		if _, err := s.Store.PrepareToolDispatch(ctx, l); err != nil {
			return domain.ToolDispatchPermit{}, err
		}
	}
	return domain.ToolDispatchPermit{}, errors.New("synthetic dispatch acknowledgement unavailable")
}
func TestAuthorToolCleanupDistinguishesLostCommitAcknowledgementFromProvenUnsent(t *testing.T) {
	for _, commit := range []bool{false, true} {
		name := "denied_without_marker"
		if commit {
			name = "committed_marker_acknowledgement_lost"
		}
		t.Run(name, func(t *testing.T) {
			f := independentNativeSetup(t, nil)
			ctx := context.Background()
			if _, err := f.executeModel(t, f.s); err != nil {
				t.Fatal(err)
			}
			proposal := f.proposal(t)
			v := f.decide(t, f.prepare(t, proposal), "approve")
			raw, err := f.s.DispatchToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDispatch(v))
			v = independentToolView(t, raw, err)
			l, c, err := f.s.ClaimToolExecution(ctx, "author-ack-loss-tool-worker", 30*time.Second)
			if err != nil || l == nil {
				t.Fatal(err)
			}
			runtime, err := toolruntime.New(authorToolAdmissionAckLoss{Store: f.s, commit: commit}, toolruntime.Config{Connection: c, Credential: "independent-native-tool-key", PersistenceTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			out, err := runtime.Execute(ctx, l)
			if err == nil || out.Status != "unknown" || f.toolCalls.Load() != 0 {
				t.Fatal("admission error was presented as a definitive unsent result", err)
			}
			if err = f.s.PauseToolExecution(ctx, l, "author_dispatch_acknowledgement_unavailable"); err != nil {
				t.Fatal(err)
			}
			v, err = f.s.GetToolExecution(ctx, f.p, f.task.ID, v.Execution.ID)
			if err != nil {
				t.Fatal(err)
			}
			var reservation, action string
			var marked bool
			if err = f.s.Pool.QueryRow(ctx, "SELECT r.status,a.status,a.dispatch_issued_at IS NOT NULL FROM reservations r JOIN actions a ON a.organization_id=r.organization_id AND a.id=r.action_id WHERE r.organization_id=$1 AND r.id=$2", f.p.OrganizationID, v.Execution.ReservationID).Scan(&reservation, &action, &marked); err != nil {
				t.Fatal(err)
			}
			if commit {
				if !marked || v.Execution.Status != "unknown" || reservation != "unknown" || action != "unknown" || v.Task.Status != "blocked" || v.Receipt != nil {
					t.Fatal("committed permit was released or invented a receipt after lost acknowledgement", v.Execution.Status, reservation, action)
				}
				independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
			} else {
				if marked || v.Execution.Status != "cancelled" || reservation != "released" || action != "rejected" || v.Task.WaitingReason != "model_tools_pending" || v.Receipt != nil {
					t.Fatal("proven unsent attempt did not release safely", v.Execution.Status, reservation, action)
				}
				independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 300, 0)
			}
			again, _, err := f.s.ClaimToolExecution(ctx, "author-new-worker-after-ack-error", 30*time.Second)
			if err != nil || again != nil || f.toolCalls.Load() != 0 {
				t.Fatal("cleanup allowed an automatic resend", err)
			}
		})
	}
}

func TestAuthorAutomationRootHardGuardStopsSiblingDuplicateOrUnknownEffect(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		name := "successful_write_in_sibling"
		if unknown {
			name = "unknown_effect_in_sibling"
		}
		t.Run(name, func(t *testing.T) {
			f := independentNativeSetup(t, func(f *independentNativeFixture) { f.alwaysProposes = true; f.toolUnknown = unknown })
			ctx := context.Background()
			if _, err := f.s.CancelTask(ctx, f.p, f.task.ID, domain.NewID("key")); err != nil {
				t.Fatal(err)
			}
			team := teamAgent(t, f.s, f.p, "parallel", "all", 2, teamMember("first", f.agent), teamMember("second", f.agent))
			root := task(t, f.s, f.p, team)
			teamTick(t, f.s)
			teamTick(t, f.s)
			for i := 0; i < 2; i++ {
				if _, err := f.executeModel(t, f.s); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := f.s.Pool.Query(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND root_id=$2 AND id<>$2 ORDER BY id", f.p.OrganizationID, root.ID)
			if err != nil {
				t.Fatal(err)
			}
			children := []domain.Task{}
			for rows.Next() {
				v, e := scanTask(rows)
				if e != nil {
					t.Fatal(e)
				}
				children = append(children, v)
			}
			err = rows.Err()
			rows.Close()
			if err != nil || len(children) != 2 {
				t.Fatal("expected two real team children", err)
			}
			f.task = children[0]
			first := f.dispatchAndObserve(t, f.prepare(t, f.proposal(t)))
			if unknown && first.Execution.Status != "unknown" {
				t.Fatal("unknown injection failed")
			}
			f.task = children[1]
			current := f.current(t)
			raw, err := f.s.CreateAutomation(ctx, f.p, current.ID, domain.NewID("key"), domain.CreateTaskAutomation{ExpectedTaskVersion: current.Version, Mode: "prepare_and_continue", ToolPins: []domain.AutomationToolPin{{ToolID: f.tool.Tool.ID, ToolVersion: 1, ToolProfileDigest: domain.ToolConnectionDigest(f.tool)}}, MaxModelTurns: 3, MaxToolExecutions: 4, MaxChargeMicroUSD: 5000, ExpiresInSeconds: 60, AcknowledgeCost: true, Reason: "Explicit separate child consent; no inherited grant"})
			if err != nil {
				t.Fatal(err)
			}
			var granted domain.TaskAutomationState
			if json.Unmarshal(raw, &granted) != nil {
				t.Fatal("decode grant")
			}
			changed, err := f.s.ReconcileTaskAutomation(ctx)
			if err != nil || !changed {
				t.Fatal("root hard guard did not reconcile", err)
			}
			state, err := f.s.Automation(ctx, f.p, current.ID)
			if err != nil {
				t.Fatal(err)
			}
			reason := "repeated_effect_intent"
			if unknown {
				reason = "unknown_effect"
			}
			if state.Grant.Status != "stopped" || state.Grant.StopReason != reason || len(state.Signals) == 0 || state.Usage.ToolExecutions != 0 || state.Usage.ModelTurns != 0 || f.toolCalls.Load() != 1 {
				t.Fatal("sibling bypassed the shared root hard guard", state.Grant.StopReason, state.Usage)
			}
			var secondCount int
			if err = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM tool_executions WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, current.ID).Scan(&secondCount); err != nil || secondCount != 0 {
				t.Fatal("guard stopped after preparing a duplicate sibling effect", err)
			}
			after := f.current(t)
			if after.Status != "waiting_input" || after.CandidateID != current.CandidateID || after.Version != current.Version {
				t.Fatal("automation stop changed manual candidate or claimed task failure")
			}
			if len(state.Signals[0].EvidenceIDs) != 1 {
				t.Fatal("root stop omitted the sibling witness")
			}
		})
	}
}

type authorModelAdmissionAckLoss struct {
	*Store
	commit bool
}

func (s authorModelAdmissionAckLoss) PrepareModelDispatch(ctx context.Context, l *domain.Lease, id string) (domain.DispatchPermit, error) {
	if s.commit {
		if _, err := s.Store.PrepareModelDispatch(ctx, l, id); err != nil {
			return domain.DispatchPermit{}, err
		}
	}
	return domain.DispatchPermit{}, errors.New("synthetic model dispatch acknowledgement unavailable")
}
func TestAuthorModelCleanupDistinguishesCommitLossAndPreservesNewRun(t *testing.T) {
	for _, commit := range []bool{false, true} {
		name := "denied_without_marker"
		if commit {
			name = "committed_marker_acknowledgement_lost"
		}
		t.Run(name, func(t *testing.T) {
			f := independentNativeSetup(t, nil)
			ctx := context.Background()
			l, c, err := f.s.ClaimModel(ctx, "author-model-ack-loss-worker", 30*time.Second)
			if err != nil || l == nil {
				t.Fatal(err)
			}
			runtime, err := modelruntime.New(authorModelAdmissionAckLoss{Store: f.s, commit: commit}, RuntimeModelConfig(c, "independent-native-model-key"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := runtime.Execute(ctx, l)
			if err == nil || out.Status != modelruntime.OutcomeUnknown || out.ReservationID == "" || f.modelCalls.Load() != 0 {
				t.Fatal("unexpected controlled model admission result", err)
			}
			if err = f.s.PauseModelAfterRuntimeError(ctx, l, "model_attempt_requires_attention"); err != nil {
				t.Fatal(err)
			}
			current := f.current(t)
			if current.Status != "blocked" {
				t.Fatal("model error left running work without a durable attention state")
			}
			if commit {
				if current.WaitingReason != "model_outcome_unknown" {
					t.Fatal("committed marker was labelled known unsent")
				}
				independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 300, 0)
				raw, err := f.s.RetryTask(ctx, f.p, f.task.ID, domain.NewID("key"), domain.RetryTaskRequest{ExpectedVersion: current.Version, AcknowledgeNewCost: true, Reason: "Explicit separate attempt; preserve earlier uncertainty"})
				if err != nil {
					t.Fatal(err)
				}
				var next domain.Task
				if json.Unmarshal(raw, &next) != nil || next.CurrentRunID == l.RunID || next.Status != "queued" {
					t.Fatal("explicit new Run not created")
				}
				if err = f.s.PauseModelAfterRuntimeError(ctx, l, "model_attempt_requires_attention"); err != nil {
					t.Fatal(err)
				}
				after := f.current(t)
				if after.CurrentRunID != next.CurrentRunID || after.Version != next.Version || after.Status != "queued" {
					t.Fatal("delayed cleanup changed a newer Run")
				}
			} else {
				if current.WaitingReason != "model_attempt_requires_attention" {
					t.Fatal("unsent cleanup invented a provider outcome")
				}
				changed, err := f.s.ReconcileModelUnsent(ctx)
				if err != nil || !changed {
					t.Fatal("proven unsent reservation was not recoverable", err)
				}
				independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 0, 0)
			}
		})
	}
}
func TestAuthorModelCleanupCanRecordAttentionAfterOwnerRevocation(t *testing.T) {
	f := independentNativeSetup(t, nil)
	ctx := context.Background()
	l, _, err := f.s.ClaimModel(ctx, "author-revoked-model-worker", 30*time.Second)
	if err != nil || l == nil {
		t.Fatal(err)
	}
	if _, err = f.s.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, f.p.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.s.PauseModelAfterRuntimeError(ctx, l, "model_attempt_requires_attention"); err != nil {
		t.Fatal("trusted cleanup incorrectly required revoked execution permission", err)
	}
	var status, reason string
	if err = f.s.Pool.QueryRow(ctx, "SELECT status,waiting_reason FROM tasks WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.task.ID).Scan(&status, &reason); err != nil || status != "blocked" || reason != "model_attempt_requires_attention" || f.modelCalls.Load() != 0 {
		t.Fatal("revoked model attempt remained running or dispatched", err)
	}
}

// Long-lived consent must not keep a proved-unsent reservation hostage after
// revocation. These tests use actual public commands, PostgreSQL and fresh
// expiry checks, rather than advancing the deadline to make cleanup eligible.
func TestAuthorInactiveAutomationGrantPromptlyCleansEveryUnsentPhase(t *testing.T) {
	for _, phase := range []string{"awaiting_approval", "approved", "queued", "leased"} {
		t.Run(phase, func(t *testing.T) {
			f := independentAutomationSetup(t, nil)
			ctx := context.Background()
			f.executeModel(t)
			state := f.grant(t, func(in *domain.CreateTaskAutomation) { in.Mode = "prepare_and_continue"; in.ExpiresInSeconds = 3600 })
			f.reconcile(t)
			values := f.executions(t)
			if len(values) != 1 {
				t.Fatal("expected one prepared native request")
			}
			v, err := f.s.GetToolExecution(ctx, f.p, f.task.ID, values[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if phase != "awaiting_approval" {
				raw, e := f.s.DecideToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDecision(v, "approve"))
				v = independentToolView(t, raw, e)
			}
			if phase == "queued" || phase == "leased" {
				raw, e := f.s.DispatchToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDispatch(v))
				v = independentToolView(t, raw, e)
			}
			if phase == "leased" {
				l, _, e := f.s.ClaimToolExecution(ctx, "author-revocation-before-send", 30*time.Second)
				if e != nil || l == nil {
					t.Fatal(e)
				}
				v, e = f.s.GetToolExecution(ctx, f.p, f.task.ID, l.Execution.ID)
				if e != nil {
					t.Fatal(e)
				}
			}
			if v.Execution.Status != phase || time.Until(v.Execution.ExpiresAt) < 30*time.Minute {
				t.Fatal("test did not stage a live long-lived unsent phase", v.Execution.Status)
			}
			if changed, e := f.s.ReconcileToolUnsent(ctx); e != nil || changed {
				t.Fatal("active consent was cancelled before revocation", e)
			}
			originalCandidate, originalRun := v.Task.CandidateID, v.Task.CurrentRunID
			if _, err = f.s.RevokeAutomation(ctx, f.p, f.task.ID, domain.NewID("key"), domain.RevokeTaskAutomation{GrantID: state.Grant.ID, ExpectedVersion: state.Grant.Version, Reason: "Explicit author revocation well before the deadline"}); err != nil {
				t.Fatal(err)
			}
			changed, err := f.s.ReconcileToolUnsent(ctx)
			if err != nil || !changed {
				t.Fatal("inactive exact consent did not promptly release proved-unsent work", err)
			}
			v, err = f.s.GetToolExecution(ctx, f.p, f.task.ID, v.Execution.ID)
			if err != nil {
				t.Fatal(err)
			}
			if v.Execution.Status != "cancelled" || v.Execution.Reason != "automation_grant_inactive" || v.Receipt != nil || v.Task.Status != "waiting_input" || v.Task.WaitingReason != "model_tools_pending" || v.Task.CandidateID != originalCandidate || v.Task.CurrentRunID != originalRun {
				t.Fatal("revocation cleanup lost the manual continuation source", v.Execution.Status, v.Execution.Reason)
			}
			after, err := f.s.Automation(ctx, f.p, f.task.ID)
			if err != nil || after.Grant.Status != "revoked" || after.Usage.ToolExecutions != 1 || after.Usage.ModelTurns != 0 || after.Usage.AllocatedMicroUSD != 120 {
				t.Fatal("cleanup rewrote consent or refunded lifetime allocations", after.Usage, err)
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 300, 0)
			var reservation, action string
			var marked bool
			if err = f.s.Pool.QueryRow(ctx, "SELECT r.status,a.status,a.dispatch_issued_at IS NOT NULL FROM reservations r JOIN actions a ON a.organization_id=r.organization_id AND a.id=r.action_id WHERE r.organization_id=$1 AND r.id=$2", f.p.OrganizationID, v.Execution.ReservationID).Scan(&reservation, &action, &marked); err != nil || reservation != "released" || action != "rejected" || marked {
				t.Fatal("release lacked durable no-dispatch proof", reservation, action, err)
			}
			var releases int
			if err = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM ledger_entries WHERE organization_id=$1 AND reservation_id=$2 AND kind='release'", f.p.OrganizationID, v.Execution.ReservationID).Scan(&releases); err != nil || releases != 1 {
				t.Fatal("release ledger must be exactly once", err)
			}
			if changed, err = f.s.ReconcileToolUnsent(ctx); err != nil || changed {
				t.Fatal("inactive cleanup repeated a terminal transition", err)
			}
			if f.readCalls.Load() != 0 || f.modelCalls.Load() != 1 {
				t.Fatal("revocation cleanup invoked an external provider")
			}
		})
	}
}
func TestAuthorInactiveGrantCleanupNeverReleasesDispatchedUncertainty(t *testing.T) {
	for _, actualRequest := range []bool{false, true} {
		name := "committed_permit"
		if actualRequest {
			name = "actual_unknown_response"
		}
		t.Run(name, func(t *testing.T) {
			f := independentAutomationSetup(t, func(f *independentAutomationFixture) { f.unknown = actualRequest })
			ctx := context.Background()
			f.executeModel(t)
			state := f.grant(t, nil)
			f.reconcile(t)
			values := f.executions(t)
			if len(values) != 1 || values[0].Status != "queued" {
				t.Fatal("expected exact queued automatic read")
			}
			var execution domain.ToolExecutionView
			if actualRequest {
				execution = f.executeRead(t)
			} else {
				l, _, err := f.s.ClaimToolExecution(ctx, "author-committed-permit-before-revoke", 30*time.Second)
				if err != nil || l == nil {
					t.Fatal(err)
				}
				if _, err = f.s.PrepareToolDispatch(ctx, l); err != nil {
					t.Fatal(err)
				}
				execution, err = f.s.GetToolExecution(ctx, f.p, f.task.ID, l.Execution.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.s.RevokeAutomation(ctx, f.p, f.task.ID, domain.NewID("key"), domain.RevokeTaskAutomation{GrantID: state.Grant.ID, ExpectedVersion: state.Grant.Version, Reason: "Revoke future work without erasing a committed send permit"}); err != nil {
				t.Fatal(err)
			}
			if changed, err := f.s.ReconcileToolUnsent(ctx); err != nil || changed {
				t.Fatal("inactive grant was treated as proof that a dispatched action never sent", err)
			}
			after, err := f.s.GetToolExecution(ctx, f.p, f.task.ID, execution.Execution.ID)
			if err != nil || after.Execution.Status != execution.Execution.Status || after.Receipt != nil {
				t.Fatal("cleanup changed sent/unknown evidence", err)
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
			usage, err := f.s.Automation(ctx, f.p, f.task.ID)
			if err != nil || usage.Usage.ToolExecutions != 1 || usage.Usage.AllocatedMicroUSD != 120 {
				t.Fatal("dispatched exposure reset lifetime allocation", err)
			}
			if f.modelCalls.Load() != 1 || f.readCalls.Load() != int64(map[bool]int{false: 0, true: 1}[actualRequest]) {
				t.Fatal("cleanup issued a new request")
			}
		})
	}
}
