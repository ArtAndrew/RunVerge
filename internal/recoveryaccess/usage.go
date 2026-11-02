// SPDX-License-Identifier: Elastic-2.0
package recoveryaccess

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"agentplatform.local/platform/internal/modelgateway"
)

const usageHostPurpose = "openrouter-usage-host-v1"

var ErrUsageDenied = errors.New("current provider usage authority unavailable")
var usageGeneration = regexp.MustCompile(`^gen-[0-9A-Za-z-]+$`)

// This is a CURRENT local operator's explicit account/credential mapping, not
// proof from the provider. Provision outside the archives and restored database.
// Disabled policies retain their public mapping but contain no credential.
type UsageHostPolicy struct {
	SourceID                 string    `json:"source_id"`
	OrganizationID           string    `json:"organization_id"`
	ModelConnectionID        string    `json:"model_connection_id"`
	ModelConnectionVersion   int       `json:"model_connection_version"`
	ConnectionIdentitySHA256 string    `json:"connection_identity_sha256"`
	ArchivedProviderAccount  string    `json:"archived_provider_account"`
	CredentialReference      string    `json:"credential_reference"`
	CredentialSHA256         string    `json:"credential_sha256"`
	ExpiresAt                time.Time `json:"expires_at"`
	Enabled                  bool      `json:"enabled"`
}

// Only the trusted storage coordinator derives this from verified history.
// A request/digest supplied by a caller is not independently a source proof.
type UsageRequest struct {
	Binding                  Binding `json:"binding"`
	SourcePlanSHA256         string  `json:"source_plan_sha256"`
	SourceID                 string  `json:"source_id"`
	OrganizationID           string  `json:"organization_id"`
	ModelConnectionID        string  `json:"model_connection_id"`
	ModelConnectionVersion   int     `json:"model_connection_version"`
	ConnectionIdentitySHA256 string  `json:"connection_identity_sha256"`
	ArchivedProviderAccount  string  `json:"archived_provider_account"`
	GenerationID             string  `json:"generation_id"`
	Model                    string  `json:"model"`
}

type UsageAcquisitionReceipt struct {
	Version              int                                 `json:"version"`
	ReceiptSHA256        string                              `json:"receipt_sha256"`
	Request              UsageRequest                        `json:"request"`
	Policy               UsageHostPolicy                     `json:"policy"`
	PolicyEpoch          uint64                              `json:"policy_epoch"`
	PolicySHA256         string                              `json:"policy_sha256"`
	PolicyOperationID    string                              `json:"policy_operation_id"`
	PolicyConfirmedAt    string                              `json:"policy_confirmed_at"`
	OperatorReference    string                              `json:"operator_reference"`
	AccountMappingMethod string                              `json:"account_mapping_method"`
	Observation          modelgateway.UsageObservationRecord `json:"observation"`
}

// Opaque acquisition only. It proves neither final cost nor permission to resume.
// A later durable ingest must revalidate target and current policy. The target
// accounting head is NOT locked or asserted unchanged during the HTTP request.
type AcquiredUsageObservation struct {
	receipt UsageAcquisitionReceipt
	raw     []byte
}

func (o AcquiredUsageObservation) Receipt() UsageAcquisitionReceipt { return o.receipt }
func (o AcquiredUsageObservation) RawResponse() []byte              { return bytes.Clone(o.raw) }

func usageText(s string, n int) bool {
	if len(s) == 0 || len(s) > n || !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}
func usageCredentialValid(s string) bool {
	if len(s) == 0 || len(s) > 8192 {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func usageJSONContainsCredential(raw []byte, key string) bool {
	if key == "" {
		return false
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return true
	}
	var contains func(any) bool
	contains = func(v any) bool {
		switch x := v.(type) {
		case string:
			return strings.Contains(x, key)
		case map[string]any:
			for name, child := range x {
				if strings.Contains(name, key) || contains(child) {
					return true
				}
			}
		case []any:
			for _, child := range x {
				if contains(child) {
					return true
				}
			}
		}
		return false
	}
	return contains(value)
}
func usagePolicyValid(p UsageHostPolicy) bool {
	_, offset := p.ExpiresAt.Zone()
	return id(p.SourceID, "inst_") && label(p.OrganizationID) && label(p.ModelConnectionID) && p.ModelConnectionVersion > 0 && lowerHex(p.ConnectionIdentitySHA256, 64) && usageText(p.ArchivedProviderAccount, 128) && label(p.CredentialReference) && lowerHex(p.CredentialSHA256, 64) && !p.ExpiresAt.IsZero() && p.ExpiresAt.Year() >= 2000 && p.ExpiresAt.Year() < 10000 && offset == 0
}
func validateUsageState(s hostState) error {
	i := s.Info
	stamp, err := time.Parse(time.RFC3339Nano, i.ConfirmedAt)
	if i.Version != 1 || i.Purpose != usageHostPurpose || !validBinding(i.Binding) || i.Epoch == 0 || !lowerHex(i.OperationID, 32) || !label(i.OperatorReference) || err != nil || stamp.UTC().Format(time.RFC3339Nano) != i.ConfirmedAt || i.PublicKeySHA256 != "" || len(s.Seed) != 0 || len(i.Scopes) != 0 || i.Usage == nil || !usagePolicyValid(*i.Usage) {
		return ErrHostState
	}
	if i.Usage.Enabled {
		if !usageCredentialValid(s.UsageCredential) || stateHash([]byte(s.UsageCredential)) != i.Usage.CredentialSHA256 {
			return ErrHostState
		}
		public, _ := json.Marshal(i)
		if usageJSONContainsCredential(public, s.UsageCredential) {
			return ErrHostState
		}
	} else if s.UsageCredential != "" {
		return ErrHostState
	}
	return nil
}
func freshUsageState(binding Binding, epoch uint64, p UsageHostPolicy, credential, reference string) (hostState, error) {
	if p.Enabled && (!p.ExpiresAt.After(time.Now()) || !usageCredentialValid(credential)) {
		return hostState{}, ErrUsageDenied
	}
	// Do not trust a caller's fingerprint of a different key.
	if p.Enabled {
		p.CredentialSHA256 = stateHash([]byte(credential))
	} else if credential != "" {
		return hostState{}, ErrUsageDenied
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return hostState{}, ErrHostState
	}
	s := hostState{Info: HostInfo{Version: 1, Binding: binding, Epoch: epoch, OperationID: hex.EncodeToString(nonce[:]), OperatorReference: reference, ConfirmedAt: time.Now().UTC().Format(time.RFC3339Nano), Purpose: usageHostPurpose, Usage: &p}, UsageCredential: credential}
	if err := s.validate(); err != nil {
		return hostState{}, err
	}
	return s, nil
}

func InitUsageHost(ctx context.Context, path string, binding Binding, p UsageHostPolicy, credential, reference string) (HostInfo, error) {
	if ctx == nil {
		return HostInfo{}, ErrUsageDenied
	}
	s, err := freshUsageState(binding, 1, p, credential, reference)
	if err != nil {
		return HostInfo{}, err
	}
	d, err := createHostDirectory(ctx, path)
	if err != nil {
		return HostInfo{}, err
	}
	defer d.close()
	if err = d.writeState(s); err != nil {
		return HostInfo{}, err
	}
	if err = d.syncParent(); err != nil {
		return HostInfo{}, err
	}
	return s.Info, nil
}

// Replacement drains acquisitions under the shared lock and uses the existing
// durable pending fence. On ANY error, inspect; do not claim revocation succeeded.
func ReplaceUsageHost(ctx context.Context, path string, epoch uint64, p UsageHostPolicy, credential, reference string) (HostInfo, error) {
	if ctx == nil {
		return HostInfo{}, ErrUsageDenied
	}
	d, err := openHostDirectory(ctx, path, true)
	if err != nil {
		return HostInfo{}, err
	}
	defer d.close()
	s, pending, err := d.load()
	if err != nil {
		return HostInfo{}, err
	}
	if pending != nil {
		return HostInfo{}, ErrPending
	}
	if s.Info.Purpose != usageHostPurpose || s.Info.Epoch != epoch || epoch == ^uint64(0) {
		return HostInfo{}, ErrHostState
	}
	next, err := freshUsageState(s.Info.Binding, epoch+1, p, credential, reference)
	if err != nil {
		return HostInfo{}, err
	}
	public, _ := json.Marshal(next.Info)
	if usageJSONContainsCredential(public, s.UsageCredential) {
		return HostInfo{}, ErrHostState
	}
	i := hostIntent{PreviousSHA256: stateHash(stateBytes(s)), Next: next}
	if err = d.writeIntent(i); err != nil {
		return HostInfo{}, ErrPending
	}
	if err = d.finish(i); err != nil {
		return HostInfo{}, ErrPending
	}
	return next.Info, nil
}

func usageRequestMatches(s hostState, r UsageRequest, now time.Time) bool {
	if s.Info.Purpose != usageHostPurpose || s.validate() != nil {
		return false
	}
	p := s.Info.Usage
	return p.Enabled && now.Before(p.ExpiresAt) && r.Binding == s.Info.Binding && lowerHex(r.SourcePlanSHA256, 64) && r.SourceID == p.SourceID && r.OrganizationID == p.OrganizationID && r.ModelConnectionID == p.ModelConnectionID && r.ModelConnectionVersion == p.ModelConnectionVersion && r.ConnectionIdentitySHA256 == p.ConnectionIdentitySHA256 && r.ArchivedProviderAccount == p.ArchivedProviderAccount && len(r.GenerationID) <= 128 && usageGeneration.MatchString(r.GenerationID) && usageText(r.Model, 256)
}

// No callback, key accessor or endpoint override is exposed. The private seam
// below exists for owned synthetic tests; production always uses this reader.
func ObserveHostUsage(ctx context.Context, path string, request UsageRequest) (AcquiredUsageObservation, error) {
	return observeHostUsage(ctx, path, request, func(ctx context.Context, key, id string) (usageObservation, error) {
		r, err := modelgateway.NewOpenRouterUsageReader(key, 30*time.Second)
		if err != nil {
			return nil, err
		}
		return r.Observe(ctx, id)
	})
}

type usageObservation interface {
	Record() modelgateway.UsageObservationRecord
	RawResponse() []byte
}
type usageRead func(context.Context, string, string) (usageObservation, error)

func observeHostUsage(ctx context.Context, path string, request UsageRequest, read usageRead) (AcquiredUsageObservation, error) {
	if ctx == nil || read == nil {
		return AcquiredUsageObservation{}, ErrUsageDenied
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	d, err := openHostDirectory(ctx, path, false)
	if err != nil {
		return AcquiredUsageObservation{}, err
	}
	defer d.close()
	s, pending, err := d.load()
	if err != nil {
		return AcquiredUsageObservation{}, err
	}
	if pending != nil {
		return AcquiredUsageObservation{}, ErrPending
	}
	started := time.Now().UTC()
	if ctx.Err() != nil || !usageRequestMatches(s, request, started) {
		return AcquiredUsageObservation{}, ErrUsageDenied
	}
	o, err := read(ctx, s.UsageCredential, request.GenerationID)
	if err != nil {
		return AcquiredUsageObservation{}, err
	}
	now := time.Now().UTC()
	if o == nil || ctx.Err() != nil || !usageRequestMatches(s, request, now) {
		return AcquiredUsageObservation{}, ErrUsageDenied
	}
	raw, record := o.RawResponse(), o.Record()
	// Model aliases are not inferred. A mismatch stays unresolved.
	if len(raw) == 0 || len(raw) > 256<<10 || record.Profile != "openrouter-generation-observation-v1" || record.SettlementState != "unverified" || record.GenerationID != request.GenerationID || record.Model != request.Model || record.ResponseSHA256 != stateHash(raw) || record.ObservedAt.Before(started) || record.ObservedAt.After(now) || bytes.Contains(raw, []byte(s.UsageCredential)) {
		return AcquiredUsageObservation{}, ErrUsageDenied
	}
	// Mapping metadata is public to this local operator. Reject accidental raw
	// credential inclusion in those operator-supplied labels before returning it.
	policyRaw, _ := json.Marshal(s.Info)
	requestRaw, _ := json.Marshal(request)
	if usageJSONContainsCredential(policyRaw, s.UsageCredential) || usageJSONContainsCredential(requestRaw, s.UsageCredential) {
		return AcquiredUsageObservation{}, ErrUsageDenied
	}
	receipt := UsageAcquisitionReceipt{Version: 1, Request: request, Policy: *s.Info.Usage, PolicyEpoch: s.Info.Epoch, PolicySHA256: stateHash(policyRaw), PolicyOperationID: s.Info.OperationID, PolicyConfirmedAt: s.Info.ConfirmedAt, OperatorReference: s.Info.OperatorReference, AccountMappingMethod: "current_host_operator_attestation", Observation: record}
	encoded, _ := json.Marshal(receipt)
	receipt.ReceiptSHA256 = stateHash(encoded)
	if ctx.Err() != nil || !usageRequestMatches(s, request, time.Now()) {
		return AcquiredUsageObservation{}, ErrUsageDenied
	}
	return AcquiredUsageObservation{receipt: receipt, raw: bytes.Clone(raw)}, nil
}
