// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRecoveryExecutionHistoryAuthorResultAndAgentProjection(t *testing.T) {
	recoveryInvariantOwned(t)
	f := independentCollaborationSetup(t)
	alternate := agent(t, f.s, f.p)
	ctx := context.Background()
	tx := recoveryInvariantStage(t, f.s)
	recoveryExecutionWantValid(t, tx)
	recoveryExecutionFaults(t, tx, map[string]string{
		"model_receipt_digest_only": `UPDATE pg_temp.model_candidates SET content_digest=repeat('0',64)`,
		"missing_model_receipt":     `DELETE FROM pg_temp.model_candidates`,
	})
	t.Run("existing_wrong_run_agent", func(t *testing.T) {
		if _, err := tx.Exec(ctx, "SAVEPOINT wrong_agent"); err != nil {
			t.Fatal(err)
		}
		defer tx.Exec(ctx, "ROLLBACK TO SAVEPOINT wrong_agent")
		if _, err := tx.Exec(ctx, "UPDATE pg_temp.runs SET agent_id=$1,agent_version=$2", alternate.ID, alternate.Version); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(validateRecoveryExecutionHistory(ctx, tx, "pg_temp"), ErrRecoveryExecutionHistory) {
			t.Fatal("FK-valid agent inconsistent with task pin accepted")
		}
	})
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var snapshot bytes.Buffer
	if _, err := f.s.ExportRecoverySnapshot(ctx, &snapshot, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	target, spec, held := recoveryStageAuthorTarget(t, snapshot.Bytes())
	report, err := target.CheckRecoverySnapshotData(ctx, recoveryStageAuthorArchive(t, snapshot.Bytes()), spec, DefaultRecoverySnapshotLimits())
	if err != nil || !report.RollbackConfirmed || report.FullSemanticValidation {
		t.Fatal("valid model projection rejected", err)
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
	for _, kind := range []string{"receipt_digest", "run_agent"} {
		t.Run("signed_"+kind, func(t *testing.T) {
			changed := recoveryStageAuthorRewrite(t, snapshot.Bytes(), func(fixture *recoveryStageAuthorFixture) {
				if kind == "receipt_digest" {
					column := recoveryAuthorColumn(t, fixture.header, "model_candidates", "content_digest")
					if len(fixture.rows["model_candidates"]) != 1 {
						t.Fatal("model receipt fixture absent")
					}
					fixture.rows["model_candidates"][0][column] = []byte(strings.Repeat("0", 64))
				} else {
					column := recoveryAuthorColumn(t, fixture.header, "runs", "agent_id")
					version := recoveryAuthorColumn(t, fixture.header, "runs", "agent_version")
					if len(fixture.rows["runs"]) != 1 || alternate.Version != 1 {
						t.Fatal("run-agent fixture shape changed")
					}
					fixture.rows["runs"][0][column] = []byte(alternate.ID)
					fixture.rows["runs"][0][version] = []byte{0, 0, 0, 1}
				}
			})
			if _, err := target.CheckRecoverySnapshotData(ctx, recoveryStageAuthorArchive(t, changed), spec, DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoveryExecutionHistory) {
				t.Fatal("authenticated projection forgery accepted", err)
			}
			recoveryStageAuthorAssertUntouched(t, target, held)
		})
	}
	if f.calls.Load() != 1 {
		t.Fatal("recovery repeated the loopback model request")
	}
}

func TestRecoveryTeamHistoryAuthorArtifactProjection(t *testing.T) {
	recoveryInvariantOwned(t)
	s, _ := testStore(t)
	p, _ := identity(t, s, 1000000)
	leaf := agent(t, s, p)
	a := teamAgent(t, s, p, "parallel", "all", 1, teamMember("leaf", leaf))
	root := task(t, s, p, a)
	root = driveFixtureTeam(t, s, p, root.ID)
	if root.CandidateID == "" {
		t.Fatal("team candidate fixture absent")
	}
	ctx := context.Background()
	tx := recoveryInvariantStage(t, s)
	if err := validateRecoveryTeamHistory(ctx, tx, "pg_temp"); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]string{
		"verified_true":    `provenance=jsonb_set(provenance,'{verified}','true')`,
		"verified_null":    `provenance=jsonb_set(provenance,'{verified}','null')`,
		"missing_verified": `provenance=provenance-'verified'`,
		"wrong_run":        `provenance=jsonb_set(provenance,'{run_id}','"run_wrong"')`,
		"wrong_plan":       `provenance=jsonb_set(provenance,'{plan_digest}',to_jsonb(repeat('0',64)))`,
		"wrong_name":       `name='trusted-result.txt'`,
		"wrong_media_type": `media_type='text/plain'`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := tx.Exec(ctx, "SAVEPOINT provenance_fault"); err != nil {
				t.Fatal(err)
			}
			defer tx.Exec(ctx, "ROLLBACK TO SAVEPOINT provenance_fault")
			if _, err := tx.Exec(ctx, "UPDATE pg_temp.artifacts SET "+change+" WHERE task_id=$1", root.ID); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(validateRecoveryTeamHistory(ctx, tx, "pg_temp"), ErrRecoveryTeamHistory) {
				t.Fatal("false team artifact projection accepted")
			}
		})
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var snapshot bytes.Buffer
	if _, err := s.ExportRecoverySnapshot(ctx, &snapshot, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	target, spec, held := recoveryStageAuthorTarget(t, snapshot.Bytes())
	changed := recoveryStageAuthorRewrite(t, snapshot.Bytes(), func(fixture *recoveryStageAuthorFixture) {
		taskColumn := recoveryAuthorColumn(t, fixture.header, "artifacts", "task_id")
		column := recoveryAuthorColumn(t, fixture.header, "artifacts", "provenance")
		found := false
		for _, row := range fixture.rows["artifacts"] {
			if string(row[taskColumn]) == root.ID {
				if bytes.Count(row[column], []byte(`"verified": false`)) != 1 {
					t.Fatal("canonical false provenance field absent")
				}
				row[column] = bytes.Replace(row[column], []byte(`"verified": false`), []byte(`"verified": true`), 1)
				found = true
			}
		}
		if !found {
			t.Fatal("team artifact row missing")
		}
	})
	if _, err := target.CheckRecoverySnapshotData(ctx, recoveryStageAuthorArchive(t, changed), spec, DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoveryTeamHistory) {
		t.Fatal("authenticated false verification accepted", err)
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
}

func TestRecoveryModelHistoryAuthorPendingProposalBinding(t *testing.T) {
	recoveryInvariantOwned(t)
	f := independentNativeSetup(t, nil)
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	proposal := f.proposal(t)
	ctx := context.Background()
	tx := recoveryInvariantStage(t, f.s)
	if err := validateRecoveryModelHistory(ctx, tx, "pg_temp"); err != nil {
		t.Fatal("valid pending proposal rejected", err)
	}
	for name, change := range map[string]string{
		"name":             `UPDATE pg_temp.model_tool_proposals SET tool_name='unrelated_tool'`,
		"call_id":          `UPDATE pg_temp.model_tool_proposals SET call_id='other_call_id'`,
		"arguments":        `UPDATE pg_temp.model_tool_proposals SET arguments_json='{"changed":true}',arguments_digest=encode(sha256(convert_to('{"changed":true}','UTF8')),'hex')`,
		"missing_proposal": `DELETE FROM pg_temp.model_tool_proposals`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := tx.Exec(ctx, "SAVEPOINT pending_proposal"); err != nil {
				t.Fatal(err)
			}
			defer tx.Exec(ctx, "ROLLBACK TO SAVEPOINT pending_proposal")
			if _, err := tx.Exec(ctx, change); err != nil {
				t.Fatal("pending fault violated row shape", err)
			}
			if !errors.Is(validateRecoveryModelHistory(ctx, tx, "pg_temp"), ErrRecoveryExecutionHistory) {
				t.Fatal("pending proposal disagrees with quoted candidate")
			}
		})
	}
	t.Run("coherent_invalid_arguments", func(t *testing.T) {
		if _, err := tx.Exec(ctx, "SAVEPOINT coherent_arguments"); err != nil {
			t.Fatal(err)
		}
		defer tx.Exec(ctx, "ROLLBACK TO SAVEPOINT coherent_arguments")
		var content string
		if err := tx.QueryRow(ctx, "SELECT content FROM pg_temp.artifacts WHERE id=$1", proposal.ArtifactID).Scan(&content); err != nil {
			t.Fatal(err)
		}
		var body modelToolCandidateBody
		if err := json.Unmarshal([]byte(content), &body); err != nil || len(body.ToolCalls) != 1 {
			t.Fatal("pending body absent", err)
		}
		body.ToolCalls[0].ArgumentsJSON = `{"changed":true}`
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "UPDATE pg_temp.artifacts SET content=$1,provenance=jsonb_set(provenance,'{content_digest}',to_jsonb($2::text)) WHERE id=$3", string(raw), Hash(raw), proposal.ArtifactID); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "UPDATE pg_temp.task_candidates SET content_digest=$1 WHERE id=$2", Hash(raw), proposal.CandidateID); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "UPDATE pg_temp.model_candidates SET content_digest=$1 WHERE artifact_id=$2", Hash(raw), proposal.ArtifactID); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "UPDATE pg_temp.model_tool_proposals SET arguments_json=$1,arguments_digest=$2,content_digest=$3 WHERE id=$4", body.ToolCalls[0].ArgumentsJSON, Hash([]byte(body.ToolCalls[0].ArgumentsJSON)), Hash(raw), proposal.ID); err != nil {
			t.Fatal(err)
		}
		recoveryExecutionWantValid(t, tx)
		if !errors.Is(validateRecoveryModelHistory(ctx, tx, "pg_temp"), ErrRecoveryExecutionHistory) {
			t.Fatal("coherent proposal with invalid pinned input schema accepted")
		}
	})

	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var snapshot bytes.Buffer
	if _, err := f.s.ExportRecoverySnapshot(ctx, &snapshot, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	target, spec, held := recoveryStageAuthorTarget(t, snapshot.Bytes())
	report, err := target.CheckRecoverySnapshotData(ctx, recoveryStageAuthorArchive(t, snapshot.Bytes()), spec, DefaultRecoverySnapshotLimits())
	if err != nil || !report.RollbackConfirmed || report.FullSemanticValidation {
		t.Fatal("valid pending archive rejected", err)
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
	changed := recoveryStageAuthorRewrite(t, snapshot.Bytes(), func(fixture *recoveryStageAuthorFixture) {
		id := recoveryAuthorColumn(t, fixture.header, "model_tool_proposals", "id")
		column := recoveryAuthorColumn(t, fixture.header, "model_tool_proposals", "tool_name")
		found := false
		for _, row := range fixture.rows["model_tool_proposals"] {
			if string(row[id]) == proposal.ID {
				row[column] = []byte("unrelated_tool")
				found = true
			}
		}
		if !found {
			t.Fatal("signed pending fixture missing")
		}
	})
	if _, err := target.CheckRecoverySnapshotData(ctx, recoveryStageAuthorArchive(t, changed), spec, DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoveryExecutionHistory) {
		t.Fatal("authenticated pending proposal mismatch accepted", err)
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
	if f.modelCalls.Load() != 1 || f.toolCalls.Load() != 0 {
		t.Fatal("recovery executed a pending tool or repeated model")
	}
}
