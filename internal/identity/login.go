// SPDX-License-Identifier: Elastic-2.0
package identity

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"
)

const (
	LoginStartPath    = "/auth/oidc/start"
	LoginCallbackPath = "/auth/oidc/callback"
	SessionPath       = "/auth/session"
	SessionRotatePath = "/auth/session/rotate"
	LogoutPath        = "/auth/logout"
	SessionCookieName = "__Host-agent-platform-session"
	LoginCookieName   = "__Host-agent-platform-login"
)

// LoginConfig is trusted administrator configuration, never request input.
// PublicOrigin requires HTTPS even when the provider loopback-test flag is set.
// CSRFKey must be a random deployment secret shared by all API replicas.
type LoginConfig struct {
	Provider                   Config
	PublicOrigin, RedirectURI  string
	ClientSecret               string
	TokenAuthMethod            string // "none" or "client_secret_basic", explicit
	CSRFKey                    []byte
	AdditionalEndpointOrigins  []string
	TransactionTTL, SessionTTL time.Duration
}

// LoginService owns browser-flow security; the store owns durable authorization
// mapping and session lifecycle. It does not provision principals or memberships.
type LoginService struct {
	store                    LoginStore
	verifier                 *Verifier
	oauth                    oauth2.Config
	tokenHTTP                *http.Client
	tokenBase                *http.Transport
	publicOrigin, publicHost string
	csrfKey                  []byte
	txTTL, sessionTTL        time.Duration
	closed                   atomic.Bool
}

func NewLoginService(ctx context.Context, cfg LoginConfig, store LoginStore) (*LoginService, error) {
	if ctx == nil || store == nil || len(cfg.CSRFKey) < 32 || len(cfg.CSRFKey) > 128 {
		return nil, ErrInvalidConfig
	}
	public, ok := checkedURL(cfg.PublicOrigin, false)
	if !ok || public.Scheme != "https" || public.Path != "" || public.RawQuery != "" || public.ForceQuery ||
		cfg.RedirectURI != cfg.PublicOrigin+LoginCallbackPath {
		return nil, ErrInvalidConfig
	}
	if cfg.TransactionTTL == 0 {
		cfg.TransactionTTL = 5 * time.Minute
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 8 * time.Hour
	}
	if cfg.TransactionTTL < time.Minute || cfg.TransactionTTL > 10*time.Minute ||
		cfg.SessionTTL < 5*time.Minute || cfg.SessionTTL > 24*time.Hour {
		return nil, ErrInvalidConfig
	}
	var authStyle oauth2.AuthStyle
	switch cfg.TokenAuthMethod {
	case "none":
		if cfg.ClientSecret != "" {
			return nil, ErrInvalidConfig
		}
		authStyle = oauth2.AuthStyleInParams
	case "client_secret_basic":
		if !validText(cfg.ClientSecret, 8192, true) {
			return nil, ErrInvalidConfig
		}
		authStyle = oauth2.AuthStyleInHeader
	default:
		return nil, ErrInvalidConfig
	}
	v, err := NewVerifier(ctx, cfg.Provider)
	if err != nil {
		return nil, err
	}
	fail := func() (*LoginService, error) { v.Close(); return nil, ErrInvalidConfig }
	var metadata map[string]json.RawMessage
	if v.provider.Claims(&metadata) != nil {
		return fail()
	}
	var authURL, tokenURL string
	if json.Unmarshal(metadata["authorization_endpoint"], &authURL) != nil ||
		json.Unmarshal(metadata["token_endpoint"], &tokenURL) != nil {
		return fail()
	}
	policy := endpointPolicy{origins: make(map[string]struct{}), allowTestHTTP: cfg.Provider.AllowLoopbackHTTPForTests}
	issuer, _ := checkedURL(cfg.Provider.IssuerURL, cfg.Provider.AllowLoopbackHTTPForTests)
	policy.origins[origin(issuer)] = struct{}{}
	if len(cfg.AdditionalEndpointOrigins) > 8 {
		return fail()
	}
	for _, configured := range cfg.AdditionalEndpointOrigins {
		u, valid := checkedURL(configured, policy.allowTestHTTP)
		if !valid || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery {
			return fail()
		}
		policy.origins[origin(u)] = struct{}{}
	}
	for _, endpoint := range []string{authURL, tokenURL} {
		u, valid := checkedURL(endpoint, policy.allowTestHTTP)
		if !valid || !policy.allows(endpoint) || u.RawQuery != "" || u.ForceQuery {
			return fail()
		}
	}
	if raw, present := metadata["code_challenge_methods_supported"]; present {
		var methods []string
		if json.Unmarshal(raw, &methods) != nil || !containsString(methods, "S256") {
			return fail()
		}
	}
	if raw, present := metadata["token_endpoint_auth_methods_supported"]; present {
		var methods []string
		if json.Unmarshal(raw, &methods) != nil || !containsString(methods, cfg.TokenAuthMethod) {
			return fail()
		}
	}
	base := v.transport.Clone()
	client := &http.Client{
		Transport:     &loginTokenTransport{base: base, endpoint: tokenURL},
		Timeout:       v.timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return ErrLoginRejected },
	}
	return &LoginService{
		store: store, verifier: v, tokenHTTP: client, tokenBase: base,
		oauth: oauth2.Config{ClientID: cfg.Provider.ClientID, ClientSecret: cfg.ClientSecret,
			RedirectURL: cfg.RedirectURI, Scopes: []string{"openid", "profile", "email"},
			Endpoint: oauth2.Endpoint{AuthURL: authURL, TokenURL: tokenURL, AuthStyle: authStyle}},
		publicOrigin: cfg.PublicOrigin, publicHost: public.Host, csrfKey: append([]byte(nil), cfg.CSRFKey...),
		txTTL: cfg.TransactionTTL, sessionTTL: cfg.SessionTTL,
	}, nil
}

func (s *LoginService) Close() {
	if s.closed.CompareAndSwap(false, true) {
		s.verifier.Close()
		s.tokenBase.CloseIdleConnections()
	}
}

// TrustedIssuer reports only the issuer of this successfully initialized
// verifier. It is a runtime readiness fact, never a caller-selected URL.
func (s *LoginService) TrustedIssuer() string {
	if s == nil || s.closed.Load() || s.verifier == nil {
		return ""
	}
	return s.verifier.issuer
}

// AuthorizationOrigin is the administrator-validated form redirect destination
// for the embedding application's Content-Security-Policy, never user input.
func (s *LoginService) AuthorizationOrigin() string {
	u, _ := url.Parse(s.oauth.Endpoint.AuthURL)
	return u.Scheme + "://" + u.Host
}

// Handler exposes only the five fixed login/session routes. Applications may
// alternatively register the exported Handle* methods on their existing router.
func (s *LoginService) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case LoginStartPath:
			s.HandleStart(w, r)
		case LoginCallbackPath:
			s.HandleCallback(w, r)
		case SessionPath:
			s.HandleSession(w, r)
		case SessionRotatePath:
			s.HandleRotate(w, r)
		case LogoutPath:
			s.HandleLogout(w, r)
		default:
			loginError(w, http.StatusNotFound, "not_found")
		}
	})
}

func (s *LoginService) HandleStart(w http.ResponseWriter, r *http.Request) {
	if !s.checkRequest(w, r, http.MethodPost, LoginStartPath, false) {
		return
	}
	if !s.sameOrigin(r) {
		loginError(w, http.StatusForbidden, "forbidden")
		return
	}
	state, e1 := loginSecret()
	binding, e2 := loginSecret()
	nonce, e3 := loginSecret()
	verifier, e4 := loginSecret()
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		loginError(w, 500, "internal_error")
		return
	}
	now := time.Now().UTC()
	tx := LoginTransaction{StateHash: loginHash(state), BindingHash: loginHash(binding),
		Issuer: s.verifier.issuer, ClientID: s.oauth.ClientID, RedirectURI: s.oauth.RedirectURL,
		Nonce: nonce, CodeVerifier: verifier, CreatedAt: now, ExpiresAt: now.Add(s.txTTL)}
	if err := s.store.CreateLogin(r.Context(), tx); err != nil {
		loginError(w, 503, "unavailable")
		return
	}
	s.setCookie(w, LoginCookieName, binding, tx.ExpiresAt)
	loginRedirect(w, s.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("nonce", nonce)))
}

func (s *LoginService) HandleCallback(w http.ResponseWriter, r *http.Request) {
	if !s.checkRequest(w, r, http.MethodGet, LoginCallbackPath, true) {
		return
	}
	query, err := strictLoginQuery(r.URL.RawQuery)
	if err != nil || query.Get("iss") != s.verifier.issuer {
		loginError(w, 400, "login_rejected")
		return
	}
	state := query.Get("state")
	binding, err := loginCookie(r, LoginCookieName)
	if !validLoginSecret(state) || err != nil {
		loginError(w, 400, "login_rejected")
		return
	}
	tx, err := s.store.ConsumeLogin(r.Context(), loginHash(state), loginHash(binding))
	if err != nil {
		s.storeError(w, err, 400, "login_rejected")
		return
	}
	s.clearCookie(w, LoginCookieName)
	now := time.Now().UTC()
	if tx.StateHash != loginHash(state) || tx.BindingHash != loginHash(binding) ||
		tx.Issuer != s.verifier.issuer || tx.ClientID != s.oauth.ClientID || tx.RedirectURI != s.oauth.RedirectURL ||
		!now.Before(tx.ExpiresAt) || tx.CreatedAt.After(now.Add(time.Minute)) ||
		!validLoginSecret(tx.Nonce) || !validLoginSecret(tx.CodeVerifier) {
		loginError(w, 400, "login_rejected")
		return
	}
	if query.Get("error") != "" {
		loginError(w, 400, "login_rejected")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.verifier.timeout)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, s.tokenHTTP)
	token, err := s.oauth.Exchange(ctx, query.Get("code"), oauth2.VerifierOption(tx.CodeVerifier))
	if err != nil {
		loginError(w, 400, "login_rejected")
		return
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		loginError(w, 400, "login_rejected")
		return
	}
	claims, err := s.verifier.Verify(r.Context(), rawID, tx.Nonce)
	if err != nil {
		loginError(w, 400, "login_rejected")
		return
	}
	principalID, err := s.store.ResolveIdentity(r.Context(), claims.Issuer, claims.Subject)
	if err != nil {
		s.storeError(w, err, 403, "identity_unmapped")
		return
	}
	if !validText(principalID, 128, true) {
		loginError(w, 503, "unavailable")
		return
	}
	rawSession, err := loginSecret()
	id, idErr := loginSecret()
	if err != nil || idErr != nil {
		loginError(w, 500, "internal_error")
		return
	}
	now = time.Now().UTC()
	session := BrowserSession{ID: "ses_" + id, TokenHash: loginHash(rawSession), PrincipalID: principalID,
		Issuer: claims.Issuer, Subject: claims.Subject, AuthenticatedAt: now, CreatedAt: now, ExpiresAt: now.Add(s.sessionTTL)}
	previous, cookieErr := optionalLoginCookie(r, SessionCookieName)
	if cookieErr != nil {
		loginError(w, 400, "login_rejected")
		return
	}
	previousHash := ""
	if previous != "" {
		previousHash = loginHash(previous)
	}
	if err := s.store.CreateSession(r.Context(), session, previousHash); err != nil {
		s.storeError(w, err, 403, "identity_unmapped")
		return
	}
	s.setCookie(w, SessionCookieName, rawSession, session.ExpiresAt)
	loginRedirect(w, s.publicOrigin+"/")
}

// SessionFromRequest authenticates the browser cookie only. Organization roles
// remain the API/storage bridge's responsibility and must be checked per request.
func (s *LoginService) SessionFromRequest(ctx context.Context, r *http.Request) (BrowserSession, error) {
	if s.closed.Load() || r.Host != s.publicHost || ((r.Method == http.MethodGet || r.Method == http.MethodHead) && !s.safeReadOrigin(r)) {
		return BrowserSession{}, ErrSessionInvalid
	}
	raw, err := loginCookie(r, SessionCookieName)
	if err != nil {
		return BrowserSession{}, ErrSessionInvalid
	}
	session, err := s.store.GetSession(ctx, loginHash(raw))
	if err != nil {
		if errors.Is(err, ErrSessionInvalid) {
			return BrowserSession{}, ErrSessionInvalid
		}
		return BrowserSession{}, ErrLoginStoreUnavailable
	}
	if session.TokenHash != loginHash(raw) || session.Issuer != s.verifier.issuer ||
		!validText(session.ID, 128, true) || !validText(session.PrincipalID, 128, true) || !validText(session.Subject, 255, true) ||
		!time.Now().Before(session.ExpiresAt) {
		return BrowserSession{}, ErrSessionInvalid
	}
	return session, nil
}

// CheckCSRF validates request provenance and the session-bound token; it does
// not authenticate an active session. Use with SessionFromRequest for mutations.
func (s *LoginService) CheckCSRF(r *http.Request) error {
	if s.closed.Load() || !s.sameOrigin(r) || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return ErrSessionInvalid
	}
	raw, err := loginCookie(r, SessionCookieName)
	values := r.Header.Values("X-CSRF-Token")
	if err != nil || len(values) != 1 || !validLoginSecret(values[0]) {
		return ErrSessionInvalid
	}
	expected := s.csrfToken(raw)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(values[0])) != 1 {
		return ErrSessionInvalid
	}
	return nil
}

func (s *LoginService) HandleSession(w http.ResponseWriter, r *http.Request) {
	if !s.checkRequest(w, r, http.MethodGet, SessionPath, false) {
		return
	}
	if !s.safeReadOrigin(r) {
		loginError(w, 403, "forbidden")
		return
	}
	session, err := s.SessionFromRequest(r.Context(), r)
	if err != nil {
		s.storeError(w, err, 401, "unauthenticated")
		return
	}
	raw, _ := loginCookie(r, SessionCookieName)
	s.sessionJSON(w, session, raw)
}

func (s *LoginService) HandleRotate(w http.ResponseWriter, r *http.Request) {
	if !s.checkRequest(w, r, http.MethodPost, SessionRotatePath, false) {
		return
	}
	if s.CheckCSRF(r) != nil {
		loginError(w, 403, "forbidden")
		return
	}
	previous, err := s.SessionFromRequest(r.Context(), r)
	if err != nil {
		s.storeError(w, err, 401, "unauthenticated")
		return
	}
	raw, err := loginSecret()
	id, idErr := loginSecret()
	if err != nil || idErr != nil {
		loginError(w, 500, "internal_error")
		return
	}
	replacement := previous
	replacement.ID, replacement.TokenHash, replacement.CreatedAt = "ses_"+id, loginHash(raw), time.Now().UTC()
	if err := s.store.RotateSession(r.Context(), previous.TokenHash, replacement); err != nil {
		s.storeError(w, err, 401, "unauthenticated")
		return
	}
	s.setCookie(w, SessionCookieName, raw, replacement.ExpiresAt)
	s.sessionJSON(w, replacement, raw)
}

func (s *LoginService) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.checkRequest(w, r, http.MethodPost, LogoutPath, false) {
		return
	}
	if s.CheckCSRF(r) != nil {
		loginError(w, 403, "forbidden")
		return
	}
	raw, err := loginCookie(r, SessionCookieName)
	if err != nil {
		loginError(w, 401, "unauthenticated")
		return
	}
	if err := s.store.RevokeSession(r.Context(), loginHash(raw)); err != nil {
		loginError(w, 503, "unavailable")
		return
	}
	s.clearCookie(w, SessionCookieName)
	s.clearCookie(w, LoginCookieName)
	w.WriteHeader(http.StatusNoContent)
}

func loginSecret() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func validLoginSecret(raw string) bool {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	return len(raw) == 43 && err == nil && len(decoded) == 32
}

func loginHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (s *LoginService) csrfToken(raw string) string {
	hash := hmac.New(sha256.New, s.csrfKey)
	_, _ = hash.Write([]byte("agent-platform-csrf-v1\x00" + raw))
	return base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func strictLoginQuery(raw string) (url.Values, error) {
	if len(raw) == 0 || len(raw) > 8192 {
		return nil, ErrLoginRejected
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, ErrLoginRejected
	}
	for key, all := range values {
		if len(all) != 1 || !validText(all[0], 4096, true) {
			return nil, ErrLoginRejected
		}
		switch key {
		case "state", "code", "iss", "error", "error_description", "error_uri":
		default:
			return nil, ErrLoginRejected
		}
	}
	code, denied := values.Get("code"), values.Get("error")
	if values.Get("state") == "" || values.Get("iss") == "" || (code == "") == (denied == "") {
		return nil, ErrLoginRejected
	}
	if denied == "" && (values.Has("error_description") || values.Has("error_uri")) {
		return nil, ErrLoginRejected
	}
	return values, nil
}

func loginCookie(r *http.Request, name string) (string, error) {
	raw, err := optionalLoginCookie(r, name)
	if err != nil || raw == "" {
		return "", ErrSessionInvalid
	}
	return raw, nil
}
func optionalLoginCookie(r *http.Request, name string) (string, error) {
	value := ""
	seen := false
	// net/http intentionally skips malformed cookie pairs. Count target names
	// in the raw headers first so an unterminated quoted duplicate cannot vanish
	// and make an ambiguous credential appear to be a single valid cookie.
	for _, header := range r.Header.Values("Cookie") {
		for _, part := range strings.Split(header, ";") {
			part = strings.Trim(part, " \t")
			key, candidate, hasValue := strings.Cut(part, "=")
			if strings.TrimSpace(key) != name {
				continue
			}
			if seen || key != name || !hasValue || !validLoginSecret(candidate) {
				return "", ErrSessionInvalid
			}
			seen = true
			value = candidate
		}
	}
	return value, nil
}

func (s *LoginService) sameOrigin(r *http.Request) bool {
	values := r.Header.Values("Origin")
	return r.Host == s.publicHost && len(values) == 1 && values[0] == s.publicOrigin
}
func (s *LoginService) safeReadOrigin(r *http.Request) bool {
	if len(r.Header.Values("Origin")) > 0 && !s.sameOrigin(r) {
		return false
	}
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin" || site == "none"
}

func (s *LoginService) checkRequest(w http.ResponseWriter, r *http.Request, method, path string, query bool) bool {
	loginHeaders(w)
	if s.closed.Load() {
		loginError(w, 503, "unavailable")
		return false
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		loginError(w, 405, "method_not_allowed")
		return false
	}
	if r.Host != s.publicHost || r.URL.Path != path || r.URL.RawPath != "" || r.URL.Fragment != "" ||
		(!query && (r.URL.RawQuery != "" || r.URL.ForceQuery)) || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		loginError(w, 400, "invalid_request")
		return false
	}
	return true
}

func (s *LoginService) setCookie(w http.ResponseWriter, name, value string, expiry time.Time) {
	seconds := int(time.Until(expiry).Seconds())
	if seconds < 1 {
		seconds = 1
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expiry, MaxAge: seconds})
}
func (s *LoginService) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: time.Unix(1, 0), MaxAge: -1})
}
func (s *LoginService) sessionJSON(w http.ResponseWriter, session BrowserSession, raw string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
		"session_id": session.ID, "principal_id": session.PrincipalID, "expires_at": session.ExpiresAt, "csrf_token": s.csrfToken(raw),
	}})
}
func (s *LoginService) storeError(w http.ResponseWriter, err error, status int, code string) {
	if !errors.Is(err, ErrLoginRejected) && !errors.Is(err, ErrIdentityUnmapped) && !errors.Is(err, ErrSessionInvalid) {
		loginError(w, 503, "unavailable")
		return
	}
	loginError(w, status, code)
}
func loginHeaders(w http.ResponseWriter) {
	if w.Header().Get("X-Request-ID") == "" {
		if id, err := loginSecret(); err == nil {
			w.Header().Set("X-Request-ID", "req_"+id)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}
func loginError(w http.ResponseWriter, status int, code string) {
	loginHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": "Authentication request could not be completed", "request_id": w.Header().Get("X-Request-ID"), "retryable": status == 503}})
}
func loginRedirect(w http.ResponseWriter, location string) {
	loginHeaders(w)
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusSeeOther)
}
