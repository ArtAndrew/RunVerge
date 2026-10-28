// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

type configurationGrantRecord struct {
	domain.CapabilityEnvelope
	IssuerAuthorityEpoch, SubjectAuthorityEpoch int64
	IssuerPolicyEpoch, SubjectPolicyEpoch       int64
}

const configurationGrantColumns = "body,version,revoked,used_plans,used_verifications,allocated_charge_microusd,created_at,expires_at,issuer_authority_epoch,subject_authority_epoch,issuer_policy_epoch,subject_policy_epoch"

func configurationHumanManager(p domain.Principal) bool {
	return p.Kind == domain.PrincipalHuman && domain.PrincipalHasPermission(p, "configuration.manage")
}
func configurationOwner(p domain.Principal) bool {
	return configurationHumanManager(p) && p.Role == "owner"
}
func configurationMember(p domain.Principal) bool {
	return (p.Kind == domain.PrincipalHuman || p.Kind == domain.PrincipalService) && (p.Role == "member" || p.Role == "admin" || p.Role == "owner")
}
func configurationScope(d domain.ConfigurationDestination) domain.ConfigurationScope {
	return domain.ConfigurationScope{DestinationID: d.ID, DestinationVersion: d.Version, Kind: d.Kind, TargetID: d.TargetID, CredentialPurposeID: d.CredentialPurposeID, CredentialPurposeVersion: d.CredentialPurposeVersion}
}
func configurationHasAction(g domain.CapabilityEnvelope, action string) bool {
	for _, candidate := range g.Actions {
		if candidate == action {
			return true
		}
	}
	return false
}
func configurationHasScope(g domain.CapabilityEnvelope, scope domain.ConfigurationScope) bool {
	for _, candidate := range g.Scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}

func scanConfigurationGrant(row scanner) (g configurationGrantRecord, err error) {
	var raw []byte
	var version, plans, verifications int
	var revoked bool
	var allocated int64
	var created, expires time.Time
	err = row.Scan(&raw, &version, &revoked, &plans, &verifications, &allocated, &created, &expires, &g.IssuerAuthorityEpoch, &g.SubjectAuthorityEpoch, &g.IssuerPolicyEpoch, &g.SubjectPolicyEpoch)
	if err != nil {
		return g, rowExists(err)
	}
	if len(raw) > 32768 || json.Unmarshal(raw, &g.CapabilityEnvelope) != nil || g.Audience != domain.ConfigurationAudience || !configurationIdentifier.MatchString(g.ID) || !configurationIdentifier.MatchString(g.OrganizationID) || !configurationIdentifier.MatchString(g.IssuerID) || !configurationIdentifier.MatchString(g.SubjectID) || g.Depth < 0 || g.Depth > domain.MaxConfigurationGrantDepth || len(g.Actions) == 0 || len(g.Actions) > len(domain.ConfigurationActions) || len(g.Scopes) == 0 || len(g.Scopes) > domain.MaxConfigurationScopes {
		return g, configurationProfileConflict()
	}
	g.Version, g.Revoked, g.UsedPlans, g.UsedVerifications, g.AllocatedChargeMicroUSD = version, revoked, plans, verifications, allocated
	g.CreatedAt, g.ExpiresAt = created.UTC(), expires.UTC()
	g.Status, g.AvailableCommands = "active", []string{}
	if revoked {
		g.Status = "revoked"
	}
	return g, nil
}
func loadConfigurationGrantTx(ctx context.Context, tx pgx.Tx, org, id string, lock bool) (configurationGrantRecord, error) {
	q := "SELECT " + configurationGrantColumns + " FROM configuration_grants WHERE organization_id=$1 AND id=$2"
	if lock {
		q += " FOR UPDATE"
	}
	g, err := scanConfigurationGrant(tx.QueryRow(ctx, q, org, id))
	if err == nil && (g.OrganizationID != org || g.ID != id) {
		err = configurationProfileConflict()
	}
	return g, err
}

func currentConfigurationPrincipalTx(ctx context.Context, tx pgx.Tx, org, id string) (p domain.Principal, authorityEpoch, policyEpoch int64, err error) {
	err = tx.QueryRow(ctx, `SELECT m.principal_id,m.organization_id,m.role,p.kind,m.security_epoch,current_principal_policy_epoch(m.organization_id,m.principal_id)
 FROM memberships m JOIN principals p ON p.id=m.principal_id JOIN organizations o ON o.id=m.organization_id
 JOIN organization_login_policies q ON q.organization_id=m.organization_id
 WHERE m.organization_id=$1 AND m.principal_id=$2 AND m.active AND p.active AND o.active
 AND (p.kind='human' OR (p.kind='service' AND p.service_organization_id=m.organization_id AND q.service_access_enabled))`, org, id).Scan(&p.ID, &p.OrganizationID, &p.Role, &p.Kind, &authorityEpoch, &policyEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		err = domain.Forbidden
	}
	return
}

// The immutable ancestry is bounded before any authority is evaluated. The
// returned order is root to leaf, also used for monotonically shared counters.
func configurationGrantChainTx(ctx context.Context, tx pgx.Tx, org, id string, lock bool) ([]configurationGrantRecord, error) {
	chain := []configurationGrantRecord{}
	seen := map[string]bool{}
	for id != "" {
		if seen[id] || len(chain) > domain.MaxConfigurationGrantDepth {
			return nil, domain.Forbidden
		}
		seen[id] = true
		g, err := loadConfigurationGrantTx(ctx, tx, org, id, false)
		if err != nil {
			return nil, err
		}
		chain = append(chain, g)
		id = g.ParentGrantID
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	if lock {
		ids := make([]string, 0, len(chain))
		for _, g := range chain {
			ids = append(ids, g.ID)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if _, err := loadConfigurationGrantTx(ctx, tx, org, id, true); err != nil {
				return nil, err
			}
		}
		for i := range chain {
			var err error
			chain[i], err = loadConfigurationGrantTx(ctx, tx, org, chain[i].ID, false)
			if err != nil {
				return nil, err
			}
		}
	}
	return chain, nil
}

func checkConfigurationGrantChainTx(ctx context.Context, tx pgx.Tx, chain []configurationGrantRecord) error {
	for i, g := range chain {
		var timely bool
		if err := tx.QueryRow(ctx, "SELECT clock_timestamp()<$1", g.ExpiresAt).Scan(&timely); err != nil {
			return err
		}
		if !timely || g.Revoked || g.Depth != i || g.Audience != domain.ConfigurationAudience {
			return domain.Forbidden
		}
		issuer, ie, ip, err := currentConfigurationPrincipalTx(ctx, tx, g.OrganizationID, g.IssuerID)
		if err != nil {
			return err
		}
		subject, se, sp, err := currentConfigurationPrincipalTx(ctx, tx, g.OrganizationID, g.SubjectID)
		if err != nil {
			return err
		}
		if !configurationMember(subject) || ie != g.IssuerAuthorityEpoch || ip != g.IssuerPolicyEpoch || se != g.SubjectAuthorityEpoch || sp != g.SubjectPolicyEpoch {
			return domain.Forbidden
		}
		if i == 0 {
			if g.ParentGrantID != "" || !configurationOwner(issuer) {
				return domain.Forbidden
			}
			continue
		}
		parent := chain[i-1]
		if g.ParentGrantID != parent.ID || g.IssuerID != parent.SubjectID || !configurationMember(issuer) || !configurationHasAction(parent.CapabilityEnvelope, "delegate") || g.ExpiresAt.After(parent.ExpiresAt) || g.MaxPlans > parent.MaxPlans || g.MaxVerifications > parent.MaxVerifications || g.MaxAllocatedChargeMicroUSD > parent.MaxAllocatedChargeMicroUSD {
			return domain.Forbidden
		}
		for _, action := range g.Actions {
			if !configurationHasAction(parent.CapabilityEnvelope, action) {
				return domain.Forbidden
			}
		}
		for _, scope := range g.Scopes {
			if !configurationHasScope(parent.CapabilityEnvelope, scope) {
				return domain.Forbidden
			}
		}
	}
	return nil
}

func configurationAuthorityTx(ctx context.Context, tx pgx.Tx, p domain.Principal, grantID, action string, scopes []domain.ConfigurationScope) ([]configurationGrantRecord, error) {
	if !configurationMember(p) {
		return nil, domain.Forbidden
	}
	if grantID == "" {
		if !configurationHumanManager(p) {
			return nil, domain.Forbidden
		}
		return nil, nil
	}
	chain, err := configurationGrantChainTx(ctx, tx, p.OrganizationID, grantID, false)
	if err != nil {
		return nil, err
	}
	if len(chain) == 0 || chain[len(chain)-1].SubjectID != p.ID || !configurationHasAction(chain[len(chain)-1].CapabilityEnvelope, action) {
		return nil, domain.Forbidden
	}
	if err = checkConfigurationGrantChainTx(ctx, tx, chain); err != nil {
		return nil, err
	}
	for _, scope := range scopes {
		if !configurationHasScope(chain[len(chain)-1].CapabilityEnvelope, scope) {
			return nil, domain.Forbidden
		}
	}
	return chain, nil
}

func allocateConfigurationGrantTx(ctx context.Context, tx pgx.Tx, org, id string, plans, verifications int, charge int64) error {
	if id == "" {
		return nil
	}
	chain, err := configurationGrantChainTx(ctx, tx, org, id, true)
	if err != nil {
		return err
	}
	if err = checkConfigurationGrantChainTx(ctx, tx, chain); err != nil {
		return err
	}
	for _, g := range chain {
		if plans < 0 || verifications < 0 || charge < 0 || g.UsedPlans+plans > g.MaxPlans || g.UsedVerifications+verifications > g.MaxVerifications || charge > g.MaxAllocatedChargeMicroUSD-g.AllocatedChargeMicroUSD {
			return domain.Err("budget_exceeded", "Исчерпано ограничение делегированной настройки", 409)
		}
	}
	for _, g := range chain {
		if _, err = tx.Exec(ctx, `UPDATE configuration_grants SET used_plans=used_plans+$3,used_verifications=used_verifications+$4,allocated_charge_microusd=allocated_charge_microusd+$5 WHERE organization_id=$1 AND id=$2`, org, g.ID, plans, verifications, charge); err != nil {
			return err
		}
	}
	return nil
}

func configurationPreAuthorization(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
	return configurationGateTx(ctx, tx, p.OrganizationID)
}

// Bound resources keep their exact purpose/destination trust after they have
// been copied into an ordinary runtime profile. Legacy host installations have
// no binding and retain their established trusted-host admission semantics.
func checkConfigurationResourceAdmissionTx(ctx context.Context, tx pgx.Tx, org, kind, targetID string, version int) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT d.active AND p.active AND d.kind=p.kind
 FROM configuration_resource_bindings b JOIN configuration_destinations d
 ON d.organization_id=b.organization_id AND d.id=b.destination_id AND d.version=b.destination_version
 JOIN credential_purpose_profiles p ON p.organization_id=b.organization_id AND p.id=b.credential_purpose_id AND p.version=b.credential_purpose_version
 WHERE b.organization_id=$1 AND b.kind=$2 AND b.target_id=$3 AND b.resource_version=$4 FOR SHARE OF d,p`, org, kind, targetID, version).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !valid {
		return domain.Forbidden
	}
	return nil
}
