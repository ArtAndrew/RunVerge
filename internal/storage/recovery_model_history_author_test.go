// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

func TestRecoveryModelHistoryAuthorContinuation(t *testing.T) {
	recoveryInvariantOwned(t)
	for _, observed := range []bool{false, true} {
		name := "known_denial"
		if observed {
			name = "recorded_synthetic_response"
		}
		t.Run(name, func(t *testing.T) {
			f := independentNativeSetup(t, nil)
			if _, err := f.executeModel(t, f.s); err != nil {
				t.Fatal("synthetic model fixture failed")
			}
			proposal := f.proposal(t)
			prepared := f.prepare(t, proposal)
			var view domain.ToolExecutionView
			if observed {
				view = f.dispatchAndObserve(t, prepared)
			} else {
				view = f.decide(t, prepared, "deny")
			}
			if _, err := f.s.ContinueModel(context.Background(), f.p, f.task.ID, domain.NewID("key"), independentNativeContinue(proposal, view)); err != nil {
				t.Fatal("synthetic continuation fixture failed")
			}
			tx := recoveryInvariantStage(t, f.s)
			if err := validateRecoveryModelHistory(context.Background(), tx, "pg_temp"); err != nil {
				t.Fatal("valid quoted conversation rejected", err)
			}
			var raw string
			if tx.QueryRow(context.Background(), "SELECT messages_json FROM pg_temp.model_turns WHERE ordinal=2").Scan(&raw) != nil {
				t.Fatal("continuation missing")
			}
			cases := []string{"changed_prefix", "promoted_system_role", "trusted_tool_claim", "different_proposal", "different_result"}
			for _, fault := range cases {
				t.Run(fault, func(t *testing.T) {
					var messages []domain.ModelMessage
					if json.Unmarshal([]byte(raw), &messages) != nil || len(messages) != 4 {
						t.Fatal("fixture transcript shape")
					}
					switch fault {
					case "changed_prefix":
						messages[0].Content = "Different original instruction"
					case "promoted_system_role":
						messages[2].Role = "system"
					default:
						var quoted modelQuotedToolResult
						if json.Unmarshal([]byte(messages[3].Content), &quoted) != nil {
							t.Fatal("fixture quote missing")
						}
						switch fault {
						case "trusted_tool_claim":
							quoted.Untrusted = false
						case "different_proposal":
							quoted.ProposalID = "different-proposal"
						case "different_result":
							quoted.Result = "Different claimed result"
						}
						encoded, _ := json.Marshal(quoted)
						messages[3].Content = string(encoded)
					}
					encoded, _ := json.Marshal(messages)
					if _, err := tx.Exec(context.Background(), "SAVEPOINT model_history_fault"); err != nil {
						t.Fatal("savepoint failed")
					}
					defer tx.Exec(context.Background(), "ROLLBACK TO SAVEPOINT model_history_fault")
					if _, err := tx.Exec(context.Background(), "UPDATE pg_temp.model_turns SET messages_json=$1,messages_digest=$2 WHERE ordinal=2", string(encoded), Hash(encoded)); err != nil {
						t.Fatal("forgery violated prerequisite row CHECK")
					}
					if !errors.Is(validateRecoveryModelHistory(context.Background(), tx, "pg_temp"), ErrRecoveryExecutionHistory) {
						t.Fatal("coherent transcript forgery accepted")
					}
				})
			}
			if f.modelCalls.Load() != 1 {
				t.Fatal("validator invoked model")
			}
			want := int64(0)
			if observed {
				want = 1
			}
			if f.toolCalls.Load() != want {
				t.Fatal("validator invoked tool")
			}
		})
	}
}
