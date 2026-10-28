// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// commandAuthority checks the current source of authority after business-lock waits.
// Public commands pass currentActorTx; automatic work passes its exact durable grant.
type commandAuthority func(context.Context, pgx.Tx, domain.Principal) error

const automationGrantColumns = "grant_body,status,version,stop_reason,created_at,expires_at"

func scanAutomationGrant(row scanner) (g domain.TaskAutomationGrant, err error) {
	var raw []byte
	var status, reason string
	var version int
	var created, expires time.Time
	if err = row.Scan(&raw, &status, &version, &reason, &created, &expires); err != nil {
		return g, rowExists(err)
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err = d.Decode(&g); err != nil {
		return g, automationSnapshotConflict()
	}
	g.Status, g.Version, g.StopReason, g.CreatedAt, g.ExpiresAt = status, version, reason, created.UTC(), expires.UTC()
	if !validText(g.ID, 128) || !validText(g.OrganizationID, 128) || !validText(g.TaskID, 128) || !validText(g.PrincipalID, 128) || !validText(g.OwnerID, 128) || !validText(g.AgentID, 128) || g.AgentVersion < 1 || !validText(g.ModelConnectionID, 128) || g.ModelConnectionVersion < 1 || g.Version < 1 || (g.Mode != "read_only" && g.Mode != "prepare_and_continue") || len(g.ToolPins) < 1 || len(g.ToolPins) > domain.MaxModelTools || g.MaxModelTurns < 1 || g.MaxModelTurns >= domain.MaxModelTurnsPerTask || g.MaxToolExecutions < 1 || g.MaxToolExecutions > domain.MaxToolExecutionsPerTask || g.MaxChargeMicroUSD < 1 || g.MaxChargeMicroUSD > domain.MaxMoney {
		return g, automationSnapshotConflict()
	}
	seen := map[string]bool{}
	for _, pin := range g.ToolPins {
		if !validText(pin.ToolID, 128) || pin.ToolVersion < 1 || !validDigest(pin.ToolProfileDigest) || seen[pin.ToolID] {
			return g, automationSnapshotConflict()
		}
		seen[pin.ToolID] = true
	}
	switch g.Status {
	case "active", "revoked", "expired", "stopped", "completed":
	default:
		return g, automationSnapshotConflict()
	}
	return g, nil
}
func automationSnapshotConflict() error {
	return domain.Err("conflict", "Saved automation delegation does not match its immutable sources", 409)
}
func loadAutomationGrantTx(ctx context.Context, tx pgx.Tx, org, id string, lock bool) (domain.TaskAutomationGrant, error) {
	q := "SELECT " + automationGrantColumns + " FROM task_automation_grants WHERE organization_id=$1 AND id=$2"
	if lock {
		q += " FOR UPDATE"
	}
	return scanAutomationGrant(tx.QueryRow(ctx, q, org, id))
}

// Lock principal and membership rows in stable order before any structural,
// operation or money lock. No session/token is retained or reconstructed.
func lockAutomationAuthorityTx(ctx context.Context, tx pgx.Tx, org, taskID, grantID string) error {
	var ids []string
	err := tx.QueryRow(ctx, `SELECT ARRAY(SELECT DISTINCT x FROM unnest(ARRAY[t.owner_id,g.owner_id,g.principal_id]) x WHERE x IS NOT NULL ORDER BY x) FROM tasks t LEFT JOIN task_automation_grants g ON g.organization_id=t.organization_id AND g.id=$3 AND g.task_id=t.id WHERE t.organization_id=$1 AND t.id=$2`, org, taskID, grantID).Scan(&ids)
	if err != nil {
		return rowExists(err)
	}
	ancestors, err := ancestorAuthorityIDsTx(ctx, tx, org, taskID)
	if err != nil {
		return err
	}
	return lockAuthorityPrincipalsTx(ctx, tx, org, append(ids, ancestors...))
}
func currentAutomationAuthorityTx(ctx context.Context, tx pgx.Tx, g domain.TaskAutomationGrant, task domain.Task) (domain.Principal, error) {
	var p domain.Principal
	fresh, err := loadAutomationGrantTx(ctx, tx, g.OrganizationID, g.ID, false)
	if err != nil {
		return p, err
	}
	var timely bool
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()<$1 AND clock_timestamp()<$2", fresh.ExpiresAt, task.ExecutionDeadline).Scan(&timely); err != nil {
		return p, err
	}
	if fresh.Status != "active" || !timely || fresh.TaskID != task.ID || fresh.OrganizationID != task.OrganizationID || fresh.OwnerID != task.OwnerID || fresh.AgentID != task.AgentID || fresh.AgentVersion != task.AgentVersion || fresh.ModelConnectionID != task.ModelConnectionID || fresh.ModelConnectionVersion != task.ModelConnectionVersion {
		return p, domain.Forbidden
	}
	if err = tx.QueryRow(ctx, `SELECT m.principal_id,m.organization_id,m.role,p.kind FROM memberships m JOIN principals p ON p.id=m.principal_id JOIN organizations o ON o.id=m.organization_id
 JOIN task_automation_grants g ON g.organization_id=m.organization_id AND g.id=$3 AND g.principal_id=m.principal_id
 JOIN memberships own ON own.organization_id=g.organization_id AND own.principal_id=g.owner_id
 WHERE m.organization_id=$1 AND m.principal_id=$2 AND m.active AND p.active AND o.active
 AND g.principal_authority_epoch=m.security_epoch AND g.owner_authority_epoch=own.security_epoch
 AND g.principal_policy_epoch=current_principal_policy_epoch(g.organization_id,g.principal_id) AND g.owner_policy_epoch=current_principal_policy_epoch(g.organization_id,g.owner_id)`, fresh.OrganizationID, fresh.PrincipalID, fresh.ID).Scan(&p.ID, &p.OrganizationID, &p.Role, &p.Kind); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return p, domain.Forbidden
		}
		return p, err
	}
	for _, permission := range []string{"tasks.automate_any", "tasks.continue_any", "actions.prepare_any", "actions.dispatch_any", "actions.approve_any"} {
		if !domain.PrincipalHasPermission(p, permission) {
			return p, domain.Forbidden
		}
	}
	var modelActive bool
	if err = tx.QueryRow(ctx, "SELECT active FROM model_connections WHERE organization_id=$1 AND id=$2 AND version=$3", task.OrganizationID, task.ModelConnectionID, task.ModelConnectionVersion).Scan(&modelActive); err != nil {
		return p, rowExists(err)
	}
	if !modelActive {
		return p, domain.Forbidden
	}
	if err = authorizeTaskOwner(ctx, tx, p.OrganizationID, task.OwnerID); err != nil {
		return p, err
	}
	if err = checkAncestorRunAuthorityTx(ctx, tx, task.OrganizationID, task.ID); err != nil {
		return p, err
	}
	return p, nil
}
func automationUsageTx(ctx context.Context, tx pgx.Tx, org, taskID string) (domain.AutomationUsage, error) {
	var u domain.AutomationUsage
	err := tx.QueryRow(ctx, "SELECT model_turns,tool_executions,allocated_microusd FROM task_automation_usage WHERE organization_id=$1 AND task_id=$2", org, taskID).Scan(&u.ModelTurns, &u.ToolExecutions, &u.AllocatedMicroUSD)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, nil
	}
	return u, err
}
func automationAuthority(g domain.TaskAutomationGrant, taskID string) commandAuthority {
	return func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		t, err := scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND id=$2", g.OrganizationID, taskID))
		if err != nil {
			return err
		}
		current, err := currentAutomationAuthorityTx(ctx, tx, g, t)
		if err != nil {
			return err
		}
		if current.ID != p.ID || current.OrganizationID != p.OrganizationID || current.Role != p.Role {
			return domain.Forbidden
		}
		return nil
	}
}
func allocateAutomationTx(ctx context.Context, tx pgx.Tx, g domain.TaskAutomationGrant, kind string, amount int64) error {
	if amount < 0 || amount > domain.MaxMoney {
		return automationSnapshotConflict()
	}
	if _, err := tx.Exec(ctx, "INSERT INTO task_automation_usage(organization_id,task_id) VALUES($1,$2) ON CONFLICT DO NOTHING", g.OrganizationID, g.TaskID); err != nil {
		return err
	}
	var u domain.AutomationUsage
	if err := tx.QueryRow(ctx, "SELECT model_turns,tool_executions,allocated_microusd FROM task_automation_usage WHERE organization_id=$1 AND task_id=$2 FOR UPDATE", g.OrganizationID, g.TaskID).Scan(&u.ModelTurns, &u.ToolExecutions, &u.AllocatedMicroUSD); err != nil {
		return err
	}
	if amount > g.MaxChargeMicroUSD-u.AllocatedMicroUSD {
		return domain.Err("budget_exceeded", "Automation cumulative charge ceiling exhausted", 409)
	}
	dm, dt := 0, 0
	switch kind {
	case "model":
		dm = 1
		if u.ModelTurns >= g.MaxModelTurns {
			return domain.Err("invalid_transition", "Automation lifetime model turn ceiling exhausted", 409)
		}
	case "tool":
		dt = 1
		if u.ToolExecutions >= g.MaxToolExecutions {
			return domain.Err("invalid_transition", "Automation lifetime tool ceiling exhausted", 409)
		}
	default:
		return automationSnapshotConflict()
	}
	_, err := tx.Exec(ctx, "UPDATE task_automation_usage SET model_turns=model_turns+$3,tool_executions=tool_executions+$4,allocated_microusd=allocated_microusd+$5 WHERE organization_id=$1 AND task_id=$2", g.OrganizationID, g.TaskID, dm, dt, amount)
	return err
}
func boundAutomationModelGrantTx(ctx context.Context, tx pgx.Tx, org, operationID string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, "SELECT COALESCE(automation_grant_id,'') FROM model_turns WHERE organization_id=$1 AND operation_id=$2", org, operationID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}
func checkBoundAutomationModelTx(ctx context.Context, tx pgx.Tx, t domain.Task, operationID string) error {
	id, err := boundAutomationModelGrantTx(ctx, tx, t.OrganizationID, operationID)
	if err != nil || id == "" {
		return err
	}
	g, err := loadAutomationGrantTx(ctx, tx, t.OrganizationID, id, false)
	if err != nil {
		return err
	}
	_, err = currentAutomationAuthorityTx(ctx, tx, g, t)
	return err
}
func lockModelAutomationAuthorityTx(ctx context.Context, tx pgx.Tx, org, taskID, operationID string) error {
	if err := lockConfigurationModelProbeAuthorityTx(ctx, tx, org, taskID, operationID); err != nil {
		return err
	}
	id, err := boundAutomationModelGrantTx(ctx, tx, org, operationID)
	if err != nil {
		return err
	}
	if id != "" {
		if err = lockAutomationAuthorityTx(ctx, tx, org, taskID, id); err != nil {
			return err
		}
	}
	return lockOperationAuthorityTx(ctx, tx, org, taskID, operationID)
}

func automationToolEligibility(g domain.TaskAutomationGrant, c domain.ToolConnection, automaticApproval bool) error {
	found := false
	for _, pin := range g.ToolPins {
		if pin.ToolID == c.Tool.ID && pin.ToolVersion == c.Tool.Version && pin.ToolProfileDigest == domain.ToolConnectionDigest(c) {
			found = true
			break
		}
	}
	if !found || !c.Tool.Active {
		return domain.Forbidden
	}
	if (g.Mode == "read_only" || automaticApproval) && (g.Mode != "read_only" || c.Tool.Effect != "read" || !c.Tool.AutomationAllowed || c.Tool.RequireDistinctApprover) {
		return domain.Forbidden
	}
	return nil
}
