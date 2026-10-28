// SPDX-License-Identifier: Elastic-2.0
package identity_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/identity"
	jose "github.com/go-jose/go-jose/v4"
)

const fixtureClientID = "workspace-test-client"
const fixtureNonce = "independent-login-transaction-nonce"

// providerFixture is a separate HTTP issuer, not an implementation mock. It
// generates private keys in memory and signs through go-jose. No private key or
// signed token is written to disk or included in assertion messages.
type providerFixture struct {
	server             *httptest.Server
	mu                 sync.RWMutex
	private            map[string]*rsa.PrivateKey
	published          []string
	issuerOverride     string
	jwksOverride       string
	discoveryRedirect  string
	jwksRedirect       string
	discoveryDelay     time.Duration
	jwksDelay          time.Duration
	discoveryPadding   bool
	jwksPadding        bool
	keysRequests       atomic.Int64
	credentialReceived atomic.Bool
	secure             bool
}

func newFixture(t *testing.T, secure bool) *providerFixture {
	t.Helper()
	f := &providerFixture{private: make(map[string]*rsa.PrivateKey), secure: secure}
	f.addKey(t, "first")
	f.published = []string{"first"}
	if secure {
		f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	} else {
		f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	}
	t.Cleanup(f.server.Close)
	return f
}

func (f *providerFixture) addKey(t *testing.T, kid string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal("fixture key generation failed")
	}
	f.mu.Lock()
	f.private[kid] = key
	f.mu.Unlock()
}

func (f *providerFixture) publish(kids ...string) {
	f.mu.Lock()
	f.published = append([]string(nil), kids...)
	f.mu.Unlock()
}

func (f *providerFixture) config() identity.Config {
	cfg := identity.Config{IssuerURL: f.server.URL, ClientID: fixtureClientID, RequestTimeout: time.Second, AllowLoopbackHTTPForTests: !f.secure}
	if f.secure {
		cfg.RootCAs = x509.NewCertPool()
		cfg.RootCAs.AddCert(f.server.Certificate())
	}
	return cfg
}

func (f *providerFixture) claims() map[string]any {
	now := time.Now().Unix()
	return map[string]any{
		"iss": f.server.URL, "sub": "employee-immutable-123", "aud": fixtureClientID,
		"iat": now - 1, "exp": now + 300, "nonce": fixtureNonce,
		"email": "employee@example.test", "email_verified": true,
		"groups": []string{"engineering", "owner"},
	}
}

func (f *providerFixture) sign(t *testing.T, kid string, claims map[string]any) string {
	t.Helper()
	f.mu.RLock()
	key := f.private[kid]
	f.mu.RUnlock()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	if err != nil {
		t.Fatal("fixture signer creation failed")
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal("fixture claims encoding failed")
	}
	signed, err := signer.Sign(body)
	if err != nil {
		t.Fatal("fixture signing failed")
	}
	compact, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal("fixture token encoding failed")
	}
	return compact
}

func (f *providerFixture) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
		f.credentialReceived.Store(true)
	}
	f.mu.RLock()
	issuer, jwks := f.issuerOverride, f.jwksOverride
	discoveryRedirect, jwksRedirect := f.discoveryRedirect, f.jwksRedirect
	discoveryDelay, jwksDelay := f.discoveryDelay, f.jwksDelay
	discoveryPadding, jwksPadding := f.discoveryPadding, f.jwksPadding
	keys := []jose.JSONWebKey{}
	for _, kid := range f.published {
		keys = append(keys, jose.JSONWebKey{Key: &f.private[kid].PublicKey, KeyID: kid, Algorithm: string(jose.RS256), Use: "sig"})
	}
	f.mu.RUnlock()
	if issuer == "" {
		issuer = f.server.URL
	}
	if jwks == "" {
		jwks = f.server.URL + "/keys"
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		if discoveryRedirect != "" {
			http.Redirect(w, r, discoveryRedirect, http.StatusFound)
			return
		}
		if !waitRequest(r, discoveryDelay) {
			return
		}
		metadata := map[string]any{"issuer": issuer, "jwks_uri": jwks, "authorization_endpoint": f.server.URL + "/authorize", "token_endpoint": f.server.URL + "/token", "id_token_signing_alg_values_supported": []string{"RS256"}, "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}}
		if discoveryPadding {
			metadata["padding"] = strings.Repeat("x", 1024*1024+1)
		}
		_ = json.NewEncoder(w).Encode(metadata)
	case "/keys":
		f.keysRequests.Add(1)
		if jwksRedirect != "" {
			http.Redirect(w, r, jwksRedirect, http.StatusFound)
			return
		}
		if !waitRequest(r, jwksDelay) {
			return
		}
		payload := map[string]any{"keys": keys}
		if jwksPadding {
			payload["padding"] = strings.Repeat("x", 1024*1024+1)
		}
		_ = json.NewEncoder(w).Encode(payload)
	default:
		http.NotFound(w, r)
	}
}

func waitRequest(r *http.Request, duration time.Duration) bool {
	if duration == 0 {
		return true
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-r.Context().Done():
		return false
	}
}
