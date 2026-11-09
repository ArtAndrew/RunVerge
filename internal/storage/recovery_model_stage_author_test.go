// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

// Re-signing the archive and updating its transcript digest cannot make a
// forged quoted tool result consistent with the retained conversation chain.
func TestRecoveryModelStageAuthorRewrittenTranscript(t *testing.T) {
	recoveryInvariantOwned(t)
	f := independentNativeSetup(t, nil)
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal("synthetic model fixture failed")
	}
	proposal := f.proposal(t)
	view := f.decide(t, f.prepare(t, proposal), "deny")
	if _, err := f.s.ContinueModel(context.Background(), f.p, f.task.ID, domain.NewID("key"), independentNativeContinue(proposal, view)); err != nil {
		t.Fatal("synthetic continuation fixture failed", err)
	}
	var original bytes.Buffer
	if _, err := f.s.ExportRecoverySnapshot(context.Background(), &original, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal("source export failed", err)
	}
	target, spec, held := recoveryStageAuthorTarget(t, original.Bytes())
	valid := recoveryStageAuthorArchive(t, original.Bytes())
	report, err := target.CheckRecoverySnapshotData(context.Background(), valid, spec, DefaultRecoverySnapshotLimits())
	if err != nil || !report.RollbackConfirmed || report.FullSemanticValidation {
		t.Fatal("valid model history stage failed", err)
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
	changed := recoveryStageAuthorRewrite(t, original.Bytes(), func(stage *recoveryStageAuthorFixture) {
		textColumn := recoveryAuthorColumn(t, stage.header, "model_turns", "messages_json")
		digestColumn := recoveryAuthorColumn(t, stage.header, "model_turns", "messages_digest")
		ordinalColumn := recoveryAuthorColumn(t, stage.header, "model_turns", "ordinal")
		var row [][]byte
		for _, candidate := range stage.rows["model_turns"] {
			// PostgreSQL binary int4 for the queued second turn.
			if bytes.Equal(candidate[ordinalColumn], []byte{0, 0, 0, 2}) {
				row = candidate
			}
		}
		if row == nil {
			t.Fatal("queued continuation missing")
		}
		var messages []domain.ModelMessage
		if json.Unmarshal(row[textColumn], &messages) != nil || len(messages) != 4 {
			t.Fatal("unexpected source transcript")
		}
		var quote modelQuotedToolResult
		if json.Unmarshal([]byte(messages[3].Content), &quote) != nil {
			t.Fatal("tool quote missing")
		}
		quote.Untrusted = false
		quoted, _ := json.Marshal(quote)
		messages[3].Content = string(quoted)
		raw, err := json.Marshal(messages)
		if err != nil {
			t.Fatal("encoding test transcript failed")
		}
		row[textColumn], row[digestColumn] = raw, []byte(Hash(raw))
		// This queued turn has not reserved a request yet. Existing intent
		// validation does not rebuild its full quoted conversation.
		tx := recoveryInvariantStage(t, f.s)
		if _, err := tx.Exec(context.Background(), "UPDATE pg_temp.model_turns SET messages_json=$1,messages_digest=$2 WHERE ordinal=2", string(raw), Hash(raw)); err != nil {
			t.Fatal("temporary forgery failed", err)
		}
		if err := validateRecoveryExecutionHistory(context.Background(), tx, "pg_temp"); err != nil {
			t.Fatal("regression already rejected by prior checks", err)
		}
		if err := tx.Rollback(context.Background()); err != nil {
			t.Fatal("temporary prior-validator probe rollback failed", err)
		}

	})
	forged := recoveryStageAuthorArchive(t, changed)
	if _, err := target.CheckRecoverySnapshotData(context.Background(), forged, spec, DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoveryExecutionHistory) {
		t.Fatal("re-signed rewritten transcript was not rejected", err)
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
	if f.modelCalls.Load() != 1 || f.toolCalls.Load() != 0 {
		t.Fatal("recovery validation dispatched external work")
	}
}
