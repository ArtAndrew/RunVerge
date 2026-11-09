// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"strconv"
	"strings"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

func recoveryFinanceResolutionID(target, restore, origin, quarantine string, revision int) string {
	return digestValue([]string{"cold-finance-resolution-v1", target, restore, origin, quarantine, strconv.Itoa(revision)})[:32]
}

func recoveryFinanceResolutionCommitment(origin, originPlan, quarantineSHA, beforeSHA, previous, request string) string {
	return digestValue([]string{"cold-finance-resolution-proof-v1", origin, originPlan, quarantineSHA, beforeSHA, previous, request})
}

func (s *Store) resolveRecoveryFinanceStatementTx(ctx context.Context, tx pgx.Tx, held RecoveryTargetState, head string,
	b recoveryAccountingBatch, result RecoveryFinanceResult, originPlan string, outcome domain.AccountingOutcome,
	selection RecoveryFinanceResolution, apply bool) (RecoveryFinanceResult, bool, error) {
	var empty RecoveryFinanceResult
	if outcome.QuarantineID != selection.QuarantineID {
		return empty, false, ErrRecoveryAccounting
	}
	schema := pgx.Identifier{s.runtimeSchema}.Sanitize()
	origin := result.OperationID
	b.expectedRevision = int64(selection.ExpectedRevision)
	b.id = recoveryFinanceResolutionID(held.InstallationID, held.RestoreID, origin, selection.QuarantineID, selection.ExpectedRevision)
	b.requestSHA256 = recoveryAccountingResolutionHash(b.organizationID, b.reservationID, selection.QuarantineID, selection.ExpectedRevision)
	result.OperationID, result.Outcome = b.id, &outcome
	var quarantineSHA, beforeSHA string
	var exists bool
	err := tx.QueryRow(ctx, strings.ReplaceAll(`SELECT encode(sha256(convert_to(m.after_row::text,'UTF8')),'hex'),
 encode(sha256(convert_to(to_jsonb(r)::text,'UTF8')),'hex'),
 EXISTS(SELECT 1 FROM $S$.recovery_accounting_batches WHERE id=$7)
 FROM $S$.cost_quarantines z JOIN $S$.reservations r ON r.organization_id=z.organization_id AND r.id=z.reservation_id
 JOIN $S$.recovery_accounting_mutations m ON m.batch_id=$1 AND m.table_name='cost_quarantines' AND m.operation='INSERT'
  AND m.after_row=to_jsonb(z)
 WHERE z.organization_id=$2 AND z.reservation_id=$3 AND z.id=$4 AND z.evidence_id=$5 AND z.revision=$6`, "$S$", schema),
		origin, b.organizationID, b.reservationID, selection.QuarantineID, outcome.EvidenceID, selection.ExpectedRevision, b.id).
		Scan(&quarantineSHA, &beforeSHA, &exists)
	if err != nil {
		return empty, false, ErrRecoveryAccounting
	}
	if exists {
		if outcome.Status != "settled" || validateRecoveryFinanceResolutions(ctx, tx, schema, b.id) != nil {
			return empty, false, ErrRecoveryAccounting
		}
		result.State = "already_recorded"
		return result, false, nil
	}
	if outcome.Status != "quarantined" || outcome.Reservation.Revision != selection.ExpectedRevision ||
		outcome.Reservation.PendingQuarantineID != selection.QuarantineID {
		return empty, false, ErrRecoveryAccounting
	}
	if !apply {
		return result, false, nil
	}
	b.planSHA256 = recoveryFinanceResolutionCommitment(origin, originPlan, quarantineSHA, beforeSHA, head, b.requestSHA256)
	next, receipt, err := s.resolveRecoveryFinancialQuarantineTx(ctx, tx, held, b, selection.QuarantineID, selection.ExpectedRevision)
	if err != nil {
		return empty, false, err
	}
	result.Outcome = &next
	if receipt.ID == "" && next.Status == "quarantined" {
		result.State = "quarantine_retained"
		return result, false, nil
	}
	if receipt.ID != b.id || receipt.PreviousID != head || next.Status != "settled" ||
		validateRecoveryFinanceResolutions(ctx, tx, schema, b.id) != nil {
		return empty, false, ErrRecoveryAccounting
	}
	result.State = "recorded"
	return result, true, nil
}

// Cold accounting batches without a statement must be resolutions of exactly
// one prior statement's quarantine. Verify immutable before-images on replay
// and import; a later correction may already have advanced the live revision.
func validateRecoveryFinanceResolutions(ctx context.Context, tx pgx.Tx, schema, only string) error {
	rows, err := tx.Query(ctx, strings.ReplaceAll(`SELECT b.id,b.target_installation_id,b.restore_id,b.organization_id,b.reservation_id,
 b.expected_revision,b.request_sha256,b.plan_sha256,COALESCE(b.previous_id,''),
 x.after_row->>'quarantine_id',p.id,p.plan_sha256,
 encode(sha256(convert_to(z.after_row::text,'UTF8')),'hex'),encode(sha256(convert_to(r.before_row::text,'UTF8')),'hex'),
 (SELECT count(*) FROM $S$.recovery_accounting_mutations m WHERE m.batch_id=b.id AND m.table_name='cost_quarantine_resolutions')=1
 AND (SELECT count(*) FROM $S$.recovery_accounting_mutations m WHERE m.batch_id=b.id AND m.table_name='reservations')=1
 AND NOT EXISTS(SELECT 1 FROM $S$.recovery_accounting_mutations m WHERE m.batch_id=b.id AND m.table_name IN ('cost_evidence','cost_quarantines'))
 AND o.batch_id IS NOT NULL AND EXISTS (
  SELECT 1 FROM $S$.recovery_accounting_mutations e WHERE e.batch_id=p.id AND e.table_name='cost_evidence' AND e.operation='INSERT'
   AND e.after_row->>'id'=x.after_row->>'evidence_id' AND e.after_row->>'payload_digest'=o.statement_sha256
   AND e.after_row->>'request_hash'=p.request_sha256 AND e.after_row->>'reservation_id'=b.reservation_id
   AND e.after_row->>'organization_id'=b.organization_id AND e.after_row->'actual_microusd'=r.after_row->'actual_microusd')
 FROM $S$.recovery_accounting_batches b JOIN $S$.recovery_evidence_batches a ON a.id=b.evidence_batch_id AND a.payload_bytes=0
 LEFT JOIN $S$.recovery_finance_statements f ON f.batch_id=b.id
 LEFT JOIN $S$.recovery_accounting_mutations x ON x.batch_id=b.id AND x.table_name='cost_quarantine_resolutions' AND x.operation='INSERT'
  AND x.after_row->>'organization_id'=b.organization_id AND x.after_row->>'reservation_id'=b.reservation_id
  AND x.after_row->>'resulting_revision'=(b.expected_revision+1)::text
 LEFT JOIN $S$.recovery_accounting_mutations r ON r.batch_id=b.id AND r.table_name='reservations' AND r.operation='UPDATE'
  AND r.before_row->>'id'=b.reservation_id AND r.before_row->>'organization_id'=b.organization_id
  AND r.before_row->>'revision'=b.expected_revision::text AND r.before_row->>'pending_quarantine_id'=x.after_row->>'quarantine_id'
  AND r.after_row->>'revision'=(b.expected_revision+1)::text AND r.after_row->'pending_quarantine_id'='null'::jsonb AND r.after_row->>'status'='settled'
 LEFT JOIN $S$.recovery_accounting_mutations z ON z.table_name='cost_quarantines' AND z.operation='INSERT'
  AND z.after_row->>'id'=x.after_row->>'quarantine_id' AND z.after_row->>'evidence_id'=x.after_row->>'evidence_id'
  AND z.after_row->>'organization_id'=b.organization_id AND z.after_row->>'reservation_id'=b.reservation_id
  AND z.after_row->>'revision'=b.expected_revision::text
 LEFT JOIN $S$.recovery_accounting_batches p ON p.id=z.batch_id AND p.ordinal<b.ordinal
  AND p.evidence_batch_id=b.evidence_batch_id AND p.restore_id=b.restore_id AND p.target_installation_id=b.target_installation_id
  AND p.organization_id=b.organization_id AND p.reservation_id=b.reservation_id
 LEFT JOIN $S$.recovery_finance_statements o ON o.batch_id=p.id
 WHERE f.batch_id IS NULL AND ($1='' OR b.id=$1)
 LIMIT 10001`, "$S$", schema), only)
	if err != nil {
		return ErrRecoveryStageData
	}
	defer rows.Close()
	seen := make(map[string]bool)
	for rows.Next() {
		var id, target, restore, org, reservation, request, plan, previous, quarantine, origin, originPlan, quarantineSHA, beforeSHA string
		var revision int
		var unchangedFact bool
		if len(seen) >= 10000 || rows.Scan(&id, &target, &restore, &org, &reservation, &revision, &request, &plan, &previous,
			&quarantine, &origin, &originPlan, &quarantineSHA, &beforeSHA, &unchangedFact) != nil || seen[id] || !unchangedFact ||
			id != recoveryFinanceResolutionID(target, restore, origin, quarantine, revision) ||
			request != recoveryAccountingResolutionHash(org, reservation, quarantine, revision) ||
			plan != recoveryFinanceResolutionCommitment(origin, originPlan, quarantineSHA, beforeSHA, previous, request) {
			return ErrRecoveryStageData
		}
		seen[id] = true
	}
	if rows.Err() != nil || (only != "" && !seen[only]) {
		return ErrRecoveryStageData
	}
	return nil
}
