// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/toolruntime"
	"github.com/jackc/pgx/v5"
)

// ClaimModel never retries a previously sent action. A reclaimed uncertain
// attempt is moved to reconciliation; only an un-dispatched intent can proceed.
func (s *Store) ClaimModel(ctx context.Context, worker string, duration time.Duration) (*domain.Lease, domain.ModelConnection, error) {
	var connection domain.ModelConnection
	if !validText(worker, 100) || duration < time.Second || duration > 5*time.Minute {
		return nil, connection, domain.Err("invalid_request", "Некорректный исполнитель или аренда", 400)
	}
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return nil, connection, e
	}
	defer tx.Rollback(ctx)
	l := &domain.Lease{}
	e = tx.QueryRow(ctx, `SELECT o.organization_id,o.id,o.task_id
 FROM operations o JOIN tasks t ON t.organization_id=o.organization_id AND t.id=o.task_id
 WHERE (o.status='queued' OR (o.status='leased' AND o.lease_until<clock_timestamp())) AND t.status IN('queued','running') AND o.run_id=t.current_run_id AND t.model_connection_id IS NOT NULL
 ORDER BY o.created_at,o.id LIMIT 1`).Scan(&l.OrganizationID, &l.OperationID, &l.TaskID)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, connection, nil
	}
	if e != nil {
		return nil, connection, e
	}
	// Selection is only a hint. Structural writers all take current authority,
	// then the permanent tree gate, before operations and accounting scopes.
	// Locking the hinted operation first can deadlock a continuation/cancel that
	// already holds the gate and is waiting for that same operation.
	ownerRevoked := false
	if e = lockModelAutomationAuthorityTx(ctx, tx, l.OrganizationID, l.TaskID, l.OperationID); errors.Is(e, domain.Forbidden) {
		ownerRevoked = true
	} else if e != nil {
		return nil, connection, e
	}
	if _, e = lockTaskTreeTx(ctx, tx, l.OrganizationID, l.TaskID); e != nil {
		return nil, connection, e
	}
	var attempts, maxAttempts int
	e = tx.QueryRow(ctx, `SELECT o.run_id,o.fencing_token,o.attempts,o.max_attempts
 FROM operations o JOIN tasks t ON t.organization_id=o.organization_id AND t.id=o.task_id
 WHERE o.organization_id=$1 AND o.id=$2 AND o.task_id=$3
 AND (o.status='queued' OR (o.status='leased' AND o.lease_until<clock_timestamp()))
 AND t.status IN('queued','running') AND o.run_id=t.current_run_id AND t.model_connection_id IS NOT NULL
 FOR UPDATE OF o`, l.OrganizationID, l.OperationID, l.TaskID).Scan(&l.RunID, &l.Token, &attempts, &maxAttempts)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, connection, nil
	}
	if e != nil {
		return nil, connection, e
	}
	scopes, e := lockCostScopes(ctx, tx, l.OrganizationID, l.TaskID)
	if e != nil {
		return nil, connection, e
	}
	l.Task = scopes.tasks[len(scopes.tasks)-1]
	l.RunInput, e = loadRunInputTx(ctx, tx, l.OrganizationID, l.TaskID, l.RunID)
	if e != nil {
		return nil, connection, e
	}
	// Any possibly dispatched attempt remains charged/held and cannot be reset
	// by an expired lease, model reconfiguration, cancellation or fresh worker.
	var uncertain bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reservations r JOIN actions a ON a.organization_id=r.organization_id AND a.id=r.action_id
 WHERE r.organization_id=$1 AND r.operation_id=$2 AND r.category='model' AND (a.status<>'pending' OR r.status<>'reserved'))`, l.OrganizationID, l.OperationID).Scan(&uncertain); e != nil {
		return nil, connection, e
	}
	reason := ""
	if !scopes.now.Before(l.Task.ExecutionDeadline) {
		reason = "execution_deadline"
	}
	if uncertain {
		reason = "model_outcome_unknown"
	} else if ownerRevoked {
		reason = "authorization_revoked"
	} else if e = authorizeTaskOwner(ctx, tx, l.OrganizationID, l.Task.OwnerID); e != nil {
		if !errors.Is(e, domain.Forbidden) {
			return nil, connection, e
		}
		reason = "authorization_revoked"
	}
	if reason == "" && !scopes.active {
		reason = "authorization_revoked"
	}
	if reason == "" {
		if e = checkBoundAutomationModelTx(ctx, tx, l.Task, l.OperationID); e != nil {
			if !errors.Is(e, domain.Forbidden) {
				return nil, connection, e
			}
			reason = "automation_authorization_revoked"
		}
	}
	if reason == "" && attempts >= maxAttempts {
		reason = "attempt_limit"
	}
	if reason == "" {
		if e = scopes.admit(0); e != nil {
			reason = "budget_exceeded"
		}
	}
	if reason == "" {
		connection, e = scanModelConnection(tx.QueryRow(ctx, "SELECT profile,active,created_at FROM model_connections WHERE organization_id=$1 AND id=$2 AND version=$3 FOR SHARE", l.OrganizationID, l.Task.ModelConnectionID, l.Task.ModelConnectionVersion))
		if e != nil {
			return nil, connection, e
		}
		if !connection.Active {
			reason = "model_profile_unavailable"
		}
	}
	if reason == "" {
		if e = checkConfigurationResourceAdmissionTx(ctx, tx, l.OrganizationID, "model", connection.ID, connection.Version); e != nil {
			if !errors.Is(e, domain.Forbidden) {
				return nil, connection, e
			}
			reason = "configuration_profile_unavailable"
		}
	}
	if reason == "" {
		probe, probeErr := loadConfigurationVerificationForModelTx(ctx, tx, l.OrganizationID, l.TaskID)
		if probeErr == nil {
			l.ConfigurationVerificationID = probe.ID
			if e = checkConfigurationModelProbeAuthorityTx(ctx, tx, l.Task, l.OperationID); e != nil {
				if !errors.Is(e, domain.Forbidden) {
					return nil, connection, e
				}
				reason = "configuration_authorization_revoked"
			}
		} else if !errors.Is(probeErr, domain.NotFound) {
			return nil, connection, probeErr
		}
	}
	if reason != "" {
		if e = block(ctx, tx, l, reason); e != nil {
			return nil, connection, e
		}
		// A sent/unknown action remains reserved. Only its classification changes.
		if uncertain {
			if e = recoverModelUnknownTx(ctx, tx, l); e != nil {
				return nil, connection, e
			}
		}
		return nil, connection, tx.Commit(ctx)
	}
	l.Agent, e = scanAgent(tx.QueryRow(ctx, "SELECT "+agentColumns+" FROM agents WHERE organization_id=$1 AND id=$2 AND version=$3", l.OrganizationID, l.Task.AgentID, l.Task.AgentVersion))
	if e != nil {
		return nil, connection, e
	}
	if l.Agent.Model != connection.Model || !l.Agent.Published {
		return nil, connection, domain.Err("conflict", "Агент не соответствует профилю выполнения", 409)
	}
	if _, e = loadModelContextTx(ctx, tx, l.Task, l); e != nil {
		if errors.Is(e, domain.Forbidden) {
			if e = block(ctx, tx, l, "model_tool_profile_unavailable"); e != nil {
				return nil, connection, e
			}
			return nil, connection, tx.Commit(ctx)
		}
		return nil, connection, e
	}
	if e = checkBoundAutomationModelTx(ctx, tx, l.Task, l.OperationID); e != nil {
		if !errors.Is(e, domain.Forbidden) {
			return nil, connection, e
		}
		if e = block(ctx, tx, l, "automation_authorization_revoked"); e != nil {
			return nil, connection, e
		}
		return nil, connection, tx.Commit(ctx)
	}
	l.OwnerID = worker
	l.Token++
	if _, e = tx.Exec(ctx, "UPDATE operations SET status='leased',lease_owner=$3,lease_until=clock_timestamp()+$4::interval,fencing_token=$5,attempts=attempts+1 WHERE organization_id=$1 AND id=$2", l.OrganizationID, l.OperationID, worker, duration.String(), l.Token); e != nil {
		return nil, connection, e
	}
	l.Task, e = scanTask(tx.QueryRow(ctx, "UPDATE tasks SET status='running',waiting_reason='',version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2 RETURNING "+taskColumns, l.OrganizationID, l.TaskID))
	if e != nil {
		return nil, connection, e
	}
	if _, e = tx.Exec(ctx, "UPDATE runs SET status='running' WHERE organization_id=$1 AND id=$2", l.OrganizationID, l.RunID); e != nil {
		return nil, connection, e
	}
	if e = emit(ctx, tx, l.OrganizationID, l.TaskID, "operation.claimed", map[string]any{"operation_id": l.OperationID, "fencing_token": l.Token, "attempt": attempts + 1, "adapter": "model", "model_connection_id": connection.ID, "model_connection_version": connection.Version}); e != nil {
		return nil, connection, e
	}
	return l, connection, tx.Commit(ctx)
}

func (s *Store) RenewModelLease(ctx context.Context, l *domain.Lease, duration time.Duration) error {
	if duration < time.Second || duration > 5*time.Minute {
		return domain.Err("invalid_request", "Некорректная аренда", 400)
	}
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if e = lockModelAutomationAuthorityTx(ctx, tx, l.OrganizationID, l.TaskID, l.OperationID); e != nil {
		return e
	}
	if e = checkLeaseTx(ctx, tx, l); e != nil {
		return e
	}
	scopes, e := lockCostScopes(ctx, tx, l.OrganizationID, l.TaskID)
	if e != nil {
		return e
	}
	task := scopes.tasks[len(scopes.tasks)-1]
	if !scopes.active || task.Status != "running" {
		return domain.Forbidden
	}
	if e = checkTaskModelAdmissionTx(ctx, tx, task, l, nil); e != nil {
		return e
	}
	if e = checkBoundAutomationModelTx(ctx, tx, task, l.OperationID); e != nil {
		return e
	}
	result, e := tx.Exec(ctx, "UPDATE operations SET lease_until=clock_timestamp()+$5::interval WHERE organization_id=$1 AND id=$2 AND lease_owner=$3 AND fencing_token=$4 AND status='leased' AND lease_until>clock_timestamp()", l.OrganizationID, l.OperationID, l.OwnerID, l.Token, duration.String())
	if e != nil {
		return e
	}
	if result.RowsAffected() != 1 {
		return domain.Err("stale_lease", "Аренда истекла", 409)
	}
	return tx.Commit(ctx)
}

func (s *Store) CompleteModelCandidate(ctx context.Context, l *domain.Lease, reservationID string, c domain.ModelCandidate) (domain.Artifact, error) {
	var empty domain.Artifact
	if !utf8.ValidString(c.ProviderRequestID) || len(c.ProviderRequestID) > 256 || strings.ContainsRune(c.ProviderRequestID, 0) || !validText(c.Model, 256) || !validText(c.PriceVersion, 128) || !validDigest(c.IntentDigest) || c.InputTokens < 0 || c.OutputTokens < 0 || c.EstimatedMicroUSD < 0 || c.EstimatedMicroUSD > domain.MaxMoney {
		return empty, domain.Err("invalid_request", "Некорректный кандидат результата", 400)
	}
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return empty, e
	}
	defer tx.Rollback(ctx)
	if e = scrubConfigurationProbeCandidateTx(ctx, tx, l.OrganizationID, l.TaskID, &c); e != nil {
		return empty, e
	}
	content, mediaType, e := modelCandidateContent(c)
	if e != nil {
		return empty, e
	}
	if e = lockOperationAuthorityTx(ctx, tx, l.OrganizationID, l.TaskID, l.OperationID); e != nil {
		return empty, e
	}
	if e = checkLeaseTx(ctx, tx, l); e != nil {
		return empty, e
	}
	scopes, e := lockCostScopes(ctx, tx, l.OrganizationID, l.TaskID)
	if e != nil {
		return empty, e
	}
	t := scopes.tasks[len(scopes.tasks)-1]
	if !scopes.active || t.Status != "running" {
		return empty, domain.Forbidden
	}
	if e = checkTaskModelAdmissionTx(ctx, tx, t, l, nil); e != nil {
		return empty, e
	}
	if t.ModelConnectionID == "" {
		return empty, domain.Forbidden
	}
	policy, e := scanModelConnection(tx.QueryRow(ctx, "SELECT profile,active,created_at FROM model_connections WHERE organization_id=$1 AND id=$2 AND version=$3 FOR SHARE", t.OrganizationID, t.ModelConnectionID, t.ModelConnectionVersion))
	if e != nil {
		return empty, e
	}
	estimate, e := RuntimeModelConfig(policy, "candidate-validation-only").Price.Estimate(c.InputTokens, c.OutputTokens)
	if e != nil || estimate != c.EstimatedMicroUSD || len(content) > policy.MaxOutputBytes || c.InputTokens > policy.MaxInputTokens || c.OutputTokens > int64(policy.MaxOutputTokens) || c.EstimatedMicroUSD > policy.MaxChargeMicroUSD {
		return empty, domain.Err("invalid_request", "Кандидат превышает закреплённые ограничения", 400)
	}
	a, e := scanAgent(tx.QueryRow(ctx, "SELECT "+agentColumns+" FROM agents WHERE organization_id=$1 AND id=$2 AND version=$3", l.OrganizationID, t.AgentID, t.AgentVersion))
	if e != nil {
		return empty, e
	}
	snapshot := *l
	snapshot.Task, snapshot.Agent = t, a
	snapshot.RunInput, e = loadRunInputTx(ctx, tx, t.OrganizationID, t.ID, l.RunID)
	if e != nil {
		return empty, e
	}
	if _, e = loadModelContextTx(ctx, tx, t, &snapshot); e != nil {
		return empty, e
	}
	byName := make(map[string]domain.ModelToolDefinition, len(snapshot.ModelTools))
	for _, def := range snapshot.ModelTools {
		byName[def.Name] = def
	}
	seenCalls := map[string]bool{}
	for _, message := range snapshot.ModelMessages {
		for _, call := range message.ToolCalls {
			seenCalls[call.ID] = true
		}
	}
	for _, call := range c.ToolCalls {
		def, exists := byName[call.Name]
		if !exists || !modelWireName.MatchString(call.ID) || seenCalls[call.ID] {
			return empty, domain.Err("invalid_request", "Предложение модели не соответствует закреплённым инструментам", 400)
		}
		seenCalls[call.ID] = true
		connection, e := loadToolConnectionTx(ctx, tx, t.OrganizationID, def.ToolID, def.ToolVersion, true)
		if e != nil {
			return empty, e
		}
		if _, e = toolruntime.CanonicalArguments(connection, call.ArgumentsJSON); e != nil {
			return empty, domain.Err("invalid_request", "Аргументы предложения модели не соответствуют инструменту", 400)
		}
	}
	r, e := scanReservation(tx.QueryRow(ctx, "SELECT "+reservationColumns+" FROM reservations WHERE organization_id=$1 AND id=$2 FOR UPDATE", l.OrganizationID, reservationID))
	if e != nil {
		return empty, e
	}
	if r.TaskID != l.TaskID || r.OperationID != l.OperationID || r.Category != "model" || r.ActionID == "" || r.PriceVersion != c.PriceVersion || a.Model != c.Model || r.Status != "unknown" || r.PendingQuarantineID != "" {
		return empty, domain.Err("conflict", "Кандидат не соответствует платной попытке", 409)
	}
	var intent, status string
	if e = tx.QueryRow(ctx, "SELECT intent_hash,status FROM actions WHERE organization_id=$1 AND id=$2 FOR UPDATE", l.OrganizationID, r.ActionID).Scan(&intent, &status); e != nil {
		return empty, e
	}
	if intent != c.IntentDigest || status != "unknown" {
		return empty, domain.Err("conflict", "Намерение действия изменилось", 409)
	}
	// Recheck expiry after all lock waits before publishing anything.
	if e = checkLeaseTx(ctx, tx, l); e != nil {
		return empty, e
	}
	name, source, waitingReason := "model-candidate.txt", "model", "model_candidate_pending"
	if len(c.ToolCalls) > 0 {
		name, source, waitingReason = "model-tool-proposals.json", "model_tools", "model_tools_pending"
	}
	artifact := domain.Artifact{ID: domain.NewID("art"), OrganizationID: l.OrganizationID, TaskID: l.TaskID, Name: name, MediaType: mediaType, Content: content, Version: 1, Provenance: map[string]any{"adapter": "model", "verified": false, "verification": "awaiting_human", "operation_id": l.OperationID, "reservation_id": r.ID, "intent_hash": intent, "content_digest": Hash([]byte(content)), "model": c.Model, "price_version": c.PriceVersion, "provider_request_id": c.ProviderRequestID, "input_tokens": c.InputTokens, "output_tokens": c.OutputTokens, "estimated_microusd": strconv.FormatInt(c.EstimatedMicroUSD, 10), "cost_status": "unknown", "model_accounting_mode": domain.EffectiveModelAccountingMode(policy.AccountingMode), "model_connection_id": t.ModelConnectionID, "model_connection_version": t.ModelConnectionVersion, "proposal_count": len(c.ToolCalls)}}
	provenance, _ := json.Marshal(artifact.Provenance)
	if e = tx.QueryRow(ctx, "INSERT INTO artifacts(organization_id,id,task_id,name,media_type,content,provenance) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING created_at", artifact.OrganizationID, artifact.ID, artifact.TaskID, artifact.Name, artifact.MediaType, artifact.Content, provenance).Scan(&artifact.CreatedAt); e != nil {
		return empty, e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO model_candidates(organization_id,operation_id,task_id,reservation_id,artifact_id,content_digest,intent_hash) VALUES($1,$2,$3,$4,$5,$6,$7)", l.OrganizationID, l.OperationID, l.TaskID, r.ID, artifact.ID, Hash([]byte(content)), intent); e != nil {
		return empty, e
	}
	candidate, e := RegisterCandidateTx(ctx, tx, t, l.RunID, l.OperationID, artifact, source)
	if e != nil {
		return empty, e
	}
	for i, call := range c.ToolCalls {
		def := byName[call.Name]
		if _, e = tx.Exec(ctx, `INSERT INTO model_tool_proposals(organization_id,id,task_id,run_id,operation_id,candidate_id,artifact_id,artifact_version,content_digest,call_id,tool_id,tool_version,tool_profile_digest,tool_name,arguments_json,arguments_digest,ordinal)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, l.OrganizationID, domain.NewID("mtp"), l.TaskID, l.RunID, l.OperationID, candidate.ID, artifact.ID, artifact.Version, candidate.ContentDigest, call.ID, def.ToolID, def.ToolVersion, def.ToolProfileDigest, call.Name, call.ArgumentsJSON, Hash([]byte(call.ArgumentsJSON)), i+1); e != nil {
			return empty, e
		}
	}
	if _, e = tx.Exec(ctx, "UPDATE tasks SET status='waiting_input',waiting_reason=$3,version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2", l.OrganizationID, l.TaskID, waitingReason); e != nil {
		return empty, e
	}
	if _, e = tx.Exec(ctx, "UPDATE runs SET status='waiting_input' WHERE organization_id=$1 AND id=$2", l.OrganizationID, l.RunID); e != nil {
		return empty, e
	}
	result, e := tx.Exec(ctx, "UPDATE operations SET status='blocked',lease_owner=NULL,lease_until=NULL WHERE organization_id=$1 AND id=$2 AND status='leased' AND lease_owner=$3 AND fencing_token=$4 AND lease_until>clock_timestamp()", l.OrganizationID, l.OperationID, l.OwnerID, l.Token)
	if e != nil {
		return empty, e
	}
	if result.RowsAffected() != 1 {
		return empty, domain.Err("stale_lease", "Аренда истекла до публикации", 409)
	}
	if e = emit(ctx, tx, l.OrganizationID, l.TaskID, "model.candidate_created", map[string]any{"artifact_id": artifact.ID, "content_digest": Hash([]byte(content)), "operation_id": l.OperationID, "reservation_id": r.ID, "verified": false, "cost_status": "unknown", "source": source, "proposal_count": len(c.ToolCalls)}); e != nil {
		return empty, e
	}
	return artifact, tx.Commit(ctx)
}

func (s *Store) PauseModelAttempt(ctx context.Context, l *domain.Lease, reservationID, reason string) error {
	switch reason {
	case "model_outcome_unknown", "model_output_rejected", "model_input_rejected", "model_accounting_pending", "model_credentials_unavailable", "model_profile_unavailable":
	default:
		return domain.Err("invalid_request", "Неизвестная причина паузы", 400)
	}
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if e = lockOperationAuthorityTx(ctx, tx, l.OrganizationID, l.TaskID, l.OperationID); e != nil {
		return e
	}
	if e = checkLeaseTx(ctx, tx, l); e != nil {
		return e
	}
	scopes, e := lockCostScopes(ctx, tx, l.OrganizationID, l.TaskID)
	if e != nil {
		return e
	}
	if !scopes.active || scopes.tasks[len(scopes.tasks)-1].Status != "running" {
		return domain.Forbidden
	}
	if reservationID != "" {
		r, e := scanReservation(tx.QueryRow(ctx, "SELECT "+reservationColumns+" FROM reservations WHERE organization_id=$1 AND id=$2 FOR UPDATE", l.OrganizationID, reservationID))
		if e != nil {
			return e
		}
		if r.TaskID != l.TaskID || r.OperationID != l.OperationID {
			return domain.NotFound
		}
	}
	if e = checkLeaseTx(ctx, tx, l); e != nil {
		return e
	}
	if e = block(ctx, tx, l, reason); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

func recoverModelUnknownTx(ctx context.Context, tx pgx.Tx, l *domain.Lease) error {
	return markAbandonedModelUnknownTx(ctx, tx, l, "lease_lost_after_dispatch")
}

func markAbandonedModelUnknownTx(ctx context.Context, tx pgx.Tx, l *domain.Lease, reason string) error {
	rows, e := tx.Query(ctx, "SELECT "+reservationColumns+" FROM reservations WHERE organization_id=$1 AND operation_id=$2 AND status='reserved' AND category='model' AND pending_quarantine_id IS NULL ORDER BY id FOR UPDATE", l.OrganizationID, l.OperationID)
	if e != nil {
		return e
	}
	reservations := []domain.Reservation{}
	for rows.Next() {
		r, e := scanReservation(rows)
		if e != nil {
			rows.Close()
			return e
		}
		reservations = append(reservations, r)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, r := range reservations {
		if _, e = tx.Exec(ctx, "UPDATE reservations SET status='unknown',revision=revision+1 WHERE organization_id=$1 AND id=$2", r.OrganizationID, r.ID); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "UPDATE actions SET status='unknown' WHERE organization_id=$1 AND id=$2 AND status='sent'", r.OrganizationID, r.ActionID); e != nil {
			return e
		}
		if e = recordLedger(ctx, tx, r, "kernel:"+reason, digestValue(reason), "unknown", 0, 0); e != nil {
			return e
		}
		if e = emit(ctx, tx, r.OrganizationID, r.TaskID, "cost.unknown", map[string]any{"reservation_id": r.ID, "reason": reason}); e != nil {
			return e
		}
	}
	return nil
}

// Only the locked pending action proves that this terminal attempt never sent.
// Expiry alone or an unknown sent action never frees its reservation.
func (s *Store) ReconcileModelUnsent(ctx context.Context) (bool, error) {
	if err := s.CheckRuntimeAdmission(ctx); err != nil {
		return false, err
	}
	var org, id string
	e := s.Pool.QueryRow(ctx, `SELECT r.organization_id,r.id FROM reservations r JOIN actions a ON a.organization_id=r.organization_id AND a.id=r.action_id
 JOIN tasks t ON t.organization_id=r.organization_id AND t.id=r.task_id JOIN operations o ON o.organization_id=r.organization_id AND o.id=r.operation_id
 WHERE r.category='model' AND r.status IN('reserved','unknown') AND r.pending_quarantine_id IS NULL AND a.status='pending'
 AND (t.status IN('cancelled','blocked') OR o.run_id<>t.current_run_id) AND o.status IN('cancelled','blocked','failed') ORDER BY r.created_at,r.id LIMIT 1`).Scan(&org, &id)
	if errors.Is(e, pgx.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	_, e = s.ReleaseUnsentCost(ctx, org, id, "kernel:terminal-unsent")
	var typed *domain.Error
	if errors.As(e, &typed) && typed.Code == "conflict" {
		return false, nil
	}
	return e == nil, e
}

// PauseModelAfterRuntimeError is trusted worker cleanup, not execution
// authorization. It inspects the original fence and durable marker even after
// membership loss. An error/timeout never proves that dispatch did not commit.
func (s *Store) PauseModelAfterRuntimeError(ctx context.Context, l *domain.Lease, reason string) error {
	if l == nil || l.Token < 1 {
		return domain.Forbidden
	}
	switch reason {
	case "model_attempt_requires_attention", "model_credentials_unavailable", "model_profile_unavailable":
	default:
		return domain.Forbidden
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = lockTaskTreeTx(ctx, tx, l.OrganizationID, l.TaskID); err != nil {
		return err
	}
	scopes, err := lockCostScopes(ctx, tx, l.OrganizationID, l.TaskID)
	if err != nil {
		return err
	}
	task := scopes.tasks[len(scopes.tasks)-1]
	// A successfully published observation, cancellation, newer Run or new fence
	// cannot be changed by a delayed cleanup callback from this attempt.
	if task.CurrentRunID != l.RunID || task.Status != "running" {
		return tx.Commit(ctx)
	}
	var current bool
	if err = tx.QueryRow(ctx, "SELECT status='leased' AND lease_owner=$3 AND fencing_token=$4 AND run_id=$5 AND task_id=$6 FROM operations WHERE organization_id=$1 AND id=$2", l.OrganizationID, l.OperationID, l.OwnerID, l.Token, l.RunID, l.TaskID).Scan(&current); err != nil {
		return rowExists(err)
	}
	if !current {
		return domain.Err("stale_lease", "Model attempt no longer belongs to this worker", 409)
	}
	var marked bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM reservations r JOIN actions a ON a.organization_id=r.organization_id AND a.id=r.action_id WHERE r.organization_id=$1 AND r.operation_id=$2 AND r.category='model' AND a.dispatch_issued_at IS NOT NULL)", l.OrganizationID, l.OperationID).Scan(&marked); err != nil {
		return err
	}
	if marked {
		reason = "model_outcome_unknown"
		if err = markAbandonedModelUnknownTx(ctx, tx, l, "runtime_cleanup_after_dispatch"); err != nil {
			return err
		}
	}
	if err = block(ctx, tx, l, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
