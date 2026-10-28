// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

func optionalText(s string, max int) bool {
	return len(s) <= max && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
func currentActorTx(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
	var id, org, role, kind string
	if e := tx.QueryRow(ctx, authQuery(p), p.TokenHash, p.OrganizationID).Scan(&id, &org, &role, &kind); e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return domain.Unauthenticated
		}
		return e
	}
	if id != p.ID || org != p.OrganizationID || role != p.Role || kind != p.Kind {
		return domain.Unauthenticated
	}
	return nil
}

func (s *Store) ReviewTask(ctx context.Context, p domain.Principal, id, key string, in domain.ReviewTaskRequest) (json.RawMessage, error) {
	if in.ExpectedVersion < 1 || !validText(in.CandidateID, 128) || !validText(in.ArtifactID, 128) || in.ArtifactVersion < 1 || !validDigest(in.ContentDigest) || !optionalText(in.Reason, 4096) {
		return nil, domain.Err("invalid_request", "Нужны точная версия задачи и сохранённый результат", 400)
	}
	if in.Decision != "accept" && in.Decision != "reject" && in.Decision != "request_changes" {
		return nil, domain.Err("invalid_request", "Неизвестное решение", 400)
	}
	if in.Decision != "accept" && !validText(in.Reason, 4096) {
		return nil, domain.Err("invalid_request", "Укажите причину решения", 400)
	}
	guard := func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		return taskMutationPermission(ctx, tx, p, id, "tasks.review_own", "tasks.review_any")
	}
	return s.MutateGuarded(ctx, p, "POST /tasks/"+id+"/review", key, in, guard, func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		control, e := lockTaskTreeTx(ctx, tx, p.OrganizationID, id)
		if e != nil {
			return nil, e
		}
		tasks, e := lockTreeTasksTx(ctx, tx, p.OrganizationID, control.RootID)
		if e != nil {
			return nil, e
		}
		task, e := findTreeTask(tasks, id)
		if e != nil {
			return nil, e
		}
		if e = noActiveToolExecutionsTx(ctx, tx, p.OrganizationID, subtreeIDs(tasks, id, true)); e != nil {
			return nil, e
		}
		if task.Version != in.ExpectedVersion {
			return nil, domain.Err("conflict", "Задача изменилась. Обновите результат и примите решение заново", 409)
		}
		if task.Status != "waiting_input" || (!candidateWaitingReason(task.WaitingReason) && task.WaitingReason != "model_tools_pending") || task.CandidateID != in.CandidateID {
			return nil, domain.Err("invalid_transition", "Этот результат больше не ожидает решения", 409)
		}
		if task.ParentID != "" {
			var consumed bool
			if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM team_members WHERE organization_id=$1 AND child_task_id=$2 AND selected_candidate_id IS NOT NULL)", p.OrganizationID, id).Scan(&consumed); e != nil {
				return nil, e
			}
			if consumed {
				return nil, domain.Err("conflict", "Этот результат уже включён в работу команды; решение принимается по её итогу", 409)
			}
		}
		candidate, artifact, e := loadCandidateTx(ctx, tx, p.OrganizationID, in.CandidateID)
		if e != nil {
			return nil, e
		}
		if candidate.TaskID != id || candidate.RunID != task.CurrentRunID || candidate.ArtifactID != in.ArtifactID || candidate.ArtifactVersion != in.ArtifactVersion || candidate.ContentDigest != in.ContentDigest {
			return nil, domain.Err("conflict", "Решение относится к другой версии материала", 409)
		}
		if candidate.Source == "model_tools" && in.Decision == "accept" {
			return nil, domain.Err("invalid_transition", "Запросы инструментов не являются итоговым ответом задачи", 409)
		}
		var currentOp bool
		if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE organization_id=$1 AND id=$2 AND task_id=$3 AND run_id=$4 AND status='blocked')", p.OrganizationID, candidate.OperationID, id, task.CurrentRunID).Scan(&currentOp); e != nil {
			return nil, e
		}
		if !currentOp {
			return nil, domain.Err("conflict", "Попытка результата изменилась", 409)
		}
		subtree := subtreeIDs(tasks, id, true)
		var active bool
		if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE organization_id=$1 AND task_id=ANY($2::text[]) AND status IN('queued','leased'))", p.OrganizationID, subtree).Scan(&active); e != nil {
			return nil, e
		}
		if active {
			return nil, domain.Err("conflict", "Подзадачи ещё выполняются", 409)
		}
		// No cost admission: a late invoice/hold cannot prevent a decision about an
		// existing output, and this command cannot settle or free any liability.
		if e = currentActorTx(ctx, tx, p); e != nil {
			return nil, e
		}
		d, e := recordDecisionTx(ctx, tx, domain.TaskDecision{OrganizationID: p.OrganizationID, TaskID: id, RunID: task.CurrentRunID, CandidateID: candidate.ID, ArtifactID: artifact.ID, ArtifactVersion: artifact.Version, ContentDigest: candidate.ContentDigest, Decision: in.Decision, Reason: in.Reason, PrincipalID: p.ID, Method: "authorized_user_decision", TaskVersionBefore: task.Version, TaskVersionAfter: task.Version + 1})
		if e != nil {
			return nil, e
		}
		// A native tool request can be rejected or returned for changes, but is
		// never a final team result or an execution closure to inherit.
		if candidate.Source != "model_tools" {
			if e = finalizeTeamDecisionTx(ctx, tx, task, d); e != nil {
				return nil, e
			}
		}
		status, reason, result, opStatus, accepted := "failed", "", "", "failed", ""
		if in.Decision == "accept" {
			status, result, opStatus, accepted = "succeeded", artifact.Content, "succeeded", d.ID
		}
		if in.Decision == "request_changes" {
			status, reason = "blocked", "candidate_changes_requested"
		}
		if _, e = tx.Exec(ctx, "UPDATE operations SET status=$3,lease_owner=NULL,lease_until=NULL WHERE organization_id=$1 AND id=$2", p.OrganizationID, candidate.OperationID, opStatus); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, "UPDATE runs SET status=$3 WHERE organization_id=$1 AND id=$2", p.OrganizationID, task.CurrentRunID, opStatus); e != nil {
			return nil, e
		}
		task, e = scanTask(tx.QueryRow(ctx, "UPDATE tasks SET status=$3,waiting_reason=$4,result=$5,accepted_decision_id=NULLIF($6,''),version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2 RETURNING "+taskColumns, p.OrganizationID, id, status, reason, result, accepted))
		if e != nil {
			return nil, e
		}
		return task, emit(ctx, tx, p.OrganizationID, id, "task.result_decided", map[string]any{"decision_id": d.ID, "candidate_id": candidate.ID, "run_id": candidate.RunID, "decision": in.Decision, "method": d.Method, "principal_id": p.ID, "content_digest": candidate.ContentDigest, "costs_unchanged": true})
	})
}

var retryReasons = map[string]bool{
	"model_attempt_requires_attention": true, "automation_authorization_revoked": true, "authorization_revoked": true,
	"candidate_changes_requested": true, "model_output_rejected": true, "model_outcome_unknown": true, "model_accounting_pending": true,
	"model_credentials_unavailable": true, "model_profile_unavailable": true, "model_input_rejected": true, "fixture_input_too_large": true,
	"attempt_limit": true, "team_member_failed": true, "team_input_too_large": true, "team_output_too_large": true,
}

func (s *Store) RetryTask(ctx context.Context, p domain.Principal, id, key string, in domain.RetryTaskRequest) (json.RawMessage, error) {
	if in.ExpectedVersion < 1 || !in.AcknowledgeNewCost || !validText(in.Reason, 4096) || !optionalText(in.Feedback, 8192) {
		return nil, domain.Err("invalid_request", "Подтвердите новую попытку и её возможный расход, укажите причину", 400)
	}
	guard := func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		return taskMutationPermission(ctx, tx, p, id, "tasks.retry_own", "tasks.retry_any")
	}
	return s.MutateGuarded(ctx, p, "POST /tasks/"+id+"/retry", key, in, guard, func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		if e := lockAdmissionOwnerTx(ctx, tx, p.OrganizationID, id); e != nil {
			return nil, e
		}
		control, e := lockTaskTreeTx(ctx, tx, p.OrganizationID, id)
		if e != nil {
			return nil, e
		}
		tasks, e := lockTreeTasksTx(ctx, tx, p.OrganizationID, control.RootID)
		if e != nil {
			return nil, e
		}
		task, e := findTreeTask(tasks, id)
		if e != nil {
			return nil, e
		}
		if e = noActiveToolExecutionsTx(ctx, tx, p.OrganizationID, subtreeIDs(tasks, id, true)); e != nil {
			return nil, e
		}
		if task.Version != in.ExpectedVersion {
			return nil, domain.Err("conflict", "Версия задачи изменилась", 409)
		}
		if task.ParentID != "" || task.Status != "blocked" || !retryReasons[task.WaitingReason] {
			return nil, domain.Err("invalid_transition", "Новая попытка недоступна для этого состояния", 409)
		}
		if task.AttemptCount >= task.MaxRuns || control.UsedAttempts >= control.MaxTotalAttempts {
			return nil, domain.Err("invalid_transition", "Исчерпан общий предел попыток", 409)
		}
		if task.WaitingReason == "candidate_changes_requested" && !validText(in.Feedback, 8192) {
			return nil, domain.Err("invalid_request", "Укажите замечания для новой попытки", 400)
		}
		if task.WaitingReason == "candidate_changes_requested" && task.CandidateID == "" {
			return nil, domain.Err("conflict", "Результат для исправлений отсутствует", 409)
		}
		// Validate stored state as well as its presentation reason. A new operation
		// is deliberate, separately admitted work; it is never a retry of a sent ID.
		var running bool
		if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE organization_id=$1 AND task_id=$2 AND run_id=$3 AND status IN('queued','leased'))", p.OrganizationID, id, task.CurrentRunID).Scan(&running); e != nil {
			return nil, e
		}
		if running {
			return nil, domain.Err("conflict", "Предыдущая попытка ещё выполняется", 409)
		}
		sources := []domain.RunSource{}
		if task.CandidateID != "" {
			c, a, e := loadCandidateTx(ctx, tx, p.OrganizationID, task.CandidateID)
			if e != nil {
				return nil, e
			}
			if c.RunID != task.CurrentRunID || c.TaskID != task.ID {
				return nil, domain.Err("conflict", "Источник новой попытки устарел", 409)
			}
			if task.WaitingReason == "candidate_changes_requested" {
				var changes bool
				if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM task_decisions WHERE organization_id=$1 AND candidate_id=$2 AND decision='request_changes')", p.OrganizationID, c.ID).Scan(&changes); e != nil {
					return nil, e
				}
				if !changes {
					return nil, domain.Err("conflict", "Решение об исправлениях отсутствует", 409)
				}
			}
			sources = append(sources, domain.RunSource{TaskID: task.ID, CandidateID: c.ID, ArtifactID: a.ID, ArtifactVersion: a.Version, ContentDigest: c.ContentDigest, Content: a.Content})
		}
		input, digest, e := BuildRunInput(domain.RunInput{OriginalGoal: task.Goal, Feedback: in.Feedback, Sources: sources})
		if e != nil {
			return nil, e
		}
		scopes, e := lockCostScopes(ctx, tx, p.OrganizationID, id)
		if e != nil {
			return nil, e
		}
		for i := range scopes.tasks {
			if scopes.tasks[i].ID == id {
				scopes.tasks[i].Status = "queued"
			}
		}
		if e = scopes.admit(0); e != nil {
			return nil, e
		}
		agent, e := scanAgent(tx.QueryRow(ctx, "SELECT "+agentColumns+" FROM agents WHERE organization_id=$1 AND id=$2 AND version=$3", p.OrganizationID, task.AgentID, task.AgentVersion))
		if e != nil {
			return nil, e
		}
		if agent.Team == nil {
			if e = checkTaskModelAdmissionTx(ctx, tx, task, nil, nil); e != nil {
				return nil, e
			}
		}
		if e = currentActorTx(ctx, tx, p); e != nil {
			return nil, e
		}
		// A model profile lock can outlive the admission clock read above. The
		// lifetime deadline is checked again after those waits before new work.
		if e = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&scopes.now); e != nil {
			return nil, e
		}
		if e = scopes.admit(0); e != nil {
			return nil, e
		}
		descendants := subtreeIDs(tasks, id, false)
		if e = cancelTasksTx(ctx, tx, p.OrganizationID, descendants, "superseded_by_new_attempt"); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, "UPDATE operations SET status='cancelled',lease_owner=NULL,lease_until=NULL WHERE organization_id=$1 AND task_id=$2 AND status NOT IN('succeeded','failed','cancelled')", p.OrganizationID, id); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, "UPDATE runs SET status='failed' WHERE organization_id=$1 AND task_id=$2 AND status NOT IN('succeeded','failed','cancelled')", p.OrganizationID, id); e != nil {
			return nil, e
		}
		if e = cancelTeamExecutionsTx(ctx, tx, p.OrganizationID, []string{id}); e != nil {
			return nil, e
		}
		// A blocked task already occupies a queue slot. New team children are
		// materialized under the same organization lock and counted by initTeamTx.
		run, op := domain.NewID("run"), domain.NewID("op")
		if _, e = tx.Exec(ctx, `INSERT INTO runs(organization_id,id,task_id,principal_id,agent_id,agent_version,status,attempt,input_context,input_digest,feedback,source_candidate_id)
   VALUES($1,$2,$3,$4,$5,$6,'queued',$7,$8,$9,$10,NULLIF($11,''))`, p.OrganizationID, run, id, p.ID, task.AgentID, task.AgentVersion, task.AttemptCount+1, input, digest, in.Feedback, task.CandidateID); e != nil {
			return nil, e
		}
		opStatus := "queued"
		if agent.Team != nil {
			opStatus = "blocked"
		}
		if _, e = tx.Exec(ctx, "INSERT INTO operations(organization_id,id,task_id,run_id,status) VALUES($1,$2,$3,$4,$5)", p.OrganizationID, op, id, run, opStatus); e != nil {
			return nil, e
		}
		task, e = scanTask(tx.QueryRow(ctx, "UPDATE tasks SET status='queued',waiting_reason='',result='',version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2 RETURNING "+taskColumns, p.OrganizationID, id))
		if e != nil {
			return nil, e
		}
		if agent.Team != nil {
			if e = initTeamTx(ctx, tx, task, run, *agent.Team); e != nil {
				return nil, e
			}
			task, e = scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND id=$2", p.OrganizationID, id))
			if e != nil {
				return nil, e
			}
		}
		// Nested team initialization may acquire additional pinned-profile locks.
		// If their wait consumed the deadline, roll back this entire new attempt.
		if e = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&scopes.now); e != nil {
			return nil, e
		}
		if e = scopes.admit(0); e != nil {
			return nil, e
		}
		return task, emit(ctx, tx, p.OrganizationID, id, "task.new_attempt", map[string]any{"run_id": run, "operation_id": op, "attempt": task.AttemptCount, "input_digest": digest, "reason": in.Reason, "acknowledged_new_cost": true, "prior_liabilities_preserved": true})
	})
}

func cancelTasksTx(ctx context.Context, tx pgx.Tx, org string, ids []string, reason string) error {
	if len(ids) == 0 {
		return nil
	}
	if e := cancelToolExecutionsTx(ctx, tx, org, ids); e != nil {
		return e
	}
	rows, e := tx.Query(ctx, "SELECT DISTINCT o.id,o.task_id FROM operations o JOIN reservations r ON r.organization_id=o.organization_id AND r.operation_id=o.id WHERE o.organization_id=$1 AND o.task_id=ANY($2::text[]) AND r.category='model' AND r.status='reserved' AND r.pending_quarantine_id IS NULL ORDER BY o.id", org, ids)
	if e != nil {
		return e
	}
	attempts := []domain.Lease{}
	for rows.Next() {
		l := domain.Lease{OrganizationID: org}
		if e = rows.Scan(&l.OperationID, &l.TaskID); e != nil {
			break
		}
		attempts = append(attempts, l)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return e
	}
	for _, l := range attempts {
		if e = markAbandonedModelUnknownTx(ctx, tx, &l, "task_cancelled_with_pending_accounting"); e != nil {
			return e
		}
	}
	if _, e := tx.Exec(ctx, "UPDATE operations SET status='cancelled',lease_owner=NULL,lease_until=NULL WHERE organization_id=$1 AND task_id=ANY($2::text[]) AND status NOT IN('succeeded','failed','cancelled')", org, ids); e != nil {
		return e
	}
	if _, e := tx.Exec(ctx, "UPDATE runs SET status='cancelled' WHERE organization_id=$1 AND task_id=ANY($2::text[]) AND status NOT IN('succeeded','failed','cancelled')", org, ids); e != nil {
		return e
	}
	if _, e := tx.Exec(ctx, "UPDATE tasks SET status='cancelled',waiting_reason=$3,version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=ANY($2::text[]) AND status NOT IN('succeeded','failed','cancelled')", org, ids, reason); e != nil {
		return e
	}
	if e := cancelTeamExecutionsTx(ctx, tx, org, ids); e != nil {
		return e
	}
	for _, id := range ids {
		if e := emit(ctx, tx, org, id, "task.cancelled", map[string]any{"reason": reason, "liabilities_preserved": true}); e != nil {
			return e
		}
	}
	return nil
}

func (s *Store) cancelTaskTree(ctx context.Context, p domain.Principal, id, key string) (json.RawMessage, error) {
	guard := func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		return taskMutationPermission(ctx, tx, p, id, "tasks.cancel_own", "tasks.cancel_any")
	}
	return s.MutateGuarded(ctx, p, "POST /tasks/"+id+"/cancel", key, struct{}{}, guard, func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		control, e := lockTaskTreeTx(ctx, tx, p.OrganizationID, id)
		if e != nil {
			return nil, e
		}
		tasks, e := lockTreeTasksTx(ctx, tx, p.OrganizationID, control.RootID)
		if e != nil {
			return nil, e
		}
		task, e := findTreeTask(tasks, id)
		if e != nil {
			return nil, e
		}
		if domain.Terminal(task.Status) {
			return task, nil
		}
		if task.ParentID != "" {
			var consumed bool
			if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM team_members WHERE organization_id=$1 AND child_task_id=$2 AND selected_candidate_id IS NOT NULL)", p.OrganizationID, id).Scan(&consumed); e != nil {
				return nil, e
			}
			if consumed {
				return nil, domain.Err("conflict", "Результат уже включён в работу команды; отмените её корневую задачу", 409)
			}
		}
		if e = currentActorTx(ctx, tx, p); e != nil {
			return nil, e
		}
		if e = cancelTasksTx(ctx, tx, p.OrganizationID, subtreeIDs(tasks, id, true), ""); e != nil {
			return nil, e
		}
		return scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND id=$2", p.OrganizationID, id))
	})
}
