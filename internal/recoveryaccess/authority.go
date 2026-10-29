// SPDX-License-Identifier: Elastic-2.0
// Package recoveryaccess verifies narrow recovery-read bearer capabilities.
// It does not authenticate a human, reconstruct current ownership from a backup,
// or authorize execution. A trusted host must provision CURRENT policy separately
// from restored data and preserve its epoch across process restarts. This package
// is deliberately not wired into the ordinary HTTP/login/bootstrap paths.
package recoveryaccess

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

var ErrDenied = errors.New("recovery read access denied")

const domain = "runverge-recovery-read-v1\x00"
const MaxLifetime = 15 * time.Minute

type Binding struct {
	TargetID       string `json:"target_id"`
	RestoreID      string `json:"restore_id"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
}

// Claims name the capability's recipient, not an authenticated presenting user.
// Possession permits repeated reads until expiry or current-policy revocation.
type Claims struct {
	Version        int     `json:"version"`
	Binding        Binding `json:"binding"`
	Issuer         string  `json:"issuer"`
	Subject        string  `json:"subject"`
	OrganizationID string  `json:"organization_id"`
	Epoch          uint64  `json:"epoch"`
	IssuedAt       int64   `json:"issued_at"`
	ExpiresAt      int64   `json:"expires_at"`
}

type Scope struct{ Subject, OrganizationID string }

// Policy is host configuration, NEVER request or archive data. Keys authorize
// only this target/restore/snapshot. Scopes must reflect current operator proof,
// not restored memberships. Missing policy must fail closed at the host boundary.
type Policy struct {
	Binding Binding
	Epoch   uint64
	Issuers map[string]ed25519.PublicKey
	Scopes  []Scope
}

type Authority struct {
	mu     sync.RWMutex
	policy Policy
}

func New(policy Policy) (*Authority, error) {
	p, err := clonePolicy(policy)
	if err != nil {
		return nil, err
	}
	return &Authority{policy: p}, nil
}

// Replace is a revocation barrier: it waits for admitted bounded reads to end.
// Once it returns, no read admitted by the earlier policy remains in flight.
// It does not retract data already returned to the holder. Issuer removal or an
// empty scope list revoke access; epochs cannot roll back in this instance.
func (a *Authority) Replace(policy Policy) error {
	if a == nil {
		return ErrDenied
	}
	p, err := clonePolicy(policy)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if p.Binding != a.policy.Binding || p.Epoch <= a.policy.Epoch {
		return ErrDenied
	}
	a.policy = p
	return nil
}

// Read holds current authorization across a bounded read and checks expiry again
// before successful delivery. The callback must buffer output privately, honor
// ctx and discard that output on ANY returned error; it must not stream or write.
// Only the configured storage reader calls this in the production component.
func (a *Authority) Read(ctx context.Context, token, organization string, read func(context.Context, Claims) error) error {
	if a == nil || read == nil {
		return ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	a.mu.RLock()
	defer a.mu.RUnlock()
	if ctx.Err() != nil {
		return ErrDenied
	}
	c, err := verify(token, organization, a.policy, time.Now())
	if err != nil {
		return ErrDenied
	}
	if err = read(ctx, c); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ErrDenied
	}
	_, err = verify(token, organization, a.policy, time.Now())
	return err
}

// Sign is an operator-side helper; a signing key alone cannot override the
// reader's independently configured current issuer/scopes/epoch/binding.
func Sign(c Claims, key ed25519.PrivateKey) (string, error) {
	if len(key) != ed25519.PrivateKeySize || !validClaims(c) {
		return "", ErrDenied
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", ErrDenied
	}
	signature := ed25519.Sign(key, append([]byte(domain), raw...))
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func verify(token, org string, policy Policy, now time.Time) (Claims, error) {
	var c Claims
	if len(token) > 4096 || !label(org) {
		return c, ErrDenied
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return c, ErrDenied
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return c, ErrDenied
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(sig) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(raw) != parts[0] || base64.RawURLEncoding.EncodeToString(sig) != parts[1] || json.Unmarshal(raw, &c) != nil {
		return Claims{}, ErrDenied
	}
	canonical, err := json.Marshal(c)
	// Byte equality also rejects duplicate/unknown fields, alternate JSON forms,
	// trailing bytes and reordered fields. There is one signed wire form in v1.
	if err != nil || !bytes.Equal(raw, canonical) || !validClaims(c) || c.Binding != policy.Binding || c.Epoch != policy.Epoch || c.OrganizationID != org || now.Unix() < c.IssuedAt || now.Unix() >= c.ExpiresAt {
		return Claims{}, ErrDenied
	}
	key := policy.Issuers[c.Issuer]
	if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, append([]byte(domain), raw...), sig) {
		return Claims{}, ErrDenied
	}
	for _, scope := range policy.Scopes {
		if scope.Subject == c.Subject && scope.OrganizationID == org {
			return c, nil
		}
	}
	return Claims{}, ErrDenied
}

func clonePolicy(p Policy) (Policy, error) {
	if !validBinding(p.Binding) || p.Epoch == 0 || len(p.Issuers) > 32 || len(p.Scopes) > 1024 {
		return Policy{}, ErrDenied
	}
	copy := Policy{Binding: p.Binding, Epoch: p.Epoch, Issuers: make(map[string]ed25519.PublicKey), Scopes: append([]Scope(nil), p.Scopes...)}
	for name, key := range p.Issuers {
		if !label(name) || len(key) != ed25519.PublicKeySize {
			return Policy{}, ErrDenied
		}
		copy.Issuers[name] = append(ed25519.PublicKey(nil), key...)
	}
	seen := map[Scope]bool{}
	for _, s := range copy.Scopes {
		if !label(s.Subject) || !label(s.OrganizationID) || seen[s] {
			return Policy{}, ErrDenied
		}
		seen[s] = true
	}
	return copy, nil
}

func validClaims(c Claims) bool {
	// Bound both timestamps before subtraction to avoid integer overflow.
	return c.Version == 1 && validBinding(c.Binding) && label(c.Issuer) && label(c.Subject) && label(c.OrganizationID) && c.Epoch > 0 && c.IssuedAt > 0 && c.IssuedAt < 253402300800 && c.ExpiresAt > c.IssuedAt && c.ExpiresAt < 253402300800 && c.ExpiresAt-c.IssuedAt <= int64(MaxLifetime/time.Second)
}

func validBinding(b Binding) bool {
	return id(b.TargetID, "inst_") && id(b.RestoreID, "restore_") && lowerHex(b.SnapshotSHA256, 64)
}
func id(s, prefix string) bool {
	return strings.HasPrefix(s, prefix) && lowerHex(strings.TrimPrefix(s, prefix), 32)
}
func lowerHex(s string, n int) bool {
	_, err := hex.DecodeString(s)
	return len(s) == n && strings.ToLower(s) == s && err == nil
}
func label(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._:@/-", c)) {
			return false
		}
	}
	return true
}
