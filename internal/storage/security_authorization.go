// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Reads use a fresh credential and effective permission in the same snapshot
// as their tenant data; supplied Kind/Role values are never read authority.
func (s *Store) beginPrincipalRead(ctx context.Context, p domain.Principal, permission string) (pgx.Tx, error) {
	tx, err := s.beginRuntimeTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	current := p
	err = tx.QueryRow(ctx, authQuery(p), p.TokenHash, p.OrganizationID).Scan(&current.ID, &current.OrganizationID, &current.Role, &current.Kind)
	if errors.Is(err, pgx.ErrNoRows) {
		err = domain.Unauthenticated
	}
	if err == nil && (current.ID != p.ID || current.OrganizationID != p.OrganizationID) {
		err = domain.Unauthenticated
	}
	if err == nil && !domain.PrincipalHasPermission(current, permission) {
		err = domain.Forbidden
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}
func principalPermissionGuard(permission string) func(context.Context, pgx.Tx, domain.Principal) error {
	return func(_ context.Context, _ pgx.Tx, p domain.Principal) error {
		if !domain.PrincipalHasPermission(p, permission) {
			return domain.Forbidden
		}
		return nil
	}
}
