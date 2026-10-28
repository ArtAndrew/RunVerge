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

const organizationMemberColumns = "m.organization_id,m.principal_id,m.display_name,m.role,m.active,p.active,m.version,m.created_at,m.updated_at,m.credentials_revoked_at,p.kind"
const organizationMemberFrom = " FROM memberships m JOIN principals p ON p.id=m.principal_id"

func scanOrganizationMember(row scanner) (v domain.OrganizationMember, err error) {
	err = row.Scan(&v.OrganizationID, &v.PrincipalID, &v.DisplayName, &v.Role, &v.Active, &v.PrincipalActive, &v.Version, &v.CreatedAt, &v.UpdatedAt, &v.CredentialsRevokedAt, &v.PrincipalKind)
	if err != nil {
		return v, rowExists(err)
	}
	return v, nil
}
func validMemberRole(role string) bool {
	return role == "owner" || role == "admin" || role == "member" || role == "viewer"
}
func memberRoles(p domain.Principal) []string {
	if p.Role == "owner" {
		return []string{"owner", "admin", "member", "viewer"}
	}
	if p.Role == "admin" {
		return []string{"admin", "member", "viewer"}
	}
	return []string{}
}
func memberDirectoryPermission(p domain.Principal) error {
	if !domain.PrincipalHasPermission(p, "identity.manage") {
		return domain.Forbidden
	}
	return nil
}
func memberManagementPermission(p domain.Principal, target domain.OrganizationMember, role string) error {
	if err := memberDirectoryPermission(p); err != nil {
		return err
	}
	if target.PrincipalKind != domain.PrincipalHuman {
		return domain.Forbidden
	}
	if target.PrincipalID == p.ID {
		return domain.Err("invalid_transition", "Изменять собственный доступ этой командой нельзя", 409)
	}
	if p.Role != "owner" && (target.Role == "owner" || role == "owner") {
		return domain.Forbidden
	}
	return nil
}
func memberCapabilities(p domain.Principal, v domain.OrganizationMember) domain.OrganizationMember {
	v.AvailableCommands = []string{}
	v.AssignableRoles = []string{}
	if memberManagementPermission(p, v, v.Role) == nil {
		v.AvailableCommands = []string{"update", "revoke_credentials"}
		v.AssignableRoles = memberRoles(p)
	}
	return v
}
func loadOrganizationMemberTx(ctx context.Context, tx pgx.Tx, org, id string, lock bool) (domain.OrganizationMember, error) {
	q := "SELECT " + organizationMemberColumns + organizationMemberFrom + " WHERE m.organization_id=$1 AND m.principal_id=$2"
	if lock {
		q += " FOR UPDATE OF m"
	}
	return scanOrganizationMember(tx.QueryRow(ctx, q, org, id))
}
func beginMemberRead(ctx context.Context, s *Store, p domain.Principal, requireDirectory bool) (pgx.Tx, domain.Principal, error) {
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return nil, p, err
	}
	p, err = authorizeTx(ctx, tx, p, false)
	if err == nil && requireDirectory {
		err = memberDirectoryPermission(p)
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, p, err
	}
	return tx, p, nil
}
func (s *Store) MembershipCapabilities(ctx context.Context, p domain.Principal) (domain.MembershipCapabilities, error) {
	var empty domain.MembershipCapabilities
	tx, p, err := beginMemberRead(ctx, s, p, false)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	v := domain.MembershipCapabilities{OrganizationID: p.OrganizationID, CanCreateMembers: memberDirectoryPermission(p) == nil, AssignableRoles: memberRoles(p), SelfManagementAllowed: false}
	if err = currentActorTx(ctx, tx, p); err != nil {
		return empty, err
	}
	return v, tx.Commit(ctx)
}
func (s *Store) Member(ctx context.Context, p domain.Principal, id string) (domain.OrganizationMember, error) {
	var empty domain.OrganizationMember
	if !validText(id, 128) {
		return empty, domain.Err("invalid_request", "Нужен идентификатор участника", 400)
	}
	tx, p, err := beginMemberRead(ctx, s, p, true)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	v, err := loadOrganizationMemberTx(ctx, tx, p.OrganizationID, id, false)
	if err != nil {
		return empty, err
	}
	if err = currentActorTx(ctx, tx, p); err != nil {
		return empty, err
	}
	return memberCapabilities(p, v), tx.Commit(ctx)
}
func (s *Store) Members(ctx context.Context, p domain.Principal, cursor string, limit int) ([]domain.OrganizationMember, string, error) {
	if limit < 1 || limit > 100 {
		return nil, "", domain.Err("invalid_request", "limit должен быть от 1 до 100", 400)
	}
	c, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	tx, p, err := beginMemberRead(ctx, s, p, true)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "SELECT "+organizationMemberColumns+organizationMemberFrom+" WHERE m.organization_id=$1 AND (m.created_at,m.principal_id)<($2,$3) ORDER BY m.created_at DESC,m.principal_id DESC LIMIT $4", p.OrganizationID, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	values := []domain.OrganizationMember{}
	for rows.Next() {
		v, e := scanOrganizationMember(rows)
		if e != nil {
			rows.Close()
			return nil, "", e
		}
		values = append(values, memberCapabilities(p, v))
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(values) > limit {
		values = values[:limit]
		last := values[limit-1]
		next = encodeCursor(last.CreatedAt, last.PrincipalID)
	}
	if err = currentActorTx(ctx, tx, p); err != nil {
		return nil, "", err
	}
	return values, next, tx.Commit(ctx)
}

// This gate is taken before authorizeTx locks the caller's membership. It
// serializes IAM writers, including reciprocal admin mutations, without taking
// any money/tree/execution locks that a worker may need while holding authority.
func membershipGate(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
	if err := memberDirectoryPermission(p); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "organization-membership-administration\x1f"+p.OrganizationID)
	return err
}
func memberAuditState(v domain.OrganizationMember) domain.MembershipAuditState {
	return domain.MembershipAuditState{DisplayName: v.DisplayName, Role: v.Role, Active: v.Active, Version: v.Version}
}
func appendMembershipAuditTx(ctx context.Context, tx pgx.Tx, p domain.Principal, v domain.OrganizationMember, action, reason string, before *domain.MembershipAuditState) error {
	var beforeJSON []byte
	if before != nil {
		beforeJSON, _ = json.Marshal(before)
	}
	afterJSON, _ := json.Marshal(memberAuditState(v))
	_, err := tx.Exec(ctx, `INSERT INTO organization_membership_audit(id,organization_id,actor_id,target_principal_id,action,reason,before_state,after_state) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, domain.NewID("iam"), p.OrganizationID, p.ID, v.PrincipalID, action, reason, beforeJSON, afterJSON)
	return err
}
func (s *Store) CreateMember(ctx context.Context, p domain.Principal, key string, in domain.CreateOrganizationMember) (json.RawMessage, error) {
	if !validText(in.DisplayName, 120) || !validText(in.Reason, 4096) || !validMemberRole(in.Role) {
		return nil, domain.Err("invalid_request", "Укажите имя, роль и причину создания участника", 400)
	}
	guard := func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		if err := memberDirectoryPermission(p); err != nil {
			return err
		}
		if in.Role == "owner" && p.Role != "owner" {
			return domain.Forbidden
		}
		return nil
	}
	return s.mutateWithPreAuthorization(ctx, p, "POST /members", key, in, membershipGate, guard, func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		id := domain.NewID("usr")
		// Names are labels; no email, global subject, credential or existing
		// principal supplied by a caller can acquire this new membership.
		if _, err := tx.Exec(ctx, "INSERT INTO principals(id,name) VALUES($1,$2)", id, "Provisioned organization user"); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO memberships(organization_id,principal_id,display_name,role) VALUES($1,$2,$3,$4)", p.OrganizationID, id, in.DisplayName, in.Role); err != nil {
			return nil, err
		}
		v, err := loadOrganizationMemberTx(ctx, tx, p.OrganizationID, id, false)
		if err != nil {
			return nil, err
		}
		if err = appendMembershipAuditTx(ctx, tx, p, v, "member.created", in.Reason, nil); err != nil {
			return nil, err
		}
		return memberCapabilities(p, v), nil
	})
}
func memberMutationGuard(id, role string) func(context.Context, pgx.Tx, domain.Principal) error {
	return func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		v, err := loadOrganizationMemberTx(ctx, tx, p.OrganizationID, id, false)
		if err != nil {
			return err
		}
		return memberManagementPermission(p, v, role)
	}
}
func (s *Store) UpdateMember(ctx context.Context, p domain.Principal, id, key string, in domain.UpdateOrganizationMember) (json.RawMessage, error) {
	if !validText(id, 128) || in.ExpectedVersion < 1 || !validText(in.DisplayName, 120) || !validText(in.Reason, 4096) || !validMemberRole(in.Role) || in.Active == nil {
		return nil, domain.Err("invalid_request", "Укажите точную версию, имя, роль, active и причину изменения", 400)
	}
	return s.mutateWithPreAuthorization(ctx, p, "POST /members/"+id+"/update", key, in, membershipGate, memberMutationGuard(id, in.Role), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		before, err := loadOrganizationMemberTx(ctx, tx, p.OrganizationID, id, true)
		if err != nil {
			return nil, err
		}
		if err = memberManagementPermission(p, before, in.Role); err != nil {
			return nil, err
		}
		if before.Version != in.ExpectedVersion {
			return nil, domain.Err("conflict", "Участник изменился. Обновите данные и повторите решение", 409)
		}
		if before.DisplayName == in.DisplayName && before.Role == in.Role && before.Active == *in.Active {
			return nil, domain.Err("invalid_transition", "Изменений в данных участника нет", 409)
		}
		if before.Role == "owner" && before.Active && before.PrincipalActive && (in.Role != "owner" || !*in.Active) {
			var owners int
			if err = tx.QueryRow(ctx, "SELECT count(*) FROM memberships m JOIN principals p ON p.id=m.principal_id WHERE m.organization_id=$1 AND m.active AND p.active AND m.role='owner'", p.OrganizationID).Scan(&owners); err != nil {
				return nil, err
			}
			if owners < 2 {
				return nil, domain.Err("invalid_transition", "В организации должен оставаться активный владелец", 409)
			}
		}
		if _, err = tx.Exec(ctx, "UPDATE memberships SET display_name=$3,role=$4,active=$5 WHERE organization_id=$1 AND principal_id=$2", p.OrganizationID, id, in.DisplayName, in.Role, *in.Active); err != nil {
			return nil, err
		}
		v, err := loadOrganizationMemberTx(ctx, tx, p.OrganizationID, id, false)
		if err != nil {
			return nil, err
		}
		state := memberAuditState(before)
		if err = appendMembershipAuditTx(ctx, tx, p, v, "member.updated", in.Reason, &state); err != nil {
			return nil, err
		}
		return memberCapabilities(p, v), nil
	})
}
func (s *Store) RevokeMemberCredentials(ctx context.Context, p domain.Principal, id, key string, in domain.RevokeMemberCredentials) (json.RawMessage, error) {
	if !validText(id, 128) || in.ExpectedVersion < 1 || !validText(in.Reason, 4096) {
		return nil, domain.Err("invalid_request", "Укажите точную версию участника и причину отзыва", 400)
	}
	return s.mutateWithPreAuthorization(ctx, p, "POST /members/"+id+"/revoke-credentials", key, in, membershipGate, memberMutationGuard(id, ""), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		before, err := loadOrganizationMemberTx(ctx, tx, p.OrganizationID, id, true)
		if err != nil {
			return nil, err
		}
		if err = memberManagementPermission(p, before, before.Role); err != nil {
			return nil, err
		}
		if before.Version != in.ExpectedVersion {
			return nil, domain.Err("conflict", "Участник изменился. Обновите данные и повторите решение", 409)
		}
		if _, err = tx.Exec(ctx, "UPDATE memberships SET credential_cutoff=nextval('credential_origin_sequence') WHERE organization_id=$1 AND principal_id=$2", p.OrganizationID, id); err != nil {
			return nil, err
		}
		v, err := loadOrganizationMemberTx(ctx, tx, p.OrganizationID, id, false)
		if err != nil {
			return nil, err
		}
		state := memberAuditState(before)
		if err = appendMembershipAuditTx(ctx, tx, p, v, "member.credentials_revoked", in.Reason, &state); err != nil {
			return nil, err
		}
		return memberCapabilities(p, v), nil
	})
}
func (s *Store) MembershipAudit(ctx context.Context, p domain.Principal, id, cursor string, limit int) ([]domain.MembershipAuditEntry, string, error) {
	if limit < 1 || limit > 100 || (id != "" && !validText(id, 128)) {
		return nil, "", domain.Err("invalid_request", "Некорректный участник или limit", 400)
	}
	c, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	tx, p, err := beginMemberRead(ctx, s, p, true)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	if id != "" {
		if _, err = loadOrganizationMemberTx(ctx, tx, p.OrganizationID, id, false); err != nil {
			return nil, "", err
		}
	}
	rows, err := tx.Query(ctx, `SELECT id,organization_id,actor_id,target_principal_id,action,reason,before_state,after_state,created_at FROM organization_membership_audit WHERE organization_id=$1 AND ($2='' OR target_principal_id=$2) AND (created_at,id)<($3,$4) ORDER BY created_at DESC,id DESC LIMIT $5`, p.OrganizationID, id, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	values := []domain.MembershipAuditEntry{}
	for rows.Next() {
		var v domain.MembershipAuditEntry
		var before, after []byte
		err = rows.Scan(&v.ID, &v.OrganizationID, &v.ActorID, &v.TargetPrincipalID, &v.Action, &v.Reason, &before, &after, &v.CreatedAt)
		if err == nil && before != nil {
			err = json.Unmarshal(before, &v.Before)
		}
		if err == nil {
			err = json.Unmarshal(after, &v.After)
		}
		if err != nil {
			rows.Close()
			return nil, "", err
		}
		values = append(values, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(values) > limit {
		values = values[:limit]
		last := values[limit-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	if err = currentActorTx(ctx, tx, p); err != nil {
		return nil, "", err
	}
	return values, next, tx.Commit(ctx)
}

// IssueMemberToken is a trusted-host operation, never an HTTP/MCP command.
// The token authenticates a principal; organization selection still requires a
// current membership. Its immutable origin comes from PostgreSQL, not a clock.
func (s *Store) IssueMemberToken(ctx context.Context, orgID, principalID string, ttl time.Duration) (string, time.Time, error) {
	var empty time.Time
	if !validText(orgID, 128) || !validText(principalID, 128) || ttl < time.Second || ttl > 24*time.Hour || ttl%time.Second != 0 {
		return "", empty, domain.Err("invalid_request", "Нужны организация, участник и срок от 1 до 86400 секунд", 400)
	}
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return "", empty, err
	}
	token := "ap_" + base64.RawURLEncoding.EncodeToString(entropy)
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return "", empty, err
	}
	defer tx.Rollback(ctx)
	if err = lockAuthorityPrincipalsTx(ctx, tx, orgID, []string{principalID}); err != nil {
		return "", empty, err
	}
	var active bool
	err = tx.QueryRow(ctx, `SELECT m.active AND p.active AND o.active AND p.kind='human' AND q.mode='mixed' FROM memberships m JOIN principals p ON p.id=m.principal_id JOIN organizations o ON o.id=m.organization_id JOIN organization_login_policies q ON q.organization_id=m.organization_id WHERE m.organization_id=$1 AND m.principal_id=$2 FOR SHARE OF o`, orgID, principalID).Scan(&active)
	if err != nil {
		return "", empty, rowExists(err)
	}
	if !active {
		return "", empty, domain.Forbidden
	}
	var expires time.Time
	if err = tx.QueryRow(ctx, "INSERT INTO access_tokens(token_hash,principal_id,expires_at) VALUES($1,$2,clock_timestamp()+$3::interval) RETURNING expires_at", Hash([]byte(token)), principalID, ttl.String()).Scan(&expires); err != nil {
		return "", empty, err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", empty, err
	}
	return token, expires, nil
}

// A caller which cannot durably save a newly issued credential can revoke its
// exact hash without touching any other credential or organization membership.
func (s *Store) RevokeIssuedToken(ctx context.Context, tokenHash string) error {
	if !validDigest(tokenHash) {
		return domain.Err("invalid_request", "Нужен хеш выданного токена", 400)
	}
	_, err := s.execRuntime(ctx, "UPDATE access_tokens SET revoked=true WHERE token_hash=$1", tokenHash)
	return err
}
