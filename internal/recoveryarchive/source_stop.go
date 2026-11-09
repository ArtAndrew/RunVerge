// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"time"

	"agentplatform.local/platform/internal/recoveryprocess"
)

const sourceStopDomain = "RunVerge observed source stop v1\x00"
const sourceStopProfile = "exclusive-local-compose-observed-stop-v1"
const retiredSourceStopDomain = "RunVerge retired source stop v2\x00"
const retiredSourceStopProfile = "exclusive-local-compose-retired-stop-v2"

type SourceStopReceipt struct {
	Version              int                              `json:"version"`
	Profile              string                           `json:"profile"`
	CaptureLinkSHA256    string                           `json:"capture_link_sha256"`
	Capture              CaptureLink                      `json:"capture"`
	Source               recoveryprocess.SourceStopRecord `json:"source"`
	WindowCovered        bool                             `json:"window_covered"`
	NewWorkAllowed       bool                             `json:"new_work_allowed"`
	RetirementSealSHA256 string                           `json:"retirement_seal_sha256,omitempty"`
}
type signedSourceStop struct {
	Receipt   SourceStopReceipt `json:"receipt"`
	Signature []byte            `json:"signature"`
}

func sourceStopValid(r SourceStopReceipt, link *VerifiedCaptureLink) bool {
	if link == nil || r.WindowCovered || r.NewWorkAllowed || !r.Capture.valid() || r.Capture != link.Link() || r.CaptureLinkSHA256 != link.SHA256() || !recoveryprocess.SourceStopRecordConsistent(r.Source) {
		return false
	}
	switch r.Version {
	case 1:
		if r.Profile != sourceStopProfile || r.RetirementSealSHA256 != "" {
			return false
		}
	case 2:
		if r.Profile != retiredSourceStopProfile || r.Capture.Version != 2 || !digestPattern.MatchString(r.RetirementSealSHA256) {
			return false
		}
	default:
		return false
	}
	p, b, o, l := r.Source.Intent, r.Source.Binding, r.Source.Observation, r.Capture
	binding, _ := json.Marshal(b)
	cut, err := time.Parse(time.RFC3339Nano, l.EvidenceCutUTC)
	selected, selectedErr := time.Parse(time.RFC3339Nano, l.SelectedCutUTC)
	if l.Version == 2 {
		stopped, stopErr := time.Parse(time.RFC3339Nano, l.ExecutorsStoppedAtUTC)
		if stopErr != nil || l.SystemID != b.SystemID || l.DatabaseOID != b.DatabaseOID || b.ObservedAt.After(stopped) {
			return false
		}
	}
	for _, stamp := range []time.Time{selected, cut, l.CapturedAt} {
		_, offset := stamp.Zone()
		if stamp.IsZero() || offset != 0 {
			return false
		}
	}
	return err == nil && selectedErr == nil && !selected.After(p.CreatedAt) && !b.ObservedAt.After(cut) && !cut.After(l.CapturedAt) && !l.CapturedAt.After(o.ObservedAt) &&
		l.OperationID == p.OperationID && l.SourceID == b.SourceID && l.BindingSHA256 == digest(binding) && l.SelectedSnapshotSHA256 == b.SnapshotSHA256
}

// WriteHostSourceStop accepts only a fresh live opaque observation and an
// authenticated capture pair. No caller-entered bool or historical receipt can
// substitute for this observation. The output is create-only private host data.
func WriteHostSourceStop(path, keysFile string, link *VerifiedCaptureLink, observed recoveryprocess.ObservedSourceStop) error {
	return writeHostSourceStop(path, keysFile, link, observed, nil)
}

func WriteHostRetiredSourceStop(path, keysFile string, link *VerifiedCaptureLink, observed recoveryprocess.ObservedSourceStop, seal *VerifiedSourceRetirementSeal) error {
	if seal == nil {
		return ErrOptions
	}
	return writeHostSourceStop(path, keysFile, link, observed, seal)
}

func writeHostSourceStop(path, keysFile string, link *VerifiedCaptureLink, observed recoveryprocess.ObservedSourceStop, seal *VerifiedSourceRetirementSeal) error {
	r := SourceStopReceipt{Version: 1, Profile: sourceStopProfile, CaptureLinkSHA256: link.SHA256(), Capture: link.Link(), Source: observed.Record()}
	if seal != nil {
		r.Version, r.Profile, r.RetirementSealSHA256 = 2, retiredSourceStopProfile, seal.SHA256()
	}
	age := time.Since(r.Source.Observation.ObservedAt)
	if !sourceStopValid(r, link) || !r.matchesRetirement(seal) || age < 0 || age > 30*time.Second {
		return ErrOptions
	}
	_, key, err := loadHostKeys(keysFile)
	if err != nil {
		return err
	}
	defer clear(key)
	body, _ := json.Marshal(r)
	raw, _ := json.Marshal(signedSourceStop{r, ed25519.Sign(key, append([]byte(r.signingDomain()), body...))})
	if len(raw) > 128<<10 {
		return ErrOptions
	}
	return writeHostPrivateFile(path, raw)
}

func (r SourceStopReceipt) signingDomain() string {
	if r.Version == 2 {
		return retiredSourceStopDomain
	}
	return sourceStopDomain
}

func (r SourceStopReceipt) matchesRetirement(seal *VerifiedSourceRetirementSeal) bool {
	if seal == nil {
		return r.Version == 1
	}
	return r.Version == 2 && r.RetirementSealSHA256 == seal.SHA256() &&
		r.CaptureLinkSHA256 == seal.seal.CaptureLinkSHA256 && !r.Source.Observation.ObservedAt.Before(seal.seal.SealedAt)
}

type VerifiedSourceStop struct {
	receipt      SourceStopReceipt
	raw          []byte
	retirement   *VerifiedSourceRetirementSeal
	signerSHA256 string
}

func (v *VerifiedSourceStop) Bytes() []byte {
	if v == nil {
		return nil
	}
	return bytes.Clone(v.raw)
}
func (v *VerifiedSourceStop) SHA256() string {
	if v == nil {
		return ""
	}
	return digest(v.raw)
}
func (v *VerifiedSourceStop) Receipt() SourceStopReceipt {
	if v == nil {
		return SourceStopReceipt{}
	}
	// Decode from the retained canonical bytes so all nested slices are copied.
	var signed signedSourceStop
	_ = json.Unmarshal(v.raw, &signed)
	return signed.Receipt
}

// VerifyHostSourceStop authenticates the exact capture and, for V2, the exact
// retirement seal. Current state still requires RecheckHostSourceStop.
func VerifyHostSourceStop(path, keysFile string, link *VerifiedCaptureLink, seal *VerifiedSourceRetirementSeal) (*VerifiedSourceStop, error) {
	raw, err := ReadHostPrivateFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) > 128<<10 {
		return nil, ErrInvalid
	}
	var signed signedSourceStop
	if json.Unmarshal(raw, &signed) != nil || !sourceStopValid(signed.Receipt, link) || !signed.Receipt.matchesRetirement(seal) {
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
	body, _ := json.Marshal(signed.Receipt)
	pub := key.Public().(ed25519.PublicKey)
	if !ed25519.Verify(pub, append([]byte(signed.Receipt.signingDomain()), body...), signed.Signature) {
		return nil, ErrInvalid
	}
	return &VerifiedSourceStop{receipt: signed.Receipt, raw: bytes.Clone(raw), retirement: seal, signerSHA256: digest(pub)}, nil
}

// RecheckHostSourceStop is read-only, has no stop/retry path and grants no work
// permission. A restart followed by another stop changes the pinned generation
// and is refused even when the current container IDs and stopped flags match.
func RecheckHostSourceStop(ctx context.Context, client *recoveryprocess.Client, receipt *VerifiedSourceStop, link *VerifiedCaptureLink) (recoveryprocess.ObservedSourceStop, error) {
	if client == nil || receipt == nil || !sourceStopValid(receipt.receipt, link) {
		return recoveryprocess.ObservedSourceStop{}, ErrInvalid
	}
	r := receipt.Receipt().Source
	fresh, err := client.ObserveSourceStop(ctx, r.Intent, r.Binding)
	if err != nil || fresh.Record().Observation.ObservedAt.Before(r.Observation.ObservedAt) || !recoveryprocess.SameSourceStop(r, fresh.Record()) {
		return recoveryprocess.ObservedSourceStop{}, ErrInvalid
	}
	return fresh, nil
}

// CurrentRetiredSourceStop can only be produced by a fresh observation of
// the exact sealed Docker generation. It is checked again at admission.
type CurrentRetiredSourceStop struct {
	stopSHA256 string
	observedAt time.Time
}

func RecheckHostRetiredSourceStop(ctx context.Context, client *recoveryprocess.Client, receipt *VerifiedSourceStop, link *VerifiedCaptureLink) (CurrentRetiredSourceStop, error) {
	if receipt == nil || receipt.retirement == nil || receipt.receipt.Version != 2 {
		return CurrentRetiredSourceStop{}, ErrInvalid
	}
	o, err := RecheckHostSourceStop(ctx, client, receipt, link)
	if err != nil {
		return CurrentRetiredSourceStop{}, err
	}
	return CurrentRetiredSourceStop{receipt.SHA256(), o.Record().Observation.ObservedAt}, nil
}

func (s CurrentRetiredSourceStop) Matches(sha256 string, after time.Time) bool {
	now := time.Now()
	return digestPattern.MatchString(sha256) && s.stopSHA256 == sha256 &&
		!s.observedAt.Before(after) && !s.observedAt.After(now) && now.Sub(s.observedAt) <= 30*time.Second
}
