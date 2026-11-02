// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Resolution is a distinct commitment, not a second insertion of the invoice.
// Every input is recovered from signed archives or immutable target receipts.
// The source need not have resolved (or even quarantined) this fact: admission
// capacity is local to the target, while the selected invoice remains fixed.
type recoveryAccountingResolutionCommitment struct {
	Algorithm, OperationID, SourceID, TargetID, RestoreID, EvidenceBatchID                                          string
	SelectedReceiptSHA256, SelectedSnapshotSHA256, EvidenceReceiptSHA256, EvidenceSnapshotSHA256, CaptureLinkSHA256 string
	TargetCatalogSHA256, OrganizationID, ReservationID, ReservationIdentitySHA256                                   string
	SourceEvidenceID, SourceRowSHA256, SourceRequestSHA256, ProvenanceOrigin                                        string
	FactOperationID, FactPlanSHA256, TargetEvidenceID, TargetEvidenceSHA256                                         string
	QuarantineID, QuarantineSHA256, TargetBeforeSHA256, PreviousID, RequestSHA256                                   string
	TargetRevision                                                                                                  int64
}

type recoveryAccountingResolutionProvenance struct {
	origin, factOperation, factPlan, evidenceID, evidenceSHA, quarantineSHA string
}

func recoveryAccountingResolutionPlanHash(in recoveryAccountingInputs, source recoveryAccountingSource, provenance recoveryAccountingResolutionProvenance, request RecoveryAccountingSelection, operation, beforeSHA, previous, requestHash string, revision int64) string {
	l := in.verified.Link()
	return digestValue(recoveryAccountingResolutionCommitment{
		Algorithm: "source-financial-resolution-v1", OperationID: operation, SourceID: in.held.SourceID, TargetID: in.held.InstallationID, RestoreID: in.held.RestoreID, EvidenceBatchID: l.OperationID,
		SelectedReceiptSHA256: l.SelectedReceiptSHA256, SelectedSnapshotSHA256: in.original.SHA256, EvidenceReceiptSHA256: l.EvidenceReceiptSHA256, EvidenceSnapshotSHA256: in.later.SHA256, CaptureLinkSHA256: in.verified.SHA256(),
		TargetCatalogSHA256: recoveryAccountingSourceV1CatalogSHA256, OrganizationID: request.OrganizationID, ReservationID: request.ReservationID, ReservationIdentitySHA256: source.identitySHA256,
		SourceEvidenceID: request.SourceEvidenceID, SourceRowSHA256: source.rowSHA256, SourceRequestSHA256: source.evidence.hash, ProvenanceOrigin: provenance.origin,
		FactOperationID: provenance.factOperation, FactPlanSHA256: provenance.factPlan, TargetEvidenceID: provenance.evidenceID, TargetEvidenceSHA256: provenance.evidenceSHA,
		QuarantineID: request.QuarantineID, QuarantineSHA256: provenance.quarantineSHA, TargetBeforeSHA256: beforeSHA, PreviousID: previous, RequestSHA256: requestHash, TargetRevision: revision,
	})
}

func (s *Store) planRecoveryAccountingResolutionTx(ctx context.Context, tx pgx.Tx, in recoveryAccountingInputs, head recoveryAccountingReceipt, request RecoveryAccountingSelection) (plan RecoveryAccountingPlan, source recoveryAccountingSource, resultErr error) {
	source, err := s.loadRecoveryAccountingSourceTx(ctx, tx, in, request.OrganizationID, request.ReservationID, request.SourceEvidenceID)
	if err != nil {
		return plan, source, err
	}
	histories, err := recoveryCostRead(ctx, tx, s.runtimeSchema)
	if err != nil {
		return plan, source, err
	}
	var current *recoveryCostHistory
	for _, h := range histories {
		if h.org == request.OrganizationID && h.id == request.ReservationID {
			current = h
			break
		}
	}
	if current == nil {
		return plan, source, ErrRecoveryAccounting
	}
	legacy, opaque, _, valid := recoveryCostCheck(ctx, current, &recoveryCostWork{})
	if !valid || legacy != 0 || opaque != 0 {
		return plan, source, ErrRecoveryAccounting
	}
	z := current.quarantines[request.QuarantineID]
	if z == nil {
		return plan, source, ErrRecoveryAccounting
	}
	target := current.evidence[z.evidence]
	if target == nil || target.q != z || evidenceRequestHash(target.value, nil) != evidenceRequestHash(source.evidence.value, nil) {
		return plan, source, ErrRecoveryAccounting
	}
	q := func(table string) string { return pgx.Identifier{s.runtimeSchema, table}.Sanitize() }
	provenance := recoveryAccountingResolutionProvenance{evidenceID: target.id}
	if tx.QueryRow(ctx, `SELECT encode(sha256(convert_to(to_jsonb(e)::text,'UTF8')),'hex'),encode(sha256(convert_to(to_jsonb(z)::text,'UTF8')),'hex')
 FROM `+q("cost_evidence")+` e JOIN `+q("cost_quarantines")+` z ON z.organization_id=e.organization_id AND z.reservation_id=e.reservation_id AND z.evidence_id=e.id
 WHERE e.organization_id=$1 AND e.reservation_id=$2 AND e.id=$3 AND z.id=$4`, request.OrganizationID, request.ReservationID, target.id, z.id).Scan(&provenance.evidenceSHA, &provenance.quarantineSHA) != nil {
		return plan, source, ErrRecoveryAccounting
	}
	if source.step == nil {
		original := source.original.evidence[request.SourceEvidenceID]
		if original == nil || original.q == nil || original.q.resolution != nil || original.id != target.id || original.q.id != z.id || source.originalEvidenceSHA256 != provenance.evidenceSHA || source.originalQuarantineSHA256 != provenance.quarantineSHA {
			return plan, source, ErrRecoveryAccounting
		}
		provenance.origin = "original-snapshot"
	} else {
		// Reuse the full ordered fact-prefix verification, but do not confuse a
		// resolution operation ID with the fact's original recorded operation ID.
		factRequest := request
		factRequest.Mode, factRequest.QuarantineID = "", ""
		mapping, e := s.recoveryAccountingMappingTx(ctx, tx, in, source, *source.step, target)
		if e != nil {
			return plan, source, e
		}
		factRequest.OperationID = mapping.operationID
		factPlan, _, _, e := s.planRecoveryAccountingFactTx(ctx, tx, in, head, factRequest, source)
		if e != nil || factPlan.State != "already_recorded" || factPlan.OperationID != mapping.operationID {
			return plan, source, ErrRecoveryAccounting
		}
		provenance.origin, provenance.factOperation, provenance.factPlan = "captured-fact", mapping.operationID, mapping.planSHA256
	}
	plan = RecoveryAccountingPlan{Version: 1, Mode: "resolve", QuarantineID: z.id, RequestedOperationID: request.OperationID, OperationID: request.OperationID, State: "ready", OrganizationID: request.OrganizationID, ReservationID: request.ReservationID, SourceEvidenceID: request.SourceEvidenceID, SourceRowSHA256: source.rowSHA256, SourceRequestSHA256: source.evidence.hash, SourceRevision: source.original.revision, TargetRevision: int64(z.revision), Kind: source.evidence.kind, ActualMicroUSD: source.evidence.value.ActualMicroUSD, PreviousID: head.ID, CurrentHeadID: head.ID, ExecutionHeld: true}
	if source.step != nil {
		plan.SourceRevision = source.step.beforeRevision
	}
	plan.RequestSHA256 = recoveryAccountingResolutionHash(request.OrganizationID, request.ReservationID, z.id, z.revision)
	var operationExists bool
	if tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+q("recovery_accounting_batches")+" WHERE id=$1)", request.OperationID).Scan(&operationExists) != nil {
		return RecoveryAccountingPlan{}, source, ErrRecoveryAccounting
	}
	if z.resolution != nil {
		// Reconstruct the original resolution receipt. The current reservation may
		// already contain later corrections; its current revision is not replay authority.
		rows, e := tx.Query(ctx, `SELECT b.id,COALESCE(b.previous_id,''),b.request_sha256,b.plan_sha256,b.expected_revision,
 encode(sha256(convert_to(r.before_row::text,'UTF8')),'hex'),(r.before_row->>'revision')::bigint,r.before_row->>'pending_quarantine_id',
 b.restore_id=$1 AND b.target_installation_id=$2 AND b.evidence_batch_id=$3 AND b.organization_id=$4 AND b.reservation_id=$5
 FROM `+q("recovery_accounting_batches")+` b JOIN `+q("recovery_accounting_mutations")+` z ON z.batch_id=b.id AND z.table_name='cost_quarantine_resolutions' AND z.operation='INSERT'
 JOIN `+q("recovery_accounting_mutations")+` r ON r.batch_id=b.id AND r.table_name='reservations' AND r.operation='UPDATE'
 WHERE z.after_row->>'organization_id'=$4 AND z.after_row->>'reservation_id'=$5 AND z.after_row->>'quarantine_id'=$6 AND z.after_row->>'evidence_id'=$7 LIMIT 2`, in.held.RestoreID, in.held.InstallationID, in.verified.Link().OperationID, request.OrganizationID, request.ReservationID, z.id, target.id)
		if e != nil {
			return RecoveryAccountingPlan{}, source, ErrRecoveryAccounting
		}
		defer rows.Close()
		var operation, previous, requestHash, planHash, beforeSHA, pending string
		var revision, beforeRevision int64
		var bound bool
		if !rows.Next() || rows.Scan(&operation, &previous, &requestHash, &planHash, &revision, &beforeSHA, &beforeRevision, &pending, &bound) != nil || !bound || rows.Next() || rows.Err() != nil || pending != z.id || revision != int64(z.revision) || beforeRevision != revision || requestHash != plan.RequestSHA256 || (operationExists && operation != request.OperationID) || planHash != recoveryAccountingResolutionPlanHash(in, source, provenance, request, operation, beforeSHA, previous, requestHash, revision) {
			return RecoveryAccountingPlan{}, source, ErrRecoveryAccounting
		}
		plan.State, plan.OperationID, plan.PreviousID, plan.PlanSHA256 = "already_recorded", operation, previous, planHash
		return plan, source, nil
	}
	wantStatus := z.status
	if wantStatus == "reserved" {
		wantStatus = "unknown"
	}
	if operationExists || current.pending != z.id || current.revision != z.revision || current.status != wantStatus || !sameAmount(current.actual, z.actual) || recoverySemanticCatalogSHA256 != recoveryAccountingSourceV1CatalogSHA256 {
		return RecoveryAccountingPlan{}, source, ErrRecoveryAccounting
	}
	var beforeSHA string
	if tx.QueryRow(ctx, "SELECT encode(sha256(convert_to(to_jsonb(r)::text,'UTF8')),'hex') FROM "+q("reservations")+" r WHERE organization_id=$1 AND id=$2", request.OrganizationID, request.ReservationID).Scan(&beforeSHA) != nil {
		return RecoveryAccountingPlan{}, source, ErrRecoveryAccounting
	}
	plan.PlanSHA256 = recoveryAccountingResolutionPlanHash(in, source, provenance, request, request.OperationID, beforeSHA, head.ID, plan.RequestSHA256, plan.TargetRevision)
	return plan, source, nil
}
