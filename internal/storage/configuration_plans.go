// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

type configurationPlanRecord struct {
	domain.ConfigurationPlan
	ActorKind, AdmittedRole               string
	ActorAuthorityEpoch, ActorPolicyEpoch int64
}

const configurationPlanColumns = "body,version,status,created_at,expires_at,applied_at,COALESCE(applied_revision,0),COALESCE(receipt_id,''),actor_kind,admitted_role,actor_authority_epoch,actor_policy_epoch"

func scanConfigurationPlan(row scanner) (v configurationPlanRecord, err error) {
	var raw []byte
	var version int
	var status, receipt string
	var created, expires time.Time
	var applied *time.Time
	var revision int64
	err = row.Scan(&raw, &version, &status, &created, &expires, &applied, &revision, &receipt, &v.ActorKind, &v.AdmittedRole, &v.ActorAuthorityEpoch, &v.ActorPolicyEpoch)
	if err != nil {
		return v, rowExists(err)
	}
	if len(raw) > 32768 || json.Unmarshal(raw, &v.ConfigurationPlan) != nil || !configurationIdentifier.MatchString(v.ID) || !configurationDigest.MatchString(v.Digest) || len(v.Operations) < 1 || len(v.Operations) > domain.MaxConfigurationOperations {
		return v, configurationProfileConflict()
	}
	v.Version, v.Status, v.CreatedAt, v.ExpiresAt = version, status, created, expires
	v.AppliedAt, v.AppliedRevision, v.ReceiptID, v.AvailableCommands = applied, revision, receipt, []string{}
	return v, nil
}
func loadConfigurationPlanTx(ctx context.Context, tx pgx.Tx, org, id string, lock bool) (configurationPlanRecord, error) {
	q := "SELECT " + configurationPlanColumns + " FROM configuration_plans WHERE organization_id=$1 AND id=$2"
	if lock {
		q += " FOR UPDATE"
	}
	v, err := scanConfigurationPlan(tx.QueryRow(ctx, q, org, id))
	if err == nil && (v.OrganizationID != org || v.ID != id) {
		err = configurationProfileConflict()
	}
	return v, err
}
func configurationPlanReadable(p domain.Principal, v configurationPlanRecord) bool {
	return configurationHumanManager(p) || v.ActorID == p.ID
}
func configurationPlanScopes(v domain.ConfigurationPlan) []domain.ConfigurationScope {
	scopes := make([]domain.ConfigurationScope, 0, len(v.Operations))
	for _, op := range v.Operations {
		scopes = append(scopes, op.Scope)
	}
	return scopes
}
func checkConfigurationPlanActorTx(ctx context.Context, tx pgx.Tx, p domain.Principal, v configurationPlanRecord, action string) error {
	if v.ActorID != p.ID {
		return domain.NotFound
	}
	_, epoch, policy, err := currentConfigurationPrincipalTx(ctx, tx, p.OrganizationID, p.ID)
	if err != nil {
		return err
	}
	if p.Kind != v.ActorKind || epoch != v.ActorAuthorityEpoch || policy != v.ActorPolicyEpoch {
		return domain.Forbidden
	}
	_, err = configurationAuthorityTx(ctx, tx, p, v.GrantID, action, configurationPlanScopes(v.ConfigurationPlan))
	return err
}
func configurationPlanGuard(id, action string) func(context.Context, pgx.Tx, domain.Principal) error {
	return func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		v, err := loadConfigurationPlanTx(ctx, tx, p.OrganizationID, id, false)
		if err != nil {
			return err
		}
		return checkConfigurationPlanActorTx(ctx, tx, p, v, action)
	}
}
func configurationRevisionTx(ctx context.Context, tx pgx.Tx, org string) (revision int64, err error) {
	err = tx.QueryRow(ctx, "SELECT revision FROM configuration_revisions WHERE organization_id=$1", org).Scan(&revision)
	return revision, rowExists(err)
}

func configurationDiffTx(ctx context.Context, tx pgx.Tx, org string, operations []domain.ConfigurationOperation) ([]domain.ConfigurationDiff, []string, error) {
	diff, reasons := []domain.ConfigurationDiff{}, []string{}
	for _, op := range operations {
		d, err := loadConfigurationDestinationTx(ctx, tx, org, op.Scope.DestinationID, op.Scope.DestinationVersion, true)
		if err != nil {
			return nil, nil, err
		}
		purpose, err := loadCredentialPurposeProfileTx(ctx, tx, org, op.Scope.CredentialPurposeID, op.Scope.CredentialPurposeVersion, true)
		if err != nil {
			return nil, nil, err
		}
		if configurationScope(d.Destination) != op.Scope {
			return nil, nil, configurationProfileConflict()
		}
		if !d.Destination.Active || !purpose.Active {
			reasons = append(reasons, "profile_inactive")
		}
		table := "model_connections"
		if op.Scope.Kind == "mcp" {
			table = "mcp_connections"
		}
		var latest int
		if err = tx.QueryRow(ctx, "SELECT COALESCE(max(version),0) FROM "+table+" WHERE organization_id=$1 AND id=$2", org, op.Scope.TargetID).Scan(&latest); err != nil {
			return nil, nil, err
		}
		if latest != op.ExpectedResourceVersion {
			reasons = append(reasons, "resource_version_changed")
		}
		retired := []int{}
		// A public operation replaces the logical slot even when disabling it.
		{
			rows, e := tx.Query(ctx, "SELECT version FROM "+table+" WHERE organization_id=$1 AND id=$2 AND active ORDER BY version", org, op.Scope.TargetID)
			if e != nil {
				return nil, nil, e
			}
			for rows.Next() {
				var version int
				if e = rows.Scan(&version); e != nil {
					break
				}
				retired = append(retired, version)
			}
			if e == nil {
				e = rows.Err()
			}
			rows.Close()
			if e != nil {
				return nil, nil, e
			}
			if op.Active && d.Model != nil {
				var collision bool
				if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM model_connections WHERE organization_id=$1 AND model=$2 AND id<>$3 AND active)", org, d.Model.Model, op.Scope.TargetID).Scan(&collision); err != nil {
					return nil, nil, err
				}
				if collision {
					reasons = append(reasons, "model_name_collision")
				}
			}
		}
		diff = append(diff, domain.ConfigurationDiff{OperationID: op.ID, Kind: op.Scope.Kind, TargetID: op.Scope.TargetID, ExpectedVersion: op.ExpectedResourceVersion, NewVersion: op.ExpectedResourceVersion + 1, Active: op.Active, RetiredVersions: retired, MayStopPinnedTasks: op.Scope.Kind == "model" && len(retired) != 0})
	}
	return diff, reasons, nil
}

func (s *Store) CreateConfigurationPlan(ctx context.Context, p domain.Principal, key string, in domain.CreateConfigurationPlan) (json.RawMessage, error) {
	if err := domain.ValidateCreateConfigurationPlan(in); err != nil {
		return nil, err
	}
	guard := func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		_, err := configurationAuthorityTx(ctx, tx, p, in.GrantID, "plan", nil)
		return err
	}
	return s.mutateWithPreAuthorization(ctx, p, "POST /configuration/plans", key, in, configurationPreAuthorization, guard, func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		if err := lockConfigurationOrganizationTx(ctx, tx, p.OrganizationID); err != nil {
			return nil, err
		}
		revision, err := configurationRevisionTx(ctx, tx, p.OrganizationID)
		if err != nil {
			return nil, err
		}
		if revision != in.BaseRevision {
			return nil, configurationProfileConflict()
		}
		_, epoch, policy, err := currentConfigurationPrincipalTx(ctx, tx, p.OrganizationID, p.ID)
		if err != nil {
			return nil, err
		}
		var now time.Time
		if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
			return nil, err
		}
		v := domain.ConfigurationPlan{ID: domain.NewID("cfg_plan"), OrganizationID: p.OrganizationID, ActorID: p.ID, GrantID: in.GrantID, Version: 1, Status: "planned", BaseRevision: revision, Operations: []domain.ConfigurationOperation{}, Diff: []domain.ConfigurationDiff{}, Reason: in.Reason, CreatedAt: now, ExpiresAt: now.Add(time.Duration(in.TTLSeconds) * time.Second), AvailableCommands: []string{}}
		slots := map[string]bool{}
		for _, op := range in.Operations {
			d, e := loadConfigurationDestinationTx(ctx, tx, p.OrganizationID, op.DestinationID, op.DestinationVersion, true)
			if e != nil {
				return nil, e
			}
			scope := configurationScope(d.Destination)
			if slots[scope.Kind+"\x1f"+scope.TargetID] {
				return nil, configurationProfileInvalid()
			}
			slots[scope.Kind+"\x1f"+scope.TargetID] = true
			v.Operations = append(v.Operations, domain.ConfigurationOperation{ID: op.ID, Scope: scope, ExpectedResourceVersion: op.ExpectedResourceVersion, Active: *op.Active})
		}
		if _, err = configurationAuthorityTx(ctx, tx, p, in.GrantID, "plan", configurationPlanScopes(v)); err != nil {
			return nil, err
		}
		var reasons []string
		v.Diff, reasons, err = configurationDiffTx(ctx, tx, p.OrganizationID, v.Operations)
		if err != nil {
			return nil, err
		}
		if len(reasons) != 0 {
			return nil, configurationProfileConflict()
		}
		if v.GrantID != "" {
			chain, e := configurationGrantChainTx(ctx, tx, p.OrganizationID, v.GrantID, false)
			if e != nil {
				return nil, e
			}
			for _, g := range chain {
				if g.ExpiresAt.Before(v.ExpiresAt) {
					v.ExpiresAt = g.ExpiresAt
				}
			}
		}
		v.Digest = digestValue(struct {
			OrganizationID, ActorID, GrantID string
			BaseRevision                     int64
			ExpiresAt                        time.Time
			Operations                       []domain.ConfigurationOperation
			Reason                           string
		}{v.OrganizationID, v.ActorID, v.GrantID, v.BaseRevision, v.ExpiresAt, v.Operations, v.Reason})
		if err = allocateConfigurationGrantTx(ctx, tx, p.OrganizationID, in.GrantID, 1, 0, 0); err != nil {
			return nil, err
		}
		body, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		created, err := scanConfigurationPlan(tx.QueryRow(ctx, `INSERT INTO configuration_plans(organization_id,id,actor_id,grant_id,actor_kind,admitted_role,actor_authority_epoch,actor_policy_epoch,base_revision,digest,body,created_at,expires_at)
 VALUES($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING `+configurationPlanColumns, v.OrganizationID, v.ID, v.ActorID, v.GrantID, p.Kind, p.Role, epoch, policy, v.BaseRevision, v.Digest, body, v.CreatedAt, v.ExpiresAt))
		if err != nil {
			return nil, err
		}
		if err = checkConfigurationPlanActorTx(ctx, tx, p, created, "plan"); err != nil {
			return nil, err
		}
		if err = appendConfigurationAuditTx(ctx, tx, p.OrganizationID, "principal", p.ID, "plan", v.ID, "plan.created", v.Digest, in.Reason); err != nil {
			return nil, err
		}
		return configurationPlanViewTx(ctx, tx, p, created)
	})
}

func configurationValidationTx(ctx context.Context, tx pgx.Tx, p domain.Principal, v configurationPlanRecord) (domain.ConfigurationValidation, error) {
	revision, err := configurationRevisionTx(ctx, tx, p.OrganizationID)
	if err != nil {
		return domain.ConfigurationValidation{}, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return domain.ConfigurationValidation{}, err
	}
	_, reasons, err := configurationDiffTx(ctx, tx, p.OrganizationID, v.Operations)
	if err != nil {
		return domain.ConfigurationValidation{}, err
	}
	if !now.Before(v.ExpiresAt) {
		reasons = append(reasons, "plan_expired")
	}
	if revision != v.BaseRevision {
		reasons = append(reasons, "configuration_revision_changed")
	}
	if v.Status != "planned" {
		reasons = append(reasons, "plan_already_applied")
	}
	obligations := []string{}
	for _, op := range v.Operations {
		d, e := loadConfigurationDestinationTx(ctx, tx, p.OrganizationID, op.Scope.DestinationID, op.Scope.DestinationVersion, false)
		if e != nil {
			return domain.ConfigurationValidation{}, e
		}
		if d.Destination.RequiresApproval && len(obligations) == 0 {
			obligations = append(obligations, "owner_approval")
		}
	}
	outcome := "allow"
	if len(reasons) > 0 {
		outcome = "deny"
	} else if len(obligations) > 0 {
		outcome = "requires_approval"
	}
	return domain.ConfigurationValidation{ID: domain.NewID("cfg_validation"), OrganizationID: v.OrganizationID, PlanID: v.ID, PlanVersion: v.Version, PlanDigest: v.Digest, BaseRevision: v.BaseRevision, CurrentRevision: revision, Valid: len(reasons) == 0, Diff: append([]domain.ConfigurationDiff{}, v.Diff...), CreatedAt: now, Decision: domain.PolicyDecision{ID: domain.NewID("cfg_decision"), OrganizationID: v.OrganizationID, Action: "apply", TargetID: v.ID, Outcome: outcome, ReasonCodes: reasons, GrantID: v.GrantID, ConfigurationRevision: revision, Obligations: obligations, EvaluatedAt: now}}, nil
}
func loadConfigurationValidationTx(ctx context.Context, tx pgx.Tx, org, id string) (v domain.ConfigurationValidation, err error) {
	var raw []byte
	err = tx.QueryRow(ctx, "SELECT body FROM configuration_validations WHERE organization_id=$1 AND id=$2", org, id).Scan(&raw)
	if err != nil {
		return v, rowExists(err)
	}
	if len(raw) > 32768 || json.Unmarshal(raw, &v) != nil || v.ID != id || v.OrganizationID != org {
		return v, configurationProfileConflict()
	}
	return v, nil
}

func (s *Store) ValidateConfigurationPlan(ctx context.Context, p domain.Principal, id, key string, in domain.ValidateConfigurationPlan) (json.RawMessage, error) {
	if err := domain.ValidateConfigurationSnapshot(in.ExpectedVersion, in.PlanDigest); err != nil {
		return nil, err
	}
	return s.mutateWithPreAuthorization(ctx, p, "POST /configuration/plans/"+id+"/validate", key, in, configurationPreAuthorization, configurationPlanGuard(id, "validate"), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		if err := lockConfigurationOrganizationTx(ctx, tx, p.OrganizationID); err != nil {
			return nil, err
		}
		v, err := loadConfigurationPlanTx(ctx, tx, p.OrganizationID, id, false)
		if err != nil {
			return nil, err
		}
		if v.Version != in.ExpectedVersion || v.Digest != in.PlanDigest {
			return nil, configurationProfileConflict()
		}
		result, err := configurationValidationTx(ctx, tx, p, v)
		if err != nil {
			return nil, err
		}
		if err = checkConfigurationPlanActorTx(ctx, tx, p, v, "validate"); err != nil {
			return nil, err
		}
		body, _ := json.Marshal(result)
		if _, err = tx.Exec(ctx, "INSERT INTO configuration_validations(organization_id,id,plan_id,body) VALUES($1,$2,$3,$4)", p.OrganizationID, result.ID, id, body); err != nil {
			return nil, err
		}
		return result, appendConfigurationAuditTx(ctx, tx, p.OrganizationID, "principal", p.ID, "validation", result.ID, "plan.validated", Hash(body), v.Reason)
	})
}

func (s *Store) ApproveConfigurationPlan(ctx context.Context, p domain.Principal, id, key string, in domain.ApproveConfigurationPlan) (json.RawMessage, error) {
	if err := domain.ValidateConfigurationSnapshot(in.ExpectedVersion, in.PlanDigest); err != nil {
		return nil, err
	}
	if err := domain.ValidateConfigurationReason(in.Reason); err != nil {
		return nil, err
	}
	guard := func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		if !configurationOwner(p) {
			return domain.Forbidden
		}
		_, err := loadConfigurationPlanTx(ctx, tx, p.OrganizationID, id, false)
		return err
	}
	return s.mutateWithPreAuthorization(ctx, p, "POST /configuration/plans/"+id+"/approve", key, in, configurationPreAuthorization, guard, func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		if err := lockConfigurationOrganizationTx(ctx, tx, p.OrganizationID); err != nil {
			return nil, err
		}
		v, err := loadConfigurationPlanTx(ctx, tx, p.OrganizationID, id, false)
		if err != nil {
			return nil, err
		}
		validation, err := loadConfigurationValidationTx(ctx, tx, p.OrganizationID, in.ValidationID)
		if err != nil {
			return nil, err
		}
		if v.Version != in.ExpectedVersion || v.Digest != in.PlanDigest || validation.PlanID != id || validation.PlanDigest != v.Digest || validation.PlanVersion != v.Version || !validation.Valid || v.Status != "planned" {
			return nil, configurationProfileConflict()
		}
		actor, _, _, err := currentConfigurationPrincipalTx(ctx, tx, p.OrganizationID, v.ActorID)
		if err != nil {
			return nil, err
		}
		if err = checkConfigurationPlanActorTx(ctx, tx, actor, v, "apply"); err != nil {
			return nil, err
		}
		fresh, err := configurationValidationTx(ctx, tx, p, v)
		if err != nil {
			return nil, err
		}
		if !fresh.Valid {
			return nil, configurationProfileConflict()
		}
		_, epoch, policy, err := currentConfigurationPrincipalTx(ctx, tx, p.OrganizationID, p.ID)
		if err != nil {
			return nil, err
		}
		result := domain.ConfigurationApproval{ID: domain.NewID("cfg_approval"), OrganizationID: p.OrganizationID, PlanID: id, PlanDigest: v.Digest, ValidationID: validation.ID, PrincipalID: p.ID, Reason: in.Reason, CreatedAt: fresh.CreatedAt}
		body, _ := json.Marshal(result)
		if _, err = tx.Exec(ctx, "INSERT INTO configuration_approvals(organization_id,id,plan_id,validation_id,principal_id,authority_epoch,policy_epoch,body) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", p.OrganizationID, result.ID, id, validation.ID, p.ID, epoch, policy, body); err != nil {
			return nil, err
		}
		return result, appendConfigurationAuditTx(ctx, tx, p.OrganizationID, "principal", p.ID, "approval", result.ID, "plan.approved", Hash(body), in.Reason)
	})
}

func installMCPConnectionTx(ctx context.Context, tx pgx.Tx, org, id string, version int, profile domain.MCPConnectionProfile, active bool) error {
	if err := validateMCPConnectionProfile(profile); err != nil {
		return err
	}
	if active {
		if _, err := tx.Exec(ctx, "UPDATE mcp_connections SET active=false WHERE organization_id=$1 AND id=$2 AND active", org, id); err != nil {
			return err
		}
	}
	body, _ := json.Marshal(profile)
	_, err := tx.Exec(ctx, "INSERT INTO mcp_connections(organization_id,id,version,profile,active) VALUES($1,$2,$3,$4,$5)", org, id, version, body, active)
	return err
}

func (s *Store) ApplyConfigurationPlan(ctx context.Context, p domain.Principal, id, key string, in domain.ApplyConfigurationPlan) (json.RawMessage, error) {
	if err := domain.ValidateConfigurationSnapshot(in.ExpectedVersion, in.PlanDigest); err != nil {
		return nil, err
	}
	if err := domain.ValidateConfigurationReason(in.Reason); err != nil {
		return nil, err
	}
	return s.mutateWithPreAuthorization(ctx, p, "POST /configuration/plans/"+id+"/apply", key, in, configurationPreAuthorization, configurationPlanGuard(id, "apply"), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		if err := lockConfigurationOrganizationTx(ctx, tx, p.OrganizationID); err != nil {
			return nil, err
		}
		v, err := loadConfigurationPlanTx(ctx, tx, p.OrganizationID, id, true)
		if err != nil {
			return nil, err
		}
		validation, err := loadConfigurationValidationTx(ctx, tx, p.OrganizationID, in.ValidationID)
		if err != nil {
			return nil, err
		}
		if in.PlanDigest != v.Digest || validation.PlanID != id || validation.PlanDigest != v.Digest || validation.PlanVersion != in.ExpectedVersion || !validation.Valid {
			return nil, configurationProfileConflict()
		}
		if v.Status == "applied" && v.Version == in.ExpectedVersion+1 {
			return configurationPlanViewTx(ctx, tx, p, v)
		}
		if v.Version != in.ExpectedVersion {
			return nil, configurationProfileConflict()
		}
		fresh, err := configurationValidationTx(ctx, tx, p, v)
		if err != nil {
			return nil, err
		}
		if !fresh.Valid || validation.BaseRevision != v.BaseRevision || validation.CurrentRevision != fresh.CurrentRevision {
			return nil, configurationProfileConflict()
		}
		if len(fresh.Decision.Obligations) > 0 {
			var approved bool
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM configuration_approvals a JOIN memberships m ON m.organization_id=a.organization_id AND m.principal_id=a.principal_id JOIN principals p ON p.id=m.principal_id
 WHERE a.organization_id=$1 AND a.plan_id=$2 AND a.validation_id=$3 AND m.active AND p.active AND p.kind='human' AND m.role='owner'
 AND a.authority_epoch=m.security_epoch AND a.policy_epoch=current_principal_policy_epoch(a.organization_id,a.principal_id))`, p.OrganizationID, id, validation.ID).Scan(&approved)
			if err != nil {
				return nil, err
			}
			if !approved {
				return nil, domain.Forbidden
			}
		}
		for _, op := range v.Operations {
			d, e := loadConfigurationDestinationTx(ctx, tx, p.OrganizationID, op.Scope.DestinationID, op.Scope.DestinationVersion, true)
			if e != nil {
				return nil, e
			}
			purpose, e := loadCredentialPurposeProfileTx(ctx, tx, p.OrganizationID, op.Scope.CredentialPurposeID, op.Scope.CredentialPurposeVersion, true)
			if e != nil {
				return nil, e
			}
			table := "model_connections"
			if op.Scope.Kind == "mcp" {
				table = "mcp_connections"
			}
			if _, e = tx.Exec(ctx, "UPDATE "+table+" SET active=false WHERE organization_id=$1 AND id=$2 AND active", p.OrganizationID, op.Scope.TargetID); e != nil {
				return nil, e
			}
			if d.Model != nil {
				model := *d.Model
				model.Version, model.Active, model.CredentialEnvironment = op.ExpectedResourceVersion+1, op.Active, purpose.CredentialEnvironment
				if _, e = installModelConnectionTx(ctx, tx, model); e != nil {
					return nil, e
				}
			} else if e = installMCPConnectionTx(ctx, tx, p.OrganizationID, op.Scope.TargetID, op.ExpectedResourceVersion+1, *d.MCP, op.Active); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(ctx, `INSERT INTO configuration_resource_bindings(organization_id,kind,target_id,resource_version,destination_id,destination_version,credential_purpose_id,credential_purpose_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, p.OrganizationID, op.Scope.Kind, op.Scope.TargetID, op.ExpectedResourceVersion+1, op.Scope.DestinationID, op.Scope.DestinationVersion, op.Scope.CredentialPurposeID, op.Scope.CredentialPurposeVersion); e != nil {
				return nil, e
			}
		}
		if err = checkConfigurationPlanActorTx(ctx, tx, p, v, "apply"); err != nil {
			return nil, err
		}
		revision, err := configurationRevisionTx(ctx, tx, p.OrganizationID)
		if err != nil {
			return nil, err
		}
		v, err = scanConfigurationPlan(tx.QueryRow(ctx, "UPDATE configuration_plans SET status='applied',applied_revision=$3,receipt_id=$4 WHERE organization_id=$1 AND id=$2 RETURNING "+configurationPlanColumns, p.OrganizationID, id, revision, domain.NewID("cfg_receipt")))
		if err != nil {
			return nil, err
		}
		if err = appendConfigurationAuditTx(ctx, tx, p.OrganizationID, "principal", p.ID, "plan", id, "plan.applied", v.Digest, in.Reason); err != nil {
			return nil, err
		}
		return configurationPlanViewTx(ctx, tx, p, v)
	})
}

func configurationPlanViewTx(ctx context.Context, tx pgx.Tx, p domain.Principal, v configurationPlanRecord) (domain.ConfigurationPlan, error) {
	if !configurationPlanReadable(p, v) {
		return domain.ConfigurationPlan{}, domain.NotFound
	}
	v.AvailableCommands = []string{}
	var expired bool
	if v.Status == "planned" {
		if err := tx.QueryRow(ctx, "SELECT clock_timestamp()>=$1", v.ExpiresAt).Scan(&expired); err != nil {
			return domain.ConfigurationPlan{}, err
		}
	}
	if expired {
		v.Status = "expired"
		return v.ConfigurationPlan, nil
	}
	if v.ActorID == p.ID {
		for _, action := range []string{"validate", "apply", "verify"} {
			if (action == "verify") != (v.Status == "applied") {
				continue
			}
			if err := checkConfigurationPlanActorTx(ctx, tx, p, v, action); err == nil {
				v.AvailableCommands = append(v.AvailableCommands, action)
			} else if !errors.Is(err, domain.Forbidden) && !errors.Is(err, domain.NotFound) {
				return domain.ConfigurationPlan{}, err
			}
		}
	}
	if configurationOwner(p) && v.Status == "planned" {
		v.AvailableCommands = append(v.AvailableCommands, "approve")
	}
	return v.ConfigurationPlan, nil
}
