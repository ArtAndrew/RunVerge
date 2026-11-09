// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"reflect"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/toolruntime"
	"github.com/jackc/pgx/v5"
)

type recoveryModelProposalRecord struct {
	ID          string                     `json:"id"`
	CallID      string                     `json:"call_id"`
	Name        string                     `json:"name"`
	Arguments   string                     `json:"arguments"`
	ExecutionID string                     `json:"execution_id"`
	Status      string                     `json:"status"`
	Reason      string                     `json:"reason"`
	Receipt     *domain.ToolOutcomeReceipt `json:"receipt"`
	Result      string                     `json:"result"`
}

// The conversation is a retained chain of quoted results. Checking its exact
// bindings does not promote those results into authenticated external facts.
func validateRecoveryModelHistory(ctx context.Context, tx pgx.Tx, schema string) error {
	if ctx == nil || tx == nil || !recoverySnapshotName.MatchString(schema) {
		return ErrRecoveryExecutionHistory
	}
	if err := recoveryCheckModelProposals(ctx, tx, schema); err != nil {
		return err
	}
	q := func(n string) string { return pgx.Identifier{schema, n}.Sanitize() }
	proposalJSON := `COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('id',p.id,'call_id',p.call_id,'name',p.tool_name,'arguments',p.arguments_json,'execution_id',COALESCE(e.id,''),'status',COALESCE(e.status,''),'reason',COALESCE(e.reason,''),'receipt',pg_catalog.to_jsonb(receipt)-'artifact_version','result',COALESCE(a.content,'')) ORDER BY p.ordinal)
 FROM ` + q("model_tool_proposals") + ` p LEFT JOIN ` + q("tool_executions") + ` e ON e.organization_id=p.organization_id AND e.model_proposal_id=p.id
 LEFT JOIN ` + q("tool_outcome_receipts") + ` receipt ON receipt.organization_id=e.organization_id AND receipt.id=e.result_receipt_id
 LEFT JOIN ` + q("artifacts") + ` a ON a.organization_id=receipt.organization_id AND a.id=receipt.artifact_id AND a.version=1
 WHERE p.organization_id=c.organization_id AND p.candidate_id=c.id),'[]'::jsonb)`
	rows, err := tx.Query(ctx, `SELECT m.ordinal,m.messages_json,m.messages_digest,COALESCE(previous.messages_json,''),COALESCE(c.source,''),COALESCE(source.content,''),`+proposalJSON+`,g.instructions,t.goal,r.input_context
 FROM `+q("model_turns")+` m JOIN `+q("tasks")+` t ON t.organization_id=m.organization_id AND t.id=m.task_id
 JOIN `+q("agents")+` g ON g.organization_id=t.organization_id AND g.id=t.agent_id AND g.version=t.agent_version
 JOIN `+q("runs")+` r ON r.organization_id=m.organization_id AND r.id=m.run_id
 LEFT JOIN `+q("model_turns")+` previous ON previous.organization_id=m.organization_id AND previous.run_id=m.run_id AND previous.ordinal=m.ordinal-1
 LEFT JOIN `+q("task_candidates")+` c ON c.organization_id=m.organization_id AND c.id=m.source_candidate_id
 LEFT JOIN `+q("artifacts")+` source ON source.organization_id=c.organization_id AND source.id=c.artifact_id AND source.version=c.artifact_version`)
	if err != nil {
		return recoverySnapshotError(ctx, ErrRecoveryExecutionHistory)
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int
		var transcript, digest, previous, sourceKind, source, instructions, goal, input string
		var proposalsRaw []byte
		if rows.Scan(&ordinal, &transcript, &digest, &previous, &sourceKind, &source, &proposalsRaw, &instructions, &goal, &input) != nil {
			return ErrRecoveryExecutionHistory
		}
		var messages []domain.ModelMessage
		if decodeRecoveryHistoryJSON([]byte(transcript), &messages, domain.MaxModelTranscriptBytes) != nil || Hash([]byte(transcript)) != digest || len(messages) < 2 {
			return ErrRecoveryExecutionHistory
		}
		if input == "" {
			input = goal
		}
		if !reflect.DeepEqual(messages[0], domain.ModelMessage{Role: "system", Content: instructions}) || !reflect.DeepEqual(messages[1], domain.ModelMessage{Role: "user", Content: input}) {
			return ErrRecoveryExecutionHistory
		}
		if ordinal == 1 {
			if len(messages) != 2 || previous != "" || sourceKind != "" {
				return ErrRecoveryExecutionHistory
			}
			continue
		}
		var prefix []domain.ModelMessage
		var body modelToolCandidateBody
		var proposals []recoveryModelProposalRecord
		if ordinal < 2 || sourceKind != "model_tools" || decodeRecoveryHistoryJSON([]byte(previous), &prefix, domain.MaxModelTranscriptBytes) != nil || decodeRecoveryHistoryJSON([]byte(source), &body, 262144) != nil || decodeRecoveryHistoryJSON(proposalsRaw, &proposals, 4<<20) != nil || len(body.ToolCalls) < 1 || len(body.ToolCalls) > domain.MaxModelToolCalls || len(proposals) != len(body.ToolCalls) || len(messages) != len(prefix)+1+len(body.ToolCalls) || !reflect.DeepEqual(messages[:len(prefix)], prefix) {
			return ErrRecoveryExecutionHistory
		}
		if !reflect.DeepEqual(messages[len(prefix)], domain.ModelMessage{Role: "assistant", Content: body.Text, ToolCalls: body.ToolCalls}) {
			return ErrRecoveryExecutionHistory
		}
		for i, call := range body.ToolCalls {
			p := proposals[i]
			message := messages[len(prefix)+1+i]
			var quoted modelQuotedToolResult
			if p.CallID != call.ID || p.Name != call.Name || p.Arguments != call.ArgumentsJSON || p.ExecutionID == "" || message.Role != "tool" || message.ToolCallID != call.ID || len(message.ToolCalls) != 0 || decodeRecoveryHistoryJSON([]byte(message.Content), &quoted, domain.MaxRunInputBytes) != nil || !quoted.Untrusted || quoted.ProposalID != p.ID || quoted.ExecutionID != p.ExecutionID || quoted.Status != p.Status || quoted.Reason != p.Reason {
				return ErrRecoveryExecutionHistory
			}
			if p.Status == "succeeded" || p.Status == "rejected" {
				if quoted.Receipt == nil || p.Receipt == nil || !recoverySameToolReceipt(*quoted.Receipt, *p.Receipt) || quoted.Result != p.Result || Hash([]byte(p.Result)) != p.Receipt.ContentDigest {
					return ErrRecoveryExecutionHistory
				}
			} else if (p.Status != "denied" && p.Status != "expired" && p.Status != "cancelled") || quoted.Receipt != nil || p.Receipt != nil || quoted.Result != "" {
				return ErrRecoveryExecutionHistory
			}
		}
	}
	if rows.Err() != nil {
		return recoverySnapshotError(ctx, ErrRecoveryExecutionHistory)
	}
	return nil
}

type recoveryModelProposalBinding struct {
	CallID        string `json:"call_id"`
	Name          string `json:"name"`
	Arguments     string `json:"arguments"`
	ToolID        string `json:"tool_id"`
	ToolVersion   int    `json:"tool_version"`
	ProfileDigest string `json:"profile_digest"`
	Ordinal       int    `json:"ordinal"`
	MaxInputBytes int    `json:"max_input_bytes"`
}

// Check every proposal candidate, including a fresh result that has never had
// a tool prepared or been consumed by a continuation. Pending proposals are
// immutable execution evidence, not legacy unpinned tool metadata.
func recoveryCheckModelProposals(ctx context.Context, tx pgx.Tx, schema string) error {
	q := func(n string) string { return pgx.Identifier{schema, n}.Sanitize() }
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM `+q("model_tool_proposals")+` p JOIN `+q("task_candidates")+` c ON c.organization_id=p.organization_id AND c.id=p.candidate_id WHERE c.source<>'model_tools')
 AND NOT EXISTS(SELECT 1 FROM `+q("task_candidates")+` c WHERE c.source='model_tools' AND NOT EXISTS(SELECT 1 FROM `+q("model_turns")+` m WHERE m.organization_id=c.organization_id AND m.operation_id=c.operation_id))`).Scan(&valid); err != nil || !valid {
		return recoverySnapshotError(ctx, ErrRecoveryExecutionHistory)
	}
	proposalJSON := `COALESCE((SELECT pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('call_id',p.call_id,'name',p.tool_name,'arguments',p.arguments_json,'tool_id',p.tool_id,'tool_version',p.tool_version,'profile_digest',p.tool_profile_digest,'ordinal',p.ordinal,'max_input_bytes',connection.profile->'tool'->'max_input_bytes') ORDER BY p.ordinal) FROM ` + q("model_tool_proposals") + ` p JOIN ` + q("tool_connections") + ` connection ON connection.organization_id=p.organization_id AND connection.id=p.tool_id AND connection.version=p.tool_version WHERE p.organization_id=c.organization_id AND p.candidate_id=c.id),'[]'::jsonb)`
	rows, err := tx.Query(ctx, `SELECT artifact.content,g.model_tools,`+proposalJSON+`,m.messages_json FROM `+q("task_candidates")+` c JOIN `+q("artifacts")+` artifact ON artifact.organization_id=c.organization_id AND artifact.id=c.artifact_id AND artifact.version=c.artifact_version
 JOIN `+q("tasks")+` t ON t.organization_id=c.organization_id AND t.id=c.task_id JOIN `+q("agents")+` g ON g.organization_id=t.organization_id AND g.id=t.agent_id AND g.version=t.agent_version JOIN `+q("model_turns")+` m ON m.organization_id=c.organization_id AND m.operation_id=c.operation_id WHERE c.source='model_tools'`)
	if err != nil {
		return recoverySnapshotError(ctx, ErrRecoveryExecutionHistory)
	}
	defer rows.Close()
	for rows.Next() {
		var content, transcript string
		var pinsRaw, proposalsRaw []byte
		if rows.Scan(&content, &pinsRaw, &proposalsRaw, &transcript) != nil {
			return ErrRecoveryExecutionHistory
		}
		var body modelToolCandidateBody
		var pins []domain.ModelToolDefinition
		var proposals []recoveryModelProposalBinding
		var messages []domain.ModelMessage
		if decodeRecoveryHistoryJSON([]byte(content), &body, 262144) != nil || decodeRecoveryHistoryJSON(pinsRaw, &pins, 262144) != nil || decodeRecoveryHistoryJSON(proposalsRaw, &proposals, 1<<20) != nil || decodeRecoveryHistoryJSON([]byte(transcript), &messages, domain.MaxModelTranscriptBytes) != nil || !optionalText(body.Text, 262144) || len(body.ToolCalls) < 1 || len(body.ToolCalls) > domain.MaxModelToolCalls || len(proposals) != len(body.ToolCalls) {
			return ErrRecoveryExecutionHistory
		}
		seen := map[string]bool{}
		for _, message := range messages {
			for _, call := range message.ToolCalls {
				seen[call.ID] = true
			}
		}
		for i, call := range body.ToolCalls {
			p := proposals[i]
			if !modelWireName.MatchString(call.ID) || seen[call.ID] || p.Ordinal != i+1 || p.CallID != call.ID || p.Name != call.Name || p.Arguments != call.ArgumentsJSON {
				return ErrRecoveryExecutionHistory
			}
			seen[call.ID] = true
			matched := false
			for _, pin := range pins {
				if pin.Name == call.Name && pin.ToolID == p.ToolID && pin.ToolVersion == p.ToolVersion && pin.ToolProfileDigest == p.ProfileDigest {
					connection := domain.ToolConnection{Tool: domain.ToolInfo{InputSchemaJSON: pin.InputSchemaJSON, MaxInputBytes: p.MaxInputBytes}}
					if _, err := toolruntime.CanonicalArguments(connection, call.ArgumentsJSON); err != nil {
						return ErrRecoveryExecutionHistory
					}
					matched = true
				}
			}
			if !matched {
				return ErrRecoveryExecutionHistory
			}
		}
	}
	if rows.Err() != nil {
		return recoverySnapshotError(ctx, ErrRecoveryExecutionHistory)
	}
	return nil
}

func recoverySameToolReceipt(a, b domain.ToolOutcomeReceipt) bool {
	a.CreatedAt = a.CreatedAt.UTC()
	b.CreatedAt = b.CreatedAt.UTC()
	return a == b
}
