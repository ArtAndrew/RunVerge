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

type SourceStopReceipt struct {
	Version           int                              `json:"version"`
	Profile           string                           `json:"profile"`
	CaptureLinkSHA256 string                           `json:"capture_link_sha256"`
	Capture           CaptureLink                      `json:"capture"`
	Source            recoveryprocess.SourceStopRecord `json:"source"`
	WindowCovered     bool                             `json:"window_covered"`
	NewWorkAllowed    bool                             `json:"new_work_allowed"`
}
type signedSourceStop struct {
	Receipt   SourceStopReceipt `json:"receipt"`
	Signature []byte            `json:"signature"`
}

func sourceStopValid(r SourceStopReceipt, link *VerifiedCaptureLink) bool {
	if link == nil || r.Version != 1 || r.Profile != sourceStopProfile || r.WindowCovered || r.NewWorkAllowed || !r.Capture.valid() || r.Capture != link.Link() || r.CaptureLinkSHA256 != link.SHA256() || !recoveryprocess.SourceStopRecordConsistent(r.Source) {
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
	r := SourceStopReceipt{Version: 1, Profile: sourceStopProfile, CaptureLinkSHA256: link.SHA256(), Capture: link.Link(), Source: observed.Record()}
	age := time.Since(r.Source.Observation.ObservedAt)
	if !sourceStopValid(r, link) || age < 0 || age > 30*time.Second {
		return ErrOptions
	}
	_, key, err := loadHostKeys(keysFile)
	if err != nil {
		return err
	}
	defer clear(key)
	body, _ := json.Marshal(r)
	raw, _ := json.Marshal(signedSourceStop{r, ed25519.Sign(key, append([]byte(sourceStopDomain), body...))})
	if len(raw) > 128<<10 {
		return ErrOptions
	}
	return writeHostPrivateFile(path, raw)
}

type VerifiedSourceStop struct {
	receipt SourceStopReceipt
	raw     []byte
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

// VerifyHostSourceStop proves historical authenticity and pair binding only.
// No claim about current state follows without RecheckHostSourceStop.
func VerifyHostSourceStop(path, keysFile string, link *VerifiedCaptureLink) (*VerifiedSourceStop, error) {
	raw, err := ReadHostPrivateFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) > 128<<10 {
		return nil, ErrInvalid
	}
	var signed signedSourceStop
	if json.Unmarshal(raw, &signed) != nil || !sourceStopValid(signed.Receipt, link) {
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
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), append([]byte(sourceStopDomain), body...), signed.Signature) {
		return nil, ErrInvalid
	}
	return &VerifiedSourceStop{signed.Receipt, bytes.Clone(raw)}, nil
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
