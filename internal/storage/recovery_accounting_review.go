// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
)

type RecoveryAccountingReview struct {
	ExecutionWindow        RecoveryExecutionWindow                `json:"execution_window"`
	Version                int                                    `json:"version"`
	Algorithm              string                                 `json:"algorithm"`
	ReviewSHA256           string                                 `json:"review_sha256"`
	SourceID               string                                 `json:"source_id"`
	TargetID               string                                 `json:"target_id"`
	RestoreID              string                                 `json:"restore_id"`
	EvidenceBatchID        string                                 `json:"evidence_batch_id"`
	SelectedReceiptSHA256  string                                 `json:"selected_receipt_sha256"`
	CapturedReceiptSHA256  string                                 `json:"captured_receipt_sha256"`
	SelectedSnapshotSHA256 string                                 `json:"selected_snapshot_sha256"`
	CapturedSnapshotSHA256 string                                 `json:"captured_snapshot_sha256"`
	SelectedSchemaSHA256   string                                 `json:"selected_schema_sha256"`
	CapturedSchemaSHA256   string                                 `json:"captured_schema_sha256"`
	CaptureLinkSHA256      string                                 `json:"capture_link_sha256"`
	TargetCatalogSHA256    string                                 `json:"target_catalog_sha256"`
	HeadID                 string                                 `json:"head_id"`
	HeadOrdinal            int64                                  `json:"head_ordinal,string"`
	Organizations          []RecoveryAccountingReviewOrganization `json:"organizations"`
	Reservations           []RecoveryAccountingReviewReservation  `json:"reservations"`
	Facts                  []RecoveryAccountingReviewFact         `json:"facts"`
	Receipts               []RecoveryAccountingReviewReceipt      `json:"receipts"`
	Blockers               []RecoveryFinancialBlocker             `json:"blockers"`
	ReconciliationState    string                                 `json:"reconciliation_state"`
	ExecutionHeld          bool                                   `json:"execution_held"`
	RollbackConfirmed      bool                                   `json:"rollback_confirmed"`
}
type RecoveryAccountingReviewOrganization struct {
	OrganizationID string                    `json:"organization_id"`
	Original       *RecoveryFinancialAccount `json:"original"`
	Captured       *RecoveryFinancialAccount `json:"captured"`
	Target         *RecoveryFinancialAccount `json:"target"`
}
type RecoveryAccountingReviewReservation struct {
	OrganizationID string                     `json:"organization_id"`
	ReservationID  string                     `json:"reservation_id"`
	Original       *RecoveryFinancialPosition `json:"original"`
	Captured       *RecoveryFinancialPosition `json:"captured"`
	Target         *RecoveryFinancialPosition `json:"target"`
}
type RecoveryAccountingReviewFact struct {
	OrganizationID        string `json:"organization_id"`
	ReservationID         string `json:"reservation_id"`
	SourceEvidenceID      string `json:"source_evidence_id"`
	SourceRowSHA256       string `json:"source_row_sha256"`
	Origin                string `json:"origin"`
	Kind                  string `json:"kind"`
	ActualMicroUSD        int64  `json:"actual_microusd,string"`
	Recording             string `json:"recording"`
	TargetEvidenceID      string `json:"target_evidence_id,omitempty"`
	FactOperationID       string `json:"fact_operation_id,omitempty"`
	SourceQuarantineID    string `json:"source_quarantine_id,omitempty"`
	TargetQuarantineID    string `json:"target_quarantine_id,omitempty"`
	TargetQuarantineState string `json:"target_quarantine_state"`
	ResolutionOrigin      string `json:"resolution_origin,omitempty"`
	ResolutionOperationID string `json:"resolution_operation_id,omitempty"`
	NextStep              string `json:"next_step"`
}
type RecoveryAccountingReviewReceipt struct {
	OperationID    string `json:"operation_id"`
	Ordinal        int64  `json:"ordinal,string"`
	OrganizationID string `json:"organization_id"`
	ReservationID  string `json:"reservation_id"`
	RequestSHA256  string `json:"request_sha256"`
	PlanSHA256     string `json:"plan_sha256"`
	Attribution    string `json:"attribution"`
}

// ReviewRecoveryAccounting shows three separate financial views under one
// exclusive recovery gate. It cannot settle money, clear a hold or enable work.
func (s *Store) ReviewRecoveryAccounting(ctx context.Context, selected, late *recoveryarchive.VerifiedArchive, verified *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits) (result RecoveryAccountingReview, resultErr error) {
	if !limits.valid() {
		return result, ErrRecoveryAccounting
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	original, later, err := bindRecoveryEvidence(ctx, selected, late, verified, spec, limits)
	if err != nil {
		return result, err
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return result, err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if tx.Rollback(cleanup) != nil {
			_ = conn.Conn().Close(cleanup)
			result, resultErr = RecoveryAccountingReview{}, ErrRecoveryStageCleanup
			return
		}
		if resultErr != nil {
			result = RecoveryAccountingReview{}
			return
		}
		result.RollbackConfirmed = true
		raw, err := json.Marshal(result)
		if err != nil || len(raw)+64 > recoveryPlanMaxBytes {
			result, resultErr = RecoveryAccountingReview{}, ErrRecoveryAccounting
			return
		}
		result.ReviewSHA256 = Hash(raw)
	}()
	held, catalog, err := s.evidenceTargetTx(ctx, tx, spec, original.SHA256)
	if err != nil {
		return result, err
	}
	if _, err = s.inspectRecoveryEvidenceTx(ctx, tx, spec, verified); err != nil {
		return result, err
	}
	inventory, err := recoverySnapshotInventory(ctx, tx, s.runtimeSchema, true)
	if err != nil {
		return result, err
	}
	base, err := recoverySourceInventory(inventory, original.InventorySHA256)
	if err != nil {
		return result, err
	}
	head, err := s.verifyRecoveryAccountingBaselineTx(ctx, tx, selected, original, limits, held, catalog, base)
	if err != nil {
		return result, err
	}
	in := recoveryAccountingInputs{selected: selected, late: late, verified: verified, original: original, later: later, held: held, catalog: catalog, inventory: inventory, limits: limits}
	work := &recoveryReviewWork{}
	var snapshots [3]recoveryReviewSnapshot
	var windows [2]recoveryWindowSnapshot
	for i, a := range []*recoveryarchive.VerifiedArchive{selected, late} {
		snap := original
		if i == 1 {
			snap = later
		}
		inv, e := recoverySourceInventory(inventory, snap.InventorySHA256)
		if e != nil {
			return result, e
		}
		staged, e := s.stageRecoveryTypedDataTx(ctx, tx, a, snap, limits, held, catalog, inv)
		if e != nil {
			return result, e
		}
		snapshots[i], e = readRecoveryReviewSnapshot(ctx, tx, staged.schema, work)
		if e != nil {
			return result, e
		}
		windows[i], e = readRecoveryExecutionWindow(ctx, tx, staged.schema, work)
		if e != nil {
			return result, e
		}
		if e = dropRecoveryAccountingStageTx(ctx, tx, staged, snap); e != nil {
			return result, e
		}
	}
	snapshots[2], err = readRecoveryReviewSnapshot(ctx, tx, s.runtimeSchema, work)
	if err != nil {
		return result, err
	}
	l := verified.Link()
	result = RecoveryAccountingReview{Version: 2, Algorithm: "held-accounting-execution-review-v2", ExecutionWindow: compareRecoveryExecutionWindow(windows[0], windows[1]), SourceID: held.SourceID, TargetID: held.InstallationID, RestoreID: held.RestoreID, EvidenceBatchID: l.OperationID, SelectedReceiptSHA256: l.SelectedReceiptSHA256, CapturedReceiptSHA256: l.EvidenceReceiptSHA256, SelectedSnapshotSHA256: original.SHA256, CapturedSnapshotSHA256: later.SHA256, SelectedSchemaSHA256: original.Metadata.SchemaSHA256, CapturedSchemaSHA256: later.Metadata.SchemaSHA256, CaptureLinkSHA256: verified.SHA256(), TargetCatalogSHA256: recoverySemanticCatalogSHA256, HeadID: head.ID, HeadOrdinal: head.Ordinal, Organizations: []RecoveryAccountingReviewOrganization{}, Reservations: []RecoveryAccountingReviewReservation{}, Facts: []RecoveryAccountingReviewFact{}, Receipts: []RecoveryAccountingReviewReceipt{}, Blockers: []RecoveryFinancialBlocker{{Code: "external_window_coverage_unproven"}, {Code: "fresh_authority_and_new_work_not_enabled"}}, ReconciliationState: "pending", ExecutionHeld: true}
	if err = s.buildRecoveryAccountingReviewTx(ctx, tx, in, snapshots, work, &result); err != nil {
		return result, err
	}
	sort.Slice(result.Facts, func(i, j int) bool {
		a, b := result.Facts[i], result.Facts[j]
		if a.OrganizationID != b.OrganizationID {
			return a.OrganizationID < b.OrganizationID
		}
		if a.ReservationID != b.ReservationID {
			return a.ReservationID < b.ReservationID
		}
		if a.SourceEvidenceID != b.SourceEvidenceID {
			return a.SourceEvidenceID < b.SourceEvidenceID
		}
		return a.TargetEvidenceID < b.TargetEvidenceID
	})
	sort.Slice(result.Blockers, func(i, j int) bool {
		a, _ := json.Marshal(result.Blockers[i])
		b, _ := json.Marshal(result.Blockers[j])
		return string(a) < string(b)
	})
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}
