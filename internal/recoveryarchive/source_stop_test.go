// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/recoveryprocess"
)

// Synthetic historical metadata for signature/representation tests only. It
// cannot construct an opaque live observation or establish actual source stop.
func sourceStopMetadata(t *testing.T, dir, keys string) (*VerifiedCaptureLink, SourceStopReceipt) {
	t.Helper()
	start := time.Now().UTC().Add(-time.Minute)
	p := recoveryprocess.Intent{Version: 1, OperationID: strings.Repeat("a", 32), DaemonID: "owned-test-daemon", Project: "synthetic", CreatedAt: start}
	b := recoveryprocess.SourceBinding{Version: 1, SourceID: "inst_" + strings.Repeat("b", 32), SnapshotSHA256: strings.Repeat("c", 64), RuntimeImage: "sha256:" + strings.Repeat("d", 64), SystemID: "12345", DatabaseOID: 123, ObservedAt: start.Add(time.Second)}
	o := recoveryprocess.Observation{OperationID: p.OperationID, Project: p.Project, ObservedAt: start.Add(4 * time.Second), ProjectProcessesStopped: true, SourceInstallationBound: true, BoundSourceProcessesStopped: true, SourceID: b.SourceID}
	for i, svc := range []string{"db", "api", "worker"} {
		id := fmt.Sprintf("%064d", i+1)
		p.Containers = append(p.Containers, recoveryprocess.Container{ID: id, Name: "/synthetic-" + svc, Service: svc, Created: start.Add(-time.Hour).Format(time.RFC3339Nano), Image: b.RuntimeImage, StartedAt: start.Add(-time.Minute).Format(time.RFC3339Nano)})
		o.Containers = append(o.Containers, recoveryprocess.ContainerState{ID: id, Service: svc, Status: "exited", RestartPolicy: "no", Stopped: true})
		if svc == "db" {
			b.DatabaseContainer = id
		} else {
			b.Processes = append(b.Processes, recoveryprocess.SourceProcess{Container: id, BootID: strings.Repeat("e", 32)})
		}
	}
	raw, _ := json.Marshal(p)
	b.PlanSHA256 = digest(raw)
	raw, _ = json.Marshal(b)
	h := strings.Repeat("f", 64)
	l := CaptureLink{Version: 1, OperationID: p.OperationID, SourceID: b.SourceID, SelectedReceiptSHA256: h, SelectedSnapshotSHA256: b.SnapshotSHA256, SelectedCutUTC: start.Add(-time.Second).Format(time.RFC3339Nano), BindingSHA256: digest(raw), StoppedExecutorsSHA256: h, EvidenceReceiptSHA256: h, EvidenceSnapshotSHA256: h, EvidenceCutUTC: start.Add(2 * time.Second).Format(time.RFC3339Nano), CapturedAt: start.Add(3 * time.Second)}
	file := filepath.Join(dir, "capture")
	if err := WriteHostCaptureLink(file, keys, l); err != nil {
		t.Fatal(err)
	}
	link, err := VerifyHostCaptureLink(file, keys)
	if err != nil {
		t.Fatal(err)
	}
	return link, SourceStopReceipt{Version: 1, Profile: sourceStopProfile, CaptureLinkSHA256: link.SHA256(), Capture: l, Source: recoveryprocess.SourceStopRecord{Intent: p, Binding: b, Observation: o}}
}

func TestSourceStopHistoricalAuthenticationAndPairBinding(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(dir, "keys")
	if _, err := GenerateHostKeys(keys); err != nil {
		t.Fatal(err)
	}
	link, r := sourceStopMetadata(t, dir, keys)
	_, key, err := loadHostKeys(keys)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	write := func(name, domain string, record SourceStopReceipt) string {
		t.Helper()
		body, _ := json.Marshal(record)
		raw, _ := json.Marshal(signedSourceStop{record, ed25519.Sign(key, append([]byte(domain), body...))})
		path := filepath.Join(dir, name)
		if err := writeHostPrivateFile(path, raw); err != nil {
			t.Fatal(err)
		}
		return path
	}
	path := write("stopped", sourceStopDomain, r)
	proof, err := VerifyHostSourceStop(path, keys, link, nil)
	if err != nil {
		t.Fatal(err)
	}
	copy := proof.Receipt()
	copy.Source.Intent.Containers[0].ID = "changed"
	copy.Source.Binding.Processes[0].BootID = "changed"
	copy.Source.Observation.Containers[0].Stopped = false
	raw := proof.Bytes()
	raw[0] = '!'
	if !sourceStopValid(proof.Receipt(), link) || digest(proof.Bytes()) != proof.SHA256() {
		t.Fatal("caller mutated verified receipt")
	}
	if _, err := VerifyHostCaptureLink(path, keys); err == nil {
		t.Fatal("stop proof accepted as capture")
	}
	if _, err := VerifyHostSourceStop(filepath.Join(dir, "capture"), keys, link, nil); err == nil {
		t.Fatal("capture accepted as stop proof")
	}
	if _, err := VerifyHostSourceStop(write("wrong-domain", captureDomain, r), keys, link, nil); err == nil {
		t.Fatal("wrong signature domain accepted")
	}
	for _, fault := range []string{"binding_after_evidence", "selected_after_plan", "window_claim", "work_claim", "different_operation", "different_pair"} {
		t.Run(fault, func(t *testing.T) {
			bad := proof.Receipt()
			switch fault {
			case "binding_after_evidence":
				bad.Capture.EvidenceCutUTC = bad.Source.Intent.CreatedAt.Format(time.RFC3339Nano)
			case "selected_after_plan":
				bad.Capture.SelectedCutUTC = bad.Source.Binding.ObservedAt.Format(time.RFC3339Nano)
			case "window_claim":
				bad.WindowCovered = true
			case "work_claim":
				bad.NewWorkAllowed = true
			case "different_operation":
				bad.Capture.OperationID = strings.Repeat("f", 32)
			case "different_pair":
				bad.Capture.EvidenceReceiptSHA256 = strings.Repeat("a", 64)
			}
			capturePath := filepath.Join(dir, fault+"-capture")
			if err := WriteHostCaptureLink(capturePath, keys, bad.Capture); err != nil {
				t.Fatal(err)
			}
			other, err := VerifyHostCaptureLink(capturePath, keys)
			if err != nil {
				t.Fatal(err)
			}
			bad.CaptureLinkSHA256 = other.SHA256()
			if _, err := VerifyHostSourceStop(path, keys, other, nil); err == nil && fault != "window_claim" && fault != "work_claim" {
				t.Fatal("original proof paired with different capture")
			}
			if fault != "different_pair" {
				if _, err := VerifyHostSourceStop(write(fault, sourceStopDomain, bad), keys, other, nil); err == nil {
					t.Fatal("invalid signed record accepted")
				}
			}
		})
	}
	zeroPath := filepath.Join(dir, "zero")
	if err := WriteHostSourceStop(zeroPath, keys, link, recoveryprocess.ObservedSourceStop{}); err == nil {
		t.Fatal("zero opaque proof accepted")
	}
	if err := WriteHostSourceStop(zeroPath, keys, nil, recoveryprocess.ObservedSourceStop{}); err == nil {
		t.Fatal("nil capture accepted")
	}
	if _, err := os.Stat(zeroPath); !os.IsNotExist(err) {
		t.Fatal("invalid proof created output")
	}
}

func TestRetiredSourceStopRequiresTheExactSignedSeal(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(dir, "keys")
	if _, err := GenerateHostKeys(keys); err != nil {
		t.Fatal(err)
	}
	legacy, stop := sourceStopMetadata(t, dir, keys)
	l := legacy.Link()
	l.Version = 2
	l.ExportAlgorithm = DrainedCaptureAlgorithm
	l.SystemID, l.DatabaseOID = stop.Source.Binding.SystemID, stop.Source.Binding.DatabaseOID
	l.ExportWireBytes = 1234
	l.EvidenceSchemaSHA256 = strings.Repeat("d", 64)
	l.EvidenceBackupID = "bak_" + strings.Repeat("a", 32)
	l.ExecutorsStoppedAtUTC = stop.Source.Binding.ObservedAt.Format(time.RFC3339Nano)
	capturePath := filepath.Join(dir, "drained-capture")
	if err := WriteHostCaptureLink(capturePath, keys, l); err != nil {
		t.Fatal(err)
	}
	link, err := VerifyHostCaptureLink(capturePath, keys)
	if err != nil {
		t.Fatal(err)
	}
	binding := SourceRetirementBinding{"restore_" + strings.Repeat("a", 32), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)}
	sealPath := filepath.Join(dir, "seal")
	if err = WriteHostSourceRetirementSeal(sealPath, keys, legacy, binding); err == nil {
		t.Fatal("legacy capture qualified for signed retirement")
	}
	if err = WriteHostSourceRetirementSeal(sealPath, keys, link, binding); err != nil {
		t.Fatal(err)
	}
	seal, err := VerifyHostSourceRetirementSeal(sealPath, keys, link)
	if err != nil || seal.Binding() != binding {
		t.Fatal("exact retirement seal unavailable", err)
	}
	if err = WriteHostSourceRetirementSeal(sealPath, keys, link, binding); err == nil {
		t.Fatal("retirement seal was overwritten")
	}
	changed := binding
	changed.RestoreID = "restore_" + strings.Repeat("e", 32)
	replacementPath := filepath.Join(dir, "other-seal")
	if err = WriteHostSourceRetirementSeal(replacementPath, keys, link, changed); err != nil {
		t.Fatal(err)
	}
	replacement, err := VerifyHostSourceRetirementSeal(replacementPath, keys, link)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := loadHostKeys(keys)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	stop.Version, stop.Profile, stop.RetirementSealSHA256 = 2, retiredSourceStopProfile, seal.SHA256()
	stop.Capture, stop.CaptureLinkSHA256 = l, link.SHA256()
	stop.Source.Observation.ObservedAt = time.Now().UTC()
	write := func(name, domain string, r SourceStopReceipt) string {
		t.Helper()
		body, _ := json.Marshal(r)
		raw, _ := json.Marshal(signedSourceStop{r, ed25519.Sign(key, append([]byte(domain), body...))})
		path := filepath.Join(dir, name)
		if e := writeHostPrivateFile(path, raw); e != nil {
			t.Fatal(e)
		}
		return path
	}
	proof := write("final-stop", retiredSourceStopDomain, stop)
	verifiedStop, err := VerifyHostSourceStop(proof, keys, link, seal)
	if err != nil {
		t.Fatal("exact final stop and retirement pair refused", err)
	}
	testRecoveryPeerJournalTrust(t, dir, verifiedStop, key)
	for _, wrong := range []*VerifiedSourceRetirementSeal{nil, replacement} {
		if _, err = VerifyHostSourceStop(proof, keys, link, wrong); err == nil {
			t.Fatal("missing or substituted seal accepted")
		}
	}
	if _, err = VerifyHostSourceStop(write("wrong-domain", sourceStopDomain, stop), keys, link, seal); err == nil {
		t.Fatal("legacy signature domain accepted a retired stop")
	}
	stop.Source.Observation.ObservedAt = seal.seal.SealedAt.Add(-time.Nanosecond)
	if _, err = VerifyHostSourceStop(write("early-stop", retiredSourceStopDomain, stop), keys, link, seal); err == nil {
		t.Fatal("stop observed before the retirement seal qualified")
	}
	tampered, err := os.ReadFile(sealPath)
	if err != nil {
		t.Fatal(err)
	}
	tampered = []byte(strings.Replace(string(tampered), binding.CutProofSHA256, strings.Repeat("f", 64), 1))
	if err = os.WriteFile(sealPath, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyHostSourceRetirementSeal(sealPath, keys, link); err == nil {
		t.Fatal("tampered retirement seal verified")
	}
}
