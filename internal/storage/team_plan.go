// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"fmt"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// compiledTeamNode is a closed, immutable execution plan. References always
// name published agent versions and the model profile chosen at root admission.
// A later stage never consults the current/latest agent or model configuration.
type compiledTeamNode struct {
	TaskID                 string                 `json:"task_id"`
	AgentID                string                 `json:"agent_id"`
	AgentVersion           int                    `json:"agent_version"`
	ModelConnectionID      string                 `json:"model_connection_id,omitempty"`
	ModelConnectionVersion int                    `json:"model_connection_version,omitempty"`
	BudgetMicroUSD         int64                  `json:"budget_microusd"`
	Definition             *domain.TeamDefinition `json:"definition,omitempty"`
	Members                []compiledTeamMember   `json:"members,omitempty"`
}

type compiledTeamMember struct {
	ID   string           `json:"id"`
	Task string           `json:"task,omitempty"`
	Node compiledTeamNode `json:"node"`
}

type compiledTeamPlan struct {
	Root       compiledTeamNode `json:"root"`
	TotalTasks int              `json:"total_tasks"`
}

type teamAgentPin struct {
	id      string
	version int
}

// compileTeamPlanTx is called while the organization admission lock is held.
// No tasks, runs or reservations are created until the entire bounded plan has
// passed validation. The limit is deliberately a root-tree limit, not a limit
// restarted independently by each nested coordinator.
func compileTeamPlanTx(ctx context.Context, tx pgx.Tx, task domain.Task, agent domain.Agent) (compiledTeamPlan, error) {
	var plan compiledTeamPlan
	if agent.Team == nil || agent.Model != domain.TeamModel || !agent.Published || task.BudgetMicroUSD > agent.BudgetMicroUSD {
		return plan, domain.Err("invalid_request", "Нужна опубликованная команда с допустимым бюджетом", 400)
	}
	agents := map[teamAgentPin]domain.Agent{{agent.ID, agent.Version}: agent}
	models := map[string]domain.ModelConnection{}
	ancestry := map[teamAgentPin]bool{}
	var compile func(domain.Agent, int64, int, int) (compiledTeamNode, error)
	compile = func(a domain.Agent, budget int64, depth, remaining int) (compiledTeamNode, error) {
		node := compiledTeamNode{AgentID: a.ID, AgentVersion: a.Version, BudgetMicroUSD: min(budget, a.BudgetMicroUSD)}
		pin := teamAgentPin{a.ID, a.Version}
		if ancestry[pin] {
			return node, domain.Err("invalid_request", "Команда содержит циклическое делегирование", 400)
		}
		if depth > domain.MaxTaskDepth || remaining < 0 || plan.TotalTasks >= domain.MaxTreeTasks {
			return node, domain.Err("invalid_request", "Дерево команды превышает предел глубины или 128 задач", 400)
		}
		if !a.Published || a.OrganizationID != task.OrganizationID || node.BudgetMicroUSD < 1 || len(a.Tools) != len(a.ModelTools) || len(a.Tools) > domain.MaxModelTools {
			return node, domain.Err("invalid_request", "Участник команды не соответствует разрешённому профилю", 400)
		}
		if len(a.Tools) > 0 && (a.Model == domain.FixtureModel || a.Model == domain.TeamModel) {
			return node, domain.Err("invalid_request", "Инструменты доступны только модельному участнику", 400)
		}
		for i, def := range a.ModelTools {
			if def.ToolID != a.Tools[i] || def.Name != domain.ModelToolName(task.OrganizationID, def.ToolID, def.ToolVersion) {
				return node, modelSnapshotConflict()
			}
			c, err := loadToolConnectionTx(ctx, tx, task.OrganizationID, def.ToolID, def.ToolVersion, true)
			if err != nil {
				return node, err
			}
			if !c.Tool.Active || domain.ToolConnectionDigest(c) != def.ToolProfileDigest || c.Tool.InputSchemaJSON != def.InputSchemaJSON || c.Tool.Description != def.Description {
				return node, domain.Err("conflict", "Закреплённый инструмент участника отключён или изменился", 409)
			}
		}
		plan.TotalTasks++
		if a.Team == nil {
			if a.Model == domain.TeamModel {
				return node, domain.Err("invalid_request", "У координатора отсутствует определение команды", 400)
			}
			if a.Model != domain.FixtureModel {
				model, ok := models[a.Model]
				if !ok {
					var err error
					model, err = scanModelConnection(tx.QueryRow(ctx, "SELECT profile,active,created_at FROM model_connections WHERE organization_id=$1 AND model=$2 AND active FOR SHARE", task.OrganizationID, a.Model))
					if errors.Is(err, domain.NotFound) {
						return node, domain.Err("invalid_request", "Модель участника команды отключена", 400)
					}
					if err != nil {
						return node, err
					}
					models[a.Model] = model
				}
				node.ModelConnectionID, node.ModelConnectionVersion = model.ID, model.Version
			}
			return node, nil
		}
		if a.Model != domain.TeamModel || domain.ValidateTeam(*a.Team) != nil || len(a.Team.Members) > a.Team.MaxChildren {
			return node, domain.Err("invalid_request", "Некорректная опубликованная схема команды", 400)
		}
		definition := *a.Team
		definition.Members = append([]domain.TeamMember(nil), a.Team.Members...)
		if definition.MaxParallel == 0 {
			definition.MaxParallel = 1
			if definition.Pattern == "parallel" {
				definition.MaxParallel = len(definition.Members)
			}
		}
		if definition.MaxParallel < 1 || definition.MaxParallel > len(definition.Members) || (definition.Pattern != "parallel" && definition.MaxParallel != 1) {
			return node, domain.Err("invalid_request", "Некорректный предел параллелизма команды", 400)
		}
		node.Definition = &definition
		ancestry[pin] = true
		defer delete(ancestry, pin)
		for _, member := range definition.Members {
			if member.Task != "" && !validText(member.Task, 4096) {
				return node, domain.Err("invalid_request", "Поручение участнику должно быть не длиннее 4096 байт", 400)
			}
			childPin := teamAgentPin{member.AgentID, member.AgentVersion}
			child, ok := agents[childPin]
			if !ok {
				var err error
				child, err = scanAgent(tx.QueryRow(ctx, "SELECT "+agentColumns+" FROM agents WHERE organization_id=$1 AND id=$2 AND version=$3 AND published", task.OrganizationID, member.AgentID, member.AgentVersion))
				if errors.Is(err, domain.NotFound) {
					return node, domain.Err("invalid_request", "Закреплённая версия участника команды не опубликована", 400)
				}
				if err != nil {
					return node, fmt.Errorf("load pinned team member: %w", err)
				}
				agents[childPin] = child
			}
			childNode, err := compile(child, node.BudgetMicroUSD, depth+1, min(remaining, definition.MaxDepth)-1)
			if err != nil {
				return node, err
			}
			node.Members = append(node.Members, compiledTeamMember{ID: member.ID, Task: member.Task, Node: childNode})
		}
		return node, nil
	}
	var err error
	plan.Root, err = compile(agent, task.BudgetMicroUSD, task.Depth, domain.MaxTaskDepth-task.Depth)
	return plan, err
}
