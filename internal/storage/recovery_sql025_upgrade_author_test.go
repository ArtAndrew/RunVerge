// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/recoveryprocess"
	"github.com/jackc/pgx/v5"
)

func TestRecoverySQL025AuthorPopulatedUpgrade(t *testing.T) {
	for _, mode := range []string{"old_resolution", "new_resolution"} {
		t.Run(mode, func(t *testing.T) {
			recoveryEvidenceLegacyProducer(t, "RUNVERGE_RECOVERY_SQL025_BINARY", recoverySQL025Schema, recoverySQL025Catalog, mode)
		})
	}
}

type recoverySQL025UpgradeFixture struct {
	oldProfile                                                                                                        string
	org, reservation, capacityReservation, invoice, correction, capacityInvoice, capacityCorrection, quarantine, mode string
	oldPlans                                                                                                          []RecoveryAccountingPlan
	oldRows                                                                                                           string
}

func recoverySQL025LateFacts(t *testing.T, s *Store, org, mode string) *recoverySQL025UpgradeFixture {
	t.Helper()
	ctx := context.Background()
	f := &recoverySQL025UpgradeFixture{org: org, mode: mode, oldProfile: recoveryAccountingMutationV1}
	if err := s.Pool.QueryRow(ctx, "SELECT id FROM reservations WHERE organization_id=$1 AND status='unknown' AND amount_microusd=37", org).Scan(&f.reservation); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(ctx, "SELECT id FROM reservations WHERE organization_id=$1 AND status='unknown' AND amount_microusd=0", org).Scan(&f.capacityReservation); err != nil {
		t.Fatal(err)
	}
	var spent int64
	if s.Pool.QueryRow(ctx, "SELECT spent_microusd FROM organizations WHERE id=$1", org).Scan(&spent) != nil {
		t.Fatal("source spent")
	}
	capacity, err := s.AccountCost(ctx, org, f.capacityReservation, domain.CostEvidence{Source: "author-owned-test", Reference: "sql025-capacity", PayloadDigest: strings.Repeat("a", 64), ActualMicroUSD: math.MaxInt64 - spent, Final: true, Currency: "USD"})
	if err != nil || capacity.Status != "settled" {
		t.Fatal("source capacity", err)
	}
	f.capacityInvoice = capacity.EvidenceID
	small, err := s.AccountCost(ctx, org, f.reservation, domain.CostEvidence{Source: "author-owned-test", Reference: "sql025-invoice", PayloadDigest: strings.Repeat("b", 64), ActualMicroUSD: 7, Final: true, Currency: "USD"})
	if err != nil || small.Status != "quarantined" {
		t.Fatal("source quarantine", err)
	}
	f.invoice = small.EvidenceID
	cleared, err := s.AccountAdjustment(ctx, org, f.capacityReservation, domain.CostAdjustment{ExpectedRevision: capacity.Reservation.Revision, Evidence: domain.CostEvidence{Source: "author-owned-test", Reference: "sql025-capacity-correction", PayloadDigest: strings.Repeat("c", 64), ActualMicroUSD: 0, Final: true, Currency: "USD", Reason: "synthetic capacity correction"}})
	if err != nil || cleared.Status != "settled" {
		t.Fatal("source capacity correction", err)
	}
	f.capacityCorrection = cleared.EvidenceID
	small, err = s.ResolveCostQuarantine(ctx, org, small.QuarantineID, small.Reservation.Revision)
	if err != nil || small.Status != "settled" {
		t.Fatal("source resolution", err)
	}
	corrected, err := s.AccountAdjustment(ctx, org, f.reservation, domain.CostAdjustment{ExpectedRevision: small.Reservation.Revision, Evidence: domain.CostEvidence{Source: "author-owned-test", Reference: "sql025-correction", PayloadDigest: strings.Repeat("d", 64), ActualMicroUSD: 3, Final: true, Currency: "USD", Reason: "synthetic invoice correction"}})
	if err != nil || corrected.Status != "settled" {
		t.Fatal("source correction", err)
	}
	f.correction = corrected.EvidenceID
	return f
}

// The fixture signs an explicit synthetic capture link. It exercises old CLI
// ingestion/accounting, not a real Docker shutdown or external fencing claim.
func recoverySQL025SourceIntent(t *testing.T, dir, keys, stage string, selected, late *recoveryarchive.VerifiedArchive) (string, *recoveryarchive.VerifiedCaptureLink) {
	t.Helper()
	a, b := selected.Manifest(), late.Manifest()
	op := strings.TrimPrefix(domain.NewID("capture"), "capture_")
	binding := recoveryprocess.SourceBinding{Version: 1, SourceID: a.Metadata.SourceID, SnapshotSHA256: a.PlaintextSHA256, ObservedAt: time.Now().UTC(), Processes: []recoveryprocess.SourceProcess{}}
	observation := recoveryprocess.Observation{OperationID: op, SourceID: a.Metadata.SourceID, ObservedAt: time.Now().UTC(), Containers: []recoveryprocess.ContainerState{}}
	path := filepath.Join(dir, "sql025-source-intent.json")
	file := struct {
		Socket  string                         `json:"docker_socket"`
		Intent  recoveryprocess.Intent         `json:"intent"`
		Binding *recoveryprocess.SourceBinding `json:"source_binding"`
		Capture map[string]string              `json:"evidence_capture"`
	}{Intent: recoveryprocess.Intent{Version: 1, OperationID: op, CreatedAt: time.Now().UTC(), Containers: []recoveryprocess.Container{}}, Binding: &binding, Capture: map[string]string{"directory": filepath.Join(dir, "late"), "keys_file": keys, "staging_parent": stage, "selected_receipt_sha256": recoveryManifestReceipt(selected), "selected_directory": filepath.Join(dir, "selected"), "selected_cut_utc": a.Metadata.CutUTC}}
	if err := recoveryarchive.WriteHostJSON(path, file); err != nil {
		t.Fatal(err)
	}
	if err := recoveryarchive.WriteHostJSON(path+".capture-state", observation); err != nil {
		t.Fatal(err)
	}
	link := recoveryarchive.CaptureLink{Version: 1, OperationID: op, SourceID: a.Metadata.SourceID, SelectedReceiptSHA256: recoveryManifestReceipt(selected), SelectedSnapshotSHA256: a.PlaintextSHA256, SelectedCutUTC: a.Metadata.CutUTC, BindingSHA256: digestValue(&binding), StoppedExecutorsSHA256: digestValue(observation), EvidenceReceiptSHA256: recoveryManifestReceipt(late), EvidenceSnapshotSHA256: b.PlaintextSHA256, EvidenceCutUTC: b.Metadata.CutUTC, CapturedAt: time.Now().UTC()}
	if err := recoveryarchive.WriteHostCaptureLink(path+".capture", keys, link); err != nil {
		t.Fatal(err)
	}
	verified, err := recoveryarchive.VerifyHostCaptureLink(path+".capture", keys)
	if err != nil {
		t.Fatal(err)
	}
	return path, verified
}

func recoverySQL025Rows(t *testing.T, s *Store) string {
	t.Helper()
	q := func(n string) string { return pgx.Identifier{s.runtimeSchema, n}.Sanitize() }
	var raw []byte
	err := s.Pool.QueryRow(context.Background(), `SELECT jsonb_build_object('batches',(SELECT jsonb_agg(to_jsonb(b)-'mutation_profile' ORDER BY ordinal) FROM `+q("recovery_accounting_batches")+` b),'mutations',(SELECT jsonb_agg(to_jsonb(m) ORDER BY batch_id,ordinal) FROM `+q("recovery_accounting_mutations")+` m),'applied',(SELECT jsonb_agg(to_jsonb(a) ORDER BY batch_id,ordinal) FROM `+q("recovery_accounting_applied")+` a))`).Scan(&raw)
	if err != nil {
		t.Fatal("old immutable accounting rows", err)
	}
	return string(raw)
}

func (f *recoverySQL025UpgradeFixture) applyOld(t *testing.T, old func(string, ...string) []byte, sourceIntent, targetIntent, dir string, s *Store) {
	t.Helper()
	old("unused", "recovery", "evidence-ingest", "--source-intent-file", sourceIntent, "--target-config", targetIntent, "--confirm-ingest-evidence")
	apply := func(reservation, evidence, mode, quarantine string) RecoveryAccountingResult {
		t.Helper()
		path := filepath.Join(dir, fmt.Sprintf("sql025-financial-%d.json", len(f.oldPlans)))
		args := []string{"recovery", "accounting-plan", "--source-intent-file", sourceIntent, "--target-config", targetIntent, "--organization-id", f.org, "--reservation-id", reservation, "--source-evidence-id", evidence, "--out", path}
		if mode != "" {
			args = append(args, "--mode", mode, "--quarantine-id", quarantine)
		}
		var plan RecoveryAccountingPlan
		if json.Unmarshal(old("unused", args...), &plan) != nil || !plan.ExecutionHeld {
			t.Fatal("old financial plan")
		}
		var result RecoveryAccountingResult
		if json.Unmarshal(old("unused", "recovery", "accounting-apply", "--intent-file", path, "--confirm-record-financial-fact"), &result) != nil || !result.NewlyRecorded || result.Plan.PlanSHA256 != plan.PlanSHA256 {
			t.Fatal("old financial application")
		}
		f.oldPlans = append(f.oldPlans, plan)
		return result
	}
	if r := apply(f.capacityReservation, f.capacityInvoice, "", ""); r.Outcome.Status != "settled" {
		t.Fatal("old capacity")
	}
	invoice := apply(f.reservation, f.invoice, "", "")
	if invoice.Outcome.Status != "quarantined" {
		t.Fatal("old target quarantine")
	}
	f.quarantine = invoice.Outcome.QuarantineID
	if r := apply(f.capacityReservation, f.capacityCorrection, "", ""); r.Outcome.Status != "settled" {
		t.Fatal("old capacity correction")
	}
	if f.mode == "old_resolution" {
		if r := apply(f.reservation, f.invoice, "resolve", f.quarantine); r.Outcome.Status != "settled" {
			t.Fatal("old resolution")
		}
	}
	f.oldRows = recoverySQL025Rows(t, s)
}

func (f *recoverySQL025UpgradeFixture) checkUpgraded(t *testing.T, s *Store, selected, late *recoveryarchive.VerifiedArchive, verified *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec) {
	t.Helper()
	ctx := context.Background()
	limits := DefaultRecoverySnapshotLimits()
	q := func(n string) string { return pgx.Identifier{s.runtimeSchema, n}.Sanitize() }
	if recoverySQL025Rows(t, s) != f.oldRows {
		t.Fatal("migration changed old immutable accounting content")
	}
	var count int
	if s.Pool.QueryRow(ctx, "SELECT count(*) FROM "+q("recovery_accounting_batches")+" WHERE mutation_profile=$1", f.oldProfile).Scan(&count) != nil || count != len(f.oldPlans) {
		t.Fatal("historical profile not explicit")
	}
	replay := func() {
		t.Helper()
		for _, old := range f.oldPlans {
			request := recoveryAccountingAuthorSelection(f.org, old.ReservationID, old.SourceEvidenceID)
			request.Mode, request.QuarantineID = old.Mode, old.QuarantineID
			plan, err := s.PlanRecoveryAccounting(ctx, selected, late, verified, spec, limits, request)
			if err != nil || plan.State != "already_recorded" || plan.PlanSHA256 != old.PlanSHA256 || plan.RequestSHA256 != old.RequestSHA256 || plan.OperationID != old.OperationID {
				t.Fatal("historical commitment changed on upgraded replay", err)
			}
			result, err := s.ApplyRecoveryAccounting(ctx, selected, late, verified, spec, limits, request, plan.PlanSHA256)
			if err != nil || result.NewlyRecorded || result.Receipt.ID != old.OperationID {
				t.Fatal("historical replay mutated history", err)
			}
		}
	}
	replay()
	applyNew := func(request RecoveryAccountingSelection) RecoveryAccountingResult {
		t.Helper()
		plan, err := s.PlanRecoveryAccounting(ctx, selected, late, verified, spec, limits, request)
		if err != nil {
			t.Fatal("V2 plan", err)
		}
		result, err := s.ApplyRecoveryAccounting(ctx, selected, late, verified, spec, limits, request, plan.PlanSHA256)
		if err != nil || !result.NewlyRecorded || result.Outcome.Status != "settled" {
			t.Fatal("V2 application", err)
		}
		var profile string
		if s.Pool.QueryRow(ctx, "SELECT mutation_profile FROM "+q("recovery_accounting_batches")+" WHERE id=$1", result.Receipt.ID).Scan(&profile) != nil || profile != recoveryAccountingMutationV2 {
			t.Fatal("new mutation not V2")
		}
		return result
	}
	if f.mode == "new_resolution" {
		request := recoveryAccountingAuthorSelection(f.org, f.reservation, f.invoice)
		request.Mode, request.QuarantineID = "resolve", f.quarantine
		applyNew(request)
	}
	corrected := applyNew(recoveryAccountingAuthorSelection(f.org, f.reservation, f.correction))
	if corrected.Outcome.Reservation.ActualMicroUSD == nil || *corrected.Outcome.Reservation.ActualMicroUSD != 3 {
		t.Fatal("V2 correction amount")
	}
	replay()
	review, err := s.ReviewRecoveryAccounting(ctx, selected, late, verified, spec, limits)
	if err != nil || !review.ExecutionHeld {
		t.Fatal("mixed V1/V2 review", err)
	}
	for _, receipt := range review.Receipts {
		if receipt.Attribution == "unattributed" {
			t.Fatal("mixed profile receipt unattributed")
		}
	}
	if len(review.Receipts) != 5 || s.CheckRuntimeAdmission(ctx) != RecoveryHeld {
		t.Fatal("mixed receipt count or execution hold")
	}
}
