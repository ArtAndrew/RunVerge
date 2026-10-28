// SPDX-License-Identifier: Elastic-2.0
package identity

import (
	"context"
	"errors"
	"time"
)

var (
	ErrLoginRejected         = errors.New("oidc: login rejected")
	ErrIdentityUnmapped      = errors.New("oidc: identity has no authorized account mapping")
	ErrSessionInvalid        = errors.New("oidc: session invalid")
	ErrLoginStoreUnavailable = errors.New("oidc: login storage unavailable")
)

// LoginTransaction is durable, short-lived server-side login state. State and
// browser-binding values are SHA-256 hex digests. CodeVerifier is a credential:
// protect it at rest and never include it, Nonce or hashes in application logs.
type LoginTransaction struct {
	StateHash, BindingHash, Issuer, ClientID, RedirectURI, Nonce, CodeVerifier string
	CreatedAt, ExpiresAt                                                       time.Time
}

// BrowserSession stores only a token digest. Email/groups do not grant access.
// AuthenticatedAt is the platform login time, not the provider's auth_time claim.
type BrowserSession struct {
	ID, TokenHash, PrincipalID, Issuer, Subject string
	AuthenticatedAt, CreatedAt, ExpiresAt       time.Time
}

// LoginStore must be implemented by durable transactional storage in production.
// Expiry uses the authoritative store clock. Returning a session must recheck
// principal activity and that (Issuer, Subject) still maps to the same principal.
// All failures are fail-closed; implementations return the sentinels above.
type LoginStore interface {
	CreateLogin(context.Context, LoginTransaction) error
	// Atomically consume a matching, unexpired state+browser binding exactly once.
	// A subsequent token-exchange failure does not make the transaction reusable.
	ConsumeLogin(ctx context.Context, stateHash, bindingHash string) (LoginTransaction, error)
	// Resolve an administrator-created mapping. No email linking or automatic grants.
	ResolveIdentity(ctx context.Context, issuer, subject string) (principalID string, err error)
	// Atomically create the new session and revoke the previous browser session,
	// if any. Never revoke a service/Bearer credential that happens to share a hash.
	CreateSession(ctx context.Context, session BrowserSession, previousTokenHash string) error
	GetSession(ctx context.Context, tokenHash string) (BrowserSession, error)
	// CAS against an active old session; preserve principal, issuer, subject,
	// AuthenticatedAt and absolute ExpiresAt. Revoke old and create new atomically.
	RotateSession(ctx context.Context, oldTokenHash string, replacement BrowserSession) error
	// Idempotent browser-session revocation; never a global IdP logout.
	RevokeSession(ctx context.Context, tokenHash string) error
}
