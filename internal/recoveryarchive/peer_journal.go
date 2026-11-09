// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

const recoveryPeerJournalDomain = "RunVerge independent egress journal v1\x00"
const RecoveryPeerJournalProfile = "exclusive-source-egress-v1"
const recoveryPeerJournalMaxRecords = 2048

var ErrRecoveryPeerJournal = errors.New("independent recovery journal or its exact scope could not be authenticated")

var recoveryPeerRecordID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$`)

// A trust file pins an independent egress observer for one exact transition.
// Its key must be selected out of band, outside both archives and the journal.
type RecoveryPeerScope struct {
	SourceID             string `json:"source_installation_id"`
	TargetID             string `json:"target_installation_id"`
	RestoreID            string `json:"restore_id"`
	SourceStopSHA256     string `json:"source_stop_sha256"`
	RetirementSealSHA256 string `json:"retirement_seal_sha256"`
	CaptureLinkSHA256    string `json:"capture_link_sha256"`
	OriginalCutUTC       string `json:"original_cut_utc"`
}

type recoveryPeerTrust struct {
	Version   int               `json:"version"`
	Scope     RecoveryPeerScope `json:"scope"`
	PublicKey []byte            `json:"public_key"`
}

// The observer attests closure of its exclusive egress boundary. Dispositions
// refer to effects only. They carry no amount, invoice or release authority.
type RecoveryPeerJournal struct {
	Version           int                         `json:"version"`
	Profile           string                      `json:"profile"`
	Scope             RecoveryPeerScope           `json:"scope"`
	FencedAtUTC       string                      `json:"egress_fenced_at_utc"`
	CoveredThroughUTC string                      `json:"covered_through_utc"`
	EgressProofSHA256 string                      `json:"egress_proof_sha256"`
	Records           []RecoveryPeerJournalRecord `json:"records"`
}

type RecoveryPeerJournalRecord struct {
	Family         string `json:"family"`
	OrganizationID string `json:"organization_id"`
	RecordID       string `json:"record_id"`
	DispatchSHA256 string `json:"dispatch_sha256"`
	EvidenceSHA256 string `json:"evidence_sha256"`
	Disposition    string `json:"disposition"`
}

type signedRecoveryPeerJournal struct {
	Journal   RecoveryPeerJournal `json:"journal"`
	Signature []byte              `json:"signature"`
}

type VerifiedRecoveryPeerJournal struct {
	journal RecoveryPeerJournal
	sha256  string
	signer  string
}

func (v *VerifiedRecoveryPeerJournal) Journal() RecoveryPeerJournal {
	journal := v.journal
	journal.Records = append([]RecoveryPeerJournalRecord{}, journal.Records...)
	return journal
}

func (v *VerifiedRecoveryPeerJournal) SHA256() string { return v.sha256 }

func (v *VerifiedRecoveryPeerJournal) SignerSHA256() string { return v.signer }

func validRecoveryPeerRecord(r RecoveryPeerJournalRecord) bool {
	if !recoveryPeerRecordID.MatchString(r.RecordID) || !digestPattern.MatchString(r.EvidenceSHA256) ||
		(r.Disposition != "effect_observed" && r.Disposition != "no_effect_confirmed" && r.Disposition != "unknown") {
		return false
	}
	if r.Family == "unmatched" {
		return r.DispatchSHA256 == "" && (r.OrganizationID == "" || recoveryPeerRecordID.MatchString(r.OrganizationID))
	}
	switch r.Family {
	case "action", "tool_execution", "configuration_verification", "webhook_attempt":
		return recoveryPeerRecordID.MatchString(r.OrganizationID) && digestPattern.MatchString(r.DispatchSHA256)
	default:
		return false
	}
}

func validRecoveryPeerJournal(j RecoveryPeerJournal, scope RecoveryPeerScope, stoppedAt time.Time) bool {
	fence, fenceErr := time.Parse(time.RFC3339Nano, j.FencedAtUTC)
	through, throughErr := time.Parse(time.RFC3339Nano, j.CoveredThroughUTC)
	cut, cutErr := time.Parse(time.RFC3339Nano, scope.OriginalCutUTC)
	if j.Version != 1 || j.Profile != RecoveryPeerJournalProfile || j.Scope != scope || j.Records == nil ||
		len(j.Records) > recoveryPeerJournalMaxRecords || !digestPattern.MatchString(j.EgressProofSHA256) ||
		fenceErr != nil || throughErr != nil || cutErr != nil ||
		j.FencedAtUTC != fence.UTC().Format(time.RFC3339Nano) || j.CoveredThroughUTC != through.UTC().Format(time.RFC3339Nano) ||
		fence.Before(cut) || through.Before(fence) || through.Before(stoppedAt) || through.After(time.Now()) {
		return false
	}
	seen := make(map[[3]string]bool, len(j.Records))
	for _, r := range j.Records {
		key := [3]string{r.Family, r.OrganizationID, r.RecordID}
		if !validRecoveryPeerRecord(r) || seen[key] {
			return false
		}
		seen[key] = true
	}
	return true
}

func RecoveryPeerScopeFor(targetID string, stop *VerifiedSourceStop) (RecoveryPeerScope, error) {
	if stop == nil || stop.retirement == nil || stop.receipt.Version != 2 ||
		len(targetID) != len("inst_")+32 || !strings.HasPrefix(targetID, "inst_") || !identifier.MatchString(targetID) {
		return RecoveryPeerScope{}, ErrRecoveryPeerJournal
	}
	r := stop.receipt
	scope := RecoveryPeerScope{SourceID: r.Capture.SourceID, TargetID: targetID, RestoreID: stop.retirement.Binding().RestoreID,
		SourceStopSHA256: stop.SHA256(), RetirementSealSHA256: r.RetirementSealSHA256, CaptureLinkSHA256: r.CaptureLinkSHA256, OriginalCutUTC: r.Capture.SelectedCutUTC}
	if scope.SourceID == scope.TargetID {
		return RecoveryPeerScope{}, ErrRecoveryPeerJournal
	}
	return scope, nil
}

// VerifyHostRecoveryPeerJournal does not grant an execution permit. The
// target must still reconcile this exact journal against its imported ledger.
func VerifyHostRecoveryPeerJournal(path, trustPath, targetID string, stop *VerifiedSourceStop) (*VerifiedRecoveryPeerJournal, error) {
	scope, err := RecoveryPeerScopeFor(targetID, stop)
	if err != nil || path == trustPath {
		return nil, ErrRecoveryPeerJournal
	}
	trustRaw, err := readHostPrivateFile(trustPath, 4096)
	if err != nil {
		return nil, ErrRecoveryPeerJournal
	}
	defer clear(trustRaw)
	var trust recoveryPeerTrust
	if json.Unmarshal(trustRaw, &trust) != nil || trust.Version != 1 || trust.Scope != scope || len(trust.PublicKey) != ed25519.PublicKeySize {
		return nil, ErrRecoveryPeerJournal
	}
	canonicalTrust, _ := json.Marshal(trust)
	var compact bytes.Buffer
	if json.Compact(&compact, trustRaw) != nil || !bytes.Equal(compact.Bytes(), canonicalTrust) {
		return nil, ErrRecoveryPeerJournal
	}
	signer := digest(trust.PublicKey)
	if signer == stop.signerSHA256 {
		return nil, ErrRecoveryPeerJournal
	}
	raw, err := ReadHostPrivateFile(path)
	if err != nil {
		return nil, ErrRecoveryPeerJournal
	}
	defer clear(raw)
	var signed signedRecoveryPeerJournal
	if json.Unmarshal(raw, &signed) != nil || !validRecoveryPeerJournal(signed.Journal, scope, stop.receipt.Source.Observation.ObservedAt) {
		return nil, ErrRecoveryPeerJournal
	}
	canonical, _ := json.Marshal(signed)
	body, _ := json.Marshal(signed.Journal)
	if !bytes.Equal(raw, canonical) || !ed25519.Verify(trust.PublicKey, append([]byte(recoveryPeerJournalDomain), body...), signed.Signature) {
		return nil, ErrRecoveryPeerJournal
	}
	return &VerifiedRecoveryPeerJournal{journal: signed.Journal, sha256: digest(raw), signer: signer}, nil
}
