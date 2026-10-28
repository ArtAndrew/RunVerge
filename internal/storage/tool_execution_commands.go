// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"regexp"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/toolruntime"
	"github.com/jackc/pgx/v5"
)

var occurrencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func toolGuard(taskID, kind string) func(context.Context, pgx.Tx, domain.Principal) error {
	return func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		if kind == "approve" || kind == "reconcile" {
			var found string
			if err := tx.QueryRow(ctx, "SELECT id FROM tasks WHERE organization_id=$1 AND id=$2", p.OrganizationID, taskID).Scan(&found); err != nil {
				return rowExists(err)
			}
			permission := "actions.approve_any"
			if kind == "reconcile" {
				permission = "actions.reconcile_any"
			}
			if !domain.PrincipalHasPermission(p, permission) {
				return domain.Forbidden
			}
			return nil
		}
		return taskMutationPermission(ctx, tx, p, taskID, "actions."+kind+"_own", "actions."+kind+"_any")
	}
}
func lockToolTaskTx(ctx context.Context, tx pgx.Tx, org, taskID string) (domain.Task, []domain.Task, error) {
	var empty domain.Task
	if err := lockAdmissionOwnerTx(ctx, tx, org, taskID); err != nil {
		return empty, nil, err
	}
	c, err := lockTaskTreeTx(ctx, tx, org, taskID)
	if err != nil {
		return empty, nil, err
	}
	tasks, err := lockTreeTasksTx(ctx, tx, org, c.RootID)
	if err != nil {
		return empty, nil, err
	}
	t, err := findTreeTask(tasks, taskID)
	return t, tasks, err
}

func lockToolObservationTaskTx(ctx context.Context, tx pgx.Tx, org, taskID string) (domain.Task, []domain.Task, error) {
	var empty domain.Task
	c, err := lockTaskTreeTx(ctx, tx, org, taskID)
	if err != nil {
		return empty, nil, err
	}
	tasks, err := lockTreeTasksTx(ctx, tx, org, c.RootID)
	if err != nil {
		return empty, nil, err
	}
	t, err := findTreeTask(tasks, taskID)
	return t, tasks, err
}

func (s *Store) PrepareToolExecution(ctx context.Context, p domain.Principal, taskID, key string, in domain.PrepareToolExecution) (json.RawMessage, error) {
	if err := validatePrepareToolExecution(in); err != nil {
		return nil, err
	}
	return s.MutateGuarded(ctx, p, "POST /tasks/"+taskID+"/tool-executions", key, in, toolGuard(taskID, "prepare"), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		e, err := prepareToolExecutionTx(ctx, tx, p, taskID, in, currentActorTx, "")
		if err != nil {
			return nil, err
		}
		return toolViewTx(ctx, tx, p, e)
	})
}

func validatePrepareToolExecution(in domain.PrepareToolExecution) error {
	if !occurrencePattern.MatchString(in.OccurrenceID) || in.ExpectedTaskVersion < 1 || !validText(in.CandidateID, 128) || !validText(in.ArtifactID, 128) || in.ArtifactVersion < 1 || !validDigest(in.ContentDigest) || !validText(in.ToolID, 128) || in.ToolVersion < 1 || in.ExpiresInSeconds < 1 || in.ExpiresInSeconds > 86400 {
		return domain.Err("invalid_request", "Нужны точные версии результата, инструмента и срок нового действия", 400)
	}
	return nil
}

func prepareToolExecutionTx(ctx context.Context, tx pgx.Tx, p domain.Principal, taskID string, in domain.PrepareToolExecution, authority commandAuthority, automationGrantID string) (domain.ToolExecution, error) {
	if authority == nil {
		return domain.ToolExecution{}, domain.Forbidden
	}
	if err := validatePrepareToolExecution(in); err != nil {
		return domain.ToolExecution{}, err
	}
	t, tasks, err := lockToolTaskTx(ctx, tx, p.OrganizationID, taskID)
	if err != nil {
		return domain.ToolExecution{}, err
	}
	if t.Version != in.ExpectedTaskVersion {
		return domain.ToolExecution{}, domain.Err("conflict", "Задача изменилась. Подготовьте действие заново", 409)
	}
	if t.Status != "waiting_input" || (!candidateWaitingReason(t.WaitingReason) && t.WaitingReason != "model_tools_pending") || t.CandidateID != in.CandidateID {
		return domain.ToolExecution{}, domain.Err("invalid_transition", "Действие можно подготовить только для текущего результата, ожидающего решения", 409)
	}
	if err = noActiveToolExecutionsTx(ctx, tx, p.OrganizationID, subtreeIDs(tasks, taskID, true)); err != nil {
		return domain.ToolExecution{}, err
	}
	var consumed, active, decided bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM team_members WHERE organization_id=$1 AND child_task_id=$2 AND selected_candidate_id IS NOT NULL),EXISTS(SELECT 1 FROM operations WHERE organization_id=$1 AND task_id=ANY($3::text[]) AND status IN('queued','leased')),EXISTS(SELECT 1 FROM task_decisions WHERE organization_id=$1 AND candidate_id=$4)", p.OrganizationID, taskID, subtreeIDs(tasks, taskID, true), in.CandidateID).Scan(&consumed, &active, &decided); err != nil {
		return domain.ToolExecution{}, err
	}
	if consumed || active || decided {
		return domain.ToolExecution{}, domain.Err("conflict", "Исходный результат уже используется или выполнение ещё не остановлено", 409)
	}
	candidate, artifact, err := loadCandidateTx(ctx, tx, p.OrganizationID, in.CandidateID)
	if err != nil {
		return domain.ToolExecution{}, err
	}
	if candidate.TaskID != taskID || candidate.RunID != t.CurrentRunID || artifact.ID != in.ArtifactID || artifact.Version != in.ArtifactVersion || candidate.ContentDigest != in.ContentDigest {
		return domain.ToolExecution{}, domain.Err("conflict", "Действие относится к другой версии результата", 409)
	}
	var blocked bool
	if err = tx.QueryRow(ctx, "SELECT status='blocked' FROM operations WHERE organization_id=$1 AND id=$2 AND task_id=$3 AND run_id=$4", p.OrganizationID, candidate.OperationID, taskID, t.CurrentRunID).Scan(&blocked); err != nil {
		return domain.ToolExecution{}, rowExists(err)
	}
	if !blocked {
		return domain.ToolExecution{}, domain.Err("conflict", "Исходная операция больше не ожидает решения", 409)
	}
	connection, err := loadToolConnectionTx(ctx, tx, p.OrganizationID, in.ToolID, in.ToolVersion, true)
	if err != nil {
		return domain.ToolExecution{}, err
	}
	if !connection.Tool.Active {
		return domain.ToolExecution{}, domain.Err("conflict", "Версия инструмента отключена", 409)
	}
	arguments, err := toolruntime.CanonicalArguments(connection, in.ArgumentsJSON)
	if err != nil {
		return domain.ToolExecution{}, domain.Err("invalid_request", "Аргументы не соответствуют установленному инструменту", 400)
	}
	if err = bindPreparedModelProposalTx(ctx, tx, p.OrganizationID, candidate, in, arguments, connection); err != nil {
		return domain.ToolExecution{}, err
	}
	var own, tree int
	var duplicate bool
	if err = tx.QueryRow(ctx, "SELECT COALESCE((SELECT own_executions FROM tool_execution_counts WHERE organization_id=$1 AND task_id=$2),0),COALESCE((SELECT tree_executions FROM tool_execution_counts WHERE organization_id=$1 AND task_id=$3),0),EXISTS(SELECT 1 FROM tool_executions WHERE organization_id=$1 AND task_id=$2 AND intent->>'occurrence_id'=$4)", p.OrganizationID, taskID, t.RootID, in.OccurrenceID).Scan(&own, &tree, &duplicate); err != nil {
		return domain.ToolExecution{}, err
	}
	if duplicate {
		return domain.ToolExecution{}, domain.Err("conflict", "Идентификатор этого действия уже использован. Откройте существующее действие", 409)
	}
	if own >= domain.MaxToolExecutionsPerTask || tree >= domain.MaxToolExecutionsPerRoot {
		return domain.ToolExecution{}, domain.Err("invalid_transition", "Исчерпан пожизненный предел внешних действий дерева", 409)
	}
	scopes, err := lockCostScopes(ctx, tx, p.OrganizationID, taskID)
	if err != nil {
		return domain.ToolExecution{}, err
	}
	if err = scopes.admit(connection.Tool.MaxChargeMicroUSD); err != nil {
		return domain.ToolExecution{}, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return domain.ToolExecution{}, err
	}
	now = now.UTC()
	expires := now.Add(time.Duration(in.ExpiresInSeconds) * time.Second)
	if t.ExecutionDeadline.Before(expires) {
		expires = t.ExecutionDeadline.UTC()
	}
	if !now.Before(expires) {
		return domain.ToolExecution{}, domain.Err("invalid_transition", "Истёк срок выполнения задачи", 409)
	}
	e := domain.ToolExecution{ID: domain.NewID("tex"), OccurrenceID: in.OccurrenceID, OrganizationID: p.OrganizationID, TaskID: taskID, RunID: t.CurrentRunID, OperationID: candidate.OperationID, ActionID: domain.NewID("act"), ReservationID: domain.NewID("res"), ApprovalID: domain.NewID("apr"), ToolID: in.ToolID, ToolVersion: in.ToolVersion, ToolProfileDigest: domain.ToolConnectionDigest(connection), CandidateID: candidate.ID, ArtifactID: artifact.ID, ArtifactVersion: artifact.Version, ContentDigest: candidate.ContentDigest, SourceTaskVersion: t.Version, ArgumentsJSON: arguments, ArgumentsDigest: Hash([]byte(arguments)), MaxChargeMicroUSD: connection.Tool.MaxChargeMicroUSD, Provider: connection.Tool.Provider, PriceVersion: connection.Tool.PriceVersion, CreatedBy: p.ID, Status: "awaiting_approval", Version: 1, ExpiresAt: expires}
	e.ModelProposalID = in.ModelProposalID
	e.AutomationGrantID = automationGrantID
	if automationGrantID != "" {
		g, grantErr := loadAutomationGrantTx(ctx, tx, p.OrganizationID, automationGrantID, false)
		if grantErr != nil {
			return domain.ToolExecution{}, grantErr
		}
		if grantErr = automationToolEligibility(g, connection, false); grantErr != nil {
			return domain.ToolExecution{}, grantErr
		}
		if g.ExpiresAt.Before(e.ExpiresAt) {
			e.ExpiresAt = g.ExpiresAt
		}
		if grantErr = allocateAutomationTx(ctx, tx, g, "tool", connection.Tool.MaxChargeMicroUSD); grantErr != nil {
			return domain.ToolExecution{}, grantErr
		}
	}
	e.IntentHash = domain.ToolIntentDigest(e)
	if err = authority(ctx, tx, p); err != nil {
		return domain.ToolExecution{}, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO actions(organization_id,id,task_id,operation_id,intent_hash,idempotency_key,status) VALUES($1,$2,$3,$4,$5,$2,'pending')", p.OrganizationID, e.ActionID, taskID, e.OperationID, e.IntentHash); err != nil {
		return domain.ToolExecution{}, err
	}
	r, err := scanReservation(tx.QueryRow(ctx, "INSERT INTO reservations(organization_id,id,task_id,operation_id,action_id,root_id,scope_task_ids,charge_key,category,provider,price_version,amount_microusd,status) VALUES($1,$2,$3,$4,$5,$6,$7,$5,'tool',$8,$9,$10,'reserved') RETURNING "+reservationColumns, p.OrganizationID, e.ReservationID, taskID, e.OperationID, e.ActionID, t.RootID, scopes.ids(), e.Provider, e.PriceVersion, e.MaxChargeMicroUSD))
	if err != nil {
		return domain.ToolExecution{}, err
	}
	if err = applyCostDelta(ctx, tx, &scopes, r.AmountMicroUSD, 0, false); err != nil {
		return domain.ToolExecution{}, err
	}
	if err = recordLedger(ctx, tx, r, "reserve", e.IntentHash, "reserve", r.AmountMicroUSD, 0); err != nil {
		return domain.ToolExecution{}, err
	}
	intent, _ := json.Marshal(e)
	e, err = scanToolExecution(tx.QueryRow(ctx, `INSERT INTO tool_executions(organization_id,id,task_id,run_id,operation_id,action_id,reservation_id,approval_id,tool_id,tool_version,tool_profile_digest,candidate_id,artifact_id,artifact_version,content_digest,intent_hash,intent,created_by,status,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,'awaiting_approval',$19) RETURNING `+toolExecutionColumns, e.OrganizationID, e.ID, e.TaskID, e.RunID, e.OperationID, e.ActionID, e.ReservationID, e.ApprovalID, e.ToolID, e.ToolVersion, e.ToolProfileDigest, e.CandidateID, e.ArtifactID, e.ArtifactVersion, e.ContentDigest, e.IntentHash, intent, e.CreatedBy, e.ExpiresAt))
	if err != nil {
		return domain.ToolExecution{}, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO approvals(organization_id,id,task_id,action_id,intent_hash,execution_id,status,expires_at) VALUES($1,$2,$3,$4,$5,$6,'pending',$7)", e.OrganizationID, e.ApprovalID, e.TaskID, e.ActionID, e.IntentHash, e.ID, e.ExpiresAt); err != nil {
		return domain.ToolExecution{}, err
	}
	if _, err = tx.Exec(ctx, "UPDATE tasks SET status='waiting_approval',waiting_reason='tool_approval_pending',version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2", e.OrganizationID, e.TaskID); err != nil {
		return domain.ToolExecution{}, err
	}
	if _, err = tx.Exec(ctx, "UPDATE runs SET status='waiting_approval' WHERE organization_id=$1 AND id=$2", e.OrganizationID, e.RunID); err != nil {
		return domain.ToolExecution{}, err
	}
	if err = emit(ctx, tx, e.OrganizationID, e.TaskID, "tool.prepared", map[string]any{"execution_id": e.ID, "action_id": e.ActionID, "approval_id": e.ApprovalID, "intent_hash": e.IntentHash, "reservation_id": e.ReservationID, "external_call": false}); err != nil {
		return domain.ToolExecution{}, err
	}
	return e, nil
}

func toolSnapshotCAS(t domain.Task, e domain.ToolExecution, taskVersion, executionVersion int, intent string) error {
	if t.Version != taskVersion || e.Version != executionVersion || e.IntentHash != intent {
		return domain.Err("conflict", "Снимок действия изменился. Откройте его заново", 409)
	}
	return nil
}
func toolCurrentTask(t domain.Task, e domain.ToolExecution) error {
	if t.Status != "waiting_approval" || t.CurrentRunID != e.RunID || t.CandidateID != e.CandidateID {
		return domain.Err("invalid_transition", "Действие больше не относится к текущему ожидающему результату", 409)
	}
	return nil
}

func toolActorsAuthorizedTx(ctx context.Context, tx pgx.Tx, e domain.ToolExecution, c domain.ToolConnection, t domain.Task, a domain.Approval, requireApproval bool) error {
	if err := checkToolAuthorityEpochsTx(ctx, tx, e, requireApproval); err != nil {
		return err
	}
	if err := authorizeTaskOwner(ctx, tx, e.OrganizationID, t.OwnerID); err != nil {
		return err
	}
	var role, kind string
	if err := tx.QueryRow(ctx, `SELECT m.role,p.kind FROM memberships m JOIN principals p ON p.id=m.principal_id JOIN organizations o ON o.id=m.organization_id WHERE m.organization_id=$1 AND m.principal_id=$2 AND m.active AND p.active AND o.active`, e.OrganizationID, e.CreatedBy).Scan(&role, &kind); err != nil {
		return domain.Forbidden
	}
	actor := domain.Principal{ID: e.CreatedBy, Role: role, Kind: kind, OrganizationID: e.OrganizationID}
	if !toolPermission(actor, t, "actions.prepare_own", "actions.prepare_any") {
		return domain.Forbidden
	}
	if e.AutomationGrantID != "" {
		g, err := loadAutomationGrantTx(ctx, tx, e.OrganizationID, e.AutomationGrantID, false)
		if err != nil {
			return err
		}
		issuer, err := currentAutomationAuthorityTx(ctx, tx, g, t)
		if err != nil {
			return err
		}
		if issuer.ID != e.CreatedBy {
			return domain.Forbidden
		}
		if err = automationToolEligibility(g, c, a.DecisionMethod == "task_automation_grant"); err != nil {
			return err
		}
	}
	if requireApproval {
		if a.Status != "approved" || a.ExecutionID != e.ID || a.ActionID != e.ActionID || a.IntentHash != e.IntentHash || a.DecidedBy == "" || (c.Tool.RequireDistinctApprover && a.DecidedBy == e.CreatedBy) {
			return domain.Forbidden
		}
		if err := tx.QueryRow(ctx, `SELECT m.role,p.kind FROM memberships m JOIN principals p ON p.id=m.principal_id WHERE m.organization_id=$1 AND m.principal_id=$2 AND m.active AND p.active`, e.OrganizationID, a.DecidedBy).Scan(&role, &kind); err != nil || !domain.PrincipalHasPermission(domain.Principal{Role: role, Kind: kind}, "actions.approve_any") {
			return domain.Forbidden
		}
	}
	return nil
}

func (s *Store) DecideToolExecution(ctx context.Context, p domain.Principal, taskID, id, key string, in domain.DecideToolExecution) (json.RawMessage, error) {
	if err := validateDecideToolExecution(in); err != nil {
		return nil, err
	}
	return s.MutateGuarded(ctx, p, "POST /tasks/"+taskID+"/tool-executions/"+id+"/decide", key, in, toolGuard(taskID, "approve"), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		e, err := decideToolExecutionTx(ctx, tx, p, taskID, id, in, currentActorTx, "authorized_principal_decision")
		if err != nil {
			return nil, err
		}
		return toolViewTx(ctx, tx, p, e)
	})
}

func validateDecideToolExecution(in domain.DecideToolExecution) error {
	if in.ExpectedTaskVersion < 1 || in.ExpectedExecutionVersion < 1 || in.ExpectedApprovalVersion < 1 || !validDigest(in.IntentHash) || !validText(in.Reason, 4096) || (in.Decision != "approve" && in.Decision != "deny") {
		return domain.Err("invalid_request", "Укажите точный снимок, решение и причину", 400)
	}
	return nil
}

func decideToolExecutionTx(ctx context.Context, tx pgx.Tx, p domain.Principal, taskID string, id string, in domain.DecideToolExecution, authority commandAuthority, decisionMethod string) (domain.ToolExecution, error) {
	if authority == nil {
		return domain.ToolExecution{}, domain.Forbidden
	}
	if err := validateDecideToolExecution(in); err != nil {
		return domain.ToolExecution{}, err
	}
	if decisionMethod != "authorized_principal_decision" && decisionMethod != "task_automation_grant" {
		return domain.ToolExecution{}, domain.Forbidden
	}
	t, _, err := lockToolObservationTaskTx(ctx, tx, p.OrganizationID, taskID)
	if err != nil {
		return domain.ToolExecution{}, err
	}
	e, err := loadToolExecutionTx(ctx, tx, p.OrganizationID, taskID, id, true)
	if err != nil {
		return domain.ToolExecution{}, err
	}
	if err = toolSnapshotCAS(t, e, in.ExpectedTaskVersion, in.ExpectedExecutionVersion, in.IntentHash); err != nil {
		return domain.ToolExecution{}, err
	}
	if err = toolCurrentTask(t, e); err != nil {
		return domain.ToolExecution{}, err
	}
	a, err := scanApproval(tx.QueryRow(ctx, "SELECT "+approvalColumns+" FROM approvals WHERE organization_id=$1 AND id=$2 FOR UPDATE", e.OrganizationID, e.ApprovalID))
	if err != nil {
		return domain.ToolExecution{}, err
	}
	if e.Status != "awaiting_approval" || a.Status != "pending" || a.Version != in.ExpectedApprovalVersion {
		return domain.ToolExecution{}, domain.Err("conflict", "Согласование уже изменилось", 409)
	}
	c, err := loadToolConnectionTx(ctx, tx, e.OrganizationID, e.ToolID, e.ToolVersion, true)
	if err != nil {
		return domain.ToolExecution{}, err
	}
	if in.Decision == "approve" {
		if decisionMethod == "task_automation_grant" {
			g, e2 := loadAutomationGrantTx(ctx, tx, e.OrganizationID, e.AutomationGrantID, false)
			if e2 != nil {
				return domain.ToolExecution{}, e2
			}
			if e2 = automationToolEligibility(g, c, true); e2 != nil {
				return domain.ToolExecution{}, e2
			}
			actor, e2 := currentAutomationAuthorityTx(ctx, tx, g, t)
			if e2 != nil {
				return domain.ToolExecution{}, e2
			}
			if actor.ID != p.ID {
				return domain.ToolExecution{}, domain.Forbidden
			}
		}
		if !c.Tool.Active || domain.ToolConnectionDigest(c) != e.ToolProfileDigest {
			return domain.ToolExecution{}, domain.Forbidden
		}
		if c.Tool.RequireDistinctApprover && e.CreatedBy == p.ID {
			return domain.ToolExecution{}, domain.Forbidden
		}
		if err = toolActorsAuthorizedTx(ctx, tx, e, c, t, a, false); err != nil {
			return domain.ToolExecution{}, err
		}
	}
	if err = authority(ctx, tx, p); err != nil {
		return domain.ToolExecution{}, err
	}
	var timely bool
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()<$1 AND clock_timestamp()<$2", e.ExpiresAt, t.ExecutionDeadline).Scan(&timely); err != nil {
		return domain.ToolExecution{}, err
	}
	if !timely {
		return domain.ToolExecution{}, domain.Err("conflict", "Срок согласования истёк", 409)
	}
	status := "approved"
	if in.Decision == "deny" {
		status = "denied"
	}
	if _, err = tx.Exec(ctx, "UPDATE approvals SET status=$3,version=version+1,decided_by=$4,decided_at=clock_timestamp(),decision_method=$6,reason=$5 WHERE organization_id=$1 AND id=$2", e.OrganizationID, a.ID, status, p.ID, in.Reason, decisionMethod); err != nil {
		return domain.ToolExecution{}, err
	}
	e, err = scanToolExecution(tx.QueryRow(ctx, "UPDATE tool_executions SET status=$3,version=version+1,reason=$4 WHERE organization_id=$1 AND id=$2 RETURNING "+toolExecutionColumns, e.OrganizationID, e.ID, status, in.Reason))
	if err != nil {
		return domain.ToolExecution{}, err
	}
	if status == "denied" {
		if err = releaseToolUnsentTx(ctx, tx, e, "tool.denied.unsent"); err != nil {
			return domain.ToolExecution{}, err
		}
		if err = restoreToolTaskTx(ctx, tx, e); err != nil {
			return domain.ToolExecution{}, err
		}
	} else {
		if _, err = tx.Exec(ctx, "UPDATE tasks SET waiting_reason='tool_dispatch_pending',version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2", e.OrganizationID, e.TaskID); err != nil {
			return domain.ToolExecution{}, err
		}
	}
	if err = emit(ctx, tx, e.OrganizationID, e.TaskID, "tool.approval_decided", map[string]any{"execution_id": e.ID, "approval_id": a.ID, "decision": status, "principal_id": p.ID, "method": decisionMethod, "external_call": false}); err != nil {
		return domain.ToolExecution{}, err
	}
	return e, nil
}

func (s *Store) DispatchToolExecution(ctx context.Context, p domain.Principal, taskID, id, key string, in domain.DispatchToolExecution) (json.RawMessage, error) {
	if err := validateDispatchToolExecution(in); err != nil {
		return nil, err
	}
	return s.MutateGuarded(ctx, p, "POST /tasks/"+taskID+"/tool-executions/"+id+"/dispatch", key, in, toolGuard(taskID, "dispatch"), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		e, err := dispatchToolExecutionTx(ctx, tx, p, taskID, id, in, currentActorTx)
		if err != nil {
			return nil, err
		}
		return toolViewTx(ctx, tx, p, e)
	})
}

func validateDispatchToolExecution(in domain.DispatchToolExecution) error {
	if in.ExpectedTaskVersion < 1 || in.ExpectedExecutionVersion < 1 || in.ExpectedApprovalVersion < 1 || !validDigest(in.IntentHash) || !in.AcknowledgeCost {
		return domain.Err("invalid_request", "Подтвердите точный снимок и возможный расход", 400)
	}
	return nil
}

func dispatchToolExecutionTx(ctx context.Context, tx pgx.Tx, p domain.Principal, taskID string, id string, in domain.DispatchToolExecution, authority commandAuthority) (domain.ToolExecution, error) {
	if authority == nil {
		return domain.ToolExecution{}, domain.Forbidden
	}
	if err := validateDispatchToolExecution(in); err != nil {
		return domain.ToolExecution{}, err
	}
	t, _, err := lockToolTaskTx(ctx, tx, p.OrganizationID, taskID)
	if err != nil {
		return domain.ToolExecution{}, err
	}
	e, err := loadToolExecutionTx(ctx, tx, p.OrganizationID, taskID, id, true)
	if err != nil {
		return domain.ToolExecution{}, err
	}
	if err = toolSnapshotCAS(t, e, in.ExpectedTaskVersion, in.ExpectedExecutionVersion, in.IntentHash); err != nil {
		return domain.ToolExecution{}, err
	}
	if err = toolCurrentTask(t, e); err != nil {
		return domain.ToolExecution{}, err
	}
	a, err := scanApproval(tx.QueryRow(ctx, "SELECT "+approvalColumns+" FROM approvals WHERE organization_id=$1 AND id=$2 FOR UPDATE", e.OrganizationID, e.ApprovalID))
	if err != nil {
		return domain.ToolExecution{}, err
	}
	if e.Status != "approved" || a.Status != "approved" || a.Version != in.ExpectedApprovalVersion {
		return domain.ToolExecution{}, domain.Err("conflict", "Согласование больше не разрешает отправку", 409)
	}
	c, err := loadToolConnectionTx(ctx, tx, e.OrganizationID, e.ToolID, e.ToolVersion, true)
	if err != nil {
		return domain.ToolExecution{}, err
	}
	if !c.Tool.Active || domain.ToolConnectionDigest(c) != e.ToolProfileDigest {
		return domain.ToolExecution{}, domain.Forbidden
	}
	if err = toolActorsAuthorizedTx(ctx, tx, e, c, t, a, true); err != nil {
		return domain.ToolExecution{}, err
	}
	scopes, err := lockCostScopes(ctx, tx, e.OrganizationID, e.TaskID)
	if err != nil {
		return domain.ToolExecution{}, err
	}
	// Only this exact target's waiting_approval is an allowed admission state.
	for i := range scopes.tasks {
		if scopes.tasks[i].ID == e.TaskID && scopes.tasks[i].Status == "waiting_approval" {
			scopes.tasks[i].Status = "waiting_input"
		}
	}
	if err = scopes.admit(0); err != nil {
		return domain.ToolExecution{}, err
	}
	if err = authority(ctx, tx, p); err != nil {
		return domain.ToolExecution{}, err
	}
	var timely bool
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()<$1 AND clock_timestamp()<$2", e.ExpiresAt, t.ExecutionDeadline).Scan(&timely); err != nil {
		return domain.ToolExecution{}, err
	}
	if !timely {
		return domain.ToolExecution{}, domain.Err("conflict", "Срок действия разрешения истёк", 409)
	}
	e, err = scanToolExecution(tx.QueryRow(ctx, "UPDATE tool_executions SET status='queued',version=version+1,reason='',dispatch_requested_by=$3 WHERE organization_id=$1 AND id=$2 RETURNING "+toolExecutionColumns, e.OrganizationID, e.ID, p.ID))
	if err != nil {
		return domain.ToolExecution{}, err
	}
	if _, err = tx.Exec(ctx, "UPDATE tasks SET waiting_reason='tool_execution_pending',version=version+1,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2", e.OrganizationID, e.TaskID); err != nil {
		return domain.ToolExecution{}, err
	}
	if err = emit(ctx, tx, e.OrganizationID, e.TaskID, "tool.dispatch_queued", map[string]any{"execution_id": e.ID, "intent_hash": e.IntentHash, "principal_id": p.ID, "cost_acknowledged": true, "external_call": false}); err != nil {
		return domain.ToolExecution{}, err
	}
	return e, nil
}

func (s *Store) ReconcileToolExecution(ctx context.Context, p domain.Principal, taskID, id, key string, in domain.ReconcileToolExecution) (json.RawMessage, error) {
	if in.ExpectedTaskVersion < 1 || in.ExpectedExecutionVersion < 1 || !validDigest(in.IntentHash) || (in.Outcome != "succeeded" && in.Outcome != "rejected") || !validText(in.EffectReference, 256) || !optionalText(in.Result, 262144) || !validDigest(in.EvidenceDigest) || !validText(in.Reason, 4096) {
		return nil, domain.Err("invalid_request", "Нужны точный снимок, исход, ссылка на проверку и причина сверки", 400)
	}
	return s.MutateGuarded(ctx, p, "POST /tasks/"+taskID+"/tool-executions/"+id+"/reconcile", key, in, toolGuard(taskID, "reconcile"), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		t, _, err := lockToolObservationTaskTx(ctx, tx, p.OrganizationID, taskID)
		if err != nil {
			return nil, err
		}
		e, err := loadToolExecutionTx(ctx, tx, p.OrganizationID, taskID, id, true)
		if err != nil {
			return nil, err
		}
		if err = toolSnapshotCAS(t, e, in.ExpectedTaskVersion, in.ExpectedExecutionVersion, in.IntentHash); err != nil {
			return nil, err
		}
		if e.Status != "unknown" {
			return nil, domain.Err("invalid_transition", "Сверка доступна только для неизвестного исхода", 409)
		}
		if err = currentActorTx(ctx, tx, p); err != nil {
			return nil, err
		}
		r, err := recordToolReceiptTx(ctx, tx, e, in.Outcome, in.Result, in.EffectReference, in.EvidenceDigest, "operator_assertion", p.ID, in.Reason)
		if err != nil {
			return nil, err
		}
		e, err = loadToolExecutionTx(ctx, tx, p.OrganizationID, taskID, id, false)
		if err != nil {
			return nil, err
		}
		if err = emit(ctx, tx, e.OrganizationID, e.TaskID, "tool.operator_reconciled", map[string]any{"execution_id": e.ID, "receipt_id": r.ID, "outcome": r.Outcome, "principal_id": p.ID, "method": "operator_assertion", "financial_settlement": false, "external_call": false}); err != nil {
			return nil, err
		}
		return toolViewTx(ctx, tx, p, e)
	})
}

// Both adapters and manual resolution append the same immutable observation.
// No financial row is changed and a cancelled task is never reopened.
func recordToolReceiptTx(ctx context.Context, tx pgx.Tx, e domain.ToolExecution, outcome, result, reference, evidence, method, principal, reason string) (domain.ToolOutcomeReceipt, error) {
	var empty domain.ToolOutcomeReceipt
	artifactID := domain.NewID("art")
	provenance := map[string]any{"adapter": "tool:execution", "execution_id": e.ID, "intent_hash": e.IntentHash, "verified": false, "observation_method": method, "effect_outcome_claim": outcome, "financial_settlement": false}
	if _, err := tx.Exec(ctx, "INSERT INTO artifacts(organization_id,id,task_id,name,media_type,content,provenance) VALUES($1,$2,$3,'tool-result.txt','text/plain',$4,$5)", e.OrganizationID, artifactID, e.TaskID, result, provenance); err != nil {
		return empty, err
	}
	r, err := scanToolReceipt(tx.QueryRow(ctx, `INSERT INTO tool_outcome_receipts(organization_id,id,execution_id,intent_hash,outcome,method,effect_reference,artifact_id,content_digest,evidence_digest,principal_id,reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),$12) RETURNING `+toolReceiptColumns, e.OrganizationID, domain.NewID("tor"), e.ID, e.IntentHash, outcome, method, reference, artifactID, Hash([]byte(result)), evidence, principal, reason))
	if err != nil {
		return empty, err
	}
	if _, err = tx.Exec(ctx, "UPDATE tool_executions SET status=$3,result_receipt_id=$4,reason=$5,version=version+1,lease_owner=NULL,lease_until=NULL WHERE organization_id=$1 AND id=$2", e.OrganizationID, e.ID, outcome, r.ID, reason); err != nil {
		return empty, err
	}
	return r, restoreToolTaskTx(ctx, tx, e)
}
