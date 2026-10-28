// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

func beginConfigurationRead(ctx context.Context, s *Store, p domain.Principal) (pgx.Tx, domain.Principal, error) {
	return beginMemberRead(ctx, s, p, false)
}

// The correlated expression is built only from fixed local SQL, never caller
// input. Scope filtering happens before LIMIT/cursor selection. Every candidate
// grant's full bounded ancestry is checked without materializing a tenant's
// complete grant catalog in application memory.
func configurationInspectScopeSQL(scope string) string {
	return configurationGrantScopeSQL("inspect", scope)
}
func configurationGrantScopeSQL(action, scope string) string {
	predicate := ""
	if scope != "" {
		predicate = " AND g.body->'scopes' @> jsonb_build_array(" + scope + ")"
	}
	return `EXISTS(WITH RECURSIVE chain AS (
 SELECT g.id AS leaf_id,g.* FROM configuration_grants g WHERE g.organization_id=$1 AND g.subject_id=$2
 AND g.body->'actions' @> '["` + action + `"]'::jsonb ` + predicate + `
 UNION ALL SELECT c.leaf_id,g.* FROM configuration_grants g JOIN chain c ON g.organization_id=c.organization_id AND g.id=c.parent_grant_id AND g.depth=c.depth-1 WHERE c.depth>0
 ) SELECT 1 FROM chain c GROUP BY leaf_id HAVING bool_or(c.depth=0 AND c.parent_grant_id IS NULL)
 AND count(*)=max(c.depth)+1 AND bool_and(NOT c.revoked AND c.expires_at>clock_timestamp() AND c.body->>'audience'='platform.configuration.v1'
 AND EXISTS(SELECT 1 FROM memberships m JOIN principals p ON p.id=m.principal_id JOIN organization_login_policies q ON q.organization_id=m.organization_id
 WHERE m.organization_id=c.organization_id AND m.principal_id=c.subject_id AND m.active AND p.active AND m.role IN('member','admin','owner')
 AND p.kind IN('human','service') AND (p.kind='human' OR (p.service_organization_id=m.organization_id AND q.service_access_enabled))
 AND m.security_epoch=c.subject_authority_epoch AND current_principal_policy_epoch(m.organization_id,m.principal_id)=c.subject_policy_epoch)
 AND EXISTS(SELECT 1 FROM memberships m JOIN principals p ON p.id=m.principal_id JOIN organization_login_policies q ON q.organization_id=m.organization_id
 WHERE m.organization_id=c.organization_id AND m.principal_id=c.issuer_id AND m.active AND p.active AND m.role IN('member','admin','owner')
 AND p.kind IN('human','service') AND (p.kind='human' OR (p.service_organization_id=m.organization_id AND q.service_access_enabled))
 AND (c.depth>0 OR (p.kind='human' AND m.role='owner'))
 AND m.security_epoch=c.issuer_authority_epoch AND current_principal_policy_epoch(m.organization_id,m.principal_id)=c.issuer_policy_epoch)))`
}

const configurationDestinationScopeSQL = "jsonb_build_object('destination_id',d.id,'destination_version',d.version,'kind',d.kind,'target_id',d.target_id,'credential_purpose_id',d.credential_purpose_id,'credential_purpose_version',d.credential_purpose_version)"
const configurationBindingScopeSQL = "jsonb_build_object('kind',b.kind,'target_id',b.target_id)"

func configurationReadPage(raw string, limit int) (cursor, error) {
	if limit < 1 || limit > 100 {
		return cursor{}, configurationProfileInvalid()
	}
	if raw == "" {
		return decodeCursor(raw)
	}
	if len(raw) > 1024 {
		return cursor{}, configurationProfileInvalid()
	}
	wire, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return cursor{}, configurationProfileInvalid()
	}
	var c cursor
	if json.Unmarshal(wire, &c) != nil || !validText(c.ID, 256) || c.CreatedAt.IsZero() || encodeCursor(c.CreatedAt, c.ID) != raw {
		return cursor{}, configurationProfileInvalid()
	}
	return c, nil
}

func (s *Store) ConfigurationCatalog(ctx context.Context, p domain.Principal) (domain.ConfigurationCatalog, error) {
	tx, p, err := beginConfigurationRead(ctx, s, p)
	if err != nil {
		return domain.ConfigurationCatalog{}, err
	}
	defer tx.Rollback(ctx)
	revision, err := configurationRevisionTx(ctx, tx, p.OrganizationID)
	if err != nil {
		return domain.ConfigurationCatalog{}, err
	}
	v := domain.ConfigurationCatalog{OrganizationID: p.OrganizationID, Revision: revision, Audience: domain.ConfigurationAudience, Actions: append([]string{}, domain.ConfigurationActions...), CanIssueRootGrants: configurationOwner(p), CanApprove: configurationOwner(p), MaxOperations: domain.MaxConfigurationOperations, MaxGrantDepth: domain.MaxConfigurationGrantDepth, AvailableCommands: []string{}}
	if configurationHumanManager(p) {
		v.AvailableCommands = append(v.AvailableCommands, "inspect", "create_plan")
	} else if configurationMember(p) {
		for _, action := range []string{"inspect", "plan"} {
			var allowed bool
			if e := tx.QueryRow(ctx, "SELECT "+configurationGrantScopeSQL(action, ""), p.OrganizationID, p.ID).Scan(&allowed); e != nil {
				return v, e
			}
			if allowed {
				command := action
				if action == "plan" {
					command = "create_plan"
				}
				v.AvailableCommands = append(v.AvailableCommands, command)
			}
		}
	}

	if v.CanIssueRootGrants {
		v.AvailableCommands = append(v.AvailableCommands, "create_grant")
	}
	return v, tx.Commit(ctx)
}
func mustConfigurationJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func requireConfigurationInspectTx(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
	if configurationHumanManager(p) {
		return nil
	}
	if !configurationMember(p) {
		return domain.Forbidden
	}
	var allowed bool
	if err := tx.QueryRow(ctx, "SELECT "+configurationGrantScopeSQL("inspect", ""), p.OrganizationID, p.ID).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return domain.Forbidden
	}
	return nil
}

func (s *Store) ConfigurationDestinations(ctx context.Context, p domain.Principal, raw string, limit int) ([]domain.ConfigurationDestination, string, error) {
	c, err := configurationReadPage(raw, limit)
	if err != nil {
		return nil, "", err
	}
	tx, p, err := beginConfigurationRead(ctx, s, p)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	if err = requireConfigurationInspectTx(ctx, tx, p); err != nil {
		return nil, "", err
	}
	allowed := "$2::text<>''"
	if !configurationHumanManager(p) {
		allowed = configurationInspectScopeSQL(configurationDestinationScopeSQL)
	}
	rows, err := tx.Query(ctx, "SELECT d.profile,d.active,d.created_at FROM configuration_destinations d WHERE d.organization_id=$1 AND (d.created_at,d.id||':'||d.version::text)<($3,$4) AND "+allowed+" ORDER BY d.created_at DESC,d.id||':'||d.version::text DESC LIMIT $5", p.OrganizationID, p.ID, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	out := []domain.ConfigurationDestination{}
	for rows.Next() {
		v, e := scanConfigurationDestination(rows)
		if e != nil {
			rows.Close()
			return nil, "", e
		}
		out = append(out, v.Destination)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID+":"+configurationVersion(last.Version))
	}
	return out, next, tx.Commit(ctx)
}
func configurationVersion(v int) string { return strconv.Itoa(v) }

func (s *Store) ConfigurationResources(ctx context.Context, p domain.Principal, raw string, limit int) ([]domain.ConfigurationResource, string, error) {
	c, err := configurationReadPage(raw, limit)
	if err != nil {
		return nil, "", err
	}
	tx, p, err := beginConfigurationRead(ctx, s, p)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	if err = requireConfigurationInspectTx(ctx, tx, p); err != nil {
		return nil, "", err
	}
	allowed := "$2::text<>''"
	if !configurationHumanManager(p) {
		allowed = configurationInspectScopeSQL(configurationBindingScopeSQL)
	}
	rows, err := tx.Query(ctx, `SELECT b.kind,b.target_id,b.resource_version,b.destination_id,b.destination_version,b.credential_purpose_id,b.credential_purpose_version,r.active,r.created_at
 FROM configuration_resource_bindings b JOIN LATERAL(SELECT active,created_at FROM model_connections WHERE b.kind='model' AND organization_id=b.organization_id AND id=b.target_id AND version=b.resource_version UNION ALL SELECT active,created_at FROM mcp_connections WHERE b.kind='mcp' AND organization_id=b.organization_id AND id=b.target_id AND version=b.resource_version) r ON true
 WHERE b.organization_id=$1 AND (r.created_at,b.kind||':'||b.target_id||':'||b.resource_version::text)<($3,$4) AND `+allowed+` ORDER BY r.created_at DESC,b.kind||':'||b.target_id||':'||b.resource_version::text DESC LIMIT $5`, p.OrganizationID, p.ID, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	out := []domain.ConfigurationResource{}
	for rows.Next() {
		v := domain.ConfigurationResource{OrganizationID: p.OrganizationID}
		if err = rows.Scan(&v.Kind, &v.ID, &v.Version, &v.Scope.DestinationID, &v.Scope.DestinationVersion, &v.Scope.CredentialPurposeID, &v.Scope.CredentialPurposeVersion, &v.Active, &v.CreatedAt); err != nil {
			rows.Close()
			return nil, "", err
		}
		v.Scope.Kind, v.Scope.TargetID = v.Kind, v.ID
		out = append(out, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		v := out[len(out)-1]
		next = encodeCursor(v.CreatedAt, v.Kind+":"+v.ID+":"+configurationVersion(v.Version))
	}
	return out, next, tx.Commit(ctx)
}

func (s *Store) ConfigurationGrant(ctx context.Context, p domain.Principal, id string) (domain.CapabilityEnvelope, error) {
	tx, p, err := beginConfigurationRead(ctx, s, p)
	if err != nil {
		return domain.CapabilityEnvelope{}, err
	}
	defer tx.Rollback(ctx)
	g, err := loadConfigurationGrantTx(ctx, tx, p.OrganizationID, id, false)
	if err != nil {
		return domain.CapabilityEnvelope{}, err
	}
	v, err := configurationGrantViewTx(ctx, tx, p, g)
	if err != nil {
		return v, err
	}
	return v, tx.Commit(ctx)
}
func (s *Store) ConfigurationGrants(ctx context.Context, p domain.Principal, raw string, limit int) ([]domain.CapabilityEnvelope, string, error) {
	c, err := configurationReadPage(raw, limit)
	if err != nil {
		return nil, "", err
	}
	tx, p, err := beginConfigurationRead(ctx, s, p)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "SELECT "+configurationGrantColumns+" FROM configuration_grants WHERE organization_id=$1 AND ($2::boolean OR issuer_id=$3 OR subject_id=$3) AND (created_at,id)<($4,$5) ORDER BY created_at DESC,id DESC LIMIT $6", p.OrganizationID, configurationHumanManager(p), p.ID, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	records := []configurationGrantRecord{}
	for rows.Next() {
		v, e := scanConfigurationGrant(rows)
		if e != nil {
			rows.Close()
			return nil, "", e
		}
		records = append(records, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(records) > limit {
		records = records[:limit]
		v := records[len(records)-1]
		next = encodeCursor(v.CreatedAt, v.ID)
	}
	out := []domain.CapabilityEnvelope{}
	for _, g := range records {
		v, e := configurationGrantViewTx(ctx, tx, p, g)
		if e != nil {
			return nil, "", e
		}
		out = append(out, v)
	}
	return out, next, tx.Commit(ctx)
}
func (s *Store) ConfigurationPlan(ctx context.Context, p domain.Principal, id string) (domain.ConfigurationPlan, error) {
	tx, p, err := beginConfigurationRead(ctx, s, p)
	if err != nil {
		return domain.ConfigurationPlan{}, err
	}
	defer tx.Rollback(ctx)
	v, err := loadConfigurationPlanTx(ctx, tx, p.OrganizationID, id, false)
	if err != nil {
		return domain.ConfigurationPlan{}, err
	}
	out, err := configurationPlanViewTx(ctx, tx, p, v)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
func (s *Store) ConfigurationPlans(ctx context.Context, p domain.Principal, raw string, limit int) ([]domain.ConfigurationPlan, string, error) {
	c, err := configurationReadPage(raw, limit)
	if err != nil {
		return nil, "", err
	}
	tx, p, err := beginConfigurationRead(ctx, s, p)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "SELECT "+configurationPlanColumns+" FROM configuration_plans WHERE organization_id=$1 AND ($2::boolean OR actor_id=$3) AND (created_at,id)<($4,$5) ORDER BY created_at DESC,id DESC LIMIT $6", p.OrganizationID, configurationHumanManager(p), p.ID, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	records := []configurationPlanRecord{}
	for rows.Next() {
		v, e := scanConfigurationPlan(rows)
		if e != nil {
			rows.Close()
			return nil, "", e
		}
		records = append(records, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(records) > limit {
		records = records[:limit]
		v := records[len(records)-1]
		next = encodeCursor(v.CreatedAt, v.ID)
	}
	out := []domain.ConfigurationPlan{}
	for _, r := range records {
		v, e := configurationPlanViewTx(ctx, tx, p, r)
		if e != nil {
			return nil, "", e
		}
		out = append(out, v)
	}
	return out, next, tx.Commit(ctx)
}

func configurationVerificationViewTx(ctx context.Context, tx pgx.Tx, p domain.Principal, v configurationVerificationRecord) (domain.ConfigurationVerification, error) {
	if !configurationHumanManager(p) && v.ActorID != p.ID {
		return domain.ConfigurationVerification{}, domain.NotFound
	}
	if v.Kind == "model" {
		var status *string
		if err := tx.QueryRow(ctx, "SELECT (SELECT status FROM reservations WHERE organization_id=$1 AND task_id=$2 AND category='model' ORDER BY created_at DESC,id DESC LIMIT 1)", v.OrganizationID, v.TaskID).Scan(&status); err != nil {
			return domain.ConfigurationVerification{}, err
		}
		v.CostStatus = "not_reserved"
		if status != nil {
			v.CostStatus = *status
		}
	}
	return v.ConfigurationVerification, nil
}
func (s *Store) ConfigurationVerification(ctx context.Context, p domain.Principal, id string) (domain.ConfigurationVerification, error) {
	tx, p, err := beginConfigurationRead(ctx, s, p)
	if err != nil {
		return domain.ConfigurationVerification{}, err
	}
	defer tx.Rollback(ctx)
	v, err := loadConfigurationVerificationTx(ctx, tx, p.OrganizationID, id, false)
	if err != nil {
		return domain.ConfigurationVerification{}, err
	}
	out, err := configurationVerificationViewTx(ctx, tx, p, v)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
func (s *Store) ConfigurationVerifications(ctx context.Context, p domain.Principal, planID, raw string, limit int) ([]domain.ConfigurationVerification, string, error) {
	c, err := configurationReadPage(raw, limit)
	if err != nil {
		return nil, "", err
	}
	tx, p, err := beginConfigurationRead(ctx, s, p)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	if planID != "" {
		v, e := loadConfigurationPlanTx(ctx, tx, p.OrganizationID, planID, false)
		if e != nil {
			return nil, "", e
		}
		if !configurationPlanReadable(p, v) {
			return nil, "", domain.NotFound
		}
	}
	rows, err := tx.Query(ctx, "SELECT "+configurationVerificationColumns+" FROM configuration_verifications WHERE organization_id=$1 AND ($2::boolean OR actor_id=$3) AND ($4::text='' OR plan_id=$4) AND (created_at,id)<($5,$6) ORDER BY created_at DESC,id DESC LIMIT $7", p.OrganizationID, configurationHumanManager(p), p.ID, planID, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	records := []configurationVerificationRecord{}
	for rows.Next() {
		v, e := scanConfigurationVerification(rows)
		if e != nil {
			rows.Close()
			return nil, "", e
		}
		records = append(records, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(records) > limit {
		records = records[:limit]
		v := records[len(records)-1]
		next = encodeCursor(v.CreatedAt, v.ID)
	}
	out := []domain.ConfigurationVerification{}
	for _, r := range records {
		v, e := configurationVerificationViewTx(ctx, tx, p, r)
		if e != nil {
			return nil, "", e
		}
		out = append(out, v)
	}
	return out, next, tx.Commit(ctx)
}

func (s *Store) ConfigurationAudit(ctx context.Context, p domain.Principal, raw string, limit int) ([]domain.ConfigurationAuditEntry, string, error) {
	c, err := configurationReadPage(raw, limit)
	if err != nil {
		return nil, "", err
	}
	tx, p, err := beginConfigurationRead(ctx, s, p)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT a.id,a.organization_id,a.actor_type,COALESCE(a.actor_id,''),a.object_type,a.object_id,a.action,a.digest,a.reason,a.created_at FROM configuration_audit a
 WHERE a.organization_id=$1 AND (a.created_at,a.id)<($4,$5) AND ($2::boolean OR a.actor_id=$3
 OR (a.object_type='grant' AND EXISTS(SELECT 1 FROM configuration_grants g WHERE g.organization_id=a.organization_id AND g.id=a.object_id AND (g.issuer_id=$3 OR g.subject_id=$3)))
 OR (a.object_type='plan' AND EXISTS(SELECT 1 FROM configuration_plans p WHERE p.organization_id=a.organization_id AND p.id=a.object_id AND p.actor_id=$3))
 OR (a.object_type='validation' AND EXISTS(SELECT 1 FROM configuration_validations x JOIN configuration_plans p ON p.organization_id=x.organization_id AND p.id=x.plan_id WHERE x.organization_id=a.organization_id AND x.id=a.object_id AND p.actor_id=$3))
 OR (a.object_type='approval' AND EXISTS(SELECT 1 FROM configuration_approvals x JOIN configuration_plans p ON p.organization_id=x.organization_id AND p.id=x.plan_id WHERE x.organization_id=a.organization_id AND x.id=a.object_id AND p.actor_id=$3))
 OR (a.object_type='verification' AND EXISTS(SELECT 1 FROM configuration_verifications v WHERE v.organization_id=a.organization_id AND v.id=a.object_id AND v.actor_id=$3))) ORDER BY a.created_at DESC,a.id DESC LIMIT $6`, p.OrganizationID, configurationHumanManager(p), p.ID, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	out := []domain.ConfigurationAuditEntry{}
	for rows.Next() {
		var v domain.ConfigurationAuditEntry
		if err = rows.Scan(&v.ID, &v.OrganizationID, &v.ActorType, &v.ActorID, &v.ObjectType, &v.ObjectID, &v.Action, &v.Digest, &v.Reason, &v.CreatedAt); err != nil {
			rows.Close()
			return nil, "", err
		}
		out = append(out, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		v := out[len(out)-1]
		next = encodeCursor(v.CreatedAt, v.ID)
	}
	return out, next, tx.Commit(ctx)
}
