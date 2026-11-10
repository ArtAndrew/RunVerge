// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"strings"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

const reviewerDirectoryLimit = 25

func canAssignReviewer(p domain.Principal, owner string) bool {
	return p.Kind == "human" && (domain.PrincipalHasPermission(p, "tasks.review_any") ||
		(p.ID == owner && domain.PrincipalHasPermission(p, "tasks.review_own")))
}

// The latest committed handoff for the exact candidate is the active one.
// Events are append-only, including after event replay windows advance.
const namedReviewer = `SELECT COALESCE((
 SELECT data->>'reviewer_principal_id' FROM events
 WHERE organization_id=$1 AND task_id=$2 AND type='task.review_assigned'
 AND payload_version=1 AND data->>'candidate_id'=$3
 ORDER BY sequence DESC LIMIT 1),'')`

func taskReviewPermission(ctx context.Context, tx pgx.Tx, p domain.Principal, id, candidateID string) error {
	var reviewer string
	if err := tx.QueryRow(ctx, namedReviewer, p.OrganizationID, id, candidateID).Scan(&reviewer); err != nil {
		return err
	}
	if reviewer == "" {
		return taskMutationPermission(ctx, tx, p, id, "tasks.review_own", "tasks.review_any")
	}
	if p.Kind == "human" && p.ID == reviewer && domain.PrincipalHasPermission(p, "tasks.review_own") {
		return nil
	}
	return domain.Forbidden
}

func (s *Store) TaskReviewAssignment(ctx context.Context, p domain.Principal, id, query string) (domain.TaskReviewAssignment, error) {
	view := domain.TaskReviewAssignment{Reviewers: []domain.TaskReviewer{}}
	query = strings.TrimSpace(query)
	if !optionalText(query, 120) {
		return view, domain.Err("invalid_request", "Введите имя до 120 байт", 400)
	}
	tx, p, err := s.beginPrincipalReadAs(ctx, p, "tasks.read")
	if err != nil {
		return view, err
	}
	defer tx.Rollback(ctx)
	var owner, parent, status, reason, publisher, author string
	// Read the task version and its assignment in one snapshot so clients can
	// bind the result to the exact candidate they are displaying.
	err = tx.QueryRow(ctx, `SELECT t.owner_id,COALESCE(t.parent_id,''),t.status,t.waiting_reason,t.version,COALESCE(t.candidate_id,''),
 COALESCE(c.published_by,''),COALESCE(r.principal_id,''),COALESCE(assignment.reviewer_id,''),COALESCE(reviewer.display_name,'')
 FROM tasks t
 LEFT JOIN task_candidates c ON c.organization_id=t.organization_id AND c.task_id=t.id AND c.id=t.candidate_id
 LEFT JOIN material_revisions r ON r.organization_id=c.organization_id AND r.material_id=c.material_id AND r.version=c.material_version
 LEFT JOIN LATERAL (SELECT e.data->>'reviewer_principal_id' AS reviewer_id FROM events e
  WHERE e.organization_id=t.organization_id AND e.task_id=t.id AND e.type='task.review_assigned'
  AND e.payload_version=1 AND e.data->>'candidate_id'=t.candidate_id
  ORDER BY e.sequence DESC LIMIT 1) assignment ON true
 LEFT JOIN memberships reviewer ON reviewer.organization_id=t.organization_id AND reviewer.principal_id=assignment.reviewer_id
 WHERE t.organization_id=$1 AND t.id=$2`, p.OrganizationID, id).
		Scan(&owner, &parent, &status, &reason, &view.TaskVersion, &view.CandidateID, &publisher, &author, &view.ReviewerPrincipalID, &view.ReviewerDisplayName)
	if err != nil {
		return view, rowExists(err)
	}
	waiting := status == "waiting_input" && view.CandidateID != "" && (candidateWaitingReason(reason) || reason == "model_tools_pending")
	view.CanAssign = waiting && parent == "" && candidateWaitingReason(reason) && canAssignReviewer(p, owner)
	if view.ReviewerPrincipalID == "" {
		view.CanReview = waiting && canAssignReviewer(p, owner)
	} else {
		view.CanReview = waiting && p.ID == view.ReviewerPrincipalID && p.Kind == "human" && domain.PrincipalHasPermission(p, "tasks.review_own")
	}
	if view.CanAssign {
		rows, err := tx.Query(ctx, `SELECT m.principal_id,m.display_name FROM memberships m
 JOIN principals a ON a.id=m.principal_id
 WHERE m.organization_id=$1 AND NOT (m.principal_id=ANY($2::text[]))
 AND m.active AND a.active AND a.kind='human' AND m.role IN ('member','admin','owner')
 AND ($3='' OR strpos(lower(m.display_name),lower($3))>0)
 ORDER BY lower(m.display_name),m.principal_id LIMIT $4`, p.OrganizationID, []string{owner, p.ID, publisher, author}, query, reviewerDirectoryLimit+1)
		if err != nil {
			return view, err
		}
		view.Reviewers, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (r domain.TaskReviewer, err error) {
			err = row.Scan(&r.PrincipalID, &r.DisplayName)
			return
		})
		if err != nil {
			return view, err
		}
		if len(view.Reviewers) > reviewerDirectoryLimit {
			view.Reviewers = view.Reviewers[:reviewerDirectoryLimit]
			view.MoreReviewers = true
		}
		if view.Reviewers == nil {
			view.Reviewers = []domain.TaskReviewer{}
		}
	}
	return view, tx.Commit(ctx)
}

func (s *Store) AssignTaskReviewer(ctx context.Context, p domain.Principal, id, key string, in domain.AssignTaskReviewer) (json.RawMessage, error) {
	if in.ExpectedVersion < 1 || !validText(in.CandidateID, 128) || !validText(in.ReviewerPrincipalID, 128) {
		return nil, domain.Err("invalid_request", "Укажите версию задачи, черновик и проверяющего", 400)
	}
	before := func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		// Lock both actors before any task tree or organization money lock.
		return lockAuthorityPrincipalsTx(ctx, tx, p.OrganizationID, []string{p.ID, in.ReviewerPrincipalID})
	}
	guard := func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		var owner string
		if err := tx.QueryRow(ctx, "SELECT owner_id FROM tasks WHERE organization_id=$1 AND id=$2", p.OrganizationID, id).Scan(&owner); err != nil {
			return rowExists(err)
		}
		if !canAssignReviewer(p, owner) || in.ReviewerPrincipalID == owner || in.ReviewerPrincipalID == p.ID {
			return domain.Forbidden
		}
		return nil
	}
	return s.mutateWithPreAuthorization(ctx, p, "POST /tasks/"+id+"/review-assignment", key, in, before, guard,
		func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
			control, err := lockTaskTreeTx(ctx, tx, p.OrganizationID, id)
			if err != nil {
				return nil, err
			}
			tasks, err := lockTreeTasksTx(ctx, tx, p.OrganizationID, control.RootID)
			if err != nil {
				return nil, err
			}
			task, err := findTreeTask(tasks, id)
			if err != nil {
				return nil, err
			}
			if task.Version != in.ExpectedVersion || task.CandidateID != in.CandidateID {
				return nil, domain.Err("conflict", "Черновик изменился. Обновите задачу перед назначением", 409)
			}
			if task.ParentID != "" || task.Status != "waiting_input" || !candidateWaitingReason(task.WaitingReason) {
				return nil, domain.Err("invalid_transition", "Назначение доступно для итогового черновика задачи", 409)
			}
			var reviewerRole string
			err = tx.QueryRow(ctx, `SELECT m.role FROM memberships m
 JOIN principals a ON a.id=m.principal_id
 JOIN task_candidates c ON c.organization_id=m.organization_id AND c.task_id=$3 AND c.id=$4
 LEFT JOIN material_revisions r ON r.organization_id=c.organization_id AND r.material_id=c.material_id AND r.version=c.material_version
 WHERE m.organization_id=$1 AND m.principal_id=$2 AND m.active AND a.active AND a.kind='human'
 AND m.principal_id IS DISTINCT FROM c.published_by AND m.principal_id IS DISTINCT FROM r.principal_id`,
				p.OrganizationID, in.ReviewerPrincipalID, id, task.CandidateID).Scan(&reviewerRole)
			if err == pgx.ErrNoRows || (err == nil && !domain.HasPermission(reviewerRole, "tasks.review_own")) {
				return nil, domain.Err("invalid_request", "Выберите активного коллегу с правом ревью", 400)
			}
			if err != nil {
				return nil, err
			}
			task, err = scanTask(tx.QueryRow(ctx, "UPDATE tasks SET version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2 RETURNING "+taskColumns, p.OrganizationID, id))
			if err != nil {
				return nil, err
			}
			return task, emit(ctx, tx, p.OrganizationID, id, "task.review_assigned", map[string]any{
				"candidate_id": in.CandidateID, "reviewer_principal_id": in.ReviewerPrincipalID,
				"assigned_by_principal_id": p.ID, "task_version": task.Version,
			})
		})
}
