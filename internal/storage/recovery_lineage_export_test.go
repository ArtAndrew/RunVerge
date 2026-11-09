// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

func TestRecoveryLineageExportActual(t *testing.T) {
	f := evidenceAuthorSetup(t)
	ctx := context.Background()
	limits := DefaultRecoverySnapshotLimits()
	if _, err := f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, limits); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	var prior []byte
	source, sourceSpec := f.target, f.spec
	initial, err := source.InspectRecoveryImport(ctx, sourceSpec, f.selected.Manifest().PlaintextSHA256)
	if err != nil || initial.Receipt == nil {
		t.Fatal("source import inspection", err)
	}
	sourceSHA := initial.Receipt.SnapshotSHA256
	for generations := 1; generations <= 2; generations++ {
		var snapshot, history bytes.Buffer
		exported, err := source.ExportHeldRecoveryLineageSnapshot(ctx, &snapshot, limits, sourceSpec, sourceSHA, parent)
		if err != nil {
			t.Fatalf("export %d: %v", generations, err)
		}
		parsed, err := validateRecoverySnapshotEnvelopeTo(ctx, bytes.NewReader(snapshot.Bytes()), limits, true, &history)
		if err != nil || parsed.Lineage == nil || parsed.Lineage.Generations != generations || parsed.SHA256 != exported.SHA256 || parsed.Metadata.SourceID != sourceSpec.TargetInstallationID {
			t.Fatalf("verify export %d: %v", generations, err)
		}
		if len(prior) >= history.Len() || !bytes.HasPrefix(history.Bytes(), prior) {
			t.Fatal("old generation bytes were lost or rewritten")
		}
		archive := recoveryLineageTestArchive(t, snapshot.Bytes(), parsed.Metadata, parent)
		target, _ := recoveryAuthorStore(t)
		spec := recoveryTargetAuthorSpec(t, target)
		spec.BackupID, spec.SourceID, spec.CutUTC, spec.SchemaSHA256 = parsed.Metadata.BackupID, parsed.Metadata.SourceID, parsed.Metadata.CutUTC, parsed.Metadata.SchemaSHA256
		if _, err = target.PrepareRecoveryTarget(ctx, spec); err != nil {
			t.Fatal(err)
		}
		if _, err = target.ImportRecoverySnapshotLineage(ctx, archive, spec, limits, parent); err != nil {
			t.Fatalf("import %d: %v", generations, err)
		}
		observed, err := target.InspectRecoverySnapshotLineage(ctx, archive, spec, limits)
		if err != nil || observed.State != "committed_held" || observed.Receipt == nil || observed.Receipt.SnapshotSHA256 != parsed.SHA256 || target.CheckRuntimeAdmission(ctx) != RecoveryHeld {
			t.Fatalf("inspect import %d: %v", generations, err)
		}
		prior, source, sourceSpec, sourceSHA = bytes.Clone(history.Bytes()), target, spec, parsed.SHA256
	}
	var refused bytes.Buffer
	limited := limits
	limited.MaxMetadataEntries = 2
	if _, err := source.ExportHeldRecoveryLineageSnapshot(ctx, &refused, limited, sourceSpec, sourceSHA, parent); !errors.Is(err, ErrRecoverySnapshotLimit) {
		t.Fatalf("generation limit: %v", err)
	}
	if _, err := validateRecoverySnapshotEnvelope(ctx, &refused, limits, true); err == nil {
		t.Fatal("incomplete export accepted after a limit failure")
	}
	if _, err := source.ExportRecoverySnapshot(ctx, &bytes.Buffer{}, limits); !errors.Is(err, ErrRecoverySnapshotSource) {
		t.Fatalf("ordinary export allowed a held source: %v", err)
	}
}
