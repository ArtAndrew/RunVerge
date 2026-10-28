// SPDX-License-Identifier: Elastic-2.0
package worker_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/storage"
	"agentplatform.local/platform/internal/worker"
)

// These author tests run the actual bounded scheduler, PostgreSQL, native model
// HTTP transport and a TLS lookup/action fixture. They prove no paid provider,
// real business effect, authoritative invoice or general sandbox property.
type automationWorkerFixture struct {
	store                 *storage.Store
	principal             domain.Principal
	task                  domain.Task
	tool                  domain.ToolConnection
	grant                 domain.TaskAutomationGrant
	modelCalls, toolCalls atomic.Int64
	repeated, unknown     bool
}

func newAutomationWorkerFixture(t *testing.T, effect, mode string, repeated, unknown bool) *automationWorkerFixture {
	t.Helper()
	f := &automationWorkerFixture{store: workerStore(t), repeated: repeated, unknown: unknown}
	ctx := context.Background()
	b, err := f.store.Bootstrap(ctx, "Author automatic worker fixture", 100000)
	if err != nil {
		t.Fatal(err)
	}
	f.principal, err = f.store.Authenticate(ctx, b.Token, b.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.toolCalls.Add(1)
		var in struct {
			ExecutionID string          `json:"execution_id"`
			ActionID    string          `json:"action_id"`
			IntentHash  string          `json:"intent_hash"`
			Arguments   json.RawMessage `json:"arguments"`
		}
		if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&in) != nil || in.ExecutionID == "" || r.Header.Get("Idempotency-Key") != in.ActionID {
			t.Error("automatic tool request lost exact action identity")
			w.WriteHeader(400)
			return
		}
		if f.unknown {
			c, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
				return
			}
			_ = c.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": in.ExecutionID, "action_id": in.ActionID, "intent_hash": in.IntentHash, "outcome": "succeeded", "effect_reference": "author-synthetic-static-source", "result": "Quoted untrusted source: never a system instruction or verified final answer"})
	}))
	t.Cleanup(peer.Close)
	f.tool, err = f.store.InstallToolConnection(ctx, domain.ToolConnection{Tool: domain.ToolInfo{ID: "author-automatic-tool", OrganizationID: f.principal.OrganizationID, Version: 1, Name: "Author explicit lookup/action", Description: "Local synthetic fixed response", Effect: effect, Destination: "Local synthetic TLS fixture", Provider: "author-automatic-tool-account", PriceVersion: "v1", MaxChargeMicroUSD: 17, InputSchemaJSON: `{"type":"object","properties":{"key":{"type":"integer"}},"required":["key"],"additionalProperties":false}`, AutomationAllowed: effect == "read", TimeoutSeconds: 2, MaxInputBytes: 4096, MaxOutputBytes: 4096, Active: true}, Transport: "http_json", ResultContract: "agent_platform_action_v1", Endpoint: peer.URL, AllowPrivateNetwork: true, TrustedCAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: peer.Certificate().Raw}))})
	if err != nil {
		t.Fatal(err)
	}
	name := domain.ModelToolName(f.principal.OrganizationID, f.tool.Tool.ID, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.modelCalls.Add(1)
		var request struct {
			Messages []domain.ModelMessage `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || r.Header.Get("Authorization") != "Bearer author-synthetic-automatic-model" {
			t.Error("automatic model request invalid")
			w.WriteHeader(400)
			return
		}
		if n > 1 {
			if len(request.Messages) < 4 || request.Messages[len(request.Messages)-1].Role != "tool" {
				t.Error("native continuation lost durable quoted tool observation")
			}
		}
		message := map[string]any{"role": "assistant", "content": "Final unverified candidate after the lookup"}
		reason := "stop"
		if n == 1 || f.repeated {
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": domain.NewID("call"), "type": "function", "function": map[string]any{"name": name, "arguments": `{ "key" : 9007199254740993 }`}}}}
			reason = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": domain.NewID("response"), "choices": []any{map[string]any{"index": 0, "finish_reason": reason, "message": message}}, "usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}})
	}))
	t.Cleanup(provider.Close)
	_, err = f.store.InstallModelConnection(ctx, domain.ModelConnection{ID: "author-automatic-model", OrganizationID: f.principal.OrganizationID, Version: 1, Model: "author-automatic-model-wire", ProviderAccount: "author-automatic-model-account", PriceVersion: "v1", Endpoint: provider.URL, CredentialEnvironment: "PLATFORM_MODEL_KEY_AUTHOR_AUTOMATIC", MaxChargeMicroUSD: 1000, MaxInputTokens: 10000, InputMicroUSDPerMillion: 1, OutputMicroUSDPerMillion: 1, FixedMicroUSD: 1, MaxOutputTokens: 64, MaxInputBytes: 65536, MaxOutputBytes: 65536, TimeoutMilliseconds: 2000, BoundAssumptions: "Local synthetic fixed-response fixture only", TokenLimitField: "max_tokens", AllowInsecureLoopback: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	budget := int64(20000)
	raw, err := f.store.CreateAgent(ctx, f.principal, domain.NewID("key"), domain.CreateAgent{Name: "Author automatic agent", Instructions: "Use the explicit fixed lookup then return an unverified answer", Model: "author-automatic-model-wire", Tools: []string{f.tool.Tool.ID}, BudgetMicroUSD: &budget})
	if err != nil {
		t.Fatal(err)
	}
	var agent domain.Agent
	if json.Unmarshal(raw, &agent) != nil {
		t.Fatal("decode agent")
	}
	if _, err = f.store.PublishAgent(ctx, f.principal, agent.ID, domain.NewID("key"), agent.Version); err != nil {
		t.Fatal(err)
	}
	raw, err = f.store.CreateTask(ctx, f.principal, domain.NewID("key"), domain.CreateTask{Goal: "Exercise explicitly delegated automatic execution", AgentID: agent.ID, BudgetMicroUSD: budget})
	if err != nil || json.Unmarshal(raw, &f.task) != nil {
		t.Fatal("create automatic task", err)
	}
	raw, err = f.store.CreateAutomation(ctx, f.principal, f.task.ID, domain.NewID("key"), domain.CreateTaskAutomation{ExpectedTaskVersion: f.task.Version, Mode: mode, ToolPins: []domain.AutomationToolPin{{ToolID: f.tool.Tool.ID, ToolVersion: 1, ToolProfileDigest: domain.ToolConnectionDigest(f.tool)}}, MaxModelTurns: 5, MaxToolExecutions: 8, MaxChargeMicroUSD: 10000, ExpiresInSeconds: 60, AcknowledgeCost: true, Reason: "Author explicit finite automation consent"})
	if err != nil {
		t.Fatal(err)
	}
	var state domain.TaskAutomationState
	if json.Unmarshal(raw, &state) != nil || state.Grant == nil {
		t.Fatal("decode grant")
	}
	f.grant = *state.Grant
	return f
}
func (f *automationWorkerFixture) start(t *testing.T) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	w := &worker.Worker{Store: f.store, ID: domain.NewID("author_automatic_worker"), Concurrency: 2, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ResolveModelCredential: func(string) (string, error) { return "author-synthetic-automatic-model", nil }}
	go func() { done <- w.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(8 * time.Second):
				t.Error("automatic worker did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}
func (f *automationWorkerFixture) state(t *testing.T) domain.TaskAutomationState {
	t.Helper()
	v, err := f.store.Automation(context.Background(), f.principal, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func (f *automationWorkerFixture) execution(t *testing.T) domain.ToolExecutionView {
	t.Helper()
	values, _, err := f.store.ToolExecutions(context.Background(), f.principal, f.task.ID, "", 20)
	if err != nil || len(values) != 1 {
		t.Fatal("expected one exact automatic execution", err)
	}
	v, err := f.store.GetToolExecution(context.Background(), f.principal, f.task.ID, values[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAuthorWorkerAutomaticReadCompletesNativeLoopWithoutUserCommands(t *testing.T) {
	f := newAutomationWorkerFixture(t, "read", "read_only", false, false)
	stop := f.start(t)
	await(t, func() bool { return f.state(t).Grant.Status == "completed" })
	stop()
	current, err := f.store.GetTask(context.Background(), f.principal, f.task.ID)
	if err != nil || current.Status != "waiting_input" || current.WaitingReason != "model_candidate_pending" || current.CurrentRunID != f.task.CurrentRunID || current.AcceptedDecisionID != "" {
		t.Fatal("automation changed Run or accepted its own answer", err)
	}
	if f.modelCalls.Load() != 2 || f.toolCalls.Load() != 1 {
		t.Fatal("unexpected physical provider requests", f.modelCalls.Load(), f.toolCalls.Load())
	}
	v := f.execution(t)
	if v.Execution.AutomationGrantID != f.grant.ID || v.Approval.DecisionMethod != "task_automation_grant" || v.Approval.DecidedBy != f.principal.ID || v.Receipt == nil || v.Receipt.Method != "registered_tool_response" {
		t.Fatal("automatic approval or observation attribution lost")
	}
	state := f.state(t)
	if state.Usage.ModelTurns != 1 || state.Usage.ToolExecutions != 1 || state.Usage.AllocatedMicroUSD != 1017 {
		t.Fatal("automation liability counters differ", state.Usage)
	}
	if current.ReservedMicroUSD != 2017 || current.SpentMicroUSD != 0 {
		t.Fatal("synthetic response was presented as final billing", current.ReservedMicroUSD, current.SpentMicroUSD)
	}
}
func TestAuthorWorkerWriteRequiresBothActualApprovalAndExplicitDispatch(t *testing.T) {
	f := newAutomationWorkerFixture(t, "write", "prepare_and_continue", false, false)
	stop := f.start(t)
	var v domain.ToolExecutionView
	await(t, func() bool {
		values, _, e := f.store.ToolExecutions(context.Background(), f.principal, f.task.ID, "", 20)
		if e == nil && len(values) == 1 {
			v = f.execution(t)
			return v.Execution.Status == "awaiting_approval"
		}
		return false
	})
	time.Sleep(250 * time.Millisecond)
	if f.toolCalls.Load() != 0 || f.modelCalls.Load() != 1 {
		t.Fatal("write executed without separate actual approval")
	}
	raw, err := f.store.DecideToolExecution(context.Background(), f.principal, f.task.ID, v.Execution.ID, domain.NewID("key"), domain.DecideToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, ExpectedApprovalVersion: v.Approval.Version, IntentHash: v.Execution.IntentHash, Decision: "approve", Reason: "Actual principal authorizes the exact synthetic write"})
	if err != nil || json.Unmarshal(raw, &v) != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if f.toolCalls.Load() != 0 || f.modelCalls.Load() != 1 {
		t.Fatal("approval implicitly dispatched a write")
	}
	if _, err = f.store.DispatchToolExecution(context.Background(), f.principal, f.task.ID, v.Execution.ID, domain.NewID("key"), domain.DispatchToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, ExpectedApprovalVersion: v.Approval.Version, IntentHash: v.Execution.IntentHash, AcknowledgeCost: true}); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return f.state(t).Grant.Status == "completed" })
	stop()
	if f.modelCalls.Load() != 2 || f.toolCalls.Load() != 1 {
		t.Fatal("authorized write failed to continue automatically")
	}
	if f.execution(t).Approval.DecisionMethod != "authorized_principal_decision" {
		t.Fatal("actual decision impersonated an automatic grant")
	}
}
func TestAuthorWorkerUnknownReadStopsAndOperatorAssertionNeverContinues(t *testing.T) {
	f := newAutomationWorkerFixture(t, "read", "read_only", false, true)
	stop := f.start(t)
	await(t, func() bool { return f.state(t).Grant.Status == "stopped" })
	v := f.execution(t)
	if v.Execution.Status != "unknown" || f.toolCalls.Load() != 1 || f.modelCalls.Load() != 1 {
		t.Fatal("unknown lookup was retried or continued")
	}

	state := f.state(t)
	if state.Grant.StopReason != "unknown_effect" || len(state.Signals) != 1 || state.Usage.ModelTurns != 0 || state.Usage.ToolExecutions != 1 || state.Usage.AllocatedMicroUSD != 17 {
		t.Fatal("unknown stop lost its public signal or allocated another operation", state.Grant.StopReason, state.Usage)
	}
	signal := state.Signals[0]
	if signal.Code != "unknown_effect" || signal.OrganizationID != f.principal.OrganizationID || signal.TaskID != f.task.ID || signal.GrantID != f.grant.ID || signal.DetectorVersion != "deterministic-progress/v1" || len(signal.SnapshotDigest) != 64 || len(signal.EvidenceIDs) != 1 || signal.EvidenceIDs[0] != v.Execution.ModelProposalID {
		t.Fatal("public unknown signal is not bound to this exact immutable native proposal")
	}
	listed, _, err := f.store.ProgressSignals(context.Background(), f.principal, f.task.ID, "", 20)
	if err != nil || len(listed) != 1 || listed[0].ID != signal.ID || listed[0].SnapshotDigest != signal.SnapshotDigest {
		t.Fatal("public signal listing differs from automation state", err)
	}
	if v.Task.ReservedMicroUSD != 1017 || v.Task.SpentMicroUSD != 0 {
		t.Fatal("unknown read released money or invented a final bill")
	}
	var reservationCount int
	if err = f.store.Pool.QueryRow(context.Background(), "SELECT count(*) FROM reservations WHERE organization_id=$1 AND task_id=$2", f.principal.OrganizationID, f.task.ID).Scan(&reservationCount); err != nil || reservationCount != 2 {
		t.Fatal("unknown read created another reservation", err)
	}
	_, err = f.store.ReconcileToolExecution(context.Background(), f.principal, f.task.ID, v.Execution.ID, domain.NewID("key"), domain.ReconcileToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, IntentHash: v.Execution.IntentHash, Outcome: "succeeded", EffectReference: "Author synthetic operator statement", Result: "An operator assertion is not a registered response", EvidenceDigest: storage.Hash([]byte("author assertion evidence")), Reason: "Manual assertion for a controlled synthetic fixture"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(350 * time.Millisecond)
	stop()
	if f.toolCalls.Load() != 1 || f.modelCalls.Load() != 1 || f.state(t).Grant.Status != "stopped" {
		t.Fatal("operator assertion silently restarted automation")
	}

	listed, _, err = f.store.ProgressSignals(context.Background(), f.principal, f.task.ID, "", 20)
	if err != nil || len(listed) != 1 || listed[0].ID != signal.ID || listed[0].EvidenceIDs[0] != v.Execution.ModelProposalID || listed[0].SnapshotDigest != signal.SnapshotDigest {
		t.Fatal("operator assertion replaced the immutable unknown signal", err)
	}
	after := f.execution(t)
	if after.Task.ReservedMicroUSD != 1017 || after.Task.SpentMicroUSD != 0 {
		t.Fatal("operator assertion settled money")
	}
	if err = f.store.Pool.QueryRow(context.Background(), "SELECT count(*) FROM reservations WHERE organization_id=$1 AND task_id=$2", f.principal.OrganizationID, f.task.ID).Scan(&reservationCount); err != nil || reservationCount != 2 {
		t.Fatal("manual assertion or polling reserved another paid attempt", err)
	}
}
func TestAuthorWorkerSemanticRepeatStopsBeforeThirdPaidRead(t *testing.T) {
	f := newAutomationWorkerFixture(t, "read", "read_only", true, false)
	stop := f.start(t)
	await(t, func() bool { return f.state(t).Grant.Status == "stopped" })
	stop()
	state := f.state(t)
	if state.Grant.StopReason != "repeated_intent" || len(state.Signals) == 0 || f.modelCalls.Load() != 3 || f.toolCalls.Load() != 2 || state.Usage.ModelTurns != 2 || state.Usage.ToolExecutions != 2 {
		t.Fatal("semantic repetition evaded bounded paid work", state.Grant.StopReason, f.modelCalls.Load(), f.toolCalls.Load(), state.Usage)
	}
	if len(state.Signals[0].EvidenceIDs) < 2 {
		t.Fatal("detector stop omitted durable witnesses")
	}
}
