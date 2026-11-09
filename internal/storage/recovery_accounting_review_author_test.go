// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

func accountingAuthorReview(t *testing.T, f *evidenceAuthorFixture) RecoveryAccountingReview {
	t.Helper()
	r, err := f.target.ReviewRecoveryAccounting(context.Background(), f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits())
	if err != nil || !r.RollbackConfirmed || !r.ExecutionHeld || r.ReconciliationState != "pending" || len(r.ReviewSHA256) != 64 {
		t.Fatal("accounting review", err)
	}
	return r
}
func accountingReviewFact(t *testing.T, r RecoveryAccountingReview, org, evidence string) RecoveryAccountingReviewFact {
	t.Helper()
	for _, f := range r.Facts {
		if f.OrganizationID == org && f.SourceEvidenceID == evidence {
			return f
		}
	}
	t.Fatal("missing review fact")
	return RecoveryAccountingReviewFact{}
}
func accountingReviewReservation(t *testing.T, r RecoveryAccountingReview, org, res string) RecoveryAccountingReviewReservation {
	t.Helper()
	for _, v := range r.Reservations {
		if v.OrganizationID == org && v.ReservationID == res {
			return v
		}
	}
	t.Fatal("missing review reservation")
	return RecoveryAccountingReviewReservation{}
}
func accountingReviewApply(t *testing.T, f *evidenceAuthorFixture, r RecoveryAccountingSelection) RecoveryAccountingResult {
	t.Helper()
	ctx := context.Background()
	p, err := f.target.PlanRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), r)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.target.ApplyRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), r, p.PlanSHA256)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRecoveryAccountingAuthorReviewThreeViews(t *testing.T) {
	f, byOrg := recoveryAccountingAuthorSource(t)
	ctx := context.Background()
	v := byOrg[f.oldOrg]
	if _, err := f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	initial := accountingAuthorReview(t, f)
	if initial.HeadOrdinal != 0 || accountingReviewFact(t, initial, f.oldOrg, v.invoice).NextStep != "prepare_fact_plan" || accountingReviewFact(t, initial, f.oldOrg, v.correction).NextStep != "await_predecessor" {
		t.Fatal("incorrect fact order hints")
	}
	foreign := byOrg[f.newOrg]
	if accountingReviewFact(t, initial, f.newOrg, foreign.invoice).NextStep != "unmapped_scope" || accountingReviewReservation(t, initial, f.newOrg, foreign.reservation).Target != nil {
		t.Fatal("late-only scope became target")
	}
	before := accountingReviewReservation(t, initial, f.oldOrg, v.reservation)
	if before.Original.Actual != nil || before.Target.Actual != nil || before.Captured.Actual == nil || *before.Captured.Actual != 3 {
		t.Fatal("original/captured/current conflated")
	}
	if repeat := accountingAuthorReview(t, f); !reflect.DeepEqual(initial, repeat) {
		t.Fatal("review nondeterministic")
	}
	invoice := accountingReviewApply(t, f, recoveryAccountingAuthorSelection(f.oldOrg, v.reservation, v.invoice))
	afterInvoice := accountingAuthorReview(t, f)
	fact := accountingReviewFact(t, afterInvoice, f.oldOrg, v.invoice)
	if fact.Recording != "recorded" || fact.FactOperationID != invoice.Receipt.ID || fact.ActualMicroUSD != 7 || afterInvoice.ReviewSHA256 == initial.ReviewSHA256 || accountingReviewFact(t, afterInvoice, f.oldOrg, v.correction).NextStep != "prepare_fact_plan" {
		t.Fatal("invoice review attribution")
	}
	accountingReviewApply(t, f, recoveryAccountingAuthorSelection(f.oldOrg, v.reservation, v.correction))
	corrected := accountingAuthorReview(t, f)
	current := accountingReviewReservation(t, corrected, f.oldOrg, v.reservation)
	if current.Original.Actual != nil || current.Target.Actual == nil || *current.Target.Actual != 3 || accountingReviewFact(t, corrected, f.oldOrg, v.invoice).ActualMicroUSD != 7 {
		t.Fatal("historical invoice amount replaced by current actual")
	}
	for _, r := range corrected.Receipts {
		if r.Attribution != "source_fact" {
			t.Fatal("missing full receipt attribution")
		}
	}
	observer, err := Open(ctx, f.target.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	fresh, err := observer.ReviewRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits())
	if err != nil || !reflect.DeepEqual(corrected, fresh) {
		t.Fatal("fresh review changed", err)
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	failed, err := f.target.ReviewRecoveryAccounting(cancelled, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits())
	if err == nil || !reflect.DeepEqual(failed, RecoveryAccountingReview{}) {
		t.Fatal("cancelled review returned partial success")
	}
	limited := DefaultRecoverySnapshotLimits()
	limited.MaxRows, limited.MaxRowsPerTable = 1, 1
	failed, err = f.target.ReviewRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, limited)
	if err == nil || !reflect.DeepEqual(failed, RecoveryAccountingReview{}) {
		t.Fatal("exhausted review returned partial success")
	}
	// An independently corrupted saved mapping must not become a green review.
	tx, err := f.target.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	table := pgx.Identifier{f.target.runtimeSchema, "recovery_accounting_batches"}.Sanitize()
	if _, err = tx.Exec(ctx, "ALTER TABLE "+table+" DISABLE TRIGGER USER"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "UPDATE "+table+" SET plan_sha256=$1 WHERE id=$2", strings.Repeat("a", 64), invoice.Receipt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "ALTER TABLE "+table+" ENABLE TRIGGER USER"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	failed, err = f.target.ReviewRecoveryAccounting(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits())
	if err == nil || !reflect.DeepEqual(failed, RecoveryAccountingReview{}) {
		t.Fatal("modified receipt accepted")
	}
}

func TestRecoveryAccountingAuthorReviewQuarantineOrigins(t *testing.T) {
	for _, mode := range []string{"original_resolved", "original_invoice", "captured_direct", "source_unresolved"} {
		t.Run(mode, func(t *testing.T) {
			f, v, qid := recoveryAccountingResolutionFixture(t, mode)
			initial := accountingAuthorReview(t, f)
			fact := accountingReviewFact(t, initial, f.oldOrg, v.invoice)
			if mode == "original_resolved" {
				if fact.Recording != "imported" || fact.ResolutionOrigin != "original" || fact.TargetQuarantineState != "resolved" || fact.ResolutionOperationID != "" || len(initial.Receipts) != 0 {
					t.Fatal("imported resolution invented recovery receipt")
				}
				return
			}
			if fact.TargetQuarantineState != "pending" || fact.TargetQuarantineID != qid || fact.NextStep != "prepare_resolution_plan" {
				t.Fatal("pending quarantine not actionable")
			}
			resolution := recoveryAccountingAuthorSelection(f.oldOrg, v.reservation, v.invoice)
			resolution.Mode, resolution.QuarantineID = "resolve", qid
			retained := accountingReviewApply(t, f, resolution)
			if retained.State != "quarantine_retained" || accountingAuthorReview(t, f).ReviewSHA256 != initial.ReviewSHA256 {
				t.Fatal("retained attempt changed review")
			}
			accountingReviewApply(t, f, recoveryAccountingAuthorSelection(f.oldOrg, v.zeroReservation, v.zero))
			resolved := accountingReviewApply(t, f, resolution)
			after := accountingAuthorReview(t, f)
			fact = accountingReviewFact(t, after, f.oldOrg, v.invoice)
			if fact.TargetQuarantineState != "resolved" || fact.ResolutionOrigin != "recovery" || fact.ResolutionOperationID != resolved.Receipt.ID {
				t.Fatal("resolution mapping missing")
			}
			if v.correction != "" {
				accountingReviewApply(t, f, recoveryAccountingAuthorSelection(f.oldOrg, v.reservation, v.correction))
				after = accountingAuthorReview(t, f)
				if accountingReviewFact(t, after, f.oldOrg, v.invoice).ResolutionOperationID != resolved.Receipt.ID {
					t.Fatal("later correction changed resolution lineage")
				}
			}
			for _, r := range after.Receipts {
				if r.Attribution == "unattributed" {
					t.Fatal("captured/original resolution receipt unattributed")
				}
			}
		})
	}
}

func TestRecoveryAccountingAuthorReviewUnattributedReceipt(t *testing.T) {
	f, byOrg := recoveryAccountingAuthorSource(t)
	ctx := context.Background()
	if _, err := f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	tx, err := f.target.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	held, _, err := f.target.evidenceTargetTx(ctx, tx, f.spec, f.before.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	v := byOrg[f.oldOrg]
	b := recoveryAccountingBatch{id: strings.TrimPrefix(domain.NewID("accounting"), "accounting_"), evidenceBatchID: f.verified.Link().OperationID, organizationID: f.oldOrg, reservationID: v.reservation, requestSHA256: strings.Repeat("a", 64), planSHA256: strings.Repeat("b", 64)}
	q := func(table string) string { return pgx.Identifier{f.target.runtimeSchema, table}.Sanitize() }
	if tx.QueryRow(ctx, "SELECT revision FROM "+q("reservations")+" WHERE organization_id=$1 AND id=$2", f.oldOrg, v.reservation).Scan(&b.expectedRevision) != nil {
		t.Fatal("reservation revision")
	}
	m := recoveryAccountingMutation{table: "organizations"}
	if tx.QueryRow(ctx, "SELECT to_jsonb(r),to_jsonb(r)||jsonb_build_object('financial_hold',true) FROM "+q("organizations")+" r WHERE id=$1 AND NOT financial_hold", f.oldOrg).Scan(&m.before, &m.after) != nil {
		t.Fatal("hold fixture")
	}
	if _, err = f.target.recordRecoveryAccountingMutationsTx(ctx, tx, held, b, []recoveryAccountingMutation{m}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	review := accountingAuthorReview(t, f)
	if len(review.Receipts) != 1 || review.Receipts[0].Attribution != "unattributed" || review.Receipts[0].OperationID != b.id || accountingReviewFact(t, review, f.oldOrg, v.invoice).NextStep != "review_unattributed_operations" {
		t.Fatal("unattributed receipt hidden")
	}
}
