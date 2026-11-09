// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelgateway"
)

func TestRecoveryAccountingAuthorReviewWindowComparison(t *testing.T) {
	base := RecoveryExecutionWindowPosition{Kind: "model", Status: "unknown", TaskID: "task", OperationID: "operation", RelatedIDs: []string{"reservation"}, IdentitySHA256: strings.Repeat("a", 64), RowSHA256: strings.Repeat("b", 64)}
	original, captured := recoveryWindowSnapshot{}, recoveryWindowSnapshot{}
	want := map[string]string{"new": "captured_only", "missing": "missing_from_capture", "identity": "identity_changed", "removed": "dispatch_record_changed", "changed": "dispatch_record_changed", "late": "dispatch_recorded_after_backup", "known": "dispatch_recorded_in_both", "absent": "no_dispatch_record"}
	for id, kind := range want {
		a, b := base, base
		switch kind {
		case "identity_changed":
			b.IdentitySHA256 = strings.Repeat("c", 64)
		case "dispatch_record_changed":
			a.DispatchRecorded = true
			a.DispatchSHA256 = strings.Repeat("c", 64)
			if id == "changed" {
				b.DispatchRecorded = true
				b.DispatchSHA256 = strings.Repeat("d", 64)
			}
		case "dispatch_recorded_after_backup":
			b.DispatchRecorded = true
			b.DispatchSHA256 = strings.Repeat("c", 64)
		case "dispatch_recorded_in_both":
			a.DispatchRecorded = true
			a.DispatchSHA256 = strings.Repeat("c", 64)
			b = a
		}
		key := recoveryWindowKey{"action", "org", id}
		if kind != "captured_only" {
			original[key] = a
		}
		if kind != "missing_from_capture" {
			captured[key] = b
		}
	}
	// Same ID in another tenant and another family is a separate record. Terminal
	// state must not hide a captured-only record or manufacture a dispatch marker.
	terminal := base
	terminal.Status = "settled"
	captured[recoveryWindowKey{"action", "other-org", "new"}] = terminal
	captured[recoveryWindowKey{"reservation_without_action", "org", "new"}] = terminal
	r := compareRecoveryExecutionWindow(original, captured)
	if r.CoverageProven || r.SourceStopRechecked || r.NewWorkAllowed || len(r.Families) != 5 || len(r.Records) != 10 {
		t.Fatal("inventory implies clearance or lost records")
	}
	for _, e := range r.Records {
		if e.Comparison != want[e.RecordID] {
			t.Fatal("incorrect comparison", e.RecordID, e.Comparison)
		}
	}
	if !reflect.DeepEqual(r, compareRecoveryExecutionWindow(original, captured)) {
		t.Fatal("unstable inventory order")
	}
	for _, e := range r.Records {
		if e.Captured != nil {
			e.Captured.RelatedIDs[0] = "changed"
		}
	}
	if captured[recoveryWindowKey{"action", "org", "new"}].RelatedIDs[0] != "reservation" {
		t.Fatal("report changed input")
	}
}

func TestRecoveryAccountingAuthorReviewWindowFromRealModelAdmission(t *testing.T) {
	s, _ := recoveryAuthorStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 100000)
	c := domain.ModelConnection{ID: "window-model", OrganizationID: p.OrganizationID, Version: 1, WireProfile: domain.ModelWireOpenRouterTextV1, Model: "example/window", ProviderAccount: "synthetic-window", PriceVersion: "synthetic-v1", Endpoint: modelgateway.OpenRouterEndpoint, CredentialEnvironment: "PLATFORM_MODEL_KEY_WINDOW_TEST", MaxChargeMicroUSD: 37, MaxInputTokens: 32, InputMicroUSDPerMillion: 1000000, MaxOutputTokens: 16, MaxInputBytes: 4096, MaxOutputBytes: 4096, TimeoutMilliseconds: 2000, TokenLimitField: "max_tokens", BoundAssumptions: "Owned history only; no network request", Active: true}
	var err error
	c, err = s.InstallModelConnection(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	var original, late bytes.Buffer
	r, complete := recoveryUsageAuthorAttemptBeforeDispatch(t, s, p, c, "gen-window-original", func() {
		if _, err := s.ExportRecoverySnapshot(ctx, &original, DefaultRecoverySnapshotLimits()); err != nil {
			t.Fatal(err)
		}
	})
	complete()
	later, completeLater := recoveryUsageAuthorAttempt(t, s, p, c, "gen-window-later")
	completeLater()
	if _, err = s.ExportRecoverySnapshot(ctx, &late, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	a, b := recoveryStageAuthorArchive(t, original.Bytes()), recoveryStageAuthorArchive(t, late.Bytes())
	target, spec, _ := recoveryStageAuthorTarget(t, original.Bytes())
	if _, err = target.ImportRecoverySnapshot(ctx, a, spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	link := evidenceAuthorPair(t, a, b)
	if _, err = target.IngestRecoveryEvidence(ctx, a, b, link, spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	review, err := target.ReviewRecoveryAccounting(ctx, a, b, link, spec, DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal(err)
	}
	if review.Version != 2 || review.Algorithm != "held-accounting-execution-review-v2" || !review.ExecutionHeld || !review.RollbackConfirmed || review.ExecutionWindow.CoverageProven || review.ExecutionWindow.NewWorkAllowed {
		t.Fatal("review profile changed authority")
	}
	seen := map[string]RecoveryExecutionWindowRecord{}
	for _, e := range review.ExecutionWindow.Records {
		if e.Family == "action" {
			seen[e.RecordID] = e
		}
	}
	first, second := seen[r.ActionID], seen[later.ActionID]
	if first.Comparison != "dispatch_recorded_after_backup" || first.Original == nil || first.Original.DispatchRecorded || first.Captured == nil || !first.Captured.DispatchRecorded || first.Original.IdentitySHA256 != first.Captured.IdentitySHA256 || !reflect.DeepEqual(first.Captured.RelatedIDs, []string{r.ID}) {
		t.Fatal("post-backup admission lost its original reservation")
	}
	if second.Comparison != "captured_only" || second.Original != nil || second.Captured == nil || !second.Captured.DispatchRecorded {
		t.Fatal("late-only model admission hidden")
	}
	var status string
	var amount int64
	if target.Pool.QueryRow(ctx, "SELECT status,amount_microusd FROM reservations WHERE organization_id=$1 AND id=$2", p.OrganizationID, r.ID).Scan(&status, &amount) != nil || status != "reserved" || amount != 37 {
		t.Fatal("inventory changed original reservation")
	}
	observer, err := Open(ctx, target.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	repeat, err := observer.ReviewRecoveryAccounting(ctx, a, b, link, spec, DefaultRecoverySnapshotLimits())
	if err != nil || !reflect.DeepEqual(review, repeat) {
		t.Fatal("fresh process changed inventory", err)
	}
	// Independently recalculate the outer digest including the new inventory.
	digest := review.ReviewSHA256
	review.ReviewSHA256 = ""
	raw, _ := json.Marshal(review)
	if Hash(raw) != digest {
		t.Fatal("inventory omitted from review commitment")
	}
}

// Projection checks use ordinary admission APIs in owned schemas. They never
// execute a tool, discover an MCP server or send a webhook.
func TestRecoveryAccountingAuthorReviewWindowNonModelAdmissions(t *testing.T) {
	if os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" {
		t.Skip("new owned PostgreSQL test cluster required")
	}
	ctx := context.Background()
	read := func(t *testing.T, s *Store) recoveryWindowSnapshot {
		t.Helper()
		tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, "SET LOCAL search_path=pg_catalog,pg_temp; SET LOCAL timezone='UTC'"); err != nil {
			t.Fatal(err)
		}
		result, err := readRecoveryExecutionWindow(ctx, tx, s.runtimeSchema, &recoveryReviewWork{})
		if err != nil {
			t.Fatal(err)
		}
		exhausted, err := readRecoveryExecutionWindow(ctx, tx, s.runtimeSchema, &recoveryReviewWork{rows: 3 * recoveryPlanMaxItems})
		if err == nil || exhausted != nil {
			t.Fatal("exhausted inventory returned a prefix")
		}
		return result
	}
	check := func(t *testing.T, before, after recoveryWindowSnapshot, key recoveryWindowKey) {
		t.Helper()
		a, ok := before[key]
		b, exists := after[key]
		if !ok || !exists || a.DispatchRecorded || !b.DispatchRecorded || a.IdentitySHA256 != b.IdentitySHA256 || b.DispatchSHA256 == "" {
			t.Fatal("admission projection lost identity or marker", key.family)
		}
		for _, record := range compareRecoveryExecutionWindow(before, after).Records {
			if record.Family == key.family && record.OrganizationID == key.org && record.RecordID == key.id && record.Comparison == "dispatch_recorded_after_backup" {
				return
			}
		}
		t.Fatal("admission comparison missing", key.family)
	}
	t.Run("tool", func(t *testing.T) {
		f := independentToolSetup(t, false)
		view := f.prepare(t)
		before := read(t, f.s)
		view = f.decide(t, view, f.p, "approve")
		f.dispatch(t, view)
		lease, _, err := f.s.ClaimToolExecution(ctx, "window-tool-worker", time.Minute)
		if err != nil || lease == nil {
			t.Fatal("claim", err)
		}
		if _, err = f.s.PrepareToolDispatch(ctx, lease); err != nil {
			t.Fatal(err)
		}
		after := read(t, f.s)
		check(t, before, after, recoveryWindowKey{"tool_execution", f.p.OrganizationID, lease.Execution.ID})
		check(t, before, after, recoveryWindowKey{"action", f.p.OrganizationID, lease.Execution.ActionID})
	})
	t.Run("mcp_configuration", func(t *testing.T) {
		f := configurationAuthorSetup(t)
		plan := f.plan(t, f.p, "")
		validation := f.validate(t, f.p, plan)
		plan = f.apply(t, f.p, plan, validation)
		verification := f.verify(t, f.p, plan, "connect", 0)
		lease, _, err := f.s.ClaimConfigurationVerification(ctx, "window-mcp-worker", time.Minute)
		if err != nil || lease == nil {
			t.Fatal("claim", err)
		}
		before := read(t, f.s)
		if _, err = f.s.PrepareConfigurationVerificationDispatch(ctx, lease); err != nil {
			t.Fatal(err)
		}
		after := read(t, f.s)
		check(t, before, after, recoveryWindowKey{"configuration_verification", f.p.OrganizationID, verification.ID})
	})
	t.Run("webhook", func(t *testing.T) {
		f := webhookAuthorSetup(t)
		f.subscribe(t, 8)
		f.material(t)
		lease, _, err := f.s.ClaimWebhook(ctx, "window-webhook-worker", time.Minute)
		if err != nil || lease == nil {
			t.Fatal("claim", err)
		}
		before := read(t, f.s)
		if _, err = f.s.PrepareWebhookDispatch(ctx, lease); err != nil {
			t.Fatal(err)
		}
		after := read(t, f.s)
		check(t, before, after, recoveryWindowKey{"webhook_attempt", f.p.OrganizationID, lease.AttemptID})
	})
}
