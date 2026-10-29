// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
	"agentplatform.local/platform/internal/toolruntime"
	"github.com/jackc/pgx/v5"
)

var ErrRecoveryExecutionHistory = errors.New("invalid recovery execution history")

// This validates immutable recorded bindings, not past authorization or external
// effects. Missing dispatch at the snapshot never proves an action stayed unsent
// after the cut. No admission, mutation, secret resolution or network call occurs.
func validateRecoveryExecutionHistory(ctx context.Context, tx pgx.Tx, schema string) error {
	if ctx == nil || tx == nil || !recoverySnapshotName.MatchString(schema) {
		return ErrRecoveryExecutionHistory
	}
	for _, query := range recoveryExecutionHistoryQueries(schema) {
		var valid bool
		if err := tx.QueryRow(ctx, query).Scan(&valid); err != nil || !valid {
			return recoverySnapshotError(ctx, ErrRecoveryExecutionHistory)
		}
	}
	if err := recoveryCheckToolIntents(ctx, tx, schema); err != nil {
		return err
	}
	return recoveryCheckModelIntents(ctx, tx, schema)
}

func recoveryExecutionHistoryQueries(schema string) []string {
	queries := []string{
		`SELECT NOT EXISTS(SELECT 1 FROM {runs} WHERE (input_context='' AND input_digest<>'') OR (input_context<>'' AND input_digest<>pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(input_context,'UTF8')),'hex')))`,
		`SELECT NOT EXISTS(SELECT 1 FROM {tool_executions} e JOIN {tasks} t ON t.organization_id=e.organization_id AND t.id=e.task_id
 JOIN {reservations} r ON r.organization_id=e.organization_id AND r.id=e.reservation_id JOIN {actions} a ON a.organization_id=e.organization_id AND a.id=e.action_id
 WHERE r.task_id<>e.task_id OR r.operation_id<>e.operation_id OR r.action_id<>e.action_id OR r.category<>'tool' OR r.charge_key<>e.action_id
 OR r.amount_microusd::text IS DISTINCT FROM e.intent->>'max_charge_microusd' OR r.provider IS DISTINCT FROM e.intent->>'provider' OR r.price_version IS DISTINCT FROM e.intent->>'price_version'
 OR a.task_id<>e.task_id OR a.operation_id<>e.operation_id OR a.intent_hash<>e.intent_hash OR a.idempotency_key<>e.action_id
 OR e.expires_at>t.execution_deadline
 OR EXISTS(SELECT 1 FROM (VALUES ('id',e.id),('organization_id',e.organization_id),('task_id',e.task_id),('run_id',e.run_id),('operation_id',e.operation_id),('action_id',e.action_id),('reservation_id',e.reservation_id),('approval_id',e.approval_id),('tool_id',e.tool_id),('tool_version',e.tool_version::text),('tool_profile_digest',e.tool_profile_digest),('candidate_id',e.candidate_id),('artifact_id',e.artifact_id),('artifact_version',e.artifact_version::text),('content_digest',e.content_digest),('intent_hash',e.intent_hash),('created_by',e.created_by)) fields(key,value) WHERE e.intent->>fields.key IS DISTINCT FROM fields.value))`,
		`SELECT NOT EXISTS(SELECT 1 FROM {tool_executions} e JOIN {actions} a ON a.organization_id=e.organization_id AND a.id=e.action_id
 WHERE e.dispatch_issued_at IS DISTINCT FROM a.dispatch_issued_at OR e.dispatch_not_after IS DISTINCT FROM a.dispatch_not_after OR e.dispatch_fencing_token IS DISTINCT FROM a.dispatch_fencing_token
 OR (e.dispatch_issued_at IS NOT NULL AND (e.dispatch_not_after>e.expires_at OR e.dispatch_fencing_token>e.fencing_token))
 OR (e.dispatch_issued_at IS NULL AND e.status IN('sent','unknown','succeeded')))
 AND NOT EXISTS(SELECT 1 FROM {actions} a WHERE a.dispatch_issued_at IS NOT NULL AND
 (a.dispatch_not_after<=a.dispatch_issued_at OR a.dispatch_not_after>a.dispatch_issued_at+interval '2 seconds' OR a.dispatch_fencing_token<=0))`,
		`SELECT NOT EXISTS(SELECT 1 FROM {approvals} a JOIN {tool_executions} e ON e.organization_id=a.organization_id AND e.id=a.execution_id
 WHERE a.id<>e.approval_id OR a.task_id<>e.task_id OR a.action_id<>e.action_id OR a.intent_hash<>e.intent_hash OR a.expires_at<>e.expires_at
 OR (a.status IN('approved','denied') AND (a.decided_by IS NULL OR a.decided_at IS NULL OR a.reason='' OR a.decision_method NOT IN('authorized_principal_decision','task_automation_grant') OR a.decision_method IS NULL))
 OR (a.status NOT IN('approved','denied') AND (a.decided_by IS NOT NULL OR a.decided_at IS NOT NULL OR a.decision_method IS NOT NULL)))`,
		`SELECT NOT EXISTS(SELECT 1 FROM {tool_executions} e JOIN {task_candidates} c ON c.organization_id=e.organization_id AND c.id=e.candidate_id
 LEFT JOIN {model_tool_proposals} p ON p.organization_id=e.organization_id AND p.id=e.model_proposal_id
 WHERE (c.source='model_tools' AND (p.id IS NULL OR p.task_id<>e.task_id OR p.run_id<>e.run_id OR p.operation_id<>e.operation_id OR p.candidate_id<>e.candidate_id
 OR p.tool_id<>e.tool_id OR p.tool_version<>e.tool_version OR p.tool_profile_digest<>e.tool_profile_digest OR p.arguments_json IS DISTINCT FROM e.intent->>'arguments_json'
 OR p.arguments_digest IS DISTINCT FROM e.intent->>'arguments_digest' OR e.intent->>'occurrence_id' IS DISTINCT FROM 'model:'||p.id))
 OR (c.source<>'model_tools' AND e.model_proposal_id IS NOT NULL))`,
		`SELECT NOT EXISTS(SELECT 1 FROM {model_candidates} c JOIN {reservations} r ON r.organization_id=c.organization_id AND r.id=c.reservation_id
 JOIN {actions} a ON a.organization_id=r.organization_id AND a.id=r.action_id
 WHERE r.task_id<>c.task_id OR r.operation_id<>c.operation_id OR r.category<>'model' OR a.intent_hash<>c.intent_hash)
 AND NOT EXISTS(SELECT 1 FROM {model_turns} m JOIN {runs} r ON r.organization_id=m.organization_id AND r.id=m.run_id JOIN {tasks} t ON t.organization_id=m.organization_id AND t.id=m.task_id
 WHERE r.task_id<>m.task_id OR t.model_connection_id IS NULL OR m.messages_digest<>pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(m.messages_json,'UTF8')),'hex'))`,
		`SELECT NOT EXISTS(SELECT 1 FROM {tool_outcome_receipts} r JOIN {tool_executions} e ON e.organization_id=r.organization_id AND e.id=r.execution_id
 JOIN {artifacts} a ON a.organization_id=r.organization_id AND a.id=r.artifact_id
 WHERE e.result_receipt_id IS DISTINCT FROM r.id OR r.intent_hash<>e.intent_hash OR r.outcome<>e.status OR e.dispatch_issued_at IS NULL
 OR a.task_id<>e.task_id OR a.version<>1
 OR r.content_digest<>pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(a.content,'UTF8')),'hex')
 OR (r.method='operator_assertion' AND (r.principal_id IS NULL OR r.reason='')) OR (r.method='registered_tool_response' AND r.principal_id IS NOT NULL))`,
	}
	queries = append(queries, `SELECT NOT EXISTS(SELECT 1 FROM {reservations} r JOIN {tasks} t ON t.organization_id=r.organization_id AND t.id=r.task_id
 WHERE r.category='model' AND t.model_connection_id IS NOT NULL AND (r.action_id IS NULL OR NOT EXISTS(SELECT 1 FROM {model_turns} m WHERE m.organization_id=r.organization_id AND m.operation_id=r.operation_id AND m.task_id=r.task_id)))`)
	queries = append(queries, `SELECT NOT EXISTS(SELECT 1 FROM {reservations} r JOIN {tasks} t ON t.organization_id=r.organization_id AND t.id=r.task_id JOIN {actions} a ON a.organization_id=r.organization_id AND a.id=r.action_id
 WHERE r.category='model' AND t.model_connection_id IS NOT NULL AND (a.idempotency_key<>r.charge_key OR a.task_id<>r.task_id OR a.operation_id<>r.operation_id))`)
	queries = append(queries, `SELECT NOT EXISTS(SELECT 1 FROM {tool_executions} e WHERE
 ((e.status='succeeded' OR (e.status='rejected' AND e.dispatch_issued_at IS NOT NULL)) AND NOT EXISTS(SELECT 1 FROM {tool_outcome_receipts} r WHERE r.organization_id=e.organization_id AND r.execution_id=e.id AND r.id=e.result_receipt_id AND r.outcome=e.status))
 OR ((e.status NOT IN('succeeded','rejected') OR (e.status='rejected' AND e.dispatch_issued_at IS NULL)) AND e.result_receipt_id IS NOT NULL))`)
	queries = append(queries, `SELECT NOT EXISTS(SELECT 1 FROM {tool_executions} e JOIN {approvals} a ON a.organization_id=e.organization_id AND a.id=e.approval_id
 JOIN {tool_connections} p ON p.organization_id=e.organization_id AND p.id=e.tool_id AND p.version=e.tool_version
 WHERE (e.status='awaiting_approval' AND a.status<>'pending') OR (e.status='denied' AND a.status<>'denied')
 OR (e.status IN('approved','queued','leased','sent','unknown','succeeded','rejected') AND a.status<>'approved')
 OR (a.status='approved' AND p.profile->'tool'->'require_distinct_approver'='true'::jsonb AND a.decided_by=e.created_by))`)
	queries = append(queries, `SELECT NOT EXISTS(SELECT 1 FROM {tasks} t
 JOIN {task_decisions} d ON d.organization_id=t.organization_id AND d.id=t.accepted_decision_id
 JOIN {artifacts} a ON a.organization_id=d.organization_id AND a.id=d.artifact_id AND a.version=d.artifact_version
 WHERE d.decision<>'accept' OR d.task_id<>t.id OR t.status<>'succeeded'
 OR t.candidate_id IS DISTINCT FROM d.candidate_id OR t.current_run_id IS DISTINCT FROM d.run_id
 OR t.result IS DISTINCT FROM a.content OR d.task_version_after>t.version)`)
	// Identifiers come only from this fixed list, never archived SQL or names.
	tables := []string{"runs", "tasks", "tool_executions", "tool_connections", "reservations", "actions", "approvals", "task_candidates", "model_tool_proposals", "model_candidates", "model_turns", "tool_outcome_receipts", "artifacts", "task_decisions"}
	pairs := []string{}
	for _, table := range tables {
		pairs = append(pairs, "{"+table+"}", pgx.Identifier{schema, table}.Sanitize())
	}
	r := strings.NewReplacer(pairs...)
	for i := range queries {
		queries[i] = r.Replace(queries[i])
	}
	return queries
}

func recoveryCheckToolIntents(ctx context.Context, tx pgx.Tx, schema string) error {
	q := func(n string) string { return pgx.Identifier{schema, n}.Sanitize() }
	rows, err := tx.Query(ctx, "SELECT e.intent,e.expires_at,t.version,c.profile FROM "+q("tool_executions")+" e JOIN "+q("tasks")+" t ON t.organization_id=e.organization_id AND t.id=e.task_id JOIN "+q("tool_connections")+" c ON c.organization_id=e.organization_id AND c.id=e.tool_id AND c.version=e.tool_version")
	if err != nil {
		return recoverySnapshotError(ctx, ErrRecoveryExecutionHistory)
	}
	defer rows.Close()
	for rows.Next() {
		var raw, profile []byte
		var expires time.Time
		var version int
		if rows.Scan(&raw, &expires, &version, &profile) != nil {
			return ErrRecoveryExecutionHistory
		}
		var e domain.ToolExecution
		var c domain.ToolConnection
		if decodeRecoveryHistoryJSON(raw, &e, 131072) != nil || decodeRecoveryHistoryJSON(profile, &c, 262144) != nil || e.IntentHash != domain.ToolIntentDigest(e) || Hash([]byte(e.ArgumentsJSON)) != e.ArgumentsDigest || !e.ExpiresAt.Equal(expires) || e.SourceTaskVersion < 1 || e.SourceTaskVersion >= version || !occurrencePattern.MatchString(e.OccurrenceID) {
			return ErrRecoveryExecutionHistory
		}
		if e.Status != "awaiting_approval" || e.Version != 1 || !e.CreatedAt.IsZero() || e.DispatchedBy != "" || e.Reason != "" || e.ResultReceiptID != "" {
			return ErrRecoveryExecutionHistory
		}
		if _, err := toolruntime.CanonicalArguments(c, e.ArgumentsJSON); err != nil {
			return ErrRecoveryExecutionHistory
		}
		if domain.ToolConnectionDigest(c) != e.ToolProfileDigest || c.Tool.MaxChargeMicroUSD != e.MaxChargeMicroUSD || c.Tool.Provider != e.Provider || c.Tool.PriceVersion != e.PriceVersion {
			return ErrRecoveryExecutionHistory
		}
	}
	if rows.Err() != nil {
		return recoverySnapshotError(ctx, ErrRecoveryExecutionHistory)
	}
	return nil
}

func recoveryCheckModelIntents(ctx context.Context, tx pgx.Tx, schema string) error {
	q := func(n string) string { return pgx.Identifier{schema, n}.Sanitize() }
	rows, err := tx.Query(ctx, `SELECT r.organization_id,r.id,r.task_id,o.run_id,r.operation_id,r.charge_key,r.provider,r.price_version,r.amount_microusd,a.intent_hash,
 t.owner_id,t.agent_id,t.agent_version,t.goal,g.instructions,g.model,g.tools,g.model_tools,u.input_context,u.input_digest,m.messages_json,m.messages_digest,m.ordinal,c.profile
 FROM `+q("reservations")+` r JOIN `+q("tasks")+` t ON t.organization_id=r.organization_id AND t.id=r.task_id
 JOIN `+q("operations")+` o ON o.organization_id=r.organization_id AND o.id=r.operation_id
 JOIN `+q("runs")+` u ON u.organization_id=o.organization_id AND u.id=o.run_id
 JOIN `+q("agents")+` g ON g.organization_id=t.organization_id AND g.id=t.agent_id AND g.version=t.agent_version
 JOIN `+q("model_connections")+` c ON c.organization_id=t.organization_id AND c.id=t.model_connection_id AND c.version=t.model_connection_version
 JOIN `+q("actions")+` a ON a.organization_id=r.organization_id AND a.id=r.action_id
 JOIN `+q("model_turns")+` m ON m.organization_id=o.organization_id AND m.operation_id=o.id WHERE r.category='model'`)
	if err != nil {
		return recoverySnapshotError(ctx, ErrRecoveryExecutionHistory)
	}
	defer rows.Close()
	for rows.Next() {
		var s modelruntime.IntentSnapshot
		var id, key, provider, price, intent, inputDigest, transcript, transcriptDigest string
		var amount int64
		var tools, pins, profile []byte
		var ordinal int
		if rows.Scan(&s.OrganizationID, &id, &s.TaskID, &s.RunID, &s.OperationID, &key, &provider, &price, &amount, &intent, &s.TaskOwnerID, &s.AgentID, &s.AgentVersion, &s.Goal, &s.Instructions, &s.Price.Model, &tools, &pins, &s.RunInput, &inputDigest, &transcript, &transcriptDigest, &ordinal, &profile) != nil {
			return ErrRecoveryExecutionHistory
		}
		var c domain.ModelConnection
		var names []string
		if decodeRecoveryHistoryJSON(profile, &c, 262144) != nil || decodeRecoveryHistoryJSON(tools, &names, 32768) != nil || decodeRecoveryHistoryJSON(pins, &s.ModelTools, 262144) != nil {
			return ErrRecoveryExecutionHistory
		}
		if (s.RunInput == "" && inputDigest != "") || (s.RunInput != "" && Hash([]byte(s.RunInput)) != inputDigest) || Hash([]byte(transcript)) != transcriptDigest || (len(s.ModelTools) != 0 && len(names) != len(s.ModelTools)) || provider != c.ProviderAccount || price != c.PriceVersion || amount != c.MaxChargeMicroUSD || s.Price.Model != c.Model {
			return ErrRecoveryExecutionHistory
		}
		for i, pin := range s.ModelTools {
			if names[i] != pin.ToolID || pin.Name != domain.ModelToolName(s.OrganizationID, pin.ToolID, pin.ToolVersion) {
				return ErrRecoveryExecutionHistory
			}
		}
		var exactMessages []domain.ModelMessage
		if decodeRecoveryHistoryJSON([]byte(transcript), &exactMessages, domain.MaxModelTranscriptBytes) != nil {
			return ErrRecoveryExecutionHistory
		}
		messages, err := decodeModelMessages(transcript)
		if err != nil || ordinal < 1 || ordinal > domain.MaxModelTurnsPerTask || len(messages) < 2 || messages[0].Role != "system" || messages[0].Content != s.Instructions || messages[0].ToolCallID != "" || len(messages[0].ToolCalls) != 0 || messages[1].Role != "user" || messages[1].ToolCallID != "" || len(messages[1].ToolCalls) != 0 {
			return ErrRecoveryExecutionHistory
		}
		input := s.RunInput
		if input == "" {
			input = s.Goal
		}
		if messages[1].Content != input {
			return ErrRecoveryExecutionHistory
		}
		if err := recoveryCheckMessageBatches(messages, s.ModelTools, ordinal); err != nil {
			return err
		}
		s.Version = "model-attempt-v1"
		s.ModelMessages = messages
		if ordinal == 1 && len(s.ModelTools) == 0 {
			s.ModelMessages = nil
		}
		if len(s.ModelTools) != 0 || len(s.ModelMessages) != 0 {
			s.Version = "model-turn-v1"
		}
		s.Endpoint, s.TokenLimitField = c.Endpoint, c.TokenLimitField
		s.WireProfile, s.OpenRouterDataCollection = c.WireProfile, c.OpenRouterDataCollection
		s.MaxInputBytes, s.MaxOutputBytes, s.MaxOutputTokens = c.MaxInputBytes, c.MaxOutputBytes, c.MaxOutputTokens
		s.RequestTimeoutNanos = int64(time.Duration(c.TimeoutMilliseconds) * time.Millisecond)
		s.Price = RuntimeModelConfig(c, "unused-historical-no-network").Price
		got, err := modelruntime.HistoricalIntentDigest(s)
		if err != nil || got != intent || key != "model:"+s.OperationID {
			return ErrRecoveryExecutionHistory
		}
	}
	if rows.Err() != nil {
		return recoverySnapshotError(ctx, ErrRecoveryExecutionHistory)
	}
	return nil
}

func recoveryCheckMessageBatches(messages []domain.ModelMessage, tools []domain.ModelToolDefinition, ordinal int) error {
	byName := map[string]domain.ModelToolDefinition{}
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	seen := map[string]bool{}
	batches := 0
	for i := 2; i < len(messages); {
		assistant := messages[i]
		if assistant.Role != "assistant" || assistant.ToolCallID != "" || len(assistant.ToolCalls) < 1 || len(assistant.ToolCalls) > domain.MaxModelToolCalls || len(messages)-i-1 < len(assistant.ToolCalls) {
			return ErrRecoveryExecutionHistory
		}
		for j, call := range assistant.ToolCalls {
			def, ok := byName[call.Name]
			if !ok || !modelWireName.MatchString(call.ID) || seen[call.ID] {
				return ErrRecoveryExecutionHistory
			}
			seen[call.ID] = true
			c := domain.ToolConnection{Tool: domain.ToolInfo{InputSchemaJSON: def.InputSchemaJSON, MaxInputBytes: domain.MaxToolArgumentsBytes}}
			if _, err := toolruntime.CanonicalArguments(c, call.ArgumentsJSON); err != nil {
				return ErrRecoveryExecutionHistory
			}
			result := messages[i+j+1]
			if result.Role != "tool" || result.ToolCallID != call.ID || len(result.ToolCalls) != 0 || !validText(result.Content, domain.MaxRunInputBytes) {
				return ErrRecoveryExecutionHistory
			}
		}
		i += 1 + len(assistant.ToolCalls)
		batches++
	}
	if batches != ordinal-1 {
		return ErrRecoveryExecutionHistory
	}
	return nil
}
