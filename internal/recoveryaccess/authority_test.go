// SPDX-License-Identifier: Elastic-2.0
package recoveryaccess

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (Policy, Claims, ed25519.PrivateKey, string) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b := Binding{TargetID: "inst_" + strings.Repeat("1", 32), RestoreID: "restore_" + strings.Repeat("2", 32), SnapshotSHA256: strings.Repeat("3", 64)}
	c := Claims{Version: 1, Binding: b, Issuer: "current-host", Subject: "operator-alice", OrganizationID: "org-a", Epoch: 1, IssuedAt: time.Now().Unix() - 1, ExpiresAt: time.Now().Unix() + 60}
	p := Policy{Binding: b, Epoch: 1, Issuers: map[string]ed25519.PublicKey{c.Issuer: pub}, Scopes: []Scope{{c.Subject, c.OrganizationID}}}
	token, err := Sign(c, key)
	if err != nil {
		t.Fatal(err)
	}
	return p, c, key, token
}

func TestAuthorityCurrentScopeAndTamper(t *testing.T) {
	p, c, key, token := fixture(t)
	a, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	read := func(_ context.Context, got Claims) error {
		calls++
		if got != c {
			t.Fatal("claim mismatch")
		}
		return nil
	}
	if err = a.Read(context.Background(), token, c.OrganizationID, read); err != nil || calls != 1 {
		t.Fatal("valid read denied", err)
	}
	for name, mutate := range map[string]func(*Claims){
		"target":       func(c *Claims) { c.Binding.TargetID = "inst_" + strings.Repeat("4", 32) },
		"restore":      func(c *Claims) { c.Binding.RestoreID = "restore_" + strings.Repeat("4", 32) },
		"snapshot":     func(c *Claims) { c.Binding.SnapshotSHA256 = strings.Repeat("4", 64) },
		"organization": func(c *Claims) { c.OrganizationID = "org-b" },
		"subject":      func(c *Claims) { c.Subject = "other-operator" },
		"issuer":       func(c *Claims) { c.Issuer = "archive-signer" },
		"epoch":        func(c *Claims) { c.Epoch++ },
		"expired":      func(c *Claims) { c.IssuedAt -= 120; c.ExpiresAt -= 120 },
		"future":       func(c *Claims) { c.IssuedAt += 120; c.ExpiresAt += 120 },
	} {
		t.Run(name, func(t *testing.T) {
			altered := c
			mutate(&altered)
			signed, e := Sign(altered, key)
			if e != nil {
				t.Fatal(e)
			}
			if a.Read(context.Background(), signed, c.OrganizationID, read) != ErrDenied {
				t.Fatal("wrong capability accepted")
			}
		})
	}
	parts := strings.Split(token, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	for name, message := range map[string][]byte{
		"wrong_purpose":   append([]byte("archive-signature\x00"), raw...),
		"duplicate_field": append([]byte(domain), []byte(strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1))...),
		"unknown_field":   append([]byte(domain), []byte(strings.Replace(string(raw), `"version":1`, `"version":1,"admin":true`, 1))...),
	} {
		t.Run(name, func(t *testing.T) {
			payload := raw
			if name != "wrong_purpose" {
				payload = message[len(domain):]
			}
			forged := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, message))
			if a.Read(context.Background(), forged, c.OrganizationID, read) != ErrDenied {
				t.Fatal("noncanonical or wrong-purpose capability accepted")
			}
		})
	}
	for _, bad := range []string{"", token + "x", strings.Repeat("x", 4097), token + ".extra", token[:4] + "\r\n" + token[4:], token + "\n"} {
		if a.Read(context.Background(), bad, c.OrganizationID, read) != ErrDenied {
			t.Fatal("malformed capability accepted")
		}
	}
	if a.Read(context.Background(), token, "org-b", read) != ErrDenied || calls != 1 {
		t.Fatal("denied request entered callback")
	}
	// New clones key buffers and scope storage; caller mutation cannot change it.
	p.Issuers[c.Issuer][0] ^= 1
	p.Scopes[0].Subject = "mutated"
	if a.Read(context.Background(), token, c.OrganizationID, read) != nil {
		t.Fatal("policy aliased caller memory")
	}
}

func TestAuthorityRevocationBarrier(t *testing.T) {
	p, c, key, token := fixture(t)
	a, _ := New(p)
	entered, release := make(chan struct{}), make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		readDone <- a.Read(context.Background(), token, c.OrganizationID, func(context.Context, Claims) error { close(entered); <-release; return nil })
	}()
	<-entered
	p.Epoch++
	p.Scopes = nil
	replaceDone := make(chan error, 1)
	started := make(chan struct{})
	go func() { close(started); replaceDone <- a.Replace(p) }()
	<-started
	// Observe the writer pending behind the admitted read, rather than merely
	// assuming the replacement goroutine has already reached its lock.
	deadline := time.After(time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for a.mu.TryRLock() {
		a.mu.RUnlock()
		select {
		case <-deadline:
			t.Fatal("replacement did not reach barrier")
		case <-tick.C:
		}
	}
	select {
	case <-replaceDone:
		t.Fatal("replacement completed during admitted read")
	default:
	}
	close(release)
	if <-readDone != nil || <-replaceDone != nil {
		t.Fatal("barrier failed")
	}
	if a.Read(context.Background(), token, c.OrganizationID, func(context.Context, Claims) error { t.Fatal("revoked callback entered"); return nil }) != ErrDenied {
		t.Fatal("revoked access accepted")
	}
	if a.Replace(p) != ErrDenied {
		t.Fatal("epoch rollback/reuse accepted")
	}
	// Current epoch alone is insufficient without the authorized pair or issuer.
	c.Epoch = p.Epoch
	token, _ = Sign(c, key)
	if _, err := verify(token, c.OrganizationID, p, time.Now()); err != ErrDenied {
		t.Fatal("replacement policy bypass")
	}
	p.Scopes = []Scope{{c.Subject, c.OrganizationID}}
	p.Issuers = nil
	if _, err := verify(token, c.OrganizationID, p, time.Now()); err != ErrDenied {
		t.Fatal("removed issuer accepted")
	}
}

func TestAuthorityRechecksExpiryBeforeDelivery(t *testing.T) {
	p, c, key, _ := fixture(t)
	a, _ := New(p)
	c.ExpiresAt = time.Now().Unix() + 1
	token, _ := Sign(c, key)
	err := a.Read(context.Background(), token, c.OrganizationID, func(ctx context.Context, _ Claims) error {
		timer := time.NewTimer(time.Until(time.Unix(c.ExpiresAt, 0)) + 10*time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != ErrDenied {
		t.Fatal("expired read delivered")
	}
	c.ExpiresAt = c.IssuedAt + 901
	if _, err = Sign(c, key); err != ErrDenied {
		t.Fatal("unbounded lifetime accepted")
	}
}
