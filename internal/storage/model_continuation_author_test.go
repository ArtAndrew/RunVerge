// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
)

// These are author tests. The independent fixture is reused only for isolated
// PostgreSQL and synthetic local peers; these results are not independent review.
func TestAuthorNativeKnownUnsentCancellationContinuesWithoutEffectReceipt(t *testing.T) {
	f := independentNativeSetup(t, nil)
	ctx := context.Background()
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	proposal := f.proposal(t)
	v := f.decide(t, f.prepare(t, proposal), "approve")
	raw, err := f.s.DispatchToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDispatch(v))
	v = independentToolView(t, raw, err)
	l, _, err := f.s.ClaimToolExecution(ctx, "author-known-unsent", 30*time.Second)
	if err != nil || l == nil {
		t.Fatal("cannot claim synthetic native action", err)
	}
	if err = f.s.PauseToolExecution(ctx, l, "tool_credentials_unavailable"); err != nil {
		t.Fatal(err)
	}
	v, err = f.s.GetToolExecution(ctx, f.p, f.task.ID, v.Execution.ID)
	if err != nil || v.Execution.Status != "cancelled" || v.Execution.Reason != "tool_credentials_unavailable" || v.Receipt != nil || v.Task.WaitingReason != "model_tools_pending" {
		t.Fatal("known unsent cancellation was presented as an external rejection", err)
	}
	var status, action string
	var unsent bool
	if err = f.s.Pool.QueryRow(ctx, "SELECT r.status,a.status,a.dispatch_issued_at IS NULL FROM reservations r JOIN actions a ON a.organization_id=r.organization_id AND a.id=r.action_id WHERE r.organization_id=$1 AND r.id=$2", f.p.OrganizationID, v.Execution.ReservationID).Scan(&status, &action, &unsent); err != nil || status != "released" || action != "rejected" || !unsent || f.toolCalls.Load() != 0 {
		t.Fatal("reservation released without proven zero outbound dispatch", err)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 300, 0)
	if _, err = f.s.ContinueModel(ctx, f.p, f.task.ID, domain.NewID("key"), independentNativeContinue(proposal, v)); err != nil {
		t.Fatal("known unsent cancellation could not be quoted to the next model turn", err)
	}
	if _, err = f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	if f.modelCalls.Load() != 2 || f.toolCalls.Load() != 0 || f.current(t).CurrentRunID != f.task.CurrentRunID {
		t.Fatal("continuation resent an effect or created another Run")
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 600, 0)
	f.mu.Lock()
	defer f.mu.Unlock()
	var result modelQuotedToolResult
	if len(f.requests) != 2 || len(f.requests[1].Messages) != 4 || json.Unmarshal([]byte(f.requests[1].Messages[3].Content), &result) != nil || result.Status != "cancelled" || result.Receipt != nil || !result.Untrusted {
		t.Fatal("native transcript invented an effect receipt for a cancelled unsent action")
	}
}

func TestAuthorNativeStoreRejectsWholeInvalidBatchBeforePublishing(t *testing.T) {
	f := independentNativeSetup(t, nil)
	ctx := context.Background()
	l, _, err := f.s.ClaimModel(ctx, "author-whole-batch", 30*time.Second)
	if err != nil || l == nil {
		t.Fatal(err)
	}
	runtime, err := modelruntime.New(f.s, RuntimeModelConfig(f.model, "independent-native-model-key"))
	if err != nil {
		t.Fatal(err)
	}
	key, intent, err := runtime.DescribeIntent(*l)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.s.ReserveCost(ctx, l, domain.ReserveCostRequest{ChargeKey: key, Category: "model", Provider: f.model.ProviderAccount, PriceVersion: f.model.PriceVersion, AmountMicroUSD: f.model.MaxChargeMicroUSD, IntentHash: intent})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.PrepareModelDispatch(ctx, l, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.MarkCostUnknown(ctx, f.p.OrganizationID, r.ID, "author-synthetic-attempt", "model_accounting_pending"); err != nil {
		t.Fatal(err)
	}
	estimate, err := RuntimeModelConfig(f.model, "author-price-validation").Price.Estimate(3, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.CompleteModelCandidate(ctx, l, r.ID, domain.ModelCandidate{Model: f.model.Model, PriceVersion: f.model.PriceVersion, IntentDigest: intent, InputTokens: 3, OutputTokens: 2, EstimatedMicroUSD: estimate, ToolCalls: []domain.ModelToolCall{{ID: "valid_call", Name: f.agent.ModelTools[0].Name, ArgumentsJSON: independentNativeArguments}, {ID: "invalid_call", Name: f.agent.ModelTools[0].Name, ArgumentsJSON: `{"number":"wrong","note":"rejected"}`}}})
	code(t, err, "invalid_request")
	var artifacts, proposals, candidates int
	if err = f.s.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM artifacts WHERE organization_id=$1 AND task_id=$2),(SELECT count(*) FROM model_tool_proposals WHERE organization_id=$1 AND task_id=$2),(SELECT count(*) FROM model_candidates WHERE organization_id=$1 AND task_id=$2)", f.p.OrganizationID, f.task.ID).Scan(&artifacts, &proposals, &candidates); err != nil || artifacts != 0 || proposals != 0 || candidates != 0 || f.toolCalls.Load() != 0 || f.modelCalls.Load() != 0 {
		t.Fatal("a rejected native batch partially published or dispatched", artifacts, proposals, candidates, err)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 300, 0)
}

func TestAuthorNativeStoreAdmissionRebuildsMessagesAndToolPins(t *testing.T) {
	for _, kind := range []string{"transcript", "tool_pin"} {
		t.Run(kind, func(t *testing.T) {
			f := independentNativeSetup(t, nil)
			ctx := context.Background()
			l, _, err := f.s.ClaimModel(ctx, "author-forged-native-context", 30*time.Second)
			if err != nil || l == nil {
				t.Fatal(err)
			}
			runtime, err := modelruntime.New(f.s, RuntimeModelConfig(f.model, "independent-native-model-key"))
			if err != nil {
				t.Fatal(err)
			}
			key, intent, err := runtime.DescribeIntent(*l)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "transcript" {
				l.ModelMessages[1].Content = "A different injected goal"
			} else {
				l.ModelTools[0].ToolVersion++
			}
			_, err = f.s.ReserveCost(ctx, l, domain.ReserveCostRequest{ChargeKey: key, Category: "model", Provider: f.model.ProviderAccount, PriceVersion: f.model.PriceVersion, AmountMicroUSD: f.model.MaxChargeMicroUSD, IntentHash: intent})
			code(t, err, "conflict")
			var count int
			if err = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM reservations WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&count); err != nil || count != 0 || f.modelCalls.Load() != 0 {
				t.Fatal("forged context admitted cost or external dispatch", err)
			}
		})
	}
}

func TestAuthorDurableModelTranscriptRejectsAmbiguousJSON(t *testing.T) {
	for name, raw := range map[string]string{
		"duplicate_role":     `[{"role":"system","role":"user","content":"a"},{"role":"user","content":"b"}]`,
		"unknown_field":      `[{"role":"system","content":"a","authority":"admin"},{"role":"user","content":"b"}]`,
		"unpaired_surrogate": `[{"role":"system","content":"\ud800"},{"role":"user","content":"b"}]`,
		"non_array":          `{"role":"system","content":"a"}`,
		"trailing_value":     `[{"role":"system","content":"a"},{"role":"user","content":"b"}] true`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeModelMessages(raw); err == nil {
				t.Fatal("ambiguous durable transcript accepted")
			}
		})
	}
}

func TestAuthorModelUpgradePreservesNearLimitLegacyInput(t *testing.T) {
	// Stage the real immutable history through013. The current CreateAgent
	// scanner requires014 columns, so historical fixture rows use old SQL.
	s := independentStoreAt003(t)
	ctx := context.Background()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	files, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.Name() <= "003_execution_ledger.sql" || file.Name() >= "014_" {
			continue
		}
		name := "migrations/" + file.Name()
		body, err := migrations.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, string(body)); err != nil {
			t.Fatal(name, err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(name,sha256) VALUES($1,$2)", name, Hash(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// This fixture is deliberately still on schema013. Current Authenticate
	// requires credential-origin columns introduced by016, so retain the exact
	// principal created by a current constructor during an explicitly bounded
	// test-only absence-of-old-gate callback; this is not an old binary run.
	var bootstrap Bootstrap
	historicalFixtureWithoutRecoveryAdmission(t, s, func() {
		bootstrap, err = s.Bootstrap(ctx, "Historical author upgrade fixture", 100000)
		if err != nil {
			t.Fatal(err)
		}
	})
	p := domain.Principal{ID: bootstrap.PrincipalID, OrganizationID: bootstrap.OrganizationID, Role: "owner", TokenHash: Hash([]byte(bootstrap.Token)), CredentialKind: "bearer"}
	profile := domain.ModelConnection{ID: "author-large-upgrade-model", OrganizationID: p.OrganizationID, Version: 1, Model: "author-large-upgrade", ProviderAccount: "synthetic-upgrade-account", PriceVersion: "v1", Endpoint: "https://example.invalid/model", CredentialEnvironment: "PLATFORM_MODEL_KEY_UPGRADE_LARGE", MaxChargeMicroUSD: 1000, MaxInputTokens: 100000, InputMicroUSDPerMillion: 1, OutputMicroUSDPerMillion: 1, FixedMicroUSD: 1, MaxOutputTokens: 256, MaxInputBytes: 1 << 20, MaxOutputBytes: 4096, TimeoutMilliseconds: 1000, BoundAssumptions: "Historical synthetic input only; no external inference", TokenLimitField: "max_tokens", Active: true}
	historicalFixtureWithoutRecoveryAdmission(t, s, func() {
		if _, err = s.InstallModelConnection(ctx, profile); err != nil {
			t.Fatal(err)
		}
	})
	agentID, taskID, runID, opID := domain.NewID("agt"), domain.NewID("tsk"), domain.NewID("run"), domain.NewID("op")
	instructions := strings.Repeat("s", 32768)
	if _, err = s.Pool.Exec(ctx, "INSERT INTO agents(organization_id,id,name,instructions,model,tools,budget_microusd,published) VALUES($1,$2,'Historical large context',$3,$4,'[]',10000,true)", p.OrganizationID, agentID, instructions, profile.Model); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("a", 262144)
	sources := make([]domain.RunSource, 3)
	for i := range sources {
		sources[i] = domain.RunSource{TaskID: taskID, CandidateID: domain.NewID("cand"), ArtifactID: domain.NewID("art"), ArtifactVersion: 1, ContentDigest: Hash([]byte(content)), Content: content}
	}
	parent, _ := json.Marshal(map[string]string{"quoted": strings.Repeat("p", 111000)})
	input, inputDigest, err := BuildRunInput(domain.RunInput{OriginalGoal: "Historical exact goal", Sources: sources, ParentContext: parent})
	if err != nil || len(input) < 890000 || len(input) > domain.MaxRunInputBytes {
		t.Fatal("invalid near-limit legacy fixture", len(input), err)
	}
	if _, err = s.Pool.Exec(ctx, "INSERT INTO tasks(organization_id,id,owner_id,agent_id,agent_version,goal,status,budget_microusd,model_connection_id,model_connection_version) VALUES($1,$2,$3,$4,1,'Historical exact goal','queued',10000,$5,1)", p.OrganizationID, taskID, p.ID, agentID, profile.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, "INSERT INTO runs(organization_id,id,task_id,principal_id,agent_id,agent_version,status,input_context,input_digest) VALUES($1,$2,$3,$4,$5,1,'queued',$6,$7)", p.OrganizationID, runID, taskID, p.ID, agentID, input, inputDigest); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, "INSERT INTO operations(organization_id,id,task_id,run_id,status) VALUES($1,$2,$3,$4,'queued')", p.OrganizationID, opID, taskID, runID); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("upgrade rejected an existing valid near-limit context", err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("repeated migration changed historical context", err)
	}
	var raw, digest, restoredInput string
	if err = s.Pool.QueryRow(ctx, "SELECT m.messages_json,m.messages_digest,r.input_context FROM model_turns m JOIN runs r ON r.organization_id=m.organization_id AND r.id=m.run_id WHERE m.organization_id=$1 AND m.operation_id=$2", p.OrganizationID, opID).Scan(&raw, &digest, &restoredInput); err != nil || len(raw) <= domain.MaxRunInputBytes || Hash([]byte(raw)) != digest || restoredInput != input {
		t.Fatal("upgrade truncated, changed or omitted the large immutable input", len(raw), err)
	}
	messages, err := decodeModelMessages(raw)
	if err != nil || len(messages) != 2 || messages[0].Content != instructions || messages[1].Content != input {
		t.Fatal("upgraded transcript did not preserve exact source bytes", err)
	}
	t.Logf("preserved legacy input bytes=%d; full transcript bytes=%d", len(input), len(raw))
	l, _, err := s.ClaimModel(ctx, "author-upgraded-legacy-worker", 30*time.Second)
	if err != nil || l == nil || len(l.ModelMessages) != 0 || l.RunInput != input {
		t.Fatal("legacy first-turn intent compatibility was lost", err)
	}
	runtime, err := modelruntime.New(s, RuntimeModelConfig(profile, "synthetic-upgrade-no-dispatch"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = runtime.DescribeIntent(*l); err != nil {
		t.Fatal("existing valid model wire request no longer fits after transcript migration", err)
	}
}
