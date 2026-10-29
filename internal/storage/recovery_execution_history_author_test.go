// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

func recoveryExecutionWantValid(t *testing.T, tx pgx.Tx) {
	t.Helper()
	for i, query := range recoveryExecutionHistoryQueries("pg_temp") {
		var ok bool
		if tx.QueryRow(context.Background(), query).Scan(&ok) != nil || !ok {
			t.Fatalf("valid historical bindings rejected at static rule %d", i)
		}
	}
	if err := validateRecoveryExecutionHistory(context.Background(), tx, "pg_temp"); err != nil {
		t.Fatal("valid execution history rejected", err)
	}
}

func recoveryExecutionFaults(t *testing.T, tx pgx.Tx, cases map[string]string) {
	t.Helper()
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if _, err := tx.Exec(ctx, "SAVEPOINT execution_fault"); err != nil {
				t.Fatal("savepoint failed")
			}
			defer tx.Exec(ctx, "ROLLBACK TO SAVEPOINT execution_fault")
			if _, err := tx.Exec(ctx, sql); err != nil {
				t.Fatal("test fault violated prerequisite row constraints")
			}
			if !errors.Is(validateRecoveryExecutionHistory(ctx, tx, "pg_temp"), ErrRecoveryExecutionHistory) {
				t.Fatal("forged historical binding accepted")
			}
		})
	}
}

func TestRecoveryExecutionHistoryAuthorFixture(t *testing.T) {
	recoveryInvariantOwned(t)
	s, _ := testStore(t)
	recoveryAuthorPopulate(t, s)
	tx := recoveryInvariantStage(t, s)
	recoveryExecutionWantValid(t, tx)
}

func TestRecoveryExecutionHistoryAuthorModel(t *testing.T) {
	recoveryInvariantOwned(t)
	f := independentModelSetup(t, true, nil)
	f.reserve(t)
	// Retiring a pinned profile cannot erase or invalidate recorded history.
	if _, err := f.s.Pool.Exec(context.Background(), "UPDATE model_connections SET active=false"); err != nil {
		t.Fatal("retired fixture failed")
	}
	tx := recoveryInvariantStage(t, f.s)
	recoveryExecutionWantValid(t, tx)
	recoveryExecutionFaults(t, tx, map[string]string{
		"changed_request_hash":        `UPDATE pg_temp.actions SET intent_hash=repeat('a',64)`,
		"changed_model_action_key":    `UPDATE pg_temp.actions SET idempotency_key='different-model-key'`,
		"rewritten_original_goal":     `UPDATE pg_temp.tasks SET goal='Different historical goal'`,
		"self_consistent_new_context": `UPDATE pg_temp.runs SET input_context='{"changed":true}',input_digest=encode(sha256(convert_to('{"changed":true}','UTF8')),'hex')`,
		"filled_legacy_default":       `UPDATE pg_temp.model_connections SET profile=profile||'{"accounting_mode":"metered_api"}'::jsonb`,
		"different_profile_endpoint":  `UPDATE pg_temp.model_connections SET profile=profile||'{"endpoint":"http://127.0.0.1:1/v1/chat/completions"}'::jsonb`,
	})
	if f.calls.Load() != 0 {
		t.Fatal("historical validation contacted a model")
	}
}

func TestRecoveryExecutionHistoryAuthorTool(t *testing.T) {
	recoveryInvariantOwned(t)
	f := independentToolSetup(t, false)
	f.prepare(t)
	tx := recoveryInvariantStage(t, f.s)
	recoveryExecutionWantValid(t, tx)
	recoveryExecutionFaults(t, tx, map[string]string{
		"changed_intent_arguments":      `UPDATE pg_temp.tool_executions SET intent=jsonb_set(jsonb_set(intent,'{arguments_json}','"{\"forged\":true}"'),' {arguments_digest}',to_jsonb(encode(sha256(convert_to('{"forged":true}','UTF8')),'hex')))`,
		"different_recorded_actor":      `UPDATE pg_temp.tool_executions SET intent=jsonb_set(intent,'{created_by}','"different-principal"')`,
		"missing_source_version":        `UPDATE pg_temp.tool_executions SET intent=intent-'source_task_version'`,
		"different_reservation_ceiling": `UPDATE pg_temp.tool_executions SET intent=jsonb_set(intent,'{max_charge_microusd}','"119"')`,
		"mutable_snapshot_status":       `UPDATE pg_temp.tool_executions SET intent=jsonb_set(intent,'{status}','"succeeded"')`,
		"mutable_snapshot_version":      `UPDATE pg_temp.tool_executions SET intent=jsonb_set(intent,'{version}','2')`,
		"mutable_snapshot_reason":       `UPDATE pg_temp.tool_executions SET intent=jsonb_set(intent,'{reason}','"invented original reason"')`,
		"mutable_snapshot_dispatcher":   `UPDATE pg_temp.tool_executions SET intent=jsonb_set(intent,'{dispatched_by}','"invented-dispatcher"')`,
		"mutable_snapshot_receipt":      `UPDATE pg_temp.tool_executions SET intent=jsonb_set(intent,'{result_receipt_id}','"invented-receipt"')`,
	})
}

func TestRecoveryExecutionHistoryAuthorDispatchedReceipt(t *testing.T) {
	recoveryInvariantOwned(t)
	f := independentToolSetup(t, false)
	_, lease := f.claimed(t)
	if _, err := f.s.PrepareToolDispatch(context.Background(), lease); err != nil {
		t.Fatal("recorded dispatch fixture failed")
	}
	if _, err := f.s.RecordToolOutcome(context.Background(), independentToolOutcome(lease, "succeeded")); err != nil {
		t.Fatal("recorded synthetic observation fixture failed")
	}
	tx := recoveryInvariantStage(t, f.s)
	recoveryExecutionWantValid(t, tx)
	recoveryExecutionFaults(t, tx, map[string]string{
		"mismatched_dispatch_generation": `UPDATE pg_temp.actions SET dispatch_fencing_token=dispatch_fencing_token+1 WHERE dispatch_issued_at IS NOT NULL`,
		"wrong_receipt_status":           `UPDATE pg_temp.tool_outcome_receipts SET outcome='rejected'`,
		"wrong_receipt_intent":           `UPDATE pg_temp.tool_outcome_receipts SET intent_hash=repeat('b',64)`,
		"missing_final_receipt":          `DELETE FROM pg_temp.tool_outcome_receipts; UPDATE pg_temp.tool_executions SET result_receipt_id=NULL`,
	})
}
