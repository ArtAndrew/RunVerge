// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/recoveryprocess"
	"agentplatform.local/platform/internal/storage"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecoveryHostAuthorEvidenceConsentBeforeFiles(t *testing.T) {
	if err := runRecoveryHost(context.Background(), []string{"evidence-ingest", "--source-intent-file", "/not-read", "--target-config", "/not-read"}, io.Discard); err == nil || !strings.HasPrefix(err.Error(), "usage:") {
		t.Fatal("missing consent reached private files")
	}
}

func recoveryHostEvidenceRoundTrip(t *testing.T, ctx context.Context, source *storage.Store, sourceFile, parent, stage, keys, selected, targetConfig string, old recoveryarchive.HostBackupReceipt, accountingFixture *recoveryHostAccountingFixture) {
	t.Helper()
	if _, err := source.Bootstrap(ctx, "late synthetic organization", 1000); err != nil {
		t.Fatal(err)
	}
	appendRecoveryHostAccountingFacts(t, ctx, source, accountingFixture)
	lateDir := filepath.Join(parent, "late")
	raw, err := recoveryHostCommand(t, ctx, "backup-create", "--database-config", sourceFile, "--keys-file", keys, "--staging-parent", stage, "--out-dir", lateDir)
	var late recoveryarchive.HostBackupReceipt
	if err != nil || json.Unmarshal(raw, &late) != nil {
		t.Fatal("late command archive", err)
	}
	// This fixture authenticates a synthetic operator capture attestation.
	// Actual live Docker/process binding belongs to the installed Compose gate.
	op := strings.Repeat("c", 32)
	saved := recoveryProcessFile{Intent: recoveryprocess.Intent{OperationID: op, Containers: []recoveryprocess.Container{}}, Binding: &recoveryprocess.SourceBinding{SourceID: old.Manifest.Metadata.SourceID, SnapshotSHA256: old.Manifest.PlaintextSHA256, Processes: []recoveryprocess.SourceProcess{}}, Capture: &recoverySourceCapture{Directory: lateDir, Keys: keys, Stage: stage, SelectedDirectory: selected, SelectedReceipt: old.ReceiptSHA256, SelectedCut: old.Manifest.Metadata.CutUTC}}
	path := recoveryHostPrivateJSON(t, filepath.Join(parent, "capture-plan.json"), saved)
	var decoded recoveryProcessFile
	if err := readRecoveryFenceJSON(path, &decoded); err != nil {
		t.Fatal("synthetic capture plan shape", err)
	}
	state := recoveryprocess.Observation{OperationID: op, Containers: []recoveryprocess.ContainerState{}}
	recoveryHostPrivateJSON(t, path+".capture-state", state)
	link := recoveryarchive.CaptureLink{Version: 1, OperationID: op, SourceID: old.Manifest.Metadata.SourceID, SelectedReceiptSHA256: old.ReceiptSHA256, SelectedSnapshotSHA256: old.Manifest.PlaintextSHA256, SelectedCutUTC: old.Manifest.Metadata.CutUTC, BindingSHA256: captureHash(saved.Binding), StoppedExecutorsSHA256: captureHash(state), EvidenceReceiptSHA256: late.ReceiptSHA256, EvidenceSnapshotSHA256: late.Manifest.PlaintextSHA256, EvidenceCutUTC: late.Manifest.Metadata.CutUTC, CapturedAt: time.Now().UTC()}
	if err = recoveryarchive.WriteHostCaptureLink(path+".capture", keys, link); err != nil {
		t.Fatal(err)
	}
	args := []string{"evidence-ingest", "--source-intent-file", path, "--target-config", targetConfig, "--confirm-ingest-evidence"}
	if err = runRecoveryHost(ctx, args, recoveryHostFailedOutput{}); !errors.Is(err, errRecoveryHostOutput) {
		t.Fatal("evidence output loss", err)
	}
	raw, err = recoveryHostCommand(t, ctx, "evidence-inspect", "--source-intent-file", path, "--target-config", targetConfig)
	var report storage.RecoveryEvidenceReport
	if err != nil || json.Unmarshal(raw, &report) != nil || report.Added == 0 || report.ReconciliationState != "pending" || !report.ExecutionHeld {
		t.Fatal("fresh evidence CLI inspection", err)
	}
	if _, err = recoveryHostCommand(t, ctx, args...); err == nil {
		t.Fatal("evidence CLI replay accepted")
	}
	planArgs := []string{"reconciliation-plan", "--source-intent-file", path, "--target-config", targetConfig}
	raw, err = recoveryHostCommand(t, ctx, planArgs...)
	var plan storage.RecoveryReconciliationPlan
	if err != nil || json.Unmarshal(raw, &plan) != nil || !plan.RollbackConfirmed || !plan.ExecutionHeld || plan.ReconciliationState != "pending" || len(plan.PlanSHA256) != 64 || len(plan.Organizations) < 2 {
		t.Fatal("fresh reconciliation plan CLI", err)
	}
	raw, err = recoveryHostCommand(t, ctx, planArgs...)
	var repeated storage.RecoveryReconciliationPlan
	if err != nil || json.Unmarshal(raw, &repeated) != nil || repeated.PlanSHA256 != plan.PlanSHA256 {
		t.Fatal("plan CLI is not reproducible", err)
	}
	recoveryHostAccountingRoundTrip(t, ctx, accountingFixture, path, targetConfig, parent)
}
