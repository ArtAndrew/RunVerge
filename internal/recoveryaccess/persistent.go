// SPDX-License-Identifier: Elastic-2.0
package recoveryaccess

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrHostState = errors.New("private recovery authority unavailable; inspect the exact directory")
var ErrPending = errors.New("recovery authority change pending; inspect and explicitly resolve the exact operation")

// HostInfo contains no signing secret or bearer token. Reference is a trusted
// local operator's current-scope attestation, not proof from an identity service.
type HostInfo struct {
	Version           int     `json:"version"`
	Binding           Binding `json:"binding"`
	Epoch             uint64  `json:"epoch"`
	OperationID       string  `json:"operation_id"`
	Scopes            []Scope `json:"scopes"`
	OperatorReference string  `json:"operator_reference"`
	ConfirmedAt       string  `json:"confirmed_at"`
	PublicKeySHA256   string  `json:"public_key_sha256"`
}

type HostInspection struct {
	State   string    `json:"state"`
	Current HostInfo  `json:"current"`
	Pending *HostInfo `json:"pending,omitempty"`
}

type hostState struct {
	Info HostInfo `json:"info"`
	Seed []byte   `json:"seed"`
}
type hostIntent struct {
	PreviousSHA256 string    `json:"previous_sha256"`
	Next           hostState `json:"next"`
}

func freshState(binding Binding, epoch uint64, scopes []Scope, reference string) (hostState, error) {
	if !label(reference) {
		return hostState{}, ErrHostState
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return hostState{}, ErrHostState
	}
	var operation [16]byte
	if _, err = rand.Read(operation[:]); err != nil {
		return hostState{}, ErrHostState
	}
	s := hostState{Info: HostInfo{Version: 1, Binding: binding, Epoch: epoch, OperationID: hex.EncodeToString(operation[:]), Scopes: append([]Scope{}, scopes...), OperatorReference: reference, ConfirmedAt: time.Now().UTC().Format(time.RFC3339Nano), PublicKeySHA256: stateHash(pub)}, Seed: append([]byte(nil), key.Seed()...)}
	if _, err = s.policy(); err != nil {
		return hostState{}, err
	}
	return s, nil
}

func (s hostState) policy() (Policy, error) {
	if s.Info.Version != 1 || len(s.Seed) != ed25519.SeedSize || !label(s.Info.OperatorReference) || !lowerHex(s.Info.OperationID, 32) {
		return Policy{}, ErrHostState
	}
	stamp, err := time.Parse(time.RFC3339Nano, s.Info.ConfirmedAt)
	if err != nil || stamp.UTC().Format(time.RFC3339Nano) != s.Info.ConfirmedAt {
		return Policy{}, ErrHostState
	}
	pub := ed25519.NewKeyFromSeed(s.Seed).Public().(ed25519.PublicKey)
	if s.Info.PublicKeySHA256 != stateHash(pub) {
		return Policy{}, ErrHostState
	}
	p, err := clonePolicy(Policy{Binding: s.Info.Binding, Epoch: s.Info.Epoch, Issuers: map[string]ed25519.PublicKey{"host-v1": pub}, Scopes: s.Info.Scopes})
	if err != nil {
		return Policy{}, ErrHostState
	}
	return p, nil
}

func stateHash(raw []byte) string   { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func stateBytes(s hostState) []byte { raw, _ := json.Marshal(s); return raw }

func decodeState(raw []byte) (hostState, error) {
	var s hostState
	if json.Unmarshal(raw, &s) != nil || !bytes.Equal(stateBytes(s), raw) {
		return hostState{}, ErrHostState
	}
	if _, err := s.policy(); err != nil {
		return hostState{}, err
	}
	return s, nil
}

func decodeIntent(raw []byte) (hostIntent, error) {
	var i hostIntent
	if json.Unmarshal(raw, &i) != nil {
		return i, ErrHostState
	}
	canonical, _ := json.Marshal(i)
	if !bytes.Equal(canonical, raw) || !lowerHex(i.PreviousSHA256, 64) {
		return hostIntent{}, ErrHostState
	}
	if _, err := i.Next.policy(); err != nil {
		return hostIntent{}, err
	}
	return i, nil
}

// InitHost creates a new private directory under a trusted private parent. It
// never adopts or overwrites an existing directory, even after an uncertain init.
func InitHost(ctx context.Context, path string, binding Binding, scopes []Scope, reference string) (HostInfo, error) {
	s, err := freshState(binding, 1, scopes, reference)
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
		return HostInfo{}, ErrHostState
	}
	return s.Info, nil
}

func InspectHost(ctx context.Context, path string) (HostInspection, error) {
	d, err := openHostDirectory(ctx, path, false)
	if err != nil {
		return HostInspection{}, err
	}
	defer d.close()
	s, intent, err := d.load()
	if err != nil {
		return HostInspection{}, err
	}
	result := HostInspection{State: "active", Current: s.Info}
	if intent != nil {
		result.State = "pending"
		next := intent.Next.Info
		result.Pending = &next
	}
	return result, nil
}

// ReplaceHost first persists a fenced intent, then replaces the active state.
// A pending intent excludes ALL reads and issuance across cooperating processes.
// No returned error proves revocation: inspect before relying on any outcome.
func ReplaceHost(ctx context.Context, path string, expectedEpoch uint64, scopes []Scope, reference string) (HostInfo, error) {
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
	if s.Info.Epoch != expectedEpoch || expectedEpoch == ^uint64(0) {
		return HostInfo{}, ErrHostState
	}
	next, err := freshState(s.Info.Binding, s.Info.Epoch+1, scopes, reference)
	if err != nil {
		return HostInfo{}, err
	}
	intent := hostIntent{PreviousSHA256: stateHash(stateBytes(s)), Next: next}
	if err = d.writeIntent(intent); err != nil {
		return HostInfo{}, ErrPending
	}
	if err = d.finish(intent); err != nil {
		return HostInfo{}, ErrPending
	}
	return next.Info, nil
}

// ResolveHost completes only the exact reviewed pending operation. It never
// generates new authority, deletes an unexpected state or infers success from a
// failed command. Inspect reports the intended next scopes and operation ID.
func ResolveHost(ctx context.Context, path, operationID string) (HostInfo, error) {
	d, err := openHostDirectory(ctx, path, true)
	if err != nil {
		return HostInfo{}, err
	}
	defer d.close()
	_, pending, err := d.load()
	if err != nil {
		return HostInfo{}, err
	}
	if pending == nil || pending.Next.Info.OperationID != operationID {
		return HostInfo{}, ErrHostState
	}
	if err = d.finish(*pending); err != nil {
		return HostInfo{}, ErrPending
	}
	return pending.Next.Info, nil
}

func IssueHost(ctx context.Context, path, subject, org string, lifetime time.Duration) (string, error) {
	if lifetime < time.Second || lifetime > MaxLifetime || lifetime%time.Second != 0 {
		return "", ErrDenied
	}
	d, err := openHostDirectory(ctx, path, false)
	if err != nil {
		return "", err
	}
	defer d.close()
	s, pending, err := d.load()
	if err != nil {
		return "", err
	}
	if pending != nil {
		return "", ErrPending
	}
	now := time.Now()
	c := Claims{Version: 1, Binding: s.Info.Binding, Issuer: "host-v1", Subject: subject, OrganizationID: org, Epoch: s.Info.Epoch, IssuedAt: now.Unix(), ExpiresAt: now.Add(lifetime).Unix()}
	token, err := Sign(c, ed25519.NewKeyFromSeed(s.Seed))
	if err != nil {
		return "", err
	}
	p, err := s.policy()
	if err != nil {
		return "", err
	}
	if _, err = verify(token, org, p, time.Now()); err != nil {
		return "", err
	}
	return token, nil
}

// WithHostAuthority retains a shared OS lock through the entire trusted bounded
// callback. Construct the storage reader inside this callback; do not retain the
// authority or stream data before verification finishes. Each operation reloads
// current durable policy. Missing state never causes fallback to a backup policy.
func WithHostAuthority(ctx context.Context, path string, read func(context.Context, *Authority) error) error {
	if read == nil {
		return ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
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
	p, err := s.policy()
	if err != nil {
		return err
	}
	a, err := New(p)
	if err != nil {
		return err
	}
	defer a.close() // runs before d.close, including on callback error/panic
	if err = read(ctx, a); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ErrDenied
	}
	return nil
}

// SaveHostTokenOutside prevents token output from creating reserved authority
// state, including through ancestor symlink aliases or a descendant directory.
// Namespace continuity still requires the documented trusted parent/ancestors.
func SaveHostTokenOutside(authorityPath, output, token string) error {
	protected, err := filepath.EvalSymlinks(authorityPath)
	if err != nil {
		return ErrHostState
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return ErrHostState
	}
	rel, err := filepath.Rel(protected, parent)
	if err != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return ErrHostState
	}
	info, err := os.Stat(protected)
	if err != nil {
		return ErrHostState
	}
	for current := parent; ; current = filepath.Dir(current) {
		other, err := os.Stat(current)
		if err != nil || os.SameFile(info, other) {
			return ErrHostState
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return SaveHostToken(output, token)
}
