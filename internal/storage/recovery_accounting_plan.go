// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"strings"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// An operator selects a source record. No editable amount, reason or provider
// hash is accepted as authority for a financial change.
type RecoveryAccountingSelection struct {
	Mode             string `json:"mode,omitempty"`
	QuarantineID     string `json:"quarantine_id,omitempty"`
	OperationID      string `json:"operation_id"`
	OrganizationID   string `json:"organization_id"`
	ReservationID    string `json:"reservation_id"`
	SourceEvidenceID string `json:"source_evidence_id"`
}

type RecoveryAccountingPlan struct {
	Mode                 string `json:"mode,omitempty"`
	QuarantineID         string `json:"quarantine_id,omitempty"`
	Version              int    `json:"version"`
	RequestedOperationID string `json:"requested_operation_id"`
	OperationID          string `json:"operation_id"`
	State                string `json:"state"`
	OrganizationID       string `json:"organization_id"`
	ReservationID        string `json:"reservation_id"`
	SourceEvidenceID     string `json:"source_evidence_id"`
	SourceRowSHA256      string `json:"source_row_sha256"`
	SourceRequestSHA256  string `json:"source_request_sha256"`
	SourceRevision       int    `json:"source_revision,string"`
	TargetRevision       int64  `json:"target_revision,string"`
	Kind                 string `json:"kind"`
	ActualMicroUSD       int64  `json:"actual_microusd,string"`
	PreviousID           string `json:"previous_id"`
	CurrentHeadID        string `json:"current_head_id"`
	RequestSHA256        string `json:"request_sha256"`
	PlanSHA256           string `json:"plan_sha256"`
	ExecutionHeld        bool   `json:"execution_held"`
	RollbackConfirmed    bool   `json:"rollback_confirmed"`
}

// All fields are reconstructible from authenticated archives and immutable
// batch headers/mutations. Do not add an input that exists only in host JSON.
type recoveryAccountingSourceCommitment struct {
	Algorithm                                                                         string
	OperationID, SourceID, TargetID, RestoreID, EvidenceBatchID                       string
	SelectedReceiptSHA256, SelectedSnapshotSHA256                                     string
	EvidenceReceiptSHA256, EvidenceSnapshotSHA256, CaptureLinkSHA256                  string
	TargetCatalogSHA256, OrganizationID, ReservationID                                string
	SourceEvidenceID, SourceRowSHA256, SourceRequestSHA256, ReservationIdentitySHA256 string
	SourceKind                                                                        string
	SourceBeforeRevision, SourceAfterRevision                                         int
	SourceExpectedRevision                                                            *int
	TargetBeforeSHA256, PreviousID, TargetRequestSHA256                               string
	TargetRevision                                                                    int64
}

// V1 is permanently bound to its original mutation profile. A later schema
// must not silently change old commitment hashes: retain this value for replay
// and introduce a new version before admitting writes under a newer profile.
const recoveryAccountingSourceV1CatalogSHA256 = "70ab49a57a49bf1f8f40de0e65b063626fe734323829f9945c1d996b07cb5b98"

func recoveryAccountingSelectionValid(r RecoveryAccountingSelection) bool {
	modeValid := ((r.Mode == "" || r.Mode == "fact") && r.QuarantineID == "") || (r.Mode == "resolve" && validText(r.QuarantineID, 128))
	return modeValid && len(r.OperationID) == 32 && strings.Trim(r.OperationID, "0123456789abcdef") == "" && validText(r.OrganizationID, 128) && validText(r.ReservationID, 128) && validText(r.SourceEvidenceID, 128)
}

func recoveryAccountingSourcePlanHash(in recoveryAccountingInputs, source recoveryAccountingSource, step recoveryFinancialSourceTransition, operation, beforeSHA, previous, requestHash string, revision int64) string {
	l := in.verified.Link()
	return digestValue(recoveryAccountingSourceCommitment{
		Algorithm: "source-financial-fact-v1", OperationID: operation, SourceID: in.held.SourceID, TargetID: in.held.InstallationID, RestoreID: in.held.RestoreID, EvidenceBatchID: l.OperationID,
		SelectedReceiptSHA256: l.SelectedReceiptSHA256, SelectedSnapshotSHA256: in.original.SHA256, EvidenceReceiptSHA256: l.EvidenceReceiptSHA256, EvidenceSnapshotSHA256: in.later.SHA256, CaptureLinkSHA256: in.verified.SHA256(), TargetCatalogSHA256: recoveryAccountingSourceV1CatalogSHA256,
		OrganizationID: source.original.org, ReservationID: source.original.id, SourceEvidenceID: step.evidence.id, SourceRowSHA256: source.rowSHA256ByID[step.evidence.id], SourceRequestSHA256: step.evidence.hash, ReservationIdentitySHA256: source.identitySHA256,
		SourceKind: step.evidence.kind, SourceBeforeRevision: step.beforeRevision, SourceAfterRevision: step.afterRevision, SourceExpectedRevision: step.expected,
		TargetBeforeSHA256: beforeSHA, PreviousID: previous, TargetRequestSHA256: requestHash, TargetRevision: revision,
	})
}

type recoveryAccountingSourceMapping struct {
	operationID, previousID, requestSHA256, planSHA256 string
	revision                                           int64
}

// Reconstruct the original target request, never its current revision. This
// makes replay work even after subsequent adjustments and with a new request ID.
func (s *Store) recoveryAccountingMappingTx(ctx context.Context, tx pgx.Tx, in recoveryAccountingInputs, source recoveryAccountingSource, step recoveryFinancialSourceTransition, target *recoveryCostEvidence) (result recoveryAccountingSourceMapping, err error) {
	q := func(table string) string { return pgx.Identifier{s.runtimeSchema, table}.Sanitize() }
	rows, err := tx.Query(ctx, `SELECT b.id,COALESCE(b.previous_id,''),b.request_sha256,b.plan_sha256,b.expected_revision,
 encode(sha256(convert_to(r.before_row::text,'UTF8')),'hex'),r.before_row->>'status',(r.before_row->>'revision')::bigint,
 b.restore_id=$1 AND b.target_installation_id=$2 AND b.evidence_batch_id=$3 AND b.organization_id=$4 AND b.reservation_id=$5
 FROM `+q("recovery_accounting_batches")+` b JOIN `+q("recovery_accounting_mutations")+` e ON e.batch_id=b.id AND e.table_name='cost_evidence' AND e.operation='INSERT'
 JOIN `+q("recovery_accounting_mutations")+` r ON r.batch_id=b.id AND r.table_name='reservations' AND r.operation='UPDATE'
 WHERE e.after_row->>'organization_id'=$4 AND e.after_row->>'id'=$6 LIMIT 2`, in.held.RestoreID, in.held.InstallationID, in.verified.Link().OperationID, source.original.org, source.original.id, target.id)
	if err != nil {
		return result, ErrRecoveryAccounting
	}
	defer rows.Close()
	var beforeSHA, status string
	var beforeRevision int64
	var bound bool
	if !rows.Next() || rows.Scan(&result.operationID, &result.previousID, &result.requestSHA256, &result.planSHA256, &result.revision, &beforeSHA, &status, &beforeRevision, &bound) != nil || !bound || rows.Next() || rows.Err() != nil {
		return result, ErrRecoveryAccounting
	}
	var expected *int
	if step.evidence.kind == "adjustment" {
		if status != "settled" {
			return result, ErrRecoveryAccounting
		}
		revision := int(beforeRevision)
		expected = &revision
	} else if status == "settled" {
		return result, ErrRecoveryAccounting
	}
	if result.revision != beforeRevision || result.requestSHA256 != target.hash || target.hash != evidenceRequestHash(step.evidence.value, expected) || evidenceRequestHash(target.value, nil) != evidenceRequestHash(step.evidence.value, nil) || result.planSHA256 != recoveryAccountingSourcePlanHash(in, source, step, result.operationID, beforeSHA, result.previousID, result.requestSHA256, result.revision) {
		return result, ErrRecoveryAccounting
	}
	return result, nil
}

func (s *Store) planRecoveryAccountingSourceTx(ctx context.Context, tx pgx.Tx, in recoveryAccountingInputs, head recoveryAccountingReceipt, request RecoveryAccountingSelection) (plan RecoveryAccountingPlan, source recoveryAccountingSource, expected *int, resultErr error) {
	if request.Mode == "resolve" {
		plan, source, resultErr = s.planRecoveryAccountingResolutionTx(ctx, tx, in, head, request)
		return plan, source, nil, resultErr
	}
	source, err := s.loadRecoveryAccountingSourceTx(ctx, tx, in, request.OrganizationID, request.ReservationID, request.SourceEvidenceID)
	if err != nil {
		return plan, source, nil, err
	}
	return s.planRecoveryAccountingFactTx(ctx, tx, in, head, request, source)
}

// Reuse an already verified source inside this same transaction, including
// resolution planning. Avoid staging both archives a second time.
func (s *Store) planRecoveryAccountingFactTx(ctx context.Context, tx pgx.Tx, in recoveryAccountingInputs, head recoveryAccountingReceipt, request RecoveryAccountingSelection, source recoveryAccountingSource) (plan RecoveryAccountingPlan, resultSource recoveryAccountingSource, expected *int, resultErr error) {
	// Original evidence is already present in the import. It is not a newly
	// captured fact and must not be inserted under a new identity.
	if source.step == nil {
		return plan, source, nil, ErrRecoveryAccounting
	}
	histories, err := recoveryCostRead(ctx, tx, s.runtimeSchema)
	if err != nil {
		return plan, source, nil, err
	}
	var current *recoveryCostHistory
	for _, h := range histories {
		if h.org == request.OrganizationID && h.id == request.ReservationID {
			current = h
			break
		}
	}
	if current == nil {
		return plan, source, nil, ErrRecoveryAccounting
	}
	legacy, opaque, _, valid := recoveryCostCheck(ctx, current, &recoveryCostWork{})
	if !valid || legacy != 0 || opaque != 0 {
		return plan, source, nil, ErrRecoveryAccounting
	}
	targetByReference := map[[2]string]*recoveryCostEvidence{}
	for _, e := range current.evidence {
		targetByReference[[2]string{e.value.Source, e.value.Reference}] = e
	}
	var selectedMapping *recoveryAccountingSourceMapping
	var firstMissing string
	mapped := 0
	for _, step := range source.transitions {
		if step.kind != "fact" {
			continue
		}
		e := targetByReference[[2]string{step.evidence.value.Source, step.evidence.value.Reference}]
		if e == nil {
			if firstMissing == "" {
				firstMissing = step.evidence.id
			}
			continue
		}
		if firstMissing != "" {
			return plan, source, nil, ErrRecoveryAccounting
		}
		mapping, err := s.recoveryAccountingMappingTx(ctx, tx, in, source, step, e)
		if err != nil {
			return plan, source, nil, err
		}
		mapped++
		if step.evidence.id == request.SourceEvidenceID {
			selectedMapping = &mapping
		}
	}
	// Unmapped target-local invoices require their own authenticated provenance
	// path; this source-archive operation cannot silently interleave them.
	if len(current.evidence) != len(source.original.evidence)+mapped {
		return plan, source, nil, ErrRecoveryAccounting
	}
	q := func(table string) string { return pgx.Identifier{s.runtimeSchema, table}.Sanitize() }
	var operationExists bool
	if tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+q("recovery_accounting_batches")+" WHERE id=$1)", request.OperationID).Scan(&operationExists) != nil || (operationExists && (selectedMapping == nil || selectedMapping.operationID != request.OperationID)) {
		return plan, source, nil, ErrRecoveryAccounting
	}
	plan = RecoveryAccountingPlan{Mode: request.Mode, Version: 1, RequestedOperationID: request.OperationID, OperationID: request.OperationID, State: "ready", OrganizationID: request.OrganizationID, ReservationID: request.ReservationID, SourceEvidenceID: request.SourceEvidenceID, SourceRowSHA256: source.rowSHA256, SourceRequestSHA256: source.evidence.hash, SourceRevision: source.step.beforeRevision, Kind: source.evidence.kind, ActualMicroUSD: source.evidence.value.ActualMicroUSD, CurrentHeadID: head.ID, ExecutionHeld: true}
	if selectedMapping != nil {
		plan.State, plan.OperationID = "already_recorded", selectedMapping.operationID
		plan.PlanSHA256, plan.RequestSHA256, plan.PreviousID, plan.TargetRevision = selectedMapping.planSHA256, selectedMapping.requestSHA256, selectedMapping.previousID, selectedMapping.revision
		return plan, source, nil, nil
	}
	if firstMissing != request.SourceEvidenceID || current.pending != "" || recoverySemanticCatalogSHA256 != recoveryAccountingSourceV1CatalogSHA256 {
		return RecoveryAccountingPlan{}, source, nil, ErrRecoveryAccounting
	}
	if source.evidence.kind == "adjustment" {
		if current.status != "settled" {
			return RecoveryAccountingPlan{}, source, nil, ErrRecoveryAccounting
		}
		revision := current.revision
		expected = &revision
	} else if current.status == "settled" {
		return RecoveryAccountingPlan{}, source, nil, ErrRecoveryAccounting
	}
	var beforeSHA string
	if tx.QueryRow(ctx, "SELECT encode(sha256(convert_to(to_jsonb(r)::text,'UTF8')),'hex') FROM "+q("reservations")+" r WHERE organization_id=$1 AND id=$2", request.OrganizationID, request.ReservationID).Scan(&beforeSHA) != nil {
		return RecoveryAccountingPlan{}, source, nil, ErrRecoveryAccounting
	}
	plan.TargetRevision, plan.PreviousID = int64(current.revision), head.ID
	plan.RequestSHA256 = evidenceRequestHash(source.evidence.value, expected)
	plan.PlanSHA256 = recoveryAccountingSourcePlanHash(in, source, *source.step, request.OperationID, beforeSHA, head.ID, plan.RequestSHA256, plan.TargetRevision)
	return plan, source, expected, nil
}

// Existing evidence may be quarantined or already followed by a correction.
// Report the stored evidence outcome and current reservation, never infer that
// presence of evidence means a completed charge.
func (s *Store) recoveryAccountingSourceOutcomeTx(ctx context.Context, tx pgx.Tx, request RecoveryAccountingSelection, evidence domain.CostEvidence) (domain.AccountingOutcome, error) {
	q := func(table string) string { return pgx.Identifier{s.runtimeSchema, table}.Sanitize() }
	if _, err := tx.Exec(ctx, "SET LOCAL search_path="+pgx.Identifier{s.runtimeSchema}.Sanitize()+",pg_catalog,pg_temp"); err != nil {
		return domain.AccountingOutcome{}, ErrRecoveryAccounting
	}
	r, err := scanReservation(tx.QueryRow(ctx, "SELECT "+reservationColumns+" FROM "+q("reservations")+" WHERE organization_id=$1 AND id=$2", request.OrganizationID, request.ReservationID))
	if err != nil {
		return domain.AccountingOutcome{}, ErrRecoveryAccounting
	}
	var id string
	if tx.QueryRow(ctx, "SELECT id FROM "+q("cost_evidence")+" WHERE organization_id=$1 AND reservation_id=$2 AND source=$3 AND reference=$4", request.OrganizationID, request.ReservationID, evidence.Source, evidence.Reference).Scan(&id) != nil {
		return domain.AccountingOutcome{}, ErrRecoveryAccounting
	}
	return accountingOutcomeTx(ctx, tx, r, id)
}
