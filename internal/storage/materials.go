// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

const materialColumns = "m.id,m.organization_id,m.task_id,m.name,m.media_type,m.head_version,r.content_digest,COALESCE(m.base_candidate_id,'') AS base_candidate_id,m.created_by,m.created_at,m.updated_at"
const materialJoin = "materials m JOIN material_revisions r ON r.organization_id=m.organization_id AND r.material_id=m.id AND r.version=m.head_version"
const revisionColumns = "id,organization_id,task_id,material_id,version,base_version,merge_base_version,content_digest,principal_id,reason,created_at"

func scanMaterial(row scanner) (v domain.Material, e error) {
	e = row.Scan(&v.ID, &v.OrganizationID, &v.TaskID, &v.Name, &v.MediaType, &v.HeadVersion, &v.HeadContentDigest, &v.BaseCandidateID, &v.CreatedBy, &v.CreatedAt, &v.UpdatedAt)
	return v, rowExists(e)
}

func scanMaterialRevision(row scanner) (v domain.MaterialRevision, e error) {
	e = row.Scan(&v.ID, &v.OrganizationID, &v.TaskID, &v.MaterialID, &v.Version, &v.BaseVersion, &v.MergeBaseVersion, &v.ContentDigest, &v.PrincipalID, &v.Reason, &v.CreatedAt)
	return v, rowExists(e)
}

func materialMediaType(value string) bool {
	return value == "text/plain" || value == "text/markdown" || value == "application/json"
}

func materialTaskTx(ctx context.Context, tx pgx.Tx, org, id string) (domain.Task, error) {
	control, e := lockTaskTreeTx(ctx, tx, org, id)
	if e != nil {
		return domain.Task{}, e
	}
	tasks, e := lockTreeTasksTx(ctx, tx, org, control.RootID)
	if e != nil {
		return domain.Task{}, e
	}
	return findTreeTask(tasks, id)
}

func materialGuard(id string, publish bool) func(context.Context, pgx.Tx, domain.Principal) error {
	own, any := "materials.write_own", "materials.write_any"
	if publish {
		own, any = "materials.publish_own", "materials.publish_any"
	}
	return func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		return taskMutationPermission(ctx, tx, p, id, own, any)
	}
}

func materialLimit() error {
	return domain.Err("invalid_transition", "Достигнут предел хранения или версий материалов", 409)
}

// All callers already hold the organization and task locks. The quota covers
// draft content/reasons and the additional physical publication snapshots;
// it is not a billing fact and it never changes monetary counters.
func materialQuotaTx(ctx context.Context, tx pgx.Tx, org string, bytes int64) error {
	if _, e := tx.Exec(ctx, "INSERT INTO material_storage(organization_id) VALUES($1) ON CONFLICT DO NOTHING", org); e != nil {
		return e
	}
	var remaining int64
	if e := tx.QueryRow(ctx, "SELECT max_bytes-used_bytes FROM material_storage WHERE organization_id=$1 FOR NO KEY UPDATE", org).Scan(&remaining); e != nil {
		return e
	}
	if bytes > remaining {
		return materialLimit()
	}
	return nil
}

func loadMaterialTx(ctx context.Context, tx pgx.Tx, org, taskID, id string, lock bool) (domain.Material, error) {
	if lock {
		var found string
		if e := tx.QueryRow(ctx, "SELECT id FROM materials WHERE organization_id=$1 AND task_id=$2 AND id=$3 FOR NO KEY UPDATE", org, taskID, id).Scan(&found); e != nil {
			return domain.Material{}, rowExists(e)
		}
	}
	return scanMaterial(tx.QueryRow(ctx, "SELECT "+materialColumns+" FROM "+materialJoin+" WHERE m.organization_id=$1 AND m.task_id=$2 AND m.id=$3", org, taskID, id))
}

func insertMaterialRevisionTx(ctx context.Context, tx pgx.Tx, p domain.Principal, taskID, id string, version int, merge *int, content, reason string) error {
	_, e := tx.Exec(ctx, `INSERT INTO material_revisions(organization_id,id,task_id,material_id,version,base_version,merge_base_version,content,content_digest,principal_id,reason)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, p.OrganizationID, domain.NewID("mrv"), taskID, id, version, version-1, merge, content, Hash([]byte(content)), p.ID, reason)
	return e
}

func (s *Store) CreateMaterial(ctx context.Context, p domain.Principal, taskID, key string, in domain.CreateMaterial) (json.RawMessage, error) {
	if !validText(in.Name, 120) || !materialMediaType(in.MediaType) || !optionalText(in.Content, domain.MaxMaterialContentBytes) || !optionalText(in.Reason, 4096) || (in.BaseCandidateID != "" && !validText(in.BaseCandidateID, 128)) {
		return nil, domain.Err("invalid_request", "Некорректные имя, тип или содержимое материала", 400)
	}
	return s.MutateGuarded(ctx, p, "POST /tasks/"+taskID+"/materials", key, in, materialGuard(taskID, false), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		if _, e := materialTaskTx(ctx, tx, p.OrganizationID, taskID); e != nil {
			return nil, e
		}
		if in.BaseCandidateID != "" {
			c, _, e := loadCandidateTx(ctx, tx, p.OrganizationID, in.BaseCandidateID)
			if e != nil {
				return nil, e
			}
			if c.TaskID != taskID {
				return nil, domain.NotFound
			}
		}
		if e := materialQuotaTx(ctx, tx, p.OrganizationID, int64(len(in.Content)+len(in.Reason))); e != nil {
			return nil, e
		}
		var documents, maxDocuments, taskDocuments int
		if e := tx.QueryRow(ctx, "SELECT used_documents,max_documents FROM material_storage WHERE organization_id=$1", p.OrganizationID).Scan(&documents, &maxDocuments); e != nil {
			return nil, e
		}
		if e := tx.QueryRow(ctx, "SELECT COALESCE((SELECT documents FROM material_task_counts WHERE organization_id=$1 AND task_id=$2),0)", p.OrganizationID, taskID).Scan(&taskDocuments); e != nil {
			return nil, e
		}
		if documents >= maxDocuments || taskDocuments >= domain.MaxMaterialsPerTask {
			return nil, materialLimit()
		}
		id := domain.NewID("mat")
		if _, e := tx.Exec(ctx, "INSERT INTO materials(organization_id,id,task_id,name,media_type,base_candidate_id,created_by) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7)", p.OrganizationID, id, taskID, in.Name, in.MediaType, in.BaseCandidateID, p.ID); e != nil {
			return nil, e
		}
		if e := insertMaterialRevisionTx(ctx, tx, p, taskID, id, 1, nil, in.Content, in.Reason); e != nil {
			return nil, e
		}
		m, e := loadMaterialTx(ctx, tx, p.OrganizationID, taskID, id, false)
		if e != nil {
			return nil, e
		}
		return m, emit(ctx, tx, p.OrganizationID, taskID, "material.created", map[string]any{"material_id": id, "version": 1, "content_digest": m.HeadContentDigest, "principal_id": p.ID, "draft_only": true, "costs_unchanged": true})
	})
}

func (s *Store) ReviseMaterial(ctx context.Context, p domain.Principal, taskID, id, key string, in domain.ReviseMaterial) (json.RawMessage, error) {
	if in.ExpectedVersion < 1 || in.ExpectedVersion > domain.MaxMaterialRevisions || !optionalText(in.Content, domain.MaxMaterialContentBytes) || !validText(in.Reason, 4096) || (in.MergeBaseVersion != nil && (*in.MergeBaseVersion < 1 || *in.MergeBaseVersion > in.ExpectedVersion)) {
		return nil, domain.Err("invalid_request", "Нужны исходная версия, текст и причина изменения", 400)
	}
	return s.MutateGuarded(ctx, p, "POST /tasks/"+taskID+"/materials/"+id+"/revisions", key, in, materialGuard(taskID, false), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		if _, e := materialTaskTx(ctx, tx, p.OrganizationID, taskID); e != nil {
			return nil, e
		}
		m, e := loadMaterialTx(ctx, tx, p.OrganizationID, taskID, id, true)
		if e != nil {
			return nil, e
		}
		if m.HeadVersion != in.ExpectedVersion {
			return nil, domain.Err("conflict", "Материал изменён. Сохраните свой текст и объедините его с новой версией", 409)
		}
		if m.HeadVersion >= domain.MaxMaterialRevisions {
			return nil, materialLimit()
		}
		if e := materialQuotaTx(ctx, tx, p.OrganizationID, int64(len(in.Content)+len(in.Reason))); e != nil {
			return nil, e
		}
		if e = insertMaterialRevisionTx(ctx, tx, p, taskID, id, m.HeadVersion+1, in.MergeBaseVersion, in.Content, in.Reason); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, "UPDATE materials SET head_version=head_version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2", p.OrganizationID, id); e != nil {
			return nil, e
		}
		m, e = loadMaterialTx(ctx, tx, p.OrganizationID, taskID, id, false)
		if e != nil {
			return nil, e
		}
		return m, emit(ctx, tx, p.OrganizationID, taskID, "material.revised", map[string]any{"material_id": id, "version": m.HeadVersion, "base_version": in.ExpectedVersion, "merge_base_version": in.MergeBaseVersion, "content_digest": m.HeadContentDigest, "principal_id": p.ID, "draft_only": true, "costs_unchanged": true})
	})
}

func (s *Store) Materials(ctx context.Context, p domain.Principal, taskID, raw string, limit int) ([]domain.Material, string, error) {
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
	rows, e := tx.Query(ctx, "SELECT "+materialColumns+" FROM "+materialJoin+" WHERE m.organization_id=$1 AND m.task_id=$2 AND (m.created_at,m.id)<($3,$4) ORDER BY m.created_at DESC,m.id DESC LIMIT $5", p.OrganizationID, taskID, c.CreatedAt, c.ID, limit+1)
	if e != nil {
		return nil, "", e
	}
	result := []domain.Material{}
	var lastTime time.Time
	lastID := ""
	for rows.Next() {
		v, e := scanMaterial(rows)
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

func (s *Store) GetMaterial(ctx context.Context, p domain.Principal, taskID, id string) (domain.Material, error) {
	tx, e := s.beginMaterialRead(ctx, p, taskID)
	if e != nil {
		return domain.Material{}, e
	}
	defer tx.Rollback(ctx)
	v, e := loadMaterialTx(ctx, tx, p.OrganizationID, taskID, id, false)
	if e != nil {
		return v, e
	}
	return v, tx.Commit(ctx)
}

func (s *Store) MaterialRevisions(ctx context.Context, p domain.Principal, taskID, id, raw string, limit int) ([]domain.MaterialRevision, string, error) {
	// The material identifier is a query argument, never a SQL table fragment.
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
	if _, e = loadMaterialTx(ctx, tx, p.OrganizationID, taskID, id, false); e != nil {
		return nil, "", e
	}
	rows, e := tx.Query(ctx, "SELECT "+revisionColumns+" FROM material_revisions WHERE organization_id=$1 AND task_id=$2 AND material_id=$3 AND (created_at,id)<($4,$5) ORDER BY created_at DESC,id DESC LIMIT $6", p.OrganizationID, taskID, id, c.CreatedAt, c.ID, limit+1)
	if e != nil {
		return nil, "", e
	}
	defer rows.Close()
	result := []domain.MaterialRevision{}
	for rows.Next() {
		v, e := scanMaterialRevision(rows)
		if e != nil {
			return nil, "", e
		}
		result = append(result, v)
	}
	if e = rows.Err(); e != nil {
		return nil, "", e
	}
	rows.Close()
	next := ""
	if len(result) > limit {
		last := result[limit-1]
		next = encodeCursor(last.CreatedAt, last.ID)
		result = result[:limit]
	}
	return result, next, tx.Commit(ctx)
}

func (s *Store) MaterialVersion(ctx context.Context, p domain.Principal, taskID, id string, version int) (domain.MaterialContent, error) {
	var result domain.MaterialContent
	if version < 1 || version > domain.MaxMaterialRevisions {
		return result, domain.Err("invalid_request", "Некорректная версия материала", 400)
	}
	tx, e := s.beginMaterialRead(ctx, p, taskID)
	if e != nil {
		return result, e
	}
	defer tx.Rollback(ctx)
	if _, e = loadMaterialTx(ctx, tx, p.OrganizationID, taskID, id, false); e != nil {
		return result, e
	}
	r, e := scanMaterialRevision(tx.QueryRow(ctx, "SELECT "+revisionColumns+" FROM material_revisions WHERE organization_id=$1 AND task_id=$2 AND material_id=$3 AND version=$4", p.OrganizationID, taskID, id, version))
	if e != nil {
		return result, e
	}
	if e = tx.QueryRow(ctx, "SELECT content FROM material_revisions WHERE organization_id=$1 AND id=$2", p.OrganizationID, r.ID).Scan(&result.Content); e != nil {
		return result, rowExists(e)
	}
	if Hash([]byte(result.Content)) != r.ContentDigest {
		return result, domain.Err("conflict", "Содержимое материала не соответствует сохранённой версии", 409)
	}
	result.Revision = r
	return result, tx.Commit(ctx)
}
