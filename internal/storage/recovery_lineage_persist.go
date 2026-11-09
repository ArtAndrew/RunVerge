// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"io"

	"agentplatform.local/platform/internal/recoverylineage"
	"github.com/jackc/pgx/v5"
)

type recoveryLineagePersistence struct {
	RestoreID, TargetInstallationID, SnapshotSHA256 string
	Generations                                     int
	PayloadBytes                                    int64
	ChunksSHA256                                    string
}

func (p recoveryLineagePersistence) valid() bool {
	return validRecoveryID(p.RestoreID, "restore") && validRecoveryID(p.TargetInstallationID, "inst") && validDigest(p.SnapshotSHA256) && validDigest(p.ChunksSHA256) && p.Generations >= 1 && p.Generations <= 64 && p.PayloadBytes >= 1 && p.PayloadBytes <= recoverylineage.MaxPayloadBytes
}

// persistRecoveryLineageTx is the SQL027 write primitive, not an import entry
// point. The coordinator must authenticate and validate this exact immutable
// payload before calling it in the SAME transaction as the new business import
// and its current receipt. SQL027 additionally enforces that transaction tie.
// It never commits/retries; every error requires rollback of the entire import.
// This stays unmounted until the versioned importer and SQL027 are integrated.
func persistRecoveryLineageTx(ctx context.Context, tx pgx.Tx, schema string, spec recoveryLineagePersistence, in io.Reader) error {
	if tx == nil || in == nil || !spec.valid() {
		return ErrRecoveryStageData
	}
	// The business import flushes constraints before recording lineage.
	// Defer only this parent completeness check until all chunks are present.
	if _, err := tx.Exec(ctx, "SET CONSTRAINTS "+pgx.Identifier{schema, "recovery_lineage_complete"}.Sanitize()+" DEFERRED"); err != nil {
		return recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	commitment, err := recoverylineage.NewChunkCommitment(spec.PayloadBytes)
	if err != nil {
		return ErrRecoveryStageData
	}
	count := (spec.PayloadBytes + recoverylineage.ChunkBytes - 1) / recoverylineage.ChunkBytes
	_, err = tx.Exec(ctx, "INSERT INTO "+pgx.Identifier{schema, "recovery_lineage_imports"}.Sanitize()+"(restore_id,target_installation_id,snapshot_sha256,wire_profile,attestation,generation_count,payload_bytes,chunk_count,chunks_sha256) VALUES($1,$2,$3,'recovery-lineage-v1','current-backup-attested-history',$4,$5,$6,$7)", spec.RestoreID, spec.TargetInstallationID, spec.SnapshotSHA256, spec.Generations, spec.PayloadBytes, count, spec.ChunksSHA256)
	if err != nil {
		return recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	buffer := make([]byte, recoverylineage.ChunkBytes)
	for ordinal := int64(0); ordinal < count; ordinal++ {
		if err = ctx.Err(); err != nil {
			return err
		}
		size := min(int64(recoverylineage.ChunkBytes), spec.PayloadBytes-ordinal*recoverylineage.ChunkBytes)
		payload := buffer[:int(size)]
		if _, err = io.ReadFull(in, payload); err != nil {
			return recoverySnapshotError(ctx, ErrRecoverySnapshotIO)
		}
		if err = commitment.Add(int(ordinal), payload); err != nil {
			return ErrRecoveryStageData
		}
		if _, err = tx.Exec(ctx, "INSERT INTO "+pgx.Identifier{schema, "recovery_lineage_chunks"}.Sanitize()+"(restore_id,ordinal,payload,payload_sha256) VALUES($1,$2,$3,$4)", spec.RestoreID, ordinal, payload, Hash(payload)); err != nil {
			return recoverySnapshotError(ctx, ErrRecoveryImport)
		}
	}
	// Require exact EOF, rather than silently persisting a declared prefix.
	var extra [1]byte
	if _, err = io.ReadFull(in, extra[:]); err != io.EOF {
		return ErrRecoverySnapshotInvalid
	}
	digest, err := commitment.Digest()
	if err != nil || digest != spec.ChunksSHA256 {
		return ErrRecoverySnapshotInvalid
	}
	return nil
}

// inspectRecoveryLineageTx is read-only. After an unknown COMMIT the caller
// must inspect the business receipt as well; this check never repeats an import.
func inspectRecoveryLineageTx(ctx context.Context, tx pgx.Tx, schema string, spec recoveryLineagePersistence) error {
	return readRecoveryLineageTx(ctx, tx, schema, spec, nil)
}

// An output sink must be disposable until all chunk and parent checks pass.
func readRecoveryLineageTx(ctx context.Context, tx pgx.Tx, schema string, spec recoveryLineagePersistence, out io.Writer) error {
	if tx == nil || !spec.valid() {
		return ErrRecoveryStageData
	}
	count := (spec.PayloadBytes + recoverylineage.ChunkBytes - 1) / recoverylineage.ChunkBytes
	var exact bool
	if err := tx.QueryRow(ctx, "SELECT target_installation_id=$2 AND snapshot_sha256=$3 AND wire_profile='recovery-lineage-v1' AND attestation='current-backup-attested-history' AND generation_count=$4 AND payload_bytes=$5 AND chunk_count=$6 AND chunks_sha256=$7 FROM "+pgx.Identifier{schema, "recovery_lineage_imports"}.Sanitize()+" WHERE restore_id=$1", spec.RestoreID, spec.TargetInstallationID, spec.SnapshotSHA256, spec.Generations, spec.PayloadBytes, count, spec.ChunksSHA256).Scan(&exact); err != nil || !exact {
		return ErrRecoveryStageData
	}
	rows, err := tx.Query(ctx, "SELECT ordinal,payload,payload_sha256 FROM "+pgx.Identifier{schema, "recovery_lineage_chunks"}.Sanitize()+" WHERE restore_id=$1 ORDER BY ordinal", spec.RestoreID)
	if err != nil {
		return ErrRecoveryStageData
	}
	defer rows.Close()
	commitment, err := recoverylineage.NewChunkCommitment(spec.PayloadBytes)
	if err != nil {
		return ErrRecoveryStageData
	}
	for rows.Next() {
		var ordinal int
		var payload []byte
		var digest string
		if rows.Scan(&ordinal, &payload, &digest) != nil || Hash(payload) != digest || commitment.Add(ordinal, payload) != nil {
			return ErrRecoveryStageData
		}
		if out != nil {
			if n, err := out.Write(payload); err != nil || n != len(payload) {
				return recoverySnapshotError(ctx, ErrRecoverySnapshotIO)
			}
		}
	}
	if rows.Err() != nil {
		return recoverySnapshotError(ctx, ErrRecoveryStageData)
	}
	digest, err := commitment.Digest()
	if err != nil || digest != spec.ChunksSHA256 {
		return ErrRecoveryStageData
	}
	return nil
}
