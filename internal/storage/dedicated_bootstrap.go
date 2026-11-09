// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"agentplatform.local/platform/internal/domain"
)

// BootstrapDedicated is the trusted-host initializer for a single-tenant
// installation. Reuse the same private seed after an uncertain commit.
func (s *Store) BootstrapDedicated(ctx context.Context, name string, budget int64, seed [32]byte, requireExisting bool) (Bootstrap, error) {
	if !validText(name, 120) || budget <= 0 || budget > domain.MaxMoney || seed == ([32]byte{}) {
		return Bootstrap{}, domain.Err("invalid_request", "Invalid dedicated bootstrap", 400)
	}
	derive := func(kind string) []byte {
		mac := hmac.New(sha256.New, seed[:])
		fmt.Fprintf(mac, "runverge/dedicated-bootstrap/v1/%s\x00%d\x00%s", kind, budget, name)
		return mac.Sum(nil)
	}
	credentials := Bootstrap{
		OrganizationID: "org_" + hex.EncodeToString(derive("organization")[:16]),
		PrincipalID:    "usr_" + hex.EncodeToString(derive("owner")[:16]),
		Token:          "ap_" + hex.EncodeToString(derive("credential")),
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return Bootstrap{}, err
	}
	defer tx.Rollback(ctx)
	// Serialize the empty-installation check with every organization insert,
	// including the ordinary, unseeded bootstrap path.
	if _, err = tx.Exec(ctx, "LOCK TABLE organizations IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return Bootstrap{}, err
	}
	var matched, occupied bool
	err = tx.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1 FROM memberships m JOIN access_tokens t ON t.principal_id=m.principal_id
  WHERE m.organization_id=$1 AND m.principal_id=$2 AND t.token_hash=$3
), EXISTS(SELECT 1 FROM organizations WHERE id<>$1)
`, credentials.OrganizationID, credentials.PrincipalID, Hash([]byte(credentials.Token))).Scan(&matched, &occupied)
	if err != nil {
		return Bootstrap{}, err
	}
	if occupied || (requireExisting && !matched) {
		return Bootstrap{}, domain.Err("conflict", "Dedicated bootstrap does not match this installation", 409)
	}
	// Never modify the credential or membership on replay, even if revoked.
	if !matched {
		if err = insertBootstrap(ctx, tx, credentials, name, budget); err != nil {
			return Bootstrap{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Bootstrap{}, err
	}
	return credentials, nil
}
