// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
)

var ErrRecoveryAccountingUnknown = errors.New("financial commit outcome unknown; inspect the exact operation without replay")

type RecoveryAccountingResult struct {
	Plan          RecoveryAccountingPlan    `json:"plan"`
	State         string                    `json:"state"`
	Outcome       domain.AccountingOutcome  `json:"outcome"`
	Receipt       recoveryAccountingReceipt `json:"receipt"`
	NewlyRecorded bool                      `json:"newly_recorded"`
	ExecutionHeld bool                      `json:"execution_held"`
}

// PlanRecoveryAccounting selects a fact already recorded by the trusted source.
// The source signature is provenance, not independent invoice verification.
// This host storage operation is not exposed through an ordinary tenant API.
func (s *Store) PlanRecoveryAccounting(ctx context.Context, selected, late *recoveryarchive.VerifiedArchive, verified *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, selection RecoveryAccountingSelection) (RecoveryAccountingPlan, error) {
	r, err := s.recoveryAccountingSourceOperation(ctx, selected, late, verified, spec, limits, selection, "", false)
	return r.Plan, err
}

// ApplyRecoveryAccounting recomputes every input under the same exclusive gate
// as the exact mutation. A plan digest alone is never authority: the archived
// record, complete source history, target baseline and current head are checked.
// A lost COMMIT reply returns no success report and never starts another attempt.
func (s *Store) ApplyRecoveryAccounting(ctx context.Context, selected, late *recoveryarchive.VerifiedArchive, verified *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, selection RecoveryAccountingSelection, planSHA256 string) (RecoveryAccountingResult, error) {
	return s.recoveryAccountingSourceOperation(ctx, selected, late, verified, spec, limits, selection, planSHA256, true)
}

func (s *Store) recoveryAccountingSourceOperation(ctx context.Context, selected, late *recoveryarchive.VerifiedArchive, verified *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, selection RecoveryAccountingSelection, expectedPlan string, apply bool) (result RecoveryAccountingResult, resultErr error) {
	if !limits.valid() || !recoveryAccountingSelectionValid(selection) || (apply && !validDigest(expectedPlan)) {
		return result, ErrRecoveryAccounting
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	original, later, err := bindRecoveryEvidence(ctx, selected, late, verified, spec, limits)
	if err != nil {
		return result, err
	}
	connection, err := s.Pool.Acquire(ctx)
	if err != nil {
		return result, ErrRecoveryAccounting
	}
	defer connection.Release()
	tx, err := connection.Begin(ctx)
	if err != nil {
		return result, ErrRecoveryAccounting
	}
	committing := false
	defer func() {
		if committing {
			return
		}
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if tx.Rollback(cleanup) != nil {
			_ = connection.Conn().Close(cleanup)
			result, resultErr = RecoveryAccountingResult{}, ErrRecoveryStageCleanup
		} else if resultErr == nil {
			result.Plan.RollbackConfirmed = true
		}
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
	baseInventory, err := recoverySourceInventory(inventory, original.InventorySHA256)
	if err != nil {
		return result, err
	}
	head, err := s.verifyRecoveryAccountingBaselineTx(ctx, tx, selected, original, limits, held, catalog, baseInventory)
	if err != nil {
		return result, err
	}
	in := recoveryAccountingInputs{selected: selected, late: late, verified: verified, original: original, later: later, held: held, catalog: catalog, inventory: inventory, limits: limits}
	plan, source, expectedRevision, err := s.planRecoveryAccountingSourceTx(ctx, tx, in, head, selection)
	if err != nil {
		return result, err
	}
	result.Plan, result.ExecutionHeld = plan, true
	if !apply {
		return result, nil
	}
	if plan.PlanSHA256 != expectedPlan {
		return RecoveryAccountingResult{}, ErrRecoveryAccounting
	}
	if plan.State == "already_recorded" {
		result.Outcome, err = s.recoveryAccountingSourceOutcomeTx(ctx, tx, selection, source.evidence.value)
		if err != nil {
			return RecoveryAccountingResult{}, err
		}
		if tx.QueryRow(ctx, "SELECT id,ordinal,COALESCE(previous_id,''),mutation_count FROM "+pgx.Identifier{s.runtimeSchema, "recovery_accounting_batches"}.Sanitize()+" WHERE id=$1", plan.OperationID).Scan(&result.Receipt.ID, &result.Receipt.Ordinal, &result.Receipt.PreviousID, &result.Receipt.MutationCount) != nil {
			return RecoveryAccountingResult{}, ErrRecoveryAccounting
		}
		result.State = "already_recorded"
		return result, nil
	}
	b := recoveryAccountingBatch{id: selection.OperationID, evidenceBatchID: verified.Link().OperationID, organizationID: selection.OrganizationID, reservationID: selection.ReservationID, requestSHA256: plan.RequestSHA256, planSHA256: plan.PlanSHA256, expectedRevision: plan.TargetRevision}
	result.Outcome, result.Receipt, err = s.recordRecoveryFinancialFactTx(ctx, tx, held, b, source.evidence.value, expectedRevision)
	if err != nil || result.Receipt.ID != b.id || result.Receipt.Ordinal != head.Ordinal+1 || result.Receipt.PreviousID != head.ID {
		return RecoveryAccountingResult{}, ErrRecoveryAccounting
	}
	committing = true
	if tx.Commit(ctx) != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = connection.Conn().Close(cleanup)
		return RecoveryAccountingResult{}, ErrRecoveryAccountingUnknown
	}
	result.State, result.NewlyRecorded = "recorded", true
	return result, nil
}
