// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

type TreeControl struct {
	RootID                                                        string
	MaxTasks, UsedTasks, MaxTotalAttempts, UsedAttempts, MaxDepth int
}

// Structural transactions acquire authority, then the permanent tree gate,
// existing operations, organization and depth-ordered tasks. Leaf accounting
// never acquires the gate and never creates another operation or task.
func lockTaskTreeTx(ctx context.Context, tx pgx.Tx, org, taskID string) (TreeControl, error) {
	var control TreeControl
	if e := tx.QueryRow(ctx, "SELECT root_id FROM tasks WHERE organization_id=$1 AND id=$2", org, taskID).Scan(&control.RootID); e != nil {
		return control, rowExists(e)
	}
	// The gate serializes structural writers without blocking the deferred
	// foreign-key KEY SHARE checks of a leaf transaction finishing below it.
	if e := tx.QueryRow(ctx, "SELECT max_tasks,used_tasks,max_total_attempts,used_attempts,max_depth FROM task_tree_controls WHERE organization_id=$1 AND root_id=$2 FOR NO KEY UPDATE", org, control.RootID).Scan(&control.MaxTasks, &control.UsedTasks, &control.MaxTotalAttempts, &control.UsedAttempts, &control.MaxDepth); e != nil {
		return control, rowExists(e)
	}
	rows, e := tx.Query(ctx, `SELECT o.id FROM operations o JOIN tasks t ON t.organization_id=o.organization_id AND t.id=o.task_id
 WHERE t.organization_id=$1 AND t.root_id=$2 ORDER BY o.id FOR UPDATE OF o`, org, control.RootID)
	if e != nil {
		return control, e
	}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			break
		}
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	return control, e
}

func lockTreeTasksTx(ctx context.Context, tx pgx.Tx, org, root string) ([]domain.Task, error) {
	if _, e := tx.Exec(ctx, "SELECT id FROM organizations WHERE id=$1 FOR UPDATE", org); e != nil {
		return nil, e
	}
	rows, e := tx.Query(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND root_id=$2 ORDER BY depth,id FOR UPDATE", org, root)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	tasks := []domain.Task{}
	for rows.Next() {
		t, e := scanTask(rows)
		if e != nil {
			return nil, e
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func BuildRunInput(in domain.RunInput) (string, string, error) {
	invalid := domain.Err("invalid_request", "Контекст выполнения превышает границы или не соответствует источникам", 400)
	if !validText(in.OriginalGoal, 16384) || len(in.LocalGoal) > 16384 || !utf8.ValidString(in.LocalGoal) || strings.ContainsRune(in.LocalGoal, 0) || len(in.Feedback) > 8192 || !utf8.ValidString(in.Feedback) || strings.ContainsRune(in.Feedback, 0) || len(in.Sources) > 32 {
		return "", "", invalid
	}
	if len(in.ParentContext) > 0 && (!json.Valid(in.ParentContext) || len(in.ParentContext) > domain.MaxRunInputBytes) {
		return "", "", invalid
	}
	if in.Sources == nil {
		in.Sources = []domain.RunSource{}
	}
	for _, s := range in.Sources {
		if !validText(s.TaskID, 128) || !validText(s.CandidateID, 128) || !validText(s.ArtifactID, 128) || s.ArtifactVersion < 1 || !validDigest(s.ContentDigest) || !validText(s.Content, 262144) || Hash([]byte(s.Content)) != s.ContentDigest {
			return "", "", invalid
		}
	}
	body, e := json.Marshal(in)
	if e != nil || len(body) > domain.MaxRunInputBytes {
		return "", "", invalid
	}
	return string(body), Hash(body), nil
}

func loadRunInputTx(ctx context.Context, tx pgx.Tx, org, taskID, runID string) (string, error) {
	var input, digest string
	if e := tx.QueryRow(ctx, "SELECT input_context,input_digest FROM runs WHERE organization_id=$1 AND id=$2 AND task_id=$3", org, runID, taskID).Scan(&input, &digest); e != nil {
		return "", rowExists(e)
	}
	if (input != "" && (len(input) > domain.MaxRunInputBytes || !json.Valid([]byte(input)) || Hash([]byte(input)) != digest)) || (input == "" && digest != "") {
		return "", domain.Err("conflict", "Сохранённый контекст повреждён", 409)
	}
	return input, nil
}

const candidateColumns = "id,organization_id,task_id,run_id,operation_id,artifact_id,artifact_version,content_digest,source,created_at,COALESCE(parent_candidate_id,''),COALESCE(review_basis_candidate_id,''),COALESCE(material_id,''),COALESCE(material_version,0),COALESCE(published_by,'')"

func scanCandidate(row scanner) (c domain.TaskCandidate, e error) {
	e = row.Scan(&c.ID, &c.OrganizationID, &c.TaskID, &c.RunID, &c.OperationID, &c.ArtifactID, &c.ArtifactVersion, &c.ContentDigest, &c.Source, &c.CreatedAt, &c.ParentCandidateID, &c.ReviewBasisCandidateID, &c.MaterialID, &c.MaterialVersion, &c.PublishedBy)
	return c, rowExists(e)
}
func loadCandidateTx(ctx context.Context, tx pgx.Tx, org, id string) (domain.TaskCandidate, domain.Artifact, error) {
	c, e := scanCandidate(tx.QueryRow(ctx, "SELECT "+candidateColumns+" FROM task_candidates WHERE organization_id=$1 AND id=$2", org, id))
	var a domain.Artifact
	if e != nil {
		return c, a, e
	}
	var provenance []byte
	e = tx.QueryRow(ctx, "SELECT id,organization_id,task_id,name,media_type,content,version,created_at,provenance FROM artifacts WHERE organization_id=$1 AND id=$2", org, c.ArtifactID).Scan(&a.ID, &a.OrganizationID, &a.TaskID, &a.Name, &a.MediaType, &a.Content, &a.Version, &a.CreatedAt, &provenance)
	if e != nil {
		return c, a, rowExists(e)
	}
	if a.TaskID != c.TaskID || a.Version != c.ArtifactVersion || Hash([]byte(a.Content)) != c.ContentDigest {
		return c, a, domain.Err("conflict", "Содержимое материала не соответствует сохранённому результату", 409)
	}
	e = json.Unmarshal(provenance, &a.Provenance)
	return c, a, e
}

func RegisterCandidateTx(ctx context.Context, tx pgx.Tx, task domain.Task, runID, opID string, artifact domain.Artifact, source string) (domain.TaskCandidate, error) {
	var empty domain.TaskCandidate
	if source != "model" && source != "model_tools" && source != "team" && source != "fixture" {
		return empty, domain.Forbidden
	}
	var content string
	var version int
	var current string
	var bound bool
	if e := tx.QueryRow(ctx, "SELECT content,version FROM artifacts WHERE organization_id=$1 AND id=$2 AND task_id=$3", task.OrganizationID, artifact.ID, task.ID).Scan(&content, &version); e != nil {
		return empty, rowExists(e)
	}
	if e := tx.QueryRow(ctx, "SELECT COALESCE(current_run_id,'') FROM tasks WHERE organization_id=$1 AND id=$2", task.OrganizationID, task.ID).Scan(&current); e != nil {
		return empty, rowExists(e)
	}
	if e := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE organization_id=$1 AND id=$2 AND task_id=$3 AND run_id=$4)", task.OrganizationID, opID, task.ID, runID).Scan(&bound); e != nil {
		return empty, e
	}
	if current != runID || !bound || !validText(content, 262144) {
		return empty, domain.Err("stale_lease", "Результат относится к другой попытке", 409)
	}
	c, e := scanCandidate(tx.QueryRow(ctx, "INSERT INTO task_candidates(organization_id,id,task_id,run_id,operation_id,artifact_id,artifact_version,content_digest,source) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING "+candidateColumns, task.OrganizationID, domain.NewID("cand"), task.ID, runID, opID, artifact.ID, version, Hash([]byte(content)), source))
	if e != nil {
		return empty, e
	}
	result, e := tx.Exec(ctx, "UPDATE tasks SET candidate_id=$3 WHERE organization_id=$1 AND id=$2 AND current_run_id=$4", task.OrganizationID, task.ID, c.ID, runID)
	if e != nil {
		return empty, e
	}
	if result.RowsAffected() != 1 {
		return empty, domain.Err("stale_lease", "Попытка задачи изменилась", 409)
	}
	return c, nil
}

const decisionColumns = "id,organization_id,task_id,run_id,candidate_id,artifact_id,artifact_version,content_digest,decision,reason,principal_id,method,task_version_before,task_version_after,COALESCE(parent_decision_id,''),created_at"

func scanDecision(row scanner) (d domain.TaskDecision, e error) {
	e = row.Scan(&d.ID, &d.OrganizationID, &d.TaskID, &d.RunID, &d.CandidateID, &d.ArtifactID, &d.ArtifactVersion, &d.ContentDigest, &d.Decision, &d.Reason, &d.PrincipalID, &d.Method, &d.TaskVersionBefore, &d.TaskVersionAfter, &d.ParentDecisionID, &d.CreatedAt)
	return d, rowExists(e)
}
func recordDecisionTx(ctx context.Context, tx pgx.Tx, d domain.TaskDecision) (domain.TaskDecision, error) {
	if d.ID == "" {
		d.ID = domain.NewID("decision")
	}
	return scanDecision(tx.QueryRow(ctx, `INSERT INTO task_decisions(organization_id,id,task_id,run_id,candidate_id,artifact_id,artifact_version,content_digest,decision,reason,principal_id,method,task_version_before,task_version_after,parent_decision_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,NULLIF($15,'')) RETURNING `+decisionColumns, d.OrganizationID, d.ID, d.TaskID, d.RunID, d.CandidateID, d.ArtifactID, d.ArtifactVersion, d.ContentDigest, d.Decision, d.Reason, d.PrincipalID, d.Method, d.TaskVersionBefore, d.TaskVersionAfter, d.ParentDecisionID))
}
func (s *Store) Candidates(ctx context.Context, p domain.Principal, taskID, raw string, limit int) ([]domain.TaskCandidate, string, error) {
	return listExecution(ctx, s, p, taskID, raw, limit, "task_candidates", candidateColumns, func(row scanner) (domain.TaskCandidate, time.Time, string, error) {
		c, e := scanCandidate(row)
		return c, c.CreatedAt, c.ID, e
	})
}
func (s *Store) Decisions(ctx context.Context, p domain.Principal, taskID, raw string, limit int) ([]domain.TaskDecision, string, error) {
	return listExecution(ctx, s, p, taskID, raw, limit, "task_decisions", decisionColumns, func(row scanner) (domain.TaskDecision, time.Time, string, error) {
		d, e := scanDecision(row)
		return d, d.CreatedAt, d.ID, e
	})
}
func taskMutationPermission(ctx context.Context, tx pgx.Tx, p domain.Principal, id, own, any string) error {
	var owner string
	if e := tx.QueryRow(ctx, "SELECT owner_id FROM tasks WHERE organization_id=$1 AND id=$2", p.OrganizationID, id).Scan(&owner); e != nil {
		return rowExists(e)
	}
	if (owner == p.ID && domain.PrincipalHasPermission(p, own)) || domain.PrincipalHasPermission(p, any) {
		return nil
	}
	return domain.Forbidden
}

func findTreeTask(tasks []domain.Task, id string) (domain.Task, error) {
	for _, t := range tasks {
		if t.ID == id {
			return t, nil
		}
	}
	return domain.Task{}, domain.NotFound
}

func subtreeIDs(tasks []domain.Task, parent string, include bool) []string {
	seen := map[string]bool{parent: true}
	ids := []string{}
	for _, t := range tasks {
		if (t.ID == parent && include) || (t.ParentID != "" && seen[t.ParentID]) {
			ids = append(ids, t.ID)
			seen[t.ID] = true
		}
	}
	return ids
}

func (s *Store) Children(ctx context.Context, p domain.Principal, id, raw string, limit int) ([]domain.Task, string, error) {
	if limit < 1 || limit > 100 {
		return nil, "", domain.Err("invalid_request", "Некорректный limit", 400)
	}
	tx, e := s.beginMaterialRead(ctx, p, id)
	if e != nil {
		return nil, "", e
	}
	defer tx.Rollback(ctx)
	position, e := decodeCursor(raw)
	if e != nil {
		return nil, "", e
	}
	rows, e := tx.Query(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND parent_id=$2 AND (created_at,id)<($3,$4) ORDER BY created_at DESC,id DESC LIMIT $5", p.OrganizationID, id, position.CreatedAt, position.ID, limit+1)
	if e != nil {
		return nil, "", e
	}
	defer rows.Close()
	out := []domain.Task{}
	for rows.Next() {
		v, e := scanTask(rows)
		if e != nil {
			return nil, "", e
		}
		out = append(out, v)
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
	return out, next, tx.Commit(ctx)
}

// Kept local so callers can recognize a structural contention without leaking
// database details or treating it as a failed external attempt.
var errTreeBusy = errors.New("task tree is busy")
