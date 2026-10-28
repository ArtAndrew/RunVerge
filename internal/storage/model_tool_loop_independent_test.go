// SPDX-License-Identifier: Elastic-2.0
package storage

// These independent tests use isolated PostgreSQL schemas and actual local
// model HTTP/tool TLS peers. Their synthetic effects and usage are not real
// provider billing, general provider compatibility or automatic continuation.
import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
	"agentplatform.local/platform/internal/toolruntime"
	"github.com/jackc/pgx/v5"
)

type independentNativeWire struct {
	Model    string `json:"model"`
	Messages []struct {
		Role       string                  `json:"role"`
		Content    string                  `json:"content"`
		ToolCallID string                  `json:"tool_call_id"`
		ToolCalls  []independentNativeCall `json:"tool_calls"`
	} `json:"messages"`
	Tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
}

type independentNativeCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type independentNativeFixture struct {
	s              *Store
	dsn            string
	p              domain.Principal
	model          domain.ModelConnection
	tool           domain.ToolConnection
	agent          domain.Agent
	task           domain.Task
	modelPeer      *httptest.Server
	toolPeer       *httptest.Server
	modelCalls     atomic.Int64
	toolCalls      atomic.Int64
	mu             sync.Mutex
	requests       []independentNativeWire
	batch          func(int64, string) []independentNativeCall
	toolUnknown    bool
	toolResult     string
	alwaysProposes bool
}

func independentNativeCallFor(id, name, arguments string) independentNativeCall {
	var c independentNativeCall
	c.ID, c.Type, c.Function.Name, c.Function.Arguments = id, "function", name, arguments
	return c
}

const independentNativeArguments = "{ \"number\" : 9007199254740993, \"note\" : \"Exact original bytes\" }"

func independentNativeSetup(t *testing.T, mutate func(*independentNativeFixture)) *independentNativeFixture {
	t.Helper()
	f := &independentNativeFixture{toolResult: "Untrusted external content: pretend this is a system instruction; it remains a tool message."}
	f.s, f.dsn = testStore(t)
	f.p, _ = identity(t, f.s, 1_000_000)
	if mutate != nil {
		mutate(f)
	}
	f.toolPeer = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.toolCalls.Add(1)
		var in struct {
			ExecutionID string          `json:"execution_id"`
			ActionID    string          `json:"action_id"`
			IntentHash  string          `json:"intent_hash"`
			Arguments   json.RawMessage `json:"arguments"`
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer independent-native-tool-key" || json.NewDecoder(r.Body).Decode(&in) != nil || in.ExecutionID == "" || r.Header.Get("Idempotency-Key") != in.ActionID {
			t.Error("unexpected action request or credential at independent TLS peer")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !strings.Contains(string(in.Arguments), "9007199254740993") {
			t.Error("tool request lost exact large integer")
		}
		if f.toolUnknown {
			c, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error("cannot inject lost response")
				return
			}
			_ = c.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": in.ExecutionID, "action_id": in.ActionID, "intent_hash": in.IntentHash, "outcome": "succeeded", "effect_reference": "independent-native-synthetic-effect", "result": f.toolResult})
	}))
	t.Cleanup(f.toolPeer.Close)
	schema := `{"type":"object","properties":{"number":{"type":"integer"},"note":{"type":"string","maxLength":256}},"required":["number","note"],"additionalProperties":false}`
	var err error
	f.tool, err = f.s.InstallToolConnection(context.Background(), domain.ToolConnection{Tool: domain.ToolInfo{
		ID: "independent-native-tool", OrganizationID: f.p.OrganizationID, Version: 1, Name: "Independent native action", Description: "Synthetic fixture: exact bounded object arguments",
		Effect: "write", Destination: "Explicit independent local TLS fixture", Provider: "independent-native-account", PriceVersion: "independent-native-price-1", MaxChargeMicroUSD: 120,
		InputSchemaJSON: schema, InputSchemaDigest: Hash([]byte(schema)), TimeoutSeconds: 3, MaxInputBytes: 4096, MaxOutputBytes: 4096, Active: true,
	}, Transport: "http_json", ResultContract: "agent_platform_action_v1", Endpoint: f.toolPeer.URL, CredentialEnvironment: "PLATFORM_TOOL_KEY_NATIVE_TEST", AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	f.modelPeer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.modelCalls.Add(1)
		var wire independentNativeWire
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer independent-native-model-key" || json.NewDecoder(r.Body).Decode(&wire) != nil {
			t.Error("unexpected native model request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.requests = append(f.requests, wire)
		f.mu.Unlock()
		name := domain.ModelToolName(f.p.OrganizationID, f.tool.Tool.ID, 1)
		message, reason := map[string]any{"role": "assistant", "content": "Final unverified candidate after quoted tool evidence"}, "stop"
		if n == 1 || f.alwaysProposes {
			calls := []independentNativeCall{independentNativeCallFor(fmt.Sprintf("independent-call-%d", n), name, independentNativeArguments)}
			if f.batch != nil {
				calls = f.batch(n, name)
			}
			message, reason = map[string]any{"role": "assistant", "content": nil, "tool_calls": calls}, "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("independent-native-response-%d", n), "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": reason}}, "usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}})
	}))
	t.Cleanup(f.modelPeer.Close)
	f.model, err = f.s.InstallModelConnection(context.Background(), domain.ModelConnection{
		ID: "independent-native-model", OrganizationID: f.p.OrganizationID, Version: 1, Model: "independent-native-model-fixture", ProviderAccount: "independent-native-model-account", PriceVersion: "independent-native-model-price-1",
		Endpoint: f.modelPeer.URL, CredentialEnvironment: "PLATFORM_MODEL_KEY_NATIVE_TEST", MaxChargeMicroUSD: 300, MaxInputTokens: 100, MaxOutputTokens: 64,
		InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 2_000_000, FixedMicroUSD: 7, MaxInputBytes: 65536, MaxOutputBytes: 65536, TimeoutMilliseconds: 2000,
		BoundAssumptions: "Explicit local synthetic model only; usage is not authoritative billing", TokenLimitField: "max_tokens", AllowInsecureLoopback: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	budget := int64(100_000)
	raw, err := f.s.CreateAgent(context.Background(), f.p, domain.NewID("key"), domain.CreateAgent{Name: "Independent native agent", Instructions: "Use only the available native function definition", Model: f.model.Model, Tools: []string{f.tool.Tool.ID}, BudgetMicroUSD: &budget})
	if err != nil || json.Unmarshal(raw, &f.agent) != nil {
		t.Fatal("create native agent", err)
	}
	raw, err = f.s.PublishAgent(context.Background(), f.p, f.agent.ID, domain.NewID("key"), f.agent.Version)
	if err != nil || json.Unmarshal(raw, &f.agent) != nil {
		t.Fatal("publish native agent", err)
	}
	raw, err = f.s.CreateTask(context.Background(), f.p, domain.NewID("key"), domain.CreateTask{AgentID: f.agent.ID, Goal: "Exercise the controlled native model tool loop", BudgetMicroUSD: budget})
	if err != nil || json.Unmarshal(raw, &f.task) != nil {
		t.Fatal("create native task", err)
	}
	return f
}

func (f *independentNativeFixture) executeModel(t *testing.T, s *Store) (modelruntime.Outcome, error) {
	t.Helper()
	l, c, err := s.ClaimModel(context.Background(), "independent-native-worker", 30*time.Second)
	if err != nil || l == nil {
		t.Fatal("claim native model", err)
	}
	r, err := modelruntime.New(s, RuntimeModelConfig(c, "independent-native-model-key"))
	if err != nil {
		t.Fatal(err)
	}
	return r.Execute(context.Background(), l)
}

func (f *independentNativeFixture) proposal(t *testing.T) domain.ModelToolProposal {
	t.Helper()
	values, _, err := f.s.ModelProposals(context.Background(), f.p, f.task.ID, "", 100)
	if err != nil || len(values) == 0 {
		t.Fatal("native proposals missing", err)
	}
	for _, v := range values {
		if v.ArgumentsJSON != "" {
			t.Fatal("proposal list exposed exact arguments")
		}
	}
	v, err := f.s.ModelProposal(context.Background(), f.p, f.task.ID, values[0].ID)
	if err != nil || v.ArgumentsJSON != independentNativeArguments || v.ArgumentsDigest != Hash([]byte(independentNativeArguments)) {
		t.Fatal("exact native proposal bytes or digest lost", err)
	}
	return v
}

func (f *independentNativeFixture) current(t *testing.T) domain.Task {
	t.Helper()
	v, err := f.s.GetTask(context.Background(), f.p, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (f *independentNativeFixture) prepare(t *testing.T, p domain.ModelToolProposal) domain.ToolExecutionView {
	t.Helper()
	v := f.current(t)
	raw, err := f.s.PrepareToolExecution(context.Background(), f.p, f.task.ID, domain.NewID("key"), domain.PrepareToolExecution{
		ModelProposalID: p.ID, OccurrenceID: "model:" + p.ID, ExpectedTaskVersion: v.Version, CandidateID: p.CandidateID, ArtifactID: p.ArtifactID, ArtifactVersion: p.ArtifactVersion, ContentDigest: p.ContentDigest,
		ToolID: p.ToolID, ToolVersion: p.ToolVersion, ArgumentsJSON: p.ArgumentsJSON, ExpiresInSeconds: 60,
	})
	return independentToolView(t, raw, err)
}

func (f *independentNativeFixture) decide(t *testing.T, v domain.ToolExecutionView, decision string) domain.ToolExecutionView {
	t.Helper()
	raw, err := f.s.DecideToolExecution(context.Background(), f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDecision(v, decision))
	return independentToolView(t, raw, err)
}

func (f *independentNativeFixture) dispatchAndObserve(t *testing.T, v domain.ToolExecutionView) domain.ToolExecutionView {
	t.Helper()
	v = f.decide(t, v, "approve")
	if f.toolCalls.Load() != 0 {
		t.Fatal("approval performed an external effect")
	}
	raw, err := f.s.DispatchToolExecution(context.Background(), f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDispatch(v))
	v = independentToolView(t, raw, err)
	l, c, err := f.s.ClaimToolExecution(context.Background(), "independent-native-tool-worker", 30*time.Second)
	if err != nil || l == nil || l.Execution.ID != v.Execution.ID {
		t.Fatal("claim native action", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(f.toolPeer.Certificate())
	r, err := toolruntime.New(f.s, toolruntime.Config{Connection: c, Credential: "independent-native-tool-key", TrustedRoots: roots, PersistenceTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Execute(context.Background(), l)
	if (err != nil) != f.toolUnknown {
		t.Fatal("unexpected local action outcome", err)
	}
	v, err = f.s.GetToolExecution(context.Background(), f.p, f.task.ID, v.Execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func independentNativeContinue(p domain.ModelToolProposal, v domain.ToolExecutionView) domain.ContinueModelRequest {
	r := domain.ModelToolResultBinding{ProposalID: p.ID, ExecutionID: v.Execution.ID, ExecutionVersion: v.Execution.Version, Status: v.Execution.Status}
	if v.Receipt != nil {
		r.ReceiptID, r.ContentDigest = v.Receipt.ID, v.Receipt.ContentDigest
	}
	return domain.ContinueModelRequest{ExpectedTaskVersion: v.Task.Version, CandidateID: p.CandidateID, ArtifactID: p.ArtifactID, ArtifactVersion: p.ArtifactVersion, ContentDigest: p.ContentDigest, Results: []domain.ModelToolResultBinding{r}, AcknowledgeCost: true, Reason: "Explicit independent controlled continuation and cost acknowledgement"}
}

func TestIndependentNativeModelToolRoundTripPreservesPinsHistoryAndHolds(t *testing.T) {
	f := independentNativeSetup(t, nil)
	ctx := context.Background()
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	p := f.proposal(t)
	if p.ToolID != f.tool.Tool.ID || p.ToolVersion != 1 || p.ToolProfileDigest != domain.ToolConnectionDigest(f.tool) || p.ToolName != domain.ModelToolName(f.p.OrganizationID, p.ToolID, 1) {
		t.Fatal("model proposal did not preserve installed version and profile")
	}
	current := f.current(t)
	_, err := f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), domain.ReviewTaskRequest{ExpectedVersion: current.Version, CandidateID: p.CandidateID, ArtifactID: p.ArtifactID, ArtifactVersion: p.ArtifactVersion, ContentDigest: p.ContentDigest, Decision: "accept"})
	independentToolDeny(t, err)
	materialRaw, err := f.s.CreateMaterial(ctx, f.p, f.task.ID, domain.NewID("key"), domain.CreateMaterial{Name: "Untrusted native proposal draft", MediaType: "text/plain", Content: "A draft cannot convert unresolved native requests into a final answer", BaseCandidateID: p.CandidateID})
	var material domain.Material
	if err != nil || json.Unmarshal(materialRaw, &material) != nil {
		t.Fatal("create independent draft", err)
	}
	current = f.current(t)
	_, err = f.s.PublishMaterial(ctx, f.p, f.task.ID, material.ID, domain.NewID("key"), domain.PublishMaterial{ExpectedTaskVersion: current.Version, ExpectedMaterialVersion: material.HeadVersion, ContentDigest: material.HeadContentDigest, ParentCandidateID: p.CandidateID})
	independentToolDeny(t, err)
	if f.toolCalls.Load() != 0 || f.modelCalls.Load() != 1 {
		t.Fatal("proposal or failed result acceptance performed another external request")
	}
	v := f.dispatchAndObserve(t, f.prepare(t, p))
	if v.Receipt == nil || v.Receipt.Method != "registered_tool_response" || v.Execution.Status != "succeeded" || v.Task.Result != "" {
		t.Fatal("bound action response did not remain an effect observation")
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
	reopened, err := Open(ctx, f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	raw, err := reopened.ContinueModel(ctx, f.p, f.task.ID, domain.NewID("key"), independentNativeContinue(p, v))
	var queued domain.Task
	if err != nil || json.Unmarshal(raw, &queued) != nil || queued.Status != "queued" || queued.CurrentRunID != f.task.CurrentRunID {
		t.Fatal("durable continuation did not preserve Run", err)
	}
	if _, err = f.executeModel(t, reopened); err != nil {
		t.Fatal(err)
	}
	final := f.current(t)
	if final.Status != "waiting_input" || final.WaitingReason != "model_candidate_pending" || final.Result != "" || final.AcceptedDecisionID != "" || final.CandidateID == p.CandidateID {
		t.Fatal("final model output was accepted or intermediate proposal reused")
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 720, 0)
	if f.modelCalls.Load() != 2 || f.toolCalls.Load() != 1 {
		t.Fatal("unexpected physical local model/action calls")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 2 || len(f.requests[0].Tools) != 1 || f.requests[0].Tools[0].Function.Name != p.ToolName || len(f.requests[1].Messages) != 4 {
		t.Fatal("native wire definition or continuation message count incorrect")
	}
	m := f.requests[1].Messages
	if m[0].Role != "system" || m[0].Content != f.agent.Instructions || m[1].Role != "user" || m[2].Role != "assistant" || len(m[2].ToolCalls) != 1 || m[2].ToolCalls[0].Function.Arguments != independentNativeArguments || m[3].Role != "tool" || m[3].ToolCallID != p.CallID || !strings.Contains(m[3].Content, f.toolResult) {
		t.Fatal("continuation promoted tool content, rewrote arguments, or lost native call linkage")
	}
	if strings.Contains(string(f.requests[0].Tools[0].Function.Parameters), "credential") || strings.Contains(f.requests[0].Tools[0].Function.Description, f.toolPeer.URL) {
		t.Fatal("private registry details leaked into model tool definition")
	}
}

func TestIndependentNativeMalformedBatchCreatesNoPartialProposal(t *testing.T) {
	cases := map[string]func(int64, string) []independentNativeCall{
		"duplicate_call_id": func(_ int64, name string) []independentNativeCall {
			return []independentNativeCall{independentNativeCallFor("same", name, independentNativeArguments), independentNativeCallFor("same", name, independentNativeArguments)}
		},
		"unknown_tool_after_valid": func(_ int64, name string) []independentNativeCall {
			return []independentNativeCall{independentNativeCallFor("valid", name, independentNativeArguments), independentNativeCallFor("invalid", "not_pinned", `{}`)}
		},
		"bad_schema_after_valid": func(_ int64, name string) []independentNativeCall {
			return []independentNativeCall{independentNativeCallFor("valid", name, independentNativeArguments), independentNativeCallFor("invalid", name, `{"number":"a string","note":"bad"}`)}
		},
		"duplicate_argument_key": func(_ int64, name string) []independentNativeCall {
			return []independentNativeCall{independentNativeCallFor("invalid", name, `{"number":1,"number":2,"note":"bad"}`)}
		},
		"array_arguments": func(_ int64, name string) []independentNativeCall {
			return []independentNativeCall{independentNativeCallFor("invalid", name, `[]`)}
		},
		"nine_calls": func(_ int64, name string) []independentNativeCall {
			calls := []independentNativeCall{}
			for i := 0; i < 9; i++ {
				calls = append(calls, independentNativeCallFor(fmt.Sprintf("call-%d", i), name, independentNativeArguments))
			}
			return calls
		},
	}
	for name, batch := range cases {
		t.Run(name, func(t *testing.T) {
			f := independentNativeSetup(t, func(f *independentNativeFixture) { f.batch = batch })
			if _, err := f.executeModel(t, f.s); err == nil {
				t.Fatal("malformed native completion was accepted")
			}
			var proposals, executions int
			if err := f.s.Pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM model_tool_proposals WHERE organization_id=$1 AND task_id=$2),(SELECT count(*) FROM tool_executions WHERE organization_id=$1 AND task_id=$2)`, f.p.OrganizationID, f.task.ID).Scan(&proposals, &executions); err != nil || proposals != 0 || executions != 0 {
				t.Fatal("malformed batch partially committed proposal or effect", err)
			}
			if f.current(t).CandidateID != "" || f.modelCalls.Load() != 1 || f.toolCalls.Load() != 0 {
				t.Fatal("malformed batch created candidate or performed effect")
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 300, 0)
		})
	}
}

func TestIndependentNativeUnknownEffectBlocksContinuationAfterConfirmedBill(t *testing.T) {
	f := independentNativeSetup(t, func(f *independentNativeFixture) { f.toolUnknown = true })
	ctx := context.Background()
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	p := f.proposal(t)
	v := f.dispatchAndObserve(t, f.prepare(t, p))
	if v.Execution.Status != "unknown" || v.Receipt != nil {
		t.Fatal("dropped response invented known effect")
	}
	ev := independentEvidence(17)
	ev.Source = f.tool.Tool.Provider
	if _, err := f.s.AccountCost(ctx, f.p.OrganizationID, v.Execution.ReservationID, ev); err != nil {
		t.Fatal(err)
	}
	v, err := f.s.GetToolExecution(ctx, f.p, f.task.ID, v.Execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.ContinueModel(ctx, f.p, f.task.ID, domain.NewID("key"), independentNativeContinue(p, v))
	independentToolDeny(t, err)
	reopened, err := Open(ctx, f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if l, _, err := reopened.ClaimToolExecution(ctx, "independent-after-lost-response", 30*time.Second); err != nil || l != nil {
		t.Fatal("unknown action reclaimed", err)
	}
	if l, _, err := reopened.ClaimModel(ctx, "independent-after-lost-response", 30*time.Second); err != nil || l != nil {
		t.Fatal("unknown effect implicitly continued model", err)
	}
	if f.toolCalls.Load() != 1 || f.modelCalls.Load() != 1 {
		t.Fatal("uncertain effect retried or continued")
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 300, 17)
	raw, err := f.s.ReconcileToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), domain.ReconcileToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, IntentHash: v.Execution.IntentHash, Outcome: "succeeded", EffectReference: "independent-operator-observation", Result: "Explicit synthetic operator assertion", EvidenceDigest: Hash([]byte("independent operator evidence")), Reason: "Explicit effect resolution; not billing authority"})
	v = independentToolView(t, raw, err)
	if _, err = f.s.ContinueModel(ctx, f.p, f.task.ID, domain.NewID("key"), independentNativeContinue(p, v)); err != nil {
		t.Fatal("resolved effect could not explicitly continue", err)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 300, 17)
}

func TestIndependentNativeConcurrentContinueCreatesOneOperationAndCharge(t *testing.T) {
	f := independentNativeSetup(t, nil)
	ctx := context.Background()
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	p := f.proposal(t)
	v := f.dispatchAndObserve(t, f.prepare(t, p))
	in := independentNativeContinue(p, v)
	const n = 12
	var wg sync.WaitGroup
	var successes atomic.Int64
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.s.ContinueModel(ctx, f.p, f.task.ID, domain.NewID("key"), in)
			if err == nil {
				successes.Add(1)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			independentToolDeny(t, err)
		}
	}
	if successes.Load() != 1 {
		t.Fatal("concurrent continuation winners", successes.Load())
	}
	var operations int
	if err := f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM operations WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&operations); err != nil || operations != 2 {
		t.Fatal("continuation duplicated model operation", err)
	}
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	if f.modelCalls.Load() != 2 || f.toolCalls.Load() != 1 {
		t.Fatal("concurrent continuation duplicated physical request")
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 720, 0)
}

func TestIndependentNativeProposalAndContinuationRejectSnapshotSubstitution(t *testing.T) {
	f := independentNativeSetup(t, nil)
	ctx := context.Background()
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	p := f.proposal(t)
	current := f.current(t)
	input := domain.PrepareToolExecution{ModelProposalID: p.ID, OccurrenceID: "model:" + p.ID, ExpectedTaskVersion: current.Version, CandidateID: p.CandidateID, ArtifactID: p.ArtifactID, ArtifactVersion: p.ArtifactVersion, ContentDigest: p.ContentDigest, ToolID: p.ToolID, ToolVersion: p.ToolVersion, ArgumentsJSON: p.ArgumentsJSON, ExpiresInSeconds: 60}
	preparations := map[string]func(*domain.PrepareToolExecution){
		"missing_proposal": func(v *domain.PrepareToolExecution) { v.ModelProposalID = "" },
		"wrong_occurrence": func(v *domain.PrepareToolExecution) { v.OccurrenceID = domain.NewID("occ") },
		"wrong_proposal":   func(v *domain.PrepareToolExecution) { v.ModelProposalID = domain.NewID("mtp") },
		"same_json_different_bytes": func(v *domain.PrepareToolExecution) {
			v.ArgumentsJSON = `{"number":9007199254740993,"note":"Exact original bytes"}`
		},
		"changed_tool":         func(v *domain.PrepareToolExecution) { v.ToolID = "unlisted-tool" },
		"changed_tool_version": func(v *domain.PrepareToolExecution) { v.ToolVersion++ },
		"changed_candidate":    func(v *domain.PrepareToolExecution) { v.CandidateID = domain.NewID("cand") },
		"changed_artifact":     func(v *domain.PrepareToolExecution) { v.ArtifactID = domain.NewID("art") },
		"changed_digest":       func(v *domain.PrepareToolExecution) { v.ContentDigest = Hash([]byte("substitution")) },
	}
	for name, mutate := range preparations {
		t.Run("prepare_"+name, func(t *testing.T) {
			in := input
			mutate(&in)
			_, err := f.s.PrepareToolExecution(ctx, f.p, f.task.ID, domain.NewID("key"), in)
			independentToolDeny(t, err)
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 300, 0)
		})
	}
	other, _ := identity(t, f.s, 10_000)
	if _, err := f.s.ModelProposal(ctx, other, f.task.ID, p.ID); err == nil {
		t.Fatal("foreign organization read exact proposal")
	}
	if _, _, err := f.s.ModelProposals(ctx, other, f.task.ID, "", 100); err == nil {
		t.Fatal("foreign organization listed proposal")
	}
	v := f.dispatchAndObserve(t, f.prepare(t, p))
	input.ExpectedTaskVersion = v.Task.Version
	input.OccurrenceID = domain.NewID("occ")
	if _, err := f.s.PrepareToolExecution(ctx, f.p, f.task.ID, domain.NewID("key"), input); err == nil {
		t.Fatal("one native proposal prepared as a second action occurrence")
	}
	base := independentNativeContinue(p, v)
	continuations := map[string]func(*domain.ContinueModelRequest){
		"missing_result":            func(v *domain.ContinueModelRequest) { v.Results = nil },
		"duplicate_result":          func(v *domain.ContinueModelRequest) { v.Results = append(v.Results, v.Results[0]) },
		"changed_proposal":          func(v *domain.ContinueModelRequest) { v.Results[0].ProposalID = domain.NewID("mtp") },
		"changed_execution":         func(v *domain.ContinueModelRequest) { v.Results[0].ExecutionID = domain.NewID("tex") },
		"changed_execution_version": func(v *domain.ContinueModelRequest) { v.Results[0].ExecutionVersion++ },
		"changed_status":            func(v *domain.ContinueModelRequest) { v.Results[0].Status = "rejected" },
		"changed_receipt":           func(v *domain.ContinueModelRequest) { v.Results[0].ReceiptID = domain.NewID("tor") },
		"changed_receipt_digest":    func(v *domain.ContinueModelRequest) { v.Results[0].ContentDigest = Hash([]byte("substitution")) },
		"changed_candidate":         func(v *domain.ContinueModelRequest) { v.CandidateID = domain.NewID("cand") },
		"changed_artifact_version":  func(v *domain.ContinueModelRequest) { v.ArtifactVersion++ },
		"no_cost_acknowledgement":   func(v *domain.ContinueModelRequest) { v.AcknowledgeCost = false },
	}
	for name, mutate := range continuations {
		t.Run("continue_"+name, func(t *testing.T) {
			in := base
			in.Results = append([]domain.ModelToolResultBinding(nil), base.Results...)
			mutate(&in)
			_, err := f.s.ContinueModel(ctx, f.p, f.task.ID, domain.NewID("key"), in)
			independentToolDeny(t, err)
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
		})
	}
	if f.modelCalls.Load() != 1 || f.toolCalls.Load() != 1 {
		t.Fatal("snapshot substitution performed external work")
	}
	if _, err := f.s.ContinueModel(ctx, f.p, f.task.ID, domain.NewID("key"), base); err != nil {
		t.Fatal("negative substitutions corrupted valid continuation", err)
	}
}

func TestIndependentNativeContinuationRechecksAuthorityProfilesAndRemainingBudget(t *testing.T) {
	for _, boundary := range []string{"owner_before_continue", "owner_after_queue", "tool_retired_after_queue", "model_retired_after_queue", "cancel_after_queue", "remaining_budget"} {
		t.Run(boundary, func(t *testing.T) {
			f := independentNativeSetup(t, nil)
			ctx := context.Background()
			if _, err := f.executeModel(t, f.s); err != nil {
				t.Fatal(err)
			}
			p := f.proposal(t)
			v := f.dispatchAndObserve(t, f.prepare(t, p))
			in := independentNativeContinue(p, v)
			if boundary == "owner_before_continue" {
				if _, err := f.s.Pool.Exec(ctx, "UPDATE memberships SET role='viewer' WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, f.p.ID); err != nil {
					t.Fatal(err)
				}
				_, err := f.s.ContinueModel(ctx, f.p, f.task.ID, domain.NewID("key"), in)
				independentToolDeny(t, err)
				independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
				return
			}
			if boundary == "remaining_budget" {
				if _, err := f.s.Pool.Exec(ctx, "UPDATE tasks SET budget_microusd=500 WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.task.ID); err != nil {
					t.Fatal(err)
				}
			}
			_, err := f.s.ContinueModel(ctx, f.p, f.task.ID, domain.NewID("key"), in)
			if boundary == "remaining_budget" && err != nil {
				independentToolDeny(t, err)
				independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch boundary {
			case "owner_after_queue":
				_, err = f.s.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, f.p.ID)
			case "tool_retired_after_queue":
				_, err = f.s.Pool.Exec(ctx, "UPDATE tool_connections SET active=false WHERE organization_id=$1 AND id=$2 AND version=1", f.p.OrganizationID, f.tool.Tool.ID)
			case "model_retired_after_queue":
				_, err = f.s.Pool.Exec(ctx, "UPDATE model_connections SET active=false WHERE organization_id=$1 AND id=$2 AND version=1", f.p.OrganizationID, f.model.ID)
			case "cancel_after_queue":
				_, err = f.s.CancelTask(ctx, f.p, f.task.ID, domain.NewID("key"))
			}
			if err != nil {
				t.Fatal(err)
			}
			l, c, err := f.s.ClaimModel(ctx, "independent-native-boundary-worker", 30*time.Second)
			if err != nil {
				independentToolDeny(t, err)
			}
			if l != nil {
				if boundary != "remaining_budget" {
					t.Fatal("revoked/cancelled/profile-retired continuation was claimable")
				}
				r, err := modelruntime.New(f.s, RuntimeModelConfig(c, "independent-native-model-key"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := r.Execute(ctx, l); err == nil {
					t.Fatal("next model reserve exceeded remaining budget")
				}
			}
			if f.modelCalls.Load() != 1 || f.toolCalls.Load() != 1 {
				t.Fatal("boundary invalidation allowed another external request")
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
		})
	}
}

func TestIndependentNativeLifetimeTurnLimitSurvivesNewRun(t *testing.T) {
	f := independentNativeSetup(t, func(f *independentNativeFixture) { f.alwaysProposes = true })
	ctx := context.Background()
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	first := f.proposal(t)
	current := f.current(t)
	raw, err := f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), domain.ReviewTaskRequest{ExpectedVersion: current.Version, CandidateID: first.CandidateID, ArtifactID: first.ArtifactID, ArtifactVersion: first.ArtifactVersion, ContentDigest: first.ContentDigest, Decision: "request_changes", Reason: "Explicit new Run to test lifetime turn bound"})
	if err != nil || json.Unmarshal(raw, &current) != nil {
		t.Fatal("cannot request changes to intermediate proposal", err)
	}
	raw, err = f.s.RetryTask(ctx, f.p, f.task.ID, domain.NewID("key"), domain.RetryTaskRequest{ExpectedVersion: current.Version, Reason: "Explicit independent new Run", Feedback: "Use only the pinned native tool", AcknowledgeNewCost: true})
	if err != nil || json.Unmarshal(raw, &current) != nil || current.CurrentRunID == f.task.CurrentRunID {
		t.Fatal("new Run not created", err)
	}
	secondRun := current.CurrentRunID
	for i := 2; i <= domain.MaxModelTurnsPerTask; i++ {
		if _, err := f.executeModel(t, f.s); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		p := f.proposal(t)
		v := f.decide(t, f.prepare(t, p), "deny")
		in := independentNativeContinue(p, v)
		_, err := f.s.ContinueModel(ctx, f.p, f.task.ID, domain.NewID("key"), in)
		if i == domain.MaxModelTurnsPerTask {
			independentToolDeny(t, err)
		} else if err != nil {
			t.Fatalf("continue turn %d: %v", i, err)
		}
	}
	current = f.current(t)
	if current.CurrentRunID != secondRun || f.modelCalls.Load() != int64(domain.MaxModelTurnsPerTask) || f.toolCalls.Load() != 0 {
		t.Fatal("lifetime limit reset on new Run or dispatched denied tool")
	}
	var operations int
	if err := f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM operations WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&operations); err != nil || operations != domain.MaxModelTurnsPerTask {
		t.Fatal("lifetime model turn operations mismatch", err)
	}
	if l, _, err := f.s.ClaimModel(ctx, "independent-beyond-lifetime-cap", 30*time.Second); err != nil || l != nil {
		t.Fatal("lifetime bound queued another model operation", err)
	}
	// Another explicit Run must fail as a bounded product command, rather than
	// reaching a late SQL constraint error or resetting the lifetime counter.
	p := f.proposal(t)
	raw, err = f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), domain.ReviewTaskRequest{ExpectedVersion: current.Version, CandidateID: p.CandidateID, ArtifactID: p.ArtifactID, ArtifactVersion: p.ArtifactVersion, ContentDigest: p.ContentDigest, Decision: "request_changes", Reason: "Verify a new Run cannot reset the exhausted model turn limit"})
	if err != nil || json.Unmarshal(raw, &current) != nil {
		t.Fatal("cannot stop exhausted native proposal", err)
	}
	_, err = f.s.RetryTask(ctx, f.p, f.task.ID, domain.NewID("key"), domain.RetryTaskRequest{ExpectedVersion: current.Version, Reason: "Must reject beyond task lifetime model cap", Feedback: "No additional model turn is authorized", AcknowledgeNewCost: true})
	code(t, err, "invalid_transition")
	if err := f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM operations WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&operations); err != nil || operations != domain.MaxModelTurnsPerTask {
		t.Fatal("rejected new Run created another model operation", err)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, int64(domain.MaxModelTurnsPerTask)*300, 0)
}

func TestIndependentToolSQLIntentRequiresTypedCompleteSnapshot(t *testing.T) {
	f := independentToolSetup(t, false)
	ctx := context.Background()
	v := f.prepare(t)
	v = f.decide(t, v, f.p, "deny")
	// Each direct SQL probe runs in a rolled-back transaction. The valid control
	// reproduces the normal pre-reserve source version -> increment -> insert
	// ordering; this is a JSON persistence guard test, not accounting acceptance.
	probe := func(t *testing.T, mutate func(map[string]any)) error {
		t.Helper()
		tx, err := f.s.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		e := v.Execution
		e.ID, e.OccurrenceID, e.ActionID, e.ReservationID, e.ApprovalID = domain.NewID("tex"), domain.NewID("occ"), domain.NewID("act"), domain.NewID("res"), domain.NewID("apr")
		e.Status, e.Version, e.Reason, e.ResultReceiptID, e.DispatchedBy = "awaiting_approval", 1, "", "", ""
		e.CreatedAt = time.Time{}
		e.SourceTaskVersion = v.Task.Version
		e.ExpiresAt = time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
		e.IntentHash = domain.ToolIntentDigest(e)
		if _, err := tx.Exec(ctx, "INSERT INTO actions(organization_id,id,task_id,operation_id,intent_hash,idempotency_key,status) VALUES($1,$2,$3,$4,$5,$2,'pending')", e.OrganizationID, e.ActionID, e.TaskID, e.OperationID, e.IntentHash); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO reservations(organization_id,id,task_id,operation_id,action_id,root_id,scope_task_ids,charge_key,category,provider,price_version,amount_microusd,status) VALUES($1,$2,$3,$4,$5,$3,$6,$5,'tool',$7,$8,$9,'reserved')", e.OrganizationID, e.ReservationID, e.TaskID, e.OperationID, e.ActionID, []string{e.TaskID}, e.Provider, e.PriceVersion, e.MaxChargeMicroUSD); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "UPDATE tasks SET version=version+1 WHERE organization_id=$1 AND id=$2", e.OrganizationID, e.TaskID); err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(e)
		var intent map[string]any
		if err := json.Unmarshal(body, &intent); err != nil {
			t.Fatal(err)
		}
		if mutate != nil {
			mutate(intent)
		}
		body, _ = json.Marshal(intent)
		_, err = tx.Exec(ctx, `INSERT INTO tool_executions(organization_id,id,task_id,run_id,operation_id,action_id,reservation_id,approval_id,tool_id,tool_version,tool_profile_digest,candidate_id,artifact_id,artifact_version,content_digest,intent_hash,intent,created_by,status,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,'awaiting_approval',$19)`, e.OrganizationID, e.ID, e.TaskID, e.RunID, e.OperationID, e.ActionID, e.ReservationID, e.ApprovalID, e.ToolID, e.ToolVersion, e.ToolProfileDigest, e.CandidateID, e.ArtifactID, e.ArtifactVersion, e.ContentDigest, e.IntentHash, body, e.CreatedBy, e.ExpiresAt)
		if err == nil {
			_, err = tx.Exec(ctx, "INSERT INTO approvals(organization_id,id,task_id,action_id,intent_hash,execution_id,status,expires_at) VALUES($1,$2,$3,$4,$5,$6,'pending',$7)", e.OrganizationID, e.ApprovalID, e.TaskID, e.ActionID, e.IntentHash, e.ID, e.ExpiresAt)
			if err == nil {
				_, err = tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE")
			}
		}
		return err
	}
	t.Run("valid_control", func(t *testing.T) {
		if err := probe(t, nil); err != nil {
			t.Fatal("positive SQL control failed before negative probes", err)
		}
	})
	cases := map[string]func(map[string]any){
		"missing_arguments_and_digest": func(v map[string]any) { delete(v, "arguments_json"); delete(v, "arguments_digest") },
		"null_arguments_and_digest":    func(v map[string]any) { v["arguments_json"], v["arguments_digest"] = nil, nil },
		"missing_source_version":       func(v map[string]any) { delete(v, "source_task_version") },
		"null_source_version":          func(v map[string]any) { v["source_task_version"] = nil },
		"wrong_source_version":         func(v map[string]any) { v["source_task_version"] = 1 },
		"string_source_version":        func(v map[string]any) { v["source_task_version"] = fmt.Sprint(v["source_task_version"]) },
		"missing_expiry":               func(v map[string]any) { delete(v, "expires_at") },
		"null_expiry":                  func(v map[string]any) { v["expires_at"] = nil },
		"wrong_expiry":                 func(v map[string]any) { v["expires_at"] = time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano) },
		"numeric_charge":               func(v map[string]any) { v["max_charge_microusd"] = 120 },
		"null_charge":                  func(v map[string]any) { v["max_charge_microusd"] = nil },
		"noncanonical_charge":          func(v map[string]any) { v["max_charge_microusd"] = "0120" },
		"wrong_provider":               func(v map[string]any) { v["provider"] = "another-account" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if err := probe(t, mutate); err == nil {
				t.Fatal("SQL accepted incomplete, mistyped or mismatched intent")
			}
		})
	}
	// SQL probes cannot persist another effect, approval or financial exposure.
	var executions int
	if err := f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM tool_executions WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&executions); err != nil || executions != 1 {
		t.Fatal("rolled-back SQL probe escaped", err)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 150, 0)
}

func TestIndependentNativeContinuationReauthorizesAfterTreeLockWait(t *testing.T) {
	f := independentNativeSetup(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	p := f.proposal(t)
	v := f.dispatchAndObserve(t, f.prepare(t, p))
	if _, err := f.s.Pool.Exec(ctx, "UPDATE access_tokens SET expires_at=clock_timestamp()+interval '700 milliseconds' WHERE token_hash=$1", f.p.TokenHash); err != nil {
		t.Fatal(err)
	}
	blocker, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	var pid int
	if err := blocker.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, "SELECT root_id FROM task_tree_controls WHERE organization_id=$1 AND root_id=$2 FOR NO KEY UPDATE", f.p.OrganizationID, f.task.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.s.ContinueModel(ctx, f.p, f.task.ID, domain.NewID("key"), independentNativeContinue(p, v))
		done <- err
	}()
	independentCollaborationWaiters(t, f.s, pid, 1)
	for {
		var expired bool
		if err := f.s.Pool.QueryRow(ctx, "SELECT expires_at<clock_timestamp() FROM access_tokens WHERE token_hash=$1", f.p.TokenHash).Scan(&expired); err != nil {
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
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	code(t, <-done, "unauthenticated")
	var operations int
	if err := f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM operations WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&operations); err != nil || operations != 1 {
		t.Fatal("expired authority committed continuation operation", err)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
}

func TestIndependentNativeUpgradePreservesHistoricalRootAboveNewTurnLimit(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for actual PostgreSQL upgrade")
	}
	base, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal("open isolated upgrade base")
	}
	schema := domain.NewID("independent_upgrade")
	if _, err := base.Pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		base.Close()
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		base.Close()
		t.Fatal("parse isolated upgrade configuration")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, err := Open(ctx, u.String())
	if err != nil {
		base.Close()
		t.Fatal("open isolated upgrade schema")
	}
	t.Cleanup(func() {
		s.Close()
		_, err := base.Pool.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		base.Close()
		if err != nil {
			t.Error(err)
		}
	})
	// Stage the actual immutable migrations through013, then construct a legal
	// old-schema tree with129 historical model operations. No inference occurs.
	// The public0.6 tree lifetime allowed up to384 attempts, above the new128.
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "CREATE TABLE schema_migrations(name text PRIMARY KEY,sha256 text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		t.Fatal(err)
	}
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for _, name := range files {
		if name >= "migrations/014_" {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			t.Fatal("stage immutable migration", name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations(name,sha256) VALUES($1,$2)", name, Hash(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// This is an exact013 staging fixture; current authentication requires016.
	// Current constructors run only in the test-only old-gate callback; the
	// fixture does not claim actual execution of a historical binary.
	var bootstrap Bootstrap
	historicalFixtureWithoutRecoveryAdmission(t, s, func() {
		bootstrap, err = s.Bootstrap(ctx, "Historical independent upgrade", 1_000_000)
		if err != nil {
			t.Fatal(err)
		}
	})
	p := domain.Principal{ID: bootstrap.PrincipalID, OrganizationID: bootstrap.OrganizationID, Role: "owner", TokenHash: Hash([]byte(bootstrap.Token)), CredentialKind: "bearer"}
	profile := domain.ModelConnection{ID: "independent-upgrade-model", OrganizationID: p.OrganizationID, Version: 1, Model: "independent-upgrade-model", ProviderAccount: "independent-upgrade-account", PriceVersion: "independent-upgrade-price", CredentialEnvironment: "PLATFORM_MODEL_KEY_UPGRADE", Endpoint: "https://example.invalid/model", MaxChargeMicroUSD: 300, MaxInputTokens: 100, MaxOutputTokens: 64, InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 2_000_000, FixedMicroUSD: 7, MaxInputBytes: 65536, MaxOutputBytes: 4096, TimeoutMilliseconds: 1000, BoundAssumptions: "Historical synthetic state; no external request", TokenLimitField: "max_tokens", Active: true}
	historicalFixtureWithoutRecoveryAdmission(t, s, func() {
		if _, err := s.InstallModelConnection(ctx, profile); err != nil {
			t.Fatal(err)
		}
	})
	rootAgent, modelAgent, rootID := domain.NewID("agt"), domain.NewID("agt"), domain.NewID("tsk")
	if _, err := s.Pool.Exec(ctx, "INSERT INTO agents(organization_id,id,name,instructions,model,tools,budget_microusd,published) VALUES($1,$2,'Historical root','Synthetic historical root',$3,'[]',100000,true),($1,$4,'Historical model','Exact historical instructions',$5,'[]',100000,true)", p.OrganizationID, rootAgent, domain.FixtureModel, modelAgent, profile.Model); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "INSERT INTO tasks(organization_id,id,owner_id,agent_id,agent_version,goal,status,budget_microusd) VALUES($1,$2,$3,$4,1,'Historical root','queued',100000)", p.OrganizationID, rootID, p.ID, rootAgent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "INSERT INTO runs(organization_id,id,task_id,principal_id,agent_id,agent_version,status) VALUES($1,$2,$3,$4,$5,1,'queued')", p.OrganizationID, domain.NewID("run"), rootID, p.ID, rootAgent); err != nil {
		t.Fatal(err)
	}
	var lastChild string
	for i := 0; i < 65; i++ {
		child := domain.NewID("tsk")
		if _, err := s.Pool.Exec(ctx, "INSERT INTO tasks(organization_id,id,owner_id,agent_id,agent_version,goal,status,budget_microusd,parent_id,model_connection_id,model_connection_version) VALUES($1,$2,$3,$4,1,'Historical model child','queued',10000,$5,$6,1)", p.OrganizationID, child, p.ID, modelAgent, rootID, profile.ID); err != nil {
			t.Fatal(err)
		}
		n := 2
		if i == 64 {
			n = 1
		}
		lastChild = child
		for attempt := 1; attempt <= n; attempt++ {
			runID := domain.NewID("run")
			if _, err := s.Pool.Exec(ctx, "INSERT INTO runs(organization_id,id,task_id,principal_id,agent_id,agent_version,status,attempt) VALUES($1,$2,$3,$4,$5,1,'blocked',$6)", p.OrganizationID, runID, child, p.ID, modelAgent, attempt); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Pool.Exec(ctx, "INSERT INTO operations(organization_id,id,task_id,run_id,status) VALUES($1,$2,$3,$4,'blocked')", p.OrganizationID, domain.NewID("op"), child, runID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("upgrade erased/refused valid old history above new cap", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("upgrade replay was not stable", err)
	}
	var tree, turns, own int
	if err := s.Pool.QueryRow(ctx, "SELECT tree_turns,(SELECT count(*) FROM model_turns WHERE organization_id=$1),(SELECT own_turns FROM model_turn_counts WHERE organization_id=$1 AND task_id=$3) FROM model_turn_counts WHERE organization_id=$1 AND task_id=$2", p.OrganizationID, rootID, lastChild).Scan(&tree, &turns, &own); err != nil || tree != 129 || turns != 129 || own != 1 {
		t.Fatal("historical model turns clamped, reset or omitted", tree, turns, own, err)
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE model_turn_counts SET tree_turns=128 WHERE organization_id=$1 AND task_id=$2", p.OrganizationID, rootID); err == nil {
		t.Fatal("over-limit historical counter could reset")
	}
	next, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Rollback(ctx)
	runID := domain.NewID("run")
	if _, err := next.Exec(ctx, "INSERT INTO runs(organization_id,id,task_id,principal_id,agent_id,agent_version,status,attempt) VALUES($1,$2,$3,$4,$5,1,'queued',2)", p.OrganizationID, runID, lastChild, p.ID, modelAgent); err != nil {
		t.Fatal(err)
	}
	if _, err := next.Exec(ctx, "INSERT INTO operations(organization_id,id,task_id,run_id,status) VALUES($1,$2,$3,$4,'queued')", p.OrganizationID, domain.NewID("op"), lastChild, runID); err == nil {
		t.Fatal("historical over-limit root admitted another model turn")
	}
	if err := next.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool.QueryRow(ctx, "SELECT tree_turns FROM model_turn_counts WHERE organization_id=$1 AND task_id=$2", p.OrganizationID, rootID).Scan(&tree); err != nil || tree != 129 {
		t.Fatal("refused new turn changed historical root count", err)
	}
}

func TestIndependentNativeTeamWaitsForProposalAndUnknownEffect(t *testing.T) {
	f := independentNativeSetup(t, func(f *independentNativeFixture) { f.toolUnknown = true })
	ctx := context.Background()
	// The setup's standalone Task is cancelled before any inference. Only the
	// actual managed child below supplies the model proposal and action.
	if _, err := f.s.CancelTask(ctx, f.p, f.task.ID, domain.NewID("key")); err != nil {
		t.Fatal(err)
	}
	a := teamAgent(t, f.s, f.p, "sequence", "all", 1, teamMember("native-child", f.agent))
	raw, err := f.s.CreateTask(ctx, f.p, domain.NewID("key"), domain.CreateTask{Goal: "Wait for a managed native child without consuming its action proposal", AgentID: a.ID, BudgetMicroUSD: 100_000})
	var root domain.Task
	if err != nil || json.Unmarshal(raw, &root) != nil {
		t.Fatal("create managed native team", err)
	}
	teamTick(t, f.s)
	children, _, err := f.s.Children(ctx, f.p, root.ID, "", 100)
	if err != nil || len(children) != 1 || children[0].CurrentRunID == "" {
		t.Fatal("managed native child not activated", err)
	}
	f.task = children[0]
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	p := f.proposal(t)
	assertWaiting := func(t *testing.T, wantReason string) {
		t.Helper()
		teamTick(t, f.s)
		parent, err := f.s.GetTask(ctx, f.p, root.ID)
		if err != nil || parent.Status != "running" || parent.CandidateID != "" || parent.Result != "" {
			t.Fatal("coordinator closed or consumed intermediate native work", err)
		}
		child := f.current(t)
		if child.WaitingReason != wantReason || domain.Terminal(child.Status) {
			t.Fatal("coordinator closed child requiring explicit action resolution")
		}
		var status string
		var selected bool
		if err := f.s.Pool.QueryRow(ctx, "SELECT status,selected_candidate_id IS NOT NULL FROM team_members WHERE organization_id=$1 AND parent_run_id=$2", f.p.OrganizationID, root.CurrentRunID).Scan(&status, &selected); err != nil || status != "running" || selected {
			t.Fatal("native proposal consumed as final team member output", err)
		}
	}
	assertWaiting(t, "model_tools_pending")
	v := f.dispatchAndObserve(t, f.prepare(t, p))
	if v.Execution.Status != "unknown" {
		t.Fatal("expected actual dropped-response uncertainty")
	}
	assertWaiting(t, "external_action_unknown")
	independentBalances(t, f.s, f.p.OrganizationID, []string{root.ID, f.task.ID}, 420, 0)
	raw, err = f.s.ReconcileToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), domain.ReconcileToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, IntentHash: v.Execution.IntentHash, Outcome: "succeeded", EffectReference: "independent-team-operator-observation", Result: "Explicit synthetic operator observation", EvidenceDigest: Hash([]byte("independent team effect evidence")), Reason: "Explicit managed-child effect resolution"})
	v = independentToolView(t, raw, err)
	if _, err := f.s.ContinueModel(ctx, f.p, f.task.ID, domain.NewID("key"), independentNativeContinue(p, v)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	teamTick(t, f.s)
	parent, err := f.s.GetTask(ctx, f.p, root.ID)
	if err != nil || parent.Status != "waiting_input" || parent.WaitingReason != "team_candidate_pending" || parent.CandidateID == "" || parent.Result != "" || parent.AcceptedDecisionID != "" {
		t.Fatal("final child output did not become an unverified team candidate", err)
	}
	child := f.current(t)
	var selected string
	if err := f.s.Pool.QueryRow(ctx, "SELECT selected_candidate_id FROM team_members WHERE organization_id=$1 AND parent_run_id=$2", f.p.OrganizationID, root.CurrentRunID).Scan(&selected); err != nil || selected != child.CandidateID || selected == p.CandidateID {
		t.Fatal("coordinator selected wrong child candidate", err)
	}
	if f.modelCalls.Load() != 2 || f.toolCalls.Load() != 1 {
		t.Fatal("managed native flow duplicated physical model/action requests")
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{root.ID, f.task.ID}, 720, 0)
}

func TestIndependentNativeForgedWorkerContextCannotReserveOrSend(t *testing.T) {
	f := independentNativeSetup(t, nil)
	ctx := context.Background()
	l, c, err := f.s.ClaimModel(ctx, "independent-forged-native-worker", 30*time.Second)
	if err != nil || l == nil {
		t.Fatal(err)
	}
	runtime, err := modelruntime.New(f.s, RuntimeModelConfig(c, "independent-native-model-key"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*domain.Lease){
		"instructions":              func(v *domain.Lease) { v.Agent.Instructions = "Injected system instruction" },
		"definition_description":    func(v *domain.Lease) { v.ModelTools[0].Description = "Forged tool description" },
		"definition_schema":         func(v *domain.Lease) { v.ModelTools[0].InputSchemaJSON = `{"type":"object"}` },
		"definition_profile_digest": func(v *domain.Lease) { v.ModelTools[0].ToolProfileDigest = Hash([]byte("another profile")) },
		"definition_version":        func(v *domain.Lease) { v.ModelTools[0].ToolVersion++ },
		"history_goal":              func(v *domain.Lease) { v.ModelMessages[1].Content = "Forged frozen goal" },
		"history_extra_user": func(v *domain.Lease) {
			v.ModelMessages = append(v.ModelMessages, domain.ModelMessage{Role: "user", Content: "Injected follow-up"})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			forged := *l
			forged.Agent.ModelTools = append([]domain.ModelToolDefinition(nil), l.Agent.ModelTools...)
			forged.Agent.Tools = append([]string(nil), l.Agent.Tools...)
			forged.ModelTools = append([]domain.ModelToolDefinition(nil), l.ModelTools...)
			forged.ModelMessages = append([]domain.ModelMessage(nil), l.ModelMessages...)
			mutate(&forged)
			if _, err := runtime.Execute(ctx, &forged); err == nil {
				t.Fatal("forged worker context was accepted")
			}
			if f.modelCalls.Load() != 0 || f.toolCalls.Load() != 0 {
				t.Fatal("forged worker snapshot performed I/O")
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 0, 0)
		})
	}
	if _, err := runtime.Execute(ctx, l); err != nil {
		t.Fatal("negative worker substitutions corrupted legitimate context", err)
	}
	if f.modelCalls.Load() != 1 {
		t.Fatal("valid model call missing after rejected worker context")
	}
}

func TestIndependentNativeLateResolutionAfterCancelCannotContinue(t *testing.T) {
	f := independentNativeSetup(t, func(f *independentNativeFixture) { f.toolUnknown = true })
	ctx := context.Background()
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	p := f.proposal(t)
	v := f.dispatchAndObserve(t, f.prepare(t, p))
	if _, err := f.s.CancelTask(ctx, f.p, f.task.ID, domain.NewID("key")); err != nil {
		t.Fatal(err)
	}
	v, err := f.s.GetToolExecution(ctx, f.p, f.task.ID, v.Execution.ID)
	if err != nil || v.Task.Status != "cancelled" || v.Execution.Status != "unknown" {
		t.Fatal("cancel did not preserve admitted uncertainty", err)
	}
	raw, err := f.s.ReconcileToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), domain.ReconcileToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, IntentHash: v.Execution.IntentHash, Outcome: "succeeded", EffectReference: "late-independent-operator-observation", Result: "Late synthetic effect observation after cancellation", EvidenceDigest: Hash([]byte("late independent native effect evidence")), Reason: "Record historical effect without restarting task"})
	v = independentToolView(t, raw, err)
	if v.Receipt == nil || v.Task.Status != "cancelled" {
		t.Fatal("late receipt missing or cancelled task resurrected")
	}
	_, err = f.s.ContinueModel(ctx, f.p, f.task.ID, domain.NewID("key"), independentNativeContinue(p, v))
	independentToolDeny(t, err)
	if f.modelCalls.Load() != 1 || f.toolCalls.Load() != 1 {
		t.Fatal("late cancelled effect performed new external request")
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
}
