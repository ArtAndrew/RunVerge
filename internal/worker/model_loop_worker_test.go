// SPDX-License-Identifier: Elastic-2.0
package worker_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/worker"
)

// This is an author integration test of the actual worker scheduler, native
// model HTTP profile, PostgreSQL and a real local TLS effect peer. Both peers
// are explicit synthetic fixtures, not a paid model or a real business action.
func TestWorkerNativeEightCallBatchContinuesAfterExplicitActionsAndRestart(t *testing.T) {
	ctx := context.Background()
	s := workerStore(t)
	b, err := s.Bootstrap(ctx, "Native worker fixture", 100000)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Authenticate(ctx, b.Token, b.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	var modelCalls, toolCalls atomic.Int32
	var actionMu sync.Mutex
	actions := map[string]bool{}
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		toolCalls.Add(1)
		var in struct {
			ExecutionID string          `json:"execution_id"`
			ActionID    string          `json:"action_id"`
			IntentHash  string          `json:"intent_hash"`
			Arguments   json.RawMessage `json:"arguments"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil || in.ExecutionID == "" || r.Header.Get("Idempotency-Key") != in.ActionID {
			t.Error("tool request binding failed")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		actionMu.Lock()
		if actions[in.ActionID] {
			t.Error("the same physical action was resent")
		}
		actions[in.ActionID] = true
		actionMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": in.ExecutionID, "action_id": in.ActionID, "intent_hash": in.IntentHash, "outcome": "succeeded", "effect_reference": "synthetic-worker-effect", "result": "Untrusted tool output; never a system instruction"})
	}))
	defer peer.Close()
	schema := `{"type":"object","properties":{"number":{"type":"integer"}},"required":["number"],"additionalProperties":false}`
	tool, err := s.InstallToolConnection(ctx, domain.ToolConnection{Tool: domain.ToolInfo{ID: "native-worker-tool", OrganizationID: p.OrganizationID, Version: 1, Name: "Native worker fixture", Description: "Bounded synthetic integer action", Effect: "write", Destination: "Explicit local TLS fixture", Provider: "worker-tool-account", PriceVersion: "v1", MaxChargeMicroUSD: 120, InputSchemaJSON: schema, TimeoutSeconds: 3, MaxInputBytes: 4096, MaxOutputBytes: 4096, Active: true}, Transport: "http_json", ResultContract: "agent_platform_action_v1", Endpoint: peer.URL, AllowPrivateNetwork: true, TrustedCAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: peer.Certificate().Raw}))})
	if err != nil {
		t.Fatal(err)
	}
	name := domain.ModelToolName(p.OrganizationID, tool.Tool.ID, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := modelCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic-native-model-credential" {
			t.Error("unexpected model credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var request struct {
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				ToolCallID string `json:"tool_call_id"`
				ToolCalls  []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Tools) != 1 || request.Tools[0].Function.Name != name {
			t.Error("model did not receive the exact pinned native tool")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			if len(request.Messages) != 2 {
				t.Error("initial transcript changed")
			}
			calls := make([]any, domain.MaxModelToolCalls)
			for i := range calls {
				calls[i] = map[string]any{"id": fmt.Sprintf("worker_call_%d", i), "type": "function", "function": map[string]any{"name": name, "arguments": fmt.Sprintf(`{ "number" : %d }`, i)}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "synthetic-native-proposal", "choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "content": nil, "tool_calls": calls}}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20}})
			return
		}
		if n != 2 || len(request.Messages) != 11 || request.Messages[0].Role != "system" || request.Messages[1].Role != "user" || request.Messages[2].Role != "assistant" || len(request.Messages[2].ToolCalls) != 8 {
			t.Error("durable native continuation transcript was lost or replayed")
		}
		for i, m := range request.Messages[3:] {
			if m.Role != "tool" || m.ToolCallID != fmt.Sprintf("worker_call_%d", i) {
				t.Error("tool output was promoted or attached to a different native call")
			}
			var result struct {
				Status    string `json:"status"`
				Untrusted bool   `json:"untrusted"`
				Receipt   struct {
					Method string `json:"method"`
				} `json:"receipt"`
			}
			if json.Unmarshal([]byte(m.Content), &result) != nil || result.Status != "succeeded" || !result.Untrusted || result.Receipt.Method != "registered_tool_response" {
				t.Error("continuation lost exact observation provenance")
			}
		}
		_, _ = io.WriteString(w, `{"id":"synthetic-native-final","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Final unverified answer after eight observed effects"}}],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}}`)
	}))
	defer provider.Close()
	_, err = s.InstallModelConnection(ctx, domain.ModelConnection{ID: "native-worker-model", OrganizationID: p.OrganizationID, Version: 1, Model: "native-worker-wire", ProviderAccount: "worker-model-account", PriceVersion: "v1", Endpoint: provider.URL, CredentialEnvironment: "PLATFORM_MODEL_KEY_NATIVE_WORKER", MaxChargeMicroUSD: 1000, MaxInputTokens: 1000, InputMicroUSDPerMillion: 1000, OutputMicroUSDPerMillion: 1000, FixedMicroUSD: 1, MaxOutputTokens: 1000, MaxInputBytes: 65536, MaxOutputBytes: 32768, TimeoutMilliseconds: 3000, BoundAssumptions: "Synthetic local native fixture; bounded inputs are an operator assumption", TokenLimitField: "max_tokens", AllowInsecureLoopback: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	budget := int64(10000)
	raw, err := s.CreateAgent(ctx, p, domain.NewID("key"), domain.CreateAgent{Name: "Native worker", Instructions: "Produce a bounded candidate", Model: "native-worker-wire", Tools: []string{tool.Tool.ID}, BudgetMicroUSD: &budget})
	if err != nil {
		t.Fatal(err)
	}
	var a domain.Agent
	if err = json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PublishAgent(ctx, p, a.ID, domain.NewID("key"), a.Version); err != nil {
		t.Fatal(err)
	}
	raw, err = s.CreateTask(ctx, p, domain.NewID("key"), domain.CreateTask{Goal: "Use eight explicit synthetic actions then answer", AgentID: a.ID, BudgetMicroUSD: 10000})
	if err != nil {
		t.Fatal(err)
	}
	var task domain.Task
	if err = json.Unmarshal(raw, &task); err != nil {
		t.Fatal(err)
	}
	start := func() func() {
		run, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		w := &worker.Worker{Store: s, ID: domain.NewID("native_worker"), Concurrency: 2, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ResolveModelCredential: func(string) (string, error) { return "synthetic-native-model-credential", nil }}
		go func() { done <- w.Run(run) }()
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
					t.Error("native worker did not stop")
				}
			})
		}
		t.Cleanup(stop)
		return stop
	}
	stop := start()
	await(t, func() bool {
		current, e := s.GetTask(ctx, p, task.ID)
		if e == nil && current.WaitingReason == "model_tools_pending" {
			task = current
			return true
		}
		return false
	})
	if toolCalls.Load() != 0 || modelCalls.Load() != 1 {
		t.Fatal("model proposal implicitly executed an effect or another model turn")
	}
	initialRun := task.CurrentRunID
	proposals, _, err := s.ModelProposals(ctx, p, task.ID, "", 100)
	if err != nil || len(proposals) != 8 {
		t.Fatal("native batch was not persisted atomically", len(proposals), err)
	}
	results := make([]domain.ModelToolResultBinding, 0, 8)
	for _, meta := range proposals {
		if meta.ArgumentsJSON != "" {
			t.Fatal("proposal list leaked full argument content")
		}
		proposal, e := s.ModelProposal(ctx, p, task.ID, meta.ID)
		if e != nil {
			t.Fatal(e)
		}
		raw, e = s.PrepareToolExecution(ctx, p, task.ID, domain.NewID("key"), domain.PrepareToolExecution{OccurrenceID: "model:" + proposal.ID, ModelProposalID: proposal.ID, ExpectedTaskVersion: task.Version, CandidateID: proposal.CandidateID, ArtifactID: proposal.ArtifactID, ArtifactVersion: proposal.ArtifactVersion, ContentDigest: proposal.ContentDigest, ToolID: proposal.ToolID, ToolVersion: proposal.ToolVersion, ArgumentsJSON: proposal.ArgumentsJSON, ExpiresInSeconds: 60})
		if e != nil {
			t.Fatal(e)
		}
		var v domain.ToolExecutionView
		if e = json.Unmarshal(raw, &v); e != nil {
			t.Fatal(e)
		}
		before := toolCalls.Load()
		raw, e = s.DecideToolExecution(ctx, p, task.ID, v.Execution.ID, domain.NewID("key"), domain.DecideToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, ExpectedApprovalVersion: v.Approval.Version, IntentHash: v.Execution.IntentHash, Decision: "approve", Reason: "Explicit synthetic worker test"})
		if e != nil {
			t.Fatal(e)
		}
		if e = json.Unmarshal(raw, &v); e != nil {
			t.Fatal(e)
		}
		if toolCalls.Load() != before {
			t.Fatal("approval implicitly dispatched a native proposal")
		}
		if _, e = s.DispatchToolExecution(ctx, p, task.ID, v.Execution.ID, domain.NewID("key"), domain.DispatchToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, ExpectedApprovalVersion: v.Approval.Version, IntentHash: v.Execution.IntentHash, AcknowledgeCost: true}); e != nil {
			t.Fatal(e)
		}
		await(t, func() bool {
			current, e := s.GetToolExecution(ctx, p, task.ID, v.Execution.ID)
			if e == nil && current.Execution.Status == "succeeded" {
				v = current
				task = current.Task
				return true
			}
			return false
		})
		if v.Receipt == nil {
			t.Fatal("action lacks exact observed receipt")
		}
		results = append(results, domain.ModelToolResultBinding{ProposalID: proposal.ID, ExecutionID: v.Execution.ID, ExecutionVersion: v.Execution.Version, Status: v.Execution.Status, ReceiptID: v.Receipt.ID, ContentDigest: v.Receipt.ContentDigest})
	}
	stop()
	if toolCalls.Load() != 8 || modelCalls.Load() != 1 || task.ReservedMicroUSD != 1960 {
		t.Fatal("unexpected dispatch count or liability before continuation")
	}
	currentCandidates, _, err := s.Candidates(ctx, p, task.ID, "", 100)
	if err != nil || len(currentCandidates) != 1 {
		t.Fatal("proposal candidate missing", err)
	}
	candidate := currentCandidates[0]
	key := domain.NewID("key")
	request := domain.ContinueModelRequest{ExpectedTaskVersion: task.Version, CandidateID: candidate.ID, ArtifactID: candidate.ArtifactID, ArtifactVersion: candidate.ArtifactVersion, ContentDigest: candidate.ContentDigest, Results: results, AcknowledgeCost: true, Reason: "Continue from all eight exact synthetic observations"}
	raw, err = s.ContinueModel(ctx, p, task.ID, key, request)
	if err != nil {
		t.Fatal(err)
	}
	var queued domain.Task
	if err = json.Unmarshal(raw, &queued); err != nil {
		t.Fatal(err)
	}
	task = queued
	if task.Status != "queued" || task.CurrentRunID != initialRun || task.AttemptCount != 1 || task.CandidateID != "" || modelCalls.Load() != 1 {
		t.Fatal("continuation started a new Run, retained old candidate, or dispatched implicitly")
	}
	if _, err = s.ContinueModel(ctx, p, task.ID, key, request); err != nil {
		t.Fatal("durable same-key continuation replay failed", err)
	}
	stop = start()
	await(t, func() bool {
		current, e := s.GetTask(ctx, p, task.ID)
		if e == nil && current.WaitingReason == "model_candidate_pending" {
			task = current
			return true
		}
		return false
	})
	stop()
	if modelCalls.Load() != 2 || toolCalls.Load() != 8 || task.ReservedMicroUSD != 2960 || task.SpentMicroUSD != 0 || task.CurrentRunID != initialRun || task.AttemptCount != 1 {
		t.Fatal("restarted worker repeated an effect or lost existing uncertain liabilities")
	}
	var turns, ops int
	if err = s.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM model_turns WHERE organization_id=$1 AND task_id=$2),(SELECT count(*) FROM operations WHERE organization_id=$1 AND task_id=$2)", p.OrganizationID, task.ID).Scan(&turns, &ops); err != nil || turns != 2 || ops != 2 {
		t.Fatal("continuation failed to create one durable model operation", turns, ops, err)
	}
}
