// SPDX-License-Identifier: Elastic-2.0
package httpapi

// Actual TLS REST/MCP and isolated PostgreSQL with a trusted browser-session
// fixture. Local native model usage is synthetic; no external tool is sent and
// this does not claim a new IdP login/OAuth or paid-provider interoperability.
import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
	"agentplatform.local/platform/internal/modelruntime"
	"agentplatform.local/platform/internal/storage"
)

type independentNativeHTTPFixture struct {
	*independentBrowserFixture
	proposal domain.ModelToolProposal
	input    domain.ContinueModelRequest
	taskID   string
	listPath string
	path     string
	calls    *atomic.Int64
}

func independentNativeHTTPReady(t *testing.T) *independentNativeHTTPFixture {
	t.Helper()
	f := &independentNativeHTTPFixture{independentBrowserFixture: independentBrowserAPI(t), calls: new(atomic.Int64)}
	ctx := context.Background()
	schema := `{"type":"object","properties":{},"additionalProperties":false}`
	tool, err := f.store.InstallToolConnection(ctx, domain.ToolConnection{Tool: domain.ToolInfo{ID: "independent-native-http-tool", OrganizationID: f.bootstrap.OrganizationID, Version: 1,
		Name: "Independent native HTTP fixture", Description: "No external action is sent by these boundary tests", Effect: "write", Destination: "Declared synthetic destination", Provider: "independent-native-http-account", PriceVersion: "fixture-price-1", MaxChargeMicroUSD: 120,
		InputSchemaJSON: schema, InputSchemaDigest: storage.Hash([]byte(schema)), TimeoutSeconds: 3, MaxInputBytes: 4096, MaxOutputBytes: 4096, Active: true},
		Transport: "http_json", ResultContract: "agent_platform_action_v1", Endpoint: "https://native-http-tool.invalid/private-endpoint", CredentialEnvironment: "PLATFORM_TOOL_KEY_NATIVE_HTTP_PRIVATE"})
	if err != nil {
		t.Fatal(err)
	}
	alias := domain.ModelToolName(f.bootstrap.OrganizationID, tool.Tool.ID, tool.Tool.Version)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		var request struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Tools) != 1 || request.Tools[0].Function.Name != alias {
			t.Error("native boundary model received wrong tool pin")
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "independent-native-http-reply", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "independent-native-http-call", "type": "function", "function": map[string]any{"name": alias, "arguments": "{}"}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}})
	}))
	t.Cleanup(model.Close)
	c, err := f.store.InstallModelConnection(ctx, domain.ModelConnection{ID: "independent-native-http-model", OrganizationID: f.bootstrap.OrganizationID, Version: 1,
		Model: "independent-native-http-fixture", ProviderAccount: "independent-native-http-model-account", PriceVersion: "fixture-v1", Endpoint: model.URL, CredentialEnvironment: "PLATFORM_MODEL_KEY_NATIVE_HTTP_TEST", MaxChargeMicroUSD: 150, MaxInputTokens: 100, MaxOutputTokens: 16,
		InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 1_000_000, MaxInputBytes: 16384, MaxOutputBytes: 4096, TimeoutMilliseconds: 2000, BoundAssumptions: "Explicit local fixture; usage does not establish a bill", TokenLimitField: "max_tokens", AllowInsecureLoopback: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"name": "Independent native boundary agent", "instructions": "Use only the pinned native function", "model": c.Model, "tools": []string{tool.Tool.ID}, "budget_microusd": 10000})
	_, raw := f.request("POST", "/api/v1/agents", string(body), nil, 201)
	var ar struct {
		Data domain.Agent `json:"data"`
	}
	if json.Unmarshal(raw, &ar) != nil || len(ar.Data.ModelTools) != 1 {
		t.Fatal("agent API omitted frozen native tool definition")
	}
	f.request("POST", "/api/v1/agents/"+ar.Data.ID+"/publish", `{"expected_version":1}`, nil, 200)
	body, _ = json.Marshal(domain.CreateTask{AgentID: ar.Data.ID, Goal: "Check native proposal and continuation HTTP boundary", BudgetMicroUSD: 10000})
	_, raw = f.request("POST", "/api/v1/tasks", string(body), nil, 201)
	var tr struct {
		Data domain.Task `json:"data"`
	}
	if json.Unmarshal(raw, &tr) != nil || tr.Data.ID == "" {
		t.Fatal("native boundary task missing")
	}
	f.taskID, f.listPath, f.path = tr.Data.ID, "/api/v1/tasks/"+tr.Data.ID+"/model-tool-proposals", "/api/v1/tasks/"+tr.Data.ID+"/continue-model"
	l, _, err := f.store.ClaimModel(ctx, "independent-native-http-worker", 30*time.Second)
	if err != nil || l == nil || l.TaskID != f.taskID {
		t.Fatal("native boundary task not claimable", err)
	}
	runtime, err := modelruntime.New(f.store, storage.RuntimeModelConfig(c, "independent-local-native-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Execute(ctx, l); err != nil {
		t.Fatal(err)
	}
	_, raw = f.request("GET", f.listPath, "", nil, 200)
	var proposals struct {
		Data []domain.ModelToolProposal `json:"data"`
	}
	if json.Unmarshal(raw, &proposals) != nil || len(proposals.Data) != 1 || proposals.Data[0].ArgumentsJSON != "" {
		t.Fatal("native HTTP list does not return metadata only")
	}
	_, raw = f.request("GET", f.listPath+"/"+proposals.Data[0].ID, "", nil, 200)
	var proposal struct {
		Data domain.ModelToolProposal `json:"data"`
	}
	if json.Unmarshal(raw, &proposal) != nil || proposal.Data.ArgumentsJSON != "{}" {
		t.Fatal("exact native proposal unavailable")
	}
	f.proposal = proposal.Data
	_, raw = f.request("GET", "/api/v1/tasks/"+f.taskID, "", nil, 200)
	if json.Unmarshal(raw, &tr) != nil {
		t.Fatal("current native task unavailable")
	}
	prepare := domain.PrepareToolExecution{ModelProposalID: f.proposal.ID, OccurrenceID: "model:" + f.proposal.ID, ExpectedTaskVersion: tr.Data.Version, CandidateID: f.proposal.CandidateID, ArtifactID: f.proposal.ArtifactID, ArtifactVersion: f.proposal.ArtifactVersion, ContentDigest: f.proposal.ContentDigest, ToolID: f.proposal.ToolID, ToolVersion: f.proposal.ToolVersion, ArgumentsJSON: "{}", ExpiresInSeconds: 60}
	body, _ = json.Marshal(prepare)
	_, raw = f.request("POST", "/api/v1/tasks/"+f.taskID+"/tool-executions", string(body), nil, 201)
	var execution struct {
		Data domain.ToolExecutionView `json:"data"`
	}
	if json.Unmarshal(raw, &execution) != nil || execution.Data.Execution.ID == "" {
		t.Fatal("native action not prepared")
	}
	v := execution.Data
	body, _ = json.Marshal(domain.DecideToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, ExpectedApprovalVersion: v.Approval.Version, IntentHash: v.Execution.IntentHash, Decision: "deny", Reason: "Explicit independent denial, without external action"})
	_, raw = f.request("POST", "/api/v1/tasks/"+f.taskID+"/tool-executions/"+v.Execution.ID+"/decide", string(body), nil, 200)
	if json.Unmarshal(raw, &execution) != nil || execution.Data.Execution.Status != "denied" {
		t.Fatal("native action not denied")
	}
	v = execution.Data
	f.input = domain.ContinueModelRequest{ExpectedTaskVersion: v.Task.Version, CandidateID: f.proposal.CandidateID, ArtifactID: f.proposal.ArtifactID, ArtifactVersion: f.proposal.ArtifactVersion, ContentDigest: f.proposal.ContentDigest, Results: []domain.ModelToolResultBinding{{ProposalID: f.proposal.ID, ExecutionID: v.Execution.ID, ExecutionVersion: v.Execution.Version, Status: "denied"}}, AcknowledgeCost: true, Reason: "Explicit native HTTP continuation with possible new cost"}
	return f
}

func TestIndependentNativeHTTPStrictTenantAndReplayBoundary(t *testing.T) {
	f := independentNativeHTTPReady(t)
	foreign := func(r *http.Request) {
		r.Header.Del("Cookie")
		r.Header.Set("Authorization", "Bearer "+f.other.Token)
		r.Header.Set("X-Organization-ID", f.other.OrganizationID)
	}
	body, _ := json.Marshal(f.input)
	for _, path := range []string{f.listPath, f.listPath + "/" + f.proposal.ID} {
		f.request("GET", path, "", foreign, 404)
		f.request("GET", path+"?unknown=1", "", nil, 400)
	}
	for _, query := range []string{"?limit=", "?limit=1&limit=2", "?limit=101", "?cursor=" + strings.Repeat("x", 513)} {
		f.request("GET", f.listPath+query, "", nil, 400)
	}
	for _, change := range []func(*http.Request){func(r *http.Request) { r.Header.Del("X-CSRF-Token") }, func(r *http.Request) { r.Header.Set("Origin", "https://untrusted.invalid") }} {
		f.request("POST", f.path, string(body), change, 403)
	}
	f.request("POST", f.path, string(body), foreign, 404)
	f.request("POST", f.path, string(body), func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.bootstrap.Token) }, 401)
	f.request("POST", f.path+"?unknown=1", string(body), nil, 400)
	f.request("POST", f.path, strings.TrimSuffix(string(body), "}")+`,"expected_task_version":1}`, nil, 400)
	f.request("POST", f.path, strings.TrimSuffix(string(body), "}")+`,"endpoint":"https://untrusted.invalid"}`, nil, 400)
	key := domain.NewID("key")
	sameKey := func(r *http.Request) { r.Header.Set("Idempotency-Key", key) }
	_, raw := f.request("POST", f.path, string(body), sameKey, 200)
	var first struct {
		Data domain.Task `json:"data"`
	}
	if json.Unmarshal(raw, &first) != nil || first.Data.Status != "queued" {
		t.Fatal("valid native HTTP continuation not queued")
	}
	_, raw = f.request("POST", f.path, string(body), sameKey, 200)
	var replay struct {
		Data domain.Task `json:"data"`
	}
	if json.Unmarshal(raw, &replay) != nil || replay.Data.CurrentRunID != first.Data.CurrentRunID || replay.Data.Version != first.Data.Version {
		t.Fatal("HTTP durable replay created another continuation")
	}
	if _, err := f.store.Pool.Exec(context.Background(), "UPDATE memberships SET role='viewer' WHERE organization_id=$1 AND principal_id=$2", f.bootstrap.OrganizationID, f.bootstrap.PrincipalID); err != nil {
		t.Fatal(err)
	}
	f.request("POST", f.path, string(body), sameKey, 403)
	f.request("GET", f.listPath+"/"+f.proposal.ID, "", nil, 200)
	if f.calls.Load() != 1 {
		t.Fatal("HTTP continuation command performed inference inline")
	}
}

func TestIndependentNativeMCPUsesNewRoutesAndFreshRESTAuthority(t *testing.T) {
	f := independentNativeHTTPReady(t)
	backend := mcpbridge.HandlerBackend((&Server{Store: f.store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler())
	endpoint := func(allowed []string) *httptest.Server {
		adapter, err := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: backend, AllowedMutations: allowed})
		if err != nil {
			t.Fatal(err)
		}
		peer := httptest.NewTLSServer(adapter.Handler())
		t.Cleanup(peer.Close)
		return peer
	}
	readonly, write := endpoint(nil), endpoint([]string{"platform_model_continue"})
	sequence := 0
	call := func(peer *httptest.Server, name string, arguments any, foreign bool) (json.RawMessage, bool) {
		t.Helper()
		sequence++
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": sequence, "method": "tools/call", "params": map[string]any{"name": name, "arguments": arguments, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": mcpbridge.ProtocolVersion, "io.modelcontextprotocol/clientCapabilities": map[string]any{}}}})
		r, err := http.NewRequest("POST", peer.URL, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("MCP-Method", "tools/call")
		r.Header.Set("MCP-Name", name)
		r.Header.Set("MCP-Protocol-Version", mcpbridge.ProtocolVersion)
		credential := f.bootstrap
		if foreign {
			credential = f.other
		}
		r.Header.Set("Authorization", "Bearer "+credential.Token)
		r.Header.Set("X-Organization-ID", credential.OrganizationID)
		response, err := peer.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		if response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("MCP native result is cacheable")
		}
		var out struct {
			Error  json.RawMessage `json:"error"`
			Result struct {
				IsError    bool `json:"isError"`
				Structured struct {
					Data json.RawMessage `json:"data"`
				} `json:"structuredContent"`
			} `json:"result"`
		}
		if json.Unmarshal(raw, &out) != nil {
			t.Fatal("invalid MCP native response")
		}
		return out.Result.Structured.Data, response.StatusCode == 200 && len(out.Error) == 0 && !out.Result.IsError
	}
	raw, ok := call(readonly, "platform_model_proposals_list", map[string]any{"task_id": f.taskID}, false)
	var proposals []domain.ModelToolProposal
	if !ok || json.Unmarshal(raw, &proposals) != nil || len(proposals) != 1 || proposals[0].ArgumentsJSON != "" {
		t.Fatal("readonly MCP native metadata did not reach REST")
	}
	raw, ok = call(readonly, "platform_model_proposals_get", map[string]any{"task_id": f.taskID, "proposal_id": f.proposal.ID}, false)
	var proposal domain.ModelToolProposal
	if !ok || json.Unmarshal(raw, &proposal) != nil || proposal.ArgumentsJSON != "{}" || proposal.ID != f.proposal.ID {
		t.Fatal("exact MCP native read lost proposal binding")
	}
	for _, secret := range []string{"PLATFORM_TOOL_KEY_NATIVE_HTTP_PRIVATE", "native-http-tool.invalid/private-endpoint", "credential_environment"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("MCP native read exposed private registry configuration")
		}
	}
	if _, ok := call(write, "platform_model_proposals_get", map[string]any{"task_id": f.taskID, "proposal_id": f.proposal.ID}, true); ok {
		t.Fatal("foreign MCP credential read exact native proposal")
	}
	args := map[string]any{"task_id": f.taskID, "idempotency_key": "independent-native-mcp-continue", "input": f.input}
	if _, ok := call(readonly, "platform_model_continue", args, false); ok {
		t.Fatal("default readonly MCP queued model continuation")
	}
	if _, ok := call(write, "platform_model_continue", args, true); ok {
		t.Fatal("foreign MCP credential queued model continuation")
	}
	if _, ok := call(write, "platform_model_continue", map[string]any{"task_id": f.taskID, "idempotency_key": "wrong-native-path", "input": f.input, "path": "/unregistered"}, false); ok {
		t.Fatal("MCP accepted arbitrary route override")
	}
	raw, ok = call(write, "platform_model_continue", args, false)
	var queued domain.Task
	if !ok || json.Unmarshal(raw, &queued) != nil || queued.Status != "queued" {
		t.Fatal("MCP explicit native continuation failed")
	}
	raw, ok = call(write, "platform_model_continue", args, false)
	var replay domain.Task
	if !ok || json.Unmarshal(raw, &replay) != nil || replay.Version != queued.Version || replay.CurrentRunID != queued.CurrentRunID {
		t.Fatal("new RPC ID duplicated durable native continuation")
	}
	changed := f.input
	changed.Reason = "Different intent under same durable key"
	if _, ok := call(write, "platform_model_continue", map[string]any{"task_id": f.taskID, "idempotency_key": "independent-native-mcp-continue", "input": changed}, false); ok {
		t.Fatal("MCP durable key authorized changed continuation")
	}
	if _, err := f.store.Pool.Exec(context.Background(), "UPDATE memberships SET role='viewer' WHERE organization_id=$1 AND principal_id=$2", f.bootstrap.OrganizationID, f.bootstrap.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if _, ok := call(write, "platform_model_continue", args, false); ok {
		t.Fatal("MCP cached continuation bypassed current permission")
	}
	if _, ok := call(write, "platform_model_proposals_get", map[string]any{"task_id": f.taskID, "proposal_id": f.proposal.ID}, false); !ok {
		t.Fatal("current viewer cannot inspect native proposal")
	}
	var operations int
	if err := f.store.Pool.QueryRow(context.Background(), "SELECT count(*) FROM operations WHERE organization_id=$1 AND task_id=$2", f.bootstrap.OrganizationID, f.taskID).Scan(&operations); err != nil || operations != 2 || f.calls.Load() != 1 {
		t.Fatal("MCP requests duplicated durable work or performed inference inline", err)
	}
}
