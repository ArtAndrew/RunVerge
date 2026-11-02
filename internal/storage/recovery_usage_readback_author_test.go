// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelgateway"
	"agentplatform.local/platform/internal/modelruntime"
)

// Ordinary admission and candidate APIs build synthetic model history. No
// Runtime.Execute or external HTTP request is made by this fixture.
func recoveryUsageAuthorAttempt(t *testing.T, s *Store, p domain.Principal, c domain.ModelConnection, generation string) (domain.Reservation, func() domain.Artifact) {
	t.Helper()
	ctx := context.Background()
	budget := int64(10000)
	raw, err := s.CreateAgent(ctx, p, domain.NewID("key"), domain.CreateAgent{Name: "Synthetic usage source", Instructions: "Return a synthetic draft", Model: c.Model, BudgetMicroUSD: &budget})
	if err != nil {
		t.Fatal(err)
	}
	var a domain.Agent
	if json.Unmarshal(raw, &a) != nil {
		t.Fatal("agent decode")
	}
	raw, err = s.PublishAgent(ctx, p, a.ID, domain.NewID("key"), a.Version)
	if err != nil || json.Unmarshal(raw, &a) != nil {
		t.Fatal("publish", err)
	}
	_ = task(t, s, p, a)
	l, _, err := s.ClaimModel(ctx, "usage-synthetic-worker", 2*time.Minute)
	if err != nil || l == nil {
		t.Fatal("claim", err)
	}
	runtime, err := modelruntime.New(s, RuntimeModelConfig(c, "synthetic-unused-key"))
	if err != nil {
		t.Fatal(err)
	}
	charge, intent, err := runtime.DescribeIntent(*l)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.ReserveCost(ctx, l, domain.ReserveCostRequest{ChargeKey: charge, IntentHash: intent, Category: "model", Provider: c.ProviderAccount, PriceVersion: c.PriceVersion, AmountMicroUSD: c.MaxChargeMicroUSD})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PrepareModelDispatch(ctx, l, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.MarkCostUnknown(ctx, p.OrganizationID, r.ID, domain.NewID("unknown"), "synthetic request, no external call"); err != nil {
		t.Fatal(err)
	}
	return r, func() domain.Artifact {
		artifact, err := s.CompleteModelCandidate(ctx, l, r.ID, domain.ModelCandidate{Text: "Synthetic candidate", ProviderRequestID: generation, Model: c.Model, PriceVersion: c.PriceVersion, IntentDigest: intent, InputTokens: 3, OutputTokens: 2, EstimatedMicroUSD: 3})
		if err != nil {
			t.Fatal("synthetic completion", err)
		}
		return artifact
	}
}

func recoveryUsageAuthorFixture(t *testing.T, mode string) (*evidenceAuthorFixture, RecoveryUsageSelection) {
	t.Helper()
	ctx := context.Background()
	s, _ := recoveryAuthorStore(t)
	p, _ := identity(t, s, 100000)
	c := domain.ModelConnection{ID: "usage-router", OrganizationID: p.OrganizationID, Version: 1, WireProfile: domain.ModelWireOpenRouterTextV1, Model: "example/usage-model", ProviderAccount: "synthetic-provider-account", PriceVersion: "synthetic-v1", Endpoint: modelgateway.OpenRouterEndpoint, CredentialEnvironment: "PLATFORM_MODEL_KEY_USAGE_FIXTURE", MaxChargeMicroUSD: 37, MaxInputTokens: 32, InputMicroUSDPerMillion: 1000000, MaxOutputTokens: 16, MaxInputBytes: 4096, MaxOutputBytes: 4096, TimeoutMilliseconds: 2000, TokenLimitField: "max_tokens", BoundAssumptions: "Synthetic history only; no model request", Active: true}
	if mode == "unsupported" {
		c.WireProfile = ""
		c.Endpoint = "https://unsupported.invalid/v1/chat/completions"
	}
	var err error
	c, err = s.InstallModelConnection(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	var other domain.Artifact
	if mode == "original_collision_disappeared" || mode == "captured_collision" {
		_, complete := recoveryUsageAuthorAttempt(t, s, p, c, "gen-usage-selected")
		other = complete()
	}
	id := "gen-usage-selected"
	if mode == "empty_id" {
		id = ""
	}
	r, complete := recoveryUsageAuthorAttempt(t, s, p, c, id)
	var mainArtifact domain.Artifact
	if mode == "original" || mode == "rewritten_original" {
		mainArtifact = complete()
	}
	var original, captured bytes.Buffer
	before, err := s.ExportRecoverySnapshot(ctx, &original, DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal(err)
	}
	if mainArtifact.ID == "" && mode != "missing" {
		mainArtifact = complete()
	}
	if _, err = s.ExportRecoverySnapshot(ctx, &captured, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	originalBytes, lateBytes := original.Bytes(), captured.Bytes()
	// Keep each changed dispatch tuple structurally valid. The selector must
	// reject historical rewrites, not merely malformed timestamps or nulls.
	if mode == "changed_dispatch" || mode == "removed_dispatch" || mode == "candidate_without_dispatch" {
		mutate := func(stage *recoveryStageAuthorFixture) {
			idColumn := recoveryAuthorColumn(t, stage.header, "actions", "id")
			issued := recoveryAuthorColumn(t, stage.header, "actions", "dispatch_issued_at")
			until := recoveryAuthorColumn(t, stage.header, "actions", "dispatch_not_after")
			fence := recoveryAuthorColumn(t, stage.header, "actions", "dispatch_fencing_token")
			found := false
			for _, row := range stage.rows["actions"] {
				if string(row[idColumn]) != r.ActionID {
					continue
				}
				found = true
				if mode == "changed_dispatch" {
					for _, column := range []int{issued, until} {
						if len(row[column]) != 8 {
							t.Fatal("binary dispatch timestamp missing")
						}
						binary.BigEndian.PutUint64(row[column], binary.BigEndian.Uint64(row[column])+1)
					}
				} else {
					row[issued], row[until], row[fence] = nil, nil, nil
				}
			}
			if !found {
				t.Fatal("fixture action missing")
			}
		}
		lateBytes = recoveryStageAuthorRewrite(t, lateBytes, mutate)
		if mode == "candidate_without_dispatch" {
			originalBytes = recoveryStageAuthorRewrite(t, originalBytes, mutate)
		}
	}
	if mode == "rewritten_original" || mode == "wrong_provenance" || mode == "original_collision_disappeared" {
		lateBytes = recoveryStageAuthorRewrite(t, lateBytes, func(stage *recoveryStageAuthorFixture) {
			idColumn := recoveryAuthorColumn(t, stage.header, "artifacts", "id")
			column := recoveryAuthorColumn(t, stage.header, "artifacts", "provenance")
			wanted := mainArtifact.ID
			if mode == "original_collision_disappeared" {
				wanted = other.ID
			}
			for _, row := range stage.rows["artifacts"] {
				if string(row[idColumn]) != wanted {
					continue
				}
				var provenance map[string]any
				if len(row[column]) < 2 || row[column][0] != 1 || json.Unmarshal(row[column][1:], &provenance) != nil {
					t.Fatal("binary jsonb fixture")
				}
				if mode == "wrong_provenance" {
					provenance["reservation_id"] = "res-another"
				} else {
					provenance["provider_request_id"] = "gen-another"
				}
				encoded, _ := json.Marshal(provenance)
				row[column] = append([]byte{1}, encoded...)
			}
		})
	}
	selected, late := recoveryStageAuthorArchive(t, originalBytes), recoveryStageAuthorArchive(t, lateBytes)
	target, spec, _ := recoveryStageAuthorTarget(t, originalBytes)
	if _, err = target.ImportRecoverySnapshot(ctx, selected, spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	f := &evidenceAuthorFixture{target: target, spec: spec, selected: selected, late: late, verified: evidenceAuthorPair(t, selected, late), before: before, lateBytes: lateBytes, oldOrg: p.OrganizationID}
	if _, err = target.IngestRecoveryEvidence(ctx, selected, late, f.verified, spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal("ingest", err)
	}
	return f, RecoveryUsageSelection{OrganizationID: p.OrganizationID, ReservationID: r.ID}
}

func TestRecoveryUsageAuthorSourceBindings(t *testing.T) {
	for _, mode := range []string{"original", "captured", "missing", "empty_id", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			f, selection := recoveryUsageAuthorFixture(t, mode)
			ctx := context.Background()
			plan, err := f.target.PlanRecoveryUsageReadback(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), selection)
			if err != nil || !plan.ExecutionHeld || !plan.RollbackConfirmed || len(plan.PlanSHA256) != 64 {
				t.Fatal("binding plan", err)
			}
			want := "request_identity_available"
			if mode == "missing" || mode == "empty_id" {
				want = "request_identity_unavailable"
			}
			if mode == "unsupported" {
				want = "unsupported_provider_profile"
			}
			if plan.State != want || (want == "request_identity_available") != (plan.GenerationID == "gen-usage-selected") {
				t.Fatal("unexpected binding state")
			}
			if mode == "original" && plan.CandidateOrigin != "original" || mode == "captured" && plan.CandidateOrigin != "captured" {
				t.Fatal("candidate origin lost")
			}
			if mode == "captured" {
				again, err := f.target.PlanRecoveryUsageReadback(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), selection)
				if err != nil || !reflect.DeepEqual(plan, again) {
					t.Fatal("nondeterministic plan", err)
				}
				wrong := selection
				wrong.OrganizationID = "org-other"
				failed, err := f.target.PlanRecoveryUsageReadback(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), wrong)
				if err == nil || !reflect.DeepEqual(failed, RecoveryUsageReadbackPlan{}) {
					t.Fatal("cross organization or partial success")
				}
				cancelled, stop := context.WithCancel(ctx)
				stop()
				failed, err = f.target.PlanRecoveryUsageReadback(cancelled, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), selection)
				if err == nil || !reflect.DeepEqual(failed, RecoveryUsageReadbackPlan{}) {
					t.Fatal("cancelled plan accepted")
				}
			}
			if f.target.CheckRuntimeAdmission(ctx) != RecoveryHeld {
				t.Fatal("readback planning cleared hold")
			}
		})
	}
}

func TestRecoveryUsageAuthorRejectsRewrittenAndAmbiguousIdentity(t *testing.T) {
	for _, mode := range []string{"rewritten_original", "wrong_provenance", "captured_collision", "original_collision_disappeared", "changed_dispatch", "removed_dispatch", "candidate_without_dispatch"} {
		t.Run(mode, func(t *testing.T) {
			f, selection := recoveryUsageAuthorFixture(t, mode)
			plan, err := f.target.PlanRecoveryUsageReadback(context.Background(), f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits(), selection)
			if err == nil || !reflect.DeepEqual(plan, RecoveryUsageReadbackPlan{}) {
				t.Fatal("rewritten or ambiguous identity accepted")
			}
		})
	}
}
