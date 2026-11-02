// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/storage"
)

type recoveryHostAccountingFixture struct{ org, reservation, invoice, correction string }

func TestRecoveryHostAuthorAccountingConsentBeforeFiles(t *testing.T) {
	if err := runRecoveryHost(context.Background(), []string{"accounting-apply", "--intent-file", "/not-read"}, io.Discard); err == nil || !strings.HasPrefix(err.Error(), "usage:") {
		t.Fatal("financial mutation without consent reached private files")
	}
}

func prepareRecoveryHostAccountingFixture(t *testing.T, ctx context.Context, s *storage.Store) *recoveryHostAccountingFixture {
	t.Helper()
	b, err := s.Bootstrap(ctx, "owned accounting CLI fixture", 1000000)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Authenticate(ctx, b.Token, b.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	budget := int64(10000)
	raw, err := s.CreateAgent(ctx, p, domain.NewID("key"), domain.CreateAgent{Name: "Financial source fixture", Instructions: "No external calls", BudgetMicroUSD: &budget})
	if err != nil {
		t.Fatal(err)
	}
	var a domain.Agent
	if json.Unmarshal(raw, &a) != nil {
		t.Fatal("financial agent decode")
	}
	raw, err = s.PublishAgent(ctx, p, a.ID, domain.NewID("key"), a.Version)
	if err != nil || json.Unmarshal(raw, &a) != nil {
		t.Fatal("financial agent publish", err)
	}
	if _, err = s.CreateTask(ctx, p, domain.NewID("key"), domain.CreateTask{Goal: "Owned financial history", AgentID: a.ID, BudgetMicroUSD: 10000}); err != nil {
		t.Fatal(err)
	}
	lease, err := s.ClaimFixture(ctx, "accounting-cli-fixture", time.Minute)
	if err != nil || lease == nil {
		t.Fatal("financial fixture lease", err)
	}
	r, err := s.ReserveCost(ctx, lease, domain.ReserveCostRequest{ChargeKey: domain.NewID("charge"), Category: "compute", Provider: "host-owned-provider", PriceVersion: "host-test-v1", AmountMicroUSD: 37, IntentHash: storage.Hash([]byte("owned source financial intent"))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.MarkCostUnknown(ctx, p.OrganizationID, r.ID, "owned-financial-unknown", "no external request performed"); err != nil {
		t.Fatal(err)
	}
	return &recoveryHostAccountingFixture{org: p.OrganizationID, reservation: r.ID}
}

func appendRecoveryHostAccountingFacts(t *testing.T, ctx context.Context, s *storage.Store, fixture *recoveryHostAccountingFixture) {
	t.Helper()
	e := domain.CostEvidence{Source: "host-owned-provider", Reference: "host-invoice", PayloadDigest: strings.Repeat("d", 64), ActualMicroUSD: 7, Final: true, Currency: "USD"}
	out, err := s.AccountCost(ctx, fixture.org, fixture.reservation, e)
	if err != nil || out.Status != "settled" {
		t.Fatal("host source invoice", err)
	}
	fixture.invoice = out.EvidenceID
	e.Reference, e.ActualMicroUSD, e.Reason = "host-correction", 3, "owned correction"
	out, err = s.AccountAdjustment(ctx, fixture.org, fixture.reservation, domain.CostAdjustment{Evidence: e, ExpectedRevision: out.Reservation.Revision})
	if err != nil || out.Status != "settled" {
		t.Fatal("host source correction", err)
	}
	fixture.correction = out.EvidenceID
}

func recoveryHostAccountingRoundTrip(t *testing.T, ctx context.Context, fixture *recoveryHostAccountingFixture, sourcePlan, targetConfig, parent string) {
	t.Helper()
	planArgs := func(evidence, path string) []string {
		return []string{"accounting-plan", "--source-intent-file", sourcePlan, "--target-config", targetConfig, "--organization-id", fixture.org, "--reservation-id", fixture.reservation, "--source-evidence-id", evidence, "--out", path}
	}
	invoicePlan := filepath.Join(parent, "invoice-plan.json")
	if err := runRecoveryHost(ctx, planArgs(fixture.invoice, invoicePlan), recoveryHostFailedOutput{}); !errors.Is(err, errRecoveryHostOutput) {
		t.Fatal("financial plan output loss", err)
	}
	var saved recoveryAccountingFile
	if err := readRecoveryFenceJSON(invoicePlan, &saved); err != nil || saved.Plan.State != "ready" {
		t.Fatal("durable financial plan", err)
	}
	if _, err := recoveryHostCommand(t, ctx, planArgs(fixture.correction, filepath.Join(parent, "skipped-correction.json"))...); err == nil {
		t.Fatal("host correction skipped predecessor")
	}
	applyArgs := []string{"accounting-apply", "--intent-file", invoicePlan, "--confirm-record-financial-fact"}
	if err := runRecoveryHost(ctx, applyArgs, recoveryHostFailedOutput{}); !errors.Is(err, errRecoveryHostOutput) {
		t.Fatal("financial apply output loss", err)
	}
	if _, err := recoveryHostCommand(t, ctx, applyArgs...); err == nil {
		t.Fatal("one-use financial plan applied twice")
	}
	raw, err := recoveryHostCommand(t, ctx, "accounting-inspect", "--intent-file", invoicePlan)
	var inspected storage.RecoveryAccountingInspection
	if err != nil || json.Unmarshal(raw, &inspected) != nil || inspected.State != "recorded" || inspected.PlanSHA256 != saved.Plan.PlanSHA256 || !inspected.ExecutionHeld {
		t.Fatal("fresh financial command inspection", err)
	}
	correctionPlan := filepath.Join(parent, "correction-plan.json")
	if _, err = recoveryHostCommand(t, ctx, planArgs(fixture.correction, correctionPlan)...); err != nil {
		t.Fatal("correction command plan", err)
	}
	raw, err = recoveryHostCommand(t, ctx, "accounting-apply", "--intent-file", correctionPlan, "--confirm-record-financial-fact")
	var result storage.RecoveryAccountingResult
	if err != nil || json.Unmarshal(raw, &result) != nil || !result.NewlyRecorded || result.Receipt.Ordinal != 2 || result.Outcome.Reservation.ActualMicroUSD == nil || *result.Outcome.Reservation.ActualMicroUSD != 3 || !result.ExecutionHeld {
		t.Fatal("correction command apply", err)
	}
	if _, err = recoveryHostCommand(t, ctx, "accounting-inspect", "--intent-file", invoicePlan); err != nil {
		t.Fatal("old invoice inspection after correction", err)
	}
	var savedCorrection recoveryAccountingFile
	if err = readRecoveryFenceJSON(correctionPlan, &savedCorrection); err != nil {
		t.Fatal(err)
	}
	tampered := saved
	tampered.Plan.OperationID = savedCorrection.Plan.OperationID
	tampered.Plan.RequestSHA256 = savedCorrection.Plan.RequestSHA256
	tampered.Plan.PlanSHA256 = savedCorrection.Plan.PlanSHA256
	tamperedPath := recoveryHostPrivateJSON(t, filepath.Join(parent, "wrong-fact-inspection.json"), tampered)
	if _, err = recoveryHostCommand(t, ctx, "accounting-inspect", "--intent-file", tamperedPath); err == nil {
		t.Fatal("correction receipt misattributed to original invoice")
	}
	// New operation plan for the old source invoice points to its real receipt.
	replayPlan := filepath.Join(parent, "replay-plan.json")
	raw, err = recoveryHostCommand(t, ctx, planArgs(fixture.invoice, replayPlan)...)
	var replay storage.RecoveryAccountingPlan
	if err != nil || json.Unmarshal(raw, &replay) != nil || replay.State != "already_recorded" || replay.OperationID != saved.Plan.OperationID || replay.RequestedOperationID == replay.OperationID {
		t.Fatal("host replay identity", err)
	}
	raw, err = recoveryHostCommand(t, ctx, "accounting-apply", "--intent-file", replayPlan, "--confirm-record-financial-fact")
	if err != nil || json.Unmarshal(raw, &result) != nil || result.NewlyRecorded || result.State != "already_recorded" || result.Receipt.ID != saved.Plan.OperationID {
		t.Fatal("host replay double charge", err)
	}
}
