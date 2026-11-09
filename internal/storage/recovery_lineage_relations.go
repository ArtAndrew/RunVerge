// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"strings"

	"agentplatform.local/platform/internal/recoveryarchive"
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
	admitted := expected.Profile == recoveryLineageAdmittedProfile
	finance := admitted || expected.Profile == "sql030-current-generation-v1" || expected.Profile == "sql031-current-generation-v1"
	if err := query(`SELECT count(*)<=1 AND COALESCE(bool_and($1 AND e.evidence_cut_at=r.cut_at AND
 e.evidence_snapshot_sha256=r.snapshot_sha256 AND e.evidence_schema_sha256=r.schema_sha256 AND
 e.report->>'profile' IS NOT DISTINCT FROM 'retired-handoff-usage-anchor-v1' AND
 COALESCE(e.report->>'activation_id' ~ '^activate_[a-f0-9]{32}$',false) AND
 NOT EXISTS(SELECT 1 FROM unnest(ARRAY['import_receipt_sha256','review_sha256','source_stop_sha256','retirement_seal_sha256']) keys(k)
 WHERE (e.report->>k ~ '^[a-f0-9]{64}$') IS DISTINCT FROM true) AND
 l.capture->'version' IS NOT DISTINCT FROM '2'::jsonb AND l.capture->>'operation_id' IS NOT DISTINCT FROM e.id AND
 (l.capture->>'selected_snapshot_sha256' ~ '^[a-f0-9]{64}$') IS NOT DISTINCT FROM true AND
 (l.capture->>'selected_receipt_sha256' ~ '^[a-f0-9]{64}$') IS NOT DISTINCT FROM true AND
 l.capture->>'source_id' IS NOT DISTINCT FROM r.source_id AND l.capture->>'evidence_backup_id' IS NOT DISTINCT FROM r.backup_id AND
 l.capture->>'evidence_snapshot_sha256' IS NOT DISTINCT FROM r.snapshot_sha256 AND
 l.capture->>'evidence_receipt_sha256' IS NOT DISTINCT FROM e.evidence_receipt_sha256 AND
 l.capture->>'evidence_schema_sha256' IS NOT DISTINCT FROM e.evidence_schema_sha256 AND
 (l.capture->>'evidence_cut_utc')::timestamptz IS NOT DISTINCT FROM r.cut_at),true)
 FROM $S$.recovery_evidence_batches e JOIN $S$.recovery_import_receipts r USING(restore_id)
 CROSS JOIN LATERAL (SELECT convert_from(e.capture_link,'UTF8')::jsonb->'link' AS capture) l WHERE e.payload_bytes=0`,
		expected.Profile == "sql029-current-generation-v1" || finance); err != nil {
		return err
	}
	// LIKE does not copy foreign keys; all metadata-local references are explicit.
	for _, sql := range []string{
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_historical_rows h LEFT JOIN $S$.recovery_import_receipts r USING(restore_id) WHERE r.restore_id IS NULL)`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_evidence_batches e LEFT JOIN $S$.recovery_import_receipts r USING(restore_id) WHERE r.restore_id IS NULL OR e.target_installation_id<>r.target_installation_id OR e.source_id<>r.source_id OR e.selected_snapshot_sha256<>r.snapshot_sha256 OR (e.payload_bytes>0 AND e.evidence_cut_at<=r.cut_at))`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_evidence_chunks c LEFT JOIN $S$.recovery_evidence_batches e ON e.id=c.batch_id WHERE e.id IS NULL OR c.ordinal>=e.chunk_count OR octet_length(c.payload)<>LEAST(1048576,e.payload_bytes-c.ordinal::bigint*1048576))`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_evidence_changes c LEFT JOIN $S$.recovery_evidence_batches e ON e.id=c.batch_id WHERE e.id IS NULL OR c.ordinal>=e.changed_rows)`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_evidence_batches e WHERE (SELECT count(*) FROM $S$.recovery_evidence_chunks c WHERE c.batch_id=e.id)<>e.chunk_count OR (SELECT COALESCE(sum(octet_length(c.payload)),0) FROM $S$.recovery_evidence_chunks c WHERE c.batch_id=e.id)<>e.payload_bytes OR (SELECT count(*) FROM $S$.recovery_evidence_changes c WHERE c.batch_id=e.id)<>e.changed_rows)`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_accounting_batches b LEFT JOIN $S$.recovery_import_receipts r USING(restore_id) LEFT JOIN $S$.recovery_evidence_batches e ON e.id=b.evidence_batch_id LEFT JOIN $S$.recovery_accounting_batches p ON p.id=b.previous_id WHERE r.restore_id IS NULL OR b.target_installation_id<>r.target_installation_id OR e.id IS NULL OR e.restore_id<>b.restore_id OR (b.ordinal>1 AND (p.id IS NULL OR p.restore_id<>b.restore_id OR p.ordinal<>b.ordinal-1)))`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_accounting_mutations m LEFT JOIN $S$.recovery_accounting_batches b ON b.id=m.batch_id WHERE b.id IS NULL OR m.ordinal>=b.mutation_count)`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_accounting_applied a LEFT JOIN $S$.recovery_accounting_mutations m USING(batch_id,ordinal) WHERE m.batch_id IS NULL OR a.before_sha256 IS DISTINCT FROM CASE WHEN m.before_row IS NULL THEN NULL ELSE encode(sha256(convert_to(m.before_row::text,'UTF8')),'hex') END OR a.after_sha256 IS DISTINCT FROM encode(sha256(convert_to(m.after_row::text,'UTF8')),'hex'))`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_accounting_batches b WHERE (SELECT count(*) FROM $S$.recovery_accounting_mutations m WHERE m.batch_id=b.id)<>b.mutation_count OR (SELECT count(*) FROM $S$.recovery_accounting_applied a WHERE a.batch_id=b.id)<>b.mutation_count OR (SELECT COALESCE(sum(octet_length(COALESCE(m.before_row::text,''))+octet_length(m.after_row::text)),0) FROM $S$.recovery_accounting_mutations m WHERE m.batch_id=b.id)<>b.mutation_bytes)`,
		// Bind declared keys to row images before grouping history by key.
		// JSONB string checks prevent numeric/object aliases becoming text IDs.
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_accounting_mutations m JOIN $S$.recovery_accounting_batches b ON b.id=m.batch_id
 CROSS JOIN LATERAL (SELECT CASE WHEN m.table_name='organizations' THEN ARRAY['id'] WHEN m.table_name='cost_evidence' THEN ARRAY['organization_id','source','reference'] ELSE ARRAY['organization_id','id'] END AS names) fields
 WHERE EXISTS(SELECT 1 FROM unnest(fields.names) AS field(name) WHERE jsonb_typeof(m.after_row->name) IS DISTINCT FROM 'string' OR (m.before_row IS NOT NULL AND (jsonb_typeof(m.before_row->name) IS DISTINCT FROM 'string' OR m.before_row->name IS DISTINCT FROM m.after_row->name)))
 OR m.key_parts IS DISTINCT FROM (SELECT jsonb_agg(m.after_row->name ORDER BY ordinal) FROM unnest(fields.names) WITH ORDINALITY AS keys(name,ordinal))
 OR (CASE WHEN m.table_name='organizations' THEN m.after_row->>'id' ELSE m.after_row->>'organization_id' END) IS DISTINCT FROM b.organization_id)`,
		`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_usage_observations u LEFT JOIN $S$.recovery_import_receipts r USING(restore_id) LEFT JOIN $S$.recovery_evidence_batches e ON e.id=u.evidence_batch_id WHERE r.restore_id IS NULL OR u.target_installation_id<>r.target_installation_id OR e.id IS NULL OR e.restore_id<>u.restore_id)`,
	} {
		if err := query(sql); err != nil {
			return err
		}
	}
	var admission recoveryAccountingAdmission
	if admitted {
		var err error
		admission, err = recoveryLineageAdmissionTx(ctx, tx, schema, expected)
		if err != nil {
			return err
		}
		if err := query(`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_accounting_mutations m WHERE m.table_name='organizations'
 AND (m.operation<>'UPDATE' OR NOT ` + recoveryLineageOrganizationChange("m.before_row", "m.after_row") + "))"); err != nil {
			return err
		}
	}
	// Keep exact continuity for historical rows. After an explicit admission,
	// ordinary work may change the organization's shared totals between two
	// historical postings. The business snapshot checks its ledger separately.
	if err := query(`SELECT NOT EXISTS(SELECT 1 FROM (
 SELECT m.table_name,m.operation,m.before_row,b.organization_id,b.ordinal,
 lag(m.after_row) OVER history AS previous_after,row_number() OVER history AS position
 FROM $S$.recovery_accounting_mutations m JOIN $S$.recovery_accounting_batches b ON b.id=m.batch_id
 WINDOW history AS (PARTITION BY m.table_name,m.key_parts ORDER BY b.ordinal,m.ordinal)) chain
 WHERE position>1 AND (operation<>'UPDATE' OR (before_row IS DISTINCT FROM previous_after AND NOT (
 table_name='organizations' AND ordinal>$1 AND COALESCE(organization_id=ANY($2::text[]),false)
 AND `+recoveryLineageOrganizationChange("chain.previous_after", "chain.before_row")+"))))",
		admission.headOrdinal, admission.organizations); err != nil {
		return err
	}
	if finance {
		if err := query(`SELECT NOT EXISTS(SELECT 1 FROM $S$.recovery_accounting_batches b
 JOIN $S$.recovery_evidence_batches e ON e.id=b.evidence_batch_id
 LEFT JOIN $S$.recovery_finance_statements f ON f.batch_id=b.id
 WHERE (e.payload_bytes>0 AND f.batch_id IS NOT NULL) OR ($1 AND e.payload_bytes=0 AND f.batch_id IS NULL))`,
			expected.Profile == "sql030-current-generation-v1"); err != nil {
			return err
		}
		if expected.Profile != "sql030-current-generation-v1" {
			if err := validateRecoveryFinanceResolutions(ctx, tx, q, ""); err != nil {
				return err
			}
		}
		return validateRecoveryFinanceLineage(ctx, tx, q)
	}
	return nil
}

func validateRecoveryFinanceLineage(ctx context.Context, tx pgx.Tx, quotedSchema string) error {
	rows, err := tx.Query(ctx, strings.ReplaceAll(`SELECT f.batch_id,f.usage_id,f.statement_sha256,f.envelope,f.public_key,
 b.target_installation_id,b.restore_id,b.organization_id,b.expected_revision,b.request_sha256,b.plan_sha256,
 u.source_plan_sha256,u.acquisition_sha256,e.report->>'activation_id',
 convert_from(u.source_plan_commitment,'UTF8')::jsonb->>'archived_provider_account',
 (SELECT count(*) FROM $S$.recovery_accounting_mutations m WHERE m.batch_id=b.id AND m.table_name='cost_evidence')=1
 AND EXISTS(SELECT 1 FROM $S$.recovery_accounting_mutations m WHERE m.batch_id=b.id AND m.table_name='cost_evidence' AND m.operation='INSERT'
  AND m.after_row->>'payload_digest'=f.statement_sha256 AND m.after_row->>'request_hash'=b.request_sha256)
 AND NOT EXISTS(SELECT 1 FROM $S$.recovery_accounting_mutations m WHERE m.batch_id=b.id AND m.table_name='cost_quarantine_resolutions')
 FROM $S$.recovery_finance_statements f
 LEFT JOIN $S$.recovery_accounting_batches b ON b.id=f.batch_id
 LEFT JOIN $S$.recovery_usage_observations u ON u.id=f.usage_id AND u.evidence_batch_id=b.evidence_batch_id
  AND u.restore_id=b.restore_id AND u.target_installation_id=b.target_installation_id
  AND u.organization_id=b.organization_id AND u.reservation_id=b.reservation_id
 LEFT JOIN $S$.recovery_evidence_batches e ON e.id=b.evidence_batch_id AND e.payload_bytes=0
 LIMIT 10001`, "$S$", quotedSchema))
	if err != nil {
		return ErrRecoveryStageData
	}
	defer rows.Close()
	for count := 1; rows.Next(); count++ {
		var batch, usage, sha, request, commitment, plan, acquisition string
		var scope recoveryarchive.FinanceScope
		var envelope, key []byte
		var revision int
		var mutation bool
		if count > 10000 || rows.Scan(&batch, &usage, &sha, &envelope, &key, &scope.TargetID, &scope.RestoreID, &scope.OrganizationID,
			&revision, &request, &commitment, &plan, &acquisition, &scope.ActivationID, &scope.Account, &mutation) != nil || !mutation {
			return ErrRecoveryStageData
		}
		st, ok := recoveryarchive.RetainedFinanceStatement(envelope, key)
		if !ok || Hash(envelope) != sha || st.Scope != scope || st.ObservationID != usage || st.AcquisitionSHA != acquisition || st.ExpectedRevision != revision {
			return ErrRecoveryStageData
		}
		id, fact, correction := recoveryFinanceEvidence(st, sha)
		if id[:32] != batch || evidenceRequestHash(fact, correction) != request || recoveryFinanceCommitment(sha, Hash(key), plan, acquisition) != commitment {
			return ErrRecoveryStageData
		}
	}
	if rows.Err() != nil {
		return ErrRecoveryStageData
	}
	return nil
}
