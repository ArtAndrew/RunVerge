// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
)

// validateRecoveryLineageEvidence checks retained snapshot bytes against their
// generation metadata. This does not re-authenticate past publisher signatures
// or turn provider observations into final billing evidence.
func validateRecoveryLineageEvidence(ctx context.Context, tx pgx.Tx, schema string, limits RecoverySnapshotLimits) error {
	if tx == nil || !limits.valid() {
		return ErrRecoveryStageData
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	var owned bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_namespace WHERE oid=pg_catalog.pg_my_temp_schema() AND nspname=$1)", schema).Scan(&owned); err != nil || !owned {
		return ErrRecoveryStageTarget
	}
	q := func(table string) string { return pgx.Identifier{schema, table}.Sanitize() }
	// The pinned layout permits at most one evidence batch per generation.
	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+q("recovery_evidence_batches")).Scan(&count); err != nil || count > 1 {
		return ErrRecoveryStageData
	}
	if count == 0 {
		return nil
	}
	var id, source, digest, sourceSchema string
	var size int64
	var chunks int
	var cut time.Time
	if err := tx.QueryRow(ctx, "SELECT id,source_id,evidence_snapshot_sha256,evidence_schema_sha256,evidence_cut_at,payload_bytes,chunk_count FROM "+q("recovery_evidence_batches")).Scan(&id, &source, &digest, &sourceSchema, &cut, &size, &chunks); err != nil || size < 1 || size > 384<<20 || chunks != int((size+(1<<20)-1)/(1<<20)) {
		return ErrRecoveryStageData
	}
	rows, err := tx.Query(ctx, "SELECT ordinal,payload,payload_sha256 FROM "+q("recovery_evidence_chunks")+" WHERE batch_id=$1 ORDER BY ordinal", id)
	if err != nil {
		return ErrRecoveryStageData
	}
	defer rows.Close()
	reader := &recoveryLineageEvidenceReader{rows: rows, total: size, chunks: chunks}
	report, err := ValidateRecoverySnapshot(ctx, reader, limits)
	if err != nil || !reader.done || reader.failure != nil || report.WireBytes != size || report.SHA256 != digest || report.Metadata.SourceID != source || report.Metadata.SchemaSHA256 != sourceSchema || report.Metadata.CutUTC != cut.UTC().Format(time.RFC3339Nano) {
		return recoverySnapshotError(ctx, ErrRecoveryStageData)
	}
	return nil
}

type recoveryLineageEvidenceReader struct {
	rows          pgx.Rows
	total         int64
	chunks, index int
	bytes         int64
	pending       []byte
	done          bool
	failure       error
}

func (r *recoveryLineageEvidenceReader) Read(out []byte) (int, error) {
	if len(out) == 0 {
		return 0, nil
	}
	if r.failure != nil {
		return 0, r.failure
	}
	if len(r.pending) > 0 {
		n := copy(out, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	if r.done {
		return 0, io.EOF
	}
	fail := func() (int, error) { r.failure = ErrRecoveryStageData; r.rows.Close(); return 0, r.failure }
	if !r.rows.Next() {
		if r.rows.Err() != nil || r.index != r.chunks || r.bytes != r.total {
			return fail()
		}
		r.done = true
		return 0, io.EOF
	}
	var ordinal int
	var payload []byte
	var digest string
	if r.rows.Scan(&ordinal, &payload, &digest) != nil || ordinal != r.index || r.index >= r.chunks || int64(len(payload)) != min(int64(1<<20), r.total-r.bytes) || Hash(payload) != digest {
		return fail()
	}
	r.index++
	r.bytes += int64(len(payload))
	r.pending = payload
	n := copy(out, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
