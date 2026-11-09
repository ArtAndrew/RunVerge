// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/recoveryprocess"
)

func TestDrainedCaptureAuthenticationAndRetainedObservation(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(dir, "keys")
	if _, err := GenerateHostKeys(keys); err != nil {
		t.Fatal(err)
	}
	legacy, final := sourceStopMetadata(t, dir, keys)
	r := final.Source
	r.Observation.Containers = append([]recoveryprocess.ContainerState(nil), r.Observation.Containers...)
	r.Observation.ProjectProcessesStopped = false
	r.Observation.BoundSourceProcessesStopped = false
	r.Observation.ObservedAt = r.Binding.ObservedAt.Add(500 * time.Millisecond)
	r.Observation.Containers[0].Stopped = false
	r.Observation.Containers[0].Status = "running"
	if !recoveryprocess.SourceCaptureRecordConsistent(r) {
		t.Fatal("invalid synthetic capture")
	}
	if legacy.MatchesDrainedSource(r) {
		t.Fatal("v1 promoted to drained proof")
	}
	legacyRaw, _ := json.Marshal(legacy.Link())
	if strings.Contains(string(legacyRaw), "export_algorithm") || strings.Contains(string(legacyRaw), "executors_stopped_at_utc") {
		t.Fatal("legacy canonical bytes changed")
	}
	l := legacy.Link()
	l.Version = 2
	l.ExportAlgorithm = DrainedCaptureAlgorithm
	l.SystemID = r.Binding.SystemID
	l.DatabaseOID = r.Binding.DatabaseOID
	l.ExportWireBytes = 1234
	l.EvidenceSchemaSHA256 = strings.Repeat("d", 64)
	l.EvidenceBackupID = "bak_" + strings.Repeat("a", 32)
	l.ExecutorsStoppedAtUTC = r.Observation.ObservedAt.Format(time.RFC3339Nano)
	raw, _ := json.Marshal(r.Observation)
	l.StoppedExecutorsSHA256 = digest(raw)
	file := filepath.Join(dir, "v2")
	if err := WriteHostCaptureLink(file, keys, l); err != nil {
		t.Fatal(err)
	}
	v, err := VerifyHostCaptureLink(file, keys)
	if err != nil || !v.MatchesDrainedSource(r) {
		t.Fatal("v2 roundtrip", err)
	}
	manifest := Manifest{PlaintextBytes: l.ExportWireBytes, PlaintextSHA256: l.EvidenceSnapshotSHA256, Metadata: Metadata{BackupID: l.EvidenceBackupID, SourceID: l.SourceID, CutUTC: l.EvidenceCutUTC, SchemaSHA256: l.EvidenceSchemaSHA256}}
	if !v.MatchesEvidenceManifest(manifest) {
		t.Fatal("matching manifest refused")
	}
	for _, field := range []string{"bytes", "schema", "backup"} {
		x := l
		switch field {
		case "bytes":
			x.ExportWireBytes++
		case "schema":
			x.EvidenceSchemaSHA256 = strings.Repeat("e", 64)
		case "backup":
			x.EvidenceBackupID = "bak_" + strings.Repeat("b", 32)
		}
		path := filepath.Join(dir, "signed-"+field)
		if err := WriteHostCaptureLink(path, keys, x); err != nil {
			t.Fatal(err)
		}
		wrong, err := VerifyHostCaptureLink(path, keys)
		if err != nil {
			t.Fatal(err)
		}
		if wrong.MatchesEvidenceManifest(manifest) {
			t.Fatal("correct signature accepted contradictory archive", field)
		}
	}
	for _, fault := range []string{"executor_running", "database_stopped", "missing", "restart_policy", "external_claim", "unbound", "wrong_system", "after_cut", "before_binding", "wrong_plan"} {
		t.Run(fault, func(t *testing.T) {
			x := r
			x.Observation.Containers = append([]recoveryprocess.ContainerState(nil), r.Observation.Containers...)
			switch fault {
			case "executor_running":
				x.Observation.Containers[1].Stopped = false
				x.Observation.Containers[1].Status = "running"
			case "database_stopped":
				x.Observation.Containers[0].Stopped = true
				x.Observation.Containers[0].Status = "exited"
			case "missing":
				x.Observation.Containers = x.Observation.Containers[:1]
			case "restart_policy":
				x.Observation.Containers[1].RestartPolicy = "always"
			case "external_claim":
				x.Observation.ExternalEffectsSettled = true
			case "unbound":
				x.Observation.SourceInstallationBound = false
			case "wrong_system":
				x.Binding.SystemID = "999"
			case "after_cut":
				x.Observation.ObservedAt = l.CapturedAt
			case "before_binding":
				x.Observation.ObservedAt = x.Intent.CreatedAt
			case "wrong_plan":
				x.Intent.OperationID = strings.Repeat("f", 32)
			}
			if v.MatchesDrainedSource(x) {
				t.Fatal("altered capture accepted")
			}
		})
	}
	_, key, err := loadHostKeys(keys)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	for _, fault := range []string{"none", "system", "oid", "binding_after_stop"} {
		x := l
		switch fault {
		case "system":
			x.SystemID = "98765"
		case "oid":
			x.DatabaseOID++
		case "binding_after_stop":
			x.ExecutorsStoppedAtUTC = r.Intent.CreatedAt.Format(time.RFC3339Nano)
		}
		path := filepath.Join(dir, "final-"+fault+"-link")
		if err := WriteHostCaptureLink(path, keys, x); err != nil {
			t.Fatal(err)
		}
		pair, err := VerifyHostCaptureLink(path, keys)
		if err != nil {
			t.Fatal(err)
		}
		stop := final
		stop.Capture = x
		stop.CaptureLinkSHA256 = pair.SHA256()
		body, _ := json.Marshal(stop)
		raw, _ := json.Marshal(signedSourceStop{stop, ed25519.Sign(key, append([]byte(sourceStopDomain), body...))})
		path = filepath.Join(dir, "final-"+fault)
		if err := writeHostPrivateFile(path, raw); err != nil {
			t.Fatal(err)
		}
		_, err = VerifyHostSourceStop(path, keys, pair, nil)
		if (err == nil) != (fault == "none") {
			t.Fatal("contradictory signed source stop", fault, err)
		}
	}
	for _, fault := range []string{"old_domain", "algorithm", "zero_bytes", "bad_time", "legacy_with_v2_fields"} {
		t.Run(fault, func(t *testing.T) {
			x := l
			domain := drainedCaptureDomain
			switch fault {
			case "old_domain":
				domain = captureDomain
			case "algorithm":
				x.ExportAlgorithm = "ordinary-export"
			case "zero_bytes":
				x.ExportWireBytes = 0
			case "bad_time":
				x.ExecutorsStoppedAtUTC = x.CapturedAt.Add(time.Hour).Format(time.RFC3339Nano)
			case "legacy_with_v2_fields":
				x.Version = 1
				domain = captureDomain
			}
			body, _ := json.Marshal(x)
			raw, _ := json.Marshal(signedCapture{x, ed25519.Sign(key, append([]byte(domain), body...))})
			path := filepath.Join(dir, fault)
			if err := writeHostPrivateFile(path, raw); err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyHostCaptureLink(path, keys); err == nil {
				t.Fatal("invalid signed profile accepted")
			}
		})
	}
}
