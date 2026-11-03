// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/domain"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestRecoveryLineageSourceInventory(t *testing.T) {
	f := independentRecoveryFixtureNew(t)
	projected, err := recoveryLineageSourceInventory(f.header.Inventory)
	if err != nil || len(projected.Tables) != 9 || len(projected.Sequences) != 0 {
		t.Fatal("trusted history projection failed", err)
	}
	for _, table := range projected.Tables {
		if !table.Exported || table.Name == "runtime_recovery_control" || table.Name == "access_tokens" {
			t.Fatal("authority in history projection")
		}
	}
	// A caller cannot inject a table even with a syntactically safe identifier.
	altered := f.header.Inventory
	altered.Tables = append([]RecoverySnapshotTable{}, altered.Tables...)
	altered.Tables[0].Name = "arbitrary_table"
	if _, err = recoveryLineageSourceInventory(altered); err == nil {
		t.Fatal("untrusted catalog accepted")
	}
	if _, err = recoveryLineageSourceInventory(f.header.Inventory); err != nil {
		t.Fatal("projection modified source inventory", err)
	}
}

func TestRecoveryLineageSourceAuthorActualImportedGeneration(t *testing.T) {
	f := independentImportFixture(t)
	ctx := context.Background()
	limits := DefaultRecoverySnapshotLimits()
	for _, options := range []pgx.TxOptions{{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly}, {IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadWrite}} {
		tx, err := f.target.Pool.BeginTx(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		w := &recoverySnapshotWriter{ctx: ctx, out: &out, hash: sha256.New(), limit: limits.MaxWireBytes}
		report := RecoverySnapshotReport{}
		err = writeRecoveryLineageGenerationTx(ctx, tx, f.target.runtimeSchema, w, limits, &report)
		_ = tx.Rollback(ctx)
		if err == nil || out.Len() != 0 {
			t.Fatal("non-read-only RR export admitted")
		}
	}
	tx, err := f.target.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var out bytes.Buffer
	w := &recoverySnapshotWriter{ctx: ctx, out: &out, hash: sha256.New(), limit: limits.MaxWireBytes}
	report := RecoverySnapshotReport{}
	if err = writeRecoveryLineageGenerationTx(ctx, tx, f.target.runtimeSchema, w, limits, &report); err != nil {
		t.Fatal("actual generation export failed", err)
	}
	if len(report.Tables) != 9 || report.Rows < 2 || report.Bytes < 1 || !bytes.Contains(out.Bytes(), []byte(`"kind":"generation_end"`)) {
		t.Fatal("incomplete generation export")
	}
	// Validate the actual database-produced generation using the independently
	// trusted inventory and identity, without ending the caller transaction.
	var header recoveryLineageGenerationStart
	if err = json.Unmarshal(bytes.SplitN(out.Bytes(), []byte("\n"), 2)[0], &header); err != nil {
		t.Fatal(err)
	}
	if header.InstallationID != f.held.InstallationID || header.RestoreID != f.held.RestoreID || header.ImportedSourceID != f.receipt.SourceID || header.ImportedSnapshotSHA256 != f.receipt.SnapshotSHA256 {
		t.Fatal("exported generation identity mismatch")
	}
	reader := &recoverySnapshotReader{ctx: ctx, reader: bufio.NewReader(bytes.NewReader(out.Bytes())), hash: sha256.New(), limit: limits.MaxWireBytes}
	var parsed RecoverySnapshotReport
	if err = readRecoveryLineageGeneration(reader, f.inventory, header, limits, &parsed); err != nil || parsed.Rows != report.Rows || parsed.Bytes != report.Bytes {
		t.Fatal("actual generation layout rejected", err)
	}
	if _, err = reader.reader.Peek(1); err != io.EOF {
		t.Fatal("unexpected generation trailing bytes", err)
	}
	var count int
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM recovery_import_receipts").Scan(&count); err != nil || count != 1 {
		t.Fatal("export ended caller transaction", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := f.target.InspectRecoveryTarget(ctx)
	if err != nil || after != f.held {
		t.Fatal("read-only history export changed hold", err)
	}
	independentImportAssertRows(t, f.rows, independentImportRows(t, f.target, f.inventory, false))
}

func TestRecoveryLineageSourceAuthorFullEvidenceChunk(t *testing.T) {
	ctx := context.Background()
	f := evidenceAuthorSetupWithMutation(t, func(source *Store) {
		p, _ := identity(t, source, 1_000_000)
		a := agent(t, source, p)
		task := task(t, source, p, a)
		for i := 0; i < 3; i++ {
			if _, err := source.CreateMaterial(ctx, p, task.ID, domain.NewID("key"), domain.CreateMaterial{Name: "Synthetic large retained evidence", MediaType: "text/plain", Content: strings.Repeat("x", domain.MaxMaterialContentBytes)}); err != nil {
				t.Fatal(err)
			}
		}
	})
	if _, err := f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal("actual evidence ingestion failed", err)
	}
	var fullChunks int
	if err := f.target.Pool.QueryRow(ctx, "SELECT count(*) FROM recovery_evidence_chunks WHERE octet_length(payload)=1048576").Scan(&fullChunks); err != nil || fullChunks == 0 {
		t.Fatal("fixture lacks a full size evidence chunk", err)
	}
	tx, err := f.target.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	limits := DefaultRecoverySnapshotLimits()
	var out bytes.Buffer
	w := &recoverySnapshotWriter{ctx: ctx, out: &out, hash: sha256.New(), limit: limits.MaxWireBytes}
	report := RecoverySnapshotReport{}
	if err = writeRecoveryLineageGenerationTx(ctx, tx, f.target.runtimeSchema, w, limits, &report); err != nil {
		t.Fatal("full chunk plus metadata rejected", err)
	}
	found := false
	for _, table := range report.Tables {
		if table.Name == "recovery_evidence_chunks" && table.Bytes > 1<<20 {
			found = true
		}
	}
	if !found || limits.MaxRowBytes != 1<<20 {
		t.Fatal("metadata missing or business limit changed")
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	stageTx, err := f.target.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stageTx.Rollback(ctx)
	inventory, err := recoverySnapshotInventory(ctx, stageTx, f.target.runtimeSchema, true)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := readRecoveryImportReceipt(ctx, stageTx, f.target.runtimeSchema)
	if err != nil {
		t.Fatal(err)
	}
	expected := recoveryLineageGenerationStart{Kind: "generation", Profile: "sql026-current-generation-v1", InstallationID: receipt.TargetInstallationID, RestoreID: receipt.RestoreID, ImportedSourceID: receipt.SourceID, ImportedSnapshotSHA256: receipt.SnapshotSHA256, InventorySHA256: recoveryLineageSQL026Inventory, CatalogSHA256: recoveryLineageSQL026Catalog}
	staged, err := stageRecoveryLineageGeneration(ctx, stageTx, f.target.runtimeSchema, bytes.NewReader(out.Bytes()), inventory, expected, limits)
	if err != nil || staged.Rows != report.Rows || staged.Bytes != report.Bytes {
		t.Fatal("large actual generation staging rejected", err)
	}
	var stageSchema string
	if err = stageTx.QueryRow(ctx, "SELECT nspname FROM pg_catalog.pg_namespace WHERE oid=pg_catalog.pg_my_temp_schema()").Scan(&stageSchema); err != nil {
		t.Fatal(err)
	}
	for name, sql := range map[string]string{
		"payload_rehashed":    "UPDATE pg_temp.recovery_evidence_chunks SET payload=set_byte(payload,0,80),payload_sha256=encode(sha256(set_byte(payload,0,80)),'hex') WHERE ordinal=0",
		"wrong_schema":        "UPDATE pg_temp.recovery_evidence_batches SET evidence_schema_sha256=repeat('f',64)",
		"wrong_cut":           "UPDATE pg_temp.recovery_evidence_batches SET evidence_cut_at=evidence_cut_at+interval '1 second'",
		"wrong_source":        "UPDATE pg_temp.recovery_evidence_batches SET source_id='inst_00000000000000000000000000000000'",
		"wrong_snapshot_hash": "UPDATE pg_temp.recovery_evidence_batches SET evidence_snapshot_sha256=repeat('f',64)",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := stageTx.Exec(ctx, "SAVEPOINT evidence_corruption"); err != nil {
				t.Fatal(err)
			}
			tag, err := stageTx.Exec(ctx, sql)
			if err != nil || tag.RowsAffected() == 0 {
				t.Fatal("corruption fixture failed", err)
			}
			if validateRecoveryLineageEvidence(ctx, stageTx, stageSchema, limits) == nil {
				t.Fatal("corrupt retained snapshot accepted")
			}
			if _, err := stageTx.Exec(ctx, "ROLLBACK TO SAVEPOINT evidence_corruption"); err != nil {
				t.Fatal(err)
			}
			if _, err := stageTx.Exec(ctx, "RELEASE SAVEPOINT evidence_corruption"); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err = validateRecoveryLineageEvidence(ctx, stageTx, stageSchema, limits); err != nil {
		t.Fatal("original evidence no longer valid", err)
	}
}
