// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRecoveryTeamHistoryAuthorNestedRetry(t *testing.T) {
	recoveryInvariantOwned(t)
	source, _ := testStore(t)
	p, _ := identity(t, source, 1000000)
	leaf := agent(t, source, p)
	inner := teamAgent(t, source, p, "parallel", "all", 0, teamMember("left", leaf), teamMember("right", leaf))
	outer := teamAgent(t, source, p, "sequence", "all", 0, teamMember("nested", inner), teamMember("last", leaf))
	root := task(t, source, p, outer)
	root = driveFixtureTeam(t, source, p, root.ID)
	if root.CandidateID == "" {
		t.Fatal("nested fixture did not produce candidate")
	}
	tx := recoveryInvariantStage(t, source)
	if err := validateRecoveryTeamHistory(context.Background(), tx, "pg_temp"); err != nil {
		t.Fatal("valid nested team rejected", err)
	}
	t.Run("coherent_plan_instruction_forgery", func(t *testing.T) {
		if _, err := tx.Exec(context.Background(), "SAVEPOINT team_fault"); err != nil {
			t.Fatal(err)
		}
		defer tx.Exec(context.Background(), "ROLLBACK TO SAVEPOINT team_fault")
		var raw []byte
		if err := tx.QueryRow(context.Background(), "SELECT plan FROM pg_temp.team_executions WHERE task_id=$1", root.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var plan compiledTeamNode
		if err := json.Unmarshal(raw, &plan); err != nil {
			t.Fatal(err)
		}
		plan.Members[0].Task = "Forged historical member instruction"
		encoded, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(context.Background(), "UPDATE pg_temp.team_executions SET plan=$1,plan_digest=$2 WHERE task_id=$3", encoded, Hash(encoded), root.ID); err != nil {
			t.Fatal("fault violated row shape", err)
		}
		if !errors.Is(validateRecoveryTeamHistory(context.Background(), tx, "pg_temp"), ErrRecoveryTeamHistory) {
			t.Fatal("coherent plan forgery accepted")
		}
	})
	t.Run("changed_member_order", func(t *testing.T) {
		if _, err := tx.Exec(context.Background(), "SAVEPOINT order_fault"); err != nil {
			t.Fatal(err)
		}
		defer tx.Exec(context.Background(), "ROLLBACK TO SAVEPOINT order_fault")
		if _, err := tx.Exec(context.Background(), "UPDATE pg_temp.team_members SET ordinal=ordinal+10"); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(validateRecoveryTeamHistory(context.Background(), tx, "pg_temp"), ErrRecoveryTeamHistory) {
			t.Fatal("reordered slots accepted")
		}
	})
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	var selected domain.TaskCandidate
	for _, c := range mustTeamCandidates(t, source, p, root.ID) {
		if c.ID == root.CandidateID {
			selected = c
		}
	}
	if _, err := source.ReviewTask(context.Background(), p, root.ID, domain.NewID("key"), domain.ReviewTaskRequest{ExpectedVersion: root.Version, CandidateID: selected.ID, ArtifactID: selected.ArtifactID, ArtifactVersion: selected.ArtifactVersion, ContentDigest: selected.ContentDigest, Decision: "request_changes", Reason: "Explicit historical team retry"}); err != nil {
		t.Fatal(err)
	}
	root, err := source.GetTask(context.Background(), p, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.RetryTask(context.Background(), p, root.ID, domain.NewID("key"), domain.RetryTaskRequest{ExpectedVersion: root.Version, Reason: "New bounded generation", Feedback: "Use the same frozen plan", AcknowledgeNewCost: true}); err != nil {
		t.Fatal(err)
	}
	root = driveFixtureTeam(t, source, p, root.ID)
	if root.CandidateID == "" || root.AttemptCount != 2 {
		t.Fatal("retry fixture did not retain two generations")
	}
	tx = recoveryInvariantStage(t, source)
	if err = validateRecoveryTeamHistory(context.Background(), tx, "pg_temp"); err != nil {
		t.Fatal("retained historical generations rejected", err)
	}
	t.Run("wrong_nested_root_generation", func(t *testing.T) {
		if _, err := tx.Exec(context.Background(), "SAVEPOINT root_generation_fault"); err != nil {
			t.Fatal(err)
		}
		defer tx.Exec(context.Background(), "ROLLBACK TO SAVEPOINT root_generation_fault")
		if _, err := tx.Exec(context.Background(), `UPDATE pg_temp.team_executions e SET root_run_id=(SELECT parent.run_id FROM pg_temp.team_executions parent WHERE parent.organization_id=e.organization_id AND parent.task_id=e.root_id AND parent.run_id<>e.root_run_id LIMIT 1) WHERE e.task_id<>e.root_id`); err != nil {
			t.Fatal("root fault violated row/FK shape", err)
		}
		if !errors.Is(validateRecoveryTeamHistory(context.Background(), tx, "pg_temp"), ErrRecoveryTeamHistory) {
			t.Fatal("nested execution rebound to another generation")
		}
	})
	if err = tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range mustTeamCandidates(t, source, p, root.ID) {
		if c.ID == root.CandidateID {
			selected = c
		}
	}
	if _, err = source.ReviewTask(context.Background(), p, root.ID, domain.NewID("key"), domain.ReviewTaskRequest{ExpectedVersion: root.Version, CandidateID: selected.ID, ArtifactID: selected.ArtifactID, ArtifactVersion: selected.ArtifactVersion, ContentDigest: selected.ContentDigest, Decision: "accept", Reason: "Root human acceptance copied to descendants"}); err != nil {
		t.Fatal(err)
	}
	tx = recoveryInvariantStage(t, source)
	if err = validateRecoveryTeamHistory(context.Background(), tx, "pg_temp"); err != nil {
		t.Fatal("inherited nested acceptance rejected", err)
	}
	var inherited int
	if err = tx.QueryRow(context.Background(), "SELECT count(*) FROM pg_temp.task_decisions WHERE parent_decision_id IS NOT NULL").Scan(&inherited); err != nil || inherited < 1 {
		t.Fatal("inherited fixture absent", err)
	}
	t.Run("inherited_from_wrong_generation", func(t *testing.T) {
		if _, err := tx.Exec(context.Background(), "SAVEPOINT inherited_generation"); err != nil {
			t.Fatal(err)
		}
		defer tx.Exec(context.Background(), "ROLLBACK TO SAVEPOINT inherited_generation")
		if _, err := tx.Exec(context.Background(), `UPDATE pg_temp.task_decisions d SET parent_decision_id=old.id, decision=old.decision, reason=old.reason FROM pg_temp.task_decisions old WHERE d.parent_decision_id IS NOT NULL AND old.method='authorized_user_decision' AND old.decision='request_changes'`); err != nil {
			t.Fatal("generation fault violated row shape", err)
		}
		if !errors.Is(validateRecoveryTeamHistory(context.Background(), tx, "pg_temp"), ErrRecoveryTeamHistory) {
			t.Fatal("inherited decision rebound to unselected historical subtree")
		}
	})
	if _, err = tx.Exec(context.Background(), "UPDATE pg_temp.task_decisions SET reason='Different inherited human reason' WHERE parent_decision_id IS NOT NULL"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(validateRecoveryTeamHistory(context.Background(), tx, "pg_temp"), ErrRecoveryTeamHistory) {
		t.Fatal("inherited reason forgery accepted")
	}
	if err = tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	recoveryConfigurationObservedStage(t, source)
}

func TestRecoveryTeamHistoryAuthorLongDefinition(t *testing.T) {
	recoveryInvariantOwned(t)
	source, _ := testStore(t)
	p, _ := identity(t, source, 1000000)
	leaf := agent(t, source, p)
	var members []domain.TeamMember
	for i := 0; i < 9; i++ {
		m := teamMember(string(rune('a'+i)), leaf)
		m.Task = strings.Repeat("<", 4096)
		members = append(members, m)
	}
	coordinator := teamAgent(t, source, p, "sequence", "all", 0, members...)
	_ = task(t, source, p, coordinator)
	tx := recoveryInvariantStage(t, source)
	if err := validateRecoveryTeamHistory(context.Background(), tx, "pg_temp"); err != nil {
		t.Fatal("supported long team definition rejected", err)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	recoveryConfigurationObservedStage(t, source)
}
