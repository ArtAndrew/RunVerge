// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"io"

	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
)

// This object owns immutable extracted bytes for one authenticated archive.
// It is not a reusable admission token: the coordinator must still validate
// every generation's typed rows and the current business snapshot in its import.
type recoveryLineageInput struct {
	payload  *recoveryarchive.DerivedPayload
	snapshot RecoverySnapshotReport
	sections []recoveryLineageSection
}

func (p *recoveryLineageInput) Close() error { return p.payload.Close() }

func prepareRecoveryLineageArchive(ctx context.Context, archive *recoveryarchive.VerifiedArchive, spec RecoveryTargetSpec, inventory RecoverySnapshotInventory, limits RecoverySnapshotLimits, stagingParent string) (_ *recoveryLineageInput, resultErr error) {
	if ctx == nil || archive == nil || !limits.valid() {
		return nil, ErrRecoverySnapshotInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	var snapshot RecoverySnapshotReport
	payload, err := recoveryarchive.StageDerived(ctx, stagingParent, min(limits.MaxTotalBytes, int64(384<<20)), func(out io.Writer) error {
		var err error
		snapshot, err = recoveryBoundArchiveTo(ctx, archive, spec, limits, true, out)
		if err != nil {
			return err
		}
		if snapshot.Format != recoverySnapshotLineageFormat || snapshot.Lineage == nil {
			return ErrRecoverySnapshotInvalid
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep && payload.Close() != nil {
			resultErr = ErrRecoveryStageCleanup
		}
	}()
	sections, err := indexRecoveryLineagePayload(ctx, payload, snapshot.Metadata.SourceID, *snapshot.Lineage, inventory, limits)
	if err != nil {
		return nil, err
	}
	if _, err = payload.Seek(0, io.SeekStart); err != nil {
		return nil, ErrRecoverySnapshotIO
	}
	keep = true
	return &recoveryLineageInput{payload: payload, snapshot: snapshot, sections: sections}, nil
}

// validateRecoveryLineageInputTx runs each generation in its own savepoint so
// no historical metadata survives into the current business staging tables.
// The coordinator owns the transaction and must roll it back on every error.
// It must acquire its import/migration gates before this helper: rollback to a
// savepoint can release locks acquired inside that generation validation.
func validateRecoveryLineageInputTx(ctx context.Context, tx pgx.Tx, schema string, input *recoveryLineageInput, inventory RecoverySnapshotInventory, limits RecoverySnapshotLimits) error {
	if ctx == nil || tx == nil || input == nil || input.payload == nil || input.snapshot.Lineage == nil || !limits.valid() {
		return ErrRecoverySnapshotInvalid
	}
	declared := input.snapshot.Lineage
	if len(input.sections) != declared.Generations || len(input.sections) < 1 || len(input.sections) > 64 {
		return ErrRecoverySnapshotInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	var rows, offset int64
	for _, section := range input.sections {
		if section.Offset != offset || section.Length < 1 || section.Length > declared.PayloadBytes-offset {
			return ErrRecoverySnapshotInvalid
		}
		if _, err := tx.Exec(ctx, "SAVEPOINT recovery_lineage_validation"); err != nil {
			return ErrRecoveryStageTarget
		}
		report, err := stageRecoveryLineageGeneration(ctx, tx, schema, io.NewSectionReader(input.payload, section.Offset, section.Length), inventory, section.Identity, limits)
		if err != nil {
			return err
		}
		if report.Rows > declared.Records-rows {
			return ErrRecoverySnapshotInvalid
		}
		rows += report.Rows
		offset += section.Length
		if _, err = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT recovery_lineage_validation"); err != nil {
			return ErrRecoveryStageCleanup
		}
		if _, err = tx.Exec(ctx, "RELEASE SAVEPOINT recovery_lineage_validation"); err != nil {
			return ErrRecoveryStageCleanup
		}
	}
	if offset != declared.PayloadBytes || rows != declared.Records {
		return ErrRecoverySnapshotInvalid
	}
	return nil
}
