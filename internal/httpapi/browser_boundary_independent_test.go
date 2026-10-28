// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/identity"
	"agentplatform.local/platform/internal/storage"
)

type independentBrowserFixture struct {
	t                 *testing.T
	store             *storage.Store
	bootstrap         storage.Bootstrap
	other             storage.Bootstrap
	origin, raw, csrf string
	client            *http.Client
}

// This fixture exercises real TLS HTTP and PostgreSQL. The browser session is
// created through the trusted store API; it does not claim an IdP login flow.
func independentBrowserAPI(t *testing.T) *independentBrowserFixture {
	t.Helper()
	ctx := context.Background()
	s := browserIntegrationStore(t)
	b, e := s.Bootstrap(ctx, "independent browser organization", 10000000)
	if e != nil {
		t.Fatal(e)
	}
	other, e := s.Bootstrap(ctx, "unrelated organization", 10000000)
	if e != nil {
		t.Fatal(e)
	}
	var provider *httptest.Server
	provider = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": provider.URL, "jwks_uri": provider.URL + "/keys", "authorization_endpoint": provider.URL + "/authorize", "token_endpoint": provider.URL + "/token", "id_token_signing_alg_values_supported": []string{"RS256"}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}})
	}))
	t.Cleanup(provider.Close)
	var handler atomic.Value
	app := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.Load().(http.Handler).ServeHTTP(w, r) }))
	t.Cleanup(app.Close)
	actual, _ := url.Parse(app.URL)
	host := app.Certificate().DNSNames[0]
	origin := "https://" + net.JoinHostPort(host, actual.Port())
	pool := x509.NewCertPool()
	pool.AddCert(provider.Certificate())
	pool.AddCert(app.Certificate())
	login, e := identity.NewLoginService(ctx, identity.LoginConfig{Provider: identity.Config{IssuerURL: provider.URL, ClientID: "independent-boundary", RootCAs: pool, RequestTimeout: time.Second}, PublicOrigin: origin, RedirectURI: origin + identity.LoginCallbackPath, TokenAuthMethod: "none", CSRFKey: make([]byte, 32)}, s)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(login.Close)
	api := &Server{Store: s, Login: login, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	handler.Store(api.Handler())
	if e = s.LinkIdentity(ctx, provider.URL, "independent-subject", b.PrincipalID); e != nil {
		t.Fatal(e)
	}
	var secret [32]byte
	if _, e = rand.Read(secret[:]); e != nil {
		t.Fatal(e)
	}
	raw := base64.RawURLEncoding.EncodeToString(secret[:])
	now := time.Now().UTC().Truncate(time.Microsecond)
	if e = s.CreateSession(ctx, identity.BrowserSession{ID: domain.NewID("ses"), TokenHash: storage.Hash([]byte(raw)), PrincipalID: b.PrincipalID, Issuer: provider.URL, Subject: "independent-subject", CreatedAt: now, AuthenticatedAt: now, ExpiresAt: now.Add(time.Hour)}, ""); e != nil {
		t.Fatal(e)
	}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == net.JoinHostPort(host, actual.Port()) {
			address = actual.Host
		}
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, address)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	f := &independentBrowserFixture{t: t, store: s, bootstrap: b, other: other, origin: origin, raw: raw, client: &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	_, body := f.request("GET", identity.SessionPath, "", nil, 200)
	var session struct {
		Data struct {
			CSRF string `json:"csrf_token"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &session) != nil || session.Data.CSRF == "" {
		t.Fatal("missing session CSRF")
	}
	f.csrf = session.Data.CSRF
	return f
}
func (f *independentBrowserFixture) request(method, path, body string, change func(*http.Request), want int) (*http.Response, []byte) {
	f.t.Helper()
	r, e := http.NewRequest(method, f.origin+path, strings.NewReader(body))
	if e != nil {
		f.t.Fatal(e)
	}
	r.Header.Set("Cookie", identity.SessionCookieName+"="+f.raw)
	r.Header.Set("X-Organization-ID", f.bootstrap.OrganizationID)
	r.Header.Set(domain.APIContractHeader, domain.ContractVersion)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", domain.NewID("key"))
	}
	if method != "GET" && method != "HEAD" {
		r.Header.Set("Origin", f.origin)
		r.Header.Set("X-CSRF-Token", f.csrf)
	}
	if change != nil {
		change(r)
	}
	response, e := f.client.Do(r)
	if e != nil {
		f.t.Fatal("fixture HTTP failed:", e)
	}
	defer response.Body.Close()
	data, e := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if e != nil {
		f.t.Fatal(e)
	}
	if response.StatusCode != want {
		f.t.Fatalf("%s %s: status=%d want=%d", method, path, response.StatusCode, want)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		f.t.Fatal("sensitive response is cacheable")
	}
	return response, data
}
func TestIndependentBrowserHTTPRejectsCredentialAmbiguity(t *testing.T) {
	f := independentBrowserAPI(t)
	cases := []struct {
		name   string
		change func(*http.Request)
	}{
		{"cookie_and_bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.bootstrap.Token) }},
		{"invalid_bearer_no_cookie_fallback", func(r *http.Request) { r.Header.Set("Authorization", "Bearer invalid") }},
		{"empty_authorization", func(r *http.Request) { r.Header["Authorization"] = []string{""} }},
		{"duplicate_authorization", func(r *http.Request) {
			r.Header.Del("Cookie")
			r.Header["Authorization"] = []string{"Bearer " + f.bootstrap.Token, "Bearer " + f.bootstrap.Token}
		}},
		{"malformed_cookie_with_bearer", func(r *http.Request) {
			r.Header.Set("Cookie", identity.SessionCookieName+"=\"unterminated")
			r.Header.Set("Authorization", "Bearer "+f.bootstrap.Token)
		}},
		{"duplicate_cookie", func(r *http.Request) { r.Header.Add("Cookie", identity.SessionCookieName+"="+f.raw) }},
		{"malformed_duplicate_cookie", func(r *http.Request) { r.Header.Add("Cookie", identity.SessionCookieName+"=\"unterminated") }},
		{"duplicate_organization", func(r *http.Request) { r.Header.Add("X-Organization-ID", f.bootstrap.OrganizationID) }},
		{"foreign_organization", func(r *http.Request) { r.Header.Set("X-Organization-ID", f.other.OrganizationID) }},
		{"forwarded_host_not_authority", func(r *http.Request) {
			r.Host = "attacker.example.test"
			r.Header.Set("X-Forwarded-Host", strings.TrimPrefix(f.origin, "https://"))
			r.Header.Set("X-Forwarded-Proto", "https")
		}},
		{"cross_origin_read", func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example.test") }},
		{"cross_site_read", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { f.t = t; f.request("GET", "/api/v1/agents", "", c.change, 401) })
	}
	f.t = t
	f.request("GET", "/api/v1/agents", "", func(r *http.Request) {
		r.Header.Del("Cookie")
		r.Header.Set("Authorization", "Bearer "+f.bootstrap.Token)
	}, 200)
}
func TestIndependentBrowserHTTPCSRFAndRotation(t *testing.T) {
	f := independentBrowserAPI(t)
	body := `{"name":"independent browser agent","instructions":"fixture"}`
	for _, c := range []struct {
		name   string
		change func(*http.Request)
	}{
		{"missing", func(r *http.Request) { r.Header.Del("X-CSRF-Token") }},
		{"duplicate", func(r *http.Request) { r.Header.Add("X-CSRF-Token", f.csrf) }},
		{"wrong_token", func(r *http.Request) { r.Header.Set("X-CSRF-Token", f.raw) }},
		{"origin_missing", func(r *http.Request) { r.Header.Del("Origin") }},
		{"origin_cross_site", func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example.test") }},
		{"origin_duplicate", func(r *http.Request) { r.Header.Add("Origin", f.origin) }},
		{"origin_null", func(r *http.Request) { r.Header.Set("Origin", "null") }},
	} {
		t.Run(c.name, func(t *testing.T) { f.t = t; f.request("POST", "/api/v1/agents", body, c.change, 403) })
	}
	f.t = t
	var count int
	if e := f.store.Pool.QueryRow(context.Background(), "SELECT count(*) FROM agents").Scan(&count); e != nil || count != 0 {
		t.Fatal("rejected mutation had side effect")
	}
	oldRaw, oldCSRF := f.raw, f.csrf
	response, data := f.request("POST", identity.SessionRotatePath, "", nil, 200)
	for _, cookie := range response.Cookies() {
		if cookie.Name == identity.SessionCookieName {
			f.raw = cookie.Value
		}
	}
	if f.raw == oldRaw {
		t.Fatal("rotation did not change cookie")
	}
	var rotated struct {
		Data struct {
			CSRF string `json:"csrf_token"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &rotated) != nil || rotated.Data.CSRF == "" {
		t.Fatal("missing rotated CSRF")
	}
	f.csrf = oldCSRF
	f.request("POST", "/api/v1/agents", body, nil, 403)
	f.csrf = rotated.Data.CSRF
	f.request("GET", "/api/v1/agents", "", func(r *http.Request) { r.Header.Set("Cookie", identity.SessionCookieName+"="+oldRaw) }, 401)
	f.request("POST", "/api/v1/agents", body, nil, 201)
}
func TestIndependentBrowserHTTPOrganizationPaginationAndRevocation(t *testing.T) {
	f := independentBrowserAPI(t)
	ctx := context.Background()
	extra, e := f.store.Bootstrap(ctx, "second visible organization", 10000)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.store.Pool.Exec(ctx, "INSERT INTO memberships(organization_id,principal_id,role) VALUES($1,$2,'viewer')", extra.OrganizationID, f.bootstrap.PrincipalID); e != nil {
		t.Fatal(e)
	}
	type page struct {
		Data []storage.BrowserOrganization `json:"data"`
		Next string                        `json:"next_cursor"`
	}
	_, data := f.request("GET", "/auth/organizations?limit=1", "", nil, 200)
	var first page
	if json.Unmarshal(data, &first) != nil || len(first.Data) != 1 || first.Next == "" {
		t.Fatal("first page missing cursor")
	}
	_, data = f.request("GET", "/auth/organizations?limit=1&cursor="+url.QueryEscape(first.Next), "", nil, 200)
	var second page
	if json.Unmarshal(data, &second) != nil || len(second.Data) != 1 || second.Next != "" || first.Data[0].ID == second.Data[0].ID {
		t.Fatal("second page incorrect")
	}
	seen := map[string]bool{first.Data[0].ID: true, second.Data[0].ID: true}
	if !seen[f.bootstrap.OrganizationID] || !seen[extra.OrganizationID] || seen[f.other.OrganizationID] {
		t.Fatal("organization discovery crossed membership scope")
	}
	for _, query := range []string{"?limit=1&limit=2", "?unknown=1", "?cursor=garbage", "?Limit=1"} {
		f.request("GET", "/auth/organizations"+query, "", nil, 400)
	}
	if _, e = f.store.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", extra.OrganizationID, f.bootstrap.PrincipalID); e != nil {
		t.Fatal(e)
	}
	_, data = f.request("GET", "/auth/organizations", "", nil, 200)
	var remaining page
	if json.Unmarshal(data, &remaining) != nil || len(remaining.Data) != 1 || remaining.Data[0].ID != f.bootstrap.OrganizationID {
		t.Fatal("revoked membership remained visible")
	}
	f.request("GET", "/api/v1/agents", "", func(r *http.Request) { r.Header.Set("X-Organization-ID", extra.OrganizationID) }, 401)
	if _, e = f.store.Pool.Exec(ctx, "UPDATE organizations SET active=false WHERE id=$1", f.bootstrap.OrganizationID); e != nil {
		t.Fatal(e)
	}
	_, data = f.request("GET", "/auth/organizations", "", nil, 200)
	if json.Unmarshal(data, &remaining) != nil || len(remaining.Data) != 0 {
		t.Fatal("inactive organization visible")
	}
	if e = f.store.RevokeSession(ctx, storage.Hash([]byte(f.raw))); e != nil {
		t.Fatal(e)
	}
	f.request("GET", "/auth/organizations", "", nil, 401)
}
func TestIndependentBrowserHTTPDefaultDiscoveryIsDisabled(t *testing.T) {
	api := &Server{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	app := httptest.NewServer(api.Handler())
	defer app.Close()
	for _, c := range []struct {
		path string
		want int
	}{{"/auth/config", 200}, {"/auth/config?issuer=anything", 400}, {identity.LoginStartPath, 404}, {identity.SessionPath, 404}, {"/auth/organizations", 401}, {"/api/v1/agents", 401}} {
		response, e := app.Client().Get(app.URL + c.path)
		if e != nil {
			t.Fatal(e)
		}
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != c.want {
			t.Fatalf("disabled route %s: %d", c.path, response.StatusCode)
		}
		if c.path == "/auth/config" && !strings.Contains(string(data), `"oidc_enabled":false`) {
			t.Fatal("disabled discovery inaccurate")
		}
	}
}
