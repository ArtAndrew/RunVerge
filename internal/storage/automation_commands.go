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

const progressSignalColumns = "id,organization_id,task_id,grant_id,code,detector_version,fingerprint,evidence_ids,snapshot_digest,created_at"

func scanProgressSignal(row scanner) (v domain.ProgressSignal, err error) {
	var raw []byte
	err = row.Scan(&v.ID, &v.OrganizationID, &v.TaskID, &v.GrantID, &v.Code, &v.DetectorVersion, &v.Fingerprint, &raw, &v.SnapshotDigest, &v.CreatedAt)
	if err != nil {
		return v, rowExists(err)
	}
	if json.Unmarshal(raw, &v.EvidenceIDs) != nil || v.EvidenceIDs == nil || len(v.EvidenceIDs) > 512 || !validDigest(v.SnapshotDigest) {
		return v, automationSnapshotConflict()
	}
	return v, nil
}

func progressSignalsTx(ctx context.Context, tx pgx.Tx, org, taskID string, c cursor, limit int) ([]domain.ProgressSignal, string, error) {
	rows, err := tx.Query(ctx, "SELECT "+progressSignalColumns+" FROM progress_signals WHERE organization_id=$1 AND task_id=$2 AND (created_at,id)<($3,$4) ORDER BY created_at DESC,id DESC LIMIT $5", org, taskID, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	values := []domain.ProgressSignal{}
	for rows.Next() {
		v, e := scanProgressSignal(rows)
		if e != nil {
			rows.Close()
			return nil, "", e
		}
		values = append(values, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(values) > limit {
		values = values[:limit]
		last := values[limit-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return values, next, nil
}

func automationStateTx(ctx context.Context, tx pgx.Tx, p domain.Principal, taskID string) (domain.TaskAutomationState, error) {
	v := domain.TaskAutomationState{Signals: []domain.ProgressSignal{}, AvailableCommands: []string{}}
	g, err := scanAutomationGrant(tx.QueryRow(ctx, "SELECT "+automationGrantColumns+" FROM task_automation_grants WHERE organization_id=$1 AND task_id=$2 ORDER BY created_at DESC,id DESC LIMIT 1", p.OrganizationID, taskID))
	if err == nil {
		v.Grant = &g
	} else if !errors.Is(err, domain.NotFound) {
		return v, err
	}
	v.Usage, err = automationUsageTx(ctx, tx, p.OrganizationID, taskID)
	if err != nil {
		return v, err
	}
	c, _ := decodeCursor("")
	v.Signals, _, err = progressSignalsTx(ctx, tx, p.OrganizationID, taskID, c, 20)
	if err != nil {
		return v, err
	}
	// Discovery is current credential authority, never the caller's cached role.
	var id, org, role, kind string
	if err = tx.QueryRow(ctx, authQuery(p), p.TokenHash, p.OrganizationID).Scan(&id, &org, &role, &kind); err != nil {
		return v, domain.Unauthenticated
	}
	if id != p.ID || org != p.OrganizationID {
		return v, domain.Unauthenticated
	}
	if domain.PrincipalHasPermission(domain.Principal{ID: id, OrganizationID: org, Role: role, Kind: kind}, "tasks.automate_any") {
		t, e := scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND id=$2", org, taskID))
		if e != nil {
			return v, e
		}
		if v.Grant != nil && v.Grant.Status == "active" {
			v.AvailableCommands = append(v.AvailableCommands, "revoke")
		} else if !domain.Terminal(t.Status) && t.Status != "blocked" && !t.FinancialHold && t.ModelConnectionID != "" {
			var eligible bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agents a JOIN model_connections m ON m.organization_id=a.organization_id AND m.id=$4 AND m.version=$5 WHERE a.organization_id=$1 AND a.id=$2 AND a.version=$3 AND jsonb_array_length(a.model_tools)>0 AND m.active) AND clock_timestamp()<$6 AND NOT EXISTS(SELECT 1 FROM tool_executions WHERE organization_id=$1 AND task_id=$7 AND status='unknown')`, org, t.AgentID, t.AgentVersion, t.ModelConnectionID, t.ModelConnectionVersion, t.ExecutionDeadline, t.ID).Scan(&eligible); err != nil {
				return v, err
			}
			if eligible {
				v.AvailableCommands = append(v.AvailableCommands, "create")
			}
		}
	}
	return v, nil
}

func (s *Store) Automation(ctx context.Context, p domain.Principal, taskID string) (domain.TaskAutomationState, error) {
	tx, err := s.beginMaterialRead(ctx, p, taskID)
	if err != nil {
		return domain.TaskAutomationState{}, err
	}
	defer tx.Rollback(ctx)
	v, err := automationStateTx(ctx, tx, p, taskID)
	if err != nil {
		return v, err
	}
	return v, tx.Commit(ctx)
}
func (s *Store) ProgressSignals(ctx context.Context, p domain.Principal, taskID, raw string, limit int) ([]domain.ProgressSignal, string, error) {
	if limit < 1 || limit > 100 {
		return nil, "", domain.Err("invalid_request", "limit должен быть от 1 до 100", 400)
	}
	c, err := decodeCursor(raw)
	if err != nil {
		return nil, "", err
	}
	tx, err := s.beginMaterialRead(ctx, p, taskID)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	v, next, err := progressSignalsTx(ctx, tx, p.OrganizationID, taskID, c, limit)
	if err != nil {
		return nil, "", err
	}
	return v, next, tx.Commit(ctx)
}
func automationGuard(taskID string) func(context.Context, pgx.Tx, domain.Principal) error {
	return func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		if !domain.PrincipalHasPermission(p, "tasks.automate_any") {
			return domain.Forbidden
		}
		var found string
		return rowExists(tx.QueryRow(ctx, "SELECT id FROM tasks WHERE organization_id=$1 AND id=$2", p.OrganizationID, taskID).Scan(&found))
	}
}
func (s *Store) CreateAutomation(ctx context.Context, p domain.Principal, taskID, key string, in domain.CreateTaskAutomation) (json.RawMessage, error) {
	if in.ExpectedTaskVersion < 1 || (in.Mode != "read_only" && in.Mode != "prepare_and_continue") || len(in.ToolPins) < 1 || len(in.ToolPins) > domain.MaxModelTools || in.MaxModelTurns < 1 || in.MaxModelTurns >= domain.MaxModelTurnsPerTask || in.MaxToolExecutions < 1 || in.MaxToolExecutions > domain.MaxToolExecutionsPerTask || in.MaxChargeMicroUSD < 1 || in.MaxChargeMicroUSD > domain.MaxMoney || in.ExpiresInSeconds < 1 || in.ExpiresInSeconds > domain.MaxAutomationTTLSeconds || !in.AcknowledgeCost || !validText(in.Reason, 4096) {
		return nil, domain.Err("invalid_request", "Укажите точные инструменты, конечные лимиты, срок и согласие на расход", 400)
	}
	seen := map[string]bool{}
	for _, pin := range in.ToolPins {
		if !validText(pin.ToolID, 128) || pin.ToolVersion < 1 || !validDigest(pin.ToolProfileDigest) || seen[pin.ToolID] {
			return nil, domain.Err("invalid_request", "Нужны уникальные точные версии инструментов", 400)
		}
		seen[pin.ToolID] = true
	}
	return s.MutateGuarded(ctx, p, "POST /tasks/"+taskID+"/automation", key, in, automationGuard(taskID), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		t, _, err := lockToolTaskTx(ctx, tx, p.OrganizationID, taskID)
		if err != nil {
			return nil, err
		}
		if t.Version != in.ExpectedTaskVersion {
			return nil, domain.Err("conflict", "Задача изменилась. Проверьте лимиты заново", 409)
		}
		if domain.Terminal(t.Status) || t.Status == "blocked" || t.FinancialHold || t.ModelConnectionID == "" {
			return nil, domain.Err("invalid_transition", "Для этой задачи автоматизация сейчас недоступна", 409)
		}
		var unknown bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM tool_executions WHERE organization_id=$1 AND task_id=$2 AND status='unknown')", p.OrganizationID, taskID).Scan(&unknown); err != nil {
			return nil, err
		}
		if unknown {
			return nil, domain.Err("invalid_transition", "Сначала установите исход неизвестного внешнего действия", 409)
		}
		var modelActive bool
		if err = tx.QueryRow(ctx, "SELECT active FROM model_connections WHERE organization_id=$1 AND id=$2 AND version=$3 FOR SHARE", p.OrganizationID, t.ModelConnectionID, t.ModelConnectionVersion).Scan(&modelActive); err != nil {
			return nil, rowExists(err)
		}
		if !modelActive {
			return nil, domain.Forbidden
		}
		var pins []byte
		if err = tx.QueryRow(ctx, "SELECT model_tools FROM agents WHERE organization_id=$1 AND id=$2 AND version=$3", p.OrganizationID, t.AgentID, t.AgentVersion).Scan(&pins); err != nil {
			return nil, rowExists(err)
		}
		var definitions []domain.ModelToolDefinition
		if json.Unmarshal(pins, &definitions) != nil || len(definitions) == 0 {
			return nil, automationSnapshotConflict()
		}
		for _, pin := range in.ToolPins {
			bound := false
			for _, d := range definitions {
				if d.ToolID == pin.ToolID && d.ToolVersion == pin.ToolVersion && d.ToolProfileDigest == pin.ToolProfileDigest {
					bound = true
					break
				}
			}
			if !bound {
				return nil, domain.Err("conflict", "Инструмент не закреплён за этой версией агента", 409)
			}
			c, e := loadToolConnectionTx(ctx, tx, p.OrganizationID, pin.ToolID, pin.ToolVersion, true)
			if e != nil {
				return nil, e
			}
			if !c.Tool.Active || domain.ToolConnectionDigest(c) != pin.ToolProfileDigest {
				return nil, domain.Forbidden
			}
			if in.Mode == "read_only" && (c.Tool.Effect != "read" || !c.Tool.AutomationAllowed || c.Tool.RequireDistinctApprover) {
				return nil, domain.Err("invalid_request", "Самостоятельное выполнение разрешено только для явно разрешённого чтения", 400)
			}
		}
		u, err := automationUsageTx(ctx, tx, p.OrganizationID, taskID)
		if err != nil {
			return nil, err
		}
		if in.MaxModelTurns < u.ModelTurns || in.MaxToolExecutions < u.ToolExecutions || in.MaxChargeMicroUSD < u.AllocatedMicroUSD {
			return nil, domain.Err("invalid_request", "Новые лимиты не могут быть ниже уже использованных за всё время задачи", 400)
		}
		var now time.Time
		if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
			return nil, err
		}
		now = now.UTC()
		if _, err = tx.Exec(ctx, "UPDATE task_automation_grants SET status='expired',version=version+1,stop_reason='grant_expired' WHERE organization_id=$1 AND task_id=$2 AND status='active' AND expires_at<=clock_timestamp()", p.OrganizationID, taskID); err != nil {
			return nil, err
		}
		var active bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM task_automation_grants WHERE organization_id=$1 AND task_id=$2 AND status='active')", p.OrganizationID, taskID).Scan(&active); err != nil {
			return nil, err
		}
		if active {
			return nil, domain.Err("conflict", "Сначала отзовите действующее разрешение", 409)
		}
		expires := now.Add(time.Duration(in.ExpiresInSeconds) * time.Second)
		if t.ExecutionDeadline.Before(expires) {
			expires = t.ExecutionDeadline.UTC()
		}
		if !now.Before(expires) {
			return nil, domain.Err("invalid_transition", "Срок задачи истёк", 409)
		}
		g := domain.TaskAutomationGrant{ID: domain.NewID("aut"), OrganizationID: p.OrganizationID, TaskID: taskID, PrincipalID: p.ID, OwnerID: t.OwnerID, AgentID: t.AgentID, AgentVersion: t.AgentVersion, ModelConnectionID: t.ModelConnectionID, ModelConnectionVersion: t.ModelConnectionVersion, Version: 1, Status: "active", Mode: in.Mode, ToolPins: in.ToolPins, MaxModelTurns: in.MaxModelTurns, MaxToolExecutions: in.MaxToolExecutions, MaxChargeMicroUSD: in.MaxChargeMicroUSD, Reason: in.Reason, CreatedAt: now, ExpiresAt: expires}
		body, _ := json.Marshal(g)
		if err = currentActorTx(ctx, tx, p); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO task_automation_grants(organization_id,id,task_id,principal_id,owner_id,grant_body,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", g.OrganizationID, g.ID, g.TaskID, g.PrincipalID, g.OwnerID, body, g.CreatedAt, g.ExpiresAt); err != nil {
			return nil, err
		}
		if _, err = currentAutomationAuthorityTx(ctx, tx, g, t); err != nil {
			return nil, err
		}
		if err = emit(ctx, tx, p.OrganizationID, taskID, "automation.granted", map[string]any{"grant_id": g.ID, "mode": g.Mode, "principal_id": p.ID, "expires_at": g.ExpiresAt, "max_model_turns": g.MaxModelTurns, "max_tool_executions": g.MaxToolExecutions, "max_charge_microusd": in.MaxChargeMicroUSD, "independent_of_login_session": true, "acceptance_automated": false}); err != nil {
			return nil, err
		}
		return automationStateTx(ctx, tx, p, taskID)
	})
}
func (s *Store) RevokeAutomation(ctx context.Context, p domain.Principal, taskID, key string, in domain.RevokeTaskAutomation) (json.RawMessage, error) {
	if !validText(in.GrantID, 128) || in.ExpectedVersion < 1 || !validText(in.Reason, 4096) {
		return nil, domain.Err("invalid_request", "Укажите точное разрешение, версию и причину отзыва", 400)
	}
	return s.MutateGuarded(ctx, p, "POST /tasks/"+taskID+"/automation/revoke", key, in, automationGuard(taskID), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		if _, _, err := lockToolObservationTaskTx(ctx, tx, p.OrganizationID, taskID); err != nil {
			return nil, err
		}
		g, err := loadAutomationGrantTx(ctx, tx, p.OrganizationID, in.GrantID, true)
		if err != nil {
			return nil, err
		}
		if g.TaskID != taskID {
			return nil, domain.NotFound
		}
		if g.Status != "active" || g.Version != in.ExpectedVersion {
			return nil, domain.Err("conflict", "Разрешение уже изменилось", 409)
		}
		if err = currentActorTx(ctx, tx, p); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, "UPDATE task_automation_grants SET status='revoked',version=version+1,stop_reason=$3 WHERE organization_id=$1 AND id=$2", p.OrganizationID, g.ID, in.Reason); err != nil {
			return nil, err
		}
		if err = emit(ctx, tx, p.OrganizationID, taskID, "automation.revoked", map[string]any{"grant_id": g.ID, "principal_id": p.ID, "reason": in.Reason, "past_effects_preserved": true}); err != nil {
			return nil, err
		}
		return automationStateTx(ctx, tx, p, taskID)
	})
}
