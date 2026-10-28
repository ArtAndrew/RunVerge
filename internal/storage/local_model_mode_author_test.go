// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
)

// Reuse the real DB/HTTP fixture only for setup. These are author tests.
func authorLocalModelReady(t *testing.T) *independentModelFixture {
	t.Helper()
	f := independentModelSetup(t, false, nil)
	ctx := context.Background()
	if _, err := f.s.CancelTask(ctx, f.p, f.task.ID, domain.NewID("key")); err != nil {
		t.Fatal(err)
	}
	c := f.c
	c.Version++
	c.Endpoint += "/v1/chat/completions"
	c.AccountingMode = domain.ModelAccountingLocalUnmetered
	c.MaxChargeMicroUSD = 0
	c.InputMicroUSDPerMillion = 0
	c.OutputMicroUSDPerMillion = 0
	c.FixedMicroUSD = 0
	c.PriceVersion = "operator-local-zero-v1"
	var err error
	f.c, err = f.s.InstallModelConnection(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	runs, deadline := 2, 60
	raw, err := f.s.CreateTask(ctx, f.p, domain.NewID("key"), domain.CreateTask{AgentID: f.task.AgentID, Goal: "Review a local draft", BudgetMicroUSD: 1, MaxRuns: &runs, ExecutionTimeoutSeconds: &deadline})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &f.task); err != nil {
		t.Fatal(err)
	}
	f.lease, c, err = f.s.ClaimModel(ctx, "author-local-worker", 30*time.Second)
	if err != nil || f.lease == nil || c.AccountingMode != domain.ModelAccountingLocalUnmetered {
		t.Fatal("zero model was not pinned and claimed", err)
	}
	return f
}

func TestAuthorLocalZeroModelPersistsUnknownWithoutDebitingMoney(t *testing.T) {
	f := authorLocalModelReady(t)
	ctx := context.Background()
	out, err := f.runtime(t, f.s).Execute(ctx, f.lease)
	if err != nil || out.Status != "candidate_waiting_input" || f.calls.Load() != 1 {
		t.Fatal("local model did not produce candidate", out, err)
	}
	current, err := f.s.GetTask(ctx, f.p, f.task.ID)
	if err != nil || current.ReservedMicroUSD != 0 || current.SpentMicroUSD != 0 || current.Status != "waiting_input" || current.FinancialHold {
		t.Fatal("local model fabricated monetary cost or accepted draft", err)
	}
	report, err := f.s.Costs(ctx, f.p, f.task.ID, CostCursors{Limit: 1})
	if err != nil || report.UnknownMicroUSD != 0 || report.UnknownReservationCount != 1 || len(report.Reservations) != 1 || report.Reservations[0].Status != "unknown" || report.Reservations[0].AmountMicroUSD != 0 {
		t.Fatal("zero amount concealed unresolved accounting", report, err)
	}
	reopened, err := Open(ctx, f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if again, retryErr := f.runtime(t, reopened).Execute(ctx, f.lease); f.calls.Load() != 1 || (retryErr == nil && again.Status != "already_attempted") {
		t.Fatal("reopened zero-price model resent attempt")
	}
	models, _, err := reopened.Models(ctx, f.p, "", 100)
	if err != nil || len(models) != 1 || models[0].AccountingMode != domain.ModelAccountingLocalUnmetered || models[0].MaxChargeMicroUSD != 0 || models[0].TimeoutMilliseconds != f.c.TimeoutMilliseconds || models[0].MaxOutputBytes != f.c.MaxOutputBytes {
		t.Fatal("safe model metadata lost accounting/resource bounds", models, err)
	}
}

func TestAuthorLocalZeroModeCannotBypassFinancialOrAuthorityHold(t *testing.T) {
	for _, state := range []string{"organization_hold", "task_hold", "revoked"} {
		t.Run(state, func(t *testing.T) {
			f := authorLocalModelReady(t)
			ctx := context.Background()
			var err error
			switch state {
			case "organization_hold":
				_, err = f.s.Pool.Exec(ctx, "UPDATE organizations SET financial_hold=true WHERE id=$1", f.p.OrganizationID)
			case "task_hold":
				_, err = f.s.Pool.Exec(ctx, "UPDATE tasks SET financial_hold=true WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.task.ID)
			case "revoked":
				_, err = f.s.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, f.p.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.runtime(t, f.s).Execute(ctx, f.lease); err == nil || f.calls.Load() != 0 {
				t.Fatal("zero tariff bypassed current admission", err)
			}
		})
	}
}

func TestAuthorUnknownZeroCountDoesNotDependOnReservationPage(t *testing.T) {
	f := authorLocalModelReady(t)
	ctx := context.Background()
	if _, err := f.runtime(t, f.s).Execute(ctx, f.lease); err != nil {
		t.Fatal(err)
	}
	c := &independentCollaborationFixture{independentModelFixture: f}
	c.refresh(t)
	if _, err := f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), c.review("request_changes")); err != nil {
		t.Fatal(err)
	}
	current, err := f.s.GetTask(ctx, f.p, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.RetryTask(ctx, f.p, f.task.ID, domain.NewID("key"), domain.RetryTaskRequest{ExpectedVersion: current.Version, AcknowledgeNewCost: true, Feedback: "Refine the local draft for human review", Reason: "Explicit second local attempt"})
	if err != nil {
		t.Fatal(err)
	}
	f.lease, _, err = f.s.ClaimModel(ctx, "author-local-worker-2", 30*time.Second)
	if err != nil || f.lease == nil {
		t.Fatal(err)
	}
	if _, err = f.runtime(t, f.s).Execute(ctx, f.lease); err != nil {
		t.Fatal(err)
	}
	report, err := f.s.Costs(ctx, f.p, f.task.ID, CostCursors{Limit: 1})
	if err != nil || report.UnknownMicroUSD != 0 || report.UnknownReservationCount != 2 || len(report.Reservations) != 1 || report.NextReservationCursor == "" {
		t.Fatal("paginated zero uncertainties were hidden", report, err)
	}
	c.refresh(t)
	if _, err = f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), c.review("request_changes")); err != nil {
		t.Fatal(err)
	}
	current, err = f.s.GetTask(ctx, f.p, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.RetryTask(ctx, f.p, f.task.ID, domain.NewID("key"), domain.RetryTaskRequest{ExpectedVersion: current.Version, AcknowledgeNewCost: true, Feedback: "Third attempt must not bypass the lifetime cap", Reason: "Explicit denied attempt"}); err == nil {
		t.Fatal("zero tariff bypassed lifetime max_runs")
	}
	if f.calls.Load() != 2 {
		t.Fatal("zero model attempt counter reset")
	}
}

func TestAuthorLocalZeroModelDoesNotMakePaidToolsFree(t *testing.T) {
	f := authorLocalModelReady(t)
	ctx := context.Background()
	if _, err := f.runtime(t, f.s).Execute(ctx, f.lease); err != nil {
		t.Fatal(err)
	}
	candidate := &independentCollaborationFixture{independentModelFixture: f}
	candidate.refresh(t)
	peer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("overbudget tool reached peer") }))
	defer peer.Close()
	schema := `{"type":"object","properties":{},"additionalProperties":false}`
	tool, err := f.s.InstallToolConnection(ctx, domain.ToolConnection{
		Tool:      domain.ToolInfo{ID: "paid-tool", OrganizationID: f.p.OrganizationID, Version: 1, Name: "Paid tool fixture", Description: "Must retain its own tariff", Effect: "read", Destination: "Owned test peer", Provider: "paid-fixture", PriceVersion: "paid-price-v1", MaxChargeMicroUSD: 120, InputSchemaJSON: schema, InputSchemaDigest: Hash([]byte(schema)), TimeoutSeconds: 3, MaxInputBytes: 4096, MaxOutputBytes: 4096, Active: true},
		Transport: "http_json", ResultContract: domain.ToolResultContractActionV1, Endpoint: peer.URL, AllowPrivateNetwork: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.PrepareToolExecution(ctx, f.p, f.task.ID, domain.NewID("key"), domain.PrepareToolExecution{OccurrenceID: domain.NewID("occ"), ExpectedTaskVersion: candidate.current.Version, CandidateID: candidate.candidate.ID, ArtifactID: candidate.candidate.ArtifactID, ArtifactVersion: candidate.candidate.ArtifactVersion, ContentDigest: candidate.candidate.ContentDigest, ToolID: tool.Tool.ID, ToolVersion: tool.Tool.Version, ArgumentsJSON: "{}", ExpiresInSeconds: 60})
	var failure *domain.Error
	if !errors.As(err, &failure) || failure.Code != "budget_exceeded" {
		t.Fatal("local model zero tariff bypassed paid tool budget", err)
	}
	report, err := f.s.Costs(ctx, f.p, f.task.ID, CostCursors{Limit: 1})
	if err != nil || report.UnknownReservationCount != 1 || report.ReservedMicroUSD != 0 || report.SpentMicroUSD != 0 {
		t.Fatal("denied tool fabricated a charge", err)
	}
}
