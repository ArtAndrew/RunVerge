// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

const toolExecutionColumns = "intent,status,version,reason,COALESCE(result_receipt_id,''),created_at,expires_at,COALESCE(dispatch_requested_by,'')"
const approvalColumns = "id,organization_id,task_id,action_id,intent_hash,status,version,COALESCE(decided_by,''),created_at,expires_at,COALESCE(execution_id,''),decided_at,COALESCE(decision_method,''),reason"
const toolReceiptColumns = "id,organization_id,execution_id,intent_hash,outcome,method,effect_reference,artifact_id,content_digest,evidence_digest,COALESCE(principal_id,''),reason,created_at"

func scanToolExecution(row scanner) (v domain.ToolExecution, e error) {
	var raw []byte
	var status, reason, receipt, dispatchBy string
	var version int
	var created, expires time.Time
	if e = row.Scan(&raw, &status, &version, &reason, &receipt, &created, &expires, &dispatchBy); e != nil {
		return v, rowExists(e)
	}
	if e = json.Unmarshal(raw, &v); e != nil {
		return v, e
	}
	v.Status, v.Version, v.Reason, v.ResultReceiptID = status, version, reason, receipt
	v.DispatchedBy = dispatchBy
	v.CreatedAt, v.ExpiresAt = created.UTC(), expires.UTC()
	if domain.ToolIntentDigest(v) != v.IntentHash || Hash([]byte(v.ArgumentsJSON)) != v.ArgumentsDigest {
		return v, domain.Err("conflict", "Сохранённое намерение действия повреждено", 409)
	}
	return v, nil
}
func loadToolExecutionTx(ctx context.Context, tx pgx.Tx, org, taskID, id string, lock bool) (domain.ToolExecution, error) {
	query := "SELECT " + toolExecutionColumns + " FROM tool_executions WHERE organization_id=$1 AND id=$2 AND ($3='' OR task_id=$3)"
	if lock {
		query += " FOR UPDATE"
	}
	return scanToolExecution(tx.QueryRow(ctx, query, org, id, taskID))
}
func scanApproval(row scanner) (v domain.Approval, e error) {
	e = row.Scan(&v.ID, &v.OrganizationID, &v.TaskID, &v.ActionID, &v.IntentHash, &v.Status, &v.Version, &v.DecidedBy, &v.CreatedAt, &v.ExpiresAt, &v.ExecutionID, &v.DecidedAt, &v.DecisionMethod, &v.Reason)
	return v, rowExists(e)
}
func scanToolReceipt(row scanner) (v domain.ToolOutcomeReceipt, e error) {
	e = row.Scan(&v.ID, &v.OrganizationID, &v.ExecutionID, &v.IntentHash, &v.Outcome, &v.Method, &v.EffectReference, &v.ArtifactID, &v.ContentDigest, &v.EvidenceDigest, &v.PrincipalID, &v.Reason, &v.CreatedAt)
	return v, rowExists(e)
}
func toolPermission(p domain.Principal, t domain.Task, own, any string) bool {
	return domain.PrincipalHasPermission(p, any) || (p.ID == t.OwnerID && domain.PrincipalHasPermission(p, own))
}
func toolViewTx(ctx context.Context, tx pgx.Tx, p domain.Principal, e domain.ToolExecution) (domain.ToolExecutionView, error) {
	v := domain.ToolExecutionView{Execution: e, AvailableCommands: []string{}}
	var id, org, role, kind string
	if err := tx.QueryRow(ctx, authQuery(p), p.TokenHash, p.OrganizationID).Scan(&id, &org, &role, &kind); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return v, domain.Unauthenticated
		}
		return v, err
	}
	if id != p.ID || org != e.OrganizationID {
		return v, domain.Unauthenticated
	}
	p.Role, p.Kind = role, kind
	var err error
	v.Task, err = scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND id=$2", org, e.TaskID))
	if err != nil {
		return v, err
	}
	c, err := loadToolConnectionTx(ctx, tx, org, e.ToolID, e.ToolVersion, false)
	if err != nil {
		return v, err
	}
	v.Tool = c.Tool
	v.Approval, err = scanApproval(tx.QueryRow(ctx, "SELECT "+approvalColumns+" FROM approvals WHERE organization_id=$1 AND id=$2 AND execution_id=$3", org, e.ApprovalID, e.ID))
	if err != nil {
		return v, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return v, err
	}
	current := v.Task.CandidateID == e.CandidateID && v.Task.CurrentRunID == e.RunID && v.Task.Status == "waiting_approval"
	if current && now.Before(e.ExpiresAt) {
		if e.Status == "awaiting_approval" && v.Approval.Status == "pending" && domain.PrincipalHasPermission(domain.Principal{ID: id, OrganizationID: org, Role: role, Kind: kind}, "actions.approve_any") {
			v.AvailableCommands = append(v.AvailableCommands, "deny")
			if c.Tool.Active && domain.ToolConnectionDigest(c) == e.ToolProfileDigest && (!c.Tool.RequireDistinctApprover || e.CreatedBy != p.ID) {
				if authority := toolActorsAuthorizedTx(ctx, tx, e, c, v.Task, v.Approval, false); authority == nil {
					v.AvailableCommands = append(v.AvailableCommands, "approve")
				} else if !errors.Is(authority, domain.Forbidden) {
					return v, authority
				}
			}
		}
		if e.Status == "approved" && v.Approval.Status == "approved" && c.Tool.Active && toolPermission(p, v.Task, "actions.dispatch_own", "actions.dispatch_any") {
			if authority := toolActorsAuthorizedTx(ctx, tx, e, c, v.Task, v.Approval, true); authority == nil {
				v.AvailableCommands = append(v.AvailableCommands, "dispatch")
			} else if !errors.Is(authority, domain.Forbidden) {
				return v, authority
			}
		}
	}
	if e.Status == "unknown" && domain.PrincipalHasPermission(domain.Principal{ID: id, OrganizationID: org, Role: role, Kind: kind}, "actions.reconcile_any") {
		v.AvailableCommands = append(v.AvailableCommands, "reconcile")
	}
	if e.ResultReceiptID != "" {
		r, err := scanToolReceipt(tx.QueryRow(ctx, "SELECT "+toolReceiptColumns+" FROM tool_outcome_receipts WHERE organization_id=$1 AND id=$2 AND execution_id=$3", org, e.ResultReceiptID, e.ID))
		if err != nil {
			return v, err
		}
		v.Receipt = &r
	}
	return v, nil
}

func (s *Store) GetToolExecution(ctx context.Context, p domain.Principal, taskID, id string) (domain.ToolExecutionView, error) {
	var empty domain.ToolExecutionView
	tx, err := s.beginMaterialRead(ctx, p, taskID)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	e, err := loadToolExecutionTx(ctx, tx, p.OrganizationID, taskID, id, false)
	if err != nil {
		return empty, err
	}
	v, err := toolViewTx(ctx, tx, p, e)
	if err != nil {
		return v, err
	}
	return v, tx.Commit(ctx)
}
func (s *Store) ToolExecutions(ctx context.Context, p domain.Principal, taskID, raw string, limit int) ([]domain.ToolExecution, string, error) {
	if limit < 1 || limit > 100 {
		return nil, "", domain.Err("invalid_request", "limit должен быть от 1 до 100", 400)
	}
	c, err := decodeCursor(raw)
	if err != nil {
		return nil, "", err
	}
	tx, err := s.beginMaterialRead(ctx, p, taskID)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "SELECT "+toolExecutionColumns+" FROM tool_executions WHERE organization_id=$1 AND task_id=$2 AND (created_at,id)<($3,$4) ORDER BY created_at DESC,id DESC LIMIT $5", p.OrganizationID, taskID, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	values := []domain.ToolExecution{}
	next := ""
	for rows.Next() {
		v, err := scanToolExecution(rows)
		if err != nil {
			rows.Close()
			return nil, "", err
		}
		v.ArgumentsJSON = ""
		values = append(values, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	if len(values) > limit {
		values = values[:limit]
		last := values[limit-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return values, next, tx.Commit(ctx)
}

// Restoring the existing candidate never accepts it or starts another Run.
func restoreToolTaskTx(ctx context.Context, tx pgx.Tx, e domain.ToolExecution) error {
	_, err := tx.Exec(ctx, `UPDATE tasks t SET status='waiting_input',waiting_reason=CASE c.source WHEN 'model' THEN 'model_candidate_pending' WHEN 'model_tools' THEN 'model_tools_pending' WHEN 'team' THEN 'team_candidate_pending' ELSE 'material_candidate_pending' END,version=t.version+1,updated_at=clock_timestamp()
 FROM task_candidates c WHERE t.organization_id=$1 AND t.id=$2 AND t.current_run_id=$3 AND t.candidate_id=$4 AND t.status IN('waiting_approval','blocked') AND (t.status<>'blocked' OR t.waiting_reason='external_action_unknown')
 AND c.organization_id=t.organization_id AND c.id=t.candidate_id AND NOT EXISTS(SELECT 1 FROM tool_executions x WHERE x.organization_id=t.organization_id AND x.task_id=t.id AND x.status IN('awaiting_approval','approved','queued','leased','sent','unknown'))`, e.OrganizationID, e.TaskID, e.RunID, e.CandidateID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE runs r SET status='waiting_input' FROM tasks t WHERE r.organization_id=$1 AND r.id=$2 AND r.status='waiting_approval' AND t.organization_id=r.organization_id AND t.id=$3 AND t.current_run_id=r.id AND t.status='waiting_input'", e.OrganizationID, e.RunID, e.TaskID)
	return err
}

// Caller holds the tree, operations, organization/tasks and job. A financial
// Action changed by billing is not treated as proof of non-dispatch.
func releaseToolUnsentTx(ctx context.Context, tx pgx.Tx, e domain.ToolExecution, eventKey string) error {
	scopes, err := lockCostScopes(ctx, tx, e.OrganizationID, e.TaskID)
	if err != nil {
		return err
	}
	r, err := scanReservation(tx.QueryRow(ctx, "SELECT "+reservationColumns+" FROM reservations WHERE organization_id=$1 AND id=$2 FOR UPDATE", e.OrganizationID, e.ReservationID))
	if err != nil {
		return err
	}
	var status string
	var undispatched bool
	if err = tx.QueryRow(ctx, "SELECT status,dispatch_issued_at IS NULL FROM actions WHERE organization_id=$1 AND id=$2 FOR UPDATE", e.OrganizationID, e.ActionID).Scan(&status, &undispatched); err != nil {
		return err
	}
	if r.Status == "released" {
		return nil
	}
	if status != "pending" || !undispatched || (r.Status != "reserved" && r.Status != "unknown") || r.PendingQuarantineID != "" {
		return nil
	}
	if _, err = tx.Exec(ctx, "UPDATE actions SET status='rejected' WHERE organization_id=$1 AND id=$2 AND status='pending'", e.OrganizationID, e.ActionID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE reservations SET status='released',revision=revision+1 WHERE organization_id=$1 AND id=$2", e.OrganizationID, r.ID); err != nil {
		return err
	}
	if err = applyCostDelta(ctx, tx, &scopes, -r.AmountMicroUSD, 0, false); err != nil {
		return err
	}
	if err = recordLedger(ctx, tx, r, eventKey, digestValue(e.IntentHash), "release", -r.AmountMicroUSD, 0); err != nil {
		return err
	}
	return emit(ctx, tx, e.OrganizationID, e.TaskID, "tool.cost_released", map[string]any{"execution_id": e.ID, "reservation_id": r.ID, "proof": "pending_action_never_dispatched"})
}

func noActiveToolExecutionsTx(ctx context.Context, tx pgx.Tx, org string, ids []string) error {
	var active bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM tool_executions WHERE organization_id=$1 AND task_id=ANY($2::text[]) AND status IN('awaiting_approval','approved','queued','leased','sent','unknown'))", org, ids).Scan(&active); err != nil {
		return err
	}
	if active {
		return domain.Err("conflict", "Сначала завершите или разрешите исход внешнего действия", 409)
	}
	return nil
}

// A late conflicting observation cannot rewrite the accepted observation.
// Store bounded immutable evidence and expose its reference through events.
func recordToolConflictTx(ctx context.Context, tx pgx.Tx, e domain.ToolExecution, r domain.ToolOutcomeReceipt, in domain.ToolOutcomeEvidence) error {
	hash := digestValue(in)
	var duplicate bool
	var count int
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM tool_outcome_conflicts WHERE organization_id=$1 AND execution_id=$2 AND payload_digest=$3),(SELECT count(*) FROM tool_outcome_conflicts WHERE organization_id=$1 AND execution_id=$2)", e.OrganizationID, e.ID, hash).Scan(&duplicate, &count); err != nil {
		return err
	}
	if duplicate {
		return nil
	}
	if count >= 8 {
		var already bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE organization_id=$1 AND task_id=$2 AND type='tool.outcome_conflict_overflow' AND data->>'execution_id'=$3)", e.OrganizationID, e.TaskID, e.ID).Scan(&already); err != nil {
			return err
		}
		if already {
			return nil
		}
		return emit(ctx, tx, e.OrganizationID, e.TaskID, "tool.outcome_conflict_overflow", map[string]any{"execution_id": e.ID, "receipt_id": r.ID, "retained_conflicts": 8, "additional_payload_retained": false, "requires_operator_review": true})
	}
	artifact := domain.NewID("art")
	id := domain.NewID("toc")
	digest := Hash([]byte(in.Result))
	provenance := map[string]any{"adapter": "tool:conflicting_observation", "execution_id": e.ID, "receipt_id": r.ID, "verified": false, "financial_settlement": false}
	if _, err := tx.Exec(ctx, "INSERT INTO artifacts(organization_id,id,task_id,name,media_type,content,provenance) VALUES($1,$2,$3,'tool-conflicting-observation.txt','text/plain',$4,$5)", e.OrganizationID, artifact, e.TaskID, in.Result, provenance); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO tool_outcome_conflicts(organization_id,id,execution_id,receipt_id,slot,payload_digest,outcome,effect_reference,artifact_id,content_digest,evidence_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)", e.OrganizationID, id, e.ID, r.ID, count+1, hash, in.Outcome, in.EffectReference, artifact, digest, in.EvidenceDigest); err != nil {
		return err
	}
	return emit(ctx, tx, e.OrganizationID, e.TaskID, "tool.outcome_conflicted", map[string]any{"execution_id": e.ID, "conflict_id": id, "receipt_id": r.ID, "observed_outcome": in.Outcome, "effect_reference": in.EffectReference, "artifact_id": artifact, "artifact_version": 1, "content_digest": digest, "evidence_digest": in.EvidenceDigest, "existing_receipt_unchanged": true, "financial_settlement": false})
}

func cancelToolExecutionsTx(ctx context.Context, tx pgx.Tx, org string, ids []string) error {
	rows, err := tx.Query(ctx, "SELECT "+toolExecutionColumns+" FROM tool_executions WHERE organization_id=$1 AND task_id=ANY($2::text[]) AND status IN('awaiting_approval','approved','queued','leased','sent') ORDER BY id FOR UPDATE", org, ids)
	if err != nil {
		return err
	}
	values := []domain.ToolExecution{}
	for rows.Next() {
		v, err := scanToolExecution(rows)
		if err != nil {
			rows.Close()
			return err
		}
		values = append(values, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range values {
		status := "cancelled"
		if v.Status == "sent" {
			status = "unknown"
		}
		if _, err = tx.Exec(ctx, "UPDATE tool_executions SET status=$3,reason='task_cancelled',version=version+1,lease_owner=NULL,lease_until=NULL WHERE organization_id=$1 AND id=$2", org, v.ID, status); err != nil {
			return err
		}
		if v.Status != "sent" {
			if err = releaseToolUnsentTx(ctx, tx, v, "tool.cancel.unsent"); err != nil {
				return err
			}
		} else {
			if err = classifyToolUnknownTx(ctx, tx, v); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, "UPDATE approvals SET status='expired',version=version+1,reason='task_cancelled' WHERE organization_id=$1 AND id=$2 AND status='pending'", org, v.ApprovalID); err != nil {
			return err
		}
		if err = emit(ctx, tx, org, v.TaskID, "tool.cancelled", map[string]any{"execution_id": v.ID, "outcome": status, "liabilities_preserved": v.Status == "sent"}); err != nil {
			return err
		}
	}
	return nil
}
