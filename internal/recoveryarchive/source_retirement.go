// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"time"
)

const sourceRetirementDomain = "RunVerge observed source retirement v1\x00"

// SourceRetirementBinding refers to a current database catalog observation
// taken by the trusted host before stopping the source. The capture link pins
// the source and snapshot. The intent pins the physical destination.
type SourceRetirementBinding struct {
	RestoreID              string `json:"restore_id"`
	IntentSHA256           string `json:"intent_sha256"`
	AdmissionReceiptSHA256 string `json:"admission_receipt_sha256"`
	CutProofSHA256         string `json:"cut_proof_sha256"`
}

type sourceRetirementSeal struct {
	Version           int                     `json:"version"`
	CaptureLinkSHA256 string                  `json:"capture_link_sha256"`
	Binding           SourceRetirementBinding `json:"binding"`
	SealedAt          time.Time               `json:"sealed_at"`
}

type signedSourceRetirement struct {
	Seal      sourceRetirementSeal `json:"seal"`
	Signature []byte               `json:"signature"`
}

func (s sourceRetirementSeal) valid(link *VerifiedCaptureLink) bool {
	if link == nil || s.Version != 1 || link.link.Version != 2 || !link.link.valid() ||
		s.CaptureLinkSHA256 != link.SHA256() || s.SealedAt.Before(link.link.CapturedAt) {
		return false
	}
	_, offset := s.SealedAt.Zone()
	b := s.Binding
	return offset == 0 && len(b.RestoreID) == len("restore_")+32 && strings.HasPrefix(b.RestoreID, "restore_") && identifier.MatchString(b.RestoreID) &&
		digestPattern.MatchString(b.IntentSHA256) && digestPattern.MatchString(b.AdmissionReceiptSHA256) && digestPattern.MatchString(b.CutProofSHA256)
}

// WriteHostSourceRetirementSeal is a create-only host signer. The production
// caller must obtain the binding from a fresh, exact source catalog check.
// This seal alone does not establish that the source is stopped.
func WriteHostSourceRetirementSeal(path, keysFile string, link *VerifiedCaptureLink, binding SourceRetirementBinding) error {
	s := sourceRetirementSeal{Version: 1, CaptureLinkSHA256: link.SHA256(), Binding: binding, SealedAt: time.Now().UTC()}
	if !s.valid(link) {
		return ErrOptions
	}
	_, key, err := loadHostKeys(keysFile)
	if err != nil {
		return err
	}
	defer clear(key)
	body, _ := json.Marshal(s)
	raw, _ := json.Marshal(signedSourceRetirement{s, ed25519.Sign(key, append([]byte(sourceRetirementDomain), body...))})
	return writeHostPrivateFile(path, raw)
}

type VerifiedSourceRetirementSeal struct {
	seal sourceRetirementSeal
	sha  string
}

func (v *VerifiedSourceRetirementSeal) SHA256() string {
	if v == nil {
		return ""
	}
	return v.sha
}

func (v *VerifiedSourceRetirementSeal) Binding() SourceRetirementBinding { return v.seal.Binding }

func VerifyHostSourceRetirementSeal(path, keysFile string, link *VerifiedCaptureLink) (*VerifiedSourceRetirementSeal, error) {
	raw, err := ReadHostPrivateFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) > 4<<10 {
		return nil, ErrInvalid
	}
	var signed signedSourceRetirement
	if json.Unmarshal(raw, &signed) != nil || !signed.Seal.valid(link) {
		return nil, ErrInvalid
	}
	canonical, _ := json.Marshal(signed)
	if !bytes.Equal(raw, canonical) {
		return nil, ErrInvalid
	}
	_, key, err := loadHostKeys(keysFile)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	body, _ := json.Marshal(signed.Seal)
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), append([]byte(sourceRetirementDomain), body...), signed.Signature) {
		return nil, ErrInvalid
	}
	return &VerifiedSourceRetirementSeal{signed.Seal, digest(raw)}, nil
}
