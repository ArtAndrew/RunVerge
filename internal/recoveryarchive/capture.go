// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"strconv"
	"time"

	"agentplatform.local/platform/internal/recoveryprocess"
)

const captureDomain = "RunVerge source evidence link v1\x00"
const drainedCaptureDomain = "RunVerge drained source evidence link v2\x00"
const DrainedCaptureAlgorithm = "exclusive-compose-pg-drain-nonce-rr-export-v1"

// CaptureLink authenticates the pairing of two archives. It attests capture,
// not final source shutdown, external settlement or permission for new work.
type CaptureLink struct {
	Version                int       `json:"version"`
	OperationID            string    `json:"operation_id"`
	SourceID               string    `json:"source_id"`
	SelectedReceiptSHA256  string    `json:"selected_receipt_sha256"`
	SelectedSnapshotSHA256 string    `json:"selected_snapshot_sha256"`
	SelectedCutUTC         string    `json:"selected_cut_utc"`
	BindingSHA256          string    `json:"binding_sha256"`
	StoppedExecutorsSHA256 string    `json:"stopped_executors_sha256"`
	EvidenceReceiptSHA256  string    `json:"evidence_receipt_sha256"`
	EvidenceSnapshotSHA256 string    `json:"evidence_snapshot_sha256"`
	EvidenceCutUTC         string    `json:"evidence_cut_utc"`
	CapturedAt             time.Time `json:"captured_at"`
	// V2 attests the trusted host export path; V1 bytes remain unchanged.
	ExportAlgorithm       string `json:"export_algorithm,omitempty"`
	SystemID              string `json:"system_id,omitempty"`
	DatabaseOID           uint32 `json:"database_oid,omitempty"`
	ExportWireBytes       int64  `json:"export_wire_bytes,omitempty"`
	EvidenceSchemaSHA256  string `json:"evidence_schema_sha256,omitempty"`
	EvidenceBackupID      string `json:"evidence_backup_id,omitempty"`
	ExecutorsStoppedAtUTC string `json:"executors_stopped_at_utc,omitempty"`
}
type signedCapture struct {
	Link      CaptureLink `json:"link"`
	Signature []byte      `json:"signature"`
}

func (l CaptureLink) valid() bool {
	a, e := time.Parse(time.RFC3339Nano, l.SelectedCutUTC)
	b, f := time.Parse(time.RFC3339Nano, l.EvidenceCutUTC)
	if (l.Version != 1 && l.Version != 2) || len(l.OperationID) != 32 || !identifier.MatchString("op_"+l.OperationID) || !identifier.MatchString(l.SourceID) || e != nil || f != nil || !b.After(a) || l.CapturedAt.IsZero() {
		return false
	}
	for _, d := range []string{l.SelectedReceiptSHA256, l.SelectedSnapshotSHA256, l.BindingSHA256, l.StoppedExecutorsSHA256, l.EvidenceReceiptSHA256, l.EvidenceSnapshotSHA256} {
		if !digestPattern.MatchString(d) {
			return false
		}
	}
	if l.Version == 1 {
		return l.ExportAlgorithm == "" && l.SystemID == "" && l.DatabaseOID == 0 && l.ExportWireBytes == 0 && l.EvidenceSchemaSHA256 == "" && l.EvidenceBackupID == "" && l.ExecutorsStoppedAtUTC == ""
	}
	system, err := strconv.ParseUint(l.SystemID, 10, 64)
	stopped, errTime := time.Parse(time.RFC3339Nano, l.ExecutorsStoppedAtUTC)
	if l.ExportAlgorithm != DrainedCaptureAlgorithm || err != nil || system == 0 || strconv.FormatUint(system, 10) != l.SystemID || l.DatabaseOID == 0 || l.ExportWireBytes <= 0 || !digestPattern.MatchString(l.EvidenceSchemaSHA256) || !identifier.MatchString(l.EvidenceBackupID) || errTime != nil || stopped.IsZero() || a.After(stopped) || stopped.After(b) || b.After(l.CapturedAt) {
		return false
	}
	for _, stamp := range []time.Time{a, b, stopped, l.CapturedAt} {
		_, offset := stamp.Zone()
		if stamp.IsZero() || offset != 0 {
			return false
		}
	}
	return true
}

func captureSigningDomain(version int) string {
	if version == 2 {
		return drainedCaptureDomain
	}
	return captureDomain
}
func WriteHostCaptureLink(path, keysFile string, link CaptureLink) error {
	if !link.valid() {
		return ErrOptions
	}
	_, key, err := loadHostKeys(keysFile)
	if err != nil {
		return err
	}
	defer clear(key)
	raw, _ := json.Marshal(link)
	return WriteHostJSON(path, signedCapture{link, ed25519.Sign(key, append([]byte(captureSigningDomain(link.Version)), raw...))})
}
func VerifyHostCaptureLink(path, keysFile string) (*VerifiedCaptureLink, error) {
	var zero *VerifiedCaptureLink
	raw, err := ReadHostPrivateFile(path)
	if err != nil {
		return zero, err
	}
	defer clear(raw)
	var signed signedCapture
	if json.Unmarshal(raw, &signed) != nil || !signed.Link.valid() {
		return zero, ErrInvalid
	}
	canonical, _ := json.Marshal(signed)
	if !bytes.Equal(raw, canonical) {
		return zero, ErrInvalid
	}
	_, key, err := loadHostKeys(keysFile)
	if err != nil {
		return zero, err
	}
	defer clear(key)
	body, _ := json.Marshal(signed.Link)
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), append([]byte(captureSigningDomain(signed.Link.Version)), body...), signed.Signature) {
		return zero, ErrInvalid
	}
	return &VerifiedCaptureLink{link: signed.Link, raw: append([]byte(nil), raw...)}, nil
}

// MatchesDrainedSource checks signed V2 provenance and the retained pre-capture
// observation. It grants no authority, checks no current source state, and cannot
// prove the host signer truthful or settle external effects. V1 never qualifies.
func (v *VerifiedCaptureLink) MatchesDrainedSource(r recoveryprocess.SourceStopRecord) bool {
	if v == nil || v.link.Version != 2 || !v.link.valid() || !recoveryprocess.SourceCaptureRecordConsistent(r) {
		return false
	}
	l := v.link
	b, _ := json.Marshal(r.Binding)
	o, _ := json.Marshal(r.Observation)
	selected, _ := time.Parse(time.RFC3339Nano, l.SelectedCutUTC)
	return l.OperationID == r.Intent.OperationID && l.SourceID == r.Binding.SourceID && l.SelectedSnapshotSHA256 == r.Binding.SnapshotSHA256 && l.BindingSHA256 == digest(b) && l.StoppedExecutorsSHA256 == digest(o) && l.SystemID == r.Binding.SystemID && l.DatabaseOID == r.Binding.DatabaseOID && l.ExecutorsStoppedAtUTC == r.Observation.ObservedAt.UTC().Format(time.RFC3339Nano) && !selected.After(r.Intent.CreatedAt)
}

// MatchesEvidenceManifest binds every attested archive field, including the
// V2 export metadata. Archive authenticity is checked separately by its reader.
func (v *VerifiedCaptureLink) MatchesEvidenceManifest(m Manifest) bool {
	if v == nil || !v.link.valid() {
		return false
	}
	l := v.link
	if m.PlaintextSHA256 != l.EvidenceSnapshotSHA256 || m.Metadata.SourceID != l.SourceID || m.Metadata.CutUTC != l.EvidenceCutUTC {
		return false
	}
	return l.Version == 1 || (m.PlaintextBytes == l.ExportWireBytes && m.Metadata.SchemaSHA256 == l.EvidenceSchemaSHA256 && m.Metadata.BackupID == l.EvidenceBackupID)
}

// VerifiedCaptureLink cannot be constructed from unauthenticated caller fields.
type VerifiedCaptureLink struct {
	link                        CaptureLink
	raw                         []byte
	retiredStop, retirementSeal string
	retirementSigner            string
}

// WithRetiredSourceStop returns a copy with the historical authenticated stop
// binding. It makes no current Docker observation and grants no work authority.
func (v *VerifiedCaptureLink) WithRetiredSourceStop(stop *VerifiedSourceStop) (*VerifiedCaptureLink, error) {
	if v == nil || stop == nil || stop.retirement == nil || stop.receipt.Version != 2 ||
		!sourceStopValid(stop.receipt, v) || !stop.receipt.matchesRetirement(stop.retirement) {
		return nil, ErrInvalid
	}
	bound := *v
	bound.retiredStop, bound.retirementSeal = stop.SHA256(), stop.retirement.SHA256()
	bound.retirementSigner = stop.signerSHA256
	return &bound, nil
}

func (v *VerifiedCaptureLink) RetirementSignerSHA256() string {
	if v == nil {
		return ""
	}
	return v.retirementSigner
}

func (v *VerifiedCaptureLink) RetiredSourceHashes() (string, string) {
	if v == nil {
		return "", ""
	}
	return v.retiredStop, v.retirementSeal
}

func (v *VerifiedCaptureLink) Link() CaptureLink {
	if v == nil {
		return CaptureLink{}
	}
	return v.link
}
func (v *VerifiedCaptureLink) Bytes() []byte {
	if v == nil {
		return nil
	}
	return append([]byte(nil), v.raw...)
}
func (v *VerifiedCaptureLink) SHA256() string {
	if v == nil {
		return ""
	}
	return digest(v.raw)
}
func ReadHostCaptureLink(path, keysFile string) (CaptureLink, error) {
	v, err := VerifyHostCaptureLink(path, keysFile)
	if err != nil {
		return CaptureLink{}, err
	}
	return v.Link(), nil
}
