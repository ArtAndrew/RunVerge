// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/domain"
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestRecoveryExecutionHistoryAuthorAcceptedDecision(t *testing.T) {
	recoveryInvariantOwned(t)
	f := independentCollaborationSetup(t)
	if _, err := f.s.ReviewTask(context.Background(), f.p, f.task.ID, domain.NewID("key"), f.review("accept")); err != nil {
		t.Fatal("accepted-result fixture failed", err)
	}
	tx := recoveryInvariantStage(t, f.s)
	recoveryExecutionWantValid(t, tx)
	recoveryExecutionFaults(t, tx, map[string]string{
		"accepted_decision_relabelled_reject":  `UPDATE pg_temp.task_decisions SET decision='reject'`,
		"accepted_decision_relabelled_changes": `UPDATE pg_temp.task_decisions SET decision='request_changes'`,
		"accepted_result_rewritten":            `UPDATE pg_temp.tasks SET result='Different accepted text' WHERE accepted_decision_id IS NOT NULL`,
	})
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	var snapshot bytes.Buffer
	if _, err := f.s.ExportRecoverySnapshot(context.Background(), &snapshot, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	target, spec, held := recoveryStageAuthorTarget(t, snapshot.Bytes())
	report, err := target.CheckRecoverySnapshotData(context.Background(), recoveryStageAuthorArchive(t, snapshot.Bytes()), spec, DefaultRecoverySnapshotLimits())
	if err != nil || !report.RollbackConfirmed || report.FullSemanticValidation {
		t.Fatal("valid accepted decision rejected", err)
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
	changed := recoveryStageAuthorRewrite(t, snapshot.Bytes(), func(stage *recoveryStageAuthorFixture) {
		decision := recoveryAuthorColumn(t, stage.header, "task_decisions", "decision")
		if len(stage.rows["task_decisions"]) != 1 {
			t.Fatal("expected exactly one explicit decision")
		}
		stage.rows["task_decisions"][0][decision] = []byte("reject")
	})
	if _, err := target.CheckRecoverySnapshotData(context.Background(), recoveryStageAuthorArchive(t, changed), spec, DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoveryExecutionHistory) {
		t.Fatal("authenticated rejected decision restored as accepted", err)
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
	if f.calls.Load() != 1 {
		t.Fatal("recovery validation repeated model request")
	}
}
