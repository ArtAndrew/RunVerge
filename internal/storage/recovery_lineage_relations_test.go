// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

func TestRecoveryLineageRelationsActualEvidence(t *testing.T) {
	f := evidenceAuthorSetup(t)
	ctx := context.Background()
	if _, err := f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	tx, err := f.target.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	receipt, err := readRecoveryImportReceipt(ctx, tx, f.target.runtimeSchema)
	if err != nil {
		t.Fatal(err)
	}
	expected := recoveryLineageGenerationStart{Kind: "generation", Profile: "sql026-current-generation-v1", InventorySHA256: recoveryLineageSQL026Inventory, CatalogSHA256: recoveryLineageSQL026Catalog, InstallationID: receipt.TargetInstallationID, RestoreID: receipt.RestoreID, ImportedSourceID: receipt.SourceID, ImportedSnapshotSHA256: receipt.SnapshotSHA256}
	if err := validateRecoveryLineageRelations(ctx, tx, f.target.runtimeSchema, expected); err == nil {
		t.Fatal("permanent schema accepted as staging")
	}
	inventory, err := recoverySnapshotInventory(ctx, tx, f.target.runtimeSchema, true)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := recoveryLineageSourceInventory(inventory)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range projected.Tables {
		target := pgx.Identifier{"pg_temp", table.Name}.Sanitize()
		source := pgx.Identifier{f.target.runtimeSchema, table.Name}.Sanitize()
		if _, err = tx.Exec(ctx, "CREATE TEMP TABLE "+target+" (LIKE "+source+" INCLUDING CONSTRAINTS INCLUDING INDEXES EXCLUDING DEFAULTS) ON COMMIT DROP"); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO "+target+" SELECT * FROM "+source); err != nil {
			t.Fatal(err)
		}
	}
	var schema string
	if err = tx.QueryRow(ctx, "SELECT nspname FROM pg_catalog.pg_namespace WHERE oid=pg_catalog.pg_my_temp_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	// Synthetic metadata-only fixtures exercise nonempty accounting and usage
	// links. They deliberately do not claim valid business references or a
	// verified external invoice; those belong to later mandatory gates.
	if _, err = tx.Exec(ctx, `
INSERT INTO pg_temp.recovery_accounting_batches
(id,restore_id,target_installation_id,evidence_batch_id,organization_id,reservation_id,expected_revision,ordinal,previous_id,request_sha256,plan_sha256,mutation_count,mutation_bytes,mutation_profile,creating_transaction,operator_role,recorded_at)
SELECT repeat('a',32),restore_id,target_installation_id,id,'synthetic-org','synthetic-reservation',1,1,NULL,repeat('a',64),repeat('b',64),1,octet_length('{"synthetic": true}'::jsonb::text),'sql026-exact-mutation-v1',pg_current_xact_id(),session_user,clock_timestamp() FROM pg_temp.recovery_evidence_batches;
INSERT INTO pg_temp.recovery_accounting_mutations VALUES(repeat('a',32),0,'organizations','INSERT','["synthetic-org"]',NULL,'{"synthetic":true}');
INSERT INTO pg_temp.recovery_accounting_applied SELECT batch_id,ordinal,NULL,encode(sha256(convert_to(after_row::text,'UTF8')),'hex') FROM pg_temp.recovery_accounting_mutations;
INSERT INTO pg_temp.recovery_usage_observations
(id,restore_id,target_installation_id,evidence_batch_id,organization_id,reservation_id,source_plan_sha256,source_plan_commitment,acquisition_sha256,acquisition_commitment,response_sha256,response,admission_sha256,admission_commitment,observation_state,creating_transaction,operator_role,recorded_at)
SELECT repeat('b',32),restore_id,target_installation_id,id,'synthetic-org','synthetic-reservation',encode(sha256('x'::bytea),'hex'),'x'::bytea,encode(sha256('x'::bytea),'hex'),'x'::bytea,encode(sha256('x'::bytea),'hex'),'x'::bytea,encode(sha256('x'::bytea),'hex'),'x'::bytea,'observed_unverified',pg_current_xact_id(),session_user,clock_timestamp() FROM pg_temp.recovery_evidence_batches;
`); err != nil {
		t.Fatal("synthetic nonempty metadata fixture", err)
	}
	if err = validateRecoveryLineageRelations(ctx, tx, schema, expected); err != nil {
		t.Fatal("actual history relations rejected", err)
	}
	bad := expected
	bad.RestoreID = domain.NewID("restore")
	if validateRecoveryLineageRelations(ctx, tx, schema, bad) == nil {
		t.Fatal("different receipt accepted")
	}
	cases := map[string]string{
		"nonlater_evidence":         "UPDATE pg_temp.recovery_import_receipts SET cut_at=(SELECT evidence_cut_at FROM pg_temp.recovery_evidence_batches)",
		"missing_accounting_parent": "UPDATE pg_temp.recovery_accounting_batches SET ordinal=2,previous_id=repeat('c',32)",
		"wrong_applied_hash":        "UPDATE pg_temp.recovery_accounting_applied SET after_sha256=repeat('c',64)",
		"missing_applied":           "DELETE FROM pg_temp.recovery_accounting_applied",
		"wrong_usage_evidence":      "UPDATE pg_temp.recovery_usage_observations SET evidence_batch_id=repeat('c',32)",
		"missing_receipt":           "DELETE FROM pg_temp.recovery_import_receipts",
		"orphan_history":            "UPDATE pg_temp.recovery_historical_rows SET restore_id='restore_00000000000000000000000000000000'",
		"orphan_chunk":              "UPDATE pg_temp.recovery_evidence_chunks SET batch_id='00000000000000000000000000000000'",
		"incomplete_chunks":         "DELETE FROM pg_temp.recovery_evidence_chunks WHERE ordinal=0",
		"wrong_evidence_source":     "UPDATE pg_temp.recovery_evidence_batches SET source_id='inst_00000000000000000000000000000000'",
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := tx.Exec(ctx, "SAVEPOINT corrupt_generation"); err != nil {
				t.Fatal(err)
			}
			tag, err := tx.Exec(ctx, sql)
			if err != nil || tag.RowsAffected() == 0 {
				t.Fatal("corruption fixture did not change rows", err)
			}
			if validateRecoveryLineageRelations(ctx, tx, schema, expected) == nil {
				t.Fatal("broken generation reference accepted")
			}
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT corrupt_generation"); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT corrupt_generation"); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err = validateRecoveryLineageRelations(ctx, tx, schema, expected); err != nil {
		t.Fatal("rollback did not preserve original history", err)
	}
	// Exercise PostgreSQL binary receive/send of nonempty name/xid8 values,
	// using the explicit synthetic metadata above, not current authority.
	limits := DefaultRecoverySnapshotLimits()
	counts, err := recoverySnapshotPreflight(ctx, tx, schema, projected, limits)
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	writer := &recoverySnapshotWriter{ctx: ctx, out: &wire, hash: sha256.New(), limit: limits.MaxWireBytes}
	var exported RecoverySnapshotReport
	if err = writer.record(expected); err != nil {
		t.Fatal(err)
	}
	if err = writeRecoverySnapshotTables(ctx, tx, schema, projected, counts, writer, limits, &exported); err != nil {
		t.Fatal(err)
	}
	if err = writer.record(recoveryLineageGenerationEnd{Kind: "generation_end", Tables: len(exported.Tables), Rows: exported.Rows, Bytes: exported.Bytes}); err != nil {
		t.Fatal(err)
	}
	stageTx, err := f.target.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stageTx.Rollback(ctx)
	staged, err := stageRecoveryLineageGeneration(ctx, stageTx, f.target.runtimeSchema, bytes.NewReader(wire.Bytes()), inventory, expected, limits)
	if err != nil || staged.Rows != exported.Rows || staged.Bytes != exported.Bytes {
		t.Fatal("nonempty metadata PostgreSQL staging failed", err)
	}
	var stagedSchema string
	if err = stageTx.QueryRow(ctx, "SELECT nspname FROM pg_catalog.pg_namespace WHERE oid=pg_catalog.pg_my_temp_schema()").Scan(&stagedSchema); err != nil {
		t.Fatal(err)
	}
	if _, err = stageTx.Exec(ctx, "SAVEPOINT missing_marker"); err != nil {
		t.Fatal(err)
	}
	tag, err := stageTx.Exec(ctx, "DELETE FROM pg_temp.recovery_historical_rows WHERE ctid IN(SELECT ctid FROM pg_temp.recovery_historical_rows LIMIT 1)")
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatal("marker corruption fixture", err)
	}
	if validateRecoveryLineageReceipt(ctx, stageTx, stagedSchema, inventory, limits) == nil {
		t.Fatal("missing historical row marker accepted")
	}
	if _, err = stageTx.Exec(ctx, "ROLLBACK TO SAVEPOINT missing_marker"); err != nil {
		t.Fatal(err)
	}
	if err = validateRecoveryLineageReceipt(ctx, stageTx, stagedSchema, inventory, limits); err != nil {
		t.Fatal("original markers rejected", err)
	}

}
