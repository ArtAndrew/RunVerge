// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/domain"
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

func listExecution[T any](ctx context.Context, s *Store, p domain.Principal, taskID, raw string, limit int, table, columns string, scan func(scanner) (T, time.Time, string, error)) ([]T, string, error) {
	if limit < 1 || limit > 100 {
		return nil, "", domain.Err("invalid_request", "limit должен быть от 1 до 100", 400)
	}
	tx, e := s.beginMaterialRead(ctx, p, taskID)
	if e != nil {
		return nil, "", e
	}
	defer tx.Rollback(ctx)
	c, e := decodeCursor(raw)
	if e != nil {
		return nil, "", e
	}
	// The table/columns are private compile-time arguments, never request input.
	rows, e := tx.Query(ctx, "SELECT "+columns+" FROM "+table+" WHERE organization_id=$1 AND task_id=$2 AND (created_at,id)<($3,$4) ORDER BY created_at DESC,id DESC LIMIT $5", p.OrganizationID, taskID, c.CreatedAt, c.ID, limit+1)
	if e != nil {
		return nil, "", e
	}
	defer rows.Close()
	out := []T{}
	var lastTime time.Time
	lastID := ""
	for rows.Next() {
		v, created, id, e := scan(rows)
		if e != nil {
			return nil, "", e
		}
		out = append(out, v)
		if len(out) <= limit {
			lastTime = created
			lastID = id
		}
	}
	if e = rows.Err(); e != nil {
		return nil, "", e
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = encodeCursor(lastTime, lastID)
	}
	return out, next, tx.Commit(ctx)
}
func (s *Store) Runs(ctx context.Context, p domain.Principal, taskID, raw string, limit int) ([]domain.Run, string, error) {
	return listExecution(ctx, s, p, taskID, raw, limit, "runs", "id,organization_id,task_id,principal_id,agent_id,agent_version,status,created_at,attempt,input_digest,feedback,COALESCE(source_candidate_id,'')", func(row scanner) (v domain.Run, t time.Time, id string, e error) {
		e = row.Scan(&v.ID, &v.OrganizationID, &v.TaskID, &v.PrincipalID, &v.AgentID, &v.AgentVersion, &v.Status, &v.CreatedAt, &v.Attempt, &v.InputDigest, &v.Feedback, &v.SourceCandidateID)
		return v, v.CreatedAt, v.ID, e
	})
}
func (s *Store) Operations(ctx context.Context, p domain.Principal, taskID, raw string, limit int) ([]domain.Operation, string, error) {
	return listExecution(ctx, s, p, taskID, raw, limit, "operations", "id,organization_id,task_id,run_id,status,COALESCE(lease_owner,''),lease_until,fencing_token,attempts,max_attempts,created_at", func(row scanner) (v domain.Operation, t time.Time, id string, e error) {
		e = row.Scan(&v.ID, &v.OrganizationID, &v.TaskID, &v.RunID, &v.Status, &v.LeaseOwner, &v.LeaseUntil, &v.FencingToken, &v.Attempts, &v.MaxAttempts, &v.CreatedAt)
		return v, v.CreatedAt, v.ID, e
	})
}
func (s *Store) Actions(ctx context.Context, p domain.Principal, taskID, raw string, limit int) ([]domain.Action, string, error) {
	return listExecution(ctx, s, p, taskID, raw, limit, "actions", "id,organization_id,task_id,operation_id,intent_hash,idempotency_key,status,created_at", func(row scanner) (v domain.Action, t time.Time, id string, e error) {
		e = row.Scan(&v.ID, &v.OrganizationID, &v.TaskID, &v.OperationID, &v.IntentHash, &v.IdempotencyKey, &v.Status, &v.CreatedAt)
		return v, v.CreatedAt, v.ID, e
	})
}
func (s *Store) Approvals(ctx context.Context, p domain.Principal, taskID, raw string, limit int) ([]domain.Approval, string, error) {
	return listExecution(ctx, s, p, taskID, raw, limit, "approvals", approvalColumns, func(row scanner) (v domain.Approval, t time.Time, id string, e error) {
		v, e = scanApproval(row)
		return v, v.CreatedAt, v.ID, e
	})
}

type CostCursors struct {
	Reservation, Entry, Incident, Quarantine string
	Limit                                    int
}

func (s *Store) Costs(ctx context.Context, p domain.Principal, taskID string, cursors CostCursors) (domain.CostReport, error) {
	report := domain.CostReport{Reservations: []domain.Reservation{}, Entries: []domain.LedgerEntry{}, Incidents: []domain.FinancialIncident{}, Quarantines: []domain.CostQuarantine{}}
	if cursors.Limit < 1 || cursors.Limit > 100 {
		return report, domain.Err("invalid_request", "limit должен быть от 1 до 100", 400)
	}
	rc, e := decodeCursor(cursors.Reservation)
	if e != nil {
		return report, e
	}
	ec, e := decodeCursor(cursors.Entry)
	if e != nil {
		return report, e
	}
	ic, e := decodeCursor(cursors.Incident)
	if e != nil {
		return report, e
	}
	qc, e := decodeCursor(cursors.Quarantine)
	if e != nil {
		return report, e
	}
	tx, e := s.beginRuntimeTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return report, e
	}
	defer tx.Rollback(ctx)
	var current domain.Principal
	current.CredentialKind = p.CredentialKind
	e = tx.QueryRow(ctx, authQuery(p), p.TokenHash, p.OrganizationID).Scan(&current.ID, &current.OrganizationID, &current.Role, &current.Kind)
	if e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return report, domain.Unauthenticated
		}
		return report, rowExists(e)
	}
	if !domain.PrincipalHasPermission(current, "costs.read") {
		return report, domain.Forbidden
	}
	task, e := scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND id=$2", p.OrganizationID, taskID))
	if e != nil {
		return report, e
	}
	report.BudgetMicroUSD = task.BudgetMicroUSD
	report.ReservedMicroUSD = task.ReservedMicroUSD
	report.SpentMicroUSD = task.SpentMicroUSD
	report.FinancialHold = task.FinancialHold
	if e = tx.QueryRow(ctx, "SELECT COALESCE(sum(amount_microusd),0)::bigint,count(*) FROM reservations WHERE organization_id=$1 AND $2=ANY(scope_task_ids) AND status='unknown'", p.OrganizationID, taskID).Scan(&report.UnknownMicroUSD, &report.UnknownReservationCount); e != nil {
		return report, e
	}
	rows, e := tx.Query(ctx, "SELECT "+reservationColumns+" FROM reservations WHERE organization_id=$1 AND $2=ANY(scope_task_ids) AND (created_at,id)<($3,$4) ORDER BY created_at DESC,id DESC LIMIT $5", p.OrganizationID, taskID, rc.CreatedAt, rc.ID, cursors.Limit+1)
	if e != nil {
		return report, e
	}
	for rows.Next() {
		r, e := scanReservation(rows)
		if e != nil {
			rows.Close()
			return report, e
		}
		report.Reservations = append(report.Reservations, r)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return report, e
	}
	if len(report.Reservations) > cursors.Limit {
		report.Reservations = report.Reservations[:cursors.Limit]
		last := report.Reservations[cursors.Limit-1]
		report.NextReservationCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	rows, e = tx.Query(ctx, `SELECT e.id,e.organization_id,e.reservation_id,e.task_id,e.root_id,e.kind,e.delta_reserved_microusd,e.delta_spent_microusd,e.synthetic,e.created_at FROM ledger_entries e JOIN reservations r ON r.organization_id=e.organization_id AND r.id=e.reservation_id WHERE e.organization_id=$1 AND $2=ANY(r.scope_task_ids) AND (e.created_at,e.id)<($3,$4) ORDER BY e.created_at DESC,e.id DESC LIMIT $5`, p.OrganizationID, taskID, ec.CreatedAt, ec.ID, cursors.Limit+1)
	if e != nil {
		return report, e
	}
	for rows.Next() {
		var v domain.LedgerEntry
		if e = rows.Scan(&v.ID, &v.OrganizationID, &v.ReservationID, &v.TaskID, &v.RootID, &v.Kind, &v.DeltaReservedMicroUSD, &v.DeltaSpentMicroUSD, &v.Synthetic, &v.CreatedAt); e != nil {
			rows.Close()
			return report, e
		}
		report.Entries = append(report.Entries, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return report, e
	}
	if len(report.Entries) > cursors.Limit {
		report.Entries = report.Entries[:cursors.Limit]
		last := report.Entries[cursors.Limit-1]
		report.NextEntryCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	rows, e = tx.Query(ctx, `SELECT e.id,e.organization_id,e.reservation_id,e.task_id,e.kind,e.amount_microusd,e.created_at FROM financial_incidents e JOIN reservations r ON r.organization_id=e.organization_id AND r.id=e.reservation_id WHERE e.organization_id=$1 AND $2=ANY(r.scope_task_ids) AND (e.created_at,e.id)<($3,$4) ORDER BY e.created_at DESC,e.id DESC LIMIT $5`, p.OrganizationID, taskID, ic.CreatedAt, ic.ID, cursors.Limit+1)
	if e != nil {
		return report, e
	}
	for rows.Next() {
		var v domain.FinancialIncident
		if e = rows.Scan(&v.ID, &v.OrganizationID, &v.ReservationID, &v.TaskID, &v.Kind, &v.AmountMicroUSD, &v.CreatedAt); e != nil {
			rows.Close()
			return report, e
		}
		report.Incidents = append(report.Incidents, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return report, e
	}
	if len(report.Incidents) > cursors.Limit {
		report.Incidents = report.Incidents[:cursors.Limit]
		last := report.Incidents[cursors.Limit-1]
		report.NextIncidentCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM cost_quarantines q JOIN reservations r ON r.organization_id=q.organization_id AND r.id=q.reservation_id
 WHERE q.organization_id=$1 AND $2=ANY(r.scope_task_ids) AND NOT EXISTS(SELECT 1 FROM cost_quarantine_resolutions x WHERE x.organization_id=q.organization_id AND x.quarantine_id=q.id)`, p.OrganizationID, taskID).Scan(&report.PendingQuarantineCount); e != nil {
		return report, e
	}
	report.Incomplete = report.PendingQuarantineCount > 0
	rows, e = tx.Query(ctx, `SELECT q.id,q.organization_id,q.reservation_id,q.evidence_id,r.task_id,e.actual_microusd,
 CASE WHEN x.id IS NULL THEN 'pending' ELSE 'resolved' END,q.reason,q.requested_kind,q.base_revision,q.revision,COALESCE(x.id,''),q.created_at
 FROM cost_quarantines q JOIN reservations r ON r.organization_id=q.organization_id AND r.id=q.reservation_id
 JOIN cost_evidence e ON e.organization_id=q.organization_id AND e.id=q.evidence_id
 LEFT JOIN cost_quarantine_resolutions x ON x.organization_id=q.organization_id AND x.quarantine_id=q.id
 WHERE q.organization_id=$1 AND $2=ANY(r.scope_task_ids) AND (q.created_at,q.id)<($3,$4) ORDER BY q.created_at DESC,q.id DESC LIMIT $5`, p.OrganizationID, taskID, qc.CreatedAt, qc.ID, cursors.Limit+1)
	if e != nil {
		return report, e
	}
	for rows.Next() {
		var q domain.CostQuarantine
		if e = rows.Scan(&q.ID, &q.OrganizationID, &q.ReservationID, &q.EvidenceID, &q.TaskID, &q.ActualMicroUSD, &q.Status, &q.Reason, &q.RequestedKind, &q.BaseRevision, &q.Revision, &q.ResolutionID, &q.CreatedAt); e != nil {
			rows.Close()
			return report, e
		}
		report.Quarantines = append(report.Quarantines, q)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return report, e
	}
	if len(report.Quarantines) > cursors.Limit {
		report.Quarantines = report.Quarantines[:cursors.Limit]
		last := report.Quarantines[cursors.Limit-1]
		report.NextQuarantineCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return report, tx.Commit(ctx)
}
