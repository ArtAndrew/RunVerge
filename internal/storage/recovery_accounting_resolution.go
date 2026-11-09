// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

type recoveryAccountingResolution struct {
	quarantineID, evidenceID, evidenceSHA256, baseStatus string
	revision                                             int
	baseActual                                           *int64
}

func recoveryAccountingResolutionHash(org, reservation, quarantine string, revision int) string {
	return digestValue(struct {
		Purpose      string
		Organization string
		Reservation  string
		Quarantine   string
		Revision     int
	}{"recovery-quarantine-resolution-v1", org, reservation, quarantine, revision})
}

// Resolution uses only an immutable persisted fact, never replacement provider
// data. Like fact application, its caller must hold the exclusive recovery gate
// and have verified the complete original/receipt/current baseline first.
func (s *Store) resolveRecoveryFinancialQuarantineTx(ctx context.Context, tx pgx.Tx, held RecoveryTargetState, b recoveryAccountingBatch, quarantine string, expectedRevision int) (domain.AccountingOutcome, recoveryAccountingReceipt, error) {
	var empty domain.AccountingOutcome
	var receipt recoveryAccountingReceipt
	if !validText(quarantine, 128) || expectedRevision < 1 || b.requestSHA256 != recoveryAccountingResolutionHash(b.organizationID, b.reservationID, quarantine, expectedRevision) {
		return empty, receipt, ErrRecoveryAccounting
	}
	q := func(table string) string { return pgx.Identifier{s.runtimeSchema, table}.Sanitize() }
	var evidence domain.CostEvidence
	resolution := recoveryAccountingResolution{quarantineID: quarantine}
	var originalExpected *int
	var kind string
	err := tx.QueryRow(ctx, `SELECT q.evidence_id,q.revision,q.base_status,q.base_actual_microusd,q.requested_kind,q.expected_revision,
 e.request_hash,e.source,e.reference,e.payload_digest,e.actual_microusd,e.reason,e.final,e.currency
 FROM `+q("cost_quarantines")+` q JOIN `+q("cost_evidence")+` e
 ON e.organization_id=q.organization_id AND e.id=q.evidence_id AND e.reservation_id=q.reservation_id
 WHERE q.organization_id=$1 AND q.id=$2 AND q.reservation_id=$3`, b.organizationID, quarantine, b.reservationID).Scan(
		&resolution.evidenceID, &resolution.revision, &resolution.baseStatus, &resolution.baseActual, &kind, &originalExpected,
		&resolution.evidenceSHA256, &evidence.Source, &evidence.Reference, &evidence.PayloadDigest, &evidence.ActualMicroUSD, &evidence.Reason, &evidence.Final, &evidence.Currency)
	if err != nil || resolution.revision != expectedRevision || resolution.evidenceSHA256 != evidenceRequestHash(evidence, originalExpected) || (kind != "settle" && kind != "adjustment") || ((kind == "adjustment") != (originalExpected != nil)) {
		return empty, receipt, ErrRecoveryAccounting
	}
	return s.recordRecoveryFinancialChangeTx(ctx, tx, held, b, evidence, originalExpected, &resolution)
}
