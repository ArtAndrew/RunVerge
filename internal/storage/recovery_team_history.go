// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"reflect"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

var ErrRecoveryTeamHistory = errors.New("invalid recovery team plan history")
var ErrRecoveryTeamLimit = errors.New("recovery team validation exceeds supported limits")

type recoveryTeamExecution struct {
	org, task, run, root, rootRun, digest string
	raw                                   []byte
}

// Only retained immutable plan bindings are checked here. Current liveness,
// permissions and latest profile versions must not reinterpret old admissions.
// Manifest/dependency-input reconstruction remains a separate coverage boundary.
func validateRecoveryTeamHistory(ctx context.Context, tx pgx.Tx, schema string) error {
	if ctx == nil || tx == nil || !recoverySnapshotName.MatchString(schema) {
		return ErrRecoveryTeamHistory
	}
	q := func(table string) string { return pgx.Identifier{schema, table}.Sanitize() }
	rows, err := tx.Query(ctx, "SELECT organization_id,task_id,run_id,root_id,root_run_id,plan,plan_digest FROM "+q("team_executions"))
	if err != nil {
		return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
	}
	var executions []recoveryTeamExecution
	totalBytes := 0
	for rows.Next() {
		var e recoveryTeamExecution
		if rows.Scan(&e.org, &e.task, &e.run, &e.root, &e.rootRun, &e.raw, &e.digest) != nil {
			rows.Close()
			return ErrRecoveryTeamHistory
		}
		totalBytes += len(e.raw)
		if len(executions) >= 200000 || totalBytes > 64<<20 {
			rows.Close()
			return ErrRecoveryTeamLimit
		}
		executions = append(executions, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
	}
	work := 0
	for _, e := range executions {
		var node compiledTeamNode
		if decodeRecoveryHistoryJSON(e.raw, &node, 16<<20) != nil || digestValue(node) != e.digest || node.TaskID != e.task || node.Definition == nil {
			return ErrRecoveryTeamHistory
		}
		if e.task == e.root {
			if e.rootRun != e.run {
				return ErrRecoveryTeamHistory
			}
		} else {
			var bound int
			if tx.QueryRow(ctx, `SELECT pg_catalog.count(*) FROM `+q("team_members")+` m JOIN `+q("team_executions")+` parent ON parent.organization_id=m.organization_id AND parent.run_id=m.parent_run_id WHERE m.organization_id=$1 AND m.child_task_id=$2 AND m.child_run_id=$3 AND m.plan_digest=$4 AND parent.root_id=$5 AND parent.root_run_id=$6`, e.org, e.task, e.run, e.digest, e.root, e.rootRun).Scan(&bound) != nil || bound != 1 {
				return ErrRecoveryTeamHistory
			}
		}
		seen := map[string]bool{}
		ancestry := map[teamAgentPin]bool{}
		count := 0
		var visit func(compiledTeamNode, string, string, int, int, int64) error
		visit = func(n compiledTeamNode, parent, goal string, depth, remaining int, budget int64) error {
			count++
			work++
			if work > 1000000 {
				return ErrRecoveryTeamLimit
			}
			if count > domain.MaxTreeTasks || seen[n.TaskID] || depth > domain.MaxTaskDepth || remaining < 0 {
				return ErrRecoveryTeamHistory
			}
			seen[n.TaskID] = true
			var aid, actualParent, root, actualGoal, modelID, model string
			var version, actualDepth, modelVersion int
			var taskBudget, agentBudget int64
			var team []byte
			var published bool
			err := tx.QueryRow(ctx, `SELECT t.agent_id,t.agent_version,COALESCE(t.parent_id,''),t.root_id,t.depth,t.goal,t.budget_microusd,COALESCE(t.model_connection_id,''),COALESCE(t.model_connection_version,0),a.model,a.team,a.budget_microusd,a.published FROM `+q("tasks")+` t JOIN `+q("agents")+` a ON a.organization_id=t.organization_id AND a.id=t.agent_id AND a.version=t.agent_version WHERE t.organization_id=$1 AND t.id=$2`, e.org, n.TaskID).Scan(&aid, &version, &actualParent, &root, &actualDepth, &actualGoal, &taskBudget, &modelID, &modelVersion, &model, &team, &agentBudget, &published)
			if err != nil {
				return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
			}
			if parent == "" {
				depth = actualDepth
				remaining = domain.MaxTaskDepth - depth
				budget = taskBudget
			} else if actualParent != parent || actualGoal != goal || actualDepth != depth {
				return ErrRecoveryTeamHistory
			}
			pin := teamAgentPin{aid, version}
			if ancestry[pin] || root != e.root || aid != n.AgentID || version != n.AgentVersion || !published || n.BudgetMicroUSD < 1 || n.BudgetMicroUSD != taskBudget || n.BudgetMicroUSD != min(budget, agentBudget) || modelID != n.ModelConnectionID || modelVersion != n.ModelConnectionVersion {
				return ErrRecoveryTeamHistory
			}
			var definition *domain.TeamDefinition
			if len(team) > 0 && decodeRecoveryHistoryJSON(team, &definition, 16<<20) != nil {
				return ErrRecoveryTeamHistory
			}
			if definition == nil {
				if n.Definition != nil || len(n.Members) != 0 || model == domain.TeamModel {
					return ErrRecoveryTeamHistory
				}
				if model == domain.FixtureModel {
					if modelID != "" || modelVersion != 0 {
						return ErrRecoveryTeamHistory
					}
				} else {
					var name string
					if modelID == "" || tx.QueryRow(ctx, "SELECT model FROM "+q("model_connections")+" WHERE organization_id=$1 AND id=$2 AND version=$3", e.org, modelID, modelVersion).Scan(&name) != nil || name != model {
						return ErrRecoveryTeamHistory
					}
				}
				return nil
			}
			if model != domain.TeamModel || modelID != "" || modelVersion != 0 || domain.ValidateTeam(*definition) != nil {
				return ErrRecoveryTeamHistory
			}
			if definition.MaxParallel == 0 {
				definition.MaxParallel = 1
				if definition.Pattern == "parallel" {
					definition.MaxParallel = len(definition.Members)
				}
			}
			if n.Definition == nil || !reflect.DeepEqual(*definition, *n.Definition) || len(n.Members) != len(definition.Members) {
				return ErrRecoveryTeamHistory
			}
			ancestry[pin] = true
			defer delete(ancestry, pin)
			for i, member := range n.Members {
				expected := definition.Members[i]
				if member.ID != expected.ID || member.Task != expected.Task || member.Node.AgentID != expected.AgentID || member.Node.AgentVersion != expected.AgentVersion {
					return ErrRecoveryTeamHistory
				}
				childGoal := member.Task
				if childGoal == "" {
					childGoal = actualGoal
				}
				if err := visit(member.Node, n.TaskID, childGoal, depth+1, min(remaining, definition.MaxDepth)-1, n.BudgetMicroUSD); err != nil {
					return err
				}
			}
			return nil
		}
		if err := visit(node, "", "", 0, 0, 0); err != nil {
			return err
		}
		// Read member rows without application helpers that lock or require current
		// candidate/run pointers. Historical retries retain their old slot records.
		members, err := tx.Query(ctx, "SELECT member_id,ordinal,child_task_id,plan,plan_digest FROM "+q("team_members")+" WHERE organization_id=$1 AND parent_run_id=$2 ORDER BY ordinal", e.org, e.run)
		if err != nil {
			return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
		}
		index := 0
		valid := true
		for members.Next() {
			var id, task, digest string
			var ordinal int
			var raw []byte
			var member compiledTeamNode
			if members.Scan(&id, &ordinal, &task, &raw, &digest) != nil || index >= len(node.Members) || decodeRecoveryHistoryJSON(raw, &member, 16<<20) != nil || digestValue(member) != digest {
				valid = false
				break
			}
			expected := node.Members[index]
			if id != expected.ID || ordinal != index || task != expected.Node.TaskID || !reflect.DeepEqual(member, expected.Node) {
				valid = false
				break
			}
			index++
		}
		err = members.Err()
		members.Close()
		if !valid || err != nil || index != len(node.Members) {
			return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
		}
	}
	var inherited bool
	if err := tx.QueryRow(ctx, `WITH RECURSIVE paths(organization_id,id,ancestors) AS (
 SELECT organization_id,id,ARRAY[]::text[] FROM `+q("tasks")+` WHERE parent_id IS NULL
 UNION ALL SELECT c.organization_id,c.id,p.ancestors||p.id FROM `+q("tasks")+` c JOIN paths p ON p.organization_id=c.organization_id AND p.id=c.parent_id WHERE pg_catalog.cardinality(p.ancestors)<16
 ) SELECT NOT EXISTS(SELECT 1 FROM `+q("task_decisions")+` d LEFT JOIN `+q("task_decisions")+` parent ON parent.organization_id=d.organization_id AND parent.id=d.parent_decision_id
 LEFT JOIN paths path ON path.organization_id=d.organization_id AND path.id=d.task_id
 WHERE (d.method='authorized_user_decision' AND d.parent_decision_id IS NOT NULL)
 OR (d.method='inherited_authorized_user_decision' AND (parent.id IS NULL OR parent.method<>'authorized_user_decision' OR path.id IS NULL OR NOT(parent.task_id=ANY(path.ancestors)) OR d.principal_id<>parent.principal_id OR d.decision<>parent.decision OR d.reason<>parent.reason)))`).Scan(&inherited); err != nil || !inherited {
		return recoverySnapshotError(ctx, ErrRecoveryTeamHistory)
	}
	return nil
}
