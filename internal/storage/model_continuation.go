// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/jsonwire"
	"agentplatform.local/platform/internal/modelruntime"
	"agentplatform.local/platform/internal/toolruntime"
	"github.com/jackc/pgx/v5"
)

const modelProposalColumns = "p.id,p.organization_id,p.task_id,p.run_id,p.operation_id,p.candidate_id,p.artifact_id,p.artifact_version,p.content_digest,p.call_id,p.tool_id,p.tool_version,p.tool_profile_digest,p.tool_name,p.arguments_json,p.arguments_digest,COALESCE(e.id,''),COALESCE(e.status,''),p.created_at"

var modelWireName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func scanModelProposal(row scanner) (p domain.ModelToolProposal, err error) {
	err = row.Scan(&p.ID, &p.OrganizationID, &p.TaskID, &p.RunID, &p.OperationID, &p.CandidateID, &p.ArtifactID, &p.ArtifactVersion, &p.ContentDigest, &p.CallID, &p.ToolID, &p.ToolVersion, &p.ToolProfileDigest, &p.ToolName, &p.ArgumentsJSON, &p.ArgumentsDigest, &p.ExecutionID, &p.ExecutionStatus, &p.CreatedAt)
	if err != nil {
		return p, rowExists(err)
	}
	if p.ToolName != domain.ModelToolName(p.OrganizationID, p.ToolID, p.ToolVersion) || Hash([]byte(p.ArgumentsJSON)) != p.ArgumentsDigest || !validDigest(p.ContentDigest) || !validDigest(p.ToolProfileDigest) {
		return p, modelSnapshotConflict()
	}
	return p, nil
}

func modelSnapshotConflict() error {
	return domain.Err("conflict", "Сохранённый контекст выполнения модели не соответствует источникам", 409)
}

func loadModelProposalTx(ctx context.Context, tx pgx.Tx, org, taskID, id string) (domain.ModelToolProposal, error) {
	return scanModelProposal(tx.QueryRow(ctx, "SELECT "+modelProposalColumns+" FROM model_tool_proposals p LEFT JOIN tool_executions e ON e.organization_id=p.organization_id AND e.model_proposal_id=p.id WHERE p.organization_id=$1 AND p.task_id=$2 AND p.id=$3", org, taskID, id))
}

func (s *Store) ModelProposal(ctx context.Context, p domain.Principal, taskID, id string) (domain.ModelToolProposal, error) {
	var empty domain.ModelToolProposal
	tx, err := s.beginMaterialRead(ctx, p, taskID)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	v, err := loadModelProposalTx(ctx, tx, p.OrganizationID, taskID, id)
	if err != nil {
		return empty, err
	}
	return v, tx.Commit(ctx)
}

func (s *Store) ModelProposals(ctx context.Context, p domain.Principal, taskID, raw string, limit int) ([]domain.ModelToolProposal, string, error) {
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
	rows, err := tx.Query(ctx, "SELECT "+modelProposalColumns+" FROM model_tool_proposals p LEFT JOIN tool_executions e ON e.organization_id=p.organization_id AND e.model_proposal_id=p.id WHERE p.organization_id=$1 AND p.task_id=$2 AND (p.created_at,p.id)<($3,$4) ORDER BY p.created_at DESC,p.id DESC LIMIT $5", p.OrganizationID, taskID, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	values := []domain.ModelToolProposal{}
	for rows.Next() {
		v, readErr := scanModelProposal(rows)
		if readErr != nil {
			rows.Close()
			return nil, "", readErr
		}
		v.ArgumentsJSON = ""
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
	return values, next, tx.Commit(ctx)
}

// Decode the internal transcript without accepting duplicate fields, repaired
// Unicode or an unknown schema. Hash equality alone cannot establish its shape.
func decodeModelMessages(raw string) ([]domain.ModelMessage, error) {
	if raw == "" || len(raw) > domain.MaxModelTranscriptBytes || !jsonwire.ValidUnicode([]byte(raw)) {
		return nil, modelSnapshotConflict()
	}
	d := json.NewDecoder(strings.NewReader(raw))
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 16 {
			return modelSnapshotConflict()
		}
		tok, err := d.Token()
		if err != nil {
			return err
		}
		if del, ok := tok.(json.Delim); ok {
			switch del {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					key, err := d.Token()
					name, ok := key.(string)
					if err != nil || !ok || seen[name] {
						return modelSnapshotConflict()
					}
					seen[name] = true
					if err = walk(depth + 1); err != nil {
						return err
					}
				}
				end, err := d.Token()
				if err != nil || end != json.Delim('}') {
					return modelSnapshotConflict()
				}
			case '[':
				for d.More() {
					if err = walk(depth + 1); err != nil {
						return err
					}
				}
				end, err := d.Token()
				if err != nil || end != json.Delim(']') {
					return modelSnapshotConflict()
				}
			default:
				return modelSnapshotConflict()
			}
		}
		return nil
	}
	if err := walk(0); err != nil {
		return nil, modelSnapshotConflict()
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, modelSnapshotConflict()
	}
	d = json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	var messages []domain.ModelMessage
	if err := d.Decode(&messages); err != nil || len(messages) < 2 || len(messages) > 2+(domain.MaxModelTurnsPerTask-1)*(domain.MaxModelToolCalls+1) {
		return nil, modelSnapshotConflict()
	}
	return messages, nil
}

// loadModelContextTx rebuilds the worker-visible context from durable state and
// current tool activation. The returned definitions contain no private profile.
func loadModelContextTx(ctx context.Context, tx pgx.Tx, task domain.Task, l *domain.Lease) (int, error) {
	var ordinal int
	var raw, digest string
	if err := tx.QueryRow(ctx, "SELECT ordinal,messages_json,messages_digest FROM model_turns WHERE organization_id=$1 AND operation_id=$2 AND task_id=$3 AND run_id=$4", task.OrganizationID, l.OperationID, task.ID, l.RunID).Scan(&ordinal, &raw, &digest); err != nil {
		return 0, rowExists(err)
	}
	if Hash([]byte(raw)) != digest || ordinal < 1 || ordinal > domain.MaxModelTurnsPerTask {
		return 0, modelSnapshotConflict()
	}
	messages, err := decodeModelMessages(raw)
	if err != nil {
		return 0, err
	}
	connections, err := modelToolConnectionsTx(ctx, tx, task, l.Agent)
	if err != nil {
		return 0, err
	}
	if messages[0].Role != "system" || messages[0].Content != l.Agent.Instructions || messages[0].ToolCallID != "" || len(messages[0].ToolCalls) != 0 || messages[1].Role != "user" || messages[1].Content != domain.ExecutionInput(*l) || messages[1].ToolCallID != "" || len(messages[1].ToolCalls) != 0 {
		return 0, modelSnapshotConflict()
	}
	seen := map[string]bool{}
	batches := 0
	for i := 2; i < len(messages); {
		assistant := messages[i]
		if assistant.Role != "assistant" || assistant.ToolCallID != "" || !optionalText(assistant.Content, 262144) || len(assistant.ToolCalls) < 1 || len(assistant.ToolCalls) > domain.MaxModelToolCalls || len(messages)-i-1 < len(assistant.ToolCalls) {
			return 0, modelSnapshotConflict()
		}
		for j, call := range assistant.ToolCalls {
			c, exists := connections[call.Name]
			if !exists || !modelWireName.MatchString(call.ID) || seen[call.ID] {
				return 0, modelSnapshotConflict()
			}
			seen[call.ID] = true
			if _, err := toolruntime.CanonicalArguments(c, call.ArgumentsJSON); err != nil {
				return 0, modelSnapshotConflict()
			}
			result := messages[i+j+1]
			if result.Role != "tool" || result.ToolCallID != call.ID || len(result.ToolCalls) != 0 || !validText(result.Content, domain.MaxRunInputBytes) || !json.Valid([]byte(result.Content)) {
				return 0, modelSnapshotConflict()
			}
		}
		i += len(assistant.ToolCalls) + 1
		batches++
	}
	if batches != ordinal-1 {
		return 0, modelSnapshotConflict()
	}
	l.ModelTools = append([]domain.ModelToolDefinition(nil), l.Agent.ModelTools...)
	l.ModelMessages = messages
	// Preserve legacy first-turn intent hashes for existing text-only jobs.
	if ordinal == 1 && len(l.ModelTools) == 0 {
		l.ModelMessages = nil
	}
	return ordinal, nil
}

func modelToolConnectionsTx(ctx context.Context, tx pgx.Tx, task domain.Task, agent domain.Agent) (map[string]domain.ToolConnection, error) {
	if len(agent.Tools) != len(agent.ModelTools) || len(agent.ModelTools) > domain.MaxModelTools {
		return nil, modelSnapshotConflict()
	}
	connections := make(map[string]domain.ToolConnection, len(agent.ModelTools))
	for i, def := range agent.ModelTools {
		if def.ToolID != agent.Tools[i] || def.Name != domain.ModelToolName(task.OrganizationID, def.ToolID, def.ToolVersion) {
			return nil, modelSnapshotConflict()
		}
		if _, exists := connections[def.Name]; exists {
			return nil, modelSnapshotConflict()
		}
		c, err := loadToolConnectionTx(ctx, tx, task.OrganizationID, def.ToolID, def.ToolVersion, true)
		if err != nil {
			return nil, err
		}
		if !c.Tool.Active {
			return nil, domain.Err("forbidden", "Закреплённый инструмент отключён", 403)
		}
		if domain.ToolConnectionDigest(c) != def.ToolProfileDigest || c.Tool.InputSchemaJSON != def.InputSchemaJSON || c.Tool.Description != def.Description || toolruntime.ValidateConnection(c) != nil {
			return nil, modelSnapshotConflict()
		}
		connections[def.Name] = c
	}
	return connections, nil
}

func checkModelTurnBudgetTx(ctx context.Context, tx pgx.Tx, task domain.Task) error {
	var own, tree int
	if err := tx.QueryRow(ctx, "SELECT COALESCE((SELECT own_turns FROM model_turn_counts WHERE organization_id=$1 AND task_id=$2),0),COALESCE((SELECT tree_turns FROM model_turn_counts WHERE organization_id=$1 AND task_id=$3),0)", task.OrganizationID, task.ID, task.RootID).Scan(&own, &tree); err != nil {
		return err
	}
	if own >= domain.MaxModelTurnsPerTask || tree >= domain.MaxModelTurnsPerRoot {
		return domain.Err("invalid_transition", "Исчерпан пожизненный предел шагов модели дерева", 409)
	}
	return nil
}

type modelToolCandidateBody struct {
	Text      string                 `json:"text"`
	ToolCalls []domain.ModelToolCall `json:"tool_calls"`
}

func modelCandidateContent(c domain.ModelCandidate) (string, string, error) {
	if len(c.ToolCalls) == 0 {
		if !validText(c.Text, 262144) {
			return "", "", domain.Err("invalid_request", "Некорректный кандидат результата", 400)
		}
		return c.Text, "text/plain", nil
	}
	if len(c.ToolCalls) > domain.MaxModelToolCalls || !optionalText(c.Text, 262144) {
		return "", "", domain.Err("invalid_request", "Некорректный пакет предложений модели", 400)
	}
	raw, err := json.Marshal(modelToolCandidateBody{Text: c.Text, ToolCalls: c.ToolCalls})
	if err != nil || len(raw) > 262144 {
		return "", "", domain.Err("invalid_request", "Пакет предложений модели превышает предел", 400)
	}
	return string(raw), "application/json", nil
}

// A tool response is untrusted input regardless of who recorded its receipt.
// The complete receipt method is retained, including an operator assertion.
type modelQuotedToolResult struct {
	ProposalID  string                     `json:"proposal_id"`
	ExecutionID string                     `json:"execution_id"`
	Status      string                     `json:"status"`
	Receipt     *domain.ToolOutcomeReceipt `json:"receipt,omitempty"`
	Result      string                     `json:"result,omitempty"`
	Reason      string                     `json:"reason,omitempty"`
	Untrusted   bool                       `json:"untrusted"`
}

func (s *Store) ContinueModel(ctx context.Context, p domain.Principal, taskID, key string, in domain.ContinueModelRequest) (json.RawMessage, error) {
	if _, err := validateContinueModelRequest(in); err != nil {
		return nil, err
	}
	guard := func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		return taskMutationPermission(ctx, tx, p, taskID, "tasks.continue_own", "tasks.continue_any")
	}
	return s.MutateGuarded(ctx, p, "POST /tasks/"+taskID+"/continue-model", key, in, guard, func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		return continueModelTx(ctx, tx, p, taskID, in, currentActorTx, "")
	})
}

func validateContinueModelRequest(in domain.ContinueModelRequest) (map[string]domain.ModelToolResultBinding, error) {
	if in.ExpectedTaskVersion < 1 || !validText(in.CandidateID, 128) || !validText(in.ArtifactID, 128) || in.ArtifactVersion < 1 || !validDigest(in.ContentDigest) || len(in.Results) < 1 || len(in.Results) > domain.MaxModelToolCalls || !in.AcknowledgeCost || !validText(in.Reason, 4096) {
		return nil, domain.Err("invalid_request", "Нужны точный пакет результатов и подтверждение следующего платного шага", 400)
	}
	bindings := map[string]domain.ModelToolResultBinding{}
	for _, b := range in.Results {
		if !validText(b.ProposalID, 128) || !validText(b.ExecutionID, 128) || b.ExecutionVersion < 1 || bindings[b.ProposalID].ProposalID != "" {
			return nil, domain.Err("invalid_request", "Некорректная привязка результата инструмента", 400)
		}
		switch b.Status {
		case "succeeded", "rejected":
			if !validText(b.ReceiptID, 128) || !validDigest(b.ContentDigest) {
				return nil, domain.Err("invalid_request", "Для ответа инструмента нужна точная квитанция", 400)
			}
		case "denied", "expired", "cancelled":
			if b.ReceiptID != "" || b.ContentDigest != "" {
				return nil, domain.Err("invalid_request", "У невыполненного действия не должно быть квитанции эффекта", 400)
			}
		default:
			return nil, domain.Err("invalid_transition", "Продолжение требует завершённых действий без неизвестного эффекта", 409)
		}
		bindings[b.ProposalID] = b
	}
	return bindings, nil
}

func continueModelTx(ctx context.Context, tx pgx.Tx, p domain.Principal, taskID string, in domain.ContinueModelRequest, authority commandAuthority, automationGrantID string) (domain.Task, error) {
	if authority == nil {
		return domain.Task{}, domain.Forbidden
	}
	bindings, err := validateContinueModelRequest(in)
	if err != nil {
		return domain.Task{}, err
	}
	task, tasks, err := lockToolTaskTx(ctx, tx, p.OrganizationID, taskID)
	if err != nil {
		return domain.Task{}, err
	}
	if task.Version != in.ExpectedTaskVersion {
		return domain.Task{}, domain.Err("conflict", "Задача изменилась. Обновите результаты перед продолжением", 409)
	}
	if task.Status != "waiting_input" || task.WaitingReason != "model_tools_pending" || task.CandidateID != in.CandidateID || task.ModelConnectionID == "" {
		return domain.Task{}, domain.Err("invalid_transition", "Этот пакет предложений больше не ожидает продолжения", 409)
	}
	if err = noActiveToolExecutionsTx(ctx, tx, p.OrganizationID, subtreeIDs(tasks, taskID, true)); err != nil {
		return domain.Task{}, err
	}
	var consumed, active, decided bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM team_members WHERE organization_id=$1 AND child_task_id=$2 AND selected_candidate_id IS NOT NULL),EXISTS(SELECT 1 FROM operations WHERE organization_id=$1 AND task_id=ANY($3::text[]) AND status IN('queued','leased')),EXISTS(SELECT 1 FROM task_decisions WHERE organization_id=$1 AND candidate_id=$4)", p.OrganizationID, taskID, subtreeIDs(tasks, taskID, true), in.CandidateID).Scan(&consumed, &active, &decided); err != nil {
		return domain.Task{}, err
	}
	if consumed || active || decided {
		return domain.Task{}, domain.Err("conflict", "Результат уже использован или выполнение не остановлено", 409)
	}
	candidate, artifact, err := loadCandidateTx(ctx, tx, p.OrganizationID, in.CandidateID)
	if err != nil {
		return domain.Task{}, err
	}
	if candidate.TaskID != taskID || candidate.RunID != task.CurrentRunID || candidate.Source != "model_tools" || artifact.ID != in.ArtifactID || artifact.Version != in.ArtifactVersion || candidate.ContentDigest != in.ContentDigest {
		return domain.Task{}, domain.Err("conflict", "Продолжение относится к другой версии предложений", 409)
	}
	var blocked bool
	if err = tx.QueryRow(ctx, "SELECT status='blocked' FROM operations WHERE organization_id=$1 AND id=$2 AND task_id=$3 AND run_id=$4", p.OrganizationID, candidate.OperationID, taskID, task.CurrentRunID).Scan(&blocked); err != nil {
		return domain.Task{}, rowExists(err)
	}
	if !blocked {
		return domain.Task{}, domain.Err("conflict", "Исходный шаг больше не ожидает продолжения", 409)
	}
	l := domain.Lease{OrganizationID: p.OrganizationID, TaskID: taskID, RunID: task.CurrentRunID, OperationID: candidate.OperationID, Task: task}
	l.Agent, err = scanAgent(tx.QueryRow(ctx, "SELECT "+agentColumns+" FROM agents WHERE organization_id=$1 AND id=$2 AND version=$3", p.OrganizationID, task.AgentID, task.AgentVersion))
	if err != nil {
		return domain.Task{}, err
	}
	l.RunInput, err = loadRunInputTx(ctx, tx, p.OrganizationID, taskID, task.CurrentRunID)
	if err != nil {
		return domain.Task{}, err
	}
	ordinal, err := loadModelContextTx(ctx, tx, task, &l)
	if err != nil {
		return domain.Task{}, err
	}
	if ordinal >= domain.MaxModelTurnsPerTask {
		return domain.Task{}, domain.Err("invalid_transition", "Исчерпан пожизненный предел шагов модели", 409)
	}
	var body modelToolCandidateBody
	d := json.NewDecoder(strings.NewReader(artifact.Content))
	d.DisallowUnknownFields()
	if err = d.Decode(&body); err != nil || len(body.ToolCalls) < 1 || len(body.ToolCalls) > domain.MaxModelToolCalls {
		return domain.Task{}, modelSnapshotConflict()
	}
	rows, err := tx.Query(ctx, "SELECT "+modelProposalColumns+" FROM model_tool_proposals p LEFT JOIN tool_executions e ON e.organization_id=p.organization_id AND e.model_proposal_id=p.id WHERE p.organization_id=$1 AND p.task_id=$2 AND p.candidate_id=$3 ORDER BY p.ordinal", p.OrganizationID, taskID, candidate.ID)
	if err != nil {
		return domain.Task{}, err
	}
	proposals := []domain.ModelToolProposal{}
	for rows.Next() {
		proposal, scanErr := scanModelProposal(rows)
		if scanErr != nil {
			rows.Close()
			return domain.Task{}, scanErr
		}
		proposals = append(proposals, proposal)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return domain.Task{}, err
	}
	if len(proposals) != len(body.ToolCalls) || len(proposals) != len(bindings) {
		return domain.Task{}, domain.Err("conflict", "Продолжение должно включать все предложения текущего пакета", 409)
	}
	messages := append([]domain.ModelMessage(nil), l.ModelMessages...)
	messages = append(messages, domain.ModelMessage{Role: "assistant", Content: body.Text, ToolCalls: body.ToolCalls})
	for i, proposal := range proposals {
		b, exists := bindings[proposal.ID]
		call := body.ToolCalls[i]
		if !exists || call.ID != proposal.CallID || call.Name != proposal.ToolName || call.ArgumentsJSON != proposal.ArgumentsJSON || proposal.OperationID != candidate.OperationID || proposal.ExecutionID != b.ExecutionID {
			return domain.Task{}, domain.Err("conflict", "Пакет результатов не соответствует сохранённым предложениям", 409)
		}
		e, err := loadToolExecutionTx(ctx, tx, p.OrganizationID, taskID, b.ExecutionID, true)
		if err != nil {
			return domain.Task{}, err
		}
		if e.ModelProposalID != proposal.ID || e.CandidateID != candidate.ID || e.OperationID != candidate.OperationID || e.RunID != task.CurrentRunID || e.ToolID != proposal.ToolID || e.ToolVersion != proposal.ToolVersion || e.ToolProfileDigest != proposal.ToolProfileDigest || e.ArgumentsJSON != proposal.ArgumentsJSON || e.Version != b.ExecutionVersion || e.Status != b.Status {
			return domain.Task{}, domain.Err("conflict", "Версия действия или его результат изменились", 409)
		}
		quoted := modelQuotedToolResult{ProposalID: proposal.ID, ExecutionID: e.ID, Status: e.Status, Reason: e.Reason, Untrusted: true}
		if e.Status == "succeeded" || e.Status == "rejected" {
			r, err := scanToolReceipt(tx.QueryRow(ctx, "SELECT "+toolReceiptColumns+" FROM tool_outcome_receipts WHERE organization_id=$1 AND id=$2 AND execution_id=$3", p.OrganizationID, b.ReceiptID, e.ID))
			if err != nil {
				return domain.Task{}, err
			}
			if r.ID != e.ResultReceiptID || r.ContentDigest != b.ContentDigest || r.IntentHash != e.IntentHash || r.Outcome != e.Status {
				return domain.Task{}, domain.Err("conflict", "Квитанция не соответствует результату действия", 409)
			}
			var result string
			if err = tx.QueryRow(ctx, "SELECT content FROM artifacts WHERE organization_id=$1 AND id=$2 AND task_id=$3 AND version=1", p.OrganizationID, r.ArtifactID, taskID).Scan(&result); err != nil {
				return domain.Task{}, rowExists(err)
			}
			if Hash([]byte(result)) != r.ContentDigest {
				return domain.Task{}, modelSnapshotConflict()
			}
			quoted.Receipt, quoted.Result = &r, result
		} else if e.ResultReceiptID != "" || b.ReceiptID != "" || (e.Status != "denied" && e.Status != "expired" && e.Status != "cancelled") {
			return domain.Task{}, domain.Err("invalid_transition", "Неизвестный эффект запрещает продолжение", 409)
		}
		content, err := json.Marshal(quoted)
		if err != nil {
			return domain.Task{}, err
		}
		messages = append(messages, domain.ModelMessage{Role: "tool", ToolCallID: call.ID, Content: string(content)})
	}
	encoded, err := json.Marshal(messages)
	if err != nil || len(encoded) > domain.MaxModelTranscriptBytes {
		return domain.Task{}, domain.Err("invalid_transition", "Точный контекст продолжения превышает допустимый размер", 409)
	}
	profile, err := scanModelConnection(tx.QueryRow(ctx, "SELECT profile,active,created_at FROM model_connections WHERE organization_id=$1 AND id=$2 AND version=$3 FOR SHARE", p.OrganizationID, task.ModelConnectionID, task.ModelConnectionVersion))
	if err != nil {
		return domain.Task{}, err
	}
	if !profile.Active {
		return domain.Task{}, domain.Forbidden
	}
	// Check the exact next wire request before creating durable work. The real
	// dispatch still reserves and rechecks authority/budget independently.
	runtime, err := modelruntime.New(&Store{}, RuntimeModelConfig(profile, "continuation-preflight-only"))
	if err != nil {
		return domain.Task{}, modelSnapshotConflict()
	}
	next := l
	next.OperationID = domain.NewID("op")
	next.OwnerID, next.Token = "continuation-preflight", 1
	next.Task.Status = "running"
	next.ModelMessages = messages
	if _, _, err = runtime.DescribeIntent(next); err != nil {
		return domain.Task{}, domain.Err("invalid_transition", "Точный контекст не помещается в закреплённый профиль модели", 409)
	}
	scopes, err := lockCostScopes(ctx, tx, p.OrganizationID, taskID)
	if err != nil {
		return domain.Task{}, err
	}
	if err = scopes.admit(profile.MaxChargeMicroUSD); err != nil {
		return domain.Task{}, err
	}
	if err = checkModelTurnBudgetTx(ctx, tx, task); err != nil {
		return domain.Task{}, err
	}
	if err = authority(ctx, tx, p); err != nil {
		return domain.Task{}, err
	}
	if automationGrantID != "" {
		g, grantErr := loadAutomationGrantTx(ctx, tx, p.OrganizationID, automationGrantID, false)
		if grantErr != nil {
			return domain.Task{}, grantErr
		}
		if grantErr = allocateAutomationTx(ctx, tx, g, "model", profile.MaxChargeMicroUSD); grantErr != nil {
			return domain.Task{}, grantErr
		}
	}
	if _, err = tx.Exec(ctx, "INSERT INTO model_turns(organization_id,operation_id,task_id,run_id,ordinal,messages_json,messages_digest,source_candidate_id,automation_grant_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''))", p.OrganizationID, next.OperationID, taskID, task.CurrentRunID, ordinal+1, string(encoded), Hash(encoded), candidate.ID, automationGrantID); err != nil {
		return domain.Task{}, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO operations(organization_id,id,task_id,run_id,status,authority_principal_id) VALUES($1,$2,$3,$4,'queued',$5)", p.OrganizationID, next.OperationID, taskID, task.CurrentRunID, p.ID); err != nil {
		return domain.Task{}, err
	}
	if _, err = tx.Exec(ctx, "UPDATE operations SET status='succeeded',lease_owner=NULL,lease_until=NULL WHERE organization_id=$1 AND id=$2 AND status='blocked'", p.OrganizationID, candidate.OperationID); err != nil {
		return domain.Task{}, err
	}
	if _, err = tx.Exec(ctx, "UPDATE runs SET status='queued' WHERE organization_id=$1 AND id=$2", p.OrganizationID, task.CurrentRunID); err != nil {
		return domain.Task{}, err
	}
	task, err = scanTask(tx.QueryRow(ctx, "UPDATE tasks SET status='queued',waiting_reason='',candidate_id=NULL,version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2 RETURNING "+taskColumns, p.OrganizationID, taskID))
	if err != nil {
		return domain.Task{}, err
	}
	return task, emit(ctx, tx, p.OrganizationID, taskID, "model.continuation_queued", map[string]any{"operation_id": next.OperationID, "source_operation_id": candidate.OperationID, "source_candidate_id": candidate.ID, "run_id": task.CurrentRunID, "model_turn": ordinal + 1, "messages_digest": Hash(encoded), "acknowledged_new_cost": true, "prior_liabilities_preserved": true, "reason": in.Reason})
}
