// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// This verifies the unapplied SQL027 storage/transaction guards only. The
// synthetic receipt has no business import: this is not full restore acceptance.
func TestRecoveryLineagePersistenceCandidate(t *testing.T) {
	guardFailure := func(t *testing.T, err error) {
		t.Helper()
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "55000" {
			t.Fatal("expected permanent/transaction guard SQLSTATE 55000", err)
		}
	}
	s, _ := independentRecoveryStore(t)
	ctx := context.Background()
	spec := independentRecoverySpec(t, s)
	if _, err := s.PrepareRecoveryTarget(ctx, spec); err != nil {
		t.Fatal(err)
	}
	ddl, err := os.ReadFile("testdata/recovery-lineage-candidate.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, string(ddl)); err != nil {
		t.Fatal("candidate schema failed", err)
	}
	persistence := recoveryLineagePersistence{RestoreID: spec.RestoreID, TargetInstallationID: spec.TargetInstallationID, SnapshotSHA256: strings.Repeat("a", 64), Generations: 1, PayloadBytes: 3, ChunksSHA256: "6ae408386a61f69681602a20e7ea6f4916caffd320353b0477e11293fdc7438c"}
	start := func(t *testing.T) pgx.Tx {
		t.Helper()
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(ctx) })
		_, err = tx.Exec(ctx, `INSERT INTO recovery_import_receipts(profile,target_installation_id,restore_id,backup_id,source_id,cut_at,snapshot_sha256,schema_sha256,inventory_sha256,catalog_sha256,tables,sequences,cost_history,history_coverage)
 VALUES('pg18-held-import-v1',$1,$2,$3,$4,$5,$6,$7,$8,$9,'[]','[]','{}','{}')`, spec.TargetInstallationID, spec.RestoreID, spec.BackupID, spec.SourceID, spec.CutUTC, persistence.SnapshotSHA256, spec.SchemaSHA256, recoveryLineageSQL026Inventory, recoveryLineageSQL026Catalog)
		if err != nil {
			t.Fatal("synthetic current transaction receipt", err)
		}
		return tx
	}
	for name, payload := range map[string]string{"truncated": "ab", "extra": "abcd", "changed": "abd"} {
		t.Run(name, func(t *testing.T) {
			tx := start(t)
			if persistRecoveryLineageTx(ctx, tx, s.runtimeSchema, persistence, strings.NewReader(payload)) == nil {
				t.Fatal("invalid payload persisted")
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("incomplete_commit", func(t *testing.T) {
		tx := start(t)
		if _, err := tx.Exec(ctx, `INSERT INTO recovery_lineage_imports(restore_id,target_installation_id,snapshot_sha256,wire_profile,attestation,generation_count,payload_bytes,chunk_count,chunks_sha256) VALUES($1,$2,$3,'recovery-lineage-v1','current-backup-attested-history',1,3,1,$4)`, spec.RestoreID, spec.TargetInstallationID, persistence.SnapshotSHA256, persistence.ChunksSHA256); err != nil {
			t.Fatal(err)
		}
		guardFailure(t, tx.Commit(ctx))
	})
	var count int
	if err = s.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM recovery_import_receipts)+(SELECT count(*) FROM recovery_lineage_imports)+(SELECT count(*) FROM recovery_lineage_chunks)").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed candidate transaction leaked rows", err)
	}
	tx := start(t)
	// Match the real business import, which flushes all constraints first.
	if _, err = tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if err = persistRecoveryLineageTx(ctx, tx, s.runtimeSchema, persistence, bytes.NewReader([]byte("abc"))); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
		t.Fatal("completed lineage constraint flush failed", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal("complete history commit failed", err)
	}
	inspect, err := s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	if err = inspectRecoveryLineageTx(ctx, inspect, s.runtimeSchema, persistence); err != nil {
		t.Fatal("fresh read-only inspection failed", err)
	}
	if err = inspect.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for name, sql := range map[string]string{"update": "UPDATE recovery_lineage_chunks SET payload=payload", "delete": "DELETE FROM recovery_lineage_chunks", "truncate": "TRUNCATE recovery_lineage_chunks"} {
		t.Run(name, func(t *testing.T) {
			_, err := s.Pool.Exec(ctx, sql)
			guardFailure(t, err)
		})
	}
	late, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer late.Rollback(ctx)
	if err = persistRecoveryLineageTx(ctx, late, s.runtimeSchema, persistence, strings.NewReader("abc")); err == nil {
		t.Fatal("later transaction replay admitted")
	}
	if err = late.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// Verify that refusal comes from the transaction guard, not duplicate PK.
	_, err = s.Pool.Exec(ctx, `INSERT INTO recovery_lineage_imports(restore_id,target_installation_id,snapshot_sha256,wire_profile,attestation,generation_count,payload_bytes,chunk_count,chunks_sha256) VALUES($1,$2,$3,'recovery-lineage-v1','current-backup-attested-history',1,3,1,$4)`, spec.RestoreID, spec.TargetInstallationID, persistence.SnapshotSHA256, persistence.ChunksSHA256)
	guardFailure(t, err)

}
