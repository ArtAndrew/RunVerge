// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/toolruntime"
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

// Authority rows are locked before the structural gate. Every subsequent
// admission checks fresh state and DB wall time after all profile/ledger waits.
func lockToolAuthorityTx(ctx context.Context, tx pgx.Tx, e domain.ToolExecution) error {
	var ids []string
	if err := tx.QueryRow(ctx, `SELECT ARRAY(SELECT DISTINCT x FROM unnest(ARRAY[t.owner_id,j.created_by,a.decided_by,j.dispatch_requested_by]) x WHERE x IS NOT NULL ORDER BY x) FROM tool_executions j JOIN tasks t ON t.organization_id=j.organization_id AND t.id=j.task_id JOIN approvals a ON a.organization_id=j.organization_id AND a.id=j.approval_id WHERE j.organization_id=$1 AND j.id=$2`, e.OrganizationID, e.ID).Scan(&ids); err != nil {
		return rowExists(err)
	}
	ancestors, err := ancestorAuthorityIDsTx(ctx, tx, e.OrganizationID, e.TaskID)
	if err != nil {
		return err
	}
	return lockAuthorityPrincipalsTx(ctx, tx, e.OrganizationID, append(ids, ancestors...))
}
func lockToolRuntimeTx(ctx context.Context, tx pgx.Tx, e domain.ToolExecution, authority bool) (domain.ToolExecution, costScopes, error) {
	var scopes costScopes
	if authority {
		if err := lockToolAuthorityTx(ctx, tx, e); err != nil {
			return e, scopes, err
		}
	}
	if _, err := lockTaskTreeTx(ctx, tx, e.OrganizationID, e.TaskID); err != nil {
		return e, scopes, err
	}
	var err error
	scopes, err = lockCostScopes(ctx, tx, e.OrganizationID, e.TaskID)
	if err != nil {
		return e, scopes, err
	}
	e, err = loadToolExecutionTx(ctx, tx, e.OrganizationID, e.TaskID, e.ID, true)
	return e, scopes, err
}
func toolRuntimeAdmissionTx(ctx context.Context, tx pgx.Tx, e domain.ToolExecution, scopes costScopes) (domain.ToolConnection, error) {
	var empty domain.ToolConnection
	// Admission's local waiting_approval exception must not mutate a caller's
	// snapshot, which is reused for the final fresh admission after ledger waits.
	scopes.tasks = append([]domain.Task(nil), scopes.tasks...)
	t := scopes.tasks[len(scopes.tasks)-1]
	if err := toolCurrentTask(t, e); err != nil {
		return empty, err
	}
	if t.ModelConnectionID != "" {
		var active bool
		if err := tx.QueryRow(ctx, "SELECT active FROM model_connections WHERE organization_id=$1 AND id=$2 AND version=$3 FOR SHARE", t.OrganizationID, t.ModelConnectionID, t.ModelConnectionVersion).Scan(&active); err != nil {
			return empty, rowExists(err)
		}
		if !active {
			return empty, domain.Forbidden
		}
	}
	c, err := loadToolConnectionTx(ctx, tx, e.OrganizationID, e.ToolID, e.ToolVersion, true)
	if err != nil {
		return empty, err
	}
	if !c.Tool.Active || domain.ToolConnectionDigest(c) != e.ToolProfileDigest || toolruntime.ValidateConnection(c) != nil {
		return empty, domain.Forbidden
	}
	a, err := scanApproval(tx.QueryRow(ctx, "SELECT "+approvalColumns+" FROM approvals WHERE organization_id=$1 AND id=$2 FOR SHARE", e.OrganizationID, e.ApprovalID))
	if err != nil {
		return empty, err
	}
	if err = toolActorsAuthorizedTx(ctx, tx, e, c, t, a, true); err != nil {
		return empty, err
	}
	var role, kind string
	if err = tx.QueryRow(ctx, "SELECT m.role,p.kind FROM memberships m JOIN principals p ON p.id=m.principal_id WHERE m.organization_id=$1 AND m.principal_id=$2 AND m.active AND p.active", e.OrganizationID, e.DispatchedBy).Scan(&role, &kind); err != nil {
		return empty, domain.Forbidden
	}
	if !toolPermission(domain.Principal{ID: e.DispatchedBy, Role: role, Kind: kind, OrganizationID: e.OrganizationID}, t, "actions.dispatch_own", "actions.dispatch_any") {
		return empty, domain.Forbidden
	}
	var blocked, consumed bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE organization_id=$1 AND id=$2 AND run_id=$3 AND task_id=$4 AND status='blocked'),EXISTS(SELECT 1 FROM team_members WHERE organization_id=$1 AND child_task_id=$4 AND selected_candidate_id IS NOT NULL)`, e.OrganizationID, e.OperationID, e.RunID, e.TaskID).Scan(&blocked, &consumed); err != nil {
		return empty, err
	}
	if !blocked || consumed {
		return empty, domain.Err("conflict", "Исходный результат больше недоступен для действия", 409)
	}
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&scopes.now); err != nil {
		return empty, err
	}
	if !scopes.now.Before(e.ExpiresAt) || !scopes.now.Before(a.ExpiresAt) {
		return empty, domain.Err("execution_deadline", "Срок действия разрешения истёк", 409)
	}
	for i := range scopes.tasks {
		if scopes.tasks[i].ID == e.TaskID && scopes.tasks[i].Status == "waiting_approval" {
			scopes.tasks[i].Status = "waiting_input"
		}
	}
	if err = scopes.admit(0); err != nil {
		return empty, err
	}
	return c, nil
}
func checkToolLeaseTx(ctx context.Context, tx pgx.Tx, l *domain.ToolLease, allowSent bool) error {
	if l == nil || l.FencingToken < 1 {
		return domain.Err("stale_lease", "Нет права на выполнение инструмента", 409)
	}
	var valid bool
	err := tx.QueryRow(ctx, `SELECT lease_owner=$3 AND fencing_token=$4 AND lease_until>clock_timestamp() AND (status='leased' OR ($5 AND status='sent')) FROM tool_executions WHERE organization_id=$1 AND id=$2`, l.Execution.OrganizationID, l.Execution.ID, l.LeaseOwner, l.FencingToken, allowSent).Scan(&valid)
	if err != nil {
		return rowExists(err)
	}
	if !valid {
		return domain.Err("stale_lease", "Аренда инструмента истекла", 409)
	}
	return nil
}

func (s *Store) ClaimToolExecution(ctx context.Context, worker string, duration time.Duration) (*domain.ToolLease, domain.ToolConnection, error) {
	var connection domain.ToolConnection
	if err := s.CheckRuntimeAdmission(ctx); err != nil {
		return nil, connection, err
	}
	if !validText(worker, 100) || duration < time.Second || duration > 5*time.Minute {
		return nil, connection, domain.Err("invalid_request", "Некорректная аренда инструмента", 400)
	}
	e, err := scanToolExecution(s.Pool.QueryRow(ctx, "SELECT "+toolExecutionColumns+" FROM tool_executions WHERE (status='queued' OR (status='leased' AND lease_until<clock_timestamp())) AND NOT recovery_historical_task(organization_id,task_id) ORDER BY created_at,id LIMIT 1"))
	if errors.Is(err, domain.NotFound) {
		return nil, connection, nil
	}
	if err != nil {
		return nil, connection, err
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return nil, connection, err
	}
	defer tx.Rollback(ctx)
	e, scopes, err := lockToolRuntimeTx(ctx, tx, e, true)
	if err != nil {
		return nil, connection, err
	}
	var attempts int
	var available bool
	if err = tx.QueryRow(ctx, "SELECT attempts,status='queued' OR (status='leased' AND lease_until<clock_timestamp()) FROM tool_executions WHERE organization_id=$1 AND id=$2", e.OrganizationID, e.ID).Scan(&attempts, &available); err != nil {
		return nil, connection, err
	}
	if !available {
		return nil, connection, nil
	}
	connection, err = toolRuntimeAdmissionTx(ctx, tx, e, scopes)
	if err != nil || attempts >= 5 {
		reason := "tool_authorization_or_deadline"
		if attempts >= 5 {
			reason = "tool_claim_limit"
		}
		if stopErr := stopToolUnsentTx(ctx, tx, e, reason); stopErr != nil {
			return nil, connection, stopErr
		}
		return nil, connection, tx.Commit(ctx)
	}
	l := &domain.ToolLease{LeaseOwner: worker}
	err = tx.QueryRow(ctx, "UPDATE tool_executions SET status='leased',version=version+1,attempts=attempts+1,lease_owner=$3,lease_until=LEAST(expires_at,clock_timestamp()+$4::interval),fencing_token=fencing_token+1 WHERE organization_id=$1 AND id=$2 AND expires_at>clock_timestamp() RETURNING fencing_token,lease_until", e.OrganizationID, e.ID, worker, duration.String()).Scan(&l.FencingToken, &l.LeaseUntil)
	if err != nil {
		return nil, connection, rowExists(err)
	}
	l.Execution, err = loadToolExecutionTx(ctx, tx, e.OrganizationID, e.TaskID, e.ID, false)
	if err != nil {
		return nil, connection, err
	}
	if err = emit(ctx, tx, e.OrganizationID, e.TaskID, "tool.claimed", map[string]any{"execution_id": e.ID, "fencing_token": l.FencingToken, "attempt": attempts + 1}); err != nil {
		return nil, connection, err
	}
	return l, connection, tx.Commit(ctx)
}
func (s *Store) RenewToolLease(ctx context.Context, l *domain.ToolLease, duration time.Duration) error {
	if l == nil || duration < time.Second || duration > 5*time.Minute {
		return domain.Forbidden
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	e, scopes, err := lockToolRuntimeTx(ctx, tx, l.Execution, true)
	if err != nil {
		return err
	}
	if _, err = toolRuntimeAdmissionTx(ctx, tx, e, scopes); err != nil {
		return err
	}
	if err = checkToolLeaseTx(ctx, tx, l, true); err != nil {
		return err
	}
	// A sent job keeps its original dispatch permit. Renewal only bounds waiting
	// for a response; it cannot issue another request or extend send admission.
	if e.Status == "sent" {
		return tx.Commit(ctx)
	}
	_, err = tx.Exec(ctx, "UPDATE tool_executions SET version=version+1,lease_until=LEAST(expires_at,clock_timestamp()+$3::interval) WHERE organization_id=$1 AND id=$2", e.OrganizationID, e.ID, duration.String())
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) PrepareToolDispatch(ctx context.Context, l *domain.ToolLease) (domain.ToolDispatchPermit, error) {
	var empty domain.ToolDispatchPermit
	if l == nil {
		return empty, domain.Forbidden
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	e, scopes, err := lockToolRuntimeTx(ctx, tx, l.Execution, true)
	if err != nil {
		return empty, err
	}
	if e.IntentHash != l.Execution.IntentHash || e.Status != "leased" {
		return empty, domain.Forbidden
	}
	if _, err = toolRuntimeAdmissionTx(ctx, tx, e, scopes); err != nil {
		return empty, err
	}
	r, err := scanReservation(tx.QueryRow(ctx, "SELECT "+reservationColumns+" FROM reservations WHERE organization_id=$1 AND id=$2 FOR UPDATE", e.OrganizationID, e.ReservationID))
	if err != nil {
		return empty, err
	}
	var actionStatus, intent string
	var dispatched bool
	if err = tx.QueryRow(ctx, "SELECT status,intent_hash,dispatch_issued_at IS NOT NULL FROM actions WHERE organization_id=$1 AND id=$2 FOR UPDATE", e.OrganizationID, e.ActionID).Scan(&actionStatus, &intent, &dispatched); err != nil {
		return empty, err
	}
	if actionStatus != "pending" || dispatched || intent != e.IntentHash || r.ActionID != e.ActionID || r.OperationID != e.OperationID || r.TaskID != e.TaskID || r.Status != "reserved" || r.PendingQuarantineID != "" || r.Category != "tool" || r.AmountMicroUSD != e.MaxChargeMicroUSD || r.Provider != e.Provider || r.PriceVersion != e.PriceVersion {
		return empty, domain.Err("conflict", "Внешнее действие уже отправлено или остановлено", 409)
	}
	// Fresh checks after all lock waits, including ledger and profile locks.
	if _, err = toolRuntimeAdmissionTx(ctx, tx, e, scopes); err != nil {
		return empty, err
	}
	if err = checkToolLeaseTx(ctx, tx, l, false); err != nil {
		return empty, err
	}
	permit := domain.ToolDispatchPermit{ExecutionID: e.ID, ActionID: e.ActionID, ReservationID: e.ReservationID, IntentHash: e.IntentHash, FencingToken: l.FencingToken}
	treeDeadline := e.ExpiresAt
	for _, task := range scopes.tasks {
		if !task.ExecutionDeadline.IsZero() && task.ExecutionDeadline.Before(treeDeadline) {
			treeDeadline = task.ExecutionDeadline
		}
	}
	err = tx.QueryRow(ctx, `WITH current AS MATERIALIZED (SELECT clock_timestamp() AS stamp,lease_until,expires_at FROM tool_executions WHERE organization_id=$1 AND id=$2 AND status='leased' AND lease_owner=$3 AND fencing_token=$4 AND lease_until>clock_timestamp() AND expires_at>clock_timestamp() AND $5::timestamptz>clock_timestamp()) UPDATE tool_executions j SET status='sent',version=version+1,dispatch_issued_at=c.stamp,dispatch_not_after=LEAST(c.lease_until,c.expires_at,$5::timestamptz,c.stamp+interval '2 seconds'),dispatch_fencing_token=$4 FROM current c WHERE j.organization_id=$1 AND j.id=$2 RETURNING j.dispatch_issued_at,j.dispatch_not_after`, e.OrganizationID, e.ID, l.LeaseOwner, l.FencingToken, treeDeadline).Scan(&permit.IssuedAt, &permit.NotAfter)
	if err != nil {
		return empty, rowExists(err)
	}
	permit.MaxAge = permit.NotAfter.Sub(permit.IssuedAt)
	if permit.MaxAge <= 0 || permit.MaxAge > 2*time.Second {
		return empty, domain.Forbidden
	}
	if _, err = tx.Exec(ctx, "UPDATE actions SET status='sent',dispatch_issued_at=$3,dispatch_not_after=$4,dispatch_fencing_token=$5 WHERE organization_id=$1 AND id=$2 AND status='pending'", e.OrganizationID, e.ActionID, permit.IssuedAt, permit.NotAfter, permit.FencingToken); err != nil {
		return empty, err
	}
	if err = recordLedger(ctx, tx, r, "dispatch", digestValue(e.IntentHash), "dispatch", 0, 0); err != nil {
		return empty, err
	}
	if err = emit(ctx, tx, e.OrganizationID, e.TaskID, "tool.dispatched", map[string]any{"execution_id": e.ID, "action_id": e.ActionID, "intent_hash": e.IntentHash, "dispatch_not_after": permit.NotAfter, "fencing_token": permit.FencingToken}); err != nil {
		return empty, err
	}
	return permit, tx.Commit(ctx)
}

func stopToolUnsentTx(ctx context.Context, tx pgx.Tx, e domain.ToolExecution, reason string) error {

	// The job and the structural gate are locked. A revoked grant, elapsed TTL,
	// financial Action status or client error cannot substitute for the durable
	// no-dispatch proof on BOTH records before cancellation or reserve release.
	switch e.Status {
	case "awaiting_approval", "approved", "queued", "leased":
	default:
		return domain.Err("conflict", "Tool action is no longer a proven unsent job", 409)
	}
	var unsent bool
	if err := tx.QueryRow(ctx, `SELECT j.dispatch_issued_at IS NULL AND a.dispatch_issued_at IS NULL FROM tool_executions j JOIN actions a ON a.organization_id=j.organization_id AND a.id=j.action_id WHERE j.organization_id=$1 AND j.id=$2 AND j.intent_hash=$3`, e.OrganizationID, e.ID, e.IntentHash).Scan(&unsent); err != nil {
		return rowExists(err)
	}
	if !unsent {
		return domain.Err("conflict", "Committed tool dispatch requires outcome reconciliation", 409)
	}
	status := "expired"
	if e.Status == "queued" || e.Status == "leased" {
		status = "rejected"
		if e.ModelProposalID != "" {
			// Kernel cancellation before a dispatch marker is not an external
			// tool rejection and has no effect receipt. Keep that distinction so
			// a model can explicitly continue from a known unsent cancellation.
			status = "cancelled"
		}
	}
	if reason == "automation_grant_inactive" || reason == "authority_epoch_changed" {
		status = "cancelled"
	}
	if _, err := tx.Exec(ctx, "UPDATE tool_executions SET status=$3,reason=$4,version=version+1,lease_owner=NULL,lease_until=NULL WHERE organization_id=$1 AND id=$2", e.OrganizationID, e.ID, status, reason); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE approvals SET status='expired',version=version+1,reason=$3 WHERE organization_id=$1 AND id=$2 AND status='pending'", e.OrganizationID, e.ApprovalID, reason); err != nil {
		return err
	}
	if err := releaseToolUnsentTx(ctx, tx, e, "tool.runtime.unsent"); err != nil {
		return err
	}
	if err := restoreToolTaskTx(ctx, tx, e); err != nil {
		return err
	}
	return emit(ctx, tx, e.OrganizationID, e.TaskID, "tool.stopped_unsent", map[string]any{"execution_id": e.ID, "reason": reason, "external_call": false})
}
func (s *Store) PauseToolExecution(ctx context.Context, l *domain.ToolLease, reason string) error {
	if l == nil || l.FencingToken < 1 || !validText(reason, 256) {
		return domain.Forbidden
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	e, _, err := lockToolRuntimeTx(ctx, tx, l.Execution, false)
	if err != nil {
		return err
	}
	if e.IntentHash != l.Execution.IntentHash {
		return domain.Forbidden
	}
	var fence, dispatchFence int64
	var owner string
	var marked, actionMarked bool
	if err = tx.QueryRow(ctx, `SELECT j.fencing_token,COALESCE(j.lease_owner,''),COALESCE(j.dispatch_fencing_token,0),j.dispatch_issued_at IS NOT NULL,a.dispatch_issued_at IS NOT NULL FROM tool_executions j JOIN actions a ON a.organization_id=j.organization_id AND a.id=j.action_id WHERE j.organization_id=$1 AND j.id=$2`, e.OrganizationID, e.ID).Scan(&fence, &owner, &dispatchFence, &marked, &actionMarked); err != nil {
		return err
	}
	if fence != l.FencingToken {
		return domain.Err("stale_lease", "Tool attempt fencing token changed", 409)
	}
	// A lost dispatch acknowledgement is NOT proof that no application write was
	// authorized. Inspect the durable marker; preserve sent uncertainty and money.
	if marked || actionMarked {
		if dispatchFence != l.FencingToken {
			return domain.Forbidden
		}
		if e.Status == "succeeded" || e.Status == "rejected" {
			return tx.Commit(ctx)
		}
		if e.Status != "sent" && e.Status != "unknown" {
			return domain.Err("conflict", "Tool dispatch state requires reconciliation", 409)
		}
		if err = classifyToolUnknownTx(ctx, tx, e); err != nil {
			return err
		}
		if e.Status == "sent" {
			if _, err = tx.Exec(ctx, "UPDATE tool_executions SET status='unknown',version=version+1,reason='external_action_unknown',lease_owner=NULL,lease_until=NULL WHERE organization_id=$1 AND id=$2", e.OrganizationID, e.ID); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, "UPDATE tasks SET status='blocked',waiting_reason='external_action_unknown',version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2 AND current_run_id=$3 AND candidate_id=$4 AND status='waiting_approval'", e.OrganizationID, e.TaskID, e.RunID, e.CandidateID); err != nil {
				return err
			}
			if err = emit(ctx, tx, e.OrganizationID, e.TaskID, "tool.outcome_unknown", map[string]any{"execution_id": e.ID, "resend_allowed": false, "reason": "dispatch_acknowledgement_uncertain", "financial_settlement": false}); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}
	// This exact worker attempt can stop even after lease expiry, but only while
	// no new fence exists and the original job still has proven zero dispatch.
	if e.Status != "leased" || owner != l.LeaseOwner {
		return domain.Err("stale_lease", "Tool attempt is no longer leased by this worker", 409)
	}
	if err = stopToolUnsentTx(ctx, tx, e, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) RecordToolOutcome(ctx context.Context, in domain.ToolOutcomeEvidence) (domain.ToolOutcomeReceipt, error) {
	var empty domain.ToolOutcomeReceipt
	if !validText(in.OrganizationID, 128) || !validText(in.ExecutionID, 128) || !validDigest(in.IntentHash) || in.FencingToken < 1 || (in.Outcome != "unknown" && in.Outcome != "succeeded" && in.Outcome != "rejected") {
		return empty, domain.Err("invalid_request", "Некорректное наблюдение инструмента", 400)
	}
	if in.Outcome != "unknown" && (!optionalText(in.Result, 262144) || !validText(in.EffectReference, 256) || !validDigest(in.EvidenceDigest)) {
		return empty, domain.Err("invalid_request", "Некорректная квитанция инструмента", 400)
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	if err := admitRecoveryRowTx(ctx, tx, "tool_executions", in.OrganizationID, in.ExecutionID); err != nil {
		return empty, err
	}
	e, err := loadToolExecutionTx(ctx, tx, in.OrganizationID, "", in.ExecutionID, false)
	if err != nil {
		return empty, err
	}
	e, _, err = lockToolRuntimeTx(ctx, tx, e, false)
	if err != nil {
		return empty, err
	}
	var dispatchedFence int64
	if err = tx.QueryRow(ctx, "SELECT COALESCE(dispatch_fencing_token,0) FROM tool_executions WHERE organization_id=$1 AND id=$2", e.OrganizationID, e.ID).Scan(&dispatchedFence); err != nil {
		return empty, err
	}
	if e.IntentHash != in.IntentHash || dispatchedFence != in.FencingToken {
		return empty, domain.Forbidden
	}
	c, err := loadToolConnectionTx(ctx, tx, e.OrganizationID, e.ToolID, e.ToolVersion, false)
	if err != nil {
		return empty, err
	}
	if len(in.Result) > c.Tool.MaxOutputBytes {
		return empty, domain.Err("invalid_request", "Ответ инструмента превышает лимит", 400)
	}
	if e.ResultReceiptID != "" {
		r, err := scanToolReceipt(tx.QueryRow(ctx, "SELECT "+toolReceiptColumns+" FROM tool_outcome_receipts WHERE organization_id=$1 AND id=$2", e.OrganizationID, e.ResultReceiptID))
		if err != nil {
			return empty, err
		}
		if in.Outcome == "unknown" {
			return r, tx.Commit(ctx)
		}
		if r.Outcome != in.Outcome || r.EffectReference != in.EffectReference || r.EvidenceDigest != in.EvidenceDigest || r.ContentDigest != Hash([]byte(in.Result)) {
			if err = recordToolConflictTx(ctx, tx, e, r, in); err != nil {
				return empty, err
			}
			if err = tx.Commit(ctx); err != nil {
				return empty, err
			}
			return empty, domain.Err("conflict", "Другая квитанция уже записана", 409)
		}
		return r, tx.Commit(ctx)
	}
	if e.Status != "sent" && e.Status != "unknown" {
		return empty, domain.Err("conflict", "Намерение не отправлялось", 409)
	}
	if err = classifyToolUnknownTx(ctx, tx, e); err != nil {
		return empty, err
	}
	if in.Outcome == "unknown" {
		if e.Status == "sent" {
			if _, err = tx.Exec(ctx, "UPDATE tool_executions SET status='unknown',version=version+1,reason='external_action_unknown',lease_owner=NULL,lease_until=NULL WHERE organization_id=$1 AND id=$2", e.OrganizationID, e.ID); err != nil {
				return empty, err
			}
		}
		if _, err = tx.Exec(ctx, "UPDATE tasks SET status='blocked',waiting_reason='external_action_unknown',version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2 AND current_run_id=$3 AND candidate_id=$4 AND status='waiting_approval'", e.OrganizationID, e.TaskID, e.RunID, e.CandidateID); err != nil {
			return empty, err
		}
		if err = emit(ctx, tx, e.OrganizationID, e.TaskID, "tool.outcome_unknown", map[string]any{"execution_id": e.ID, "resend_allowed": false, "financial_settlement": false}); err != nil {
			return empty, err
		}
		return empty, tx.Commit(ctx)
	}
	receipt, err := recordToolReceiptTx(ctx, tx, e, in.Outcome, in.Result, in.EffectReference, in.EvidenceDigest, "registered_tool_response", "", "")
	if err != nil {
		return empty, err
	}
	if err = emit(ctx, tx, e.OrganizationID, e.TaskID, "tool.effect_observed", map[string]any{"execution_id": e.ID, "receipt_id": receipt.ID, "outcome": in.Outcome, "method": "registered_tool_response", "financial_settlement": false}); err != nil {
		return empty, err
	}
	return receipt, tx.Commit(ctx)
}
func classifyToolUnknownTx(ctx context.Context, tx pgx.Tx, e domain.ToolExecution) error {
	r, err := scanReservation(tx.QueryRow(ctx, "SELECT "+reservationColumns+" FROM reservations WHERE organization_id=$1 AND id=$2 FOR UPDATE", e.OrganizationID, e.ReservationID))
	if err != nil {
		return err
	}
	if r.Status == "reserved" {
		if _, err = tx.Exec(ctx, "UPDATE reservations SET status='unknown',revision=revision+1 WHERE organization_id=$1 AND id=$2", e.OrganizationID, e.ReservationID); err != nil {
			return err
		}
		if err = recordLedger(ctx, tx, r, "tool.effect.unknown", digestValue(e.IntentHash), "unknown", 0, 0); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, "UPDATE actions SET status='unknown' WHERE organization_id=$1 AND id=$2 AND status='sent'", e.OrganizationID, e.ActionID)
	return err
}

// Terminal delegation cleanup is separate from execution authorization. An
// exact old grant cannot keep an unsent hold alive or borrow a replacement.
const inactiveToolAutomationGrant = `automation_grant_id IS NOT NULL AND EXISTS(SELECT 1 FROM task_automation_grants g WHERE g.organization_id=tool_executions.organization_id AND g.id=tool_executions.automation_grant_id AND g.task_id=tool_executions.task_id AND g.status<>'active')`
const inactiveToolAuthorityEpoch = `NOT EXISTS(SELECT 1 FROM tasks t
 JOIN memberships own ON own.organization_id=t.organization_id AND own.principal_id=t.owner_id
 JOIN memberships creator ON creator.organization_id=t.organization_id AND creator.principal_id=tool_executions.created_by
 JOIN approvals a ON a.organization_id=t.organization_id AND a.id=tool_executions.approval_id
 LEFT JOIN memberships decider ON decider.organization_id=t.organization_id AND decider.principal_id=a.decided_by
 LEFT JOIN memberships dispatcher ON dispatcher.organization_id=t.organization_id AND dispatcher.principal_id=tool_executions.dispatch_requested_by
 WHERE t.organization_id=tool_executions.organization_id AND t.id=tool_executions.task_id
 AND tool_executions.owner_authority_epoch=own.security_epoch AND tool_executions.creator_authority_epoch=creator.security_epoch
 AND (a.decided_by IS NULL OR a.decider_authority_epoch=decider.security_epoch)
 AND (tool_executions.dispatch_requested_by IS NULL OR tool_executions.dispatcher_authority_epoch=dispatcher.security_epoch)
 AND tool_executions.owner_policy_epoch=current_principal_policy_epoch(t.organization_id,t.owner_id) AND tool_executions.creator_policy_epoch=current_principal_policy_epoch(t.organization_id,tool_executions.created_by)
 AND (a.decided_by IS NULL OR a.decider_policy_epoch=current_principal_policy_epoch(t.organization_id,a.decided_by))
 AND (tool_executions.dispatch_requested_by IS NULL OR tool_executions.dispatcher_policy_epoch=current_principal_policy_epoch(t.organization_id,tool_executions.dispatch_requested_by)))`

// An older managed child may have a different local initiator. Parent Run
// consent is still required and cannot be revived by restoring an admin role.
const inactiveToolAncestorAuthority = `EXISTS(WITH RECURSIVE authority_ancestors AS (
 SELECT p.id,p.parent_id,p.current_run_id,p.owner_id,1 AS level FROM tasks t JOIN tasks p ON p.organization_id=t.organization_id AND p.id=t.parent_id
 WHERE t.organization_id=tool_executions.organization_id AND t.id=tool_executions.task_id
 UNION ALL SELECT p.id,p.parent_id,p.current_run_id,p.owner_id,a.level+1 FROM tasks p JOIN authority_ancestors a ON p.id=a.parent_id WHERE p.organization_id=tool_executions.organization_id AND a.level<16
 ) SELECT 1 FROM authority_ancestors ancestor
 LEFT JOIN runs r ON r.organization_id=tool_executions.organization_id AND r.id=ancestor.current_run_id AND r.task_id=ancestor.id
 LEFT JOIN memberships own ON own.organization_id=tool_executions.organization_id AND own.principal_id=ancestor.owner_id
 LEFT JOIN memberships actor ON actor.organization_id=tool_executions.organization_id AND actor.principal_id=r.principal_id
 WHERE r.id IS NULL OR r.owner_authority_epoch IS DISTINCT FROM own.security_epoch OR r.principal_authority_epoch IS DISTINCT FROM actor.security_epoch
 OR r.owner_policy_epoch IS DISTINCT FROM current_principal_policy_epoch(tool_executions.organization_id,ancestor.owner_id) OR r.principal_policy_epoch IS DISTINCT FROM current_principal_policy_epoch(tool_executions.organization_id,r.principal_id)
 OR (ancestor.level=16 AND ancestor.parent_id IS NOT NULL))`
const inactiveToolConsent = `(` + inactiveToolAuthorityEpoch + `) OR (` + inactiveToolAncestorAuthority + `)`
const toolUnsentReconcileDue = `(status IN('awaiting_approval','approved','queued','leased') AND (expires_at<=clock_timestamp() OR (` + inactiveToolAutomationGrant + `) OR (` + inactiveToolConsent + `))) OR (status='sent' AND lease_until<=clock_timestamp())`

func (s *Store) ReconcileToolUnsent(ctx context.Context) (bool, error) {
	if err := s.CheckRuntimeAdmission(ctx); err != nil {
		return false, err
	}
	e, err := scanToolExecution(s.Pool.QueryRow(ctx, "SELECT "+toolExecutionColumns+" FROM tool_executions WHERE ("+toolUnsentReconcileDue+") AND NOT recovery_historical_task(organization_id,task_id) ORDER BY created_at,id LIMIT 1"))
	if errors.Is(err, domain.NotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	e, _, err = lockToolRuntimeTx(ctx, tx, e, false)
	if err != nil {
		return false, err
	}
	var due, inactiveGrant, inactiveAuthority bool
	if err = tx.QueryRow(ctx, "SELECT ("+toolUnsentReconcileDue+"),("+inactiveToolAutomationGrant+"),("+inactiveToolConsent+") FROM tool_executions WHERE organization_id=$1 AND id=$2", e.OrganizationID, e.ID).Scan(&due, &inactiveGrant, &inactiveAuthority); err != nil {
		return false, err
	}
	if !due {
		return false, nil
	}
	if e.Status == "sent" {
		if err = classifyToolUnknownTx(ctx, tx, e); err != nil {
			return false, err
		}
		if _, err = tx.Exec(ctx, "UPDATE tool_executions SET status='unknown',version=version+1,reason='external_action_unknown',lease_owner=NULL,lease_until=NULL WHERE organization_id=$1 AND id=$2", e.OrganizationID, e.ID); err != nil {
			return false, err
		}
		if _, err = tx.Exec(ctx, "UPDATE tasks SET status='blocked',waiting_reason='external_action_unknown',version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2 AND status='waiting_approval' AND current_run_id=$3 AND candidate_id=$4", e.OrganizationID, e.TaskID, e.RunID, e.CandidateID); err != nil {
			return false, err
		}
		if err = emit(ctx, tx, e.OrganizationID, e.TaskID, "tool.outcome_unknown", map[string]any{"execution_id": e.ID, "resend_allowed": false, "reason": "worker_lease_expired"}); err != nil {
			return false, err
		}
	} else {
		reason := "tool_expired"
		if inactiveAuthority {
			reason = "authority_epoch_changed"
		}
		if inactiveGrant {
			reason = "automation_grant_inactive"
		}
		if err = stopToolUnsentTx(ctx, tx, e, reason); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}
