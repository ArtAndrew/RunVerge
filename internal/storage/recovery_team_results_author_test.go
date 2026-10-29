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

func TestRecoveryTeamHistoryAuthorSelectedInputs(t *testing.T) {
	recoveryInvariantOwned(t)
	s, _ := testStore(t)
	p, _ := identity(t, s, 1000000)
	leaf := agent(t, s, p)
	coordinator := teamAgent(t, s, p, "sequence", "all", 1, teamMember("first", leaf), teamMember("next", leaf))
	root := task(t, s, p, coordinator)
	root = driveFixtureTeam(t, s, p, root.ID)
	if root.CandidateID == "" {
		t.Fatal("sequence fixture has no manifest")
	}
	tx := recoveryInvariantStage(t, s)
	ctx := context.Background()
	if err := validateRecoveryTeamHistory(ctx, tx, "pg_temp"); err != nil {
		t.Fatal("valid selection rejected", err)
	}
	var childRun, childInput string
	if err := tx.QueryRow(ctx, `SELECT r.id,r.input_context FROM pg_temp.runs r JOIN pg_temp.team_members m ON m.organization_id=r.organization_id AND m.child_run_id=r.id WHERE m.parent_run_id=$1 AND m.ordinal=1`, root.CurrentRunID).Scan(&childRun, &childInput); err != nil {
		t.Fatal(err)
	}
	var input domain.RunInput
	if err := json.Unmarshal([]byte(childInput), &input); err != nil || len(input.Sources) != 1 {
		t.Fatal("dependency fixture absent", err)
	}
	for _, name := range []string{"coherent_feedback", "missing_predecessor", "changed_parent_context"} {
		t.Run(name, func(t *testing.T) {
			if _, err := tx.Exec(ctx, "SAVEPOINT input_fault"); err != nil {
				t.Fatal(err)
			}
			defer tx.Exec(ctx, "ROLLBACK TO SAVEPOINT input_fault")
			var changed domain.RunInput
			if err := json.Unmarshal([]byte(childInput), &changed); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "coherent_feedback":
				changed.Feedback = "Invented inter-agent feedback"
			case "missing_predecessor":
				changed.Sources = nil
			case "changed_parent_context":
				changed.ParentContext = json.RawMessage(`{"original_goal":"another parent","untrusted_sources":[]}`)
			}
			body, digest, err := BuildRunInput(changed)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(ctx, "UPDATE pg_temp.runs SET input_context=$1,input_digest=$2 WHERE id=$3", body, digest, childRun); err != nil {
				t.Fatal("fault violated row shape", err)
			}
			if !errors.Is(validateRecoveryTeamHistory(ctx, tx, "pg_temp"), ErrRecoveryTeamHistory) {
				t.Fatal("coherently rehashed input accepted")
			}
		})
	}
	t.Run("coherent_manifest_omission", func(t *testing.T) {
		if _, err := tx.Exec(ctx, "SAVEPOINT manifest_fault"); err != nil {
			t.Fatal(err)
		}
		defer tx.Exec(ctx, "ROLLBACK TO SAVEPOINT manifest_fault")
		var artifact, content string
		if err := tx.QueryRow(ctx, `SELECT a.id,a.content FROM pg_temp.artifacts a JOIN pg_temp.task_candidates c ON c.organization_id=a.organization_id AND c.artifact_id=a.id WHERE c.id=$1`, root.CandidateID).Scan(&artifact, &content); err != nil {
			t.Fatal(err)
		}
		var m teamManifest
		if err := json.Unmarshal([]byte(content), &m); err != nil {
			t.Fatal(err)
		}
		m.Members = m.Members[:1]
		body, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "UPDATE pg_temp.artifacts SET content=$1 WHERE id=$2", string(body), artifact); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "UPDATE pg_temp.task_candidates SET content_digest=$1 WHERE id=$2", Hash(body), root.CandidateID); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(validateRecoveryTeamHistory(ctx, tx, "pg_temp"), ErrRecoveryTeamHistory) {
			t.Fatal("rehashed omitted member accepted")
		}
	})
	t.Run("coherent_incomplete_all_join", func(t *testing.T) {
		if _, err := tx.Exec(ctx, "SAVEPOINT join_fault"); err != nil {
			t.Fatal(err)
		}
		defer tx.Exec(ctx, "ROLLBACK TO SAVEPOINT join_fault")
		var artifact, content string
		if err := tx.QueryRow(ctx, `SELECT a.id,a.content FROM pg_temp.artifacts a JOIN pg_temp.task_candidates c ON c.organization_id=a.organization_id AND c.artifact_id=a.id WHERE c.id=$1`, root.CandidateID).Scan(&artifact, &content); err != nil {
			t.Fatal(err)
		}
		var m teamManifest
		if err := json.Unmarshal([]byte(content), &m); err != nil {
			t.Fatal(err)
		}
		m.Complete = false
		m.Members[1].Status = "failed"
		m.Members[1].Result = nil
		body, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "UPDATE pg_temp.team_members SET status='failed',selected_candidate_id=NULL,selected_artifact_id=NULL,selected_artifact_version=NULL,selected_content_digest=NULL WHERE parent_run_id=$1 AND ordinal=1", root.CurrentRunID); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "UPDATE pg_temp.artifacts SET content=$1 WHERE id=$2", string(body), artifact); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "UPDATE pg_temp.task_candidates SET content_digest=$1 WHERE id=$2", Hash(body), root.CandidateID); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(validateRecoveryTeamHistory(ctx, tx, "pg_temp"), ErrRecoveryTeamHistory) {
			t.Fatal("incomplete all-join manifest accepted")
		}
	})
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var snapshot bytes.Buffer
	if _, err := s.ExportRecoverySnapshot(ctx, &snapshot, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	target, spec, held := recoveryStageAuthorTarget(t, snapshot.Bytes())
	report, err := target.CheckRecoverySnapshotData(ctx, recoveryStageAuthorArchive(t, snapshot.Bytes()), spec, DefaultRecoverySnapshotLimits())
	if err != nil || !report.RollbackConfirmed || report.FullSemanticValidation {
		t.Fatal("valid full stage failed", err)
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
	changed := recoveryStageAuthorRewrite(t, snapshot.Bytes(), func(f *recoveryStageAuthorFixture) {
		id := recoveryAuthorColumn(t, f.header, "runs", "id")
		body := recoveryAuthorColumn(t, f.header, "runs", "input_context")
		digest := recoveryAuthorColumn(t, f.header, "runs", "input_digest")
		found := false
		for _, row := range f.rows["runs"] {
			if string(row[id]) == childRun {
				var in domain.RunInput
				if err := json.Unmarshal(row[body], &in); err != nil {
					t.Fatal(err)
				}
				in.Sources = nil
				text, hash, err := BuildRunInput(in)
				if err != nil {
					t.Fatal(err)
				}
				row[body], row[digest] = []byte(text), []byte(hash)
				found = true
			}
		}
		if !found {
			t.Fatal("signed fault run missing")
		}
	})
	if _, err = target.CheckRecoverySnapshotData(ctx, recoveryStageAuthorArchive(t, changed), spec, DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoveryTeamHistory) {
		t.Fatal("authenticated coherent dependency forgery accepted", err)
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
}

func TestRecoveryTeamHistoryAuthorEditedReviewBasis(t *testing.T) {
	recoveryInvariantOwned(t)
	s, _ := testStore(t)
	p, _ := identity(t, s, 1000000)
	leaf := agent(t, s, p)
	inner := teamAgent(t, s, p, "parallel", "all", 1, teamMember("leaf", leaf))
	outer := teamAgent(t, s, p, "sequence", "all", 1, teamMember("inner", inner))
	root := task(t, s, p, outer)
	root = driveFixtureTeam(t, s, p, root.ID)
	m := independentMaterialCreate(t, s, p, root.ID, "A human-edited final document; this is deliberately not a team manifest.", root.CandidateID)
	raw, err := s.PublishMaterial(context.Background(), p, root.ID, m.ID, domain.NewID("key"), domain.PublishMaterial{ExpectedTaskVersion: root.Version, ExpectedMaterialVersion: m.HeadVersion, ContentDigest: m.HeadContentDigest, ParentCandidateID: root.CandidateID})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	var selected domain.TaskCandidate
	for _, c := range mustTeamCandidates(t, s, p, root.ID) {
		if c.ID == root.CandidateID {
			selected = c
		}
	}
	if selected.Source != "material" {
		t.Fatal("edited fixture not published")
	}
	if _, err = s.ReviewTask(context.Background(), p, root.ID, domain.NewID("key"), domain.ReviewTaskRequest{ExpectedVersion: root.Version, CandidateID: selected.ID, ArtifactID: selected.ArtifactID, ArtifactVersion: selected.ArtifactVersion, ContentDigest: selected.ContentDigest, Decision: "accept", Reason: "Accept edited document and original selected closure"}); err != nil {
		t.Fatal(err)
	}
	tx := recoveryInvariantStage(t, s)
	if err = validateRecoveryTeamHistory(context.Background(), tx, "pg_temp"); err != nil {
		t.Fatal("edited team basis rejected", err)
	}
	if err = tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	recoveryConfigurationObservedStage(t, s)
}

func TestRecoveryTeamHistoryAuthorPartialAndCancelled(t *testing.T) {
	recoveryInvariantOwned(t)
	s, _ := testStore(t)
	p, _ := identity(t, s, 1000000)
	leaf := agent(t, s, p)
	a := teamAgent(t, s, p, "parallel", "allow_partial", 2, teamMember("keep", leaf), teamMember("omit", leaf))
	root := task(t, s, p, a)
	ctx := context.Background()
	var omitted string
	if err := s.Pool.QueryRow(ctx, "SELECT child_task_id FROM team_members WHERE organization_id=$1 AND parent_run_id=$2 AND member_id='omit'", p.OrganizationID, root.CurrentRunID).Scan(&omitted); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelTask(ctx, p, omitted, domain.NewID("key")); err != nil {
		t.Fatal(err)
	}
	root = driveFixtureTeam(t, s, p, root.ID)
	if root.CandidateID == "" {
		t.Fatal("partial candidate missing")
	}
	tx := recoveryInvariantStage(t, s)
	if err := validateRecoveryTeamHistory(ctx, tx, "pg_temp"); err != nil {
		t.Fatal("valid partial result rejected", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelTask(ctx, p, root.ID, domain.NewID("key")); err != nil {
		t.Fatal(err)
	}
	tx = recoveryInvariantStage(t, s)
	if err := validateRecoveryTeamHistory(ctx, tx, "pg_temp"); err != nil {
		t.Fatal("cancelled execution lost its frozen result", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	recoveryConfigurationObservedStage(t, s)
}
