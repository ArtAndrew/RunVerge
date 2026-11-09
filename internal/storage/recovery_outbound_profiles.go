// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// RecoveryOutboundProfile identifies a stored connection, not a credential or
// permission to dispatch. Workers check these before resolving any secret;
// ordinary authority, immutable profile and dispatch checks still apply.
type RecoveryOutboundProfile struct {
	OrganizationID string
	Kind           string
	ID             string
	Version        int
}

func checkRecoveryOutboundProfileTx(ctx context.Context, tx pgx.Tx, org, kind, id string, version int) error {
	var table string
	switch kind {
	case "model":
		table = "model_connections"
	case "tool":
		table = "tool_connections"
	case "mcp":
		table = "mcp_connections"
	case "webhook":
		table = "webhook_destinations"
	case "destination":
		table = "configuration_destinations"
	case "credential_purpose":
		table = "credential_purpose_profiles"
	default:
		return domain.Forbidden
	}
	if org == "" || id == "" || version < 1 {
		return domain.Forbidden
	}
	if err := admitRecoveryRowTx(ctx, tx, table, org, id, version); err != nil {
		if errors.Is(err, RecoveryHeld) {
			return domain.Forbidden
		}
		return err
	}
	return nil
}

// Imported secret references and even keyless endpoints are historical data,
// never permission to use the restored host's environment or network. This is
// a denial fence, not the future explicit recovery reauthorization operation.
func (s *Store) CheckRecoveryOutboundProfiles(ctx context.Context, refs ...RecoveryOutboundProfile) error {
	if len(refs) == 0 || len(refs) > 4 {
		return domain.Forbidden
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, ref := range refs {
		if err = checkRecoveryOutboundProfileTx(ctx, tx, ref.OrganizationID, ref.Kind, ref.ID, ref.Version); err != nil {
			return err
		}
		if ref.Kind == "model" || ref.Kind == "mcp" {
			if err = checkConfigurationResourceAdmissionTx(ctx, tx, ref.OrganizationID, ref.Kind, ref.ID, ref.Version); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}
