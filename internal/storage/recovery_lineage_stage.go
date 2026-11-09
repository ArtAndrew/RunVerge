// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"crypto/sha256"
	"io"

	"github.com/jackc/pgx/v5"
)

// stageRecoveryLineageGeneration validates an authenticated generation into
// isolated temporary tables in the caller's transaction. The caller exclusively
// owns input and must roll back on any error. This does not persist lineage or
// admit execution. Payload/business/accounting semantic gates remain required.
func stageRecoveryLineageGeneration(ctx context.Context, tx pgx.Tx, sourceSchema string, in io.ReadSeeker, inventory RecoverySnapshotInventory, expected recoveryLineageGenerationStart, limits RecoverySnapshotLimits) (RecoverySnapshotReport, error) {
	var empty RecoverySnapshotReport
	if tx == nil || in == nil || !limits.valid() {
		return empty, ErrRecoverySnapshotLimit
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	currentProfile, err := recoveryLineageCurrentProfile()
	if err != nil {
		return empty, err
	}
	if _, err := tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock_shared(841932079)"); err != nil {
		return empty, err
	}
	if _, err := validateRecoverySemanticCatalog(ctx, tx, sourceSchema); err != nil {
		return empty, err
	}
	projected, err := recoveryLineageInventoryProfile(inventory, expected.Profile)
	if err != nil {
		return empty, err
	}
	if _, err = in.Seek(0, io.SeekStart); err != nil {
		return empty, ErrRecoverySnapshotIO
	}
	reader := recoveryStageReader(ctx, in, limits.MaxWireBytes)
	var report RecoverySnapshotReport
	if err = readRecoveryLineageGeneration(reader, inventory, expected, limits, &report); err != nil {
		return empty, err
	}
	if _, err = reader.reader.Peek(1); err != io.EOF {
		return empty, ErrRecoverySnapshotInvalid
	}
	report.WireBytes, report.SHA256 = reader.bytes, recoverySnapshotHash(reader.hash)
	var occupied bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_class WHERE relnamespace=pg_catalog.pg_my_temp_schema())").Scan(&occupied); err != nil || occupied {
		return empty, ErrRecoveryStageTarget
	}
	for _, table := range projected.Tables {
		ddl := "CREATE TEMP TABLE " + pgx.Identifier{"pg_temp", table.Name}.Sanitize() + " (LIKE " + pgx.Identifier{sourceSchema, table.Name}.Sanitize() + " INCLUDING CONSTRAINTS INCLUDING INDEXES EXCLUDING DEFAULTS EXCLUDING GENERATED EXCLUDING IDENTITY) ON COMMIT DROP"
		if _, err = tx.Exec(ctx, ddl); err != nil {
			return empty, recoveryStageSchemaFailure(ctx, "lineage_temporary_table", err)
		}
	}
	var stage string
	if err = tx.QueryRow(ctx, "SELECT nspname FROM pg_catalog.pg_namespace WHERE oid=pg_catalog.pg_my_temp_schema()").Scan(&stage); err != nil || stage == "" {
		return empty, ErrRecoveryStageSchema
	}
	if err = recoveryStageCheckShape(ctx, tx, sourceSchema, stage, projected); err != nil {
		return empty, err
	}
	// Only the old generation drops the new target-local receipt field.
	if currentProfile.Name != "sql026-current-generation-v1" && expected.Profile == "sql026-current-generation-v1" {
		if _, err = tx.Exec(ctx, "ALTER TABLE "+pgx.Identifier{stage, "recovery_import_receipts"}.Sanitize()+" DROP COLUMN creating_transaction"); err != nil {
			return empty, ErrRecoveryStageSchema
		}
	}
	if err = recoveryLineageStageShape(ctx, tx, stage, projected); err != nil {
		return empty, err
	}
	if _, err = in.Seek(0, io.SeekStart); err != nil {
		return empty, ErrRecoverySnapshotIO
	}
	reader = recoveryStageReader(ctx, in, limits.MaxWireBytes)
	line, err := reader.line()
	var start recoveryLineageGenerationStart
	if err != nil || recoverySnapshotDecode(line, &start) != nil || start != expected {
		return empty, ErrRecoverySnapshotInvalid
	}
	reader.accept(line)
	metadataLimits := limits
	metadataLimits.MaxRowBytes = recoveryLineageMetadataRowBytes
	for i, table := range projected.Tables {
		line, err = reader.line()
		var tableStart recoverySnapshotTableStart
		if err != nil || recoverySnapshotDecode(line, &tableStart) != nil || tableStart.Kind != "table" || tableStart.Name != table.Name {
			return empty, ErrRecoverySnapshotInvalid
		}
		reader.accept(line)
		chunks := &recoveryStageChunks{r: reader, expected: report.Tables[i], hash: sha256.New()}
		copyContext, cancelCopy := context.WithCancel(ctx)
		typed := newRecoveryStageTypedReader(copyContext, chunks, table, metadataLimits)
		tag, copyErr := tx.Conn().PgConn().CopyFrom(copyContext, typed, "COPY "+pgx.Identifier{stage, table.Name}.Sanitize()+"("+recoverySnapshotColumns(table)+") FROM STDIN (FORMAT binary)")
		cancelCopy()
		typedErr := typed.Stop()
		if copyErr != nil || typedErr != nil || !chunks.done || tag.RowsAffected() != chunks.expected.Rows {
			return empty, recoverySnapshotError(ctx, ErrRecoveryStageData)
		}
	}
	line, err = reader.line()
	var end recoveryLineageGenerationEnd
	if err != nil || recoverySnapshotDecode(line, &end) != nil || end.Kind != "generation_end" || end.Tables != len(report.Tables) || end.Rows != report.Rows || end.Bytes != report.Bytes {
		return empty, ErrRecoverySnapshotInvalid
	}
	reader.accept(line)
	if reader.bytes != report.WireBytes || recoverySnapshotHash(reader.hash) != report.SHA256 {
		return empty, ErrRecoverySnapshotInvalid
	}
	if _, err = reader.reader.Peek(1); err != io.EOF {
		return empty, ErrRecoverySnapshotInvalid
	}
	if _, err = tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
		return empty, ErrRecoveryStageData
	}
	if err = recoveryStageCheckDigests(ctx, tx, stage, projected, report, metadataLimits); err != nil {
		return empty, err
	}
	if err = validateRecoveryLineageRelations(ctx, tx, stage, expected); err != nil {
		return empty, err
	}
	if err = validateRecoveryLineageEvidence(ctx, tx, stage, limits); err != nil {
		return empty, err
	}
	if err = validateRecoveryLineageReceipt(ctx, tx, stage, inventory, expected.Profile, limits); err != nil {
		return empty, err
	}
	return report, nil
}
