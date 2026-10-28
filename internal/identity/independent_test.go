// SPDX-License-Identifier: Elastic-2.0
package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/identity"
	"github.com/go-jose/go-jose/v4"
)

func TestIndependentClaimNamesCannotShadowCanonicalClaims(t *testing.T) {
	f := newFixture(t, false)
	v := verifierFor(t, f.config())
	for _, tc := range []struct {
		name, claim, alias string
		canonical, shadow  any
	}{
		{"authorized_party", "azp", "AZP", "", fixtureClientID},
		{"audience", "aud", "AUD", "different-client", fixtureClientID},
		{"nonce", "nonce", "NONCE", "different-transaction", fixtureNonce},
		{"expiry", "exp", "EXP", time.Now().Unix() - 30, time.Now().Unix() + 300},
		{"issuer", "iss", "ISS", "https://different-issuer.example", f.server.URL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := f.claims()
			claims[tc.claim] = tc.canonical
			encoded, err := json.Marshal(claims)
			if err != nil {
				t.Fatal(err)
			}
			alias, _ := json.Marshal(tc.alias)
			shadow, _ := json.Marshal(tc.shadow)
			payload := []byte(strings.TrimSuffix(string(encoded), "}") + "," + string(alias) + ":" + string(shadow) + "}")
			f.mu.RLock()
			key := f.private["first"]
			f.mu.RUnlock()
			signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "first"))
			if err != nil {
				t.Fatal(err)
			}
			signed, err := signer.Sign(payload)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := signed.CompactSerialize()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := v.Verify(context.Background(), raw, fixtureNonce); !errors.Is(err, identity.ErrInvalidToken) {
				t.Fatalf("noncanonical %s shadowed invalid canonical %s: %v", tc.alias, tc.claim, err)
			}
		})
	}
}

// These cases exercise the documented application profile with independently
// selected malformed claims; a valid signature must not bypass claim typing.
func TestIndependentPresentClaimsCannotMasqueradeAsAbsent(t *testing.T) {
	f := newFixture(t, false)
	v := verifierFor(t, f.config())
	for _, tc := range []struct {
		name  string
		claim string
		value any
	}{
		{"empty_authorized_party", "azp", ""},
		{"null_authorized_party", "azp", nil},
		{"null_email_verification", "email_verified", nil},
		{"null_groups", "groups", nil},
		{"null_not_before", "nbf", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := f.claims()
			claims[tc.claim] = tc.value
			if _, err := v.Verify(context.Background(), f.sign(t, "first", claims), fixtureNonce); !errors.Is(err, identity.ErrInvalidToken) {
				t.Fatalf("present invalid %s accepted; want ErrInvalidToken, got %v", tc.claim, err)
			}
		})
	}
	claims := f.claims()
	delete(claims, "email_verified")
	delete(claims, "groups")
	if _, err := v.Verify(context.Background(), f.sign(t, "first", claims), fixtureNonce); err != nil {
		t.Fatalf("absent optional claims should remain supported: %v", err)
	}
}

// All cases use the same valid RSA public key. The verifier must honor the
// provider's explicit restriction on that key, not just its mathematical type.
func TestIndependentJWKSKeyRestrictions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		use         string
		alg         string
		ops         []string
		allowedAlgs []string
		accept      bool
	}{
		{name: "encryption_only", use: "enc", alg: "RS256"},
		{name: "different_algorithm", use: "sig", alg: "RS512"},
		{name: "different_algorithm_both_allowed", use: "sig", alg: "RS512", allowedAlgs: []string{"RS256", "RS512"}},
		{name: "key_ops_excludes_verify", alg: "RS256", ops: []string{"encrypt"}},
		{name: "explicit_verification_key", use: "sig", alg: "RS256", ops: []string{"verify"}, accept: true},
		{name: "optional_metadata_absent", accept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, false)
			f.mu.Lock()
			pub := f.private["first"].Public()
			f.mu.Unlock()
			encoded, err := json.Marshal(jose.JSONWebKey{Key: pub, KeyID: "first", Use: tc.use, Algorithm: tc.alg})
			if err != nil {
				t.Fatal(err)
			}
			var key map[string]any
			if err := json.Unmarshal(encoded, &key); err != nil {
				t.Fatal(err)
			}
			if tc.ops != nil {
				key["key_ops"] = tc.ops
			}
			keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{key}})
			}))
			defer keys.Close()
			f.mu.Lock()
			f.jwksOverride = keys.URL + "/keys"
			f.mu.Unlock()
			cfg := f.config()
			cfg.AdditionalJWKSOrigins = []string{keys.URL}
			cfg.AllowedSigningAlgorithms = tc.allowedAlgs
			v := verifierFor(t, cfg)
			_, err = v.Verify(context.Background(), f.sign(t, "first", f.claims()), fixtureNonce)
			if tc.accept && err != nil {
				t.Fatalf("valid verification key rejected: %v", err)
			}
			if !tc.accept && err == nil {
				t.Fatal("token accepted using a JWK explicitly restricted against this verification")
			}
		})
	}
}

func TestIndependentExpiredCacheCannotFallBackAfterRefreshFailure(t *testing.T) {
	f := newFixture(t, false)
	var unavailable atomic.Bool
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		f.serve(w, r)
	}))
	defer keys.Close()
	f.mu.Lock()
	f.jwksOverride = keys.URL + "/keys"
	f.mu.Unlock()
	cfg := f.config()
	cfg.AdditionalJWKSOrigins = []string{keys.URL}
	cfg.KeyCacheTTL = 100 * time.Millisecond
	v := verifierFor(t, cfg)
	raw := f.sign(t, "first", f.claims())
	if _, err := v.Verify(context.Background(), raw, fixtureNonce); err != nil {
		t.Fatal(err)
	}
	unavailable.Store(true)
	time.Sleep(150 * time.Millisecond)
	if _, err := v.Verify(context.Background(), raw, fixtureNonce); err == nil {
		t.Fatal("expired cached key accepted after failed JWKS refresh")
	}
}

func TestIndependentJWKSDeadlineIncludesBodyRead(t *testing.T) {
	f := newFixture(t, false)
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"keys":[`))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer keys.Close()
	f.mu.Lock()
	f.jwksOverride = keys.URL + "/keys"
	f.mu.Unlock()
	cfg := f.config()
	cfg.AdditionalJWKSOrigins = []string{keys.URL}
	cfg.RequestTimeout = 100 * time.Millisecond
	v := verifierFor(t, cfg)
	raw := f.sign(t, "first", f.claims())
	started := time.Now()
	if _, err := v.Verify(context.Background(), raw, fixtureNonce); err == nil {
		t.Fatal("incomplete body accepted")
	}
	if elapsed := time.Since(started); elapsed > 750*time.Millisecond {
		t.Fatalf("response headers bypassed body-read deadline: %v", elapsed)
	}
}
