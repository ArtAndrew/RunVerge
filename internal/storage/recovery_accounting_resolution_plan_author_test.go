// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// The original archive either already contains a quarantine (including an
// upward adjustment), or contains a full capacity invoice. In the latter case
// the source settles the small invoice directly after clearing capacity, while
// the target sees that same captured invoice before the capacity correction.
func recoveryAccountingResolutionFixture(t *testing.T, mode string) (*evidenceAuthorFixture, recoveryAccountingSourceFixture, string) {
	t.Helper()
	ctx := context.Background()
	var org, quarantine string
	var v recoveryAccountingSourceFixture
	var capacity, small domain.AccountingOutcome
	fact := domain.CostEvidence{Source: "author-owned-test", Reference: "resolution-invoice", PayloadDigest: strings.Repeat("f", 64), ActualMicroUSD: 7, Final: true, Currency: "USD"}
	f := evidenceAuthorSetupWithMutations(t, func(s *Store) {
		if err := s.Pool.QueryRow(ctx, "SELECT organization_id,id FROM reservations WHERE status='unknown' AND amount_microusd=37 AND provider='author-owned-test'").Scan(&org, &v.reservation); err != nil {
			t.Fatal(err)
		}
		if err := s.Pool.QueryRow(ctx, "SELECT id FROM reservations WHERE organization_id=$1 AND status='unknown' AND amount_microusd=0 AND provider='author-owned-test'", org).Scan(&v.zeroReservation); err != nil {
			t.Fatal(err)
		}
		var err error
		if mode == "original_adjustment" {
			small, err = s.AccountCost(ctx, org, v.reservation, fact)
			if err != nil || small.Status != "settled" {
				t.Fatal("original settlement", err)
			}
		}
		var spent int64
		if err = s.Pool.QueryRow(ctx, "SELECT spent_microusd FROM organizations WHERE id=$1", org).Scan(&spent); err != nil {
			t.Fatal(err)
		}
		c := fact
		c.Reference, c.ActualMicroUSD = "resolution-capacity", math.MaxInt64-spent
		capacity, err = s.AccountCost(ctx, org, v.zeroReservation, c)
		if err != nil || capacity.Status != "settled" {
			t.Fatal("original capacity", err)
		}
		if mode == "captured_direct" {
			return
		}
		if mode == "original_adjustment" {
			fact.Reference, fact.ActualMicroUSD, fact.Reason = "resolution-upward", 11, "owned upward adjustment"
			small, err = s.AccountAdjustment(ctx, org, v.reservation, domain.CostAdjustment{Evidence: fact, ExpectedRevision: small.Reservation.Revision})
		} else {
			small, err = s.AccountCost(ctx, org, v.reservation, fact)
		}
		if err != nil || small.Status != "quarantined" {
			t.Fatal("original quarantine", err)
		}
		v.invoice, quarantine = small.EvidenceID, small.QuarantineID
		if mode == "original_resolved" {
			c = fact
			c.Reference, c.ActualMicroUSD, c.Reason = "original-capacity-clear", 0, "owned original correction"
			capacity, err = s.AccountAdjustment(ctx, org, v.zeroReservation, domain.CostAdjustment{Evidence: c, ExpectedRevision: capacity.Reservation.Revision})
			if err != nil || capacity.Status != "settled" {
				t.Fatal("original capacity cleared", err)
			}
			small, err = s.ResolveCostQuarantine(ctx, org, quarantine, small.Reservation.Revision)
			if err != nil || small.Status != "settled" {
				t.Fatal("original resolution", err)
			}
		}
	}, func(s *Store) {
		c := fact
		c.Reference, c.ActualMicroUSD, c.Reason = "resolution-capacity-cleared", 0, "owned capacity correction"
		out, err := s.AccountAdjustment(ctx, org, v.zeroReservation, domain.CostAdjustment{Evidence: c, ExpectedRevision: capacity.Reservation.Revision})
		if err != nil || out.Status != "settled" {
			t.Fatal("source capacity correction", err)
		}
		v.zero = out.EvidenceID
		if mode == "source_unresolved" {
			return
		}
		if mode == "captured_direct" {
			small, err = s.AccountCost(ctx, org, v.reservation, fact)
			v.invoice = small.EvidenceID
		} else if mode != "original_resolved" {
			small, err = s.ResolveCostQuarantine(ctx, org, quarantine, small.Reservation.Revision)
		}
		if err != nil || small.Status != "settled" {
			t.Fatal("source invoice settlement", err)
		}
		c = fact
		c.Reference, c.ActualMicroUSD, c.Reason = "resolution-final-correction", 3, "owned final correction"
		out, err = s.AccountAdjustment(ctx, org, v.reservation, domain.CostAdjustment{Evidence: c, ExpectedRevision: small.Reservation.Revision})
		if err != nil || out.Status != "settled" {
			t.Fatal("source final correction", err)
		}
		v.correction = out.EvidenceID
	})
	if _, err := f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	if mode == "captured_direct" {
		r := recoveryAccountingAuthorSelection(org, v.reservation, v.invoice)
		p, err := f.target.PlanRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), r)
		if err != nil {
			t.Fatal("captured direct invoice plan", err)
		}
		out, err := f.target.ApplyRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), r, p.PlanSHA256)
		if err != nil || out.Outcome.Status != "quarantined" {
			t.Fatal("target quarantine for direct source invoice", err)
		}
		quarantine = out.Outcome.QuarantineID
	}
	return f, v, quarantine
}

func TestRecoveryAccountingAuthorSourceBoundResolution(t *testing.T) {
	for _, mode := range []string{"original_invoice", "original_adjustment", "captured_direct", "source_unresolved"} {
		t.Run(mode, func(t *testing.T) {
			f, v, qid := recoveryAccountingResolutionFixture(t, mode)
			ctx := context.Background()
			plan := func(r RecoveryAccountingSelection) (RecoveryAccountingPlan, error) {
				return f.target.PlanRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), r)
			}
			apply := func(r RecoveryAccountingSelection, p RecoveryAccountingPlan) (RecoveryAccountingResult, error) {
				return f.target.ApplyRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), r, p.PlanSHA256)
			}
			request := recoveryAccountingAuthorSelection(f.oldOrg, v.reservation, v.invoice)
			request.Mode, request.QuarantineID = "resolve", qid
			for _, fault := range []string{"mode", "fact_with_quarantine", "quarantine", "evidence", "reservation", "organization"} {
				bad := request
				switch fault {
				case "mode":
					bad.Mode = "resolution"
				case "fact_with_quarantine":
					bad.Mode = "fact"
				case "quarantine":
					bad.QuarantineID = "other-quarantine"
				case "evidence":
					bad.SourceEvidenceID = v.zero
				case "reservation":
					bad.ReservationID = v.zeroReservation
				case "organization":
					bad.OrganizationID = f.newOrg
				}
				if _, err := plan(bad); err == nil {
					t.Fatalf("accepted mismatched resolution %s", fault)
				}
			}
			p, err := plan(request)
			if err != nil || p.Mode != "resolve" || p.QuarantineID != qid || !p.RollbackConfirmed {
				t.Fatal("resolution plan", err)
			}
			// No room yet: no new batch and no false success. An upward adjustment must
			// retain its previous actual amount while the new invoice remains quarantined.
			retained, err := apply(request, p)
			if err != nil || retained.State != "quarantine_retained" || retained.NewlyRecorded || retained.Receipt.ID != "" || !retained.Plan.RollbackConfirmed || retained.Outcome.Status != "quarantined" {
				t.Fatal("retained quarantine result", err)
			}
			if mode == "original_adjustment" && (retained.Outcome.Reservation.ActualMicroUSD == nil || *retained.Outcome.Reservation.ActualMicroUSD != 7) {
				t.Fatal("prior actual lost")
			}
			inspected, err := f.target.InspectRecoveryAccounting(ctx, f.selected, f.verified, f.spec, DefaultRecoverySnapshotLimits(), request.OperationID, p.RequestSHA256)
			if err != nil || inspected.State != "not_recorded" {
				t.Fatal("retained attempt reported recorded", err)
			}
			capacity := recoveryAccountingAuthorSelection(f.oldOrg, v.zeroReservation, v.zero)
			cp, err := plan(capacity)
			if err != nil {
				t.Fatal("capacity plan", err)
			}
			cleared, err := apply(capacity, cp)
			if err != nil || !cleared.NewlyRecorded {
				t.Fatal("capacity apply", err)
			}
			if _, err = apply(request, p); err == nil {
				t.Fatal("resolution accepted stale chain head")
			}
			p, err = plan(request)
			if err != nil {
				t.Fatal("fresh resolution plan", err)
			}
			bad := p
			bad.PlanSHA256 = cp.PlanSHA256
			if _, err = apply(request, bad); err == nil {
				t.Fatal("fact digest accepted for resolution")
			}
			resolved, err := apply(request, p)
			if err != nil || !resolved.NewlyRecorded || resolved.Outcome.Status != "settled" || resolved.Outcome.Reservation.PendingQuarantineID != "" || resolved.Outcome.EvidenceID != retained.Outcome.EvidenceID || resolved.Receipt.PreviousID != cleared.Receipt.ID {
				t.Fatal("resolution application", err)
			}
			corrected := resolved
			if v.correction != "" {
				correction := recoveryAccountingAuthorSelection(f.oldOrg, v.reservation, v.correction)
				correctionPlan, err := plan(correction)
				if err != nil {
					t.Fatal("post-resolution correction plan", err)
				}
				corrected, err = apply(correction, correctionPlan)
				if err != nil || corrected.Outcome.Reservation.ActualMicroUSD == nil || *corrected.Outcome.Reservation.ActualMicroUSD != 3 {
					t.Fatal("post-resolution correction", err)
				}
			}
			request.OperationID = strings.TrimPrefix(domain.NewID("accounting"), "accounting_")
			replay, err := plan(request)
			if err != nil || replay.State != "already_recorded" || replay.OperationID != resolved.Receipt.ID || replay.PlanSHA256 != p.PlanSHA256 {
				t.Fatal("resolution mapping replay", err)
			}
			result, err := apply(request, replay)
			if err != nil || result.NewlyRecorded || result.Receipt.ID != resolved.Receipt.ID || result.Outcome.Reservation.Revision != corrected.Outcome.Reservation.Revision {
				t.Fatal("resolution applied again", err)
			}
			request.OperationID = cleared.Receipt.ID
			if _, err = plan(request); err == nil {
				t.Fatal("other operation ID reused for resolution")
			}
			if !errors.Is(f.target.CheckRuntimeAdmission(ctx), RecoveryHeld) {
				t.Fatal("resolution enabled execution")
			}
		})
	}
}

func TestRecoveryAccountingAuthorResolutionCommitLoss(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			f, v, qid := recoveryAccountingResolutionFixture(t, "original_invoice")
			ctx := context.Background()
			capacity := recoveryAccountingAuthorSelection(f.oldOrg, v.zeroReservation, v.zero)
			cp, err := f.target.PlanRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), capacity)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.target.ApplyRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), capacity, cp.PlanSHA256); err != nil {
				t.Fatal(err)
			}
			request := recoveryAccountingAuthorSelection(f.oldOrg, v.reservation, v.invoice)
			request.Mode, request.QuarantineID = "resolve", qid
			p, err := f.target.PlanRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), request)
			if err != nil {
				t.Fatal(err)
			}
			victim, proxy := independentImportCommitProxy(t, f.target.Pool.Config().ConnString())
			cut := proxy.arm(t, "insert into "+pgx.Identifier{victim.runtimeSchema, "recovery_accounting_batches"}.Sanitize(), mode)
			result, err := victim.ApplyRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), request, p.PlanSHA256)
			if !errors.Is(err, ErrRecoveryAccountingUnknown) || !reflect.DeepEqual(result, RecoveryAccountingResult{}) {
				t.Fatal("lost resolution commit reported success", err)
			}
			proxy.wait(t, cut)
			observer, err := Open(ctx, f.target.Pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			defer observer.Close()
			inspected, err := observer.InspectRecoveryAccounting(ctx, f.selected, f.verified, f.spec, DefaultRecoverySnapshotLimits(), request.OperationID, p.RequestSHA256)
			if err != nil || (mode == "before_commit" && inspected.State != "not_recorded") || (mode == "after_commit" && inspected.State != "recorded") {
				t.Fatal("resolution commit inspection", err)
			}
			if mode == "after_commit" {
				fresh, err := observer.PlanRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), request)
				if err != nil || fresh.State != "already_recorded" || fresh.PlanSHA256 != p.PlanSHA256 {
					t.Fatal("resolution provenance after lost commit", err)
				}
			}
		})
	}
}
