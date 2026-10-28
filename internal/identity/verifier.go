// SPDX-License-Identifier: Elastic-2.0

// Package identity verifies OIDC ID tokens against administrator-controlled
// provider configuration. It does not create sessions, principals or permissions.
package identity

import (
	"context"
	"crypto/subtle"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
)

var (
	ErrInvalidConfig       = errors.New("oidc: invalid administrator configuration")
	ErrProviderUnavailable = errors.New("oidc: provider unavailable or metadata rejected")
	ErrInvalidToken        = errors.New("oidc: ID token rejected")
	ErrClosed              = errors.New("oidc: verifier closed")
)

// Config is a trusted deployment configuration, never an HTTP-header contract.
// Construct one verifier per configured identity provider/client, not per token.
type Config struct {
	IssuerURL                 string
	ClientID                  string
	AllowedSigningAlgorithms  []string
	AdditionalJWKSOrigins     []string
	RequestTimeout            time.Duration
	KeyCacheTTL               time.Duration
	RootCAs                   *x509.CertPool
	AllowLoopbackHTTPForTests bool
}

// Claims contains signature-validated profile assertions. Email and groups are
// not platform identities or roles. Account linking must use (Issuer, Subject)
// and an independently authorized organization-membership mapping.
type Claims struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	Groups        []string
	Audience      []string
	IssuedAt      time.Time
	ExpiresAt     time.Time
}

// Verifier is safe for concurrent use. It never retains the supplied ID token.
type Verifier struct {
	issuer     string
	clientID   string
	provider   *oidc.Provider
	client     *http.Client
	transport  *http.Transport
	algorithms []string
	timeout    time.Duration
	cacheTTL   time.Duration
	mu         sync.Mutex
	validators map[string]*oidc.IDTokenVerifier
	cacheUntil time.Time
	closed     bool
}

// NewVerifier discovers metadata using a restricted client. It does not perform
// an authorization-code exchange or trust any request-provided identity header.
func NewVerifier(ctx context.Context, cfg Config) (*Verifier, error) {
	if ctx == nil || !validText(cfg.ClientID, 512, true) || cfg.ClientID != strings.TrimSpace(cfg.ClientID) {
		return nil, ErrInvalidConfig
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 5 * time.Second
	}
	if cfg.RequestTimeout < 100*time.Millisecond || cfg.RequestTimeout > 30*time.Second {
		return nil, ErrInvalidConfig
	}
	if cfg.KeyCacheTTL == 0 {
		cfg.KeyCacheTTL = 5 * time.Minute
	}
	if cfg.KeyCacheTTL < 100*time.Millisecond || cfg.KeyCacheTTL > time.Hour {
		return nil, ErrInvalidConfig
	}
	algs := append([]string(nil), cfg.AllowedSigningAlgorithms...)
	if len(algs) == 0 {
		algs = []string{oidc.RS256}
	}
	if len(algs) > 10 {
		return nil, ErrInvalidConfig
	}
	uniqueAlgs := make(map[string]struct{}, len(algs))
	for _, alg := range algs {
		switch alg {
		case oidc.RS256, oidc.RS384, oidc.RS512, oidc.PS256, oidc.PS384, oidc.PS512,
			oidc.ES256, oidc.ES384, oidc.ES512, oidc.EdDSA:
		default:
			return nil, ErrInvalidConfig
		}
		uniqueAlgs[alg] = struct{}{}
	}
	algs = algs[:0]
	for alg := range uniqueAlgs {
		algs = append(algs, alg)
	}
	client, transport, policy, err := newProviderClient(cfg)
	if err != nil {
		return nil, err
	}
	// Do not inherit opaque context values which can alter go-oidc discovery.
	discoveryCtx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	defer cancel()
	if ctx.Err() != nil {
		transport.CloseIdleConnections()
		return nil, ctx.Err()
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(discoveryCtx, client), cfg.IssuerURL)
	if err != nil {
		transport.CloseIdleConnections()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrProviderUnavailable
	}
	var metadata struct {
		Issuer  string `json:"issuer"`
		JWKSURL string `json:"jwks_uri"`
	}
	if provider.Claims(&metadata) != nil || metadata.Issuer != cfg.IssuerURL || !policy.allows(metadata.JWKSURL) {
		transport.CloseIdleConnections()
		return nil, ErrProviderUnavailable
	}
	return &Verifier{
		issuer: cfg.IssuerURL, clientID: cfg.ClientID, provider: provider,
		client: client, transport: transport, algorithms: algs,
		timeout: cfg.RequestTimeout, cacheTTL: cfg.KeyCacheTTL,
	}, nil
}

// Verify requires the nonce from a trusted, expiring login transaction. It must
// not be taken from this token, a request header, or an unbound callback field.
// The future login-flow owner must atomically consume that transaction once.
func (v *Verifier) Verify(ctx context.Context, rawIDToken, expectedNonce string) (*Claims, error) {
	if ctx == nil || len(rawIDToken) == 0 || len(rawIDToken) > 64*1024 || !validText(expectedNonce, 512, true) {
		return nil, ErrInvalidToken
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// This untrusted header only selects a configured algorithm-specific cache.
	// Signature and claim verification below remain the libraries' responsibility.
	allowed := make([]jose.SignatureAlgorithm, len(v.algorithms))
	for i, alg := range v.algorithms {
		allowed[i] = jose.SignatureAlgorithm(alg)
	}
	signed, err := jose.ParseSignedCompact(rawIDToken, allowed)
	if err != nil || len(signed.Signatures) != 1 {
		return nil, ErrInvalidToken
	}
	validator, err := v.currentValidator(signed.Signatures[0].Header.Algorithm)
	if err != nil {
		return nil, err
	}
	checkCtx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	token, err := validator.Verify(checkCtx, rawIDToken)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var networkError net.Error
		if checkCtx.Err() != nil || errors.As(err, &networkError) {
			return nil, ErrProviderUnavailable
		}
		// Upstream errors may include claims or endpoint response bodies.
		return nil, ErrInvalidToken
	}
	if checkCtx.Err() != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrProviderUnavailable
	}
	var present map[string]json.RawMessage
	if token.Claims(&present) != nil || !canonicalClaimNames(present) {
		return nil, ErrInvalidToken
	}
	// Exact matching also removes go-oidc's historical Google issuer exception.
	if token.Issuer != v.issuer || !validText(token.Subject, 255, true) ||
		!validText(token.Nonce, 512, true) || subtle.ConstantTimeCompare([]byte(token.Nonce), []byte(expectedNonce)) != 1 {
		return nil, ErrInvalidToken
	}
	var profile struct {
		Email         string   `json:"email"`
		EmailVerified bool     `json:"email_verified"`
		Groups        []string `json:"groups"`
		AuthorizedFor string   `json:"azp"`
		IssuedAt      *int64   `json:"iat"`
		ExpiresAt     *int64   `json:"exp"`
		NotBefore     *int64   `json:"nbf"`
	}
	if token.Claims(&profile) != nil || profile.IssuedAt == nil || profile.ExpiresAt == nil {
		return nil, ErrInvalidToken
	}
	// encoding/json treats null as a zero value for strings/bools/slices. Optional
	// omission is supported, but an explicitly ill-typed claim is not omission.
	for _, name := range []string{"email", "email_verified", "groups", "azp", "nbf"} {
		if raw, ok := present[name]; ok && strings.TrimSpace(string(raw)) == "null" {
			return nil, ErrInvalidToken
		}
	}
	if _, ok := present["azp"]; ok && profile.AuthorizedFor != v.clientID {
		return nil, ErrInvalidToken
	}
	if (len(token.Audience) > 1 && profile.AuthorizedFor != v.clientID) ||
		(profile.AuthorizedFor != "" && profile.AuthorizedFor != v.clientID) {
		return nil, ErrInvalidToken
	}
	now := time.Now()
	// This profile accepts integer NumericDate values, no expired token grace,
	// and at most 60 seconds of future iat/nbf clock skew.
	if *profile.IssuedAt < 0 || *profile.ExpiresAt <= *profile.IssuedAt ||
		!now.Before(token.Expiry) || token.IssuedAt.After(now.Add(time.Minute)) {
		return nil, ErrInvalidToken
	}
	if profile.NotBefore != nil && (*profile.NotBefore > *profile.ExpiresAt || time.Unix(*profile.NotBefore, 0).After(now.Add(time.Minute))) {
		return nil, ErrInvalidToken
	}
	if !validText(profile.Email, 512, false) || len(profile.Groups) > 256 {
		return nil, ErrInvalidToken
	}
	for _, group := range profile.Groups {
		if !validText(group, 512, true) {
			return nil, ErrInvalidToken
		}
	}
	return &Claims{
		Issuer: token.Issuer, Subject: token.Subject, Email: profile.Email,
		EmailVerified: profile.EmailVerified, Groups: append([]string(nil), profile.Groups...),
		Audience: append([]string(nil), token.Audience...), IssuedAt: token.IssuedAt, ExpiresAt: token.Expiry,
	}, nil
}

// Go's struct decoder matches JSON field names case-insensitively. JWT claim
// names are case-sensitive, so a later "ISS" must not shadow canonical "iss".
// Reject aliases before trusting either upstream-decoded or profile values.
// Unrelated extension claims remain allowed; no authentication decision is
// based on them. Required canonical claims must actually exist in the payload.
func canonicalClaimNames(claims map[string]json.RawMessage) bool {
	canonical := [...]string{"iss", "sub", "aud", "exp", "iat", "nbf", "nonce", "azp", "email", "email_verified", "groups"}
	for name := range claims {
		for _, expected := range canonical {
			if name != expected && strings.EqualFold(name, expected) {
				return false
			}
		}
	}
	for _, name := range []string{"iss", "sub", "aud", "exp", "iat", "nonce"} {
		if _, present := claims[name]; !present {
			return false
		}
	}
	return true
}

func (v *Verifier) currentValidator(algorithm string) (*oidc.IDTokenVerifier, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil, ErrClosed
	}
	now := time.Now()
	if v.validators == nil || !now.Before(v.cacheUntil) {
		// go-oidc refreshes on an unfamiliar kid. Replacing its cache on a bounded
		// interval additionally bounds use of keys removed from JWKS.
		v.validators = make(map[string]*oidc.IDTokenVerifier)
		v.cacheUntil = now.Add(v.cacheTTL)
	}
	if v.validators[algorithm] == nil {
		client := *v.client
		client.Transport = &signingKeyTransport{
			base: v.client.Transport, algorithm: algorithm, allowUnspecifiedAlgorithm: len(v.algorithms) == 1,
		}
		v.validators[algorithm] = v.provider.VerifierContext(oidc.ClientContext(context.Background(), &client), &oidc.Config{
			ClientID: v.clientID, SupportedSigningAlgs: []string{algorithm},
		})
	}
	return v.validators[algorithm], nil
}

// Close prevents new verification calls and releases idle HTTP connections.
// Already running bounded requests may finish; caller cancellation remains active.
func (v *Verifier) Close() {
	v.mu.Lock()
	v.closed = true
	v.mu.Unlock()
	v.transport.CloseIdleConnections()
}

func validText(value string, maxBytes int, required bool) bool {
	if (required && value == "") || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
