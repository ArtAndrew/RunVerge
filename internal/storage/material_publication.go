// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

func (s *Store) PublishMaterial(ctx context.Context, p domain.Principal, taskID, id, key string, in domain.PublishMaterial) (json.RawMessage, error) {
	if in.ExpectedTaskVersion < 1 || in.ExpectedMaterialVersion < 1 || in.ExpectedMaterialVersion > domain.MaxMaterialRevisions || !validDigest(in.ContentDigest) || !validText(in.ParentCandidateID, 128) {
		return nil, domain.Err("invalid_request", "Нужны точные версии задачи, материала и исходного результата", 400)
	}
	return s.MutateGuarded(ctx, p, "POST /tasks/"+taskID+"/materials/"+id+"/publish", key, in, materialGuard(taskID, true), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		control, e := lockTaskTreeTx(ctx, tx, p.OrganizationID, taskID)
		if e != nil {
			return nil, e
		}
		tasks, e := lockTreeTasksTx(ctx, tx, p.OrganizationID, control.RootID)
		if e != nil {
			return nil, e
		}
		task, e := findTreeTask(tasks, taskID)
		if e != nil {
			return nil, e
		}
		if e = noActiveToolExecutionsTx(ctx, tx, p.OrganizationID, subtreeIDs(tasks, taskID, true)); e != nil {
			return nil, e
		}
		if task.Version != in.ExpectedTaskVersion || task.CandidateID != in.ParentCandidateID {
			return nil, domain.Err("conflict", "Результат задачи изменился. Проверьте публикацию заново", 409)
		}
		if task.Status != "waiting_input" || !candidateWaitingReason(task.WaitingReason) {
			return nil, domain.Err("invalid_transition", "Публикация возможна только вместо текущего результата, ожидающего решения", 409)
		}
		if task.ParentID != "" {
			var consumed bool
			if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM team_members WHERE organization_id=$1 AND child_task_id=$2 AND selected_candidate_id IS NOT NULL)", p.OrganizationID, taskID).Scan(&consumed); e != nil {
				return nil, e
			}
			if consumed {
				return nil, domain.Err("conflict", "Материал участника уже включён в работу команды", 409)
			}
		}
		parent, _, e := loadCandidateTx(ctx, tx, p.OrganizationID, in.ParentCandidateID)
		if e != nil {
			return nil, e
		}
		if parent.TaskID != taskID || parent.RunID != task.CurrentRunID {
			return nil, domain.Err("conflict", "Исходный результат относится к другой попытке", 409)
		}
		var ready bool
		if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE organization_id=$1 AND id=$2 AND run_id=$3 AND task_id=$4 AND status='blocked') AND NOT EXISTS(SELECT 1 FROM task_decisions WHERE organization_id=$1 AND candidate_id=$5)", p.OrganizationID, parent.OperationID, parent.RunID, taskID, parent.ID).Scan(&ready); e != nil {
			return nil, e
		}
		if !ready {
			return nil, domain.Err("invalid_transition", "Исходная попытка больше не ожидает решения", 409)
		}
		var active bool
		if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE organization_id=$1 AND task_id=ANY($2::text[]) AND status IN('queued','leased'))", p.OrganizationID, subtreeIDs(tasks, taskID, true)).Scan(&active); e != nil {
			return nil, e
		}
		if active {
			return nil, domain.Err("conflict", "Подзадачи ещё выполняются", 409)
		}
		m, e := loadMaterialTx(ctx, tx, p.OrganizationID, taskID, id, true)
		if e != nil {
			return nil, e
		}
		if m.HeadVersion != in.ExpectedMaterialVersion || m.HeadContentDigest != in.ContentDigest {
			return nil, domain.Err("conflict", "Материал изменился. Проверьте точную версию перед публикацией", 409)
		}
		var content string
		if e = tx.QueryRow(ctx, "SELECT content FROM material_revisions WHERE organization_id=$1 AND material_id=$2 AND version=$3", p.OrganizationID, id, m.HeadVersion).Scan(&content); e != nil {
			return nil, rowExists(e)
		}
		if Hash([]byte(content)) != in.ContentDigest {
			return nil, domain.Err("conflict", "Контрольная сумма материала не совпадает", 409)
		}
		if !validText(content, domain.MaxMaterialContentBytes) || (m.MediaType == "application/json" && !json.Valid([]byte(content))) {
			return nil, domain.Err("invalid_request", "Для публикации нужен непустой текст и корректный JSON, если выбран этот тип", 400)
		}
		if e = materialQuotaTx(ctx, tx, p.OrganizationID, int64(len(content))); e != nil {
			return nil, e
		}
		var publications int
		if e = tx.QueryRow(ctx, "SELECT COALESCE((SELECT publications FROM material_publication_counts WHERE organization_id=$1 AND task_id=$2),0)", p.OrganizationID, taskID).Scan(&publications); e != nil {
			return nil, e
		}
		if publications >= domain.MaxMaterialPublications {
			return nil, materialLimit()
		}
		basis, e := materialReviewBasisTx(ctx, tx, task, parent)
		if e != nil {
			return nil, e
		}
		if e = currentActorTx(ctx, tx, p); e != nil {
			return nil, e
		}
		artifactID, candidateID := domain.NewID("art"), domain.NewID("cand")
		provenance := map[string]any{"adapter": "material:revision", "verified": false, "verification": "awaiting_authorized_decision", "material_id": id, "material_version": m.HeadVersion, "content_digest": in.ContentDigest, "parent_candidate_id": parent.ID, "review_basis_candidate_id": basis.ID, "published_by": p.ID}
		if _, e = tx.Exec(ctx, "INSERT INTO artifacts(organization_id,id,task_id,name,media_type,content,provenance) VALUES($1,$2,$3,$4,$5,$6,$7)", p.OrganizationID, artifactID, taskID, m.Name, m.MediaType, content, provenance); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO task_candidates(organization_id,id,task_id,run_id,operation_id,artifact_id,artifact_version,content_digest,source,parent_candidate_id,review_basis_candidate_id,material_id,material_version,published_by)
 VALUES($1,$2,$3,$4,$5,$6,1,$7,'material',$8,$9,$10,$11,$12)`, p.OrganizationID, candidateID, taskID, parent.RunID, parent.OperationID, artifactID, in.ContentDigest, parent.ID, basis.ID, id, m.HeadVersion, p.ID); e != nil {
			return nil, e
		}
		task, e = scanTask(tx.QueryRow(ctx, "UPDATE tasks SET candidate_id=$3,waiting_reason='material_candidate_pending',version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2 RETURNING "+taskColumns, p.OrganizationID, taskID, candidateID))
		if e != nil {
			return nil, e
		}
		return task, emit(ctx, tx, p.OrganizationID, taskID, "material.published", map[string]any{"material_id": id, "material_version": m.HeadVersion, "candidate_id": candidateID, "parent_candidate_id": parent.ID, "review_basis_candidate_id": basis.ID, "artifact_id": artifactID, "artifact_version": 1, "content_digest": in.ContentDigest, "published_by": p.ID, "new_run_created": false, "costs_unchanged": true})
	})
}

func candidateWaitingReason(reason string) bool {
	return reason == "model_candidate_pending" || reason == "team_candidate_pending" || reason == "material_candidate_pending"
}

// A material may change the final prose, never the original execution closure.
// This direct immutable link avoids traversing a user-sized predecessor chain.
func materialReviewBasisTx(ctx context.Context, tx pgx.Tx, task domain.Task, candidate domain.TaskCandidate) (domain.TaskCandidate, error) {
	basis := candidate
	if candidate.Source == "material" {
		var e error
		basis, _, e = loadCandidateTx(ctx, tx, task.OrganizationID, candidate.ReviewBasisCandidateID)
		if e != nil {
			return basis, e
		}
	}
	if basis.Source == "material" || basis.Source == "model_tools" || basis.TaskID != task.ID || basis.RunID != task.CurrentRunID || basis.OperationID != candidate.OperationID {
		return basis, domain.Err("conflict", "Основа решения не соответствует исходному выполнению", 409)
	}
	return basis, nil
}
