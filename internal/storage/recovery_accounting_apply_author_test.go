// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

type recoveryAccountingSourceFixture struct {
	reservation, zeroReservation string
	invoice, correction, zero    string
}

func recoveryAccountingAuthorSource(t *testing.T) (*evidenceAuthorFixture, map[string]recoveryAccountingSourceFixture) {
	return recoveryAccountingAuthorSourceMode(t, false)
}

func recoveryAccountingAuthorSourceMode(t *testing.T, sourceQuarantine bool) (*evidenceAuthorFixture, map[string]recoveryAccountingSourceFixture) {
	t.Helper()
	byOrg := map[string]recoveryAccountingSourceFixture{}
	f := evidenceAuthorSetupWithMutation(t, func(source *Store) {
		ctx := context.Background()
		rows, err := source.Pool.Query(ctx, "SELECT organization_id,id,amount_microusd FROM reservations WHERE status='unknown' AND provider='author-owned-test' ORDER BY organization_id,amount_microusd")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var org, id string
			var amount int64
			if rows.Scan(&org, &id, &amount) != nil {
				t.Fatal("source reservation decode")
			}
			v := byOrg[org]
			if amount == 0 {
				v.zeroReservation = id
			} else {
				v.reservation = id
			}
			byOrg[org] = v
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		for org, v := range byOrg {
			var capacity domain.AccountingOutcome
			if sourceQuarantine {
				var spent int64
				if err = source.Pool.QueryRow(ctx, "SELECT spent_microusd FROM organizations WHERE id=$1", org).Scan(&spent); err != nil {
					t.Fatal(err)
				}
				capacityFact := domain.CostEvidence{Source: "author-owned-test", Reference: "source-capacity-invoice", PayloadDigest: strings.Repeat("b", 64), ActualMicroUSD: math.MaxInt64 - spent, Final: true, Currency: "USD"}
				capacity, err = source.AccountCost(ctx, org, v.zeroReservation, capacityFact)
				if err != nil || capacity.Status != "settled" {
					t.Fatal("source integer capacity", err)
				}
			}
			// A real additional unknown event makes source and restored target
			// revisions diverge. It must not be replayed as an execution event.
			if _, err = source.MarkCostUnknown(ctx, org, v.reservation, "source-late-unknown", "owned source fixture"); err != nil {
				t.Fatal(err)
			}
			fact := domain.CostEvidence{Source: "author-owned-test", Reference: "source-invoice", PayloadDigest: strings.Repeat("a", 64), ActualMicroUSD: 7, Final: true, Currency: "USD"}
			out, err := source.AccountCost(ctx, org, v.reservation, fact)
			if err != nil || (!sourceQuarantine && out.Status != "settled") || (sourceQuarantine && out.Status != "quarantined") {
				t.Fatal("source invoice", err)
			}
			v.invoice = out.EvidenceID
			if sourceQuarantine {
				capacityCorrection := domain.CostEvidence{Source: "author-owned-test", Reference: "source-capacity-correction", PayloadDigest: strings.Repeat("c", 64), ActualMicroUSD: 0, Reason: "owned capacity correction", Final: true, Currency: "USD"}
				cleared, err := source.AccountAdjustment(ctx, org, v.zeroReservation, domain.CostAdjustment{Evidence: capacityCorrection, ExpectedRevision: capacity.Reservation.Revision})
				if err != nil || cleared.Status != "settled" {
					t.Fatal("source capacity correction", err)
				}
				v.zero = cleared.EvidenceID
				out, err = source.ResolveCostQuarantine(ctx, org, out.QuarantineID, out.Reservation.Revision)
				if err != nil || out.Status != "settled" {
					t.Fatal("source quarantine resolution", err)
				}
			}
			fact.Reference, fact.ActualMicroUSD, fact.Reason = "source-correction", 3, "owned correction"
			out, err = source.AccountAdjustment(ctx, org, v.reservation, domain.CostAdjustment{Evidence: fact, ExpectedRevision: out.Reservation.Revision})
			if err != nil || out.Status != "settled" {
				t.Fatal("source correction", err)
			}
			v.correction = out.EvidenceID
			if !sourceQuarantine {
				fact.Reference, fact.ActualMicroUSD, fact.Reason = "source-zero", 0, ""
				out, err = source.AccountCost(ctx, org, v.zeroReservation, fact)
				if err != nil || out.Status != "settled" {
					t.Fatal("source zero", err)
				}
				v.zero = out.EvidenceID
			}
			byOrg[org] = v
		}
	})
	return f, byOrg
}

func TestRecoveryAccountingAuthorSourceQuarantineTargetSettlement(t *testing.T) {
	f, byOrg := recoveryAccountingAuthorSourceMode(t, true)
	ctx := context.Background()
	if _, err := f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	v := byOrg[f.oldOrg]
	invoice := recoveryAccountingAuthorSelection(f.oldOrg, v.reservation, v.invoice)
	p, err := f.target.PlanRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), invoice)
	if err != nil {
		t.Fatal("source quarantined fact plan", err)
	}
	first, err := f.target.ApplyRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), invoice, p.PlanSHA256)
	if err != nil || first.Outcome.Status != "settled" || first.Outcome.QuarantineID != "" || !first.NewlyRecorded {
		t.Fatal("source quarantine incorrectly imposed on target", err)
	}
	correction := recoveryAccountingAuthorSelection(f.oldOrg, v.reservation, v.correction)
	pc, err := f.target.PlanRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), correction)
	if err != nil || pc.SourceRevision-int(pc.TargetRevision) != 2 || pc.SourceRequestSHA256 == pc.RequestSHA256 {
		t.Fatal("quarantine revision translation", err)
	}
	second, err := f.target.ApplyRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), correction, pc.PlanSHA256)
	if err != nil || second.Outcome.Reservation.ActualMicroUSD == nil || *second.Outcome.Reservation.ActualMicroUSD != 3 {
		t.Fatal("post-quarantine correction", err)
	}
	invoice.OperationID = strings.TrimPrefix(domain.NewID("accounting"), "accounting_")
	replay, err := f.target.PlanRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), invoice)
	if err != nil || replay.State != "already_recorded" || replay.PlanSHA256 != p.PlanSHA256 || replay.OperationID != first.Receipt.ID {
		t.Fatal("source quarantine mapping replay", err)
	}
	if f.target.CheckRuntimeAdmission(ctx) != RecoveryHeld {
		t.Fatal("quarantine translation enabled work")
	}
}

func recoveryAccountingAuthorSelection(org, reservation, evidence string) RecoveryAccountingSelection {
	return RecoveryAccountingSelection{OperationID: strings.TrimPrefix(domain.NewID("accounting"), "accounting_"), OrganizationID: org, ReservationID: reservation, SourceEvidenceID: evidence}
}

func TestRecoveryAccountingAuthorSourceFactProvenanceAndReplay(t *testing.T) {
	f, byOrg := recoveryAccountingAuthorSource(t)
	ctx := context.Background()
	v := byOrg[f.oldOrg]
	invoice := recoveryAccountingAuthorSelection(f.oldOrg, v.reservation, v.invoice)
	plan := func(r RecoveryAccountingSelection) (RecoveryAccountingPlan, error) {
		return f.target.PlanRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), r)
	}
	apply := func(r RecoveryAccountingSelection, p RecoveryAccountingPlan) (RecoveryAccountingResult, error) {
		return f.target.ApplyRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), r, p.PlanSHA256)
	}
	if _, err := plan(invoice); err == nil {
		t.Fatal("uncommitted source evidence accepted")
	}
	if _, err := f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	q := func(table string) string { return pgx.Identifier{f.target.runtimeSchema, table}.Sanitize() }
	execution := func() json.RawMessage {
		t.Helper()
		var result json.RawMessage
		if err := f.target.Pool.QueryRow(ctx, `SELECT jsonb_build_object('tasks',(SELECT jsonb_agg(to_jsonb(t)-ARRAY['reserved_microusd','spent_microusd','financial_hold'] ORDER BY organization_id,id) FROM `+q("tasks")+` t),'actions',(SELECT jsonb_agg(to_jsonb(a) ORDER BY organization_id,id) FROM `+q("actions")+` a),'events',(SELECT count(*) FROM `+q("events")+`),'outbox',(SELECT count(*) FROM `+q("outbox")+`))`).Scan(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	originalExecution := execution()
	correction := recoveryAccountingAuthorSelection(f.oldOrg, v.reservation, v.correction)
	if _, err := plan(correction); err == nil {
		t.Fatal("correction skipped its invoice")
	}
	foreign := byOrg[f.newOrg]
	if _, err := plan(recoveryAccountingAuthorSelection(f.newOrg, foreign.reservation, foreign.invoice)); err == nil {
		t.Fatal("late-only identity became active")
	}
	if _, err := plan(recoveryAccountingAuthorSelection(f.oldOrg, v.reservation, foreign.invoice)); err == nil {
		t.Fatal("another source scope selected")
	}
	p, err := plan(invoice)
	if err != nil || p.State != "ready" || p.SourceRevision == int(p.TargetRevision) || !p.RollbackConfirmed || !p.ExecutionHeld {
		t.Fatal("bound invoice plan", err)
	}
	zero := recoveryAccountingAuthorSelection(f.oldOrg, v.zeroReservation, v.zero)
	staleZero, err := plan(zero)
	if err != nil {
		t.Fatal(err)
	}
	wrong := p
	wrong.PlanSHA256 = strings.Repeat("f", 64)
	if _, err = apply(invoice, wrong); err == nil {
		t.Fatal("edited plan digest accepted")
	}
	first, err := apply(invoice, p)
	if err != nil || !first.NewlyRecorded || first.Receipt.ID != invoice.OperationID || first.Outcome.Status != "settled" {
		t.Fatal("bound invoice application", err)
	}
	if _, err = apply(zero, staleZero); err == nil {
		t.Fatal("stale chain head accepted")
	}
	pc, err := plan(correction)
	if err != nil || pc.RequestSHA256 == pc.SourceRequestSHA256 || pc.SourceRevision == int(pc.TargetRevision) {
		t.Fatal("source revision translation", err)
	}
	second, err := apply(correction, pc)
	if err != nil || second.Receipt.Ordinal != 2 || second.Outcome.Reservation.ActualMicroUSD == nil || *second.Outcome.Reservation.ActualMicroUSD != 3 {
		t.Fatal("translated correction", err)
	}
	// A new request ID for the old invoice returns its real original operation,
	// even after the reservation's revision and actual amount have changed.
	repeat := invoice
	repeat.OperationID = strings.TrimPrefix(domain.NewID("accounting"), "accounting_")
	rp, err := plan(repeat)
	if err != nil || rp.State != "already_recorded" || rp.OperationID != invoice.OperationID || rp.RequestedOperationID != repeat.OperationID || rp.PlanSHA256 != p.PlanSHA256 {
		t.Fatal("old mapping was not reconstructed", err)
	}
	replayed, err := apply(repeat, rp)
	if err != nil || replayed.NewlyRecorded || replayed.Receipt.ID != invoice.OperationID || replayed.Outcome.EvidenceID != first.Outcome.EvidenceID || replayed.Outcome.Reservation.Revision != second.Outcome.Reservation.Revision || !replayed.Plan.RollbackConfirmed {
		t.Fatal("invoice replay was reapplied", err)
	}
	notRecorded, err := f.target.InspectRecoveryAccounting(ctx, f.selected, f.verified, f.spec, DefaultRecoverySnapshotLimits(), repeat.OperationID, rp.RequestSHA256)
	if err != nil || notRecorded.State != "not_recorded" {
		t.Fatal("new replay ID incorrectly claimed a commit", err)
	}
	// The correction also replays from its stored translated revision.
	again, err := apply(correction, pc)
	if err != nil || again.NewlyRecorded || again.Receipt.ID != correction.OperationID {
		t.Fatal("translated correction replay", err)
	}
	pz, err := plan(zero)
	if err != nil {
		t.Fatal(err)
	}
	last, err := apply(zero, pz)
	if err != nil || last.Receipt.Ordinal != 3 || last.Outcome.Reservation.ActualMicroUSD == nil || *last.Outcome.Reservation.ActualMicroUSD != 0 {
		t.Fatal("confirmed source zero", err)
	}
	if string(originalExecution) != string(execution()) || f.target.CheckRuntimeAdmission(ctx) != RecoveryHeld {
		t.Fatal("source accounting altered execution")
	}
	invoice.OperationID = zero.OperationID
	if _, err := plan(invoice); err == nil {
		t.Fatal("operation ID reused for another source fact")
	}
}

func TestRecoveryAccountingAuthorSourceApplyCommitLoss(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			f, byOrg := recoveryAccountingAuthorSource(t)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			if _, err := f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits()); err != nil {
				t.Fatal(err)
			}
			v := byOrg[f.oldOrg]
			selection := recoveryAccountingAuthorSelection(f.oldOrg, v.reservation, v.invoice)
			plan, err := f.target.PlanRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), selection)
			if err != nil {
				t.Fatal(err)
			}
			victim, proxy := independentImportCommitProxy(t, f.target.Pool.Config().ConnString())
			cut := proxy.arm(t, "insert into "+pgx.Identifier{victim.runtimeSchema, "recovery_accounting_batches"}.Sanitize(), mode)
			result, err := victim.ApplyRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), selection, plan.PlanSHA256)
			if !errors.Is(err, ErrRecoveryAccountingUnknown) || !reflect.DeepEqual(result, RecoveryAccountingResult{}) {
				t.Fatal("unknown commit reported success", err)
			}
			proxy.wait(t, cut)
			observer, err := Open(ctx, f.target.Pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			defer observer.Close()
			inspected, err := observer.InspectRecoveryAccounting(ctx, f.selected, f.verified, f.spec, DefaultRecoverySnapshotLimits(), selection.OperationID, plan.RequestSHA256)
			if err != nil || (mode == "before_commit" && inspected.State != "not_recorded") || (mode == "after_commit" && inspected.State != "recorded") {
				t.Fatal("fresh commit inspection", err)
			}
			if mode == "after_commit" {
				fresh, err := observer.PlanRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), selection)
				if err != nil || fresh.State != "already_recorded" || fresh.PlanSHA256 != plan.PlanSHA256 {
					t.Fatal("committed provenance not reconstructible", err)
				}
			}
		})
	}
}
