// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/recoveryprocess"
	"agentplatform.local/platform/internal/storage"
)

func TestRecoveryProcessHostRequiresExactConsentBeforeDocker(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "intent.json")
	if err := recoveryarchive.WriteHostJSON(path, recoveryProcessFile{Socket: "/unavailable/socket", Intent: recoveryprocess.Intent{Project: "synthetic", Containers: []recoveryprocess.Container{}}}); err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{nil, {"--confirm-stop-project"}, {"--expected-project", "synthetic"}, {"--confirm-stop-project", "--expected-project", "different"}} {
		args := append([]string{"container-stop-apply", "--intent-file", path}, flags...)
		if err := runRecoveryHost(context.Background(), args, io.Discard); err == nil || !strings.HasPrefix(err.Error(), "usage: platform recovery container-stop-plan|") {
			t.Fatalf("consent not refused before connection: %v", err)
		}
	}
}

func TestRecoveryHostAuthorRetirementNeedsSeparateConsent(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	saved := recoveryProcessFile{Socket: "/unavailable/socket", Intent: recoveryprocess.Intent{Project: "synthetic", Containers: []recoveryprocess.Container{}},
		Binding: &recoveryprocess.SourceBinding{Processes: []recoveryprocess.SourceProcess{}}, Capture: &recoverySourceCapture{},
		RetireConfig: "/unavailable/destination", RetireTo: &storage.RecoveryRetirementDestination{RestoreID: "synthetic-destination"}}
	retired := recoveryHostPrivateJSON(t, filepath.Join(parent, "retired.json"), saved)
	saved.RetireTo = nil
	saved.RetireConfig = ""
	plain := recoveryHostPrivateJSON(t, filepath.Join(parent, "plain.json"), saved)
	for _, args := range [][]string{
		{"source-stop-apply", "--intent-file", retired, "--expected-project", "synthetic", "--confirm-stop-project"},
		{"source-stop-apply", "--intent-file", retired, "--expected-project", "synthetic", "--confirm-stop-project", "--confirm-retire-source", "--expected-restore-id", "different"},
		{"source-stop-apply", "--intent-file", plain, "--expected-project", "synthetic", "--confirm-stop-project", "--confirm-retire-source"},
		{"source-stop-finish-retired", "--intent-file", plain, "--expected-project", "synthetic", "--confirm-stop-project"},
		{"source-stop-finish-retired", "--intent-file", retired, "--expected-project", "synthetic"},
	} {
		if err := runRecoveryHost(context.Background(), args, io.Discard); err == nil || !strings.HasPrefix(err.Error(), "usage:") {
			t.Fatal("retirement or final shutdown reached Docker without its exact plan and consent", err)
		}
	}
}

func TestSourceStopProofPathsExcludeBothArchives(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	selected, evidence := filepath.Join(parent, "selected"), filepath.Join(parent, "evidence")
	for _, dir := range []string{selected, evidence} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	saved := recoveryProcessFile{Socket: "/unavailable/socket", Intent: recoveryprocess.Intent{Containers: []recoveryprocess.Container{}}, Binding: &recoveryprocess.SourceBinding{Processes: []recoveryprocess.SourceProcess{}}, Capture: &recoverySourceCapture{SelectedDirectory: selected, Directory: evidence}}
	outside := filepath.Join(parent, "fresh")
	if err := validateSourceStopPath(saved, outside); err != nil {
		t.Fatal(err)
	}
	intent := filepath.Join(parent, "intent")
	if err := recoveryarchive.WriteHostJSON(intent, saved); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(selected, "proof"), filepath.Join(evidence, "proof")} {
		if err := validateSourceStopPath(saved, path); err == nil {
			t.Fatal("archive accepted as authority")
		}
		if err := runRecoveryProcessHost(context.Background(), []string{"source-stop-inspect", "--intent-file", intent, "--source-stop-out", path}, io.Discard); err == nil || err.Error() != "source stop proof requires capture and a private path outside both archives" {
			t.Fatal("path not refused before Docker", err)
		}
	}
	saved.Capture = nil
	if err := validateSourceStopPath(saved, outside); err == nil {
		t.Fatal("proof without capture accepted")
	}
}

func TestHandoffRetirementSealPinsTheSavedIntentAndBothReceipts(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	selected, late := filepath.Join(parent, "selected"), filepath.Join(parent, "late")
	for _, dir := range []string{selected, late} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	keys, path := filepath.Join(parent, "keys"), filepath.Join(parent, "source-stop")
	if _, err := recoveryarchive.GenerateHostKeys(keys); err != nil {
		t.Fatal(err)
	}
	now, sha := time.Now().UTC(), strings.Repeat("a", 64)
	l := recoveryarchive.CaptureLink{
		Version: 2, OperationID: strings.Repeat("a", 32), SourceID: "inst_" + strings.Repeat("b", 32),
		SelectedReceiptSHA256: sha, SelectedSnapshotSHA256: sha, SelectedCutUTC: now.Add(-2 * time.Second).Format(time.RFC3339Nano),
		BindingSHA256: sha, StoppedExecutorsSHA256: sha, EvidenceReceiptSHA256: sha, EvidenceSnapshotSHA256: sha,
		EvidenceCutUTC: now.Add(-time.Second).Format(time.RFC3339Nano), CapturedAt: now,
		ExportAlgorithm: recoveryarchive.DrainedCaptureAlgorithm, SystemID: "1234", DatabaseOID: 123, ExportWireBytes: 456,
		EvidenceSchemaSHA256: sha, EvidenceBackupID: "bak_" + strings.Repeat("c", 32), ExecutorsStoppedAtUTC: now.Add(-time.Second).Format(time.RFC3339Nano),
	}
	if err := recoveryarchive.WriteHostCaptureLink(path+".capture", keys, l); err != nil {
		t.Fatal(err)
	}
	link, err := recoveryarchive.VerifyHostCaptureLink(path+".capture", keys)
	if err != nil {
		t.Fatal(err)
	}
	dest := storage.RecoveryRetirementDestination{SystemID: "5678", WALTimeline: "00000001", MaintenanceOID: 5, AdminOID: 10, RestoreID: "restore_" + strings.Repeat("d", 32)}
	saved := recoveryProcessFile{RetireTo: &dest, Binding: &recoveryprocess.SourceBinding{},
		Capture: &recoverySourceCapture{SelectedDirectory: selected, Directory: late, Keys: keys}}
	p := storage.RecoverySourceRetirementIntent{Version: 2, SystemID: l.SystemID, SourceOID: l.DatabaseOID, MaintenanceRoleOID: 10,
		BaselineSHA256: l.EvidenceSnapshotSHA256, SourceInstallation: l.SourceID, CaptureLinkSHA256: link.SHA256(), Destination: dest}
	o := storage.RecoverySourceRetirementObservation{State: "source_admission_retired", MaintenanceOwnsDB: true, SourceInstallation: l.SourceID,
		CaptureLinkSHA256: link.SHA256(), Destination: dest, ReceiptSHA256: strings.Repeat("e", 64), CutProofSHA256: strings.Repeat("f", 64)}
	recoveryHostPrivateJSON(t, path+".retirement-intent", p)
	recoveryHostPrivateJSON(t, path+".retirement", o)
	old, seal, err := loadHandoffRetirement(saved, path, link)
	if err != nil || old.SealSHA256 != "" || seal != nil {
		t.Fatal("legacy held handoff incorrectly claimed an authenticated retirement", err)
	}
	if err = recoveryarchive.WriteHostSourceRetirementSeal(path+sourceRetirementSealSuffix, keys, link, sourceRetirementBinding(p, o)); err != nil {
		t.Fatal(err)
	}
	verified, seal, err := loadHandoffRetirement(saved, path, link)
	if err != nil || verified.SealSHA256 == "" || verified.SealSHA256 != seal.SHA256() || verified.Destination != dest {
		t.Fatal("exact sealed retirement did not bind the handoff", err)
	}
	rewrite := func(suffix string, value any) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil || os.WriteFile(path+suffix, raw, 0600) != nil {
			t.Fatal("could not rewrite the owned test file")
		}
	}
	o.CutProofSHA256 = sha
	rewrite(".retirement", o)
	if _, _, err = loadHandoffRetirement(saved, path, link); err == nil {
		t.Fatal("edited retirement observation retained the seal's authority")
	}
	o.CutProofSHA256 = strings.Repeat("f", 64)
	rewrite(".retirement", o)
	p.MaintenanceRoleOID++
	rewrite(".retirement-intent", p)
	if _, _, err = loadHandoffRetirement(saved, path, link); err == nil {
		t.Fatal("edited intent retained the seal's authority")
	}
	saved.RetireTo = nil
	if _, _, err = loadHandoffRetirement(saved, path, link); err == nil {
		t.Fatal("removing the selected destination concealed retirement sidecars")
	}
}
