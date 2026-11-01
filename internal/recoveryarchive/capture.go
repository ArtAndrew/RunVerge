// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"time"
)

const captureDomain = "RunVerge source evidence link v1\x00"

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
}
type signedCapture struct {
	Link      CaptureLink `json:"link"`
	Signature []byte      `json:"signature"`
}

func (l CaptureLink) valid() bool {
	a, e := time.Parse(time.RFC3339Nano, l.SelectedCutUTC)
	b, f := time.Parse(time.RFC3339Nano, l.EvidenceCutUTC)
	if l.Version != 1 || len(l.OperationID) != 32 || !identifier.MatchString("op_"+l.OperationID) || !identifier.MatchString(l.SourceID) || e != nil || f != nil || !b.After(a) || l.CapturedAt.IsZero() {
		return false
	}
	for _, d := range []string{l.SelectedReceiptSHA256, l.SelectedSnapshotSHA256, l.BindingSHA256, l.StoppedExecutorsSHA256, l.EvidenceReceiptSHA256, l.EvidenceSnapshotSHA256} {
		if !digestPattern.MatchString(d) {
			return false
		}
	}
	return true
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
	return WriteHostJSON(path, signedCapture{link, ed25519.Sign(key, append([]byte(captureDomain), raw...))})
}
func ReadHostCaptureLink(path, keysFile string) (CaptureLink, error) {
	var zero CaptureLink
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
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), append([]byte(captureDomain), body...), signed.Signature) {
		return zero, ErrInvalid
	}
	return signed.Link, nil
}
