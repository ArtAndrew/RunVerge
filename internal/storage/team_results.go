// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

func activateTeamMemberTx(ctx context.Context, tx pgx.Tx, execution teamExecution, parent, child domain.Task, slots []teamSlot, index int) error {
	slot := slots[index]
	if child.ID != slot.Plan.TaskID || child.ParentID != parent.ID || child.CurrentRunID != "" || child.AttemptCount != 0 || child.Status != "waiting_input" || child.WaitingReason != "team_dependency_pending" || child.AgentID != slot.Plan.AgentID || child.AgentVersion != slot.Plan.AgentVersion || child.ModelConnectionID != slot.Plan.ModelConnectionID || child.ModelConnectionVersion != slot.Plan.ModelConnectionVersion {
		return domain.Err("conflict", "Участник уже запущен или не соответствует плану", 409)
	}
	if child.ModelConnectionID != "" {
		var active bool
		if err := tx.QueryRow(ctx, "SELECT active FROM model_connections WHERE organization_id=$1 AND id=$2 AND version=$3 FOR SHARE", child.OrganizationID, child.ModelConnectionID, child.ModelConnectionVersion).Scan(&active); err != nil {
			return err
		}
		if !active {
			return domain.Err("model_profile_unavailable", "Закреплённая модель участника отключена", 409)
		}
	}
	var parentContext, authorityPrincipal string
	if err := tx.QueryRow(ctx, "SELECT input_context,principal_id FROM runs WHERE organization_id=$1 AND id=$2 AND task_id=$3", parent.OrganizationID, execution.RunID, parent.ID).Scan(&parentContext, &authorityPrincipal); err != nil {
		return err
	}
	input := domain.RunInput{OriginalGoal: parent.Goal, LocalGoal: child.Goal, Sources: []domain.RunSource{}}
	if parentContext != "" {
		var previous domain.RunInput
		if err := json.Unmarshal([]byte(parentContext), &previous); err != nil {
			return domain.Err("conflict", "Контекст родительского запуска повреждён", 409)
		}
		input.OriginalGoal = previous.OriginalGoal
		input.ParentContext = json.RawMessage(parentContext)
	}
	var sourceCandidate string
	if execution.Plan.Definition.Pattern != "parallel" && index > 0 {
		previous := slots[index-1]
		if previous.Source == nil || previous.Status != "candidate" {
			return domain.Err("conflict", "Предыдущий участник ещё не подготовил результат", 409)
		}
		predecessor, err := scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND id=$2", parent.OrganizationID, previous.ChildTaskID))
		if err != nil {
			return err
		}
		source, _, err := teamCandidateSourceTx(ctx, tx, predecessor, previous.Source.CandidateID)
		if err != nil {
			return err
		}
		if source.ArtifactID != previous.Source.ArtifactID || source.ArtifactVersion != previous.Source.ArtifactVersion || source.ContentDigest != previous.Source.ContentDigest {
			return domain.Err("conflict", "Выбранный результат участника изменился", 409)
		}
		input.Sources = []domain.RunSource{source}
		sourceCandidate = source.CandidateID
	}
	contextBody, contextDigest, err := BuildRunInput(input)
	if err != nil {
		return domain.Err("team_input_too_large", "Точный контекст команды превышает допустимый размер", 409)
	}
	// A profile lock can be held beyond the deadline without changing any row.
	// Re-read database wall time after every admission wait and input build; the
	// earlier parent/scopes timestamp is not an admission lease.
	var withinDeadline bool
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp() < LEAST($1::timestamptz,$2::timestamptz)", parent.ExecutionDeadline, child.ExecutionDeadline).Scan(&withinDeadline); err != nil {
		return err
	}
	if !withinDeadline {
		return domain.Err("execution_deadline", "Общий срок выполнения команды истёк", 409)
	}
	if err = checkRunAuthorityTx(ctx, tx, parent.OrganizationID, parent.ID, execution.RunID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE tasks SET status='queued',waiting_reason='',version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2", child.OrganizationID, child.ID); err != nil {
		return err
	}
	runID, operationID := domain.NewID("run"), domain.NewID("op")
	if _, err = tx.Exec(ctx, `INSERT INTO runs(organization_id,id,task_id,principal_id,agent_id,agent_version,status,attempt,input_context,input_digest,source_candidate_id) VALUES($1,$2,$3,$4,$5,$6,'queued',1,$7,$8,NULLIF($9,''))`, child.OrganizationID, runID, child.ID, authorityPrincipal, child.AgentID, child.AgentVersion, contextBody, contextDigest, sourceCandidate); err != nil {
		return err
	}
	operationStatus := "queued"
	if slot.Plan.Definition != nil {
		operationStatus = "blocked"
	}
	if _, err = tx.Exec(ctx, `INSERT INTO operations(organization_id,id,task_id,run_id,status) VALUES($1,$2,$3,$4,$5)`, child.OrganizationID, operationID, child.ID, runID, operationStatus); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE team_members SET status='running',child_run_id=$4 WHERE organization_id=$1 AND parent_run_id=$2 AND member_id=$3 AND status='planned' AND child_run_id IS NULL", parent.OrganizationID, execution.RunID, slot.ID, runID); err != nil {
		return err
	}
	if slot.Plan.Definition != nil {
		if err = createTeamExecutionTx(ctx, tx, child, runID, execution.RootRunID, slot.Plan); err != nil {
			return err
		}
	}
	return emit(ctx, tx, child.OrganizationID, child.ID, "team.member_started", map[string]any{"parent_id": parent.ID, "parent_run_id": execution.RunID, "member_id": slot.ID, "run_id": runID, "operation_id": operationID, "input_digest": contextDigest, "source_candidate_id": sourceCandidate})
}

type teamManifest struct {
	Version      int                  `json:"version"`
	Pattern      string               `json:"pattern"`
	JoinPolicy   string               `json:"join_policy"`
	Complete     bool                 `json:"complete"`
	AllFixture   bool                 `json:"all_fixture"`
	Verification string               `json:"verification"`
	Members      []teamManifestMember `json:"members"`
}
type teamManifestMember struct {
	ID     string            `json:"id"`
	TaskID string            `json:"task_id"`
	RunID  string            `json:"run_id,omitempty"`
	Status string            `json:"status"`
	Result *domain.RunSource `json:"result,omitempty"`
}

func createTeamCandidateTx(ctx context.Context, tx pgx.Tx, execution teamExecution, parent domain.Task, slots []teamSlot) error {
	ids := teamPlanTaskIDs(execution.Plan)
	var active bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE organization_id=$1 AND task_id=ANY($2::text[]) AND status IN('queued','leased'))", parent.OrganizationID, ids).Scan(&active); err != nil {
		return err
	}
	if active {
		return domain.Err("conflict", "Команда ещё содержит активную внешнюю работу", 409)
	}
	manifest := teamManifest{Version: 1, Pattern: execution.Plan.Definition.Pattern, JoinPolicy: execution.Plan.Definition.JoinPolicy, Complete: true, AllFixture: teamAllFixture(execution.Plan), Verification: "awaiting_authorized_decision", Members: []teamManifestMember{}}
	for _, slot := range slots {
		member := teamManifestMember{ID: slot.ID, TaskID: slot.ChildTaskID, RunID: slot.ChildRunID, Status: slot.Status}
		if slot.Status == "candidate" {
			if slot.Source == nil {
				return domain.Err("conflict", "У готового участника отсутствует точный результат", 409)
			}
			child, err := scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND id=$2", parent.OrganizationID, slot.ChildTaskID))
			if err != nil {
				return err
			}
			source, _, err := teamCandidateSourceTx(ctx, tx, child, slot.Source.CandidateID)
			if err != nil {
				return err
			}
			if source.ArtifactID != slot.Source.ArtifactID || source.ArtifactVersion != slot.Source.ArtifactVersion || source.ContentDigest != slot.Source.ContentDigest {
				return domain.Err("conflict", "Источник команды не соответствует закреплённому результату", 409)
			}
			member.Result = &source
		} else {
			manifest.Complete = false
		}
		manifest.Members = append(manifest.Members, member)
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if len(body) > 262144 {
		return domain.Err("team_output_too_large", "Точный итог команды превышает допустимый размер материала", 409)
	}
	var operationID string
	if err = tx.QueryRow(ctx, "SELECT id FROM operations WHERE organization_id=$1 AND task_id=$2 AND run_id=$3 AND status='blocked'", parent.OrganizationID, parent.ID, execution.RunID).Scan(&operationID); err != nil {
		return rowExists(err)
	}
	artifact := domain.Artifact{ID: domain.NewID("art"), OrganizationID: parent.OrganizationID, TaskID: parent.ID, Name: "team-result.json", MediaType: "application/json", Content: string(body), Version: 1, Provenance: map[string]any{"adapter": "team:coordinator", "verified": false, "verification": "awaiting_authorized_decision", "pattern": manifest.Pattern, "join_policy": manifest.JoinPolicy, "complete": manifest.Complete, "all_fixture": manifest.AllFixture, "synthetic_cost": manifest.AllFixture, "run_id": execution.RunID, "content_digest": Hash(body), "plan_digest": digestValue(execution.Plan)}}
	provenance, _ := json.Marshal(artifact.Provenance)
	if err = tx.QueryRow(ctx, "INSERT INTO artifacts(organization_id,id,task_id,name,media_type,content,provenance) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING created_at", artifact.OrganizationID, artifact.ID, artifact.TaskID, artifact.Name, artifact.MediaType, artifact.Content, provenance).Scan(&artifact.CreatedAt); err != nil {
		return err
	}
	candidate, err := RegisterCandidateTx(ctx, tx, parent, execution.RunID, operationID, artifact, "team")
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE tasks SET status='waiting_input',waiting_reason='team_candidate_pending',version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2", parent.OrganizationID, parent.ID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE runs SET status='waiting_input' WHERE organization_id=$1 AND id=$2", parent.OrganizationID, execution.RunID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE team_executions SET status='candidate',version=version+1 WHERE organization_id=$1 AND run_id=$2", parent.OrganizationID, execution.RunID); err != nil {
		return err
	}
	return emit(ctx, tx, parent.OrganizationID, parent.ID, "team.candidate_created", map[string]any{"candidate_id": candidate.ID, "artifact_id": artifact.ID, "content_digest": candidate.ContentDigest, "run_id": execution.RunID, "verified": false, "complete": manifest.Complete, "all_fixture": manifest.AllFixture, "synthetic_cost": manifest.AllFixture})
}

func teamPlanTaskIDs(node compiledTeamNode) []string {
	result := []string{node.TaskID}
	for _, member := range node.Members {
		result = append(result, teamPlanTaskIDs(member.Node)...)
	}
	return result
}

func cancelTeamExecutionsTx(ctx context.Context, tx pgx.Tx, org string, taskIDs []string) error {
	if len(taskIDs) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, "UPDATE team_members SET status='cancelled' WHERE organization_id=$1 AND child_task_id=ANY($2::text[]) AND status IN('planned','running')", org, taskIDs); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE team_executions SET status='cancelled',version=version+1 WHERE organization_id=$1 AND task_id=ANY($2::text[]) AND status<>'cancelled'", org, taskIDs)
	return err
}

func closeTeamPlanSubtreeTx(ctx context.Context, tx pgx.Tx, org string, node compiledTeamNode, reason string) error {
	return cancelTasksTx(ctx, tx, org, teamPlanTaskIDs(node), reason)
}

func finishTeamBlock(ctx context.Context, tx pgx.Tx, execution teamExecution, tasks map[string]domain.Task, reason string) (bool, error) {
	for _, member := range execution.Plan.Members {
		if err := closeTeamPlanSubtreeTx(ctx, tx, execution.OrganizationID, member.Node, reason); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, "UPDATE team_executions SET status='blocked',version=version+1 WHERE organization_id=$1 AND run_id=$2", execution.OrganizationID, execution.RunID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, "UPDATE tasks SET status='blocked',waiting_reason=$3,version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2 AND current_run_id=$4", execution.OrganizationID, execution.TaskID, reason, execution.RunID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, "UPDATE runs SET status='blocked' WHERE organization_id=$1 AND id=$2", execution.OrganizationID, execution.RunID); err != nil {
		return false, err
	}
	if err := emit(ctx, tx, execution.OrganizationID, execution.TaskID, "team.blocked", map[string]any{"run_id": execution.RunID, "reason": reason}); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// finalizeTeamDecisionTx is invoked after the root receipt exists and the caller
// owns every structural/execution lock. Inheritance follows the exact frozen
// manifest and selected candidate tuples, never whatever happens to be current
// on an unrelated descendant. Independent pre-consumption acceptances survive.
func finalizeTeamDecisionTx(ctx context.Context, tx pgx.Tx, task domain.Task, decision domain.TaskDecision) error {
	basisTask, source, kind, err := teamReviewSourceTx(ctx, tx, task)
	if err != nil {
		return err
	}
	if kind != "team" {
		return nil
	}
	closure, err := selectedTeamClosureTx(ctx, tx, basisTask, source, map[string]bool{})
	if err != nil {
		return err
	}
	for _, selected := range closure {
		child, source := selected.Task, selected.Source
		if child.Status == "succeeded" {
			if selected.Kind != "fixture" {
				var accepted bool
				if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM task_decisions WHERE organization_id=$1 AND id=$2 AND candidate_id=$3 AND decision='accept')", child.OrganizationID, child.AcceptedDecisionID, source.CandidateID).Scan(&accepted); err != nil {
					return err
				}
				if !accepted {
					return domain.Err("conflict", "Готовый дочерний результат не имеет решения о принятии", 409)
				}
			}
			continue
		}
		if child.Status != "waiting_input" || child.CandidateID != source.CandidateID {
			return domain.Err("conflict", "Выбранный дочерний результат больше не ожидает решения", 409)
		}
		inherited, err := recordDecisionTx(ctx, tx, domain.TaskDecision{OrganizationID: task.OrganizationID, TaskID: child.ID, RunID: child.CurrentRunID, CandidateID: source.CandidateID, ArtifactID: source.ArtifactID, ArtifactVersion: source.ArtifactVersion, ContentDigest: source.ContentDigest, Decision: decision.Decision, Reason: decision.Reason, PrincipalID: decision.PrincipalID, Method: "inherited_authorized_user_decision", TaskVersionBefore: child.Version, TaskVersionAfter: child.Version + 1, ParentDecisionID: decision.ID})
		if err != nil {
			return err
		}
		status, result, accepted := "failed", "", ""
		if decision.Decision == "accept" {
			status, result, accepted = "succeeded", source.Content, inherited.ID
		}
		if _, err = tx.Exec(ctx, "UPDATE tasks SET status=$3,result=$4,accepted_decision_id=NULLIF($5,''),waiting_reason='',version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2", task.OrganizationID, child.ID, status, result, accepted); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE runs SET status=$3 WHERE organization_id=$1 AND id=$2", task.OrganizationID, child.CurrentRunID, status); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE operations SET status=$3 WHERE organization_id=$1 AND task_id=$2 AND run_id=$4 AND status='blocked'", task.OrganizationID, child.ID, status, child.CurrentRunID); err != nil {
			return err
		}
		if err = emit(ctx, tx, task.OrganizationID, child.ID, "task.decision_inherited", map[string]any{"decision_id": inherited.ID, "parent_decision_id": decision.ID, "decision": decision.Decision, "candidate_id": child.CandidateID, "method": inherited.Method}); err != nil {
			return err
		}
	}
	return nil
}

type selectedTeamResult struct {
	Task   domain.Task
	Source domain.RunSource
	Kind   string
}

func selectedTeamClosureTx(ctx context.Context, tx pgx.Tx, parent domain.Task, source domain.RunSource, visited map[string]bool) ([]selectedTeamResult, error) {
	invalid := domain.Err("conflict", "Объединённый результат не соответствует закреплённым материалам команды", 409)
	if visited[parent.ID] || len(visited) >= domain.MaxTreeTasks {
		return nil, invalid
	}
	visited[parent.ID] = true
	var execution teamExecution
	execution.OrganizationID, execution.TaskID, execution.RunID = parent.OrganizationID, parent.ID, parent.CurrentRunID
	var raw []byte
	var digest string
	if err := tx.QueryRow(ctx, "SELECT root_id,root_run_id,plan,plan_digest FROM team_executions WHERE organization_id=$1 AND task_id=$2 AND run_id=$3 AND status='candidate'", parent.OrganizationID, parent.ID, parent.CurrentRunID).Scan(&execution.RootID, &execution.RootRunID, &raw, &digest); err != nil {
		return nil, rowExists(err)
	}
	if err := json.Unmarshal(raw, &execution.Plan); err != nil || digestValue(execution.Plan) != digest || execution.Plan.Definition == nil || execution.Plan.TaskID != parent.ID {
		return nil, invalid
	}
	var manifest teamManifest
	if err := json.Unmarshal([]byte(source.Content), &manifest); err != nil || manifest.Version != 1 || manifest.Pattern != execution.Plan.Definition.Pattern || manifest.JoinPolicy != execution.Plan.Definition.JoinPolicy || manifest.AllFixture != teamAllFixture(execution.Plan) || manifest.Verification != "awaiting_authorized_decision" {
		return nil, invalid
	}
	slots, err := teamSlotsTx(ctx, tx, execution)
	if err != nil {
		return nil, err
	}
	if len(slots) != len(execution.Plan.Members) || len(manifest.Members) != len(slots) {
		return nil, invalid
	}
	var active bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE organization_id=$1 AND task_id=ANY($2::text[]) AND status IN('queued','leased'))", parent.OrganizationID, teamPlanTaskIDs(execution.Plan)).Scan(&active); err != nil {
		return nil, err
	}
	if active {
		return nil, domain.Err("conflict", "Нельзя принять команду с активными дочерними действиями", 409)
	}
	result := []selectedTeamResult{}
	complete := true
	for i, slot := range slots {
		member := manifest.Members[i]
		if slot.Ordinal != i || slot.ID != execution.Plan.Members[i].ID || digestValue(slot.Plan) != digestValue(execution.Plan.Members[i].Node) || member.ID != slot.ID || member.TaskID != slot.ChildTaskID || member.RunID != slot.ChildRunID || member.Status != slot.Status {
			return nil, invalid
		}
		if slot.Status != "candidate" {
			complete = false
			if member.Result != nil || (slot.Status != "failed" && slot.Status != "cancelled" && slot.Status != "skipped") {
				return nil, invalid
			}
			var unfinished bool
			if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM tasks WHERE organization_id=$1 AND id=ANY($2::text[]) AND status NOT IN('succeeded','failed','cancelled'))", parent.OrganizationID, teamPlanTaskIDs(slot.Plan)).Scan(&unfinished); err != nil {
				return nil, err
			}
			if unfinished {
				return nil, invalid
			}
			continue
		}
		if slot.Source == nil || member.Result == nil {
			return nil, invalid
		}
		child, err := scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND id=$2", parent.OrganizationID, slot.ChildTaskID))
		if err != nil {
			return nil, err
		}
		if child.ParentID != parent.ID || child.CurrentRunID != slot.ChildRunID {
			return nil, invalid
		}
		exact, kind, err := teamCandidateSourceTx(ctx, tx, child, slot.Source.CandidateID)
		if err != nil {
			return nil, err
		}
		if exact.ArtifactID != slot.Source.ArtifactID || exact.ArtifactVersion != slot.Source.ArtifactVersion || exact.ContentDigest != slot.Source.ContentDigest || digestValue(exact) != digestValue(*member.Result) {
			return nil, invalid
		}
		result = append(result, selectedTeamResult{Task: child, Source: exact, Kind: kind})
		basisChild, basisSource, basisKind, basisErr := teamReviewSourceTx(ctx, tx, child)
		if basisErr != nil {
			return nil, basisErr
		}
		if basisKind == "team" {
			nested, err := selectedTeamClosureTx(ctx, tx, basisChild, basisSource, visited)
			if err != nil {
				return nil, err
			}
			result = append(result, nested...)
		}
	}
	if manifest.Complete != complete {
		return nil, invalid
	}
	return result, nil
}

// Edited final text has its own exact candidate, while the selected execution
// closure remains bound to the original coordinator manifest. Never parse an
// edited prose result as authority to select or omit another branch.
func teamReviewSourceTx(ctx context.Context, tx pgx.Tx, task domain.Task) (domain.Task, domain.RunSource, string, error) {
	source, kind, e := teamCandidateSourceTx(ctx, tx, task, task.CandidateID)
	if e != nil || kind != "material" {
		return task, source, kind, e
	}
	candidate, _, e := loadCandidateTx(ctx, tx, task.OrganizationID, task.CandidateID)
	if e != nil {
		return task, source, kind, e
	}
	basis, e := materialReviewBasisTx(ctx, tx, task, candidate)
	if e != nil {
		return task, source, kind, e
	}
	task.CandidateID = basis.ID
	source, kind, e = teamCandidateSourceTx(ctx, tx, task, basis.ID)
	return task, source, kind, e
}
