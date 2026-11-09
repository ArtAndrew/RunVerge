// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/recoverylineage"
	"github.com/jackc/pgx/v5"
)

// The business rows, prior bytes and new generation share the export's
// repeatable-read snapshot. Private staging lets us declare the exact length
// and commitment before writing chunks to the enclosing snapshot.
func writeHeldRecoverySnapshotLineage(ctx context.Context, tx pgx.Tx, schema string, header recoverySnapshotHeader, w *recoverySnapshotWriter, limits RecoverySnapshotLimits, report *RecoverySnapshotReport, held recoveryHeldBackup) (resultErr error) {
	receipt, err := readRecoveryImportReceipt(ctx, tx, schema)
	if err != nil || receipt.TargetInstallationID != header.Metadata.SourceID || !recoveryImportProfileSupported(receipt) {
		return ErrRecoverySnapshotSource
	}
	cut, err := time.Parse(time.RFC3339Nano, header.Metadata.CutUTC)
	priorCut, priorErr := time.Parse(time.RFC3339Nano, receipt.CutUTC)
	if err != nil || priorErr != nil || !cut.After(priorCut) {
		return ErrRecoverySnapshotSource
	}

	imports := pgx.Identifier{schema, "recovery_lineage_imports"}.Sanitize()
	chunks := pgx.Identifier{schema, "recovery_lineage_chunks"}.Sanitize()
	receipts := pgx.Identifier{schema, "recovery_import_receipts"}.Sanitize()
	prior := recoveryLineagePersistence{RestoreID: receipt.RestoreID, TargetInstallationID: receipt.TargetInstallationID, SnapshotSHA256: receipt.SnapshotSHA256}
	err = tx.QueryRow(ctx, "SELECT l.generation_count,l.payload_bytes,l.chunks_sha256 FROM "+imports+" l JOIN "+receipts+" r USING(restore_id) WHERE l.restore_id=$1 AND l.creating_transaction=r.creating_transaction", prior.RestoreID).Scan(&prior.Generations, &prior.PayloadBytes, &prior.ChunksSHA256)
	hasPrior := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		var unexpected bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+imports+") OR EXISTS(SELECT 1 FROM "+chunks+")").Scan(&unexpected); err != nil || unexpected {
			return ErrRecoveryStageData
		}
	} else if err != nil || !prior.valid() {
		return ErrRecoveryStageData
	}
	generations := prior.Generations + 1
	maxBytes := min(limits.MaxTotalBytes-report.Bytes, limits.MaxWireBytes, int64(recoverylineage.MaxPayloadBytes))
	if generations > min(64, limits.MaxMetadataEntries) || prior.PayloadBytes >= maxBytes || report.Rows >= limits.MaxRows {
		return ErrRecoverySnapshotLimit
	}
	var currentBytes int64
	payload, err := recoveryarchive.StageDerived(ctx, held.stagingParent, maxBytes, func(out io.Writer) error {
		if hasPrior {
			if err := readRecoveryLineageTx(ctx, tx, schema, prior, out); err != nil {
				return err
			}
		}
		writer := &recoverySnapshotWriter{ctx: ctx, out: out, hash: sha256.New(), limit: maxBytes - prior.PayloadBytes}
		var generation RecoverySnapshotReport
		if err := writeRecoveryLineageGenerationTx(ctx, tx, schema, writer, limits, &generation); err != nil {
			return err
		}
		currentBytes = writer.bytes
		return nil
	})
	if err != nil {
		return err
	}
	defer func() {
		if payload.Close() != nil {
			resultErr = ErrRecoveryStageCleanup
		}
	}()
	total := prior.PayloadBytes + currentBytes
	sections, records, err := scanRecoveryLineagePayload(ctx, payload, header.Metadata.SourceID, generations, total, header.Inventory, limits)
	if err != nil {
		return err
	}
	if sections[len(sections)-1].Offset != prior.PayloadBytes {
		return ErrRecoverySnapshotInvalid
	}
	commitment, err := recoverylineage.NewChunkCommitment(total)
	if err != nil {
		return ErrRecoverySnapshotLimit
	}
	reader := io.NewSectionReader(payload, 0, total)
	buf := make([]byte, min(total, int64(recoverylineage.ChunkBytes)))
	for ordinal, remaining := 0, total; remaining > 0; ordinal++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := buf[:min(remaining, int64(len(buf)))]
		if _, err := io.ReadFull(reader, chunk); err != nil {
			return ErrRecoverySnapshotIO
		}
		if err := commitment.Add(ordinal, chunk); err != nil {
			return ErrRecoverySnapshotInvalid
		}
		remaining -= int64(len(chunk))
	}
	digest, err := commitment.Digest()
	if err != nil {
		return ErrRecoverySnapshotInvalid
	}
	declared := RecoveryLineageFramingReport{PayloadBytes: total, Records: records, Generations: generations, ChunksSHA256: digest}
	return writeRecoverySnapshotLineage(w, io.NewSectionReader(payload, 0, total), declared, limits, report)
}
