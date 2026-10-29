// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	oidcidentity "agentplatform.local/platform/internal/identity"
	"github.com/jackc/pgx/v5"
)

var _ oidcidentity.LoginStore = (*Store)(nil)

// ConfigureLoginEncryption is a startup-only operation. The deployment key must
// be backed up with the database and supplied by a secret file/broker, not SQL.
// PKCE verifiers are encrypted with AAD binding all immutable login context.
func (s *Store) ConfigureLoginEncryption(key []byte) error {
	if len(key) != 32 {
		return oidcidentity.ErrInvalidConfig
	}
	block, e := aes.NewCipher(key)
	if e != nil {
		return oidcidentity.ErrInvalidConfig
	}
	aead, e := cipher.NewGCM(block)
	if e != nil {
		return oidcidentity.ErrInvalidConfig
	}
	s.loginCipher = aead
	return nil
}
func loginAAD(t oidcidentity.LoginTransaction) []byte {
	b, _ := json.Marshal([]string{"login-pkce-v1", t.StateHash, t.BindingHash, t.Issuer, t.ClientID, t.RedirectURI, t.Nonce})
	return b
}
func (s *Store) CreateLogin(ctx context.Context, t oidcidentity.LoginTransaction) error {
	if s.loginCipher == nil {
		return oidcidentity.ErrLoginStoreUnavailable
	}
	if !validDigest(t.StateHash) || !validDigest(t.BindingHash) || !validText(t.Nonce, 128) || !validText(t.CodeVerifier, 128) || !validText(t.Issuer, 2048) || !validText(t.ClientID, 512) || !validText(t.RedirectURI, 2048) || !t.ExpiresAt.After(t.CreatedAt) || t.ExpiresAt.Sub(t.CreatedAt) > 10*time.Minute {
		return oidcidentity.ErrLoginRejected
	}
	nonce := make([]byte, s.loginCipher.NonceSize())
	if _, e := rand.Read(nonce); e != nil {
		return oidcidentity.ErrLoginStoreUnavailable
	}
	sealed := s.loginCipher.Seal(nonce, nonce, []byte(t.CodeVerifier), loginAAD(t))
	result, e := s.execRuntime(ctx, "INSERT INTO login_transactions(state_hash,binding_hash,issuer,client_id,redirect_uri,nonce,code_verifier_ciphertext,created_at,expires_at) SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9 WHERE $9::timestamptz>clock_timestamp() AND $8::timestamptz<=clock_timestamp()+interval '1 minute'", t.StateHash, t.BindingHash, t.Issuer, t.ClientID, t.RedirectURI, t.Nonce, sealed, t.CreatedAt, t.ExpiresAt)
	if e != nil {
		return oidcidentity.ErrLoginStoreUnavailable
	}
	if result.RowsAffected() != 1 {
		return oidcidentity.ErrLoginRejected
	}
	return nil
}
func (s *Store) ConsumeLogin(ctx context.Context, stateHash, bindingHash string) (oidcidentity.LoginTransaction, error) {
	var t oidcidentity.LoginTransaction
	if s.loginCipher == nil {
		return t, oidcidentity.ErrLoginStoreUnavailable
	}
	if !validDigest(stateHash) || !validDigest(bindingHash) {
		return t, oidcidentity.ErrLoginRejected
	}
	var sealed []byte
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return t, oidcidentity.ErrLoginStoreUnavailable
	}
	defer tx.Rollback(ctx)
	e = tx.QueryRow(ctx, `WITH selected AS (
 SELECT * FROM login_transactions WHERE state_hash=$1 AND binding_hash=$2 AND consumed_at IS NULL AND expires_at>clock_timestamp() FOR UPDATE
 ), consumed AS (
 UPDATE login_transactions l SET consumed_at=clock_timestamp(),code_verifier_ciphertext=''::bytea FROM selected x
 WHERE l.state_hash=x.state_hash AND l.consumed_at IS NULL AND l.expires_at>clock_timestamp() RETURNING l.state_hash
 ) SELECT x.state_hash,x.binding_hash,x.issuer,x.client_id,x.redirect_uri,x.nonce,x.code_verifier_ciphertext,x.created_at,x.expires_at FROM selected x JOIN consumed c USING(state_hash)`, stateHash, bindingHash).Scan(&t.StateHash, &t.BindingHash, &t.Issuer, &t.ClientID, &t.RedirectURI, &t.Nonce, &sealed, &t.CreatedAt, &t.ExpiresAt)
	if errors.Is(e, pgx.ErrNoRows) {
		return oidcidentity.LoginTransaction{}, oidcidentity.ErrLoginRejected
	}
	if e != nil {
		return oidcidentity.LoginTransaction{}, oidcidentity.ErrLoginStoreUnavailable
	}
	if e = tx.Commit(ctx); e != nil {
		return oidcidentity.LoginTransaction{}, oidcidentity.ErrLoginStoreUnavailable
	}
	if len(sealed) < s.loginCipher.NonceSize()+s.loginCipher.Overhead() {
		return oidcidentity.LoginTransaction{}, oidcidentity.ErrLoginRejected
	}
	plain, e := s.loginCipher.Open(nil, sealed[:s.loginCipher.NonceSize()], sealed[s.loginCipher.NonceSize():], loginAAD(t))
	if e != nil {
		return oidcidentity.LoginTransaction{}, oidcidentity.ErrLoginRejected
	}
	t.CodeVerifier = string(plain)
	return t, nil
}

// LinkIdentity is a trusted-host setup operation. No public or email-based
// linking route exists. An existing mapping cannot silently change principals.
func (s *Store) LinkIdentity(ctx context.Context, issuer, subject, principalID string) error {
	u, e := url.Parse(issuer)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || strings.TrimSpace(issuer) != issuer || !validText(issuer, 2048) || !validText(subject, 1024) || !validText(principalID, 128) {
		return domain.Err("invalid_request", "Некорректная привязка идентичности", 400)
	}
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	var active bool
	var kind string
	if e = tx.QueryRow(ctx, "SELECT active,kind FROM principals WHERE id=$1 FOR UPDATE", principalID).Scan(&active, &kind); e != nil {
		return rowExists(e)
	}
	if !active || kind != domain.PrincipalHuman {
		return domain.Forbidden
	}
	var current string
	e = tx.QueryRow(ctx, "SELECT principal_id FROM identity_mappings WHERE issuer=$1 AND subject=$2 FOR UPDATE", issuer, subject).Scan(&current)
	if e == nil && current != principalID {
		return domain.Err("conflict", "Идентичность уже связана с другой учётной записью", 409)
	}
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return e
	}
	result, e := tx.Exec(ctx, "INSERT INTO identity_mappings(issuer,subject,principal_id) VALUES($1,$2,$3) ON CONFLICT(issuer,subject) DO UPDATE SET active=true WHERE identity_mappings.principal_id=EXCLUDED.principal_id", issuer, subject, principalID)
	if e != nil {
		return e
	}
	if result.RowsAffected() != 1 {
		return domain.Err("conflict", "Идентичность уже связана с другой учётной записью", 409)
	}
	return tx.Commit(ctx)
}
func (s *Store) ResolveIdentity(ctx context.Context, issuer, subject string) (string, error) {
	var id string
	tx, e := s.beginRuntimeTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if e != nil {
		return "", oidcidentity.ErrLoginStoreUnavailable
	}
	defer tx.Rollback(ctx)
	e = tx.QueryRow(ctx, "SELECT p.id FROM identity_mappings i JOIN principals p ON p.id=i.principal_id WHERE i.issuer=$1 AND i.subject=$2 AND i.active AND p.active AND p.kind='human' AND NOT recovery_historical_principal(p.id)", issuer, subject).Scan(&id)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", oidcidentity.ErrIdentityUnmapped
	}
	if e != nil {
		return "", oidcidentity.ErrLoginStoreUnavailable
	}
	return id, nil
}

const sessionColumns = "t.id,t.token_hash,t.principal_id,t.issuer,t.subject,t.authenticated_at,t.created_at,t.expires_at"
const currentSessionFrom = " FROM browser_sessions t JOIN identity_mappings i ON i.issuer=t.issuer AND i.subject=t.subject AND i.principal_id=t.principal_id JOIN principals p ON p.id=t.principal_id WHERE t.token_hash=$1 AND NOT t.revoked AND t.expires_at>clock_timestamp() AND i.active AND p.active AND p.kind='human' AND NOT recovery_historical_principal(p.id)"

func scanBrowserSession(row scanner) (oidcidentity.BrowserSession, error) {
	var t oidcidentity.BrowserSession
	e := row.Scan(&t.ID, &t.TokenHash, &t.PrincipalID, &t.Issuer, &t.Subject, &t.AuthenticatedAt, &t.CreatedAt, &t.ExpiresAt)
	return t, e
}
func (s *Store) GetSession(ctx context.Context, tokenHash string) (oidcidentity.BrowserSession, error) {
	if !validDigest(tokenHash) {
		return oidcidentity.BrowserSession{}, oidcidentity.ErrSessionInvalid
	}
	tx, e := s.beginRuntimeTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if e != nil {
		return oidcidentity.BrowserSession{}, oidcidentity.ErrLoginStoreUnavailable
	}
	defer tx.Rollback(ctx)
	t, e := scanBrowserSession(tx.QueryRow(ctx, "SELECT "+sessionColumns+currentSessionFrom, tokenHash))
	if errors.Is(e, pgx.ErrNoRows) {
		return oidcidentity.BrowserSession{}, oidcidentity.ErrSessionInvalid
	}
	if e != nil {
		return oidcidentity.BrowserSession{}, oidcidentity.ErrLoginStoreUnavailable
	}
	return t, nil
}
func validBrowserSession(t oidcidentity.BrowserSession) bool {
	return validText(t.ID, 128) && validDigest(t.TokenHash) && validText(t.PrincipalID, 128) && validText(t.Issuer, 2048) && validText(t.Subject, 1024) && !t.AuthenticatedAt.IsZero() && !t.CreatedAt.Before(t.AuthenticatedAt) && t.ExpiresAt.After(t.CreatedAt) && t.ExpiresAt.Sub(t.AuthenticatedAt) <= 24*time.Hour
}
func lockBrowserAuthority(ctx context.Context, tx pgx.Tx, t oidcidentity.BrowserSession) error {
	var active bool
	var kind string
	if e := tx.QueryRow(ctx, "SELECT active,kind FROM principals WHERE id=$1 AND NOT recovery_historical_principal(id) FOR SHARE", t.PrincipalID).Scan(&active, &kind); e != nil {
		return e
	}
	if !active || kind != domain.PrincipalHuman {
		return oidcidentity.ErrSessionInvalid
	}
	var id string
	if e := tx.QueryRow(ctx, "SELECT principal_id FROM identity_mappings WHERE issuer=$1 AND subject=$2 AND active FOR SHARE", t.Issuer, t.Subject).Scan(&id); e != nil {
		return e
	}
	if id != t.PrincipalID {
		return oidcidentity.ErrSessionInvalid
	}
	return nil
}
func insertBrowserSession(ctx context.Context, tx pgx.Tx, t oidcidentity.BrowserSession, generation *int64) error {
	result, e := tx.Exec(ctx, "INSERT INTO browser_sessions(id,token_hash,principal_id,issuer,subject,authenticated_at,created_at,expires_at,credential_generation) SELECT $1,$2,$3,$4,$5,$6,$7,$8,COALESCE($9::bigint,nextval('credential_origin_sequence')) WHERE $8::timestamptz>clock_timestamp() AND $7::timestamptz<=clock_timestamp()+interval '1 minute'", t.ID, t.TokenHash, t.PrincipalID, t.Issuer, t.Subject, t.AuthenticatedAt, t.CreatedAt, t.ExpiresAt, generation)
	if e != nil {
		return e
	}
	if result.RowsAffected() != 1 {
		return oidcidentity.ErrSessionInvalid
	}
	return nil
}
func loginStoreError(e error) error {
	if e == nil {
		return nil
	}
	if errors.Is(e, oidcidentity.ErrSessionInvalid) || errors.Is(e, pgx.ErrNoRows) {
		return oidcidentity.ErrSessionInvalid
	}
	return oidcidentity.ErrLoginStoreUnavailable
}
func (s *Store) CreateSession(ctx context.Context, t oidcidentity.BrowserSession, previousTokenHash string) error {
	if !validBrowserSession(t) || previousTokenHash != "" && !validDigest(previousTokenHash) {
		return oidcidentity.ErrSessionInvalid
	}
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return oidcidentity.ErrLoginStoreUnavailable
	}
	defer tx.Rollback(ctx)
	if e = lockBrowserAuthority(ctx, tx, t); e != nil {
		return loginStoreError(e)
	}
	if previousTokenHash != "" {
		if _, e = tx.Exec(ctx, "UPDATE browser_sessions SET revoked=true WHERE token_hash=$1", previousTokenHash); e != nil {
			return oidcidentity.ErrLoginStoreUnavailable
		}
	}
	if e = insertBrowserSession(ctx, tx, t, nil); e != nil {
		return loginStoreError(e)
	}
	return loginStoreError(tx.Commit(ctx))
}
func (s *Store) RotateSession(ctx context.Context, oldHash string, replacement oidcidentity.BrowserSession) error {
	if !validDigest(oldHash) || !validBrowserSession(replacement) || oldHash == replacement.TokenHash {
		return oidcidentity.ErrSessionInvalid
	}
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return oidcidentity.ErrLoginStoreUnavailable
	}
	defer tx.Rollback(ctx)
	if e = lockBrowserAuthority(ctx, tx, replacement); e != nil {
		return loginStoreError(e)
	}
	old, e := scanBrowserSession(tx.QueryRow(ctx, "SELECT "+sessionColumns+currentSessionFrom+" FOR UPDATE OF t", oldHash))
	if e != nil {
		return loginStoreError(e)
	}
	if old.PrincipalID != replacement.PrincipalID || old.Issuer != replacement.Issuer || old.Subject != replacement.Subject || !old.AuthenticatedAt.Equal(replacement.AuthenticatedAt) || !old.ExpiresAt.Equal(replacement.ExpiresAt) {
		return oidcidentity.ErrSessionInvalid
	}
	var generation int64
	if e = tx.QueryRow(ctx, "SELECT credential_generation FROM browser_sessions WHERE token_hash=$1", oldHash).Scan(&generation); e != nil {
		return loginStoreError(e)
	}
	if _, e = tx.Exec(ctx, "UPDATE browser_sessions SET revoked=true WHERE token_hash=$1", oldHash); e != nil {
		return oidcidentity.ErrLoginStoreUnavailable
	}
	if e = insertBrowserSession(ctx, tx, replacement, &generation); e != nil {
		return loginStoreError(e)
	}
	return loginStoreError(tx.Commit(ctx))
}
func (s *Store) RevokeSession(ctx context.Context, tokenHash string) error {
	if !validDigest(tokenHash) {
		return oidcidentity.ErrSessionInvalid
	}
	_, e := s.execRuntime(ctx, "UPDATE browser_sessions SET revoked=true WHERE token_hash=$1", tokenHash)
	if e != nil {
		return oidcidentity.ErrLoginStoreUnavailable
	}
	return nil
}
