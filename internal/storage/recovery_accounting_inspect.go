// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"strings"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
)

type RecoveryAccountingInspection struct {
	OperationID       string `json:"operation_id"`
	RequestSHA256     string `json:"request_sha256"`
	State             string `json:"state"`
	HeadID            string `json:"head_id"`
	HeadOrdinal       int64  `json:"head_ordinal,string"`
	BatchOrdinal      int64  `json:"batch_ordinal,string"`
	MutationCount     int    `json:"mutation_count"`
	ExecutionHeld     bool   `json:"execution_held"`
	RollbackConfirmed bool   `json:"rollback_confirmed"`
}

// InspectRecoveryAccounting resolves uncertainty about one exact local commit.
// It verifies the whole baseline under the exclusive recovery gate, including
// receipts after the requested operation. It neither reapplies a fact nor calls
// a provider. A missing batch is reported only after acquiring that gate; an
// in-flight accounting transaction cannot be mistaken for a completed absence.
// This is a host storage operation, not a tenant-facing permission boundary.
func (s *Store) InspectRecoveryAccounting(ctx context.Context, selected *recoveryarchive.VerifiedArchive, verified *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, operationID, requestSHA256 string) (result RecoveryAccountingInspection, resultErr error) {
	if selected == nil || verified == nil || !limits.valid() || len(operationID) != 32 || strings.Trim(operationID, "0123456789abcdef") != "" || !validDigest(requestSHA256) {
		return result, ErrRecoveryAccounting
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	original, err := recoveryBoundArchive(ctx, selected, spec, limits)
	if err != nil {
		return result, err
	}
	l := verified.Link()
	if l.SourceID != spec.SourceID || l.SelectedCutUTC != spec.CutUTC || l.SelectedSnapshotSHA256 != original.SHA256 || l.SelectedReceiptSHA256 != recoveryManifestReceipt(selected) {
		return result, ErrRecoveryAccounting
	}
	connection, err := s.Pool.Acquire(ctx)
	if err != nil {
		return result, ErrRecoveryAccounting
	}
	defer connection.Release()
	// PostgreSQL READ ONLY forbids creating/dropping the TEMP staging tables.
	// This transaction writes temporary verification data only and must roll
	// back, exactly as the reconciliation planner does. No persistent mutation
	// statement or commit is reachable here.
	tx, err := connection.Begin(ctx)
	if err != nil {
		return result, ErrRecoveryAccounting
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if tx.Rollback(cleanup) != nil {
			_ = connection.Conn().Close(cleanup)
			result, resultErr = RecoveryAccountingInspection{}, ErrRecoveryStageCleanup
		} else if resultErr == nil {
			result.RollbackConfirmed = true
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
	inventory, err = recoverySourceInventory(inventory, original.InventorySHA256)
	if err != nil {
		return result, err
	}
	head, err := s.verifyRecoveryAccountingBaselineTx(ctx, tx, selected, original, limits, held, catalog, inventory)
	if err != nil {
		return result, err
	}
	result = RecoveryAccountingInspection{OperationID: operationID, RequestSHA256: requestSHA256, State: "not_recorded", HeadID: head.ID, HeadOrdinal: head.Ordinal, ExecutionHeld: true}
	var storedHash, batch string
	err = tx.QueryRow(ctx, "SELECT request_sha256,evidence_batch_id,ordinal,mutation_count FROM "+pgx.Identifier{s.runtimeSchema, "recovery_accounting_batches"}.Sanitize()+" WHERE id=$1 AND restore_id=$2 AND target_installation_id=$3", operationID, held.RestoreID, held.InstallationID).Scan(&storedHash, &batch, &result.BatchOrdinal, &result.MutationCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil || storedHash != requestSHA256 || batch != l.OperationID {
		return RecoveryAccountingInspection{}, ErrRecoveryAccounting
	}
	result.State = "recorded"
	return result, nil
}
