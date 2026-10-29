// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

func TestRecoveryHistoryCoverageAuthorBoundary(t *testing.T) {
	if _, err := checkRecoveryHistoryCoverage(nil, nil, "pg_temp"); !errors.Is(err, ErrRecoveryHistoryCoverage) {
		t.Fatal("nil boundary not rejected")
	}
	literal := `{"version":1,"organization_id":"org-A","table":"idempotency_keys","keys":["principal","POST /tasks","a:b"]}`
	got := recoveryHistoryRecordSHA256("org-A", "idempotency_keys", []string{"principal", "POST /tasks", "a:b"})
	if got != Hash([]byte(literal)) {
		t.Fatal("versioned record reference changed")
	}
	for _, other := range []string{
		recoveryHistoryRecordSHA256("org-B", "idempotency_keys", []string{"principal", "POST /tasks", "a:b"}),
		recoveryHistoryRecordSHA256("org-A", "another_table", []string{"principal", "POST /tasks", "a:b"}),
		recoveryHistoryRecordSHA256("org-A", "idempotency_keys", []string{"principal", "POST /tasks:a", "b"}),
	} {
		if other == got {
			t.Fatal("record reference lost tenant/table/key boundaries")
		}
	}
}

func TestRecoveryHistoryCoverageAuthorStageAndLimits(t *testing.T) {
	recoveryInvariantOwned(t)
	s, _ := testStore(t)
	p, a, _ := recoveryAuthorPopulate(t, s)
	other, _ := identity(t, s, 1000000)
	root := task(t, s, p, a)
	coordinator := teamAgent(t, s, p, "parallel", "all", 1, teamMember("one", a))
	_ = task(t, s, p, coordinator)
	ctx := context.Background()
	// Retained pre-014 fixture: the current API correctly cannot create unpinned
	// tools. No historical payload is repaired or resolved against a live tool.
	const privateText = "synthetic-private-document-not-for-report"
	for _, org := range []string{p.OrganizationID, other.OrganizationID} {
		if _, err := s.Pool.Exec(ctx, `INSERT INTO agents(organization_id,id,version,name,instructions,model,tools,budget_microusd,published) VALUES($1,'agent-legacy',1,$2,$2,'legacy-model','["private-tool-name"]',1000,true)`, org, privateText); err != nil {
			t.Fatal("legacy fixture failed", err)
		}
	}
	const rawKey = "synthetic-private-idempotency-key"
	for _, principal := range []domain.Principal{p, other} {
		if _, err := s.Pool.Exec(ctx, `INSERT INTO idempotency_keys(organization_id,principal_id,scope,key,request_hash,response) VALUES($1,$2,'POST /private-scope',$3,$4,$5)`, principal.OrganizationID, principal.ID, rawKey, Hash([]byte("unverifiable historical request")), `{"private_response":"`+privateText+`"}`); err != nil {
			t.Fatal("cached fixture failed", err)
		}
	}
	var sequence int64
	if err := s.Pool.QueryRow(ctx, `INSERT INTO events(organization_id,id,task_id,type,data,payload_version) VALUES($1,$2,$3,'legacy-untyped-example',$4,0) RETURNING sequence`, p.OrganizationID, domain.NewID("evt"), root.ID, `{"private":"`+privateText+`"}`).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceEventReplayWindow(ctx, p.OrganizationID, root.ID, sequence, "Synthetic retention boundary"); err != nil {
		t.Fatal(err)
	}
	tx := recoveryInvariantStage(t, s)
	first, err := checkRecoveryHistoryCoverage(ctx, tx, "pg_temp")
	if err != nil {
		t.Fatal(err)
	}
	second, err := checkRecoveryHistoryCoverage(ctx, tx, "pg_temp")
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("classification is incomplete or nondeterministic", err)
	}
	if first.ClassificationVersion != 1 || first.Scope != "identified_unverified_history" || len(first.AffectedOrganizations) != 2 {
		t.Fatal("affected organization inventory missing")
	}
	counts := map[string]int64{}
	seen := map[string]bool{}
	legacy, untyped, modern := 0, 0, 0
	for _, r := range first.Records {
		if !validDigest(r.RecordSHA256) || seen[r.RecordSHA256] || r.RequiredDisposition == "" {
			t.Fatal("invalid or duplicate record reference")
		}
		seen[r.RecordSHA256] = true
		counts[r.Table]++
		switch r.Reason {
		case "unpinned_tool_metadata":
			legacy++
		case "legacy_untyped_event":
			untyped++
		case "event_provenance_not_reconstructed":
			modern++
		}
		if r.Table == "idempotency_keys" && r.RequiredDisposition != "never_replay_saved_response" {
			t.Fatal("cached response became replayable")
		}
	}
	if legacy != 2 || untyped != 1 || modern == 0 {
		t.Fatal("old and current retained history not distinguished")
	}
	// Count directly from the owned source, independent of the classifier's rule
	// builder; a successful partial/truncated report is not acceptable.
	for _, table := range []string{"events", "outbox", "task_event_windows", "event_replay_window_audit", "idempotency_keys", "tasks", "runs", "operations", "team_executions", "team_members"} {
		var expected int64
		if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&expected); err != nil || expected != counts[table] {
			t.Fatal("table coverage count differs", table, err)
		}
	}
	var summed int64
	for _, summary := range first.Summaries {
		summed += summary.Records
	}
	if summed != int64(len(first.Records)) {
		t.Fatal("summary lost records")
	}
	for _, org := range []string{p.OrganizationID, other.OrganizationID} {
		if !seen[recoveryHistoryRecordSHA256(org, "agents", []string{"agent-legacy", "1"})] {
			t.Fatal("legacy agent reference not actionable")
		}
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{privateText, rawKey, "POST /private-scope", "private-tool-name", "private_response", "legacy-model"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatal("classification exposed row content or a raw key")
		}
	}
	for name, limits := range map[string]recoveryHistoryCoverageLimits{
		"record_cap": {1, recoveryHistoryCoverageMaxBytes}, "byte_cap": {recoveryHistoryCoverageMaxRecords, 1},
	} {
		t.Run(name, func(t *testing.T) {
			partial, err := checkRecoveryHistoryCoverageLimits(ctx, tx, "pg_temp", limits)
			if !errors.Is(err, ErrRecoveryHistoryCoverageLimit) || !reflect.DeepEqual(partial, RecoveryHistoryCoverageReport{}) {
				t.Fatal("limit silently returned a partial success", err)
			}
		})
	}
	t.Run("oversize_key", func(t *testing.T) {
		if _, err := tx.Exec(ctx, "SAVEPOINT key_size"); err != nil {
			t.Fatal(err)
		}
		defer tx.Exec(ctx, "ROLLBACK TO SAVEPOINT key_size")
		if _, err := tx.Exec(ctx, "UPDATE pg_temp.idempotency_keys SET scope=$1 WHERE key=$2", strings.Repeat("x", 65537), rawKey); err != nil {
			t.Fatal(err)
		}
		if _, err := checkRecoveryHistoryCoverage(ctx, tx, "pg_temp"); !errors.Is(err, ErrRecoveryHistoryCoverageLimit) {
			t.Fatal("oversized classification key accepted", err)
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
		t.Fatal("classified signed stage failed or overclaimed", err)
	}
	if !reflect.DeepEqual(report.HistoryCoverage, first) {
		t.Fatal("aggregate stage discarded or changed classification")
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
}
