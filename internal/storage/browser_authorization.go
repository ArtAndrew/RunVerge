// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

const BrowserSessionCredential = "browser_session"

const browserAuthSQL = `SELECT p.id,m.organization_id,m.role,p.kind FROM browser_sessions t
JOIN identity_mappings i ON i.issuer=t.issuer AND i.subject=t.subject AND i.principal_id=t.principal_id
JOIN principals p ON p.id=t.principal_id JOIN memberships m ON m.principal_id=p.id
JOIN organizations o ON o.id=m.organization_id
JOIN organization_login_policies q ON q.organization_id=m.organization_id
WHERE t.token_hash=$1 AND m.organization_id=$2 AND NOT t.revoked AND t.expires_at>clock_timestamp()
AND i.active AND p.active AND p.kind='human' AND m.active AND o.active AND (q.mode='mixed' OR (q.mode='oidc_required' AND q.oidc_issuer=t.issuer)) AND t.credential_generation>m.credential_cutoff`

// authQuery keeps internal reauthorization in the credential's original domain.
// A browser-session hash must never be looked up in the Bearer credential table.
func authQuery(p domain.Principal) string {
	switch p.CredentialKind {
	case "", "bearer":
		return authSQL
	case ServiceKeyCredential:
		return serviceAuthSQL
	case BrowserSessionCredential:
		return browserAuthSQL
	default:
		return `SELECT NULL::text,NULL::text,NULL::text,NULL::text WHERE false AND $1::text IS NOT NULL AND $2::text IS NOT NULL`
	}
}

func (s *Store) AuthenticateBrowserSession(ctx context.Context, hash, org string) (domain.Principal, error) {
	p := domain.Principal{TokenHash: hash, CredentialKind: BrowserSessionCredential}
	if !validDigest(hash) || !validText(org, 128) {
		return p, domain.Unauthenticated
	}
	tx, e := s.beginRuntimeTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if e != nil {
		return p, e
	}
	defer tx.Rollback(ctx)
	e = tx.QueryRow(ctx, browserAuthSQL, hash, org).Scan(&p.ID, &p.OrganizationID, &p.Role, &p.Kind)
	if errors.Is(e, pgx.ErrNoRows) {
		return p, domain.Unauthenticated
	}
	return p, e
}

type BrowserOrganization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// BrowserOrganizations reads only memberships reachable from the currently
// mapped active session. It intentionally exposes no tokens or provider data.
func (s *Store) BrowserOrganizations(ctx context.Context, hash, rawCursor string, limit int) ([]BrowserOrganization, string, error) {
	if !validDigest(hash) {
		return nil, "", domain.Unauthenticated
	}
	if limit < 1 || limit > 100 {
		return nil, "", domain.Err("invalid_request", "Некорректный limit", 400)
	}
	c, e := decodeCursor(rawCursor)
	if e != nil {
		return nil, "", e
	}
	tx, e := s.beginRuntimeTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if e != nil {
		return nil, "", e
	}
	defer tx.Rollback(ctx)
	rows, e := tx.Query(ctx, `SELECT o.id,o.name,m.role,o.created_at FROM browser_sessions t
JOIN identity_mappings i ON i.issuer=t.issuer AND i.subject=t.subject AND i.principal_id=t.principal_id
JOIN principals p ON p.id=t.principal_id JOIN memberships m ON m.principal_id=p.id
JOIN organizations o ON o.id=m.organization_id
JOIN organization_login_policies q ON q.organization_id=m.organization_id
WHERE t.token_hash=$1 AND NOT t.revoked AND t.expires_at>clock_timestamp() AND i.active AND p.active AND p.kind='human' AND m.active AND o.active AND (q.mode='mixed' OR (q.mode='oidc_required' AND q.oidc_issuer=t.issuer))
AND t.credential_generation>m.credential_cutoff
AND (o.created_at,o.id)<($2,$3) ORDER BY o.created_at DESC,o.id DESC LIMIT $4`, hash, c.CreatedAt, c.ID, limit+1)
	if e != nil {
		return nil, "", e
	}
	defer rows.Close()
	items := []BrowserOrganization{}
	var last time.Time
	for rows.Next() {
		var item BrowserOrganization
		var created time.Time
		if e = rows.Scan(&item.ID, &item.Name, &item.Role, &created); e != nil {
			return nil, "", e
		}
		if len(items) == limit {
			return items, encodeCursor(last, items[len(items)-1].ID), nil
		}
		items = append(items, item)
		last = created
	}
	return items, "", rows.Err()
}
