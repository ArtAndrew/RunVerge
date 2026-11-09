// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testRecoveryPeerJournalTrust(t *testing.T, dir string, stop *VerifiedSourceStop, sourceKey ed25519.PrivateKey) {
	t.Helper()
	pub, peerKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(peerKey)
	r := stop.Receipt()
	scope := RecoveryPeerScope{SourceID: r.Capture.SourceID, TargetID: "inst_" + strings.Repeat("f", 32), RestoreID: stop.retirement.Binding().RestoreID,
		SourceStopSHA256: stop.SHA256(), RetirementSealSHA256: r.RetirementSealSHA256, CaptureLinkSHA256: r.CaptureLinkSHA256, OriginalCutUTC: r.Capture.SelectedCutUTC}
	sha := strings.Repeat("f", 64)
	journal := RecoveryPeerJournal{Version: 1, Profile: RecoveryPeerJournalProfile, Scope: scope,
		FencedAtUTC: r.Capture.EvidenceCutUTC, CoveredThroughUTC: r.Source.Observation.ObservedAt.Format(time.RFC3339Nano),
		EgressProofSHA256: sha, Records: []RecoveryPeerJournalRecord{{Family: "action", OrganizationID: "org_test", RecordID: "act_test",
			DispatchSHA256: sha, EvidenceSHA256: sha, Disposition: "effect_observed"}}}
	write := func(name string, j RecoveryPeerJournal, key ed25519.PrivateKey) string {
		t.Helper()
		path := filepath.Join(dir, "peer-"+name)
		body, _ := json.Marshal(j)
		if err := WriteHostJSON(path, signedRecoveryPeerJournal{j, ed25519.Sign(key, append([]byte(recoveryPeerJournalDomain), body...))}); err != nil {
			t.Fatal(err)
		}
		return path
	}
	trust := filepath.Join(dir, "peer-trust")
	if err = WriteHostJSON(trust, recoveryPeerTrust{1, scope, pub}); err != nil {
		t.Fatal(err)
	}
	exact := write("exact", journal, peerKey)
	v, err := VerifyHostRecoveryPeerJournal(exact, trust, scope.TargetID, stop)
	if err != nil || v.SignerSHA256() != digest(pub) || v.Journal().Scope != scope {
		t.Fatal("exact independent evidence did not verify", err)
	}
	copy := v.Journal()
	copy.Records[0].EvidenceSHA256 = ""
	if v.Journal().Records[0].EvidenceSHA256 != sha {
		t.Fatal("caller changed verified evidence")
	}
	if _, err = VerifyHostRecoveryPeerJournal(exact, trust, "inst_"+strings.Repeat("c", 32), stop); err == nil {
		t.Fatal("journal verified for a different physical target")
	}
	for _, fault := range []string{"wrong-stop", "open-window", "duplicate", "wrong-signer"} {
		j := journal
		key := peerKey
		switch fault {
		case "wrong-stop":
			j.Scope.SourceStopSHA256 = sha
		case "open-window":
			j.CoveredThroughUTC = r.Source.Observation.ObservedAt.Add(-time.Nanosecond).Format(time.RFC3339Nano)
		case "duplicate":
			j.Records = append(j.Records, j.Records[0])
		case "wrong-signer":
			key = sourceKey
		}
		if _, err = VerifyHostRecoveryPeerJournal(write(fault, j, key), trust, scope.TargetID, stop); err == nil {
			t.Fatal("invalid or mismatched journal qualified", fault)
		}
	}
	same := filepath.Join(dir, "source-key-trust")
	if err = WriteHostJSON(same, recoveryPeerTrust{1, scope, sourceKey.Public().(ed25519.PublicKey)}); err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyHostRecoveryPeerJournal(write("source-key", journal, sourceKey), same, scope.TargetID, stop); err == nil {
		t.Fatal("source key substituted for the independent peer")
	}
}
