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

type teamExecution struct {
	OrganizationID, TaskID, RunID, RootID, RootRunID, Status string
	Plan                                                     compiledTeamNode
}

type teamSlot struct {
	ID, ChildTaskID, ChildRunID, Status string
	Ordinal                             int
	Plan                                compiledTeamNode
	Source                              *domain.RunSource
}

// ReconcileTeam performs one bounded, durable transition. It never calls a
// provider and never holds a worker lease while waiting for another member or
// for an authorized result decision. All network execution stays in leaf runs.
func (s *Store) ReconcileTeam(ctx context.Context) (bool, error) {
	if err := s.CheckRuntimeAdmission(ctx); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var selected teamExecution
	err := s.Pool.QueryRow(ctx, `SELECT organization_id,task_id,run_id,root_id,root_run_id FROM team_executions WHERE status='running' AND next_check_at<=clock_timestamp() ORDER BY next_check_at,created_at,run_id LIMIT 1`).Scan(&selected.OrganizationID, &selected.TaskID, &selected.RunID, &selected.RootID, &selected.RootRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	// Authority precedes the structural gate, then ALL existing operations.
	// lockTaskTreeTx shares this order with cancellation/retry/final decisions.
	// NO KEY UPDATE serializes structural writers while allowing deferred FK
	// KEY SHARE checks by leaf transactions which already own an operation.
	authorityErr := lockRunAuthorityTx(ctx, tx, selected.OrganizationID, selected.TaskID, selected.RunID)
	if authorityErr != nil && !errors.Is(authorityErr, domain.Forbidden) {
		return false, authorityErr
	}
	var gate string
	if err = tx.QueryRow(ctx, "SELECT root_id FROM task_tree_controls WHERE organization_id=$1 AND root_id=$2 FOR NO KEY UPDATE SKIP LOCKED", selected.OrganizationID, selected.RootID).Scan(&gate); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	control, err := lockTaskTreeTx(ctx, tx, selected.OrganizationID, selected.TaskID)
	if err != nil {
		return false, err
	}
	var organizationActive bool
	if err = tx.QueryRow(ctx, "SELECT active FROM organizations WHERE id=$1 FOR UPDATE", selected.OrganizationID).Scan(&organizationActive); err != nil {
		return false, err
	}
	tasks, err := teamTasksTx(ctx, tx, selected.OrganizationID, control.RootID)
	if err != nil {
		return false, err
	}
	parent, ok := tasks[selected.TaskID]
	if !ok {
		return false, domain.NotFound
	}
	var raw []byte
	var digest string
	if err = tx.QueryRow(ctx, "SELECT plan,plan_digest,status FROM team_executions WHERE organization_id=$1 AND run_id=$2 FOR UPDATE", selected.OrganizationID, selected.RunID).Scan(&raw, &digest, &selected.Status); err != nil {
		return false, err
	}
	if selected.Status != "running" {
		return false, tx.Commit(ctx)
	}
	if err = json.Unmarshal(raw, &selected.Plan); err != nil || digestValue(selected.Plan) != digest || selected.Plan.TaskID != parent.ID || selected.Plan.Definition == nil {
		return false, domain.Err("conflict", "Сохранённый план команды повреждён", 409)
	}
	root := tasks[control.RootID]
	if parent.CurrentRunID != selected.RunID || root.CurrentRunID != selected.RootRunID || domain.Terminal(parent.Status) {
		if _, err = tx.Exec(ctx, "UPDATE team_executions SET status='cancelled',version=version+1 WHERE organization_id=$1 AND run_id=$2", selected.OrganizationID, selected.RunID); err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return false, err
	}
	if authorityErr == nil {
		authorityErr = checkRunAuthorityTx(ctx, tx, selected.OrganizationID, selected.TaskID, selected.RunID)
	}
	if authorityErr != nil || !organizationActive {
		return finishTeamBlock(ctx, tx, selected, tasks, "authorization_revoked")
	}
	if !now.Before(parent.ExecutionDeadline) {
		return finishTeamBlock(ctx, tx, selected, tasks, "execution_deadline")
	}
	for ancestor := parent; ; ancestor = tasks[ancestor.ParentID] {
		if domain.Terminal(ancestor.Status) || ancestor.Status == "blocked" || ancestor.Status == "waiting_approval" {
			return finishTeamBlock(ctx, tx, selected, tasks, "parent_not_running")
		}
		if ancestor.ParentID == "" {
			break
		}
	}
	slots, err := teamSlotsTx(ctx, tx, selected)
	if err != nil {
		return false, err
	}
	if len(slots) != len(selected.Plan.Members) {
		return false, domain.Err("conflict", "Участники команды не соответствуют плану", 409)
	}
	for i, slot := range slots {
		if slot.Ordinal != i || slot.ID != selected.Plan.Members[i].ID || digestValue(slot.Plan) != digestValue(selected.Plan.Members[i].Node) {
			return false, domain.Err("conflict", "Снимок участника отличается от закрытого плана команды", 409)
		}
	}
	changed := false
	if parent.Status == "queued" {
		if _, err = tx.Exec(ctx, "UPDATE tasks SET status='running',waiting_reason='team_members_running',version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2", parent.OrganizationID, parent.ID); err != nil {
			return false, err
		}
		if _, err = tx.Exec(ctx, "UPDATE runs SET status='running' WHERE organization_id=$1 AND id=$2", parent.OrganizationID, selected.RunID); err != nil {
			return false, err
		}
		changed = true
	}
	for i := range slots {
		slot := &slots[i]
		child := tasks[slot.ChildTaskID]
		if slot.Status == "planned" && (domain.Terminal(child.Status) || child.Status == "blocked") {
			status := "failed"
			if child.Status == "cancelled" {
				status = "cancelled"
			}
			if err = closeTeamPlanSubtreeTx(ctx, tx, parent.OrganizationID, slot.Plan, "team_member_cancelled_before_start"); err != nil {
				return false, err
			}
			if _, err = tx.Exec(ctx, "UPDATE team_members SET status=$4 WHERE organization_id=$1 AND parent_run_id=$2 AND member_id=$3", parent.OrganizationID, selected.RunID, slot.ID, status); err != nil {
				return false, err
			}
			slot.Status, changed = status, true
			continue
		}
		if slot.Status != "running" {
			continue
		}
		if child.CurrentRunID != slot.ChildRunID || child.ParentID != parent.ID {
			return finishTeamBlock(ctx, tx, selected, tasks, "team_run_mismatch")
		}
		if child.Status == "blocked" && child.WaitingReason == "external_action_unknown" {
			continue
		}
		if child.CandidateID != "" && (child.Status == "waiting_input" || child.Status == "succeeded") {
			if child.WaitingReason == "model_tools_pending" {
				continue
			}
			source, _, err := teamCandidateSourceTx(ctx, tx, child, child.CandidateID)
			if err != nil {
				return false, err
			}
			if _, err = tx.Exec(ctx, `UPDATE team_members SET status='candidate',selected_candidate_id=$4,selected_artifact_id=$5,selected_artifact_version=$6,selected_content_digest=$7 WHERE organization_id=$1 AND parent_run_id=$2 AND member_id=$3 AND status='running'`, parent.OrganizationID, selected.RunID, slot.ID, source.CandidateID, source.ArtifactID, source.ArtifactVersion, source.ContentDigest); err != nil {
				return false, err
			}
			slot.Status, slot.Source = "candidate", &source
			changed = true
			if err = emit(ctx, tx, parent.OrganizationID, parent.ID, "team.member_candidate", map[string]any{"run_id": selected.RunID, "member_id": slot.ID, "child_task_id": child.ID, "candidate_id": source.CandidateID, "content_digest": source.ContentDigest, "acceptance": "not_implied"}); err != nil {
				return false, err
			}
		} else if domain.Terminal(child.Status) || child.Status == "blocked" {
			status := "failed"
			if child.Status == "cancelled" {
				status = "cancelled"
			}
			if err = closeTeamPlanSubtreeTx(ctx, tx, parent.OrganizationID, slot.Plan, "team_member_failed"); err != nil {
				return false, err
			}
			if _, err = tx.Exec(ctx, "UPDATE team_members SET status=$4 WHERE organization_id=$1 AND parent_run_id=$2 AND member_id=$3", parent.OrganizationID, selected.RunID, slot.ID, status); err != nil {
				return false, err
			}
			slot.Status, changed = status, true
		}
	}
	failed, ready, active := 0, 0, 0
	for _, slot := range slots {
		switch slot.Status {
		case "failed", "cancelled", "skipped":
			failed++
		case "candidate":
			ready++
		case "running":
			active++
		}
	}
	if failed > 0 && selected.Plan.Definition.JoinPolicy == "all" {
		return finishTeamBlock(ctx, tx, selected, tasks, "team_member_failed")
	}
	// A failed predecessor cannot be silently omitted from a dependent sequence.
	// allow_partial records the failed/skipped slots and joins only exact ready
	// snapshots; it does not improvise a new graph or feed stale output onward.
	if failed > 0 && selected.Plan.Definition.Pattern != "parallel" {
		for i := range slots {
			if slots[i].Status == "planned" {
				if err = closeTeamPlanSubtreeTx(ctx, tx, parent.OrganizationID, slots[i].Plan, "team_dependency_failed"); err != nil {
					return false, err
				}
				if _, err = tx.Exec(ctx, "UPDATE team_members SET status='skipped' WHERE organization_id=$1 AND parent_run_id=$2 AND member_id=$3", parent.OrganizationID, selected.RunID, slots[i].ID); err != nil {
					return false, err
				}
				slots[i].Status = "skipped"
				failed++
				changed = true
			}
		}
	}
	if ready+failed == len(slots) {
		if ready == 0 {
			return finishTeamBlock(ctx, tx, selected, tasks, "team_member_failed")
		}
		if err = createTeamCandidateTx(ctx, tx, selected, parent, slots); err != nil {
			var de *domain.Error
			if errors.As(err, &de) && de.Code == "team_output_too_large" {
				return finishTeamBlock(ctx, tx, selected, tasks, "team_output_too_large")
			}
			return false, err
		}
		return true, tx.Commit(ctx)
	}
	if active < selected.Plan.Definition.MaxParallel {
		for i := range slots {
			if slots[i].Status != "planned" {
				continue
			}
			if selected.Plan.Definition.Pattern != "parallel" && i > 0 && slots[i-1].Status != "candidate" {
				break
			}
			if control.UsedAttempts >= control.MaxTotalAttempts {
				return finishTeamBlock(ctx, tx, selected, tasks, "attempt_limit")
			}
			scopes, err := lockCostScopes(ctx, tx, parent.OrganizationID, slots[i].ChildTaskID)
			if err != nil {
				return false, err
			}
			if err = scopes.admit(0); err != nil {
				return finishTeamBlock(ctx, tx, selected, tasks, "budget_exceeded")
			}
			if err = activateTeamMemberTx(ctx, tx, selected, parent, tasks[slots[i].ChildTaskID], slots, i); err != nil {
				var de *domain.Error
				if errors.As(err, &de) && (de.Code == "team_input_too_large" || de.Code == "model_profile_unavailable" || de.Code == "execution_deadline") {
					return finishTeamBlock(ctx, tx, selected, tasks, de.Code)
				}
				return false, err
			}
			changed = true
			break // one admitted run per short transaction
		}
	}
	if _, err = tx.Exec(ctx, "UPDATE team_executions SET next_check_at=clock_timestamp()+interval '250 milliseconds',version=version+1 WHERE organization_id=$1 AND run_id=$2", parent.OrganizationID, selected.RunID); err != nil {
		return false, err
	}
	return changed, tx.Commit(ctx)
}

func teamTasksTx(ctx context.Context, tx pgx.Tx, org, root string) (map[string]domain.Task, error) {
	rows, err := tx.Query(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND root_id=$2 ORDER BY depth,id FOR UPDATE", org, root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]domain.Task{}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		result[task.ID] = task
	}
	return result, rows.Err()
}

func teamSlotsTx(ctx context.Context, tx pgx.Tx, execution teamExecution) ([]teamSlot, error) {
	rows, err := tx.Query(ctx, `SELECT member_id,ordinal,child_task_id,COALESCE(child_run_id,''),status,plan,plan_digest,COALESCE(selected_candidate_id,''),COALESCE(selected_artifact_id,''),COALESCE(selected_artifact_version,0),COALESCE(selected_content_digest,'') FROM team_members WHERE organization_id=$1 AND parent_run_id=$2 ORDER BY ordinal FOR UPDATE`, execution.OrganizationID, execution.RunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []teamSlot
	for rows.Next() {
		var slot teamSlot
		var raw []byte
		var digest string
		var source domain.RunSource
		if err = rows.Scan(&slot.ID, &slot.Ordinal, &slot.ChildTaskID, &slot.ChildRunID, &slot.Status, &raw, &digest, &source.CandidateID, &source.ArtifactID, &source.ArtifactVersion, &source.ContentDigest); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &slot.Plan); err != nil || digestValue(slot.Plan) != digest || slot.Plan.TaskID != slot.ChildTaskID {
			return nil, domain.Err("conflict", "Снимок участника команды повреждён", 409)
		}
		if source.CandidateID != "" {
			source.TaskID = slot.ChildTaskID
			slot.Source = &source
		}
		result = append(result, slot)
	}
	return result, rows.Err()
}

func teamCandidateSourceTx(ctx context.Context, tx pgx.Tx, task domain.Task, candidateID string) (domain.RunSource, string, error) {
	var source domain.RunSource
	var kind, runID string
	err := tx.QueryRow(ctx, `SELECT c.task_id,c.run_id,c.id,c.artifact_id,c.artifact_version,c.content_digest,c.source,a.content FROM task_candidates c JOIN artifacts a ON a.organization_id=c.organization_id AND a.id=c.artifact_id AND a.version=c.artifact_version AND a.task_id=c.task_id WHERE c.organization_id=$1 AND c.id=$2 AND c.task_id=$3`, task.OrganizationID, candidateID, task.ID).Scan(&source.TaskID, &runID, &source.CandidateID, &source.ArtifactID, &source.ArtifactVersion, &source.ContentDigest, &kind, &source.Content)
	if err != nil {
		return source, kind, rowExists(err)
	}
	if runID != task.CurrentRunID || task.CandidateID != candidateID || Hash([]byte(source.Content)) != source.ContentDigest {
		return source, kind, domain.Err("conflict", "Результат участника не соответствует текущему запуску", 409)
	}
	if kind == "model_tools" {
		return source, kind, domain.Err("invalid_transition", "Запрос инструмента не является результатом участника", 409)
	}
	return source, kind, nil
}
