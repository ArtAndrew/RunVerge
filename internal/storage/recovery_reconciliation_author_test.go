// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

func TestRecoveryReconciliationAuthorCompleteReadOnlyPlan(t *testing.T) {
	f := evidenceAuthorSetup(t)
	ctx := context.Background()
	if _, err := f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	before, err := f.target.InspectRecoveryEvidence(ctx, f.spec, f.verified)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.target.PlanRecoveryReconciliation(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !p.RollbackConfirmed || !p.ExecutionHeld || p.ReconciliationState != "pending" || !validDigest(p.PlanSHA256) || len(p.Organizations) != 2 {
		t.Fatal("incomplete plan")
	}
	var original, lateOnly *RecoveryFinancialOrganization
	for i := range p.Organizations {
		o := &p.Organizations[i]
		if o.OrganizationID == f.oldOrg {
			original = o
		}
		if o.OrganizationID == f.newOrg {
			lateOnly = o
		}
	}
	if original == nil || original.Before == nil || original.After == nil || original.Before.UnknownCount != 2 || original.After.UnknownCount != 2 || lateOnly == nil || lateOnly.Before != nil || lateOnly.After == nil || lateOnly.After.UnknownCount != 2 {
		t.Fatal("lost organization or zero-cost uncertainty")
	}
	zeroUnknown := false
	for _, r := range p.Reservations {
		if r.Before != nil && r.Before.Status == "unknown" && r.Before.Amount == 0 {
			zeroUnknown = true
			if r.Before.Actual != nil || r.After == nil || r.After.Actual != nil {
				t.Fatal("unknown became confirmed zero")
			}
		}
	}
	if !zeroUnknown || len(p.NewFacts) == 0 {
		t.Fatal("lost reservations or late facts")
	}
	for _, b := range p.Blockers {
		if b.Code == "financial_history_not_contiguous" || strings.HasPrefix(b.Code, "immutable_financial_fact_") {
			t.Fatal("unchanged original financial history rejected", b.Code)
		}
	}
	copyPlan := p
	copyPlan.PlanSHA256 = ""
	raw, _ := json.Marshal(copyPlan)
	if Hash(raw) != p.PlanSHA256 {
		t.Fatal("plan digest does not bind exact output")
	}
	again, err := f.target.PlanRecoveryReconciliation(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits())
	if err != nil || !reflect.DeepEqual(p, again) {
		t.Fatal("plan is not reproducible", err)
	}
	after, err := f.target.InspectRecoveryEvidence(ctx, f.spec, f.verified)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("planning altered evidence", err)
	}
	if f.target.CheckRuntimeAdmission(ctx) != RecoveryHeld {
		t.Fatal("planning enabled ordinary work")
	}
	var reserved, booked int64
	if f.target.Pool.QueryRow(ctx, "SELECT reserved_microusd,spent_microusd FROM organizations WHERE id=$1", f.oldOrg).Scan(&reserved, &booked) != nil || reserved != original.Before.Reserved || booked != original.Before.Booked {
		t.Fatal("planning changed target accounts")
	}
}

func TestRecoveryReconciliationAuthorRequiresCommittedBatch(t *testing.T) {
	f := evidenceAuthorSetup(t)
	if _, err := f.target.PlanRecoveryReconciliation(context.Background(), f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits()); err == nil {
		t.Fatal("uncommitted evidence planned")
	}
}

func TestRecoveryReconciliationAuthorDetectsRewrittenFinancialFact(t *testing.T) {
	f := evidenceAuthorSetupWithMutation(t, func(s *Store) {
		// Owned negative source fixture: both snapshots remain individually
		// valid, but an old immutable record's timestamp has been rewritten.
		tx, err := s.Pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		for _, sql := range []string{"ALTER TABLE ledger_entries DISABLE TRIGGER USER", "UPDATE ledger_entries SET created_at=created_at-interval '1 microsecond'", "ALTER TABLE ledger_entries ENABLE TRIGGER USER"} {
			if _, err = tx.Exec(context.Background(), sql); err != nil {
				t.Fatal(err)
			}
		}
		if err = tx.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	ctx := context.Background()
	if _, err := f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal("individually valid evidence", err)
	}
	p, err := f.target.PlanRecoveryReconciliation(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range p.Blockers {
		if b.Code == "immutable_financial_fact_changed" && b.Table == "ledger_entries" && b.OrganizationID == f.oldOrg {
			found = true
		}
	}
	if !found {
		t.Fatal("financial rewrite hidden by equal sums")
	}
}

func TestRecoveryReconciliationHistoryRequiresCheckpointContinuity(t *testing.T) {
	makeHistory := func(settled bool, unknown bool, adjusted bool) *recoveryCostHistory {
		h := &recoveryCostHistory{org: "org", id: "reservation", operation: "op", task: "task", root: "task", category: "compute", provider: "test", status: "reserved", amount: 10, revision: 1, ledger: map[string]*recoveryCostLedger{}, evidence: map[string]*recoveryCostEvidence{}, quarantines: map[string]*recoveryCostQuarantine{}}
		h.ledger["reserve"] = &recoveryCostLedger{id: "reserve", key: "reserve", hash: strings.Repeat("a", 64), kind: "reserve", task: h.task, root: h.root, reserved: 10}
		if unknown {
			h.status = "unknown"
			h.revision++
			h.ledger["unknown:observed"] = &recoveryCostLedger{id: "unknown", key: "unknown:observed", hash: strings.Repeat("b", 64), kind: "unknown", task: h.task, root: h.root}
		}
		if settled {
			e := &recoveryCostEvidence{id: "bill", kind: "settle", value: domain.CostEvidence{Source: "test", Reference: "bill", PayloadDigest: strings.Repeat("c", 64), ActualMicroUSD: 7, Final: true, Currency: "USD"}}
			e.hash = evidenceRequestHash(e.value, nil)
			h.evidence[e.id] = e
			key := "settle:test:bill"
			h.ledger[key] = &recoveryCostLedger{id: "settle", key: key, hash: e.hash, kind: "settle", task: h.task, root: h.root, reserved: -10, spent: 7}
			actual := int64(7)
			h.actual = &actual
			h.status = "settled"
			h.revision++
		}
		if adjusted {
			revision := h.revision
			e := &recoveryCostEvidence{id: "correction", kind: "adjustment", value: domain.CostEvidence{Source: "test", Reference: "correction", PayloadDigest: strings.Repeat("d", 64), ActualMicroUSD: 3, Final: true, Currency: "USD", Reason: "synthetic correction"}}
			e.hash = evidenceRequestHash(e.value, &revision)
			h.evidence[e.id] = e
			key := "adjustment:test:correction"
			h.ledger[key] = &recoveryCostLedger{id: "adjust", key: key, hash: e.hash, kind: "adjustment", task: h.task, root: h.root, spent: -4}
			actual := int64(3)
			h.actual = &actual
			h.revision++
		}
		if _, _, _, ok := recoveryCostCheck(context.Background(), h, &recoveryCostWork{}); !ok {
			t.Fatal("test history is not independently valid")
		}
		return h
	}
	makeQuarantine := func(resolved, adjusted bool) *recoveryCostHistory {
		h := makeHistory(false, false, false)
		e := &recoveryCostEvidence{id: "quarantined-bill", value: domain.CostEvidence{Source: "test", Reference: "quarantined-bill", PayloadDigest: strings.Repeat("e", 64), ActualMicroUSD: 7, Final: true, Currency: "USD"}}
		e.hash = evidenceRequestHash(e.value, nil)
		q := &recoveryCostQuarantine{id: "quarantine", evidence: e.id, baseRevision: 1, revision: 2, status: "reserved", kind: "settle", reason: "projection_overflow"}
		e.q = q
		h.evidence[e.id] = e
		h.quarantines[q.id] = q
		key := "quarantine:" + q.id
		h.ledger[key] = &recoveryCostLedger{id: "quarantined", key: key, hash: e.hash, kind: "quarantine", task: h.task, root: h.root}
		h.status = "unknown"
		h.pending = q.id
		h.revision = 2
		if resolved {
			revision := 3
			q.resolution = &revision
			key = "resolve:" + q.id
			h.ledger[key] = &recoveryCostLedger{id: "resolved", key: key, hash: e.hash, kind: "quarantine_resolution", task: h.task, root: h.root, reserved: -10, spent: 7}
			actual := int64(7)
			h.actual = &actual
			h.status = "settled"
			h.pending = ""
			h.revision = 3
		}
		if adjusted {
			revision := 3
			a := &recoveryCostEvidence{id: "after-quarantine", value: domain.CostEvidence{Source: "test", Reference: "after-quarantine", PayloadDigest: strings.Repeat("f", 64), ActualMicroUSD: 3, Final: true, Currency: "USD", Reason: "synthetic correction"}}
			a.hash = evidenceRequestHash(a.value, &revision)
			h.evidence[a.id] = a
			key = "adjustment:test:after-quarantine"
			h.ledger[key] = &recoveryCostLedger{id: "q-adjusted", key: key, hash: a.hash, kind: "adjustment", task: h.task, root: h.root, spent: -4}
			actual := int64(3)
			h.actual = &actual
			h.revision = 4
		}
		if _, _, _, ok := recoveryCostCheck(context.Background(), h, &recoveryCostWork{}); !ok {
			t.Fatal("quarantine history not independently valid")
		}
		return h
	}
	t.Run("exhaustion_is_not_a_financial_blocker", func(t *testing.T) {
		before, after := makeHistory(false, false, false), makeHistory(true, false, false)
		work := &recoveryCostWork{hashes: recoveryCostMaxHashWork}
		if valid, err := recoveryCostContinuationChecked(context.Background(), before, after, work); valid || err == nil {
			t.Fatal("exhausted check returned a financial conclusion")
		}
		if valid, err := recoveryCostContinuationChecked(context.Background(), before, before, work); valid || err == nil {
			t.Fatal("later unchanged history hid prior exhaustion")
		}
		if valid, err := recoveryCostContinuationChecked(context.Background(), before, after, &recoveryCostWork{}); !valid || err != nil {
			t.Fatal("complete valid history was refused", err)
		}
		invalidBefore, invalidAfter := makeHistory(true, false, false), makeHistory(true, true, false)
		if valid, err := recoveryCostContinuationChecked(context.Background(), invalidBefore, invalidAfter, &recoveryCostWork{}); valid || err != nil {
			t.Fatal("actual discontinuity not distinguished from exhaustion", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if valid, err := recoveryCostContinuationChecked(ctx, before, before, &recoveryCostWork{}); valid || err == nil {
			t.Fatal("cancelled check returned a financial conclusion")
		}
	})
	for _, scenario := range []struct {
		name          string
		before, after *recoveryCostHistory
	}{
		{"pending_unchanged", makeQuarantine(false, false), makeQuarantine(false, false)},
		{"pending_resolved", makeQuarantine(false, false), makeQuarantine(true, false)},
		{"pending_resolved_then_adjusted", makeQuarantine(false, false), makeQuarantine(true, true)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if !recoveryCostContinues(context.Background(), scenario.before, scenario.after, &recoveryCostWork{}) {
				t.Fatal("valid quarantine continuation rejected")
			}
		})
	}
	makeReleased := func(unknown bool) *recoveryCostHistory {
		h := makeHistory(false, unknown, false)
		key := "release:synthetic"
		h.ledger[key] = &recoveryCostLedger{id: "released", key: key, hash: digestValue("not_dispatched"), kind: "release", task: h.task, root: h.root, reserved: -10}
		h.status = "released"
		h.revision++
		if _, _, _, ok := recoveryCostCheck(context.Background(), h, &recoveryCostWork{}); !ok {
			t.Fatal("released snapshot not independently valid")
		}
		return h
	}
	if !recoveryCostContinues(context.Background(), makeHistory(false, false, false), makeReleased(false), &recoveryCostWork{}) {
		t.Fatal("unsent release rejected")
	}
	if recoveryCostContinues(context.Background(), makeHistory(false, true, false), makeReleased(true), &recoveryCostWork{}) {
		t.Fatal("new release erased old unknown")
	}
	makeFixture := func(settled bool) *recoveryCostHistory {
		h := makeHistory(false, false, false)
		h.category = "fixture_legacy"
		h.amount = domain.FixtureCost
		h.ledger = map[string]*recoveryCostLedger{"fixture.reserve": {id: "fixture-reserved", key: "fixture.reserve", hash: digestValue(h.operation), kind: "reserve", task: h.task, root: h.root, reserved: h.amount, synthetic: true}}
		if settled {
			h.ledger["fixture.settle"] = &recoveryCostLedger{id: "fixture-settled", key: "fixture.settle", hash: digestValue(h.operation), kind: "settle", task: h.task, root: h.root, reserved: -h.amount, spent: h.amount, synthetic: true}
			actual := h.amount
			h.actual = &actual
			h.status = "settled"
			h.revision = 2
		}
		if _, _, _, ok := recoveryCostCheck(context.Background(), h, &recoveryCostWork{}); !ok {
			t.Fatal("fixture history not independently valid")
		}
		return h
	}
	if !recoveryCostContinues(context.Background(), makeFixture(false), makeFixture(true), &recoveryCostWork{}) {
		t.Fatal("new fixture completion rejected")
	}
	for _, category := range []string{"model", "tool"} {
		before, after := makeHistory(false, true, false), makeHistory(false, true, false)
		before.category, after.category = category, category
		key := "kernel:late"
		hash := digestValue("late")
		if category == "tool" {
			key = "tool.effect.unknown"
		}
		after.ledger[key] = &recoveryCostLedger{id: "reserved-only", key: key, hash: hash, kind: "unknown", task: after.task, root: after.root}
		after.revision++
		if _, _, _, ok := recoveryCostCheck(context.Background(), after, &recoveryCostWork{}); !ok {
			t.Fatal("reserved-only late snapshot is not independently valid")
		}
		if recoveryCostContinues(context.Background(), before, after, &recoveryCostWork{}) {
			t.Fatal("reserved-only marker reordered ahead of old unknown", category)
		}
	}
	for _, c := range []struct {
		name          string
		before, after *recoveryCostHistory
		want          bool
	}{
		{"new_settlement", makeHistory(false, false, false), makeHistory(true, false, false), true},
		{"unknown_then_settlement", makeHistory(false, true, false), makeHistory(true, true, false), true},
		{"negative_adjustment", makeHistory(true, false, false), makeHistory(true, false, true), true},
		{"new_unknown_reordered_before_old_settlement", makeHistory(true, false, false), makeHistory(true, true, false), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := recoveryCostContinues(context.Background(), c.before, c.after, &recoveryCostWork{}); got != c.want {
				t.Fatal("wrong checkpoint continuity", got)
			}
		})
	}
}
