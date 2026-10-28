// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

const loginPolicyColumns = "organization_id,mode,oidc_issuer,service_access_enabled,max_service_key_ttl_seconds,version,created_at,updated_at"

// Policy locks precede principal/membership and all execution/money locks.
// IAM writers serialize before acquiring this shared lock, then may upgrade it.
func lockOrganizationPolicyTx(ctx context.Context, tx pgx.Tx, org string) error {
	var id string
	return rowExists(tx.QueryRow(ctx, "SELECT organization_id FROM organization_login_policies WHERE organization_id=$1 FOR SHARE", org).Scan(&id))
}
func organizationIAMGate(ctx context.Context, tx pgx.Tx, org string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "organization-membership-administration\x1f"+org)
	return err
}
func scanLoginPolicy(row scanner) (v domain.OrganizationLoginPolicy, err error) {
	err = row.Scan(&v.OrganizationID, &v.Mode, &v.OIDCIssuer, &v.ServiceAccessEnabled, &v.MaxServiceKeyTTLSeconds, &v.Version, &v.CreatedAt, &v.UpdatedAt)
	v.AvailableCommands = []string{}
	v.ReadinessReasons = []string{}
	return v, rowExists(err)
}
func loadLoginPolicyTx(ctx context.Context, tx pgx.Tx, org string, update bool) (domain.OrganizationLoginPolicy, error) {
	q := "SELECT " + loginPolicyColumns + " FROM organization_login_policies WHERE organization_id=$1"
	if update {
		q += " FOR UPDATE"
	}
	return scanLoginPolicy(tx.QueryRow(ctx, q, org))
}
func validIssuer(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == "" && u.RawQuery == "" && strings.TrimSpace(s) == s && validText(s, 2048)
}
func humanAdministrator(p domain.Principal) error {
	if p.Kind != domain.PrincipalHuman || !domain.PrincipalHasPermission(p, "identity.manage") {
		return domain.Forbidden
	}
	return nil
}
func securityGate(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
	if err := humanAdministrator(p); err != nil {
		return err
	}
	return organizationIAMGate(ctx, tx, p.OrganizationID)
}
func policyOwner(p domain.Principal) error {
	if p.Kind != domain.PrincipalHuman || p.Role != "owner" || !domain.PrincipalHasPermission(p, "organization.manage") {
		return domain.Forbidden
	}
	return nil
}
func policyReadinessTx(ctx context.Context, tx pgx.Tx, p domain.Principal, runtime domain.LoginPolicyRuntime, v *domain.OrganizationLoginPolicy) error {
	v.ConfiguredOIDCIssuer = runtime.TrustedIssuer
	if runtime.TrustedIssuer == "" {
		v.ReadinessReasons = append(v.ReadinessReasons, "oidc_not_configured")
	}
	if p.Kind != domain.PrincipalHuman || p.Role != "owner" || p.CredentialKind != BrowserSessionCredential {
		v.ReadinessReasons = append(v.ReadinessReasons, "browser_owner_required")
	} else if runtime.TrustedIssuer != "" {
		var exact bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM browser_sessions t JOIN identity_mappings i ON i.issuer=t.issuer AND i.subject=t.subject AND i.principal_id=t.principal_id
   WHERE t.token_hash=$1 AND t.principal_id=$2 AND t.issuer=$3 AND NOT t.revoked AND t.expires_at>clock_timestamp() AND i.active)`, p.TokenHash, p.ID, runtime.TrustedIssuer).Scan(&exact); err != nil {
			return err
		}
		if !exact {
			v.ReadinessReasons = append(v.ReadinessReasons, "issuer_mismatch")
		}
	}
	v.CanRequireOIDC = len(v.ReadinessReasons) == 0
	if policyOwner(p) == nil {
		v.AvailableCommands = []string{"update"}
	}
	return nil
}
func policyAuditState(v domain.OrganizationLoginPolicy) domain.OrganizationSecurityAuditState {
	enabled := v.ServiceAccessEnabled
	return domain.OrganizationSecurityAuditState{Version: v.Version, Mode: v.Mode, OIDCIssuer: v.OIDCIssuer, ServiceAccessEnabled: &enabled, MaxServiceKeyTTLSeconds: v.MaxServiceKeyTTLSeconds}
}
func appendSecurityAuditTx(ctx context.Context, tx pgx.Tx, org, actorType, actorID, objectType, objectID, targetID, action, reason string, before *domain.OrganizationSecurityAuditState, after domain.OrganizationSecurityAuditState) error {
	var b []byte
	if before != nil {
		b, _ = json.Marshal(before)
	}
	a, _ := json.Marshal(after)
	_, err := tx.Exec(ctx, `INSERT INTO organization_security_audit(organization_id,id,actor_type,actor_id,object_type,object_id,target_principal_id,action,reason,before_state,after_state)
 VALUES($1,$2,$3,NULLIF($4,''),$5,$6,NULLIF($7,''),$8,$9,$10,$11)`, org, domain.NewID("sec"), actorType, actorID, objectType, objectID, targetID, action, reason, b, a)
	return err
}
func (s *Store) LoginPolicy(ctx context.Context, p domain.Principal, runtime domain.LoginPolicyRuntime) (domain.OrganizationLoginPolicy, error) {
	var empty domain.OrganizationLoginPolicy
	tx, p, err := beginMemberRead(ctx, s, p, false)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	if err = humanAdministrator(p); err != nil {
		return empty, err
	}
	v, err := loadLoginPolicyTx(ctx, tx, p.OrganizationID, false)
	if err == nil {
		err = policyReadinessTx(ctx, tx, p, runtime, &v)
	}
	if err == nil {
		err = currentActorTx(ctx, tx, p)
	}
	if err != nil {
		return empty, err
	}
	return v, tx.Commit(ctx)
}
func (s *Store) UpdateLoginPolicy(ctx context.Context, p domain.Principal, key string, in domain.UpdateOrganizationLoginPolicy, runtime domain.LoginPolicyRuntime) (json.RawMessage, error) {
	if in.ExpectedVersion < 1 || in.ServiceAccessEnabled == nil || in.MaxServiceKeyTTLSeconds < 1 || in.MaxServiceKeyTTLSeconds > 86400 || !validText(in.Reason, 4096) || (in.Mode != "mixed" && in.Mode != "oidc_required") || (in.Mode == "mixed" && in.OIDCIssuer != "") || (in.Mode == "oidc_required" && !validIssuer(in.OIDCIssuer)) {
		return nil, domain.Err("invalid_request", "Укажите полную политику входа, версию и причину", 400)
	}
	guard := func(ctx context.Context, tx pgx.Tx, p domain.Principal) error { return policyOwner(p) }
	return s.mutateWithPreAuthorization(ctx, p, "POST /login-policy/update", key, in, securityGate, guard, func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		old, err := loadLoginPolicyTx(ctx, tx, p.OrganizationID, true)
		if err != nil {
			return nil, err
		}
		if old.Version != in.ExpectedVersion {
			return nil, domain.Err("conflict", "Политика входа изменилась", 409)
		}
		loginChange := old.Mode != in.Mode || old.OIDCIssuer != in.OIDCIssuer
		if !loginChange && old.ServiceAccessEnabled == *in.ServiceAccessEnabled && old.MaxServiceKeyTTLSeconds == in.MaxServiceKeyTTLSeconds {
			return nil, domain.Err("invalid_transition", "Политика уже содержит эти значения", 409)
		}
		if loginChange {
			probe := domain.OrganizationLoginPolicy{ReadinessReasons: []string{}}
			if err = policyReadinessTx(ctx, tx, p, runtime, &probe); err != nil {
				return nil, err
			}
			if !probe.CanRequireOIDC || (in.Mode == "oidc_required" && in.OIDCIssuer != runtime.TrustedIssuer) {
				return nil, domain.Forbidden
			}
		}
		v, err := scanLoginPolicy(tx.QueryRow(ctx, `UPDATE organization_login_policies SET mode=$2,oidc_issuer=$3,service_access_enabled=$4,max_service_key_ttl_seconds=$5 WHERE organization_id=$1 RETURNING `+loginPolicyColumns, p.OrganizationID, in.Mode, in.OIDCIssuer, *in.ServiceAccessEnabled, in.MaxServiceKeyTTLSeconds))
		if err != nil {
			return nil, err
		}
		before := policyAuditState(old)
		if err = appendSecurityAuditTx(ctx, tx, p.OrganizationID, "principal", p.ID, "policy", p.OrganizationID, "", "policy.updated", in.Reason, &before, policyAuditState(v)); err != nil {
			return nil, err
		}
		if err = policyReadinessTx(ctx, tx, p, runtime, &v); err != nil {
			return nil, err
		}
		return v, nil
	})
}
func (s *Store) RecoverLoginPolicy(ctx context.Context, org, reason string) (domain.OrganizationLoginPolicy, error) {
	var empty domain.OrganizationLoginPolicy
	if !validText(org, 128) || !validText(reason, 4096) {
		return empty, domain.Err("invalid_request", "Нужны организация и причина восстановления", 400)
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	if err = organizationIAMGate(ctx, tx, org); err != nil {
		return empty, err
	}
	old, err := loadLoginPolicyTx(ctx, tx, org, true)
	if err != nil {
		return empty, err
	}
	if old.Mode == "mixed" && old.OIDCIssuer == "" {
		return empty, domain.Err("invalid_transition", "Смешанный вход уже разрешён", 409)
	}
	v, err := scanLoginPolicy(tx.QueryRow(ctx, `UPDATE organization_login_policies SET mode='mixed',oidc_issuer='' WHERE organization_id=$1 RETURNING `+loginPolicyColumns, org))
	if err != nil {
		return empty, err
	}
	before := policyAuditState(old)
	if err = appendSecurityAuditTx(ctx, tx, org, "trusted_host", "", "policy", org, "", "policy.recovered", reason, &before, policyAuditState(v)); err != nil {
		return empty, err
	}
	return v, tx.Commit(ctx)
}
