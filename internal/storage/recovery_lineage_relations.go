// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

// validateRecoveryLineageRelations checks generation-local references in
// exclusively owned typed staging. The caller must first pin the schema and
// enforce row constraints/digests. Business-row references, evidence payload
// verification and accounting projection are separate mandatory import gates.
// Success grants no current authority and writes no permanent state.
func validateRecoveryLineageRelations(ctx context.Context, tx pgx.Tx, schema string, expected recoveryLineageGenerationStart) error {
	if tx == nil {
		return ErrRecoveryStageTarget
	}
	var owned bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_namespace WHERE oid=pg_catalog.pg_my_temp_schema() AND nspname=$1)`, schema).Scan(&owned); err != nil || !owned {
		return ErrRecoveryStageTarget
	}
	q := pgx.Identifier{schema}.Sanitize()
	query := func(sql string, args ...any) error {
		var valid bool
		if err := tx.QueryRow(ctx, strings.ReplaceAll(sql, "$S$", q), args...).Scan(&valid); err != nil {
			return recoverySnapshotError(ctx, ErrRecoveryStageData)
		}
		if !valid {
			return ErrRecoveryStageData
		}
		return nil
	}
	if err := query(`SELECT count(*)=1 AND COALESCE(bool_and(target_installation_id=$1 AND restore_id=$2 AND source_id=$3 AND snapshot_sha256=$4),false) FROM $S$.recovery_import_receipts`, expected.InstallationID, expected.RestoreID, expected.ImportedSourceID, expected.ImportedSnapshotSHA256); err != nil {
		return err
	}
	// LIKE does not copy foreign keys; all metadata-local references are explicit.
	for _, sql := range []string{
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_historical_rows h LEFT JOIN $S$.recovery_import_receipts r USING(restore_id) WHERE r.restore_id IS NULL)`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_evidence_batches e LEFT JOIN $S$.recovery_import_receipts r USING(restore_id) WHERE r.restore_id IS NULL OR e.target_installation_id<>r.target_installation_id OR e.source_id<>r.source_id OR e.selected_snapshot_sha256<>r.snapshot_sha256 OR e.evidence_cut_at<=r.cut_at)`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_evidence_chunks c LEFT JOIN $S$.recovery_evidence_batches e ON e.id=c.batch_id WHERE e.id IS NULL OR c.ordinal>=e.chunk_count OR octet_length(c.payload)<>LEAST(1048576,e.payload_bytes-c.ordinal::bigint*1048576))`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_evidence_changes c LEFT JOIN $S$.recovery_evidence_batches e ON e.id=c.batch_id WHERE e.id IS NULL OR c.ordinal>=e.changed_rows)`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_evidence_batches e WHERE (SELECT count(*) FROM $S$.recovery_evidence_chunks c WHERE c.batch_id=e.id)<>e.chunk_count OR (SELECT COALESCE(sum(octet_length(c.payload)),0) FROM $S$.recovery_evidence_chunks c WHERE c.batch_id=e.id)<>e.payload_bytes OR (SELECT count(*) FROM $S$.recovery_evidence_changes c WHERE c.batch_id=e.id)<>e.changed_rows)`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_accounting_batches b LEFT JOIN $S$.recovery_import_receipts r USING(restore_id) LEFT JOIN $S$.recovery_evidence_batches e ON e.id=b.evidence_batch_id LEFT JOIN $S$.recovery_accounting_batches p ON p.id=b.previous_id WHERE r.restore_id IS NULL OR b.target_installation_id<>r.target_installation_id OR e.id IS NULL OR e.restore_id<>b.restore_id OR (b.ordinal>1 AND (p.id IS NULL OR p.restore_id<>b.restore_id OR p.ordinal<>b.ordinal-1)))`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_accounting_mutations m LEFT JOIN $S$.recovery_accounting_batches b ON b.id=m.batch_id WHERE b.id IS NULL OR m.ordinal>=b.mutation_count)`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_accounting_applied a LEFT JOIN $S$.recovery_accounting_mutations m USING(batch_id,ordinal) WHERE m.batch_id IS NULL OR a.before_sha256 IS DISTINCT FROM CASE WHEN m.before_row IS NULL THEN NULL ELSE encode(sha256(convert_to(m.before_row::text,'UTF8')),'hex') END OR a.after_sha256 IS DISTINCT FROM encode(sha256(convert_to(m.after_row::text,'UTF8')),'hex'))`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_accounting_batches b WHERE (SELECT count(*) FROM $S$.recovery_accounting_mutations m WHERE m.batch_id=b.id)<>b.mutation_count OR (SELECT count(*) FROM $S$.recovery_accounting_applied a WHERE a.batch_id=b.id)<>b.mutation_count OR (SELECT COALESCE(sum(octet_length(COALESCE(m.before_row::text,''))+octet_length(m.after_row::text)),0) FROM $S$.recovery_accounting_mutations m WHERE m.batch_id=b.id)<>b.mutation_bytes)`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_usage_observations u LEFT JOIN $S$.recovery_import_receipts r USING(restore_id) LEFT JOIN $S$.recovery_evidence_batches e ON e.id=u.evidence_batch_id WHERE r.restore_id IS NULL OR u.target_installation_id<>r.target_installation_id OR e.id IS NULL OR e.restore_id<>u.restore_id)`,
	} {
		if err := query(sql); err != nil {
			return err
		}
	}
	return nil
}
