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

// A multi-query material read is authorized at its first database snapshot,
// using current credential, membership and task scope in the same read-only
// transaction. A supplied historical Principal role is never read authority.
func (s *Store) beginMaterialRead(ctx context.Context, p domain.Principal, taskID string) (pgx.Tx, error) {
	tx, e := s.beginRuntimeTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return nil, e
	}
	var id, org, role, kind string
	e = tx.QueryRow(ctx, authQuery(p), p.TokenHash, p.OrganizationID).Scan(&id, &org, &role, &kind)
	if e != nil {
		_ = tx.Rollback(ctx)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil, domain.Unauthenticated
		}
		return nil, e
	}
	if id != p.ID || org != p.OrganizationID {
		_ = tx.Rollback(ctx)
		return nil, domain.Unauthenticated
	}
	if !domain.PrincipalHasPermission(domain.Principal{ID: id, OrganizationID: org, Role: role, Kind: kind}, "tasks.read") {
		_ = tx.Rollback(ctx)
		return nil, domain.Forbidden
	}
	var found string
	if e = tx.QueryRow(ctx, "SELECT id FROM tasks WHERE organization_id=$1 AND id=$2", org, taskID).Scan(&found); e != nil {
		_ = tx.Rollback(ctx)
		return nil, rowExists(e)
	}
	return tx, nil
}

const artifactColumns = "id,organization_id,task_id,name,media_type,content,version,created_at,provenance"

func scanArtifact(row scanner) (v domain.Artifact, e error) {
	var provenance []byte
	if e = row.Scan(&v.ID, &v.OrganizationID, &v.TaskID, &v.Name, &v.MediaType, &v.Content, &v.Version, &v.CreatedAt, &provenance); e != nil {
		return v, rowExists(e)
	}
	e = json.Unmarshal(provenance, &v.Provenance)
	return v, e
}

func (s *Store) ArtifactVersion(ctx context.Context, p domain.Principal, taskID, id string, version int) (domain.Artifact, error) {
	if version < 1 {
		return domain.Artifact{}, domain.Err("invalid_request", "Некорректная версия артефакта", 400)
	}
	tx, e := s.beginMaterialRead(ctx, p, taskID)
	if e != nil {
		return domain.Artifact{}, e
	}
	defer tx.Rollback(ctx)
	v, e := scanArtifact(tx.QueryRow(ctx, "SELECT "+artifactColumns+" FROM artifacts WHERE organization_id=$1 AND task_id=$2 AND id=$3 AND version=$4", p.OrganizationID, taskID, id, version))
	if e != nil {
		return v, e
	}
	return v, tx.Commit(ctx)
}

func (s *Store) ArtifactsPage(ctx context.Context, p domain.Principal, taskID, raw string, limit int) ([]domain.Artifact, string, error) {
	if limit < 1 || limit > 100 {
		return nil, "", domain.Err("invalid_request", "limit должен быть от 1 до 100", 400)
	}
	c, e := decodeCursor(raw)
	if e != nil {
		return nil, "", e
	}
	tx, e := s.beginMaterialRead(ctx, p, taskID)
	if e != nil {
		return nil, "", e
	}
	defer tx.Rollback(ctx)
	rows, e := tx.Query(ctx, "SELECT "+artifactColumns+" FROM artifacts WHERE organization_id=$1 AND task_id=$2 AND (created_at,id)<($3,$4) ORDER BY created_at DESC,id DESC LIMIT $5", p.OrganizationID, taskID, c.CreatedAt, c.ID, limit+1)
	if e != nil {
		return nil, "", e
	}
	result := []domain.Artifact{}
	var lastTime time.Time
	lastID := ""
	for rows.Next() {
		v, e := scanArtifact(rows)
		if e != nil {
			rows.Close()
			return nil, "", e
		}
		result = append(result, v)
		if len(result) <= limit {
			lastTime, lastID = v.CreatedAt, v.ID
		}
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		return nil, "", e
	}
	rows.Close()
	next := ""
	if len(result) > limit {
		result = result[:limit]
		next = encodeCursor(lastTime, lastID)
	}
	return result, next, tx.Commit(ctx)
}
