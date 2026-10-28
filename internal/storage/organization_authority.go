// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Authority locks always precede structural, operation and accounting locks.
// Membership administrators never acquire those business locks.
func lockAuthorityPrincipalsTx(ctx context.Context, tx pgx.Tx, org string, ids []string) error {
	if err := lockOrganizationPolicyTx(ctx, tx, org); err != nil {
		return err
	}
	for i, query := range []string{
		"SELECT id FROM principals WHERE id=ANY($1::text[]) ORDER BY id FOR SHARE",
		"SELECT principal_id FROM memberships WHERE organization_id=$2 AND principal_id=ANY($1::text[]) ORDER BY principal_id FOR SHARE",
	} {
		args := []any{ids}
		if i == 1 {
			args = append(args, org)
		}
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func lockOperationAuthorityRowsTx(ctx context.Context, tx pgx.Tx, org, taskID, operationID string) error {
	var ids []string
	err := tx.QueryRow(ctx, `SELECT ARRAY(SELECT DISTINCT x FROM unnest(ARRAY[t.owner_id,o.authority_principal_id]) x ORDER BY x)
 FROM tasks t JOIN operations o ON o.organization_id=t.organization_id AND o.task_id=t.id
 WHERE t.organization_id=$1 AND t.id=$2 AND o.id=$3`, org, taskID, operationID).Scan(&ids)
	if err != nil {
		return rowExists(err)
	}
	ancestors, err := ancestorAuthorityIDsTx(ctx, tx, org, taskID)
	if err != nil {
		return err
	}
	ids = append(ids, ancestors...)
	if err = lockAuthorityPrincipalsTx(ctx, tx, org, ids); err != nil {
		return err
	}
	return nil
}

func lockOperationAuthorityTx(ctx context.Context, tx pgx.Tx, org, taskID, operationID string) error {
	if err := lockOperationAuthorityRowsTx(ctx, tx, org, taskID, operationID); err != nil {
		return err
	}
	return checkOperationAuthorityTx(ctx, tx, org, taskID, operationID)
}

func checkOperationAuthorityTx(ctx context.Context, tx pgx.Tx, org, taskID, operationID string) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operations o
 JOIN tasks t ON t.organization_id=o.organization_id AND t.id=o.task_id
 JOIN memberships own ON own.organization_id=t.organization_id AND own.principal_id=t.owner_id
 JOIN principals op ON op.id=own.principal_id
 JOIN memberships actor ON actor.organization_id=o.organization_id AND actor.principal_id=o.authority_principal_id
 JOIN principals ap ON ap.id=actor.principal_id
 JOIN organizations tenant ON tenant.id=o.organization_id
 JOIN organization_login_policies policy ON policy.organization_id=o.organization_id
 WHERE o.organization_id=$1 AND o.task_id=$2 AND o.id=$3
 AND own.active AND op.active AND own.role<>'viewer' AND actor.active AND ap.active AND actor.role<>'viewer' AND tenant.active
 AND (op.kind='human' OR policy.service_access_enabled) AND (ap.kind='human' OR policy.service_access_enabled)
 AND o.owner_authority_epoch=own.security_epoch AND o.principal_authority_epoch=actor.security_epoch
 AND o.owner_policy_epoch=current_principal_policy_epoch(o.organization_id,t.owner_id) AND o.principal_policy_epoch=current_principal_policy_epoch(o.organization_id,o.authority_principal_id))`, org, taskID, operationID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return domain.Forbidden
	}
	return checkAncestorRunAuthorityTx(ctx, tx, org, taskID)
}

func lockRunAuthorityTx(ctx context.Context, tx pgx.Tx, org, taskID, runID string) error {
	var ids []string
	err := tx.QueryRow(ctx, `SELECT ARRAY(SELECT DISTINCT x FROM unnest(ARRAY[t.owner_id,r.principal_id]) x ORDER BY x)
 FROM tasks t JOIN runs r ON r.organization_id=t.organization_id AND r.task_id=t.id WHERE t.organization_id=$1 AND t.id=$2 AND r.id=$3`, org, taskID, runID).Scan(&ids)
	if err != nil {
		return rowExists(err)
	}
	ancestors, err := ancestorAuthorityIDsTx(ctx, tx, org, taskID)
	if err != nil {
		return err
	}
	ids = append(ids, ancestors...)
	if err = lockAuthorityPrincipalsTx(ctx, tx, org, ids); err != nil {
		return err
	}
	return checkRunAuthorityTx(ctx, tx, org, taskID, runID)
}

func checkRunAuthorityTx(ctx context.Context, tx pgx.Tx, org, taskID, runID string) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runs r
 JOIN tasks t ON t.organization_id=r.organization_id AND t.id=r.task_id
 JOIN memberships own ON own.organization_id=t.organization_id AND own.principal_id=t.owner_id
 JOIN principals op ON op.id=own.principal_id
 JOIN memberships actor ON actor.organization_id=r.organization_id AND actor.principal_id=r.principal_id
 JOIN principals ap ON ap.id=actor.principal_id
 JOIN organizations tenant ON tenant.id=r.organization_id
 JOIN organization_login_policies policy ON policy.organization_id=r.organization_id
 WHERE r.organization_id=$1 AND r.task_id=$2 AND r.id=$3
 AND own.active AND op.active AND own.role<>'viewer' AND actor.active AND ap.active AND actor.role<>'viewer' AND tenant.active
 AND (op.kind='human' OR policy.service_access_enabled) AND (ap.kind='human' OR policy.service_access_enabled)
 AND r.owner_authority_epoch=own.security_epoch AND r.principal_authority_epoch=actor.security_epoch
 AND r.owner_policy_epoch=current_principal_policy_epoch(r.organization_id,t.owner_id) AND r.principal_policy_epoch=current_principal_policy_epoch(r.organization_id,r.principal_id))`, org, taskID, runID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return domain.Forbidden
	}
	return checkAncestorRunAuthorityTx(ctx, tx, org, taskID)
}

func staleOperationAuthorityTx(ctx context.Context, tx pgx.Tx, l *domain.Lease) error {
	err := checkOperationAuthorityTx(ctx, tx, l.OrganizationID, l.TaskID, l.OperationID)
	if errors.Is(err, domain.Forbidden) {
		return domain.Err("stale_lease", "Право на выполнение изменилось", 409)
	}
	return err
}

// Frozen execution consent cannot be revived by a later role/active cycle.
// Late factual observations and accounting intentionally do not call this.
func checkToolAuthorityEpochsTx(ctx context.Context, tx pgx.Tx, e domain.ToolExecution, requireApproval bool) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tool_executions j
 JOIN tasks t ON t.organization_id=j.organization_id AND t.id=j.task_id
 JOIN memberships own ON own.organization_id=j.organization_id AND own.principal_id=t.owner_id
 JOIN memberships creator ON creator.organization_id=j.organization_id AND creator.principal_id=j.created_by
 JOIN approvals a ON a.organization_id=j.organization_id AND a.id=j.approval_id
 LEFT JOIN memberships decider ON decider.organization_id=j.organization_id AND decider.principal_id=a.decided_by
 LEFT JOIN memberships dispatcher ON dispatcher.organization_id=j.organization_id AND dispatcher.principal_id=j.dispatch_requested_by
 WHERE j.organization_id=$1 AND j.id=$2 AND j.owner_authority_epoch=own.security_epoch AND j.creator_authority_epoch=creator.security_epoch
 AND (NOT $3 OR a.decider_authority_epoch=decider.security_epoch)
 AND (j.dispatch_requested_by IS NULL OR j.dispatcher_authority_epoch=dispatcher.security_epoch)
 AND j.owner_policy_epoch=current_principal_policy_epoch(j.organization_id,t.owner_id) AND j.creator_policy_epoch=current_principal_policy_epoch(j.organization_id,j.created_by)
 AND (NOT $3 OR a.decider_policy_epoch=current_principal_policy_epoch(j.organization_id,a.decided_by))
 AND (j.dispatch_requested_by IS NULL OR j.dispatcher_policy_epoch=current_principal_policy_epoch(j.organization_id,j.dispatch_requested_by)))`, e.OrganizationID, e.ID, requireApproval).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return domain.Forbidden
	}
	return checkAncestorRunAuthorityTx(ctx, tx, e.OrganizationID, e.TaskID)
}

// Parent Run consent is immutable. A fresh explicit root operation may renew
// its own consent; a managed child must still respect every ancestor's Run.
const authorityAncestorCTE = `WITH RECURSIVE authority_ancestors AS (
 SELECT p.id,p.parent_id,p.current_run_id,p.owner_id,1 AS level FROM tasks t JOIN tasks p ON p.organization_id=t.organization_id AND p.id=t.parent_id WHERE t.organization_id=$1 AND t.id=$2
 UNION ALL SELECT p.id,p.parent_id,p.current_run_id,p.owner_id,a.level+1 FROM tasks p JOIN authority_ancestors a ON p.id=a.parent_id WHERE p.organization_id=$1 AND a.level<16
) `

func ancestorAuthorityIDsTx(ctx context.Context, tx pgx.Tx, org, taskID string) ([]string, error) {
	var ids []string
	err := tx.QueryRow(ctx, authorityAncestorCTE+`SELECT ARRAY(SELECT DISTINCT x FROM authority_ancestors a LEFT JOIN runs r ON r.organization_id=$1 AND r.id=a.current_run_id CROSS JOIN LATERAL unnest(ARRAY[a.owner_id,r.principal_id]) x WHERE x IS NOT NULL ORDER BY x)`, org, taskID).Scan(&ids)
	return ids, err
}
func checkAncestorRunAuthorityTx(ctx context.Context, tx pgx.Tx, org, taskID string) error {
	var valid bool
	err := tx.QueryRow(ctx, authorityAncestorCTE+`SELECT NOT EXISTS(SELECT 1 FROM authority_ancestors a
 LEFT JOIN runs r ON r.organization_id=$1 AND r.id=a.current_run_id AND r.task_id=a.id
 LEFT JOIN memberships own ON own.organization_id=$1 AND own.principal_id=a.owner_id LEFT JOIN principals op ON op.id=own.principal_id
 LEFT JOIN memberships actor ON actor.organization_id=$1 AND actor.principal_id=r.principal_id LEFT JOIN principals ap ON ap.id=actor.principal_id
 LEFT JOIN organization_login_policies policy ON policy.organization_id=$1
 WHERE NOT COALESCE(r.owner_authority_epoch=own.security_epoch AND r.principal_authority_epoch=actor.security_epoch
 AND r.owner_policy_epoch=current_principal_policy_epoch($1,a.owner_id) AND r.principal_policy_epoch=current_principal_policy_epoch($1,r.principal_id)
 AND (op.kind='human' OR policy.service_access_enabled) AND (ap.kind='human' OR policy.service_access_enabled)
 AND own.active AND op.active AND own.role<>'viewer' AND actor.active AND ap.active AND actor.role<>'viewer',false)
 OR (a.level=16 AND a.parent_id IS NOT NULL))`, org, taskID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return domain.Forbidden
	}
	return nil
}
