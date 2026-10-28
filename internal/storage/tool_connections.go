// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/toolruntime"
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

func scanToolConnection(row scanner) (c domain.ToolConnection, err error) {
	var raw []byte
	var active bool
	var created time.Time
	var stored string
	if err = row.Scan(&raw, &active, &created, &stored); err != nil {
		return c, rowExists(err)
	}
	if err = json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	c.Tool.Active = active
	c.Tool.CreatedAt = created
	if domain.ToolConnectionDigest(c) != stored {
		return c, domain.Err("conflict", "Сохранённый профиль инструмента повреждён", 409)
	}
	return c, nil
}
func loadToolConnectionTx(ctx context.Context, tx pgx.Tx, org, id string, version int, lock bool) (domain.ToolConnection, error) {
	query := "SELECT profile,active,created_at,profile_digest FROM tool_connections WHERE organization_id=$1 AND id=$2 AND version=$3"
	if lock {
		query += " FOR SHARE"
	}
	return scanToolConnection(tx.QueryRow(ctx, query, org, id, version))
}

// InstallToolConnection is trusted-host configuration. Repeating an immutable
// version never reactivates it or changes endpoints, credentials or cost bounds.
func (s *Store) InstallToolConnection(ctx context.Context, c domain.ToolConnection) (domain.ToolConnection, error) {
	var empty domain.ToolConnection
	var err error
	c, err = normalizeInstalledToolConnection(c)
	if err != nil {
		return empty, err
	}
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return empty, e
	}
	defer tx.Rollback(ctx)
	if e = configurationGateTx(ctx, tx, c.Tool.OrganizationID); e != nil {
		return empty, e
	}
	if e = lockConfigurationOrganizationOnlyTx(ctx, tx, c.Tool.OrganizationID); e != nil {
		return empty, e
	}
	created, e := installToolConnectionTx(ctx, tx, c)
	if e != nil {
		return empty, e
	}
	return created, tx.Commit(ctx)
}

// Caller holds the configuration gate and organization row. No I/O or commit.
func normalizeInstalledToolConnection(c domain.ToolConnection) (domain.ToolConnection, error) {
	if c.Tool.InputSchemaDigest == "" {
		c.Tool.InputSchemaDigest = Hash([]byte(c.Tool.InputSchemaJSON))
	}
	if c.Tool.AutomationAllowed && (c.Tool.Effect != "read" || c.Tool.RequireDistinctApprover) {
		return c, domain.Err("invalid_request", "Automatic execution can only opt in a read tool without a distinct approver", 400)
	}
	if toolruntime.ValidateConnection(c) != nil {
		return c, domain.Err("invalid_request", "Некорректный профиль инструмента", 400)
	}

	return c, nil
}

func installToolConnectionTx(ctx context.Context, tx pgx.Tx, c domain.ToolConnection) (domain.ToolConnection, error) {
	var empty domain.ToolConnection
	var err error
	c, err = normalizeInstalledToolConnection(c)
	if err != nil {
		return empty, err
	}
	active := c.Tool.Active
	c.Tool.Active = false
	c.Tool.CreatedAt = time.Time{}
	profile, _ := json.Marshal(c)
	hash := domain.ToolConnectionDigest(c)
	existing, e := loadToolConnectionTx(ctx, tx, c.Tool.OrganizationID, c.Tool.ID, c.Tool.Version, true)
	if e == nil {
		if domain.ToolConnectionDigest(existing) != hash {
			return empty, domain.Err("conflict", "Версия инструмента уже содержит другой профиль", 409)
		}
		return existing, nil
	}
	if !errors.Is(e, domain.NotFound) {
		return empty, e
	}
	var latest int
	if e = tx.QueryRow(ctx, "SELECT COALESCE(max(version),0) FROM tool_connections WHERE organization_id=$1 AND id=$2", c.Tool.OrganizationID, c.Tool.ID).Scan(&latest); e != nil {
		return empty, e
	}
	if c.Tool.Version != latest+1 {
		return empty, domain.Err("conflict", "Версия инструмента должна следовать за предыдущей", 409)
	}
	if active {
		if _, e = tx.Exec(ctx, "UPDATE tool_connections SET active=false WHERE organization_id=$1 AND id=$2 AND active", c.Tool.OrganizationID, c.Tool.ID); e != nil {
			return empty, e
		}
	}
	created, e := scanToolConnection(tx.QueryRow(ctx, "INSERT INTO tool_connections(organization_id,id,version,profile,profile_digest,active) VALUES($1,$2,$3,$4,$5,$6) RETURNING profile,active,created_at,profile_digest", c.Tool.OrganizationID, c.Tool.ID, c.Tool.Version, profile, hash, active))
	if e != nil {
		return empty, e
	}
	return created, nil
}
func (s *Store) Tools(ctx context.Context, p domain.Principal, raw string, limit int) ([]domain.ToolInfo, string, error) {
	if limit < 1 || limit > 100 {
		return nil, "", domain.Err("invalid_request", "limit должен быть от 1 до 100", 400)
	}
	position, e := decodeCursor(raw)
	if e != nil {
		return nil, "", e
	}
	tx, e := s.beginRuntimeTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return nil, "", e
	}
	defer tx.Rollback(ctx)
	var id, org, role, kind string
	if e = tx.QueryRow(ctx, authQuery(p), p.TokenHash, p.OrganizationID).Scan(&id, &org, &role, &kind); errors.Is(e, pgx.ErrNoRows) {
		return nil, "", domain.Unauthenticated
	} else if e != nil {
		return nil, "", e
	}
	if id != p.ID || !domain.PrincipalHasPermission(domain.Principal{ID: id, OrganizationID: org, Role: role, Kind: kind}, "tasks.read") {
		return nil, "", domain.Forbidden
	}
	rows, e := tx.Query(ctx, "SELECT profile,active,created_at,profile_digest FROM tool_connections WHERE organization_id=$1 AND active AND (created_at,id)<($2,$3) ORDER BY created_at DESC,id DESC LIMIT $4", org, position.CreatedAt, position.ID, limit+1)
	if e != nil {
		return nil, "", e
	}
	defer rows.Close()
	out := []domain.ToolInfo{}
	for rows.Next() {
		c, e := scanToolConnection(rows)
		if e != nil {
			return nil, "", e
		}
		out = append(out, c.Tool)
	}
	if e = rows.Err(); e != nil {
		return nil, "", e
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	rows.Close()
	return out, next, tx.Commit(ctx)
}
