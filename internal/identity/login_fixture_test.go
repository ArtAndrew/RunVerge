// SPDX-License-Identifier: Elastic-2.0
package identity_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/identity"
	"github.com/go-jose/go-jose/v4"
)

// This mutex-protected store is deliberately test-only. Production construction
// requires the integrator's durable PostgreSQL LoginStore implementation.
type loginTestStore struct {
	mu                                sync.Mutex
	transactions                      map[string]identity.LoginTransaction
	sessions                          map[string]identity.BrowserSession
	mappings                          map[string]string
	unavailable, createSessionFailure bool
}

func newLoginTestStore() *loginTestStore {
	return &loginTestStore{transactions: map[string]identity.LoginTransaction{}, sessions: map[string]identity.BrowserSession{}, mappings: map[string]string{}}
}
func (s *loginTestStore) CreateLogin(_ context.Context, tx identity.LoginTransaction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return identity.ErrLoginStoreUnavailable
	}
	if _, present := s.transactions[tx.StateHash]; present {
		return identity.ErrLoginRejected
	}
	s.transactions[tx.StateHash] = tx
	return nil
}
func (s *loginTestStore) ConsumeLogin(_ context.Context, state, binding string) (identity.LoginTransaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return identity.LoginTransaction{}, identity.ErrLoginStoreUnavailable
	}
	tx, ok := s.transactions[state]
	if !ok || tx.BindingHash != binding || !time.Now().Before(tx.ExpiresAt) {
		return identity.LoginTransaction{}, identity.ErrLoginRejected
	}
	delete(s.transactions, state)
	return tx, nil
}
func (s *loginTestStore) ResolveIdentity(_ context.Context, issuer, subject string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return "", identity.ErrLoginStoreUnavailable
	}
	if id := s.mappings[issuer+"\x00"+subject]; id != "" {
		return id, nil
	}
	return "", identity.ErrIdentityUnmapped
}
func (s *loginTestStore) CreateSession(_ context.Context, session identity.BrowserSession, previous string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable || s.createSessionFailure {
		return identity.ErrLoginStoreUnavailable
	}
	if s.mappings[session.Issuer+"\x00"+session.Subject] != session.PrincipalID {
		return identity.ErrIdentityUnmapped
	}
	if _, ok := s.sessions[session.TokenHash]; ok {
		return identity.ErrSessionInvalid
	}
	delete(s.sessions, previous)
	s.sessions[session.TokenHash] = session
	return nil
}
func (s *loginTestStore) GetSession(_ context.Context, hash string) (identity.BrowserSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return identity.BrowserSession{}, identity.ErrLoginStoreUnavailable
	}
	session, ok := s.sessions[hash]
	if !ok || !time.Now().Before(session.ExpiresAt) || s.mappings[session.Issuer+"\x00"+session.Subject] != session.PrincipalID {
		return identity.BrowserSession{}, identity.ErrSessionInvalid
	}
	return session, nil
}
func (s *loginTestStore) RotateSession(_ context.Context, hash string, replacement identity.BrowserSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return identity.ErrLoginStoreUnavailable
	}
	old, ok := s.sessions[hash]
	if !ok || !time.Now().Before(old.ExpiresAt) || s.mappings[old.Issuer+"\x00"+old.Subject] != old.PrincipalID ||
		old.PrincipalID != replacement.PrincipalID || old.Issuer != replacement.Issuer || old.Subject != replacement.Subject ||
		!old.ExpiresAt.Equal(replacement.ExpiresAt) || !old.AuthenticatedAt.Equal(replacement.AuthenticatedAt) {
		return identity.ErrSessionInvalid
	}
	delete(s.sessions, hash)
	s.sessions[replacement.TokenHash] = replacement
	return nil
}
func (s *loginTestStore) RevokeSession(_ context.Context, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return identity.ErrLoginStoreUnavailable
	}
	delete(s.sessions, hash)
	return nil
}

type loginFixtureCode struct{ nonce, challenge, redirect string }
type loginIssuer struct {
	server                                                                  *httptest.Server
	key                                                                     *rsa.PrivateKey
	mu                                                                      sync.Mutex
	codes                                                                   map[string]loginFixtureCode
	redirectURI, secret, authMethod, tokenRedirect, tokenNonce, tokenIssuer string
	tokenEndpoint, authEndpoint                                             string
	tokenStatus                                                             int
	tokenDelay, tokenBodyDelay                                              time.Duration
	oversize, missingID                                                     bool
	sequence, tokenCalls                                                    atomic.Int64
	browserCookieLeak                                                       atomic.Bool
}

func newLoginIssuer(t *testing.T) *loginIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal("key generation failed")
	}
	f := &loginIssuer{key: key, codes: map[string]loginFixtureCode{}, authMethod: "none"}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}
func (f *loginIssuer) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		f.mu.Lock()
		method, authEndpoint, tokenEndpoint := f.authMethod, f.authEndpoint, f.tokenEndpoint
		f.mu.Unlock()
		if authEndpoint == "" {
			authEndpoint = f.server.URL + "/authorize"
		}
		if tokenEndpoint == "" {
			tokenEndpoint = f.server.URL + "/token"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": f.server.URL, "jwks_uri": f.server.URL + "/keys",
			"authorization_endpoint": authEndpoint, "token_endpoint": tokenEndpoint,
			"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"}, "code_challenge_methods_supported": []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{method}, "authorization_response_iss_parameter_supported": true})
	case "/keys":
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []jose.JSONWebKey{{Key: &f.key.PublicKey, KeyID: "login-fixture", Use: "sig", Algorithm: "RS256"}}})
	case "/authorize":
		if r.Header.Get("Cookie") != "" {
			f.browserCookieLeak.Store(true)
		}
		q := r.URL.Query()
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != "GET" || q.Get("client_id") != fixtureClientID || q.Get("redirect_uri") != f.redirectURI || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || len(q.Get("nonce")) != 43 || len(q.Get("state")) != 43 || len(q.Get("code_challenge")) != 43 {
			http.Error(w, "invalid authorization request", 400)
			return
		}
		code := fmt.Sprintf("fixture-code-%d", f.sequence.Add(1))
		f.codes[code] = loginFixtureCode{q.Get("nonce"), q.Get("code_challenge"), f.redirectURI}
		callback := f.redirectURI + "?" + url.Values{"code": {code}, "state": {q.Get("state")}, "iss": {f.server.URL}}.Encode()
		w.Header().Set("Location", callback)
		w.WriteHeader(http.StatusFound)
	case "/token":
		f.tokenCalls.Add(1)
		f.mu.Lock()
		redirect, status, delay, bodyDelay, oversize, missingID, nonceOverride, issuerOverride, method, secret := f.tokenRedirect, f.tokenStatus, f.tokenDelay, f.tokenBodyDelay, f.oversize, f.missingID, f.tokenNonce, f.tokenIssuer, f.authMethod, f.secret
		f.mu.Unlock()
		if redirect != "" {
			w.Header().Set("Location", redirect)
			w.WriteHeader(307)
			return
		}
		if delay > 0 && !waitRequest(r, delay) {
			return
		}
		if bodyDelay > 0 {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			if !waitRequest(r, bodyDelay) {
				return
			}
		}
		if status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":"PRIVATE_PROVIDER_ERROR"}`)
			return
		}
		if oversize {
			_, _ = io.WriteString(w, strings.Repeat("x", 1024*1024+1))
			return
		}
		if r.Method != "POST" || r.ParseForm() != nil || r.Form.Get("grant_type") != "authorization_code" {
			http.Error(w, "invalid request", 400)
			return
		}
		if method == "none" {
			if r.Form.Get("client_id") != fixtureClientID || r.Header.Get("Authorization") != "" {
				http.Error(w, "client rejected", 400)
				return
			}
		} else {
			user, password, ok := r.BasicAuth()
			if !ok || user != fixtureClientID || password != secret {
				http.Error(w, "client rejected", 400)
				return
			}
		}
		f.mu.Lock()
		code, ok := f.codes[r.Form.Get("code")]
		delete(f.codes, r.Form.Get("code"))
		f.mu.Unlock()
		challenge := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(challenge[:]) != code.challenge || r.Form.Get("redirect_uri") != code.redirect {
			http.Error(w, "code rejected", 400)
			return
		}
		if nonceOverride != "" {
			code.nonce = nonceOverride
		}
		issuer := f.server.URL
		if issuerOverride != "" {
			issuer = issuerOverride
		}
		now := time.Now().Unix()
		claims := map[string]any{"iss": issuer, "sub": "stable-employee", "aud": fixtureClientID, "nonce": code.nonce, "iat": now - 1, "exp": now + 300, "email": "changed@example.test", "email_verified": true, "groups": []string{"owner"}}
		body, _ := json.Marshal(claims)
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key}, (&jose.SignerOptions{}).WithHeader("kid", "login-fixture"))
		if err != nil {
			http.Error(w, "fixture unavailable", 500)
			return
		}
		signed, err := signer.Sign(body)
		if err != nil {
			http.Error(w, "fixture unavailable", 500)
			return
		}
		raw, err := signed.CompactSerialize()
		if err != nil {
			http.Error(w, "fixture unavailable", 500)
			return
		}
		response := map[string]any{"access_token": "fixture-access-not-platform-credential", "token_type": "Bearer", "expires_in": 300}
		if !missingID {
			response["id_token"] = raw
		}
		_ = json.NewEncoder(w).Encode(response)
	default:
		http.NotFound(w, r)
	}
}

type loginEnvironment struct {
	issuer    *loginIssuer
	store     *loginTestStore
	service   *identity.LoginService
	app       *httptest.Server
	origin    string
	transport *http.Transport
	cfg       identity.LoginConfig
	current   *atomic.Pointer[identity.LoginService]
}

func newLoginEnvironment(t *testing.T, change func(*identity.LoginConfig, *loginIssuer)) *loginEnvironment {
	t.Helper()
	issuer := newLoginIssuer(t)
	store := newLoginTestStore()
	current := new(atomic.Pointer[identity.LoginService])
	app := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { current.Load().Handler().ServeHTTP(w, r) }))
	t.Cleanup(app.Close)
	actual, _ := url.Parse(app.URL)
	// Distinct browser hosts: cookies have no port isolation. The httptest
	// certificate covers example.com; the test dialer routes it only to loopback.
	if len(app.Certificate().DNSNames) == 0 {
		t.Fatal("fixture certificate has no DNS name")
	}
	origin := "https://" + net.JoinHostPort(app.Certificate().DNSNames[0], actual.Port())
	issuer.redirectURI = origin + identity.LoginCallbackPath
	pool := x509.NewCertPool()
	pool.AddCert(issuer.server.Certificate())
	pool.AddCert(app.Certificate())
	cfg := identity.LoginConfig{Provider: identity.Config{IssuerURL: issuer.server.URL, ClientID: fixtureClientID, RootCAs: pool, RequestTimeout: time.Second}, PublicOrigin: origin, RedirectURI: issuer.redirectURI, TokenAuthMethod: "none", CSRFKey: make([]byte, 32)}
	_, _ = rand.Read(cfg.CSRFKey)
	if change != nil {
		change(&cfg, issuer)
	}
	service, err := identity.NewLoginService(context.Background(), cfg, store)
	if err != nil {
		t.Fatalf("service construction: %v", err)
	}
	t.Cleanup(service.Close)
	current.Store(service)
	store.mappings[issuer.server.URL+"\x00stable-employee"] = "principal-fixture"
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address == net.JoinHostPort(app.Certificate().DNSNames[0], actual.Port()) {
				address = actual.Host
			}
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, address)
		}}
	t.Cleanup(transport.CloseIdleConnections)
	return &loginEnvironment{issuer: issuer, store: store, service: service, app: app, origin: origin, transport: transport, cfg: cfg, current: current}
}
func (e *loginEnvironment) browser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Transport: e.transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func (e *loginEnvironment) request(t *testing.T, browser *http.Client, method, path, csrf string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, e.origin+path, nil)
	if err != nil {
		t.Fatal("request creation failed")
	}
	request.Header.Set("Origin", e.origin)
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	response, err := browser.Do(request)
	if err != nil {
		t.Fatalf("local request failed: %v", err)
	}
	return response
}
func loginBody(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal("response read failed")
	}
	return body
}
func expectLoginStatus(t *testing.T, response *http.Response, want int) {
	t.Helper()
	if response.StatusCode != want {
		loginBody(t, response)
		t.Fatalf("HTTP status %d; want %d", response.StatusCode, want)
	}
}
func (e *loginEnvironment) prepare(t *testing.T, browser *http.Client) (string, *http.Cookie) {
	t.Helper()
	response := e.request(t, browser, http.MethodPost, identity.LoginStartPath, "")
	expectLoginStatus(t, response, 303)
	var binding *http.Cookie
	for _, cookie := range response.Cookies() {
		if cookie.Name == identity.LoginCookieName {
			binding = cookie
		}
	}
	if binding == nil {
		t.Fatal("binding cookie absent")
	}
	if !binding.HttpOnly || !binding.Secure || binding.SameSite != http.SameSiteLaxMode || binding.Path != "/" || binding.Domain != "" {
		t.Fatal("insecure binding cookie")
	}
	location := response.Header.Get("Location")
	loginBody(t, response)
	authorize, err := url.Parse(location)
	if err != nil {
		t.Fatal("authorization URL invalid")
	}
	q := authorize.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_verifier") != "" || q.Get("state") == q.Get("nonce") {
		t.Fatal("incorrect PKCE/state/nonce request")
	}
	response, err = browser.Get(location)
	if err != nil {
		t.Fatal("authorization fixture failed")
	}
	expectLoginStatus(t, response, 302)
	callback := response.Header.Get("Location")
	loginBody(t, response)
	return callback, binding
}
func (e *loginEnvironment) finish(t *testing.T, browser *http.Client, callback string) *http.Response {
	t.Helper()
	response, err := browser.Get(callback)
	if err != nil {
		t.Fatal("callback request failed")
	}
	return response
}
func (e *loginEnvironment) login(t *testing.T, browser *http.Client) *http.Cookie {
	t.Helper()
	callback, _ := e.prepare(t, browser)
	response := e.finish(t, browser, callback)
	expectLoginStatus(t, response, 303)
	if response.Header.Get("Location") != e.origin+"/" {
		t.Fatal("unexpected post-login destination")
	}
	var session *http.Cookie
	for _, cookie := range response.Cookies() {
		if cookie.Name == identity.SessionCookieName {
			session = cookie
		}
	}
	loginBody(t, response)
	if session == nil {
		t.Fatal("session cookie absent")
	}
	return session
}

type loginSessionView struct {
	SessionID   string    `json:"session_id"`
	PrincipalID string    `json:"principal_id"`
	ExpiresAt   time.Time `json:"expires_at"`
	CSRFToken   string    `json:"csrf_token"`
}

func (e *loginEnvironment) session(t *testing.T, browser *http.Client) loginSessionView {
	t.Helper()
	response := e.request(t, browser, "GET", identity.SessionPath, "")
	expectLoginStatus(t, response, 200)
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("session response must not cache")
	}
	body := loginBody(t, response)
	var envelope struct {
		Data loginSessionView `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || len(envelope.Data.CSRFToken) != 43 {
		t.Fatal("invalid session JSON")
	}
	return envelope.Data
}
