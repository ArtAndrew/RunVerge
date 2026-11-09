// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"io"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
)

// InspectRecoverySnapshotLineage reads the result of a lineage import against
// the same authenticated archive after an interruption.
// The shared inspector checks both receipts under the same recovery gate.
func (s *Store) InspectRecoverySnapshotLineage(ctx context.Context, archive *recoveryarchive.VerifiedArchive, spec RecoveryTargetSpec, limits RecoverySnapshotLimits) (RecoveryImportInspection, error) {
	if ctx == nil || archive == nil || !limits.valid() {
		return RecoveryImportInspection{}, ErrRecoverySnapshotInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	snapshot, err := recoveryBoundArchiveTo(ctx, archive, spec, limits, true, nil)
	if err != nil {
		return RecoveryImportInspection{}, err
	}
	if snapshot.Format != recoverySnapshotLineageFormat || snapshot.Lineage == nil {
		return RecoveryImportInspection{}, ErrRecoverySnapshotInvalid
	}
	history := snapshot.Lineage
	lineage := recoveryLineagePersistence{RestoreID: spec.RestoreID, TargetInstallationID: spec.TargetInstallationID, SnapshotSHA256: snapshot.SHA256, Generations: history.Generations, PayloadBytes: history.PayloadBytes, ChunksSHA256: history.ChunksSHA256}
	return s.inspectRecoveryImport(ctx, spec, snapshot.SHA256, &lineage)
}

// ImportRecoverySnapshotLineage imports an authenticated archive into an exact
// held target. The trusted host supplies the private staging parent.
// An uncertain commit is never retried here.
func (s *Store) ImportRecoverySnapshotLineage(ctx context.Context, archive *recoveryarchive.VerifiedArchive, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, stagingParent string) (result RecoveryImportReceipt, resultErr error) {
	if ctx == nil || archive == nil || !limits.valid() {
		return result, ErrRecoverySnapshotInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	connection, err := s.Pool.Acquire(ctx)
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	defer connection.Release()
	tx, err := connection.Begin(ctx)
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	commitStarted := false
	defer func() {
		if commitStarted {
			return
		}
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if tx.Rollback(cleanup) != nil {
			_ = connection.Conn().Close(cleanup)
			result, resultErr = RecoveryImportReceipt{}, ErrRecoveryImportCleanup
		}
	}()
	// These locks precede all generation savepoints and survive their rollbacks.
	if _, err = tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock_shared(841932079)"); err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock(841932080,pg_catalog.hashtext($1))", s.runtimeSchema); err != nil {
		return result, err
	}
	if _, err = validateRecoverySemanticCatalog(ctx, tx, s.runtimeSchema); err != nil {
		return result, err
	}
	inventory, err := recoverySnapshotInventory(ctx, tx, s.runtimeSchema, true)
	if err != nil {
		return result, err
	}
	prepared, err := prepareRecoveryLineageArchive(ctx, archive, spec, inventory, limits, stagingParent)
	if err != nil {
		return result, err
	}
	defer func() {
		if prepared.Close() != nil {
			result, resultErr = RecoveryImportReceipt{}, ErrRecoveryImportCleanup
		}
	}()
	if err = s.checkRecoveryProvisionImportTx(ctx, tx, spec, prepared.snapshot.SHA256); err != nil {
		return result, err
	}
	if err = validateRecoveryLineageInputTx(ctx, tx, s.runtimeSchema, prepared, inventory, limits); err != nil {
		return result, err
	}
	staged, err := s.stageRecoverySnapshotWithLineageTx(ctx, tx, archive, spec, prepared.snapshot, limits, true)
	if err != nil {
		return result, err
	}
	if err = s.importStagedRecoveryTx(ctx, tx, staged, limits); err != nil {
		return result, err
	}
	if _, err = prepared.payload.Seek(0, io.SeekStart); err != nil {
		return result, ErrRecoverySnapshotIO
	}
	history := prepared.snapshot.Lineage
	persistence := recoveryLineagePersistence{RestoreID: spec.RestoreID, TargetInstallationID: spec.TargetInstallationID, SnapshotSHA256: prepared.snapshot.SHA256, Generations: history.Generations, PayloadBytes: history.PayloadBytes, ChunksSHA256: history.ChunksSHA256}
	if err = persistRecoveryLineageTx(ctx, tx, s.runtimeSchema, persistence, prepared.payload); err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	result, err = readRecoveryImportReceipt(ctx, tx, s.runtimeSchema)
	if err != nil {
		return RecoveryImportReceipt{}, recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	// Confirm temporary plaintext cleanup before crossing the commit boundary.
	if err = prepared.Close(); err != nil {
		return RecoveryImportReceipt{}, ErrRecoveryImportCleanup
	}
	commitStarted = true
	if err = tx.Commit(ctx); err != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = connection.Conn().Close(cleanup)
		return RecoveryImportReceipt{}, ErrRecoveryImportOutcomeUnknown
	}
	return result, nil
}
