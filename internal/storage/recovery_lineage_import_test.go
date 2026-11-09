// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoverylineage"
	"github.com/jackc/pgx/v5"
)

// Synthetic outer export assembly exercises the internal importer only. It is
// not evidence of a resumed-source exporter or restored execution authority.
func TestRecoveryLineageAtomicImportActual(t *testing.T) {
	f := evidenceAuthorSetup(t)
	ctx := context.Background()
	limits := DefaultRecoverySnapshotLimits()
	if _, err := f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, limits); err != nil {
		t.Fatal(err)
	}
	tx, err := f.target.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	var history bytes.Buffer
	historyWriter := &recoverySnapshotWriter{ctx: ctx, out: &history, hash: sha256.New(), limit: limits.MaxWireBytes}
	historyReport := RecoverySnapshotReport{}
	if err = writeRecoveryLineageGenerationTx(ctx, tx, f.target.runtimeSchema, historyWriter, limits, &historyReport); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	receipt, err := readRecoveryImportReceipt(ctx, tx, f.target.runtimeSchema)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = f.selected.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(f.selected)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := ValidateRecoverySnapshot(ctx, bytes.NewReader(raw), limits)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte{'\n'}), []byte{'\n'})
	var header recoverySnapshotHeader
	if json.Unmarshal(lines[0], &header) != nil {
		t.Fatal("source header")
	}
	header.Format = recoverySnapshotLineageFormat
	header.Metadata.SourceID = receipt.TargetInstallationID
	header.Metadata.BackupID = domain.NewID("bak")
	header.Metadata.CutUTC = time.Now().UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	buildEnvelope := func(payload []byte) ([]byte, string) {
		outerReport := snapshot
		var envelope bytes.Buffer
		writer := &recoverySnapshotWriter{ctx: ctx, out: &envelope, hash: sha256.New(), limit: limits.MaxWireBytes}
		if err = writer.record(header); err != nil {
			t.Fatal(err)
		}
		for _, line := range lines[1 : len(lines)-1] {
			if err = writer.record(json.RawMessage(line)); err != nil {
				t.Fatal(err)
			}
		}
		commitment, err := recoverylineage.NewChunkCommitment(int64(len(payload)))
		if err != nil {
			t.Fatal(err)
		}
		remaining := payload
		for i := 0; len(remaining) > 0; i++ {
			n := min(len(remaining), recoverylineage.ChunkBytes)
			if err = commitment.Add(i, remaining[:n]); err != nil {
				t.Fatal(err)
			}
			remaining = remaining[n:]
		}
		digest, err := commitment.Digest()
		if err != nil {
			t.Fatal(err)
		}
		declared := RecoveryLineageFramingReport{PayloadBytes: int64(len(payload)), Records: historyReport.Rows, Generations: 1, ChunksSHA256: digest}
		if err = writeRecoverySnapshotLineage(writer, bytes.NewReader(payload), declared, limits, &outerReport); err != nil {
			t.Fatal(err)
		}
		if err = writer.record(recoverySnapshotComplete{Kind: "complete", Tables: len(outerReport.Tables), Rows: outerReport.Rows, Bytes: outerReport.Bytes, PrecedingWireBytes: writer.bytes, PrecedingSHA256: recoverySnapshotHash(writer.hash)}); err != nil {
			t.Fatal(err)
		}
		return envelope.Bytes(), digest
	}
	envelopeBytes, digest := buildEnvelope(history.Bytes())
	parent := t.TempDir()
	if err = os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	archive := recoveryLineageTestArchive(t, envelopeBytes, header.Metadata, parent)

	for _, mode := range []string{"acknowledged", "before_commit", "after_commit", "after_business", "invalid_history"} {
		t.Run(mode, func(t *testing.T) {
			target, _ := recoveryAuthorStore(t)
			spec := recoveryTargetAuthorSpec(t, target)
			spec.BackupID, spec.SourceID, spec.CutUTC, spec.SchemaSHA256 = header.Metadata.BackupID, header.Metadata.SourceID, header.Metadata.CutUTC, header.Metadata.SchemaSHA256
			if _, err = target.PrepareRecoveryTarget(ctx, spec); err != nil {
				t.Fatal(err)
			}
			before := stageIndependentCapture(t, target)
			selectedArchive := archive
			if mode == "invalid_history" {
				bad := bytes.Replace(history.Bytes(), []byte("\"imported_source_id\":\""+receipt.SourceID+"\""), []byte("\"imported_source_id\":\""+domain.NewID("inst")+"\""), 1)
				if bytes.Equal(bad, history.Bytes()) {
					t.Fatal("history mutation not applied")
				}
				badEnvelope, _ := buildEnvelope(bad)
				selectedArchive = recoveryLineageTestArchive(t, badEnvelope, header.Metadata, parent)
			}
			importer := target
			var proxy *independentCommitCutProxy
			var cut *independentCommitCutPlan
			if mode != "acknowledged" && mode != "invalid_history" {
				importer, proxy = independentImportCommitProxy(t, target.Pool.Config().ConnString())
				match := "insert into " + pgx.Identifier{target.runtimeSchema, "recovery_lineage_imports"}.Sanitize()
				cutMode := mode
				if mode == "after_business" {
					match = "set constraints " + pgx.Identifier{target.runtimeSchema, "recovery_lineage_complete"}.Sanitize() + " deferred"
					cutMode = "before_statement"
				}
				cut = proxy.arm(t, match, cutMode)
			}
			imported, importErr := importer.ImportRecoverySnapshotLineage(ctx, selectedArchive, spec, limits, parent)
			if proxy != nil {
				proxy.wait(t, cut)
			}
			if mode == "invalid_history" && !errors.Is(importErr, ErrRecoveryStageData) {
				t.Fatal("invalid receipt did not reach typed history validation", importErr)
			}
			committed := mode == "acknowledged" || mode == "after_commit"
			if mode == "acknowledged" {
				if importErr != nil || imported.SnapshotSHA256 != Hash(envelopeBytes) {
					t.Fatal("atomic history import", importErr)
				}
			} else {
				if importErr == nil || !reflect.DeepEqual(imported, RecoveryImportReceipt{}) {
					t.Fatal("uncertain/failed import returned a receipt", importErr)
				}
				if (mode == "before_commit" || mode == "after_commit") && !errors.Is(importErr, ErrRecoveryImportOutcomeUnknown) {
					t.Fatal("commit loss not classified unknown", importErr)
				}
			}
			// A new read-only transaction observes both commitments, never resends import.
			check, err := target.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if err != nil {
				t.Fatal(err)
			}
			defer check.Rollback(ctx)
			if _, err = validateRecoverySemanticCatalog(ctx, check, target.runtimeSchema); err != nil {
				t.Fatal("import changed trusted schema", err)
			}
			if !committed {
				var empty bool
				if err = check.QueryRow(ctx, "SELECT NOT EXISTS(SELECT 1 FROM organizations) AND NOT EXISTS(SELECT 1 FROM recovery_historical_rows) AND NOT EXISTS(SELECT 1 FROM recovery_import_receipts) AND NOT EXISTS(SELECT 1 FROM recovery_lineage_imports) AND NOT EXISTS(SELECT 1 FROM recovery_lineage_chunks)").Scan(&empty); err != nil || !empty {
					t.Fatal("failed atomic import left durable rows", err)
				}
			} else {
				expected := recoveryLineagePersistence{RestoreID: spec.RestoreID, TargetInstallationID: spec.TargetInstallationID, SnapshotSHA256: Hash(envelopeBytes), Generations: 1, PayloadBytes: int64(history.Len()), ChunksSHA256: digest}
				if err = inspectRecoveryLineageTx(ctx, check, target.runtimeSchema, expected); err != nil {
					t.Fatal("fresh history inspection", err)
				}
				stored, err := readRecoveryImportReceipt(ctx, check, target.runtimeSchema)
				if err != nil || stored.SnapshotSHA256 != expected.SnapshotSHA256 || stored.RestoreID != spec.RestoreID || stored.TargetInstallationID != spec.TargetInstallationID {
					t.Fatal("fresh business receipt", err)
				}
				var sameTransaction bool
				if err = check.QueryRow(ctx, "SELECT r.creating_transaction=l.creating_transaction FROM recovery_import_receipts r JOIN recovery_lineage_imports l USING(restore_id)").Scan(&sameTransaction); err != nil || !sameTransaction {
					t.Fatal("history and business receipt not atomic", err)
				}
				var retained string
				if err = check.QueryRow(ctx, "SELECT string_agg(encode(payload,'hex'),'' ORDER BY ordinal) FROM recovery_lineage_chunks").Scan(&retained); err != nil || retained != hex.EncodeToString(history.Bytes()) {
					t.Fatal("retained history bytes changed", err)
				}
				var organizations int
				if err = check.QueryRow(ctx, "SELECT count(*) FROM organizations").Scan(&organizations); err != nil || organizations < 1 {
					t.Fatal("business data absent", err)
				}
			}
			if err = check.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			observed, err := target.InspectRecoverySnapshotLineage(ctx, selectedArchive, spec, limits)
			if committed {
				if err != nil || observed.State != "committed_held" || observed.Receipt == nil || observed.Receipt.SnapshotSHA256 != Hash(envelopeBytes) {
					t.Fatal("verified inspection did not observe both commitments", err)
				}
				if _, err = target.InspectRecoveryImport(ctx, spec, Hash(envelopeBytes)); !errors.Is(err, ErrRecoveryImport) {
					t.Fatal("business-only inspection accepted a lineage import", err)
				}
				differentHistory := bytes.Replace(history.Bytes(), []byte(receipt.SourceID), []byte(domain.NewID("inst")), 1)
				if bytes.Equal(differentHistory, history.Bytes()) {
					t.Fatal("inspection mismatch fixture did not change")
				}
				differentEnvelope, _ := buildEnvelope(differentHistory)
				differentArchive := recoveryLineageTestArchive(t, differentEnvelope, header.Metadata, parent)
				if _, err = target.InspectRecoverySnapshotLineage(ctx, differentArchive, spec, limits); !errors.Is(err, ErrRecoveryImport) {
					t.Fatal("inspection accepted a different authenticated backup", err)
				}
			} else if err != nil || observed.State != "empty_held" || observed.Receipt != nil {
				t.Fatal("verified inspection did not observe an empty held target", err)
			}
			if !committed && !reflect.DeepEqual(stageIndependentCapture(t, target), before) {
				t.Fatal("failed import changed tables, sequences or control")
			}
			if target.CheckRuntimeAdmission(ctx) != RecoveryHeld {
				t.Fatal("history import enabled execution")
			}
		})
	}
}
