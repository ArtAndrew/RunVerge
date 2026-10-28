// SPDX-License-Identifier: Elastic-2.0
package identity_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/identity"
)

func verifierFor(t *testing.T, cfg identity.Config) *identity.Verifier {
	t.Helper()
	v, err := identity.NewVerifier(context.Background(), cfg)
	if err != nil {
		t.Fatalf("fixture discovery failed: %v", err)
	}
	t.Cleanup(v.Close)
	return v
}

func TestValidTLSClaimsAndStableSubject(t *testing.T) {
	f := newFixture(t, true)
	v := verifierFor(t, f.config())
	claims, err := v.Verify(context.Background(), f.sign(t, "first", f.claims()), fixtureNonce)
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if claims.Issuer != f.server.URL || claims.Subject != "employee-immutable-123" || claims.Email != "employee@example.test" || !claims.EmailVerified || !reflect.DeepEqual(claims.Groups, []string{"engineering", "owner"}) {
		t.Fatal("validated profile did not preserve expected claims")
	}
	changed := f.claims()
	changed["email"] = "new-address@example.test"
	changed["email_verified"] = false
	updated, err := v.Verify(context.Background(), f.sign(t, "first", changed), fixtureNonce)
	if err != nil || updated.Subject != claims.Subject || updated.Issuer != claims.Issuer || updated.EmailVerified {
		t.Fatal("email change altered identity or verification state")
	}
	if f.credentialReceived.Load() {
		t.Fatal("credentials sent to discovery/JWKS")
	}
}

func TestRejectUntrustedClaimsAndSignatures(t *testing.T) {
	f := newFixture(t, false)
	v := verifierFor(t, f.config())
	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"issuer", func(c map[string]any) { c["iss"] = "https://another-issuer.example" }},
		{"audience", func(c map[string]any) { c["aud"] = "different-client" }},
		{"multi_aud_missing_azp", func(c map[string]any) { c["aud"] = []string{fixtureClientID, "another-client"} }},
		{"multi_aud_wrong_azp", func(c map[string]any) {
			c["aud"] = []string{fixtureClientID, "another-client"}
			c["azp"] = "another-client"
		}},
		{"single_aud_wrong_azp", func(c map[string]any) { c["azp"] = "another-client" }},
		{"expiry", func(c map[string]any) { c["exp"] = time.Now().Unix() - 30 }},
		{"missing_expiry", func(c map[string]any) { delete(c, "exp") }},
		{"missing_iat", func(c map[string]any) { delete(c, "iat") }},
		{"future_iat", func(c map[string]any) { c["iat"] = time.Now().Unix() + 120 }},
		{"future_nbf", func(c map[string]any) { c["nbf"] = time.Now().Unix() + 120 }},
		{"fractional_date", func(c map[string]any) { c["exp"] = float64(time.Now().Unix()+300) + 0.5 }},
		{"missing_subject", func(c map[string]any) { delete(c, "sub") }},
		{"control_in_subject", func(c map[string]any) { c["sub"] = "unsafe\x00subject" }},
		{"missing_nonce", func(c map[string]any) { delete(c, "nonce") }},
		{"wrong_nonce", func(c map[string]any) { c["nonce"] = "different-transaction-nonce" }},
		{"untyped_groups", func(c map[string]any) { c["groups"] = "owner" }},
		{"untyped_email_verified", func(c map[string]any) { c["email_verified"] = "true" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := f.claims()
			tc.change(c)
			if _, err := v.Verify(context.Background(), f.sign(t, "first", c), fixtureNonce); !errors.Is(err, identity.ErrInvalidToken) {
				t.Fatalf("invalid token accepted or unsafe error: %v", err)
			}
		})
	}
	t.Run("tampered_payload", func(t *testing.T) {
		raw := f.sign(t, "first", f.claims())
		parts := strings.Split(raw, ".")
		parts[1] = base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"attacker"}`))
		if _, err := v.Verify(context.Background(), strings.Join(parts, "."), fixtureNonce); !errors.Is(err, identity.ErrInvalidToken) {
			t.Fatalf("tampered signature accepted: %v", err)
		}
	})
	t.Run("unpublished_key", func(t *testing.T) {
		f.addKey(t, "untrusted")
		if _, err := v.Verify(context.Background(), f.sign(t, "untrusted", f.claims()), fixtureNonce); !errors.Is(err, identity.ErrInvalidToken) {
			t.Fatalf("unknown key accepted: %v", err)
		}
	})
}

func TestAuthorizedPartyForMultipleAudiences(t *testing.T) {
	f := newFixture(t, false)
	v := verifierFor(t, f.config())
	c := f.claims()
	c["aud"] = []string{fixtureClientID, "another-client"}
	c["azp"] = fixtureClientID
	if _, err := v.Verify(context.Background(), f.sign(t, "first", c), fixtureNonce); err != nil {
		t.Fatalf("authorized multi-audience token rejected: %v", err)
	}
}

func TestJWKSRotationConcurrentAndRetirement(t *testing.T) {
	f := newFixture(t, false)
	v := verifierFor(t, f.config())
	old := f.sign(t, "first", f.claims())
	if _, err := v.Verify(context.Background(), old, fixtureNonce); err != nil {
		t.Fatal(err)
	}
	f.addKey(t, "rotated")
	f.publish("rotated")
	rotated := f.sign(t, "rotated", f.claims())
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for range 24 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := v.Verify(context.Background(), rotated, fixtureNonce); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("rotated key rejected: %v", err)
		}
	}
	if f.keysRequests.Load() < 2 {
		t.Fatal("rotation did not fetch new JWKS")
	}
	if _, err := v.Verify(context.Background(), old, fixtureNonce); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("retired key remained accepted after JWKS refresh: %v", err)
	}
}

func TestRemovedKnownKeyExpiresWithoutNewKid(t *testing.T) {
	f := newFixture(t, false)
	cfg := f.config()
	cfg.KeyCacheTTL = 100 * time.Millisecond
	v := verifierFor(t, cfg)
	raw := f.sign(t, "first", f.claims())
	if _, err := v.Verify(context.Background(), raw, fixtureNonce); err != nil {
		t.Fatal(err)
	}
	f.publish()
	time.Sleep(150 * time.Millisecond)
	if _, err := v.Verify(context.Background(), raw, fixtureNonce); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("removed key accepted after configured cache TTL: %v", err)
	}
}

func TestTransportConfigurationAndTLSValidation(t *testing.T) {
	f := newFixture(t, false)
	for _, issuer := range []string{f.server.URL, "http://example.test", "http://localhost:9999", "http://192.0.2.1", "https://user:password@example.test", "https://example.test?issuer=bad"} {
		cfg := identity.Config{IssuerURL: issuer, ClientID: fixtureClientID}
		if _, err := identity.NewVerifier(context.Background(), cfg); !errors.Is(err, identity.ErrInvalidConfig) {
			t.Fatalf("insecure config was not rejected: %v", err)
		}
	}
	cfg := f.config()
	cfg.IssuerURL = "http://192.0.2.1"
	if _, err := identity.NewVerifier(context.Background(), cfg); !errors.Is(err, identity.ErrInvalidConfig) {
		t.Fatal("HTTP test flag allowed a non-loopback address")
	}
	secure := newFixture(t, true)
	cfg = secure.config()
	cfg.RootCAs = nil
	if _, err := identity.NewVerifier(context.Background(), cfg); !errors.Is(err, identity.ErrProviderUnavailable) {
		t.Fatalf("untrusted TLS certificate accepted: %v", err)
	}
	cfg = f.config()
	cfg.AllowedSigningAlgorithms = []string{"HS256"}
	if _, err := identity.NewVerifier(context.Background(), cfg); !errors.Is(err, identity.ErrInvalidConfig) {
		t.Fatal("symmetric signing algorithm accepted")
	}
	cfg = f.config()
	cfg.ClientID = ""
	if _, err := identity.NewVerifier(context.Background(), cfg); !errors.Is(err, identity.ErrInvalidConfig) {
		t.Fatal("empty audience accepted")
	}
}

func TestDiscoveryIssuerMismatchAndRedirects(t *testing.T) {
	f := newFixture(t, false)
	f.issuerOverride = "https://different.example"
	if _, err := identity.NewVerifier(context.Background(), f.config()); !errors.Is(err, identity.ErrProviderUnavailable) {
		t.Fatal("discovery issuer mismatch accepted")
	}
	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(500) }))
	defer target.Close()
	f.mu.Lock()
	f.issuerOverride = ""
	f.discoveryRedirect = target.URL
	f.mu.Unlock()
	if _, err := identity.NewVerifier(context.Background(), f.config()); !errors.Is(err, identity.ErrProviderUnavailable) {
		t.Fatal("discovery redirect accepted")
	}
	if hits.Load() != 0 {
		t.Fatal("discovery followed a redirect")
	}
	f.mu.Lock()
	f.discoveryRedirect = ""
	f.jwksRedirect = target.URL
	f.mu.Unlock()
	v := verifierFor(t, f.config())
	if _, err := v.Verify(context.Background(), f.sign(t, "first", f.claims()), fixtureNonce); err == nil {
		t.Fatal("JWKS redirect token accepted")
	}
	if hits.Load() != 0 {
		t.Fatal("JWKS followed a redirect")
	}
}

func TestJWKSOriginMustBeExplicitlyConfigured(t *testing.T) {
	f := newFixture(t, false)
	keysServer := httptest.NewServer(http.HandlerFunc(f.serve))
	defer keysServer.Close()
	f.mu.Lock()
	f.jwksOverride = keysServer.URL + "/keys"
	f.mu.Unlock()
	if _, err := identity.NewVerifier(context.Background(), f.config()); !errors.Is(err, identity.ErrProviderUnavailable) {
		t.Fatal("unconfigured JWKS origin accepted")
	}
	cfg := f.config()
	cfg.AdditionalJWKSOrigins = []string{keysServer.URL}
	v := verifierFor(t, cfg)
	if _, err := v.Verify(context.Background(), f.sign(t, "first", f.claims()), fixtureNonce); err != nil {
		t.Fatalf("administrator-configured origin rejected: %v", err)
	}
}

func TestTimeoutsAndResponseBounds(t *testing.T) {
	for _, phase := range []string{"discovery_delay", "jwks_delay", "discovery_size", "jwks_size"} {
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t, false)
			cfg := f.config()
			cfg.RequestTimeout = 100 * time.Millisecond
			f.mu.Lock()
			switch phase {
			case "discovery_delay":
				f.discoveryDelay = time.Second
			case "jwks_delay":
				f.jwksDelay = time.Second
			case "discovery_size":
				f.discoveryPadding = true
			case "jwks_size":
				f.jwksPadding = true
			}
			f.mu.Unlock()
			started := time.Now()
			v, err := identity.NewVerifier(context.Background(), cfg)
			if strings.HasPrefix(phase, "discovery") {
				if err == nil {
					v.Close()
					t.Fatal("invalid discovery accepted")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer v.Close()
				if _, err = v.Verify(context.Background(), f.sign(t, "first", f.claims()), fixtureNonce); err == nil {
					t.Fatal("invalid JWKS accepted")
				}
			}
			if time.Since(started) > time.Second {
				t.Fatal("provider operation exceeded timeout bound")
			}
		})
	}
}

func TestNoTokenValuesInErrorsAndClosedVerifier(t *testing.T) {
	f := newFixture(t, false)
	v := verifierFor(t, f.config())
	marker := "PRIVATE_TOKEN_MARKER_DO_NOT_LOG"
	for _, raw := range []string{marker, strings.Repeat(marker, 4000)} {
		_, err := v.Verify(context.Background(), raw, fixtureNonce)
		if err == nil || strings.Contains(err.Error(), marker) {
			t.Fatal("unsafe token error")
		}
	}
	raw := f.sign(t, "first", f.claims())
	if _, err := v.Verify(context.Background(), raw, ""); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatal("missing expected nonce accepted")
	}
	if _, err := v.Verify(context.Background(), raw, fixtureNonce); err != nil {
		t.Fatal("valid token could not warm cache")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := v.Verify(ctx, raw, fixtureNonce); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not propagated: %v", err)
	}
	v.Close()
	if _, err := v.Verify(context.Background(), raw, fixtureNonce); !errors.Is(err, identity.ErrClosed) {
		t.Fatal("closed verifier accepted new call")
	}
}
