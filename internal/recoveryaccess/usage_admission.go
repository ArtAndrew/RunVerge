// SPDX-License-Identifier: Elastic-2.0
package recoveryaccess

import (
	"context"
	"encoding/json"
	"time"

	"agentplatform.local/platform/internal/modelgateway"
)

// UsageReceiptConsistent verifies retained metadata and byte commitments. It
// establishes no network origin, current authority or provider account ownership,
// and cannot manufacture an opaque observation for admission.
func UsageReceiptConsistent(r UsageAcquisitionReceipt, raw []byte) bool {
	p, q := r.Policy, r.Request
	stamp, err := time.Parse(time.RFC3339Nano, r.PolicyConfirmedAt)
	if r.Version != 1 || r.AccountMappingMethod != "current_host_operator_attestation" || !validBinding(q.Binding) || !usagePolicyValid(p) || !p.Enabled || r.PolicyEpoch == 0 || !lowerHex(r.PolicyOperationID, 32) || !label(r.OperatorReference) || err != nil || stamp.UTC().Format(time.RFC3339Nano) != r.PolicyConfirmedAt || r.Observation.ObservedAt.Before(stamp) || !r.Observation.ObservedAt.Before(p.ExpiresAt) {
		return false
	}
	if !lowerHex(q.SourcePlanSHA256, 64) || q.SourceID != p.SourceID || q.OrganizationID != p.OrganizationID || q.ModelConnectionID != p.ModelConnectionID || q.ModelConnectionVersion != p.ModelConnectionVersion || q.ConnectionIdentitySHA256 != p.ConnectionIdentitySHA256 || q.ArchivedProviderAccount != p.ArchivedProviderAccount || q.GenerationID != r.Observation.GenerationID || q.Model != r.Observation.Model || !modelgateway.UsageObservationMatchesBytes(raw, r.Observation) {
		return false
	}
	info := HostInfo{Version: 1, Binding: q.Binding, Epoch: r.PolicyEpoch, OperationID: r.PolicyOperationID, OperatorReference: r.OperatorReference, ConfirmedAt: r.PolicyConfirmedAt, Purpose: usageHostPurpose, Usage: &p}
	encoded, err := json.Marshal(info)
	if err != nil || stateHash(encoded) != r.PolicySHA256 {
		return false
	}
	expected := r.ReceiptSHA256
	r.ReceiptSHA256 = ""
	encoded, err = json.Marshal(r)
	return err == nil && lowerHex(expected, 64) && stateHash(encoded) == expected
}

// Admission policy is a separate current check, not a rewrite of acquisition
// history. This version requires the exact policy that authorized acquisition;
// rotation/revocation requires a fresh acquisition before a NEW insertion.
type UsagePolicyAdmission struct {
	PolicySHA256      string    `json:"policy_sha256"`
	PolicyEpoch       uint64    `json:"policy_epoch"`
	PolicyOperationID string    `json:"policy_operation_id"`
	AdmittedAt        time.Time `json:"admitted_at"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// WithUsageAdmission grants no source/archive proof and exposes no credential.
// Only the trusted storage coordinator may use this callback. It must rederive
// source/target provenance and commit using ctx while the current-policy shared
// lock is held. Lock order: host authority FIRST, then the DB recovery gate.
// A callback commit followed by any returned error is an UNKNOWN outcome; the
// caller must discard success output and inspect its exact operation, not retry.
func WithUsageAdmission(ctx context.Context, path string, o AcquiredUsageObservation, admit func(context.Context, UsagePolicyAdmission) error) error {
	if ctx == nil || admit == nil {
		return ErrUsageDenied
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	d, err := openHostDirectory(ctx, path, false)
	if err != nil {
		return err
	}
	defer d.close()
	s, pending, err := d.load()
	if err != nil {
		return err
	}
	if pending != nil {
		return ErrPending
	}
	r := o.receipt
	if !UsageReceiptConsistent(r, o.raw) {
		return ErrUsageDenied
	}
	if !usageRequestMatches(s, r.Request, time.Now()) || len(o.raw) == 0 || stateHash(o.raw) != r.Observation.ResponseSHA256 {
		return ErrUsageDenied
	}
	info, _ := json.Marshal(s.Info)
	if r.Version != 1 || r.AccountMappingMethod != "current_host_operator_attestation" || r.PolicySHA256 != stateHash(info) || r.PolicyEpoch != s.Info.Epoch || r.PolicyOperationID != s.Info.OperationID || r.Policy != *s.Info.Usage {
		return ErrUsageDenied
	}
	expected := r.ReceiptSHA256
	r.ReceiptSHA256 = ""
	commitment, _ := json.Marshal(r)
	if !lowerHex(expected, 64) || stateHash(commitment) != expected {
		return ErrUsageDenied
	}
	ctx, stop := context.WithDeadline(ctx, s.Info.Usage.ExpiresAt)
	defer stop()
	if ctx.Err() != nil {
		return ErrUsageDenied
	}
	current := UsagePolicyAdmission{PolicySHA256: stateHash(info), PolicyEpoch: s.Info.Epoch, PolicyOperationID: s.Info.OperationID, AdmittedAt: time.Now().UTC(), ExpiresAt: s.Info.Usage.ExpiresAt}
	if err = admit(ctx, current); err != nil {
		return err
	}
	if ctx.Err() != nil || !usageRequestMatches(s, r.Request, time.Now()) {
		return ErrUsageDenied
	}
	return nil
}
