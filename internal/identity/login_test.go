// SPDX-License-Identifier: Elastic-2.0
package identity_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/identity"
)

func TestBrowserLoginTLSFlowRotationLogout(t *testing.T) {
	for _, method := range []string{"none", "client_secret_basic"} {
		t.Run(method, func(t *testing.T) {
			e := newLoginEnvironment(t, func(cfg *identity.LoginConfig, f *loginIssuer) {
				cfg.TokenAuthMethod = method
				f.authMethod = method
				if method != "none" {
					cfg.ClientSecret = "fixture-confidential-client-secret"
					f.secret = cfg.ClientSecret
				}
			})
			browser := e.browser(t)
			cookie := e.login(t, browser)
			if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" {
				t.Fatal("insecure session cookie")
			}
			initial := e.session(t, browser)
			if initial.PrincipalID != "principal-fixture" || initial.SessionID == "" {
				t.Fatal("stable mapped principal absent")
			}
			if e.issuer.browserCookieLeak.Load() {
				t.Fatal("browser session/binding cookie leaked to distinct IdP host")
			}
			response := e.request(t, browser, "POST", identity.SessionRotatePath, initial.CSRFToken)
			expectLoginStatus(t, response, 200)
			loginBody(t, response)
			rotated := e.session(t, browser)
			if rotated.SessionID == initial.SessionID || rotated.CSRFToken == initial.CSRFToken || !rotated.ExpiresAt.Equal(initial.ExpiresAt) {
				t.Fatal("rotation did not replace token and preserve absolute lifetime")
			}
			old := e.browser(t)
			appURL, _ := url.Parse(e.origin)
			old.Jar.SetCookies(appURL, []*http.Cookie{cookie})
			response = e.request(t, old, "GET", identity.SessionPath, "")
			expectLoginStatus(t, response, 401)
			loginBody(t, response)
			response = e.request(t, browser, "POST", identity.LogoutPath, rotated.CSRFToken)
			expectLoginStatus(t, response, 204)
			for _, removed := range response.Cookies() {
				if removed.MaxAge != -1 || !removed.Secure || !removed.HttpOnly {
					t.Fatal("logout did not securely clear cookies")
				}
			}
			loginBody(t, response)
			response = e.request(t, browser, "GET", identity.SessionPath, "")
			expectLoginStatus(t, response, 401)
			loginBody(t, response)
		})
	}
}

func TestBrowserLoginStateCodeReplayAndConcurrentCallback(t *testing.T) {
	e := newLoginEnvironment(t, nil)
	browser := e.browser(t)
	callback, binding := e.prepare(t, browser)
	var successes atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			response, err := browser.Get(callback)
			if err != nil {
				return
			}
			defer response.Body.Close()
			_, _ = io.Copy(io.Discard, response.Body)
			if response.StatusCode == 303 {
				successes.Add(1)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 1 || e.issuer.tokenCalls.Load() != 1 {
		t.Fatal("concurrent callback consumed state or exchanged code more than once")
	}
	appURL, _ := url.Parse(e.origin)
	browser.Jar.SetCookies(appURL, []*http.Cookie{binding})
	response := e.finish(t, browser, callback)
	expectLoginStatus(t, response, 400)
	loginBody(t, response)
	if e.issuer.tokenCalls.Load() != 1 {
		t.Fatal("state replay reached token endpoint")
	}
	second, _ := e.prepare(t, browser)
	originalURL, _ := url.Parse(callback)
	secondURL, _ := url.Parse(second)
	q := secondURL.Query()
	q.Set("code", originalURL.Query().Get("code"))
	secondURL.RawQuery = q.Encode()
	response = e.finish(t, browser, secondURL.String())
	expectLoginStatus(t, response, 400)
	loginBody(t, response)
	if e.issuer.tokenCalls.Load() != 2 {
		t.Fatal("unexpected number of code exchange attempts")
	}
	response = e.finish(t, browser, second)
	expectLoginStatus(t, response, 400)
	loginBody(t, response)
	if e.issuer.tokenCalls.Load() != 2 {
		t.Fatal("failed exchange made transaction reusable")
	}
}

func TestBrowserLoginCSRFAndSessionFixation(t *testing.T) {
	e := newLoginEnvironment(t, nil)
	attacker, victim := e.browser(t), e.browser(t)
	attackerCallback, _ := e.prepare(t, attacker)
	_, _ = e.prepare(t, victim)
	response := e.finish(t, victim, attackerCallback)
	expectLoginStatus(t, response, 400)
	loginBody(t, response)
	if e.issuer.tokenCalls.Load() != 0 {
		t.Fatal("foreign browser binding reached exchange")
	}
	response = e.finish(t, attacker, attackerCallback)
	expectLoginStatus(t, response, 303)
	loginBody(t, response)
	initial := e.session(t, attacker)
	appURL, _ := url.Parse(e.origin)
	var prior *http.Cookie
	for _, cookie := range attacker.Jar.Cookies(appURL) {
		if cookie.Name == identity.SessionCookieName {
			copy := *cookie
			copy.Secure = true
			copy.HttpOnly = true
			copy.Path = "/"
			prior = &copy
		}
	}
	if prior == nil {
		t.Fatal("existing session absent")
	}
	newCookie := e.login(t, attacker)
	after := e.session(t, attacker)
	if initial.SessionID == after.SessionID || prior.Value == newCookie.Value {
		t.Fatal("login reused a pre-login session")
	}
	oldBrowser := e.browser(t)
	oldBrowser.Jar.SetCookies(appURL, []*http.Cookie{prior})
	response = e.request(t, oldBrowser, "GET", identity.SessionPath, "")
	expectLoginStatus(t, response, 401)
	loginBody(t, response)
}

func TestBrowserLoginStrictCallbackRejectsIssuerAndQueryConfusion(t *testing.T) {
	e := newLoginEnvironment(t, nil)
	for _, kind := range []string{"wrong_issuer", "missing_issuer", "duplicate_state", "duplicate_code", "extra_return_url", "both_code_and_error", "missing_state", "bad_escape"} {
		t.Run(kind, func(t *testing.T) {
			browser := e.browser(t)
			callback, _ := e.prepare(t, browser)
			u, _ := url.Parse(callback)
			q := u.Query()
			switch kind {
			case "wrong_issuer":
				q.Set("iss", "https://wrong.example.test")
			case "missing_issuer":
				q.Del("iss")
			case "duplicate_state":
				q.Add("state", q.Get("state"))
			case "duplicate_code":
				q.Add("code", q.Get("code"))
			case "extra_return_url":
				q.Set("return_url", "https://wrong.example.test")
			case "both_code_and_error":
				q.Set("error", "access_denied")
			case "missing_state":
				q.Del("state")
			}
			u.RawQuery = q.Encode()
			if kind == "bad_escape" {
				u.RawQuery += "&x=%ZZ"
			}
			before := e.issuer.tokenCalls.Load()
			response := e.finish(t, browser, u.String())
			expectLoginStatus(t, response, 400)
			loginBody(t, response)
			if e.issuer.tokenCalls.Load() != before {
				t.Fatal("invalid callback reached token endpoint")
			}
		})
	}
}

func TestBrowserSessionCSRFAndOriginChecks(t *testing.T) {
	e := newLoginEnvironment(t, nil)
	browser := e.browser(t)
	e.login(t, browser)
	session := e.session(t, browser)
	for _, kind := range []string{"missing_csrf", "wrong_csrf", "foreign_origin", "no_origin", "duplicate_origin", "duplicate_csrf"} {
		t.Run(kind, func(t *testing.T) {
			request, _ := http.NewRequest("POST", e.origin+identity.SessionRotatePath, nil)
			request.Header.Set("Origin", e.origin)
			request.Header.Set("X-CSRF-Token", session.CSRFToken)
			switch kind {
			case "missing_csrf":
				request.Header.Del("X-CSRF-Token")
			case "wrong_csrf":
				request.Header.Set("X-CSRF-Token", strings.Repeat("A", 43))
			case "foreign_origin":
				request.Header.Set("Origin", "https://attacker.example.test")
			case "no_origin":
				request.Header.Del("Origin")
			case "duplicate_origin":
				request.Header.Add("Origin", e.origin)
			case "duplicate_csrf":
				request.Header.Add("X-CSRF-Token", session.CSRFToken)
			}
			response, err := browser.Do(request)
			if err != nil {
				t.Fatal("local request failed")
			}
			expectLoginStatus(t, response, 403)
			loginBody(t, response)
			if e.session(t, browser).SessionID != session.SessionID {
				t.Fatal("CSRF attempt mutated session")
			}
		})
	}
	request, _ := http.NewRequest("GET", e.origin+identity.SessionPath, nil)
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	response, err := browser.Do(request)
	if err != nil {
		t.Fatal("local request failed")
	}
	expectLoginStatus(t, response, 403)
	loginBody(t, response)
	request, _ = http.NewRequest("POST", e.origin+identity.LoginStartPath, nil)
	request.Header.Set("Origin", "https://attacker.example.test")
	response, err = browser.Do(request)
	if err != nil {
		t.Fatal("local request failed")
	}
	expectLoginStatus(t, response, 403)
	loginBody(t, response)
}

func TestBrowserLoginDoesNotProvisionFromEmailOrGroups(t *testing.T) {
	e := newLoginEnvironment(t, nil)
	e.store.mu.Lock()
	delete(e.store.mappings, e.issuer.server.URL+"\x00stable-employee")
	e.store.mu.Unlock()
	browser := e.browser(t)
	callback, _ := e.prepare(t, browser)
	response := e.finish(t, browser, callback)
	expectLoginStatus(t, response, 403)
	body := string(loginBody(t, response))
	if strings.Contains(body, "changed@example.test") || strings.Contains(body, "owner") {
		t.Fatal("provider profile leaked into error")
	}
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	if len(e.store.sessions) != 0 {
		t.Fatal("unmapped email/group granted a session")
	}
}

func TestBrowserSessionRevocationMappingChangeAndExpiry(t *testing.T) {
	for _, kind := range []string{"removed", "remapped", "expired"} {
		t.Run(kind, func(t *testing.T) {
			e := newLoginEnvironment(t, nil)
			browser := e.browser(t)
			e.login(t, browser)
			view := e.session(t, browser)
			e.store.mu.Lock()
			switch kind {
			case "removed":
				delete(e.store.mappings, e.issuer.server.URL+"\x00stable-employee")
			case "remapped":
				e.store.mappings[e.issuer.server.URL+"\x00stable-employee"] = "different-principal"
			case "expired":
				for hash, session := range e.store.sessions {
					session.ExpiresAt = time.Now().Add(-time.Second)
					e.store.sessions[hash] = session
				}
			}
			e.store.mu.Unlock()
			response := e.request(t, browser, "GET", identity.SessionPath, "")
			expectLoginStatus(t, response, 401)
			loginBody(t, response)
			response = e.request(t, browser, "POST", identity.SessionRotatePath, view.CSRFToken)
			expectLoginStatus(t, response, 401)
			loginBody(t, response)
		})
	}
}

func TestBrowserLoginFailureConsumesStateAndRedactsProviderErrors(t *testing.T) {
	for _, kind := range []string{"provider_error", "bad_nonce", "bad_issuer", "missing_id_token", "oversize", "timeout", "body_timeout", "storage_commit"} {
		t.Run(kind, func(t *testing.T) {
			e := newLoginEnvironment(t, func(cfg *identity.LoginConfig, _ *loginIssuer) {
				if kind == "timeout" || kind == "body_timeout" {
					cfg.Provider.RequestTimeout = 100 * time.Millisecond
				}
			})
			browser := e.browser(t)
			callback, binding := e.prepare(t, browser)
			e.issuer.mu.Lock()
			switch kind {
			case "provider_error":
				e.issuer.tokenStatus = 503
			case "bad_nonce":
				e.issuer.tokenNonce = "wrong-nonce"
			case "bad_issuer":
				e.issuer.tokenIssuer = "https://other.example.test"
			case "missing_id_token":
				e.issuer.missingID = true
			case "oversize":
				e.issuer.oversize = true
			case "timeout":
				e.issuer.tokenDelay = time.Second
			case "body_timeout":
				e.issuer.tokenBodyDelay = time.Second
			}
			e.issuer.mu.Unlock()
			if kind == "storage_commit" {
				e.store.mu.Lock()
				e.store.createSessionFailure = true
				e.store.mu.Unlock()
			}
			started := time.Now()
			response := e.finish(t, browser, callback)
			want := 400
			if kind == "storage_commit" {
				want = 503
			}
			expectLoginStatus(t, response, want)
			body := string(loginBody(t, response))
			if strings.Contains(body, "PRIVATE_PROVIDER_ERROR") || strings.Contains(body, "fixture-access") || strings.Contains(body, "fixture-code-") {
				t.Fatal("sensitive upstream error leaked")
			}
			if (kind == "timeout" || kind == "body_timeout") && time.Since(started) > 750*time.Millisecond {
				t.Fatal("token exchange exceeded bounded timeout")
			}
			appURL, _ := url.Parse(e.origin)
			browser.Jar.SetCookies(appURL, []*http.Cookie{binding})
			response = e.finish(t, browser, callback)
			expectLoginStatus(t, response, 400)
			loginBody(t, response)
			if e.issuer.tokenCalls.Load() != 1 {
				t.Fatal("failed exchange or store commit was automatically retried")
			}
			e.store.mu.Lock()
			if len(e.store.sessions) != 0 {
				t.Fatal("failed login created a session")
			}
			e.store.mu.Unlock()
		})
	}
}

func TestBrowserLoginTokenRedirectNeverForwardsCredentials(t *testing.T) {
	var received atomic.Int64
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1); w.WriteHeader(500) }))
	defer destination.Close()
	e := newLoginEnvironment(t, func(cfg *identity.LoginConfig, f *loginIssuer) {
		cfg.TokenAuthMethod = "client_secret_basic"
		cfg.ClientSecret = "private-fixture-secret"
		f.authMethod = cfg.TokenAuthMethod
		f.secret = cfg.ClientSecret
	})
	browser := e.browser(t)
	callback, _ := e.prepare(t, browser)
	e.issuer.mu.Lock()
	e.issuer.tokenRedirect = destination.URL
	e.issuer.mu.Unlock()
	response := e.finish(t, browser, callback)
	expectLoginStatus(t, response, 400)
	loginBody(t, response)
	if received.Load() != 0 || e.issuer.tokenCalls.Load() != 1 {
		t.Fatal("token redirect followed or auth automatically retried")
	}
}

func TestBrowserLoginStoreUnavailableAndExpiredTransactionsFailClosed(t *testing.T) {
	e := newLoginEnvironment(t, nil)
	browser := e.browser(t)
	e.store.mu.Lock()
	e.store.unavailable = true
	e.store.mu.Unlock()
	response := e.request(t, browser, "POST", identity.LoginStartPath, "")
	expectLoginStatus(t, response, 503)
	if len(response.Cookies()) != 0 {
		t.Fatal("failed durable write emitted login cookie")
	}
	loginBody(t, response)
	e.store.mu.Lock()
	e.store.unavailable = false
	e.store.mu.Unlock()
	callback, _ := e.prepare(t, browser)
	e.store.mu.Lock()
	for hash, tx := range e.store.transactions {
		tx.ExpiresAt = time.Now().Add(-time.Second)
		e.store.transactions[hash] = tx
	}
	e.store.mu.Unlock()
	response = e.finish(t, browser, callback)
	expectLoginStatus(t, response, 400)
	loginBody(t, response)
	if e.issuer.tokenCalls.Load() != 0 {
		t.Fatal("expired transaction reached token endpoint")
	}
}

func TestBrowserLoginRejectsUntrustedConfigAndRequestRouting(t *testing.T) {
	e := newLoginEnvironment(t, nil)
	for _, change := range []func(*identity.LoginConfig){
		func(c *identity.LoginConfig) {
			c.PublicOrigin = "http://127.0.0.1"
			c.RedirectURI = c.PublicOrigin + identity.LoginCallbackPath
		},
		func(c *identity.LoginConfig) { c.RedirectURI = "https://attacker.example.test/callback" },
		func(c *identity.LoginConfig) { c.CSRFKey = []byte("short") },
		func(c *identity.LoginConfig) { c.TokenAuthMethod = "auto" },
		func(c *identity.LoginConfig) { c.TransactionTTL = time.Hour },
		func(c *identity.LoginConfig) { c.SessionTTL = 48 * time.Hour },
	} {
		cfg := e.cfg
		change(&cfg)
		service, err := identity.NewLoginService(context.Background(), cfg, e.store)
		if err == nil {
			service.Close()
			t.Fatal("invalid configuration accepted")
		}
	}
	browser := e.browser(t)
	response := e.request(t, browser, "GET", identity.LoginStartPath, "")
	expectLoginStatus(t, response, 405)
	loginBody(t, response)
	response = e.request(t, browser, "POST", identity.LoginStartPath+"?return_url=https://attacker.example.test", "")
	expectLoginStatus(t, response, 400)
	loginBody(t, response)
	request, _ := http.NewRequest("POST", e.origin+identity.LoginStartPath, nil)
	request.Host = "attacker.example.test"
	request.Header.Set("Origin", e.origin)
	request.Header.Set("X-Forwarded-Host", e.cfg.PublicOrigin)
	response, err := browser.Do(request)
	if err != nil {
		t.Fatal("local request failed")
	}
	expectLoginStatus(t, response, 400)
	loginBody(t, response)
	e.service.Close()
	response = e.request(t, browser, "POST", identity.LoginStartPath, "")
	expectLoginStatus(t, response, 503)
	loginBody(t, response)
}

func TestBrowserSessionResponseDoesNotExposeCredentialsOrProviderProfile(t *testing.T) {
	e := newLoginEnvironment(t, nil)
	browser := e.browser(t)
	cookie := e.login(t, browser)
	response := e.request(t, browser, "GET", identity.SessionPath, "")
	expectLoginStatus(t, response, 200)
	body := loginBody(t, response)
	if strings.Contains(string(body), cookie.Value) || strings.Contains(string(body), "changed@example.test") || strings.Contains(string(body), "owner") || strings.Contains(string(body), "fixture-access") {
		t.Fatal("session response leaked credential or provider assertions")
	}
	var envelope map[string]map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil || len(envelope["data"]) != 4 {
		t.Fatal("unexpected session response contract")
	}
}

func TestBrowserLoginAuthorizationDenialConsumesState(t *testing.T) {
	e := newLoginEnvironment(t, nil)
	browser := e.browser(t)
	callback, binding := e.prepare(t, browser)
	u, _ := url.Parse(callback)
	q := u.Query()
	q.Del("code")
	q.Set("error", "access_denied")
	q.Set("error_description", "PRIVATE_PROVIDER_DESCRIPTION")
	q.Set("error_uri", "https://provider.example.test/private-error")
	u.RawQuery = q.Encode()
	response := e.finish(t, browser, u.String())
	expectLoginStatus(t, response, 400)
	if strings.Contains(string(loginBody(t, response)), "PRIVATE_PROVIDER_DESCRIPTION") {
		t.Fatal("provider authorization error leaked")
	}
	appURL, _ := url.Parse(e.origin)
	browser.Jar.SetCookies(appURL, []*http.Cookie{binding})
	response = e.finish(t, browser, callback)
	expectLoginStatus(t, response, 400)
	loginBody(t, response)
	if e.issuer.tokenCalls.Load() != 0 {
		t.Fatal("denied/replayed authorization reached exchange")
	}
}

func TestBrowserLoginPKCERejectsFreshCodeFromAnotherTransaction(t *testing.T) {
	e := newLoginEnvironment(t, nil)
	first, second := e.browser(t), e.browser(t)
	firstCallback, _ := e.prepare(t, first)
	secondCallback, _ := e.prepare(t, second)
	a, _ := url.Parse(firstCallback)
	b, _ := url.Parse(secondCallback)
	q := a.Query()
	q.Set("code", b.Query().Get("code"))
	a.RawQuery = q.Encode()
	response := e.finish(t, first, a.String())
	expectLoginStatus(t, response, 400)
	loginBody(t, response)
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	if len(e.store.sessions) != 0 || e.issuer.tokenCalls.Load() != 1 {
		t.Fatal("cross-transaction fresh code bypassed PKCE")
	}
}

func TestBrowserLoginRejectsDuplicateBindingAndSessionCookies(t *testing.T) {
	e := newLoginEnvironment(t, nil)
	browser := e.browser(t)
	callback, binding := e.prepare(t, browser)
	for _, malformed := range []string{`="unterminated`, `=`, ``, ` =invalid`, `=not-base64`} {
		request, _ := http.NewRequest("GET", callback, nil)
		request.Header.Add("Cookie", identity.LoginCookieName+malformed)
		response, err := browser.Do(request)
		if err != nil {
			t.Fatal("local malformed-cookie request failed")
		}
		expectLoginStatus(t, response, 400)
		loginBody(t, response)
	}
	request, _ := http.NewRequest("GET", callback, nil)
	request.AddCookie(binding) // The browser jar adds the same cookie a second time.
	response, err := browser.Do(request)
	if err != nil {
		t.Fatal("local callback failed")
	}
	expectLoginStatus(t, response, 400)
	loginBody(t, response)
	if e.issuer.tokenCalls.Load() != 0 {
		t.Fatal("ambiguous binding reached token exchange")
	}
	response = e.finish(t, browser, callback)
	expectLoginStatus(t, response, 303)
	var session *http.Cookie
	for _, cookie := range response.Cookies() {
		if cookie.Name == identity.SessionCookieName {
			session = cookie
		}
	}
	loginBody(t, response)
	if session == nil {
		t.Fatal("session absent")
	}
	request, _ = http.NewRequest("GET", e.origin+identity.SessionPath, nil)
	request.AddCookie(session)
	response, err = browser.Do(request)
	if err != nil {
		t.Fatal("local session request failed")
	}
	expectLoginStatus(t, response, 401)
	loginBody(t, response)
}

func TestBrowserLoginServiceReplacementUsesSharedStore(t *testing.T) {
	e := newLoginEnvironment(t, nil)
	browser := e.browser(t)
	callback, _ := e.prepare(t, browser)
	// The service itself must not keep transactions/sessions in local memory.
	// Actual crash durability is tested separately by the PostgreSQL bridge.
	replacement, err := identity.NewLoginService(context.Background(), e.cfg, e.store)
	if err != nil {
		t.Fatal("replacement service initialization failed")
	}
	t.Cleanup(replacement.Close)
	e.current.Store(replacement)
	e.service.Close()
	response := e.finish(t, browser, callback)
	expectLoginStatus(t, response, 303)
	loginBody(t, response)
	view := e.session(t, browser)
	second, err := identity.NewLoginService(context.Background(), e.cfg, e.store)
	if err != nil {
		t.Fatal("second replacement initialization failed")
	}
	t.Cleanup(second.Close)
	e.current.Store(second)
	replacement.Close()
	if e.session(t, browser) != view {
		t.Fatal("recreated service changed the existing session")
	}
}

func TestBrowserLoginEndpointOriginsRequireExplicitAdministratorTrust(t *testing.T) {
	e := newLoginEnvironment(t, nil)
	for _, field := range []string{"authorize", "token"} {
		t.Run(field, func(t *testing.T) {
			e.issuer.mu.Lock()
			if field == "authorize" {
				e.issuer.authEndpoint = "https://separate.example.test/authorize"
			} else {
				e.issuer.tokenEndpoint = "https://separate.example.test/token"
			}
			e.issuer.mu.Unlock()
			cfg := e.cfg
			if service, err := identity.NewLoginService(context.Background(), cfg, e.store); err == nil {
				service.Close()
				t.Fatal("cross-origin discovery endpoint accepted without administrator trust")
			}
			cfg.AdditionalEndpointOrigins = []string{"https://separate.example.test"}
			service, err := identity.NewLoginService(context.Background(), cfg, e.store)
			if err != nil {
				t.Fatal("explicitly trusted HTTPS endpoint rejected")
			}
			service.Close()
			e.issuer.mu.Lock()
			e.issuer.authEndpoint, e.issuer.tokenEndpoint = "", ""
			e.issuer.mu.Unlock()
		})
	}
}
