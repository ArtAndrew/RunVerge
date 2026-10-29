// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/progressguard"
	"github.com/jackc/pgx/v5"
)

const progressDetectorVersion = "deterministic-progress/v1"

// ReconcileTaskAutomation performs at most one durable automatic transition.
// It never calls a provider, reconstructs a login credential, or accepts a final
// answer. The ordinary workers execute queued work through the existing gateway.
func (s *Store) ReconcileTaskAutomation(ctx context.Context) (bool, error) {
	if err := s.CheckRuntimeAdmission(ctx); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var org, id, taskID, rootID string
	err := s.Pool.QueryRow(ctx, `SELECT g.organization_id,g.id,g.task_id,t.root_id FROM task_automation_grants g JOIN tasks t ON t.organization_id=g.organization_id AND t.id=g.task_id
 WHERE NOT recovery_historical_task(g.organization_id,g.task_id) AND g.status='active' AND (NOT EXISTS(SELECT 1 FROM memberships actor JOIN principals ap ON ap.id=actor.principal_id
 JOIN memberships own ON own.organization_id=actor.organization_id AND own.principal_id=g.owner_id JOIN principals op ON op.id=own.principal_id
 JOIN organizations tenant ON tenant.id=actor.organization_id
 WHERE actor.organization_id=g.organization_id AND actor.principal_id=g.principal_id AND actor.active AND ap.active AND actor.role IN('owner','admin')
 AND own.active AND op.active AND own.role<>'viewer' AND tenant.active AND g.principal_authority_epoch=actor.security_epoch AND g.owner_authority_epoch=own.security_epoch
 AND g.principal_policy_epoch=current_principal_policy_epoch(g.organization_id,g.principal_id) AND g.owner_policy_epoch=current_principal_policy_epoch(g.organization_id,g.owner_id))
 OR g.expires_at<=clock_timestamp() OR t.status IN('blocked','succeeded','failed','cancelled')
 OR (t.status='waiting_input' AND t.waiting_reason IN('model_tools_pending','model_candidate_pending'))
 OR (g.grant_body->>'mode'='read_only' AND t.status='waiting_approval' AND EXISTS(SELECT 1 FROM tool_executions e WHERE e.organization_id=g.organization_id AND e.task_id=g.task_id AND e.automation_grant_id=g.id AND e.status IN('awaiting_approval','approved'))))
 ORDER BY g.created_at,g.id LIMIT 1`).Scan(&org, &id, &taskID, &rootID)
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
	if err = lockAutomationAuthorityTx(ctx, tx, org, taskID, id); err != nil {
		return false, err
	}
	var gate string
	if err = tx.QueryRow(ctx, "SELECT root_id FROM task_tree_controls WHERE organization_id=$1 AND root_id=$2 FOR NO KEY UPDATE SKIP LOCKED", org, rootID).Scan(&gate); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	task, _, err := lockToolObservationTaskTx(ctx, tx, org, taskID)
	if err != nil {
		return false, err
	}
	g, err := loadAutomationGrantTx(ctx, tx, org, id, true)
	if err != nil {
		return false, err
	}
	if g.Status != "active" {
		return false, tx.Commit(ctx)
	}
	stop := func(status, reason string, signals []progressguard.Signal, snapshot any) (bool, error) {
		if err := finishAutomationTx(ctx, tx, g, status, reason, signals, snapshot); err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}
	p, err := currentAutomationAuthorityTx(ctx, tx, g, task)
	if err != nil {
		if !errors.Is(err, domain.Forbidden) {
			return false, err
		}
		var expired bool
		if e := tx.QueryRow(ctx, "SELECT clock_timestamp()>=$1", g.ExpiresAt).Scan(&expired); e != nil {
			return false, e
		}
		if expired {
			return stop("expired", "grant_expired", nil, task)
		}
		return stop("stopped", "automation_authorization_revoked", nil, task)
	}
	history, err := automationHistoryTx(ctx, tx, org, taskID)
	if err != nil {
		return stop("stopped", "progress_history_unavailable", []progressguard.Signal{{Code: "progress_history_unavailable", EvidenceIDs: []string{taskID}}}, map[string]any{"task_id": taskID, "history_complete": false})
	}
	decision, err := progressguard.Evaluate(ctx, progressguard.DefaultPolicy(), history, nil)
	if err != nil {
		return stop("stopped", "progress_guard_unavailable", []progressguard.Signal{{Code: "progress_guard_unavailable", EvidenceIDs: []string{taskID}}}, history)
	}
	rootSignals, rootErr := automationRootHardGuardTx(ctx, tx, task, nil, nil)
	if rootErr != nil {
		return stop("stopped", "progress_history_unavailable", []progressguard.Signal{{Code: "progress_history_unavailable", EvidenceIDs: []string{task.RootID}}}, history)
	}
	if len(rootSignals) > 0 {
		return stop("stopped", rootSignals[0].Code, rootSignals, history)
	}
	nativeBatch := task.CandidateID != "" && (task.WaitingReason == "model_tools_pending" || task.WaitingReason == "tool_approval_pending" || task.WaitingReason == "tool_dispatch_pending" || task.WaitingReason == "tool_execution_pending")
	if decision.Stop && !nativeBatch {
		return stop("stopped", decision.Signals[0].Code, decision.Signals, history)
	}
	if domain.Terminal(task.Status) || task.Status == "blocked" {
		return stop("stopped", "task_requires_attention", nil, task)
	}
	if task.Status == "waiting_input" && task.WaitingReason == "model_candidate_pending" {
		return stop("completed", "final_candidate_requires_review", nil, history)
	}
	if task.CandidateID == "" {
		return false, tx.Commit(ctx)
	}
	proposals, err := automationCurrentProposalsTx(ctx, tx, task)
	if err != nil {
		return false, err
	}
	if len(proposals) == 0 {
		return stop("stopped", "proposal_batch_unavailable", nil, task)
	}
	// Check the WHOLE immutable batch before preparing any automatic action.
	// A read grant never executes the safe subset of a mixed/retired batch.
	profiles := map[string]domain.ToolConnection{}
	for _, proposal := range proposals {
		c, e := loadToolConnectionTx(ctx, tx, org, proposal.ToolID, proposal.ToolVersion, true)
		if e != nil {
			return stop("stopped", "tool_profile_unavailable", nil, proposals)
		}
		if domain.ToolConnectionDigest(c) != proposal.ToolProfileDigest || automationToolEligibility(g, c, false) != nil {
			return stop("stopped", "tool_outside_automation_grant", nil, proposals)
		}
		profiles[proposal.ID] = c
	}
	rootSignals, rootErr = automationRootHardGuardTx(ctx, tx, task, proposals, profiles)
	if rootErr != nil {
		return stop("stopped", "progress_history_unavailable", []progressguard.Signal{{Code: "progress_history_unavailable", EvidenceIDs: []string{task.RootID}}}, history)
	}
	if len(rootSignals) > 0 {
		return stop("stopped", rootSignals[0].Code, rootSignals, history)
	}
	currentIDs := map[string]bool{}
	for _, proposal := range proposals {
		currentIDs[proposal.ID] = true
	}
	prior := make([]progressguard.Observation, 0, len(history))
	byID := map[string]progressguard.Observation{}
	for _, observation := range history {
		byID[observation.ID] = observation
		if !currentIDs[observation.ID] {
			prior = append(prior, observation)
		}
	}
	fingerprints := map[string]string{}
	for _, proposal := range proposals {
		c := profiles[proposal.ID]
		intent := progressguard.Intent{ToolID: proposal.ToolID, Effect: c.Tool.Effect, ArgumentsJSON: proposal.ArgumentsJSON}
		check, e := progressguard.Evaluate(ctx, progressguard.DefaultPolicy(), prior, &intent)
		if e != nil {
			return stop("stopped", "progress_guard_unavailable", []progressguard.Signal{{Code: "progress_guard_unavailable", EvidenceIDs: []string{proposal.ID}}}, history)
		}
		if earlier := fingerprints[check.ProposedIntentFingerprint]; earlier != "" {
			return stop("stopped", "duplicate_batch_intent", []progressguard.Signal{{Code: "duplicate_batch_intent", Fingerprint: check.ProposedIntentFingerprint, EvidenceIDs: []string{earlier, proposal.ID}, Count: 2}}, history)
		}
		fingerprints[check.ProposedIntentFingerprint] = proposal.ID
		if check.Stop {
			return stop("stopped", check.Signals[0].Code, check.Signals, history)
		}
		observation, exists := byID[proposal.ID]
		if !exists {
			return stop("stopped", "progress_history_unavailable", nil, history)
		}
		prior = append(prior, observation)
	}
	authority := automationAuthority(g, taskID)
	// A savepoint makes a rejected/expired admission leave no partial reservation,
	// allocation, approval or next operation while its stop remains durable.
	step := func(fn func(pgx.Tx) error) (bool, error) {
		child, e := tx.Begin(ctx)
		if e != nil {
			return false, e
		}
		e = fn(child)
		if e == nil {
			e = authority(ctx, child, p)
		}
		if e != nil {
			if rollbackErr := child.Rollback(ctx); rollbackErr != nil {
				return false, rollbackErr
			}
			var admission *domain.Error
			if !errors.As(e, &admission) {
				return false, e
			}
			return stop("stopped", "automation_admission_"+admission.Code, nil, history)
		}
		if e = child.Commit(ctx); e != nil {
			return false, e
		}
		return true, tx.Commit(ctx)
	}
	for _, proposal := range proposals {
		if proposal.ExecutionID == "" {
			if task.Status != "waiting_input" || task.WaitingReason != "model_tools_pending" {
				return false, tx.Commit(ctx)
			}
			in := domain.PrepareToolExecution{OccurrenceID: "model:" + proposal.ID, ModelProposalID: proposal.ID, ExpectedTaskVersion: task.Version, CandidateID: proposal.CandidateID, ArtifactID: proposal.ArtifactID, ArtifactVersion: proposal.ArtifactVersion, ContentDigest: proposal.ContentDigest, ToolID: proposal.ToolID, ToolVersion: proposal.ToolVersion, ArgumentsJSON: proposal.ArgumentsJSON, ExpiresInSeconds: domain.MaxAutomationTTLSeconds}
			return step(func(child pgx.Tx) error {
				_, e := prepareToolExecutionTx(ctx, child, p, taskID, in, authority, g.ID)
				return e
			})
		}
		execution, e := loadToolExecutionTx(ctx, tx, org, taskID, proposal.ExecutionID, true)
		if e != nil {
			return false, e
		}
		switch execution.Status {
		case "succeeded", "rejected":
			var method string
			if e = tx.QueryRow(ctx, "SELECT method FROM tool_outcome_receipts WHERE organization_id=$1 AND id=$2 AND execution_id=$3", org, execution.ResultReceiptID, execution.ID).Scan(&method); e != nil {
				return false, rowExists(e)
			}
			if method != "registered_tool_response" {
				return stop("stopped", "operator_assertion_requires_review", nil, history)
			}
		case "awaiting_approval", "approved":
			if g.Mode != "read_only" {
				return false, tx.Commit(ctx)
			}
			if execution.AutomationGrantID != g.ID {
				return stop("stopped", "existing_action_requires_review", nil, history)
			}
			a, e := scanApproval(tx.QueryRow(ctx, "SELECT "+approvalColumns+" FROM approvals WHERE organization_id=$1 AND id=$2 FOR UPDATE", org, execution.ApprovalID))
			if e != nil {
				return false, e
			}
			if execution.Status == "awaiting_approval" {
				in := domain.DecideToolExecution{ExpectedTaskVersion: task.Version, ExpectedExecutionVersion: execution.Version, ExpectedApprovalVersion: a.Version, IntentHash: execution.IntentHash, Decision: "approve", Reason: "Explicit task automation grant: " + g.ID}
				return step(func(child pgx.Tx) error {
					_, e := decideToolExecutionTx(ctx, child, p, taskID, execution.ID, in, authority, "task_automation_grant")
					return e
				})
			}
			if a.DecisionMethod != "task_automation_grant" {
				return stop("stopped", "existing_action_requires_review", nil, history)
			}
			in := domain.DispatchToolExecution{ExpectedTaskVersion: task.Version, ExpectedExecutionVersion: execution.Version, ExpectedApprovalVersion: a.Version, IntentHash: execution.IntentHash, AcknowledgeCost: true}
			return step(func(child pgx.Tx) error {
				_, e := dispatchToolExecutionTx(ctx, child, p, taskID, execution.ID, in, authority)
				return e
			})
		case "queued", "leased", "sent":
			return false, tx.Commit(ctx)
		case "unknown":
			return stop("stopped", "unknown_effect", nil, history)
		default:
			return stop("stopped", "unperformed_action_requires_review", nil, history)
		}
	}
	if task.Status != "waiting_input" || task.WaitingReason != "model_tools_pending" {
		return false, tx.Commit(ctx)
	}
	in := domain.ContinueModelRequest{ExpectedTaskVersion: task.Version, CandidateID: task.CandidateID, AcknowledgeCost: true, Reason: "Explicit bounded task automation grant: " + g.ID, Results: []domain.ModelToolResultBinding{}}
	for _, proposal := range proposals {
		execution, e := loadToolExecutionTx(ctx, tx, org, taskID, proposal.ExecutionID, false)
		if e != nil {
			return false, e
		}
		receipt, e := scanToolReceipt(tx.QueryRow(ctx, "SELECT "+toolReceiptColumns+" FROM tool_outcome_receipts WHERE organization_id=$1 AND id=$2 AND execution_id=$3", org, execution.ResultReceiptID, execution.ID))
		if e != nil {
			return false, e
		}
		if receipt.Method != "registered_tool_response" {
			return stop("stopped", "operator_assertion_requires_review", nil, history)
		}
		in.ArtifactID, in.ArtifactVersion, in.ContentDigest = proposal.ArtifactID, proposal.ArtifactVersion, proposal.ContentDigest
		in.Results = append(in.Results, domain.ModelToolResultBinding{ProposalID: proposal.ID, ExecutionID: execution.ID, ExecutionVersion: execution.Version, Status: execution.Status, ReceiptID: receipt.ID, ContentDigest: receipt.ContentDigest})
	}
	return step(func(child pgx.Tx) error {
		_, e := continueModelTx(ctx, child, p, taskID, in, authority, g.ID)
		return e
	})
}

func automationCurrentProposalsTx(ctx context.Context, tx pgx.Tx, t domain.Task) ([]domain.ModelToolProposal, error) {
	rows, err := tx.Query(ctx, "SELECT "+modelProposalColumns+" FROM model_tool_proposals p LEFT JOIN tool_executions e ON e.organization_id=p.organization_id AND e.model_proposal_id=p.id WHERE p.organization_id=$1 AND p.task_id=$2 AND p.run_id=$3 AND p.candidate_id=$4 ORDER BY p.ordinal", t.OrganizationID, t.ID, t.CurrentRunID, t.CandidateID)
	if err != nil {
		return nil, err
	}
	values := []domain.ModelToolProposal{}
	for rows.Next() {
		v, e := scanModelProposal(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		values = append(values, v)
	}
	err = rows.Err()
	rows.Close()
	return values, err
}

// One observation per logical native proposal/manual execution, across ALL runs.
// No lifecycle event, new call ID, plan or model assertion becomes progress.
func automationHistoryTx(ctx context.Context, tx pgx.Tx, org, taskID string) ([]progressguard.Observation, error) {
	rows, err := tx.Query(ctx, `SELECT id,tool_id,tool_version,arguments_json,arguments_digest,status,output_digest,created_at,ordinal FROM (
 SELECT p.id,p.tool_id,p.tool_version,p.arguments_json,p.arguments_digest,COALESCE(e.status,'proposed') AS status,COALESCE(r.content_digest,'') AS output_digest,p.created_at,p.ordinal
 FROM model_tool_proposals p LEFT JOIN tool_executions e ON e.organization_id=p.organization_id AND e.model_proposal_id=p.id LEFT JOIN tool_outcome_receipts r ON r.organization_id=e.organization_id AND r.id=e.result_receipt_id
 WHERE p.organization_id=$1 AND p.task_id=$2
 UNION ALL SELECT e.id,e.tool_id,e.tool_version,e.intent->>'arguments_json',e.intent->>'arguments_digest',e.status,COALESCE(r.content_digest,''),e.created_at,1 FROM tool_executions e LEFT JOIN tool_outcome_receipts r ON r.organization_id=e.organization_id AND r.id=e.result_receipt_id WHERE e.organization_id=$1 AND e.task_id=$2 AND e.model_proposal_id IS NULL
 ) history ORDER BY created_at,ordinal,id LIMIT 513`, org, taskID)
	if err != nil {
		return nil, err
	}
	type item struct {
		id, toolID, args, argsDigest, status, out string
		version                                   int
		at                                        time.Time
		ordinal                                   int
	}
	items := []item{}
	for rows.Next() {
		var v item
		if err = rows.Scan(&v.id, &v.toolID, &v.version, &v.args, &v.argsDigest, &v.status, &v.out, &v.at, &v.ordinal); err != nil {
			break
		}
		items = append(items, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(items) > progressguard.MaxHistory {
		return nil, progressguard.ErrComplexity
	}
	type orderedObservation struct {
		observation progressguard.Observation
		at          time.Time
		ordinal     int
	}
	ordered := make([]orderedObservation, 0, len(items))
	history := make([]progressguard.Observation, 0, len(items))
	cache := map[string]domain.ToolConnection{}
	for _, v := range items {
		if Hash([]byte(v.args)) != v.argsDigest {
			return nil, automationSnapshotConflict()
		}
		key := v.toolID + "/" + strconv.Itoa(v.version)
		c, exists := cache[key]
		if !exists {
			c, err = loadToolConnectionTx(ctx, tx, org, v.toolID, v.version, false)
			if err != nil {
				return nil, err
			}
			cache[key] = c
		}
		fingerprint, e := progressguard.IntentFingerprint(ctx, progressguard.Intent{ToolID: v.toolID, Effect: c.Tool.Effect, ArgumentsJSON: v.args})
		if e != nil {
			return nil, e
		}
		outcome := v.status
		switch outcome {
		case "succeeded", "rejected", "unknown":
		case "denied", "expired", "cancelled":
			outcome = "rejected"
			if v.out == "" {
				v.out = Hash([]byte("known-unsent:" + v.status))
			}
		default:
			outcome = "proposed"
		}
		ordered = append(ordered, orderedObservation{observation: progressguard.Observation{ID: v.id, IntentFingerprint: fingerprint, Effect: c.Tool.Effect, Outcome: outcome, OutputDigest: v.out}, at: v.at, ordinal: v.ordinal})
	}
	// Text candidates remain explicitly unverified; equal output is a signal,
	// never proof that the content is false or that a task has been completed.
	rows, err = tx.Query(ctx, `SELECT c.id,a.content,c.content_digest,c.created_at FROM task_candidates c JOIN artifacts a ON a.organization_id=c.organization_id AND a.id=c.artifact_id AND a.version=c.artifact_version WHERE c.organization_id=$1 AND c.task_id=$2 AND c.source='model' ORDER BY c.created_at,c.id LIMIT 513`, org, taskID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, content, digest string
		var at time.Time
		if err = rows.Scan(&id, &content, &digest, &at); err != nil {
			break
		}
		if Hash([]byte(content)) != digest {
			err = automationSnapshotConflict()
			break
		}
		ordered = append(ordered, orderedObservation{observation: progressguard.Observation{ID: id, Outcome: "output", OutputDigest: Hash([]byte(strings.TrimSpace(content))), UnverifiedOutput: true}, at: at})
		if len(ordered) > progressguard.MaxHistory {
			err = progressguard.ErrComplexity
			break
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].at.Equal(ordered[j].at) {
			return ordered[i].at.Before(ordered[j].at)
		}
		if ordered[i].ordinal != ordered[j].ordinal {
			return ordered[i].ordinal < ordered[j].ordinal
		}
		return ordered[i].observation.ID < ordered[j].observation.ID
	})
	for _, v := range ordered {
		history = append(history, v.observation)
	}
	return history, err
}
func finishAutomationTx(ctx context.Context, tx pgx.Tx, g domain.TaskAutomationGrant, status, reason string, signals []progressguard.Signal, snapshot any) error {
	digest := digestValue(map[string]any{"snapshot": snapshot, "signals": signals})
	for _, signal := range signals {
		ids := signal.EvidenceIDs
		if ids == nil {
			ids = []string{}
		}
		body, err := json.Marshal(ids)
		if err != nil {
			return err
		}
		key := digestValue([]string{g.ID, signal.Code, signal.Fingerprint, digest})
		if _, err = tx.Exec(ctx, `INSERT INTO progress_signals(organization_id,id,task_id,grant_id,code,detector_version,fingerprint,evidence_ids,snapshot_digest,signal_key) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(organization_id,task_id,signal_key) DO NOTHING`, g.OrganizationID, domain.NewID("psg"), g.TaskID, g.ID, signal.Code, progressDetectorVersion, signal.Fingerprint, body, digest, key); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, "UPDATE task_automation_grants SET status=$3,version=version+1,stop_reason=$4 WHERE organization_id=$1 AND id=$2 AND status='active'", g.OrganizationID, g.ID, status, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	return emit(ctx, tx, g.OrganizationID, g.TaskID, "automation."+status, map[string]any{"grant_id": g.ID, "reason": reason, "snapshot_digest": digest, "signal_count": len(signals), "task_acceptance_changed": false, "external_call": false})
}

// Hard effect safety spans the root tree; soft read repetition remains causal
// to one task so concurrent legitimate sibling lookups are not a fake cycle.
func automationRootHardGuardTx(ctx context.Context, tx pgx.Tx, task domain.Task, proposals []domain.ModelToolProposal, profiles map[string]domain.ToolConnection) ([]progressguard.Signal, error) {
	rows, err := tx.Query(ctx, `SELECT COALESCE(e.model_proposal_id,e.id),e.tool_id,e.tool_version,e.intent->>'arguments_json',e.intent->>'arguments_digest',e.status FROM tool_executions e JOIN tasks t ON t.organization_id=e.organization_id AND t.id=e.task_id WHERE e.organization_id=$1 AND t.root_id=$2 AND e.status IN('unknown','succeeded') ORDER BY e.created_at,e.id LIMIT 513`, task.OrganizationID, task.RootID)
	if err != nil {
		return nil, err
	}
	type effect struct {
		id, tool, args, digest, status string
		version                        int
	}
	values := []effect{}
	for rows.Next() {
		var v effect
		if err = rows.Scan(&v.id, &v.tool, &v.version, &v.args, &v.digest, &v.status); err != nil {
			break
		}
		values = append(values, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(values) > progressguard.MaxHistory {
		return nil, progressguard.ErrComplexity
	}
	signals := []progressguard.Signal{}
	unknown := progressguard.Signal{Code: "unknown_effect", EvidenceIDs: []string{}}
	seen := map[string][]string{}
	ordered := []string{}
	for _, v := range values {
		if Hash([]byte(v.args)) != v.digest {
			return nil, automationSnapshotConflict()
		}
		if v.status == "unknown" {
			unknown.Count++
			unknown.EvidenceIDs = append(unknown.EvidenceIDs, v.id)
			continue
		}
		c, e := loadToolConnectionTx(ctx, tx, task.OrganizationID, v.tool, v.version, false)
		if e != nil {
			return nil, e
		}
		if c.Tool.Effect == "read" {
			continue
		}
		fp, e := progressguard.IntentFingerprint(ctx, progressguard.Intent{ToolID: v.tool, Effect: c.Tool.Effect, ArgumentsJSON: v.args})
		if e != nil {
			return nil, e
		}
		if len(seen[fp]) == 0 {
			ordered = append(ordered, fp)
		}
		seen[fp] = append(seen[fp], v.id)
	}
	if unknown.Count > 0 {
		signals = append(signals, unknown)
	}
	for _, fp := range ordered {
		if len(seen[fp]) > 1 {
			signals = append(signals, progressguard.Signal{Code: "repeated_effect_intent", Fingerprint: fp, EvidenceIDs: seen[fp], Count: len(seen[fp])})
			return signals, nil
		}
	}
	for _, proposal := range proposals {
		c := profiles[proposal.ID]
		if c.Tool.Effect == "read" {
			continue
		}
		fp, e := progressguard.IntentFingerprint(ctx, progressguard.Intent{ToolID: proposal.ToolID, Effect: c.Tool.Effect, ArgumentsJSON: proposal.ArgumentsJSON})
		if e != nil {
			return nil, e
		}
		ids := []string{}
		for _, id := range seen[fp] {
			if id != proposal.ID {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			signals = append(signals, progressguard.Signal{Code: "repeated_effect_intent", Fingerprint: fp, EvidenceIDs: ids, Count: len(ids) + 1})
			return signals, nil
		}
	}
	return signals, nil
}
