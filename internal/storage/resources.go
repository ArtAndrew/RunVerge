// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/domain"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

func (s *Store) CreateAgent(ctx context.Context, p domain.Principal, key string, in domain.CreateAgent) (json.RawMessage, error) {
	if !validText(in.Name, 120) || !validText(in.Instructions, 32768) {
		return nil, domain.Err("invalid_request", "Нужны имя до 120 байт и инструкции до 32 КиБ", 400)
	}
	if in.Model == "" {
		in.Model = domain.FixtureModel
	}
	if in.Team != nil {
		definition := *in.Team
		definition.Members = append([]domain.TeamMember(nil), in.Team.Members...)
		if definition.MaxParallel == 0 {
			definition.MaxParallel = 1
			if definition.Pattern == "parallel" {
				definition.MaxParallel = len(definition.Members)
			}
		}
		if domain.ValidateTeam(definition) != nil {
			return nil, domain.Err("invalid_request", "Некорректная схема команды", 400)
		}
		if in.Model != domain.FixtureModel && in.Model != domain.TeamModel {
			return nil, domain.Err("invalid_request", "У команды должен быть координатор", 400)
		}
		in.Model = domain.TeamModel
		in.Team = &definition
	} else if in.Model == domain.TeamModel {
		return nil, domain.Err("invalid_request", "Нужно определение команды", 400)
	}
	if in.BudgetMicroUSD == nil {
		value := int64(1_000_000)
		in.BudgetMicroUSD = &value
	}
	if *in.BudgetMicroUSD < 1 || *in.BudgetMicroUSD > 1_000_000_000_000 {
		return nil, domain.Err("invalid_request", "Некорректный бюджет агента", 400)
	}
	if len(in.Tools) > domain.MaxModelTools || (len(in.Tools) > 0 && (in.Model == domain.FixtureModel || in.Model == domain.TeamModel)) {
		return nil, domain.Err("invalid_request", "Инструменты доступны модельному агенту, не более 16", 400)
	}
	seenTools := map[string]bool{}
	for _, id := range in.Tools {
		if !validText(id, 128) || seenTools[id] {
			return nil, domain.Err("invalid_request", "Некорректный или повторный инструмент", 400)
		}
		seenTools[id] = true
	}
	if in.Tools == nil {
		in.Tools = []string{}
	}
	return s.MutateGuarded(ctx, p, "POST /agents", key, in, principalPermissionGuard("agents.write"), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		var orgActive bool
		if e := tx.QueryRow(ctx, "SELECT active FROM organizations WHERE id=$1 FOR UPDATE", p.OrganizationID).Scan(&orgActive); e != nil {
			return nil, e
		}
		if !orgActive {
			return nil, domain.Forbidden
		}
		if in.Model != domain.FixtureModel && in.Model != domain.TeamModel {
			var modelID string
			var modelVersion int
			if e := tx.QueryRow(ctx, "SELECT id,version FROM model_connections WHERE organization_id=$1 AND model=$2 AND active FOR SHARE", p.OrganizationID, in.Model).Scan(&modelID, &modelVersion); e != nil {
				if errors.Is(e, pgx.ErrNoRows) {
					return nil, domain.Err("invalid_request", "Модель не подключена к организации", 400)
				}
				return nil, e
			}
			if e := checkConfigurationResourceAdmissionTx(ctx, tx, p.OrganizationID, "model", modelID, modelVersion); e != nil {
				return nil, e
			}
		}
		bindings := []domain.ModelToolDefinition{}
		for _, id := range in.Tools {
			var toolVersion int
			if err := tx.QueryRow(ctx, "SELECT version FROM tool_connections WHERE organization_id=$1 AND id=$2 AND active FOR SHARE", p.OrganizationID, id).Scan(&toolVersion); err != nil {
				return nil, rowExists(err)
			}
			c, err := loadToolConnectionTx(ctx, tx, p.OrganizationID, id, toolVersion, true)
			if err != nil {
				return nil, err
			}
			if !c.Tool.Active {
				return nil, domain.Forbidden
			}
			bindings = append(bindings, domain.ModelToolDefinition{ToolID: id, ToolVersion: c.Tool.Version, ToolProfileDigest: domain.ToolConnectionDigest(c), Name: domain.ModelToolName(p.OrganizationID, id, c.Tool.Version), Description: c.Tool.Description, InputSchemaJSON: c.Tool.InputSchemaJSON})
		}
		tools, _ := json.Marshal(in.Tools)
		modelTools, _ := json.Marshal(bindings)
		var team any
		if in.Team != nil {
			body, _ := json.Marshal(in.Team)
			team = body
		}
		return scanAgent(tx.QueryRow(ctx, `INSERT INTO agents(organization_id,id,name,instructions,model,tools,budget_microusd,team,model_tools) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+agentColumns, p.OrganizationID, domain.NewID("agt"), in.Name, in.Instructions, in.Model, tools, *in.BudgetMicroUSD, team, modelTools))
	})
}
func (s *Store) PublishAgent(ctx context.Context, p domain.Principal, id, key string, version int) (json.RawMessage, error) {
	return s.MutateGuarded(ctx, p, "POST /agents/"+id+"/publish", key, map[string]int{"expected_version": version}, principalPermissionGuard("agents.write"), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		a, e := scanAgent(tx.QueryRow(ctx, "SELECT "+agentColumns+" FROM agents WHERE organization_id=$1 AND id=$2 ORDER BY version DESC LIMIT 1 FOR UPDATE", p.OrganizationID, id))
		if e != nil {
			return nil, e
		}
		if a.Version != version {
			return nil, domain.Err("conflict", "Версия агента изменилась", 409)
		}
		for _, binding := range a.ModelTools {
			c, err := loadToolConnectionTx(ctx, tx, p.OrganizationID, binding.ToolID, binding.ToolVersion, true)
			if err != nil {
				return nil, err
			}
			if !c.Tool.Active || domain.ToolConnectionDigest(c) != binding.ToolProfileDigest {
				return nil, domain.Err("conflict", "Закреплённый инструмент отключён или изменился", 409)
			}
		}
		_, e = tx.Exec(ctx, "UPDATE agents SET published=true WHERE organization_id=$1 AND id=$2 AND version=$3", p.OrganizationID, id, version)
		a.Published = true
		if e == nil && a.Team != nil {
			_, e = compileTeamPlanTx(ctx, tx, domain.Task{OrganizationID: p.OrganizationID, BudgetMicroUSD: a.BudgetMicroUSD}, a)
		}
		return a, e
	})
}
func (s *Store) CreateTask(ctx context.Context, p domain.Principal, key string, in domain.CreateTask) (json.RawMessage, error) {
	if !validText(in.Goal, 16384) || !validText(in.AgentID, 128) || in.BudgetMicroUSD < 1 || in.BudgetMicroUSD > 1_000_000_000_000 {
		return nil, domain.Err("invalid_request", "Нужны цель, опубликованный агент и положительный бюджет", 400)
	}
	maxRuns, seconds := 3, 86400
	if in.MaxRuns != nil {
		maxRuns = *in.MaxRuns
	}
	if in.ExecutionTimeoutSeconds != nil {
		seconds = *in.ExecutionTimeoutSeconds
	}
	if maxRuns < 1 || maxRuns > 10 || seconds < 1 || seconds > 2592000 {
		return nil, domain.Err("invalid_request", "Предел попыток должен быть 1…10, срок 1…2592000 секунд", 400)
	}
	return s.MutateGuarded(ctx, p, "POST /tasks", key, in, principalPermissionGuard("tasks.create"), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		var pendingLimit int
		if e := tx.QueryRow(ctx, "SELECT pending_task_limit FROM organizations WHERE id=$1 FOR UPDATE", p.OrganizationID).Scan(&pendingLimit); e != nil {
			return nil, e
		}
		var pending int
		if e := tx.QueryRow(ctx, "SELECT count(*) FROM tasks WHERE organization_id=$1 AND status NOT IN('succeeded','failed','cancelled')", p.OrganizationID).Scan(&pending); e != nil {
			return nil, e
		}
		if pending >= pendingLimit {
			return nil, &domain.Error{Code: "unavailable", Message: "Достигнут лимит незавершённых задач организации", Status: 503, Retryable: true}
		}
		a, e := scanAgent(tx.QueryRow(ctx, "SELECT "+agentColumns+" FROM agents WHERE organization_id=$1 AND id=$2 AND published ORDER BY version DESC LIMIT 1", p.OrganizationID, in.AgentID))
		if e != nil {
			return nil, e
		}
		if in.BudgetMicroUSD > a.BudgetMicroUSD {
			return nil, domain.Err("budget_exceeded", "Бюджет выше лимита опубликованного агента", 409)
		}
		var modelID string
		var modelVersion int
		if a.Model != domain.FixtureModel && a.Model != domain.TeamModel {
			if e = tx.QueryRow(ctx, "SELECT id,version FROM model_connections WHERE organization_id=$1 AND model=$2 AND active FOR SHARE", p.OrganizationID, a.Model).Scan(&modelID, &modelVersion); e != nil {
				if errors.Is(e, pgx.ErrNoRows) {
					return nil, domain.Err("invalid_request", "Подключение модели отключено", 400)
				}
				return nil, e
			}
			if e = checkConfigurationResourceAdmissionTx(ctx, tx, p.OrganizationID, "model", modelID, modelVersion); e != nil {
				return nil, e
			}
		}
		id := domain.NewID("tsk")
		run := domain.NewID("run")
		op := domain.NewID("op")
		depthLimit := domain.MaxTaskDepth
		if a.Team != nil {
			depthLimit = a.Team.MaxDepth
		}
		t, e := scanTask(tx.QueryRow(ctx, `INSERT INTO tasks(organization_id,id,owner_id,agent_id,agent_version,goal,status,budget_microusd,model_connection_id,model_connection_version,max_runs,execution_deadline,tree_depth_limit) VALUES($1,$2,$3,$4,$5,$6,'queued',$7,NULLIF($8,''),NULLIF($9::integer,0),$10,clock_timestamp()+make_interval(secs=>$11::integer),$12) RETURNING `+taskColumns, p.OrganizationID, id, p.ID, a.ID, a.Version, in.Goal, in.BudgetMicroUSD, modelID, modelVersion, maxRuns, seconds, depthLimit))
		if e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO runs(organization_id,id,task_id,principal_id,agent_id,agent_version,status) VALUES($1,$2,$3,$4,$5,$6,'queued')`, p.OrganizationID, run, id, p.ID, a.ID, a.Version); e != nil {
			return nil, e
		}
		opStatus := "queued"
		if a.Team != nil {
			opStatus = "blocked"
		}
		if _, e = tx.Exec(ctx, `INSERT INTO operations(organization_id,id,task_id,run_id,status) VALUES($1,$2,$3,$4,$5)`, p.OrganizationID, op, id, run, opStatus); e != nil {
			return nil, e
		}
		t, e = scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND id=$2", p.OrganizationID, id))
		if e != nil {
			return nil, e
		}
		if a.Team != nil {
			if e = initTeamTx(ctx, tx, t, run, *a.Team); e != nil {
				return nil, e
			}
		}
		return t, emit(ctx, tx, p.OrganizationID, id, "task.created", map[string]any{"run_id": run, "agent_version": a.Version})
	})
}
func (s *Store) CancelTask(ctx context.Context, p domain.Principal, id, key string) (json.RawMessage, error) {
	return s.cancelTaskTree(ctx, p, id, key)
}

func (s *Store) GetTask(ctx context.Context, p domain.Principal, id string) (domain.Task, error) {
	tx, err := s.beginPrincipalRead(ctx, p, "tasks.read")
	if err != nil {
		return domain.Task{}, err
	}
	defer tx.Rollback(ctx)
	v, err := scanTask(tx.QueryRow(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND id=$2", p.OrganizationID, id))
	if err != nil {
		return v, err
	}
	return v, tx.Commit(ctx)
}

type cursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

func encodeCursor(t time.Time, id string) string {
	b, _ := json.Marshal(cursor{t, id})
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodeCursor(raw string) (cursor, error) {
	var c cursor
	if raw == "" {
		return cursor{CreatedAt: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), ID: "~"}, nil
	}
	invalid := domain.Err("invalid_request", "Некорректный cursor", 400)
	if len(raw) > 512 {
		return c, invalid
	}
	b, e := base64.RawURLEncoding.DecodeString(raw)
	if e != nil || !utf8.Valid(b) {
		return c, invalid
	}
	// A nonempty cursor must contain both exact canonical fields. Decode from
	// zero values, not the initial-page sentinel, and reject duplicates/aliases.
	d := json.NewDecoder(bytes.NewReader(b))
	opening, e := d.Token()
	if e != nil || opening != json.Delim('{') {
		return c, invalid
	}
	seen := map[string]bool{}
	for d.More() {
		token, e := d.Token()
		key, ok := token.(string)
		if e != nil || !ok || seen[key] {
			return c, invalid
		}
		seen[key] = true
		switch key {
		case "created_at":
			e = d.Decode(&c.CreatedAt)
		case "id":
			e = d.Decode(&c.ID)
		default:
			return c, invalid
		}
		if e != nil {
			return c, invalid
		}
	}
	closing, e := d.Token()
	if e != nil || closing != json.Delim('}') || len(seen) != 2 || !validText(c.ID, 128) || c.CreatedAt.IsZero() {
		return c, invalid
	}
	if e = d.Decode(&struct{}{}); e != io.EOF {
		return c, invalid
	}
	return c, nil
}
func (s *Store) ListTasks(ctx context.Context, p domain.Principal, raw string, limit int) ([]domain.Task, string, error) {
	c, e := decodeCursor(raw)
	if e != nil {
		return nil, "", e
	}
	tx, e := s.beginPrincipalRead(ctx, p, "tasks.read")
	if e != nil {
		return nil, "", e
	}
	defer tx.Rollback(ctx)
	rows, e := tx.Query(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND (created_at,id)<($2,$3) ORDER BY created_at DESC,id DESC LIMIT $4", p.OrganizationID, c.CreatedAt, c.ID, limit+1)
	if e != nil {
		return nil, "", e
	}
	defer rows.Close()
	out := []domain.Task{}
	for rows.Next() {
		t, e := scanTask(rows)
		if e != nil {
			return nil, "", e
		}
		out = append(out, t)
	}
	if e = rows.Err(); e != nil {
		return nil, "", e
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[limit-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, tx.Commit(ctx)
}
func (s *Store) ListAgents(ctx context.Context, p domain.Principal, raw string, limit int) ([]domain.Agent, string, error) {
	c, e := decodeCursor(raw)
	if e != nil {
		return nil, "", e
	}
	tx, e := s.beginPrincipalRead(ctx, p, "agents.read")
	if e != nil {
		return nil, "", e
	}
	defer tx.Rollback(ctx)
	rows, e := tx.Query(ctx, "SELECT "+agentColumns+" FROM agents WHERE organization_id=$1 AND (created_at,id)<($2,$3) ORDER BY created_at DESC,id DESC LIMIT $4", p.OrganizationID, c.CreatedAt, c.ID, limit+1)
	if e != nil {
		return nil, "", e
	}
	defer rows.Close()
	out := []domain.Agent{}
	for rows.Next() {
		a, e := scanAgent(rows)
		if e != nil {
			return nil, "", e
		}
		out = append(out, a)
	}
	if e = rows.Err(); e != nil {
		return nil, "", e
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[limit-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, tx.Commit(ctx)
}
func (s *Store) Events(ctx context.Context, p domain.Principal, id string, after int64, limit int) ([]domain.Event, string, error) {
	if after < 0 || limit < 1 || limit > 100 {
		return nil, "", domain.Err("invalid_request", "Некорректные after или limit событий", 400)
	}
	tx, e := s.beginMaterialRead(ctx, p, id)
	if e != nil {
		return nil, "", e
	}
	defer tx.Rollback(ctx)
	rows, e := tx.Query(ctx, "SELECT id,organization_id,task_id,sequence,type,data::text,created_at,payload_version FROM events WHERE organization_id=$1 AND task_id=$2 AND sequence>$3 ORDER BY sequence LIMIT $4", p.OrganizationID, id, after, limit+1)
	if e != nil {
		return nil, "", e
	}
	defer rows.Close()
	out := []domain.Event{}
	for rows.Next() {
		v, scanErr := scanTaskEvent(rows)
		if scanErr != nil {
			return nil, "", scanErr
		}
		out = append(out, v)
	}
	if e = rows.Err(); e != nil {
		return nil, "", e
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		b, _ := json.Marshal(out[limit-1].Sequence)
		next = string(b)
	}
	return out, next, tx.Commit(ctx)
}
func (s *Store) Artifacts(ctx context.Context, p domain.Principal, id string) ([]domain.Artifact, error) {
	tx, e := s.beginMaterialRead(ctx, p, id)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	rows, e := tx.Query(ctx, "SELECT id,organization_id,task_id,name,media_type,content,version,created_at,provenance FROM artifacts WHERE organization_id=$1 AND task_id=$2 ORDER BY created_at,id", p.OrganizationID, id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []domain.Artifact{}
	for rows.Next() {
		var a domain.Artifact
		var data []byte
		if e = rows.Scan(&a.ID, &a.OrganizationID, &a.TaskID, &a.Name, &a.MediaType, &a.Content, &a.Version, &a.CreatedAt, &data); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(data, &a.Provenance); e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	return out, tx.Commit(ctx)
}
func rowExists(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound
	}
	return err
}
func validText(value string, max int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && len(value) <= max && !strings.ContainsRune(value, '\x00')
}
