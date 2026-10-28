// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

const serviceAccountColumns = "m.organization_id,m.principal_id,p.kind,m.display_name,m.role,m.active,p.active,m.version,m.created_at,m.updated_at,m.credentials_revoked_at"
const serviceAccountFrom = " FROM memberships m JOIN principals p ON p.id=m.principal_id WHERE p.kind='service' AND p.service_organization_id=m.organization_id"
const serviceKeyColumns = "id,organization_id,principal_id,role_ceiling,version,revoked,created_at,expires_at,CASE WHEN revoked THEN 'revoked' WHEN expires_at<=clock_timestamp() THEN 'expired' ELSE 'active' END"

func validServiceRole(role string) bool { return role == "member" || role == "viewer" }
func scanServiceAccount(row scanner) (v domain.ServiceAccount, err error) {
	err = row.Scan(&v.OrganizationID, &v.PrincipalID, &v.PrincipalKind, &v.DisplayName, &v.Role, &v.Active, &v.PrincipalActive, &v.Version, &v.CreatedAt, &v.UpdatedAt, &v.CredentialsRevokedAt)
	v.AvailableCommands = []string{"update"}
	v.AssignableRoles = []string{"member", "viewer"}
	return v, rowExists(err)
}
func loadServiceAccountTx(ctx context.Context, tx pgx.Tx, org, id string, lock bool) (domain.ServiceAccount, error) {
	q := "SELECT " + serviceAccountColumns + serviceAccountFrom + " AND m.organization_id=$1 AND m.principal_id=$2"
	if lock {
		q += " FOR UPDATE OF m"
	}
	return scanServiceAccount(tx.QueryRow(ctx, q, org, id))
}
func serviceAccountAuditState(v domain.ServiceAccount) domain.OrganizationSecurityAuditState {
	active := v.Active
	return domain.OrganizationSecurityAuditState{Version: v.Version, DisplayName: v.DisplayName, Role: v.Role, Active: &active}
}
func beginSecurityRead(ctx context.Context, s *Store, p domain.Principal) (pgx.Tx, domain.Principal, error) {
	tx, p, err := beginMemberRead(ctx, s, p, false)
	if err != nil {
		return nil, p, err
	}
	if err = humanAdministrator(p); err != nil {
		_ = tx.Rollback(ctx)
		return nil, p, err
	}
	return tx, p, nil
}
func serviceAdminGuard(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
	return humanAdministrator(p)
}
func (s *Store) ServiceAccount(ctx context.Context, p domain.Principal, id string) (domain.ServiceAccount, error) {
	var empty domain.ServiceAccount
	if !validText(id, 128) {
		return empty, domain.Err("invalid_request", "Нужен сервисный субъект", 400)
	}
	tx, p, err := beginSecurityRead(ctx, s, p)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	v, err := loadServiceAccountTx(ctx, tx, p.OrganizationID, id, false)
	if err == nil {
		err = currentActorTx(ctx, tx, p)
	}
	if err != nil {
		return empty, err
	}
	return v, tx.Commit(ctx)
}
func (s *Store) ServiceAccounts(ctx context.Context, p domain.Principal, raw string, limit int) ([]domain.ServiceAccount, string, error) {
	if limit < 1 || limit > 100 {
		return nil, "", domain.Err("invalid_request", "limit должен быть от 1 до 100", 400)
	}
	c, err := decodeCursor(raw)
	if err != nil {
		return nil, "", err
	}
	tx, p, err := beginSecurityRead(ctx, s, p)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "SELECT "+serviceAccountColumns+serviceAccountFrom+" AND m.organization_id=$1 AND (m.created_at,m.principal_id)<($2,$3) ORDER BY m.created_at DESC,m.principal_id DESC LIMIT $4", p.OrganizationID, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	out := []domain.ServiceAccount{}
	for rows.Next() {
		v, e := scanServiceAccount(rows)
		if e != nil {
			rows.Close()
			return nil, "", e
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
		v := out[limit-1]
		next = encodeCursor(v.CreatedAt, v.PrincipalID)
	}
	if err = currentActorTx(ctx, tx, p); err != nil {
		return nil, "", err
	}
	return out, next, tx.Commit(ctx)
}
func (s *Store) CreateServiceAccount(ctx context.Context, p domain.Principal, key string, in domain.CreateServiceAccount) (json.RawMessage, error) {
	if !validText(in.DisplayName, 120) || !validServiceRole(in.Role) || !validText(in.Reason, 4096) {
		return nil, domain.Err("invalid_request", "Укажите имя, роль member/viewer и причину", 400)
	}
	return s.mutateWithPreAuthorization(ctx, p, "POST /service-accounts", key, in, securityGate, serviceAdminGuard, func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		id := domain.NewID("svc")
		if _, err := tx.Exec(ctx, "INSERT INTO principals(id,name,kind,service_organization_id) VALUES($1,'Organization service account','service',$2)", id, p.OrganizationID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO memberships(organization_id,principal_id,display_name,role) VALUES($1,$2,$3,$4)", p.OrganizationID, id, in.DisplayName, in.Role); err != nil {
			return nil, err
		}
		v, err := loadServiceAccountTx(ctx, tx, p.OrganizationID, id, false)
		if err != nil {
			return nil, err
		}
		return v, appendSecurityAuditTx(ctx, tx, p.OrganizationID, "principal", p.ID, "service_account", id, id, "service_account.created", in.Reason, nil, serviceAccountAuditState(v))
	})
}
func (s *Store) UpdateServiceAccount(ctx context.Context, p domain.Principal, id, key string, in domain.UpdateServiceAccount) (json.RawMessage, error) {
	if !validText(id, 128) || in.ExpectedVersion < 1 || !validText(in.DisplayName, 120) || !validServiceRole(in.Role) || in.Active == nil || !validText(in.Reason, 4096) {
		return nil, domain.Err("invalid_request", "Нужны версия, имя, роль member/viewer, active и причина", 400)
	}
	guard := func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		if err := humanAdministrator(p); err != nil {
			return err
		}
		_, err := loadServiceAccountTx(ctx, tx, p.OrganizationID, id, false)
		return err
	}
	return s.mutateWithPreAuthorization(ctx, p, "POST /service-accounts/"+id+"/update", key, in, securityGate, guard, func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		old, err := loadServiceAccountTx(ctx, tx, p.OrganizationID, id, true)
		if err != nil {
			return nil, err
		}
		if old.Version != in.ExpectedVersion {
			return nil, domain.Err("conflict", "Сервисный субъект изменился", 409)
		}
		if old.DisplayName == in.DisplayName && old.Role == in.Role && old.Active == *in.Active {
			return nil, domain.Err("invalid_transition", "Изменений нет", 409)
		}
		if _, err = tx.Exec(ctx, "UPDATE memberships SET display_name=$3,role=$4,active=$5 WHERE organization_id=$1 AND principal_id=$2", p.OrganizationID, id, in.DisplayName, in.Role, *in.Active); err != nil {
			return nil, err
		}
		v, err := loadServiceAccountTx(ctx, tx, p.OrganizationID, id, false)
		if err != nil {
			return nil, err
		}
		before := serviceAccountAuditState(old)
		return v, appendSecurityAuditTx(ctx, tx, p.OrganizationID, "principal", p.ID, "service_account", id, id, "service_account.updated", in.Reason, &before, serviceAccountAuditState(v))
	})
}
func scanServiceKey(row scanner) (v domain.ServiceKey, err error) {
	err = row.Scan(&v.ID, &v.OrganizationID, &v.PrincipalID, &v.RoleCeiling, &v.Version, &v.Revoked, &v.CreatedAt, &v.ExpiresAt, &v.Status)
	v.AvailableCommands = []string{}
	if err == nil && !v.Revoked {
		v.AvailableCommands = []string{"revoke"}
	}
	return v, rowExists(err)
}
func serviceKeyAuditState(v domain.ServiceKey) domain.OrganizationSecurityAuditState {
	revoked := v.Revoked
	expires := v.ExpiresAt
	return domain.OrganizationSecurityAuditState{Version: v.Version, RoleCeiling: v.RoleCeiling, ExpiresAt: &expires, Revoked: &revoked}
}
func loadServiceKeyTx(ctx context.Context, tx pgx.Tx, org, id, keyID string, lock bool) (domain.ServiceKey, error) {
	q := "SELECT " + serviceKeyColumns + " FROM service_credentials WHERE organization_id=$1 AND principal_id=$2 AND id=$3"
	if lock {
		q += " FOR UPDATE"
	}
	return scanServiceKey(tx.QueryRow(ctx, q, org, id, keyID))
}
func (s *Store) ServiceKey(ctx context.Context, p domain.Principal, id, keyID string) (domain.ServiceKey, error) {
	var empty domain.ServiceKey
	if !validText(id, 128) || !validText(keyID, 128) {
		return empty, domain.Err("invalid_request", "Нужны субъект и ключ", 400)
	}
	tx, p, err := beginSecurityRead(ctx, s, p)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	if _, err = loadServiceAccountTx(ctx, tx, p.OrganizationID, id, false); err != nil {
		return empty, err
	}
	v, err := loadServiceKeyTx(ctx, tx, p.OrganizationID, id, keyID, false)
	if err == nil {
		err = currentActorTx(ctx, tx, p)
	}
	if err != nil {
		return empty, err
	}
	return v, tx.Commit(ctx)
}
func (s *Store) ServiceKeys(ctx context.Context, p domain.Principal, id, raw string, limit int) ([]domain.ServiceKey, string, error) {
	if !validText(id, 128) || limit < 1 || limit > 100 {
		return nil, "", domain.Err("invalid_request", "Нужны субъект и limit от 1 до 100", 400)
	}
	c, err := decodeCursor(raw)
	if err != nil {
		return nil, "", err
	}
	tx, p, err := beginSecurityRead(ctx, s, p)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	if _, err = loadServiceAccountTx(ctx, tx, p.OrganizationID, id, false); err != nil {
		return nil, "", err
	}
	rows, err := tx.Query(ctx, "SELECT "+serviceKeyColumns+" FROM service_credentials WHERE organization_id=$1 AND principal_id=$2 AND (created_at,id)<($3,$4) ORDER BY created_at DESC,id DESC LIMIT $5", p.OrganizationID, id, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	out := []domain.ServiceKey{}
	for rows.Next() {
		v, e := scanServiceKey(rows)
		if e != nil {
			rows.Close()
			return nil, "", e
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
		v := out[limit-1]
		next = encodeCursor(v.CreatedAt, v.ID)
	}
	if err = currentActorTx(ctx, tx, p); err != nil {
		return nil, "", err
	}
	return out, next, tx.Commit(ctx)
}
func (s *Store) RevokeServiceKey(ctx context.Context, p domain.Principal, id, keyID, key string, in domain.RevokeServiceKey) (json.RawMessage, error) {
	if !validText(id, 128) || !validText(keyID, 128) || in.ExpectedVersion < 1 || !validText(in.Reason, 4096) {
		return nil, domain.Err("invalid_request", "Нужны субъект, ключ, версия и причина", 400)
	}
	guard := func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		if err := humanAdministrator(p); err != nil {
			return err
		}
		_, err := loadServiceKeyTx(ctx, tx, p.OrganizationID, id, keyID, false)
		return err
	}
	return s.mutateWithPreAuthorization(ctx, p, "POST /service-accounts/"+id+"/keys/"+keyID+"/revoke", key, in, securityGate, guard, func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		old, err := loadServiceKeyTx(ctx, tx, p.OrganizationID, id, keyID, true)
		if err != nil {
			return nil, err
		}
		if old.Version != in.ExpectedVersion {
			return nil, domain.Err("conflict", "Ключ изменился", 409)
		}
		if old.Revoked {
			return nil, domain.Err("invalid_transition", "Ключ уже отозван", 409)
		}
		if _, err = tx.Exec(ctx, "UPDATE service_credentials SET revoked=true WHERE organization_id=$1 AND id=$2", p.OrganizationID, keyID); err != nil {
			return nil, err
		}
		v, err := loadServiceKeyTx(ctx, tx, p.OrganizationID, id, keyID, false)
		if err != nil {
			return nil, err
		}
		before := serviceKeyAuditState(old)
		return v, appendSecurityAuditTx(ctx, tx, p.OrganizationID, "principal", p.ID, "service_key", keyID, id, "service_key.revoked", in.Reason, &before, serviceKeyAuditState(v))
	})
}

// Trusted-host issuance never appears in an idempotency cache or public reply.
// The caller delivers the raw key once; only its hash reaches PostgreSQL.
func (s *Store) IssueServiceToken(ctx context.Context, org, id, ceiling string, ttl time.Duration, reason string) (string, domain.ServiceKey, error) {
	var empty domain.ServiceKey
	if !validText(org, 128) || !validText(id, 128) || !validServiceRole(ceiling) || ttl < time.Second || ttl > 24*time.Hour || ttl%time.Second != 0 || !validText(reason, 4096) {
		return "", empty, domain.Err("invalid_request", "Нужны сервисный субъект, роль member/viewer, конечный срок и причина", 400)
	}
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return "", empty, err
	}
	token := "aps_" + base64.RawURLEncoding.EncodeToString(entropy)
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return "", empty, err
	}
	defer tx.Rollback(ctx)
	if err = organizationIAMGate(ctx, tx, org); err != nil {
		return "", empty, err
	}
	if err = lockAuthorityPrincipalsTx(ctx, tx, org, []string{id}); err != nil {
		return "", empty, err
	}
	var permitted bool
	err = tx.QueryRow(ctx, `SELECT m.active AND p.active AND o.active AND q.service_access_enabled AND p.kind='service' AND p.service_organization_id=m.organization_id AND m.role IN('member','viewer') AND $3<=q.max_service_key_ttl_seconds
 FROM memberships m JOIN principals p ON p.id=m.principal_id JOIN organizations o ON o.id=m.organization_id JOIN organization_login_policies q ON q.organization_id=m.organization_id WHERE m.organization_id=$1 AND m.principal_id=$2`, org, id, int(ttl/time.Second)).Scan(&permitted)
	if err != nil {
		return "", empty, rowExists(err)
	}
	if !permitted {
		return "", empty, domain.Forbidden
	}
	v, err := scanServiceKey(tx.QueryRow(ctx, `INSERT INTO service_credentials(organization_id,id,principal_id,token_hash,role_ceiling,ttl_seconds) VALUES($1,$2,$3,$4,$5,$6) RETURNING `+serviceKeyColumns, org, domain.NewID("key"), id, Hash([]byte(token)), ceiling, int(ttl/time.Second)))
	if err != nil {
		return "", empty, err
	}
	if err = appendSecurityAuditTx(ctx, tx, org, "trusted_host", "", "service_key", v.ID, id, "service_key.issued", reason, nil, serviceKeyAuditState(v)); err != nil {
		return "", empty, err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", empty, err
	}
	return token, v, nil
}
func (s *Store) RevokeIssuedServiceToken(ctx context.Context, hash, reason string) error {
	if !validDigest(hash) || !validText(reason, 4096) {
		return domain.Err("invalid_request", "Нужны точный хеш и причина отзыва", 400)
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var org, id, keyID string
	err = tx.QueryRow(ctx, "SELECT organization_id,principal_id,id FROM service_credentials WHERE token_hash=$1", hash).Scan(&org, &id, &keyID)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if err = organizationIAMGate(ctx, tx, org); err != nil {
		return err
	}
	old, err := loadServiceKeyTx(ctx, tx, org, id, keyID, true)
	if err != nil {
		return err
	}
	if old.Revoked {
		return tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, "UPDATE service_credentials SET revoked=true WHERE organization_id=$1 AND id=$2", org, keyID); err != nil {
		return err
	}
	v, err := loadServiceKeyTx(ctx, tx, org, id, keyID, false)
	if err != nil {
		return err
	}
	before := serviceKeyAuditState(old)
	if err = appendSecurityAuditTx(ctx, tx, org, "trusted_host", "", "service_key", keyID, id, "service_key.revoked", reason, &before, serviceKeyAuditState(v)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) SecurityAudit(ctx context.Context, p domain.Principal, target, raw string, limit int) ([]domain.OrganizationSecurityAuditEntry, string, error) {
	if (target != "" && !validText(target, 128)) || limit < 1 || limit > 100 {
		return nil, "", domain.Err("invalid_request", "Некорректный фильтр или limit", 400)
	}
	c, err := decodeCursor(raw)
	if err != nil {
		return nil, "", err
	}
	tx, p, err := beginSecurityRead(ctx, s, p)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	if target != "" {
		// A filter is an exact tenant-local identity reference, not a global lookup.
		if _, err = loadOrganizationMemberTx(ctx, tx, p.OrganizationID, target, false); err != nil {
			return nil, "", err
		}
	}
	rows, err := tx.Query(ctx, `SELECT id,organization_id,actor_type,COALESCE(actor_id,''),object_type,object_id,COALESCE(target_principal_id,''),action,reason,before_state,after_state,created_at
 FROM organization_security_audit WHERE organization_id=$1 AND ($2='' OR target_principal_id=$2) AND (created_at,id)<($3,$4) ORDER BY created_at DESC,id DESC LIMIT $5`, p.OrganizationID, target, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	out := []domain.OrganizationSecurityAuditEntry{}
	for rows.Next() {
		var v domain.OrganizationSecurityAuditEntry
		var before, after []byte
		if err = rows.Scan(&v.ID, &v.OrganizationID, &v.ActorType, &v.ActorID, &v.ObjectType, &v.ObjectID, &v.TargetPrincipalID, &v.Action, &v.Reason, &before, &after, &v.CreatedAt); err != nil {
			rows.Close()
			return nil, "", err
		}
		if len(before) > 0 {
			var b domain.OrganizationSecurityAuditState
			if err = json.Unmarshal(before, &b); err != nil {
				rows.Close()
				return nil, "", err
			}
			v.Before = &b
		}
		if err = json.Unmarshal(after, &v.After); err != nil {
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
		v := out[limit-1]
		next = encodeCursor(v.CreatedAt, v.ID)
	}
	if err = currentActorTx(ctx, tx, p); err != nil {
		return nil, "", err
	}
	return out, next, tx.Commit(ctx)
}
