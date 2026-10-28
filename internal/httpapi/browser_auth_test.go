// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/identity"
	"agentplatform.local/platform/internal/storage"
	"github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5"
)

func browserIntegrationStore(t *testing.T) *storage.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for real PostgreSQL browser integration")
	}
	ctx := context.Background()
	base, e := storage.Open(ctx, dsn)
	if e != nil {
		t.Fatal("test PostgreSQL unavailable")
	}
	schema := domain.NewID("browser_test")
	if _, e = base.Pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); e != nil {
		t.Fatal("test schema failed")
	}
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal("test database URL invalid")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, e := storage.Open(ctx, u.String())
	if e != nil {
		t.Fatal("isolated database unavailable")
	}
	t.Cleanup(func() {
		s.Close()
		_, _ = base.Pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		base.Close()
	})
	if e = s.Migrate(ctx); e != nil {
		t.Fatal("test migrations failed")
	}
	if e = s.ConfigureLoginEncryption(make([]byte, 32)); e != nil {
		t.Fatal("fixture encryption initialization failed")
	}
	return s
}

func TestBrowserLoginWiredAPIAndPermissions(t *testing.T) {
	s := browserIntegrationStore(t)
	ctx := context.Background()
	b, e := s.Bootstrap(ctx, "OIDC fixture organization", 10000000)
	if e != nil {
		t.Fatal("bootstrap failed")
	}
	other, e := s.Bootstrap(ctx, "Other fixture organization", 10000000)
	if e != nil {
		t.Fatal("bootstrap failed")
	}
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal("fixture key failed")
	}
	var provider *httptest.Server
	var origin string
	var mu sync.Mutex
	type fixtureCode struct{ nonce, challenge string }
	codes := map[string]fixtureCode{}
	provider = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": provider.URL, "jwks_uri": provider.URL + "/keys", "authorization_endpoint": provider.URL + "/authorize", "token_endpoint": provider.URL + "/token", "id_token_signing_alg_values_supported": []string{"RS256"}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "browser-http-fixture", Use: "sig", Algorithm: "RS256"}}})
		case "/authorize":
			q := r.URL.Query()
			if q.Get("redirect_uri") != origin+identity.LoginCallbackPath || q.Get("client_id") != "browser-fixture" || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
				w.WriteHeader(400)
				return
			}
			code := domain.NewID("code")
			mu.Lock()
			codes[code] = fixtureCode{q.Get("nonce"), q.Get("code_challenge")}
			mu.Unlock()
			w.Header().Set("Location", origin+identity.LoginCallbackPath+"?"+url.Values{"code": {code}, "state": {q.Get("state")}, "iss": {provider.URL}}.Encode())
			w.WriteHeader(302)
		case "/token":
			if r.Method != "POST" || r.ParseForm() != nil {
				w.WriteHeader(400)
				return
			}
			mu.Lock()
			code, ok := codes[r.Form.Get("code")]
			delete(codes, r.Form.Get("code"))
			mu.Unlock()
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if !ok || r.Form.Get("client_id") != "browser-fixture" || r.Form.Get("redirect_uri") != origin+identity.LoginCallbackPath || r.Form.Get("grant_type") != "authorization_code" || code.challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
				w.WriteHeader(400)
				return
			}
			claims, _ := json.Marshal(map[string]any{"iss": provider.URL, "aud": "browser-fixture", "sub": "stable-fixture-subject", "nonce": code.nonce, "iat": time.Now().Add(-time.Second).Unix(), "exp": time.Now().Add(time.Minute).Unix()})
			signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "browser-http-fixture"))
			if err != nil {
				w.WriteHeader(500)
				return
			}
			signed, err := signer.Sign(claims)
			if err != nil {
				w.WriteHeader(500)
				return
			}
			token, err := signed.CompactSerialize()
			if err != nil {
				w.WriteHeader(500)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-only", "token_type": "Bearer", "id_token": token})
		default:
			w.WriteHeader(404)
		}
	}))
	defer provider.Close()
	api := Server{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), StaticDir: "../../web/dist"}
	var handler atomic.Value
	app := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.Load().(http.Handler).ServeHTTP(w, r) }))
	defer app.Close()
	actual, _ := url.Parse(app.URL)
	host := app.Certificate().DNSNames[0]
	origin = "https://" + net.JoinHostPort(host, actual.Port())
	pool := x509.NewCertPool()
	pool.AddCert(provider.Certificate())
	pool.AddCert(app.Certificate())
	login, e := identity.NewLoginService(ctx, identity.LoginConfig{Provider: identity.Config{IssuerURL: provider.URL, ClientID: "browser-fixture", RootCAs: pool, RequestTimeout: time.Second}, PublicOrigin: origin, RedirectURI: origin + identity.LoginCallbackPath, TokenAuthMethod: "none", CSRFKey: make([]byte, 32)}, s)
	if e != nil {
		t.Fatal("fixture login initialization failed")
	}
	defer login.Close()
	api.Login = login
	handler.Store(api.Handler())
	if e = s.LinkIdentity(ctx, provider.URL, "stable-fixture-subject", b.PrincipalID); e != nil {
		t.Fatal("trusted identity mapping failed")
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == net.JoinHostPort(host, actual.Port()) {
			address = actual.Host
		}
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, address)
	}}
	defer transport.CloseIdleConnections()
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Transport: transport, Jar: jar, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	plain := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: browser.CheckRedirect}
	request := func(client *http.Client, method, path, body, csrf, org, bearer string) *http.Response {
		t.Helper()
		r, _ := http.NewRequest(method, origin+path, strings.NewReader(body))
		r.Header.Set(domain.APIContractHeader, domain.ContractVersion)
		r.Header.Set("Origin", origin)
		if org != "" {
			r.Header.Set("X-Organization-ID", org)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", domain.NewID("req"))
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal("wired HTTP request failed")
		}
		return response
	}
	read := func(response *http.Response, want int) []byte {
		t.Helper()
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != want {
			t.Fatalf("wired HTTP status=%d want=%d", response.StatusCode, want)
		}
		return data
	}
	// Optional test-only handoff keeps this real TLS API+PostgreSQL fixture alive
	// for the tracked Playwright smoke. The metadata contains no credentials.
	if path := os.Getenv("BROWSER_AUTH_SMOKE_METADATA"); path != "" {
		data, _ := json.Marshal(map[string]string{"url": origin, "organization_name": "OIDC fixture organization", "host": host})
		if e = os.WriteFile(path, data, 0600); e != nil {
			t.Fatal("smoke metadata write failed")
		}
		deadline := time.NewTimer(2 * time.Minute)
		defer deadline.Stop()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		waiting := true
		for waiting {
			select {
			case <-deadline.C:
				t.Fatal("browser smoke handoff timed out")
			case <-ticker.C:
				if _, e := os.Stat(path + ".done"); e == nil {
					waiting = false
				}
			}
		}
		_ = os.Remove(path)
		_ = os.Remove(path + ".done")
		if os.Getenv("BROWSER_AUTH_SECURITY_SMOKE") == "1" {
			// The security UI deliberately exercises mixed -> required -> mixed.
			// Restoring the mode must never revive the original human bearer.
			_, err := s.Authenticate(ctx, b.Token, b.OrganizationID)
			var denied *domain.Error
			if !errors.As(err, &denied) || denied.Code != "unauthenticated" || denied.Status != http.StatusUnauthorized {
				t.Fatal("security browser mode roundtrip must reject the original bearer with exact401")
			}
			fresh, _, err := s.IssueMemberToken(ctx, b.OrganizationID, b.PrincipalID, time.Hour)
			if err != nil {
				t.Fatal("cannot issue fresh fixture credential after explicit policy transition")
			}
			b.Token = fresh
		}
	}
	read(request(browser, "GET", "/auth/config", "", "", "", ""), 200)
	start := request(browser, "POST", identity.LoginStartPath, "", "", "", "")
	location := start.Header.Get("Location")
	read(start, 303)
	authorize, err := browser.Get(location)
	if err != nil {
		t.Fatal("fixture authorization failed")
	}
	callback := authorize.Header.Get("Location")
	read(authorize, 302)
	finished, err := browser.Get(callback)
	if err != nil {
		t.Fatal("fixture callback failed")
	}
	read(finished, 303)
	var view struct {
		Data struct {
			CSRF string `json:"csrf_token"`
		}
	}
	if json.Unmarshal(read(request(browser, "GET", identity.SessionPath, "", "", "", ""), 200), &view) != nil || view.Data.CSRF == "" {
		t.Fatal("session JSON invalid")
	}
	var orgs struct{ Data []storage.BrowserOrganization }
	if json.Unmarshal(read(request(browser, "GET", "/auth/organizations?limit=1", "", "", "", ""), 200), &orgs) != nil || len(orgs.Data) != 1 || orgs.Data[0].ID != b.OrganizationID {
		t.Fatal("organization list escaped membership")
	}
	read(request(browser, "GET", "/api/v1/agents", "", "", b.OrganizationID, ""), 200)
	read(request(browser, "GET", "/api/v1/models", "", "", b.OrganizationID, ""), 200)
	read(request(browser, "GET", "/api/v1/agents", "", "", other.OrganizationID, ""), 401)
	input := `{"name":"Browser fixture agent","instructions":"fixture only"}`
	read(request(browser, "POST", "/api/v1/agents", input, "", b.OrganizationID, ""), 403)
	read(request(browser, "POST", "/api/v1/agents", input, view.Data.CSRF, b.OrganizationID, ""), 201)
	read(request(plain, "GET", "/api/v1/agents", "", "", b.OrganizationID, b.Token), 200)
	read(request(browser, "GET", "/api/v1/agents", "", "", b.OrganizationID, b.Token), 401)
	u, _ := url.Parse(origin)
	var raw string
	for _, cookie := range jar.Cookies(u) {
		if cookie.Name == identity.SessionCookieName {
			raw = cookie.Value
		}
	}
	if raw == "" {
		t.Fatal("session cookie absent")
	}
	read(request(plain, "POST", "/api/v1/agents", input, "", b.OrganizationID, raw), 401)
	for _, name := range []string{"Authorization", "X-Organization-ID"} {
		r, _ := http.NewRequest("GET", origin+"/api/v1/agents", nil)
		r.Header.Set("Authorization", "Bearer "+b.Token)
		r.Header.Set("X-Organization-ID", b.OrganizationID)
		r.Header.Add(name, r.Header.Get(name))
		response, err := plain.Do(r)
		if err != nil {
			t.Fatal("duplicate header request failed")
		}
		read(response, 401)
	}
	principal, e := s.AuthenticateBrowserSession(ctx, storage.Hash([]byte(raw)), b.OrganizationID)
	if e != nil || principal.CredentialKind != storage.BrowserSessionCredential {
		t.Fatal("session principal kind lost")
	}
	if _, e = s.Pool.Exec(ctx, "UPDATE memberships SET role='viewer' WHERE principal_id=$1 AND organization_id=$2", b.PrincipalID, b.OrganizationID); e != nil {
		t.Fatal("role fixture update failed")
	}
	read(request(browser, "POST", "/api/v1/agents", input, view.Data.CSRF, b.OrganizationID, ""), 403)
	if _, e = s.CreateAgent(ctx, principal, domain.NewID("key"), domain.CreateAgent{Name: "stale role", Instructions: "fixture"}); e == nil {
		t.Fatal("transaction trusted stale owner role")
	}
	if e = s.RevokeSession(ctx, storage.Hash([]byte(raw))); e != nil {
		t.Fatal("revoke failed")
	}
	read(request(browser, "GET", "/api/v1/agents", "", "", b.OrganizationID, ""), 401)
	read(request(plain, "GET", "/api/v1/agents", "", "", b.OrganizationID, b.Token), 200)
}
