// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// initTeamTx runs inside root task/run admission. The caller owns authority,
// tree control (unless this is a new root), organization and parent task locks.
// Materializing every dependency task now reserves the ordinary pending-task
// capacity and prevents an organization full of coordinators from starving its
// own children. No dependent Run or billable Operation exists before readiness.
func initTeamTx(ctx context.Context, tx pgx.Tx, parent domain.Task, runID string, definition domain.TeamDefinition) error {
	agent, err := scanAgent(tx.QueryRow(ctx, "SELECT "+agentColumns+" FROM agents WHERE organization_id=$1 AND id=$2 AND version=$3", parent.OrganizationID, parent.AgentID, parent.AgentVersion))
	if err != nil {
		return err
	}
	if agent.Team == nil || digestValue(*agent.Team) != digestValue(definition) {
		return domain.Err("conflict", "Определение команды не соответствует закреплённому агенту", 409)
	}
	var plan compiledTeamPlan
	if parent.AttemptCount > 1 {
		plan, err = cloneTeamPlanTx(ctx, tx, parent, runID)
	} else {
		plan, err = compileTeamPlanTx(ctx, tx, parent, agent)
	}
	if err != nil {
		return err
	}
	var used, maximum, pending, pendingLimit int
	if err = tx.QueryRow(ctx, "SELECT used_tasks,max_tasks FROM task_tree_controls WHERE organization_id=$1 AND root_id=$2", parent.OrganizationID, parent.RootID).Scan(&used, &maximum); err != nil {
		return err
	}
	if used+plan.TotalTasks-1 > maximum {
		return domain.Err("conflict", "Исчерпан общий предел задач дерева", 409)
	}
	if err = tx.QueryRow(ctx, "SELECT pending_task_limit FROM organizations WHERE id=$1", parent.OrganizationID).Scan(&pendingLimit); err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM tasks WHERE organization_id=$1 AND status NOT IN('succeeded','failed','cancelled')", parent.OrganizationID).Scan(&pending); err != nil {
		return err
	}
	if pending+plan.TotalTasks-1 > pendingLimit {
		return &domain.Error{Code: "unavailable", Message: "Для дерева команды недостаточно мест в очереди организации", Status: 503, Retryable: true}
	}
	plan.Root.TaskID = parent.ID
	if err = materializeTeamDescendantsTx(ctx, tx, parent, &plan.Root); err != nil {
		return err
	}
	// This operation is a structural/result fence, never a model dispatch. Both
	// initial admission and explicit root retry use this same invariant.
	if _, err = tx.Exec(ctx, "UPDATE operations SET status='blocked' WHERE organization_id=$1 AND task_id=$2 AND run_id=$3 AND status='queued'", parent.OrganizationID, parent.ID, runID); err != nil {
		return err
	}
	return createTeamExecutionTx(ctx, tx, parent, runID, runID, plan.Root)
}

// A new root attempt changes task/run identities and input, not accepted
// provider/price pins. Retiring a pinned model fails admission; it does not
// silently move the retry to a newly installed profile.
func cloneTeamPlanTx(ctx context.Context, tx pgx.Tx, parent domain.Task, runID string) (compiledTeamPlan, error) {
	var result compiledTeamPlan
	var body []byte
	var digest string
	err := tx.QueryRow(ctx, "SELECT plan,plan_digest FROM team_executions WHERE organization_id=$1 AND task_id=$2 AND run_id<>$3 ORDER BY created_at DESC,run_id DESC LIMIT 1", parent.OrganizationID, parent.ID, runID).Scan(&body, &digest)
	if err != nil {
		return result, rowExists(err)
	}
	if err = json.Unmarshal(body, &result.Root); err != nil || digestValue(result.Root) != digest || result.Root.TaskID != parent.ID || result.Root.AgentID != parent.AgentID || result.Root.AgentVersion != parent.AgentVersion || result.Root.BudgetMicroUSD != parent.BudgetMicroUSD {
		return result, domain.Err("conflict", "Прежний закрытый план команды не соответствует задаче", 409)
	}
	var visit func(compiledTeamNode) error
	visit = func(node compiledTeamNode) error {
		result.TotalTasks++
		if result.TotalTasks > domain.MaxTreeTasks {
			return domain.Err("conflict", "Сохранённый план превышает предел дерева", 409)
		}
		if node.ModelConnectionID != "" {
			var active bool
			if err := tx.QueryRow(ctx, "SELECT active FROM model_connections WHERE organization_id=$1 AND id=$2 AND version=$3 FOR SHARE", parent.OrganizationID, node.ModelConnectionID, node.ModelConnectionVersion).Scan(&active); err != nil {
				return err
			}
			if !active {
				return domain.Err("invalid_request", "Прежняя модель команды отключена; новая версия не подставляется автоматически", 400)
			}
		}
		for _, member := range node.Members {
			if err := visit(member.Node); err != nil {
				return err
			}
		}
		return nil
	}
	err = visit(result.Root)
	return result, err
}

func materializeTeamDescendantsTx(ctx context.Context, tx pgx.Tx, parent domain.Task, node *compiledTeamNode) error {
	for i := range node.Members {
		member := &node.Members[i]
		goal := member.Task
		if goal == "" {
			goal = parent.Goal
		}
		member.Node.TaskID = domain.NewID("tsk")
		child, err := scanTask(tx.QueryRow(ctx, `INSERT INTO tasks(organization_id,id,owner_id,agent_id,agent_version,goal,status,waiting_reason,budget_microusd,parent_id,model_connection_id,model_connection_version,max_runs,execution_deadline,tree_depth_limit)
 VALUES($1,$2,$3,$4,$5,$6,'waiting_input','team_dependency_pending',$7,$8,NULLIF($9,''),NULLIF($10::integer,0),1,$11,(SELECT max_depth FROM task_tree_controls WHERE organization_id=$1 AND root_id=$12)) RETURNING `+taskColumns,
			parent.OrganizationID, member.Node.TaskID, parent.OwnerID, member.Node.AgentID, member.Node.AgentVersion, goal, member.Node.BudgetMicroUSD, parent.ID, member.Node.ModelConnectionID, member.Node.ModelConnectionVersion, parent.ExecutionDeadline, parent.RootID))
		if err != nil {
			return err
		}
		if err = emit(ctx, tx, parent.OrganizationID, child.ID, "task.delegated", map[string]any{"parent_id": parent.ID, "root_id": parent.RootID, "member_id": member.ID, "agent_version": child.AgentVersion, "state": "dependency_pending"}); err != nil {
			return err
		}
		if err = materializeTeamDescendantsTx(ctx, tx, child, &member.Node); err != nil {
			return err
		}
	}
	return nil
}

func createTeamExecutionTx(ctx context.Context, tx pgx.Tx, task domain.Task, runID, rootRunID string, node compiledTeamNode) error {
	if node.Definition == nil || node.TaskID != task.ID || node.AgentID != task.AgentID || node.AgentVersion != task.AgentVersion {
		return domain.Err("conflict", "Закреплённый план не соответствует задаче команды", 409)
	}
	plan, err := json.Marshal(node)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO team_executions(organization_id,task_id,run_id,root_id,root_run_id,plan,plan_digest) VALUES($1,$2,$3,$4,$5,$6,$7)`, task.OrganizationID, task.ID, runID, task.RootID, rootRunID, plan, Hash(plan)); err != nil {
		return err
	}
	for i, member := range node.Members {
		body, err := json.Marshal(member.Node)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO team_members(organization_id,parent_run_id,member_id,ordinal,child_task_id,plan,plan_digest) VALUES($1,$2,$3,$4,$5,$6,$7)`, task.OrganizationID, runID, member.ID, i, member.Node.TaskID, body, Hash(body)); err != nil {
			return err
		}
	}
	return emit(ctx, tx, task.OrganizationID, task.ID, "team.initialized", map[string]any{"run_id": runID, "pattern": node.Definition.Pattern, "join_policy": node.Definition.JoinPolicy, "plan_digest": Hash(plan), "all_fixture": teamAllFixture(node)})
}

func teamAllFixture(node compiledTeamNode) bool {
	if node.Definition == nil {
		return node.ModelConnectionID == ""
	}
	for _, member := range node.Members {
		if !teamAllFixture(member.Node) {
			return false
		}
	}
	return true
}
