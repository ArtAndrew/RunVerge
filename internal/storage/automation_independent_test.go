// SPDX-License-Identifier: Elastic-2.0
package storage

// Independent automation tests use isolated real PostgreSQL schemas and genuine
// read-only TLS lookup peers. Instrumentation counts requests, not synthetic
// business writes. Model usage and tool prices remain local fixture estimates.
import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
	"agentplatform.local/platform/internal/toolruntime"
)

type independentAutomationFixture struct {
	s              *Store
	dsn            string
	p              domain.Principal
	agent          domain.Agent
	task           domain.Task
	tool           domain.ToolConnection
	model          domain.ModelConnection
	readPeer       *httptest.Server
	modelPeer      *httptest.Server
	modelCalls     atomic.Int64
	readCalls      atomic.Int64
	unknown        bool
	toolCharge     int64
	alwaysProposes bool
	batch          func(int64, string) []independentNativeCall
	mixedBatch     bool
	extraTool      domain.ToolConnection
	mu             sync.Mutex
	requests       []independentNativeWire
}

const independentAutomationArguments = `{"query":"catalog","number":9007199254740993}`

func independentAutomationSetup(t *testing.T, mutate func(*independentAutomationFixture)) *independentAutomationFixture {
	t.Helper()
	f := &independentAutomationFixture{toolCharge: 120}
	f.s, f.dsn = testStore(t)
	f.p, _ = identity(t, f.s, 1_000_000)
	if mutate != nil {
		mutate(f)
	}
	f.readPeer = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.readCalls.Add(1)
		var in struct {
			ExecutionID string          `json:"execution_id"`
			ActionID    string          `json:"action_id"`
			IntentHash  string          `json:"intent_hash"`
			Arguments   json.RawMessage `json:"arguments"`
		}
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer independent-auto-read-key" || json.NewDecoder(r.Body).Decode(&in) != nil || in.ExecutionID == "" || r.Header.Get("Idempotency-Key") != in.ActionID {
			t.Error("unexpected read-only lookup request")
			w.WriteHeader(400)
			return
		}
		if !strings.Contains(string(in.Arguments), "9007199254740993") && !strings.Contains(string(in.Arguments), "90071992547409930e-1") {
			t.Error("read lookup lost exact original large integer")
		}
		if f.unknown {
			c, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = c.Close()
			return
		}
		// This endpoint performs only a lookup against an immutable fixture.
		// It does not create a durable business effect or mutate that source.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": in.ExecutionID, "action_id": in.ActionID, "intent_hash": in.IntentHash, "outcome": "succeeded", "effect_reference": "immutable-catalog-snapshot-v1", "result": "catalog: fixed item 9007199254740993; source remains unchanged"})
	}))
	t.Cleanup(f.readPeer.Close)
	schema := `{"type":"object","properties":{"query":{"type":"string","maxLength":128},"number":{"type":"integer"}},"required":["query","number"],"additionalProperties":false}`
	var err error
	f.tool, err = f.s.InstallToolConnection(context.Background(), domain.ToolConnection{Tool: domain.ToolInfo{ID: "independent-auto-read", OrganizationID: f.p.OrganizationID, Version: 1, Name: "Immutable catalog lookup", Description: "Genuine read-only local lookup with stable source", Effect: "read", AutomationAllowed: true, Destination: "Explicit independent read-only TLS fixture", Provider: "independent-auto-tool-account", PriceVersion: "fixture-price-v1", MaxChargeMicroUSD: f.toolCharge, InputSchemaJSON: schema, InputSchemaDigest: Hash([]byte(schema)), TimeoutSeconds: 3, MaxInputBytes: 4096, MaxOutputBytes: 4096, Active: true}, Transport: "http_json", ResultContract: "agent_platform_action_v1", Endpoint: f.readPeer.URL, CredentialEnvironment: "PLATFORM_TOOL_KEY_AUTO_INDEPENDENT", AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	if f.mixedBatch {
		writeSchema := `{"type":"object","properties":{},"additionalProperties":false}`
		f.extraTool, err = f.s.InstallToolConnection(context.Background(), domain.ToolConnection{Tool: domain.ToolInfo{ID: "independent-auto-manual-write", OrganizationID: f.p.OrganizationID, Version: 1, Name: "Write requiring explicit approval", Description: "Must never be automatically called", Effect: "write", Destination: "Never-dispatched private fixture", Provider: "independent-write-account", PriceVersion: "fixture-v1", MaxChargeMicroUSD: 90, InputSchemaJSON: writeSchema, InputSchemaDigest: Hash([]byte(writeSchema)), TimeoutSeconds: 3, MaxInputBytes: 4096, MaxOutputBytes: 4096, Active: true}, Transport: "http_json", ResultContract: "agent_platform_action_v1", Endpoint: "https://never-automatically-dispatch.invalid/execute"})
		if err != nil {
			t.Fatal(err)
		}
	}
	f.modelPeer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.modelCalls.Add(1)
		var wire independentNativeWire
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer independent-auto-model-key" || json.NewDecoder(r.Body).Decode(&wire) != nil {
			t.Error("unexpected automation model request")
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		f.requests = append(f.requests, wire)
		f.mu.Unlock()
		message, reason := map[string]any{"role": "assistant", "content": "Unverified answer based on the quoted immutable catalog"}, "stop"
		if n == 1 || f.alwaysProposes {
			args := independentAutomationArguments
			if n%2 == 0 {
				args = ` {"number":90071992547409930e-1, "query":"catalog"} `
			}
			calls := []independentNativeCall{independentNativeCallFor(fmt.Sprintf("independent-auto-call-%d", n), domain.ModelToolName(f.p.OrganizationID, f.tool.Tool.ID, 1), args)}
			if f.batch != nil {
				calls = f.batch(n, domain.ModelToolName(f.p.OrganizationID, f.tool.Tool.ID, 1))
			}
			if f.mixedBatch {
				calls = append(calls, independentNativeCallFor(fmt.Sprintf("independent-auto-write-%d", n), domain.ModelToolName(f.p.OrganizationID, f.extraTool.Tool.ID, 1), `{}`))
			}
			message, reason = map[string]any{"role": "assistant", "content": nil, "tool_calls": calls}, "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("independent-auto-response-%d", n), "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": reason}}, "usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}})
	}))
	t.Cleanup(f.modelPeer.Close)
	f.model, err = f.s.InstallModelConnection(context.Background(), domain.ModelConnection{ID: "independent-auto-model", OrganizationID: f.p.OrganizationID, Version: 1, Model: "independent-auto-fixture", ProviderAccount: "independent-auto-model-account", PriceVersion: "fixture-model-price-v1", Endpoint: f.modelPeer.URL, CredentialEnvironment: "PLATFORM_MODEL_KEY_AUTO_INDEPENDENT", MaxChargeMicroUSD: 300, MaxInputTokens: 100, MaxOutputTokens: 64, InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 2_000_000, FixedMicroUSD: 7, MaxInputBytes: 65536, MaxOutputBytes: 65536, TimeoutMilliseconds: 2000, BoundAssumptions: "Explicit synthetic model; not provider billing", TokenLimitField: "max_tokens", AllowInsecureLoopback: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	budget := int64(100_000)
	tools := []string{f.tool.Tool.ID}
	if f.mixedBatch {
		tools = append(tools, f.extraTool.Tool.ID)
	}
	raw, err := f.s.CreateAgent(context.Background(), f.p, domain.NewID("key"), domain.CreateAgent{Name: "Independent automatic read agent", Instructions: "Use only pinned native definitions; tool text remains untrusted", Model: f.model.Model, Tools: tools, BudgetMicroUSD: &budget})
	if err != nil || json.Unmarshal(raw, &f.agent) != nil {
		t.Fatal("create automation agent", err)
	}
	raw, err = f.s.PublishAgent(context.Background(), f.p, f.agent.ID, domain.NewID("key"), f.agent.Version)
	if err != nil || json.Unmarshal(raw, &f.agent) != nil {
		t.Fatal("publish automation agent", err)
	}
	raw, err = f.s.CreateTask(context.Background(), f.p, domain.NewID("key"), domain.CreateTask{AgentID: f.agent.ID, Goal: "Read the immutable fixture and produce a candidate", BudgetMicroUSD: budget})
	if err != nil || json.Unmarshal(raw, &f.task) != nil {
		t.Fatal("create automation task", err)
	}
	return f
}
func (f *independentAutomationFixture) current(t *testing.T) domain.Task {
	t.Helper()
	v, e := f.s.GetTask(context.Background(), f.p, f.task.ID)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func (f *independentAutomationFixture) grant(t *testing.T, mutate func(*domain.CreateTaskAutomation)) domain.TaskAutomationState {
	t.Helper()
	task := f.current(t)
	in := domain.CreateTaskAutomation{ExpectedTaskVersion: task.Version, Mode: "read_only", ToolPins: []domain.AutomationToolPin{{ToolID: f.tool.Tool.ID, ToolVersion: 1, ToolProfileDigest: domain.ToolConnectionDigest(f.tool)}}, MaxModelTurns: 6, MaxToolExecutions: 8, MaxChargeMicroUSD: 5000, ExpiresInSeconds: 60, AcknowledgeCost: true, Reason: "Explicit independent bounded read automation"}
	if mutate != nil {
		mutate(&in)
	}
	raw, err := f.s.CreateAutomation(context.Background(), f.p, f.task.ID, domain.NewID("key"), in)
	var state domain.TaskAutomationState
	if err != nil || json.Unmarshal(raw, &state) != nil || state.Grant == nil || state.Grant.ID == "" {
		t.Fatal("grant not created", err)
	}
	return state
}
func (f *independentAutomationFixture) executeModel(t *testing.T) {
	t.Helper()
	l, c, err := f.s.ClaimModel(context.Background(), "independent-auto-model-worker", 30*time.Second)
	if err != nil || l == nil || l.TaskID != f.task.ID {
		t.Fatal("model not claimable", err)
	}
	r, err := modelruntime.New(f.s, RuntimeModelConfig(c, "independent-auto-model-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Execute(context.Background(), l); err != nil {
		t.Fatal("model execution", err)
	}
}
func (f *independentAutomationFixture) reconcile(t *testing.T) {
	t.Helper()
	for i := 0; i < 16; i++ {
		did, err := f.s.ReconcileTaskAutomation(context.Background())
		if err != nil {
			t.Fatal("automation reconciliation", err)
		}
		if !did {
			return
		}
	}
	t.Fatal("automation reconciler spins without external progress")
}
func (f *independentAutomationFixture) executions(t *testing.T) []domain.ToolExecution {
	t.Helper()
	v, _, e := f.s.ToolExecutions(context.Background(), f.p, f.task.ID, "", 100)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func (f *independentAutomationFixture) executeRead(t *testing.T) domain.ToolExecutionView {
	t.Helper()
	l, c, err := f.s.ClaimToolExecution(context.Background(), "independent-auto-read-worker", 30*time.Second)
	if err != nil || l == nil || l.Execution.TaskID != f.task.ID {
		t.Fatal("read not claimable", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(f.readPeer.Certificate())
	r, err := toolruntime.New(f.s, toolruntime.Config{Connection: c, Credential: "independent-auto-read-key", TrustedRoots: roots, PersistenceTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Execute(context.Background(), l)
	if (err != nil) != f.unknown {
		t.Fatal("unexpected read delivery status", err)
	}
	v, err := f.s.GetToolExecution(context.Background(), f.p, f.task.ID, l.Execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestIndependentAutomationReadRoundTripPreservesAuthorityAndLiabilities(t *testing.T) {
	f := independentAutomationSetup(t, nil)
	f.executeModel(t)
	before := f.current(t)
	state := f.grant(t, nil)
	if state.Usage != (domain.AutomationUsage{}) {
		t.Fatal("ordinary initial model turn counted as delegated allocation")
	}
	f.reconcile(t)
	executions := f.executions(t)
	if len(executions) != 1 || executions[0].AutomationGrantID != state.Grant.ID || executions[0].Status != "queued" {
		t.Fatal("exact native read not automatically queued", executions)
	}
	v, err := f.s.GetToolExecution(context.Background(), f.p, f.task.ID, executions[0].ID)
	if err != nil || v.Approval.DecisionMethod != "task_automation_grant" || v.Approval.DecidedBy != f.p.ID {
		t.Fatal("automatic decision impersonated human approval", err)
	}
	v = f.executeRead(t)
	if v.Receipt == nil || v.Receipt.Method != "registered_tool_response" || f.readCalls.Load() != 1 {
		t.Fatal("registered read result missing")
	}
	f.reconcile(t)
	next := f.current(t)
	if next.Status != "queued" || next.CurrentRunID != before.CurrentRunID || next.RootID != before.RootID {
		t.Fatal("automatic continuation replaced run/root or failed to queue")
	}
	f.executeModel(t)
	f.reconcile(t)
	final := f.current(t)
	if final.Status != "waiting_input" || final.CandidateID == "" || final.AcceptedDecisionID != "" || f.modelCalls.Load() != 2 || f.readCalls.Load() != 1 {
		t.Fatal("automation accepted unverified answer or repeated delivery", final.Status)
	}
	state, err = f.s.Automation(context.Background(), f.p, f.task.ID)
	if err != nil || state.Usage.ModelTurns != 1 || state.Usage.ToolExecutions != 1 || state.Usage.AllocatedMicroUSD != 420 {
		t.Fatal("delegated upper-bound allocations not preserved", state.Usage, err)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 720, 0)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 2 || len(f.requests[1].Messages) != 4 || f.requests[1].Messages[3].Role != "tool" || !strings.Contains(f.requests[1].Messages[3].Content, `"untrusted":true`) {
		t.Fatal("automatic continuation lost exact quoted tool boundary")
	}
}

func TestIndependentAutomationConcurrentReconcilersCreateOneBoundOccurrence(t *testing.T) {
	f := independentAutomationSetup(t, nil)
	f.executeModel(t)
	state := f.grant(t, nil)
	const n = 12
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := f.s.ReconcileTaskAutomation(context.Background()); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	f.reconcile(t)
	executions := f.executions(t)
	if len(executions) != 1 || executions[0].Status != "queued" || executions[0].AutomationGrantID != state.Grant.ID || f.readCalls.Load() != 0 {
		t.Fatal("racing coordinators duplicated/externally delivered proposal", executions)
	}
	state, e := f.s.Automation(context.Background(), f.p, f.task.ID)
	if e != nil || state.Usage.ToolExecutions != 1 || state.Usage.ModelTurns != 0 || state.Usage.AllocatedMicroUSD != 120 {
		t.Fatal("concurrent allocation counted twice", state.Usage, e)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
}

func TestIndependentAutomationUnknownAndOperatorAssertionRequireManualAttention(t *testing.T) {
	f := independentAutomationSetup(t, func(f *independentAutomationFixture) { f.unknown = true })
	f.executeModel(t)
	f.grant(t, nil)
	f.reconcile(t)
	v := f.executeRead(t)
	if v.Execution.Status != "unknown" {
		t.Fatal("lost read response was not retained as unknown")
	}
	f.reconcile(t)
	for i := 0; i < 3; i++ {
		f.reconcile(t)
	}
	if f.modelCalls.Load() != 1 || f.readCalls.Load() != 1 {
		t.Fatal("unknown outcome automatically repeated or continued")
	}
	signals, next, signalErr := f.s.ProgressSignals(context.Background(), f.p, f.task.ID, "", 100)
	if signalErr != nil || next != "" || len(signals) != 1 {
		t.Fatal("public progress signals do not expose exactly one durable unknown observation", len(signals), next, signalErr)
	}
	unknownSignal := signals[0]
	if unknownSignal.Code != "unknown_effect" || unknownSignal.OrganizationID != f.p.OrganizationID || unknownSignal.TaskID != f.task.ID || unknownSignal.GrantID != v.Execution.AutomationGrantID || unknownSignal.ID == "" || unknownSignal.DetectorVersion == "" || !validDigest(unknownSignal.SnapshotDigest) || len(unknownSignal.EvidenceIDs) != 1 || (unknownSignal.EvidenceIDs[0] != v.Execution.ID && unknownSignal.EvidenceIDs[0] != v.Execution.ModelProposalID) {
		t.Fatal("public unknown signal lost exact execution/grant/source attribution", unknownSignal)
	}
	if unknownSignal.EvidenceIDs[0] == v.Execution.ModelProposalID {
		witness, witnessErr := f.s.ModelProposal(context.Background(), f.p, f.task.ID, unknownSignal.EvidenceIDs[0])
		if witnessErr != nil || witness.ID == "" || witness.ExecutionID != v.Execution.ID || witness.ExecutionStatus != "unknown" || witness.TaskID != f.task.ID || witness.ArgumentsDigest != v.Execution.ArgumentsDigest {
			t.Fatal("public proposal witness does not resolve to the exact unknown execution", witness, witnessErr)
		}
	}
	publicState, stateErr := f.s.Automation(context.Background(), f.p, f.task.ID)
	if stateErr != nil || publicState.Grant == nil || publicState.Grant.StopReason != "unknown_effect" || len(publicState.Signals) != 1 || publicState.Signals[0].ID != unknownSignal.ID {
		t.Fatal("automation state hides or replaces the public unknown signal", publicState, stateErr)
	}
	var refreshErr error
	v, refreshErr = f.s.GetToolExecution(context.Background(), f.p, f.task.ID, v.Execution.ID)
	if refreshErr != nil {
		t.Fatal(refreshErr)
	}
	raw, e := f.s.ReconcileToolExecution(context.Background(), f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), domain.ReconcileToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, IntentHash: v.Execution.IntentHash, Outcome: "succeeded", EffectReference: "independent-operator-read-observation", Result: "Operator saw the immutable source", EvidenceDigest: Hash([]byte("independent operator read evidence")), Reason: "Explicit operator assertion, not registered response"})
	v = independentToolView(t, raw, e)
	if v.Receipt == nil || v.Receipt.Method != "operator_assertion" {
		t.Fatal("operator observation method lost")
	}
	f.reconcile(t)
	// Renewing the explicit delegation must not turn an operator assertion into
	// a registered-tool receipt or silently authorize the next model request.
	f.grant(t, nil)
	f.reconcile(t)
	if f.modelCalls.Load() != 1 || f.readCalls.Load() != 1 {
		t.Fatal("operator assertion implicitly granted model continuation")
	}
	if task := f.current(t); task.Status == "queued" || task.Status == "running" {
		t.Fatal("operator assertion restarted automatic work")
	}
	historical, next, signalErr := f.s.ProgressSignals(context.Background(), f.p, f.task.ID, "", 100)
	if signalErr != nil || next != "" || len(historical) != 1 || historical[0].ID != unknownSignal.ID || historical[0].SnapshotDigest != unknownSignal.SnapshotDigest || len(historical[0].EvidenceIDs) != 1 || historical[0].EvidenceIDs[0] != unknownSignal.EvidenceIDs[0] {
		t.Fatal("operator assertion or renewed grant erased/duplicated historical unknown signal", historical, signalErr)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
}

func TestIndependentAutomationRevokeQueuedReadStopsBeforeSend(t *testing.T) {
	f := independentAutomationSetup(t, nil)
	f.executeModel(t)
	state := f.grant(t, nil)
	f.reconcile(t)
	executions := f.executions(t)
	if len(executions) != 1 || executions[0].Status != "queued" {
		t.Fatal("queued control missing")
	}
	_, e := f.s.RevokeAutomation(context.Background(), f.p, f.task.ID, domain.NewID("key"), domain.RevokeTaskAutomation{GrantID: state.Grant.ID, ExpectedVersion: state.Grant.Version, Reason: "Explicit independent revocation before physical read"})
	if e != nil {
		t.Fatal(e)
	}
	l, _, e := f.s.ClaimToolExecution(context.Background(), "independent-revoked-worker", 30*time.Second)
	if e == nil && l != nil {
		if _, e = f.s.PrepareToolDispatch(context.Background(), l); e == nil {
			t.Fatal("revoked durable grant obtained send permit")
		}
	}
	f.reconcile(t)
	if f.readCalls.Load() != 0 || f.modelCalls.Load() != 1 {
		t.Fatal("revocation allowed a new external request")
	}
	state, e = f.s.Automation(context.Background(), f.p, f.task.ID)
	if e != nil || state.Grant == nil || state.Grant.Status != "revoked" || state.Usage.ToolExecutions != 1 || state.Usage.AllocatedMicroUSD != 120 {
		t.Fatal("revocation refunded/reset lifetime allocation", state, e)
	}
}

func TestIndependentAutomationLoopStopsSemanticRepeatBeforeThirdRead(t *testing.T) {
	f := independentAutomationSetup(t, func(f *independentAutomationFixture) { f.alwaysProposes = true })
	f.executeModel(t)
	f.grant(t, nil)
	for i := 0; i < 2; i++ {
		f.reconcile(t)
		f.executeRead(t)
		f.reconcile(t)
		f.executeModel(t)
	}
	f.reconcile(t)
	if f.readCalls.Load() != 2 || f.modelCalls.Load() != 3 {
		t.Fatal("semantic repeat crossed automatic effect boundary", f.readCalls.Load(), f.modelCalls.Load())
	}
	for _, e := range f.executions(t) {
		if e.Status == "queued" || e.Status == "leased" || e.Status == "sent" {
			t.Fatal("third repeated read remained dispatchable")
		}
	}
	state, e := f.s.Automation(context.Background(), f.p, f.task.ID)
	if e != nil || state.Grant == nil || state.Grant.Status != "stopped" || len(state.Signals) == 0 {
		t.Fatal("loop stop lacks durable attributed signal", state, e)
	}
	count := len(state.Signals)
	f.reconcile(t)
	state, e = f.s.Automation(context.Background(), f.p, f.task.ID)
	if e != nil || len(state.Signals) != count {
		t.Fatal("stopped grant emitted duplicate signals while idle", e)
	}
}

func TestIndependentAutomationCurrentAuthorityAndPinsRecheckedAfterQueue(t *testing.T) {
	for _, change := range []string{"role", "membership", "principal", "organization", "tool_retired", "model_retired"} {
		t.Run(change, func(t *testing.T) {
			f := independentAutomationSetup(t, nil)
			f.executeModel(t)
			f.grant(t, nil)
			f.reconcile(t)
			var err error
			switch change {
			case "role":
				_, err = f.s.Pool.Exec(context.Background(), "UPDATE memberships SET role='member' WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, f.p.ID)
			case "membership":
				_, err = f.s.Pool.Exec(context.Background(), "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, f.p.ID)
			case "principal":
				_, err = f.s.Pool.Exec(context.Background(), "UPDATE principals SET active=false WHERE id=$1", f.p.ID)
			case "organization":
				_, err = f.s.Pool.Exec(context.Background(), "UPDATE organizations SET active=false WHERE id=$1", f.p.OrganizationID)
			case "tool_retired":
				_, err = f.s.Pool.Exec(context.Background(), "UPDATE tool_connections SET active=false WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.tool.Tool.ID)
			case "model_retired":
				_, err = f.s.Pool.Exec(context.Background(), "UPDATE model_connections SET active=false WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.model.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			l, _, err := f.s.ClaimToolExecution(context.Background(), "independent-current-authority-worker", 30*time.Second)
			if err == nil && l != nil {
				if _, err = f.s.PrepareToolDispatch(context.Background(), l); err == nil {
					t.Fatal("stale grant acquired send permit after " + change)
				}
			}
			var issued bool
			if err = f.s.Pool.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM tool_executions WHERE organization_id=$1 AND task_id=$2 AND dispatch_issued_at IS NOT NULL)", f.p.OrganizationID, f.task.ID).Scan(&issued); err != nil || issued || f.readCalls.Load() != 0 {
				t.Fatal("authority loss allowed a dispatch marker or request", change, err)
			}
		})
	}
}

func TestIndependentAutomationSameBatchDuplicatesAndMixedEffectsStopBeforePreparation(t *testing.T) {
	for _, kind := range []string{"semantic_duplicate", "read_plus_write"} {
		t.Run(kind, func(t *testing.T) {
			f := independentAutomationSetup(t, func(f *independentAutomationFixture) {
				if kind == "read_plus_write" {
					f.mixedBatch = true
				} else {
					f.batch = func(_ int64, alias string) []independentNativeCall {
						return []independentNativeCall{independentNativeCallFor("same-batch-one", alias, independentAutomationArguments), independentNativeCallFor("same-batch-two", alias, ` { "number":90071992547409930e-1,"query":"catalog" } `)}
					}
				}
			})
			f.executeModel(t)
			f.grant(t, nil)
			f.reconcile(t)
			if f.readCalls.Load() != 0 || len(f.executions(t)) != 0 {
				t.Fatal("unsafe batch automatically prepared/executed a subset")
			}
			state, err := f.s.Automation(context.Background(), f.p, f.task.ID)
			if err != nil || state.Grant == nil || state.Grant.Status != "stopped" || state.Usage != (domain.AutomationUsage{}) {
				t.Fatal("unsafe batch missing durable stop or consumed grant allocation", state, err)
			}
		})
	}
}

func TestIndependentAutomationCumulativeChargeCeilingSurvivesSettlementAndGrantRenewal(t *testing.T) {
	f := independentAutomationSetup(t, nil)
	f.executeModel(t)
	state := f.grant(t, func(v *domain.CreateTaskAutomation) { v.MaxChargeMicroUSD = 300 })
	f.reconcile(t)
	v := f.executeRead(t)
	ev := independentEvidence(17)
	ev.Source = f.tool.Tool.Provider
	if _, err := f.s.AccountCost(context.Background(), f.p.OrganizationID, v.Execution.ReservationID, ev); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	// The task budget can afford another model and the settled charge is only
	//17. The explicit delegated allocation still includes the original120.
	if f.modelCalls.Load() != 1 {
		t.Fatal("settlement refunded delegated charge allowance")
	}
	var err error
	state, err = f.s.Automation(context.Background(), f.p, f.task.ID)
	if err != nil || state.Usage.AllocatedMicroUSD != 120 || state.Usage.ToolExecutions != 1 || state.Usage.ModelTurns != 0 {
		t.Fatal("settlement changed delegated cumulative allocation", state.Usage, err)
	}
	if state.Grant.Status == "active" {
		_, err = f.s.RevokeAutomation(context.Background(), f.p, f.task.ID, domain.NewID("key"), domain.RevokeTaskAutomation{GrantID: state.Grant.ID, ExpectedVersion: state.Grant.Version, Reason: "Renew explicit delegation without resetting usage"})
		if err != nil {
			t.Fatal(err)
		}
	}
	renewed := f.grant(t, func(v *domain.CreateTaskAutomation) { v.MaxChargeMicroUSD = 300 })
	if renewed.Usage.AllocatedMicroUSD != 120 || renewed.Usage.ToolExecutions != 1 {
		t.Fatal("new grant reset previous allocation")
	}
	f.reconcile(t)
	if f.modelCalls.Load() != 1 || f.readCalls.Load() != 1 {
		t.Fatal("grant renewal bypassed cumulative cap")
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 300, 17)
}

func TestIndependentAutomationExpiryAfterBusinessLockWaitHasNoAllocation(t *testing.T) {
	f := independentAutomationSetup(t, nil)
	f.executeModel(t)
	state := f.grant(t, func(v *domain.CreateTaskAutomation) { v.ExpiresInSeconds = 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	blocker, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	var pid int
	if err = blocker.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err = blocker.Exec(ctx, "SELECT id FROM organizations WHERE id=$1 FOR UPDATE", f.p.OrganizationID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.s.ReconcileTaskAutomation(ctx); done <- err }()
	for {
		var n int
		if err = f.s.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)) AND query LIKE 'SELECT id FROM organizations%'`, pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		select {
		case err = <-done:
			t.Fatal("coordinator ended without the forced business lock wait", err)
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	for {
		var expired bool
		if err = f.s.Pool.QueryRow(ctx, "SELECT expires_at<=clock_timestamp() FROM task_automation_grants WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, state.Grant.ID).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// A bounded domain rejection or a durable stopped/expired transition is
	// acceptable; a PostgreSQL/internal error is not an authorization result.
	if err = <-done; err != nil {
		independentToolDeny(t, err)
	}
	if len(f.executions(t)) != 0 || f.readCalls.Load() != 0 {
		t.Fatal("expired grant prepared or executed after lock wait")
	}
	state, err = f.s.Automation(context.Background(), f.p, f.task.ID)
	if err != nil || state.Usage != (domain.AutomationUsage{}) {
		t.Fatal("expired grant allocated new work", state, err)
	}
}

func TestIndependentAutomationLoopHistorySurvivesNewGrantAndExplicitRetry(t *testing.T) {
	f := independentAutomationSetup(t, func(f *independentAutomationFixture) { f.alwaysProposes = true })
	f.executeModel(t)
	f.grant(t, nil)
	for i := 0; i < 2; i++ {
		f.reconcile(t)
		f.executeRead(t)
		f.reconcile(t)
		f.executeModel(t)
	}
	f.reconcile(t)
	before, err := f.s.Automation(context.Background(), f.p, f.task.ID)
	if err != nil || before.Grant == nil || before.Grant.Status != "stopped" {
		t.Fatal("loop control did not stop", err)
	}
	f.grant(t, nil)
	f.reconcile(t)
	if f.readCalls.Load() != 2 || f.modelCalls.Load() != 3 {
		t.Fatal("renewed grant reset loop history")
	}
	proposals, _, err := f.s.ModelProposals(context.Background(), f.p, f.task.ID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	current := f.current(t)
	var proposal domain.ModelToolProposal
	for _, p := range proposals {
		if p.CandidateID == current.CandidateID {
			proposal = p
			break
		}
	}
	if proposal.ID == "" {
		t.Fatal("current loop proposal unavailable")
	}
	raw, err := f.s.ReviewTask(context.Background(), f.p, f.task.ID, domain.NewID("key"), domain.ReviewTaskRequest{ExpectedVersion: current.Version, CandidateID: proposal.CandidateID, ArtifactID: proposal.ArtifactID, ArtifactVersion: proposal.ArtifactVersion, ContentDigest: proposal.ContentDigest, Decision: "request_changes", Reason: "Explicit manual new Run must retain old repeated lookup history"})
	if err != nil || json.Unmarshal(raw, &current) != nil {
		t.Fatal("cannot request explicit changes", err)
	}
	raw, err = f.s.RetryTask(context.Background(), f.p, f.task.ID, domain.NewID("key"), domain.RetryTaskRequest{ExpectedVersion: current.Version, Reason: "Explicit independent retry with preserved task counters", Feedback: "Keep the original lookup purpose and exact pinned tool", AcknowledgeNewCost: true})
	if err != nil || json.Unmarshal(raw, &current) != nil || current.CurrentRunID == f.task.CurrentRunID {
		t.Fatal("explicit retry not created", err)
	}
	f.grant(t, nil)
	f.executeModel(t)
	f.reconcile(t)
	after, err := f.s.Automation(context.Background(), f.p, f.task.ID)
	if err != nil || after.Grant == nil || after.Grant.Status != "stopped" || after.Usage != before.Usage || f.readCalls.Load() != 2 || f.modelCalls.Load() != 4 {
		t.Fatal("new Run reset loop or cumulative allocation", after.Usage, before.Usage, err)
	}
}

func TestIndependentAutomationSQLRequiresExactTypedPinsAndMonotonicUsage(t *testing.T) {
	f := independentAutomationSetup(t, nil)
	f.executeModel(t)
	state := f.grant(t, nil)
	ctx := context.Background()
	probe := func(t *testing.T, change func(map[string]any)) error {
		t.Helper()
		tx, err := f.s.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, "UPDATE task_automation_grants SET status='revoked',version=version+1,stop_reason='independent rolled-back SQL probe' WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, state.Grant.ID); err != nil {
			t.Fatal(err)
		}
		g := *state.Grant
		g.ID = domain.NewID("aut")
		g.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
		g.ExpiresAt = g.CreatedAt.Add(time.Minute)
		raw, _ := json.Marshal(g)
		var body map[string]any
		if err = json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if change != nil {
			change(body)
		}
		raw, _ = json.Marshal(body)
		_, err = tx.Exec(ctx, "INSERT INTO task_automation_grants(organization_id,id,task_id,principal_id,owner_id,grant_body,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", g.OrganizationID, g.ID, g.TaskID, g.PrincipalID, g.OwnerID, raw, g.CreatedAt, g.ExpiresAt)
		return err
	}
	t.Run("valid_control", func(t *testing.T) {
		if err := probe(t, nil); err != nil {
			t.Fatal("SQL positive control failed before negative probes", err)
		}
	})
	changes := map[string]func(map[string]any){
		"missing_mode":         func(v map[string]any) { delete(v, "mode") },
		"null_mode":            func(v map[string]any) { v["mode"] = nil },
		"null_identity":        func(v map[string]any) { v["id"] = nil },
		"missing_model_id":     func(v map[string]any) { delete(v, "model_connection_id") },
		"wrong_model_id":       func(v map[string]any) { v["model_connection_id"] = "foreign-model" },
		"null_agent_version":   func(v map[string]any) { v["agent_version"] = nil },
		"string_agent_version": func(v map[string]any) { v["agent_version"] = "1" },
		"wrong_agent_version":  func(v map[string]any) { v["agent_version"] = 2 },
		"numeric_charge":       func(v map[string]any) { v["max_charge_microusd"] = 5000 },
		"null_tool_pins":       func(v map[string]any) { v["tool_pins"] = nil },
		"foreign_tool_pin": func(v map[string]any) {
			v["tool_pins"] = []any{map[string]any{"tool_id": "unrelated-tool", "tool_version": 1, "tool_profile_digest": strings.Repeat("b", 64)}}
		},
		"null_tool_digest": func(v map[string]any) { v["tool_pins"].([]any)[0].(map[string]any)["tool_profile_digest"] = nil },
		"wrong_tool_digest": func(v map[string]any) {
			v["tool_pins"].([]any)[0].(map[string]any)["tool_profile_digest"] = strings.Repeat("b", 64)
		},
		"duplicate_tool_pin": func(v map[string]any) { v["tool_pins"] = append(v["tool_pins"].([]any), v["tool_pins"].([]any)[0]) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			if err := probe(t, change); err == nil {
				t.Fatal("SQL admitted missing/null/substituted grant snapshot")
			}
		})
	}
	f.reconcile(t)
	for _, statement := range []string{"UPDATE task_automation_usage SET tool_executions=0,allocated_microusd=0 WHERE organization_id=$1 AND task_id=$2", "UPDATE task_automation_usage SET tool_executions=tool_executions+2 WHERE organization_id=$1 AND task_id=$2", "DELETE FROM task_automation_usage WHERE organization_id=$1 AND task_id=$2"} {
		tx, err := f.s.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		tag, err := tx.Exec(ctx, statement, f.p.OrganizationID, f.task.ID)
		_ = tx.Rollback(ctx)
		if err == nil {
			// Diagnose missing allocation separately from a database guard admitting
			// an actual row mutation, without retrying or weakening this assertion.
			diagnostic, readErr := f.s.Automation(ctx, f.p, f.task.ID)
			status, stopReason := "missing", ""
			if diagnostic.Grant != nil {
				status, stopReason = diagnostic.Grant.Status, diagnostic.Grant.StopReason
			}
			t.Fatalf("SQL reset/deleted/skipped cumulative automation usage: statement=%q rows=%d grant_status=%s stop_reason=%q usage=%+v state_error=%v", statement, tag.RowsAffected(), status, stopReason, diagnostic.Usage, readErr)
		}
	}
	var grants int
	if err := f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM task_automation_grants WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&grants); err != nil || grants != 1 {
		t.Fatal("rolled-back negative grant probe escaped", err)
	}
}

func TestIndependentAutomationParallelChildrenShareRootBudgetAndLifetimeCounters(t *testing.T) {
	f := independentAutomationSetup(t, func(f *independentAutomationFixture) { f.alwaysProposes = true })
	ctx := context.Background()
	if _, err := f.s.CancelTask(ctx, f.p, f.task.ID, domain.NewID("key")); err != nil {
		t.Fatal(err)
	}
	team := teamAgent(t, f.s, f.p, "parallel", "all", 2, teamMember("left", f.agent), teamMember("right", f.agent))
	raw, err := f.s.CreateTask(ctx, f.p, domain.NewID("key"), domain.CreateTask{AgentID: team.ID, Goal: "Competing read agents must share one root budget", BudgetMicroUSD: 800})
	var root domain.Task
	if err != nil || json.Unmarshal(raw, &root) != nil {
		t.Fatal("create budgeted team root", err)
	}
	for i := 0; i < 4; i++ {
		teamTick(t, f.s)
	}
	childIDs := []string{}
	for i := 0; i < 2; i++ {
		l, c, err := f.s.ClaimModel(ctx, "independent-root-model-worker", 30*time.Second)
		if err != nil || l == nil || l.Task.RootID != root.ID {
			t.Fatal("team model child not admitted", err)
		}
		childIDs = append(childIDs, l.TaskID)
		runtime, err := modelruntime.New(f.s, RuntimeModelConfig(c, "independent-auto-model-key"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = runtime.Execute(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	if len(childIDs) != 2 || childIDs[0] == childIDs[1] || f.modelCalls.Load() != 2 {
		t.Fatal("two independent native children were not executed")
	}
	for _, id := range childIDs {
		f.task.ID = id
		f.grant(t, nil)
	}
	const concurrent = 12
	errs := make(chan error, concurrent)
	var wg sync.WaitGroup
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := f.s.ReconcileTaskAutomation(ctx); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	f.reconcile(t)
	var queued, allocations, rootTools, rootTurns int
	var allocated int64
	err = f.s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tool_executions WHERE organization_id=$1 AND status='queued'),
 (SELECT COALESCE(sum(tool_executions),0) FROM task_automation_usage WHERE organization_id=$1),
 (SELECT COALESCE(sum(allocated_microusd),0) FROM task_automation_usage WHERE organization_id=$1),
 (SELECT tree_executions FROM tool_execution_counts WHERE organization_id=$1 AND task_id=$2),
 (SELECT tree_turns FROM model_turn_counts WHERE organization_id=$1 AND task_id=$2)`, f.p.OrganizationID, root.ID).Scan(&queued, &allocations, &allocated, &rootTools, &rootTurns)
	if err != nil || queued != 1 || allocations != 1 || allocated != 120 || rootTools != 1 || rootTurns != 2 || f.readCalls.Load() != 0 {
		t.Fatal("parallel grants exceeded/reset root allowance or counters", queued, allocations, allocated, rootTools, rootTurns, err)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{root.ID}, 720, 0)
}

func TestIndependentAutomationAlternatingRequestsStopBeforeFourthRead(t *testing.T) {
	f := independentAutomationSetup(t, func(f *independentAutomationFixture) {
		f.alwaysProposes = true
		f.batch = func(n int64, alias string) []independentNativeCall {
			q := "a"
			if n%2 == 0 {
				q = "b"
			}
			args := fmt.Sprintf(`{"query":%q,"number":9007199254740993}`, q)
			return []independentNativeCall{independentNativeCallFor(fmt.Sprintf("alternating-%d", n), alias, args)}
		}
	})
	f.executeModel(t)
	f.grant(t, nil)
	for i := 0; i < 3; i++ {
		f.reconcile(t)
		f.executeRead(t)
		f.reconcile(t)
		f.executeModel(t)
	}
	f.reconcile(t)
	state, err := f.s.Automation(context.Background(), f.p, f.task.ID)
	if err != nil || state.Grant == nil || state.Grant.Status != "stopped" || f.readCalls.Load() != 3 || f.modelCalls.Load() != 4 {
		t.Fatal("A-B-A-B cycle crossed fourth automatic effect", state, err)
	}
	found := false
	for _, signal := range state.Signals {
		if signal.Code == "alternating_intents" {
			found = true
			if len(signal.EvidenceIDs) < 3 {
				t.Fatal("alternation signal lost causal witnesses")
			}
		}
	}
	if !found {
		t.Fatal("durable alternating cycle signal missing")
	}
}

func TestIndependentAutomationPrepareModeKeepsWriteApprovalAndDispatchManual(t *testing.T) {
	// Reuse the existing explicitly declared synthetic write peer; unlike the
	// read-only tests above it is never relabeled as read or auto-approved.
	f := independentNativeSetup(t, nil)
	ctx := context.Background()
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	p := f.proposal(t)
	task := f.current(t)
	in := domain.CreateTaskAutomation{ExpectedTaskVersion: task.Version, Mode: "prepare_and_continue", ToolPins: []domain.AutomationToolPin{{ToolID: f.tool.Tool.ID, ToolVersion: 1, ToolProfileDigest: domain.ToolConnectionDigest(f.tool)}}, MaxModelTurns: 2, MaxToolExecutions: 2, MaxChargeMicroUSD: 1000, ExpiresInSeconds: 60, AcknowledgeCost: true, Reason: "Prepare exact proposals, require explicit write approval and dispatch, then bounded continuation"}
	raw, err := f.s.CreateAutomation(ctx, f.p, f.task.ID, domain.NewID("key"), in)
	var state domain.TaskAutomationState
	if err != nil || json.Unmarshal(raw, &state) != nil || state.Grant == nil {
		t.Fatal("prepare-mode grant missing", err)
	}
	reconcile := func() {
		t.Helper()
		for i := 0; i < 16; i++ {
			did, err := f.s.ReconcileTaskAutomation(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !did {
				return
			}
		}
		t.Fatal("prepare mode spins while awaiting explicit decision")
	}
	reconcile()
	proposals, _, err := f.s.ModelProposals(ctx, f.p, f.task.ID, "", 100)
	if err != nil || len(proposals) != 1 || proposals[0].ExecutionID == "" {
		t.Fatal("write was not prepared exactly once", err)
	}
	v, err := f.s.GetToolExecution(ctx, f.p, f.task.ID, proposals[0].ExecutionID)
	if err != nil || v.Execution.Status != "awaiting_approval" || v.Approval.Status != "pending" || v.Approval.DecisionMethod != "" || f.toolCalls.Load() != 0 {
		t.Fatal("prepare-only grant implicitly approved/dispatched a write", err)
	}
	v = f.dispatchAndObserve(t, v)
	if v.Approval.DecisionMethod != "authorized_principal_decision" || v.Receipt == nil || v.Receipt.Method != "registered_tool_response" {
		t.Fatal("explicit write decision or registered receipt attribution lost")
	}
	reconcile()
	task = f.current(t)
	if task.Status != "queued" || task.CurrentRunID != p.RunID || f.toolCalls.Load() != 1 || f.modelCalls.Load() != 1 {
		t.Fatal("registered manually authorized write did not admit exactly one next model")
	}
	state, err = f.s.Automation(ctx, f.p, f.task.ID)
	if err != nil || state.Usage.ToolExecutions != 1 || state.Usage.ModelTurns != 1 || state.Usage.AllocatedMicroUSD != 420 {
		t.Fatal("manual write execution reset/duplicated granted allocations", state.Usage, err)
	}
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if final := f.current(t); final.Status != "waiting_input" || final.AcceptedDecisionID != "" {
		t.Fatal("automatic continuation accepted unverified final candidate")
	}
}

func TestIndependentAutomationRevokedAwaitingJobReapedWithoutRefundingUsage(t *testing.T) {
	f := independentAutomationSetup(t, func(f *independentAutomationFixture) { f.toolCharge = 17 })
	ctx := context.Background()
	f.executeModel(t)
	state := f.grant(t, func(in *domain.CreateTaskAutomation) { in.Mode = "prepare_and_continue"; in.ExpiresInSeconds = 86400 })
	f.reconcile(t)
	jobs := f.executions(t)
	if len(jobs) != 1 || jobs[0].Status != "awaiting_approval" || jobs[0].AutomationGrantID != state.Grant.ID {
		t.Fatal("awaiting grant-bound control missing", jobs)
	}
	if _, err := f.s.RevokeAutomation(ctx, f.p, f.task.ID, domain.NewID("key"), domain.RevokeTaskAutomation{GrantID: state.Grant.ID, ExpectedVersion: state.Grant.Version, Reason: "Revoke future automation while unapproved action remains unsent"}); err != nil {
		t.Fatal(err)
	}
	did, err := f.s.ReconcileToolUnsent(ctx)
	if err != nil || !did {
		t.Fatal("revoked unapproved grant-bound action was not reaped promptly", did, err)
	}
	view, err := f.s.GetToolExecution(ctx, f.p, f.task.ID, jobs[0].ID)
	if err != nil || (view.Execution.Status != "cancelled" && view.Execution.Status != "expired") || view.Receipt != nil || view.Task.Status != "waiting_input" || view.Task.WaitingReason != "model_tools_pending" || view.Task.CandidateID != jobs[0].CandidateID || view.Task.CurrentRunID != jobs[0].RunID {
		t.Fatal("proved-unsent cleanup lost manual continuation source or claimed an effect", view, err)
	}
	state, err = f.s.Automation(ctx, f.p, f.task.ID)
	if err != nil || state.Grant == nil || state.Grant.Status != "revoked" || state.Usage.ToolExecutions != 1 || state.Usage.ModelTurns != 0 || state.Usage.AllocatedMicroUSD != 17 {
		t.Fatal("cleanup refunded/reset cumulative grant allocation", state, err)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 300, 0)
	var reservationStatus, actionStatus string
	var marked bool
	if err = f.s.Pool.QueryRow(ctx, `SELECT r.status,a.status,a.dispatch_issued_at IS NOT NULL FROM reservations r JOIN actions a ON a.organization_id=r.organization_id AND a.id=r.action_id WHERE r.organization_id=$1 AND r.id=$2`, f.p.OrganizationID, jobs[0].ReservationID).Scan(&reservationStatus, &actionStatus, &marked); err != nil || reservationStatus != "released" || actionStatus != "rejected" || marked {
		t.Fatal("cleanup failed to prove non-dispatch before release", reservationStatus, actionStatus, marked, err)
	}
	p, err := f.s.ModelProposal(ctx, f.p, f.task.ID, view.Execution.ModelProposalID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := f.s.ContinueModel(ctx, f.p, f.task.ID, domain.NewID("key"), independentNativeContinue(p, view))
	var next domain.Task
	if err != nil || json.Unmarshal(raw, &next) != nil || next.Status != "queued" || next.CurrentRunID != view.Task.CurrentRunID || f.modelCalls.Load() != 1 || f.readCalls.Load() != 0 {
		t.Fatal("revocation cleanup left task unusable for explicit manual continuation", err)
	}
	state, err = f.s.Automation(ctx, f.p, f.task.ID)
	if err != nil || state.Usage.AllocatedMicroUSD != 17 || state.Usage.ToolExecutions != 1 || state.Usage.ModelTurns != 0 {
		t.Fatal("manual continuation silently reused/reset revoked delegation", state.Usage, err)
	}
}

type independentAutomationLiabilitySnapshot struct {
	ExecutionStatus     string
	ExecutionVersion    int
	ReservationStatus   string
	ReservationRevision int
	Reserved            int64
	Spent               int64
	TaskVersion         int
	ActionStatus        string
	Marker              bool
	LedgerEntries       int
}

func independentAutomationLiability(t *testing.T, f *independentAutomationFixture, id string) independentAutomationLiabilitySnapshot {
	t.Helper()
	var v independentAutomationLiabilitySnapshot
	err := f.s.Pool.QueryRow(context.Background(), `SELECT e.status,e.version,r.status,r.revision,t.reserved_microusd,t.spent_microusd,t.version,a.status,a.dispatch_issued_at IS NOT NULL,(SELECT count(*) FROM ledger_entries l WHERE l.organization_id=e.organization_id AND l.reservation_id=e.reservation_id) FROM tool_executions e JOIN reservations r ON r.organization_id=e.organization_id AND r.id=e.reservation_id JOIN actions a ON a.organization_id=e.organization_id AND a.id=e.action_id JOIN tasks t ON t.organization_id=e.organization_id AND t.id=e.task_id WHERE e.organization_id=$1 AND e.id=$2`, f.p.OrganizationID, id).Scan(&v.ExecutionStatus, &v.ExecutionVersion, &v.ReservationStatus, &v.ReservationRevision, &v.Reserved, &v.Spent, &v.TaskVersion, &v.ActionStatus, &v.Marker, &v.LedgerEntries)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestIndependentAutomationInactiveGrantReaperPreservesMarkedAndUnknownLiabilities(t *testing.T) {
	for _, outcome := range []string{"sent", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			f := independentAutomationSetup(t, func(f *independentAutomationFixture) { f.toolCharge = 17 })
			ctx := context.Background()
			f.executeModel(t)
			state := f.grant(t, nil)
			f.reconcile(t)
			lease, _, err := f.s.ClaimToolExecution(ctx, "independent-revocation-marker-worker", 30*time.Second)
			if err != nil || lease == nil {
				t.Fatal("marker control not claimable", err)
			}
			if _, err = f.s.PrepareToolDispatch(ctx, lease); err != nil {
				t.Fatal("cannot persist committed send marker", err)
			}
			if outcome == "unknown" {
				if _, err = f.s.RecordToolOutcome(ctx, domain.ToolOutcomeEvidence{OrganizationID: f.p.OrganizationID, ExecutionID: lease.Execution.ID, IntentHash: lease.Execution.IntentHash, FencingToken: lease.FencingToken, Outcome: "unknown"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = f.s.RevokeAutomation(ctx, f.p, f.task.ID, domain.NewID("key"), domain.RevokeTaskAutomation{GrantID: state.Grant.ID, ExpectedVersion: state.Grant.Version, Reason: "Revoke future work without erasing committed or uncertain dispatch"}); err != nil {
				t.Fatal(err)
			}
			before := independentAutomationLiability(t, f, lease.Execution.ID)
			for i := 0; i < 2; i++ {
				did, err := f.s.ReconcileToolUnsent(ctx)
				if err != nil || did {
					t.Fatal("inactive grant reaper treated marked/unknown job as proven unsent", did, err)
				}
			}
			after := independentAutomationLiability(t, f, lease.Execution.ID)
			if before != after || !after.Marker || after.ExecutionStatus != outcome || after.Reserved != 317 || after.Spent != 0 {
				t.Fatal("grant revocation changed uncertain liability or dispatched job", before, after)
			}
			state, err = f.s.Automation(ctx, f.p, f.task.ID)
			if err != nil || state.Usage.AllocatedMicroUSD != 17 || state.Usage.ToolExecutions != 1 || state.Grant.Status != "revoked" || f.modelCalls.Load() != 1 || f.readCalls.Load() != 0 {
				t.Fatal("marked revocation reset grant allocation or invented a network call", state, err)
			}
		})
	}
}

func TestIndependentAutomationInactiveGrantReaperPreservesOtherGrantAndUnboundManualJob(t *testing.T) {
	f := independentAutomationSetup(t, func(f *independentAutomationFixture) { f.toolCharge = 17; f.alwaysProposes = true })
	ctx := context.Background()
	f.executeModel(t)
	firstTask := f.current(t)
	firstGrant := f.grant(t, func(in *domain.CreateTaskAutomation) { in.Mode = "prepare_and_continue" })
	f.reconcile(t)
	firstJobs := f.executions(t)
	if len(firstJobs) != 1 {
		t.Fatal("first awaiting job missing")
	}
	makeNextTask := func() {
		t.Helper()
		raw, err := f.s.CreateTask(ctx, f.p, domain.NewID("key"), domain.CreateTask{AgentID: f.agent.ID, Goal: "Independent grant binding isolation", BudgetMicroUSD: 100000})
		if err != nil || json.Unmarshal(raw, &f.task) != nil {
			t.Fatal("next task creation failed", err)
		}
		f.executeModel(t)
	}
	makeNextTask()
	secondTask := f.current(t)
	secondGrant := f.grant(t, func(in *domain.CreateTaskAutomation) { in.Mode = "prepare_and_continue" })
	f.reconcile(t)
	secondJobs := f.executions(t)
	if len(secondJobs) != 1 || secondJobs[0].AutomationGrantID != secondGrant.Grant.ID {
		t.Fatal("active other-grant control missing")
	}
	makeNextTask()
	manualTask := f.current(t)
	proposals, _, err := f.s.ModelProposals(ctx, f.p, manualTask.ID, "", 100)
	if err != nil || len(proposals) != 1 {
		t.Fatal("manual proposal control missing", err)
	}
	p, err := f.s.ModelProposal(ctx, f.p, manualTask.ID, proposals[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := f.s.PrepareToolExecution(ctx, f.p, manualTask.ID, domain.NewID("key"), domain.PrepareToolExecution{OccurrenceID: "model:" + p.ID, ModelProposalID: p.ID, ExpectedTaskVersion: manualTask.Version, CandidateID: p.CandidateID, ArtifactID: p.ArtifactID, ArtifactVersion: p.ArtifactVersion, ContentDigest: p.ContentDigest, ToolID: p.ToolID, ToolVersion: p.ToolVersion, ArgumentsJSON: p.ArgumentsJSON, ExpiresInSeconds: 3600})
	manual := independentToolView(t, raw, err)
	if manual.Execution.AutomationGrantID != "" {
		t.Fatal("manual preparation borrowed a grant")
	}
	// The manual job shares a task with a subsequently revoked grant: matching
	// by task alone would incorrectly cancel it. Only the exact immutable job
	// grant reference can authorize automatic cleanup.
	manualTaskGrant := f.grant(t, func(in *domain.CreateTaskAutomation) { in.Mode = "prepare_and_continue" })
	for _, v := range []struct {
		task  string
		grant *domain.TaskAutomationGrant
	}{{firstTask.ID, firstGrant.Grant}, {manualTask.ID, manualTaskGrant.Grant}} {
		if _, err = f.s.RevokeAutomation(ctx, f.p, v.task, domain.NewID("key"), domain.RevokeTaskAutomation{GrantID: v.grant.ID, ExpectedVersion: v.grant.Version, Reason: "Explicit independent grant binding isolation"}); err != nil {
			t.Fatal(err)
		}
	}
	did, err := f.s.ReconcileToolUnsent(ctx)
	if err != nil || !did {
		t.Fatal("eligible revoked bound control was not cleaned", did, err)
	}
	did, err = f.s.ReconcileToolUnsent(ctx)
	if err != nil || did {
		t.Fatal("reaper selected an unrelated active grant or an unbound manual job", did, err)
	}
	for _, v := range []struct{ task, id string }{{secondTask.ID, secondJobs[0].ID}, {manualTask.ID, manual.Execution.ID}} {
		got, err := f.s.GetToolExecution(ctx, f.p, v.task, v.id)
		if err != nil || got.Execution.Status != "awaiting_approval" || got.Approval.Status != "pending" || got.Task.ReservedMicroUSD != 317 || got.Receipt != nil {
			t.Fatal("reaper crossed exact grant binding or cancelled a manual action", got, err)
		}
	}
	state, err := f.s.Automation(ctx, f.p, secondTask.ID)
	if err != nil || state.Grant == nil || state.Grant.ID != secondGrant.Grant.ID || state.Grant.Status != "active" || state.Usage.AllocatedMicroUSD != 17 {
		t.Fatal("unrelated active grant was modified", state, err)
	}
	first, firstErr := f.s.GetTask(ctx, f.p, firstTask.ID)
	if firstErr != nil || first.ReservedMicroUSD != 300 || first.SpentMicroUSD != 0 {
		t.Fatal("cleaned task did not retain its original model liability", first.ReservedMicroUSD, first.SpentMicroUSD, firstErr)
	}
	var organizationHeld, organizationSpent int64
	if err = f.s.Pool.QueryRow(ctx, "SELECT reserved_microusd,spent_microusd FROM organizations WHERE id=$1", f.p.OrganizationID).Scan(&organizationHeld, &organizationSpent); err != nil || organizationHeld != 934 || organizationSpent != 0 {
		t.Fatal("cleanup released an unrelated task's liability", organizationHeld, organizationSpent, err)
	}
	if f.modelCalls.Load() != 3 || f.readCalls.Load() != 0 {
		t.Fatal("binding cleanup invented provider delivery")
	}
}
