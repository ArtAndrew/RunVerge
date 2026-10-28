// SPDX-License-Identifier: Elastic-2.0
package httpapi

// Real TLS HTTP and PostgreSQL; login is a trusted fixture session. The model
// peer only supplies a candidate, and no external tool is actually called here.
import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
	"agentplatform.local/platform/internal/modelruntime"
	"agentplatform.local/platform/internal/storage"
)

func independentToolHTTPFixture(t *testing.T) (*independentBrowserFixture, domain.PrepareToolExecution, string) {
	t.Helper()
	f := independentBrowserAPI(t)
	ctx := context.Background()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"tool-http-model-fixture","choices":[{"index":0,"message":{"role":"assistant","content":"Unverified candidate for explicit tool approval"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	t.Cleanup(model.Close)
	c, e := f.store.InstallModelConnection(ctx, domain.ModelConnection{ID: "tool-http-model", OrganizationID: f.bootstrap.OrganizationID, Version: 1,
		Model: "tool-http-model-fixture", ProviderAccount: "tool-http-model-account", PriceVersion: "fixture-v1", Endpoint: model.URL,
		CredentialEnvironment: "PLATFORM_MODEL_KEY_INDEPENDENT_TOOL_HTTP", MaxChargeMicroUSD: 150, MaxInputTokens: 100, MaxOutputTokens: 16,
		InputMicroUSDPerMillion: 1000000, OutputMicroUSDPerMillion: 1000000, MaxInputBytes: 4096, MaxOutputBytes: 1024,
		TimeoutMilliseconds: 2000, BoundAssumptions: "Explicit test assumptions; fixture usage is not a bill", TokenLimitField: "max_tokens", AllowInsecureLoopback: true, Active: true})
	if e != nil {
		t.Fatal(e)
	}
	body, _ := json.Marshal(map[string]any{"name": "Tool HTTP candidate", "instructions": "Return test text", "model": c.Model, "budget_microusd": 10000})
	_, raw := f.request("POST", "/api/v1/agents", string(body), nil, 201)
	var ar struct {
		Data domain.Agent `json:"data"`
	}
	if json.Unmarshal(raw, &ar) != nil || ar.Data.ID == "" {
		t.Fatal("missing model agent")
	}
	f.request("POST", "/api/v1/agents/"+ar.Data.ID+"/publish", `{"expected_version":1}`, nil, 200)
	body, _ = json.Marshal(domain.CreateTask{AgentID: ar.Data.ID, Goal: "Check exact approved action", BudgetMicroUSD: 10000})
	_, raw = f.request("POST", "/api/v1/tasks", string(body), nil, 201)
	var tr struct {
		Data domain.Task `json:"data"`
	}
	if json.Unmarshal(raw, &tr) != nil || tr.Data.ID == "" {
		t.Fatal("missing model task")
	}
	lease, _, e := f.store.ClaimModel(ctx, "independent-tool-http", 30*time.Second)
	if e != nil || lease == nil || lease.TaskID != tr.Data.ID {
		t.Fatal("could not claim fixture task", e)
	}
	runtime, e := modelruntime.New(f.store, storage.RuntimeModelConfig(c, "local-fixture-key"))
	if e != nil {
		t.Fatal(e)
	}
	if out, e := runtime.Execute(ctx, lease); e != nil || out.Status != modelruntime.CandidateWaitingInput {
		t.Fatal("candidate not created", e)
	}
	_, raw = f.request("GET", "/api/v1/tasks/"+tr.Data.ID, "", nil, 200)
	if json.Unmarshal(raw, &tr) != nil {
		t.Fatal("missing current task")
	}
	_, raw = f.request("GET", "/api/v1/tasks/"+tr.Data.ID+"/candidates", "", nil, 200)
	var cr struct {
		Data []domain.TaskCandidate `json:"data"`
	}
	if json.Unmarshal(raw, &cr) != nil || len(cr.Data) != 1 {
		t.Fatal("missing exact candidate")
	}
	schema := `{"type":"object","properties":{},"additionalProperties":false}`
	tool, e := f.store.InstallToolConnection(ctx, domain.ToolConnection{Tool: domain.ToolInfo{ID: "tool-http-fixture", OrganizationID: f.bootstrap.OrganizationID, Version: 1,
		Name: "Tool HTTP fixture", Description: "Registered but never sent by these tests", Effect: "write", Destination: "Fixture service", Provider: "tool-fixture-account",
		PriceVersion: "fixture-price", MaxChargeMicroUSD: 120, InputSchemaJSON: schema, InputSchemaDigest: storage.Hash([]byte(schema)),
		TimeoutSeconds: 3, MaxInputBytes: 4096, MaxOutputBytes: 4096, Active: true}, Transport: "http_json", ResultContract: "agent_platform_action_v1",
		Endpoint: "https://registered-tool.invalid/private-endpoint", CredentialEnvironment: "PLATFORM_TOOL_KEY_PRIVATE_INDEPENDENT"})
	if e != nil {
		t.Fatal(e)
	}
	can := cr.Data[0]
	in := domain.PrepareToolExecution{OccurrenceID: "http-explicit-occurrence", ExpectedTaskVersion: tr.Data.Version, CandidateID: can.ID,
		ArtifactID: can.ArtifactID, ArtifactVersion: can.ArtifactVersion, ContentDigest: can.ContentDigest, ToolID: tool.Tool.ID, ToolVersion: 1,
		ArgumentsJSON: `{}`, ExpiresInSeconds: 60}
	return f, in, "/api/v1/tasks/" + tr.Data.ID + "/tool-executions"
}

func TestIndependentToolHTTPRejectsAmbiguousInputsWithoutAdmission(t *testing.T) {
	f, in, path := independentToolHTTPFixture(t)
	valid, _ := json.Marshal(in)
	var values map[string]any
	if e := json.Unmarshal(valid, &values); e != nil {
		t.Fatal(e)
	}
	for _, test := range []struct {
		name, field string
		value       any
		omit        bool
	}{
		{"missing_arguments", "arguments_json", nil, true},
		{"null_arguments", "arguments_json", nil, false},
		{"object_arguments", "arguments_json", map[string]any{}, false},
		{"duplicate_argument_property", "arguments_json", `{"x":1,"x":2}`, false},
		{"invalid_inner_unicode", "arguments_json", `{"x":"\ud800"}`, false},
		{"trailing_arguments", "arguments_json", `{} {}`, false},
		{"unregistered_property", "arguments_json", `{"x":1}`, false},
		{"oversized_arguments", "arguments_json", `{"x":"` + strings.Repeat("a", domain.MaxToolArgumentsBytes) + `"}`, false},
		{"forged_actor", "created_by", "someone-else", false},
		{"forged_destination", "endpoint", "https://other.invalid", false},
		{"forged_price", "max_charge_microusd", "0", false},
		{"case_alias", "ToolID", "other", false},
		{"expiry_zero", "expires_in_seconds", 0, false},
		{"expiry_over_cap", "expires_in_seconds", 86401, false},
		{"occurrence_empty", "occurrence_id", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.t = t
			var v map[string]any
			_ = json.Unmarshal(valid, &v)
			if test.omit {
				delete(v, test.field)
			} else {
				v[test.field] = test.value
			}
			body, _ := json.Marshal(v)
			f.request("POST", path, string(body), nil, 400)
		})
	}
	f.t = t
	f.request("POST", path, strings.TrimSuffix(string(valid), "}")+`,"tool_id":"shadow"}`, nil, 400)
	f.request("POST", path, string(valid), func(r *http.Request) { r.Header.Del("X-CSRF-Token") }, 403)
	f.request("POST", path, string(valid), func(r *http.Request) { r.Header.Set("Origin", "https://attacker.invalid") }, 403)
	var count int
	if e := f.store.Pool.QueryRow(context.Background(), "SELECT count(*) FROM tool_executions WHERE organization_id=$1", f.bootstrap.OrganizationID).Scan(&count); e != nil || count != 0 {
		t.Fatalf("invalid HTTP calls admitted %d executions: %v", count, e)
	}
	// Successful control proves invalid requests were not merely hitting an absent route.
	_, raw := f.request("POST", path, string(valid), nil, 201)
	var v struct {
		Data domain.ToolExecutionView `json:"data"`
	}
	if e := json.Unmarshal(raw, &v); e != nil || v.Data.Execution.Status != "awaiting_approval" || v.Data.Task.Status != "waiting_approval" {
		t.Fatal("valid prepare did not create stopped approval", e)
	}
}

func TestIndependentToolHTTPReadsAndCommandsKeepTenantAndSecretBoundary(t *testing.T) {
	f, in, path := independentToolHTTPFixture(t)
	_, raw := f.request("GET", "/api/v1/tools", "", nil, 200)
	for _, secret := range []string{"PLATFORM_TOOL_KEY_PRIVATE_INDEPENDENT", "registered-tool.invalid/private-endpoint", "credential_environment", "allow_private_network"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("tool discovery exposed private configuration")
		}
	}
	body, _ := json.Marshal(in)
	_, raw = f.request("POST", path, string(body), nil, 201)
	var result struct {
		Data domain.ToolExecutionView `json:"data"`
	}
	if e := json.Unmarshal(raw, &result); e != nil || result.Data.Execution.ID == "" {
		t.Fatal("missing prepared execution")
	}
	v := result.Data
	base := path + "/" + v.Execution.ID
	foreign := func(r *http.Request) {
		r.Header.Del("Cookie")
		r.Header.Set("Authorization", "Bearer "+f.other.Token)
		r.Header.Set("X-Organization-ID", f.other.OrganizationID)
	}
	for _, endpoint := range []string{path, base} {
		f.request("GET", endpoint, "", nil, 200)
		f.request("GET", endpoint, "", foreign, 404)
		f.request("GET", endpoint+"?unknown=1", "", nil, 400)
	}
	decision, _ := json.Marshal(domain.DecideToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, ExpectedApprovalVersion: v.Approval.Version,
		IntentHash: v.Execution.IntentHash, Decision: "approve", Reason: "Explicit independent test approval"})
	dispatch, _ := json.Marshal(domain.DispatchToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, ExpectedApprovalVersion: v.Approval.Version,
		IntentHash: v.Execution.IntentHash, AcknowledgeCost: true})
	reconcile, _ := json.Marshal(domain.ReconcileToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, IntentHash: v.Execution.IntentHash,
		Outcome: "succeeded", EffectReference: "operator-fixture-ref", Result: "operator assertion", EvidenceDigest: strings.Repeat("a", 64), Reason: "Explicit fixture only"})
	for _, command := range []struct {
		name string
		body []byte
	}{{"decide", decision}, {"dispatch", dispatch}, {"reconcile", reconcile}} {
		t.Run(command.name, func(t *testing.T) {
			f.t = t
			f.request("POST", base+"/"+command.name, string(command.body), foreign, 404)
			f.request("POST", base+"/"+command.name, string(command.body), func(r *http.Request) { r.Header.Del("X-CSRF-Token") }, 403)
			f.request("POST", base+"/"+command.name, string(command.body), func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.bootstrap.Token) }, 401)
		})
	}
	f.t = t
	// Neither a caller's claimed effect result nor a dispatch acknowledgement is valid before approval.
	f.request("POST", base+"/dispatch", string(dispatch), nil, 409)
	f.request("POST", base+"/reconcile", string(reconcile), nil, 409)
	if _, e := f.store.Pool.Exec(context.Background(), "UPDATE memberships SET role='viewer' WHERE organization_id=$1 AND principal_id=$2", f.bootstrap.OrganizationID, f.bootstrap.PrincipalID); e != nil {
		t.Fatal(e)
	}
	f.request("GET", base, "", nil, 200)
	f.request("POST", base+"/decide", string(decision), nil, 403)
}

func TestIndependentToolMCPApprovalUsesExactDurableRESTBinding(t *testing.T) {
	f, in, path := independentToolHTTPFixture(t)
	taskID := strings.Split(path, "/")[4]
	backend := mcpbridge.HandlerBackend((&Server{Store: f.store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler())
	newEndpoint := func(allow []string) *httptest.Server {
		t.Helper()
		adapter, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: backend, AllowedMutations: allow})
		if e != nil {
			t.Fatal(e)
		}
		peer := httptest.NewTLSServer(adapter.Handler())
		t.Cleanup(peer.Close)
		return peer
	}
	peer := newEndpoint([]string{"platform_tool_executions_prepare", "platform_tool_executions_decide", "platform_tool_executions_dispatch"})
	var sequence int
	call := func(endpoint *httptest.Server, name string, args any, foreign bool) (domain.ToolExecutionView, bool) {
		t.Helper()
		sequence++
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": sequence, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": mcpbridge.ProtocolVersion, "io.modelcontextprotocol/clientCapabilities": map[string]any{}}}})
		r, e := http.NewRequest("POST", endpoint.URL, bytes.NewReader(body))
		if e != nil {
			t.Fatal(e)
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
		response, e := endpoint.Client().Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		raw, e := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if e != nil {
			t.Fatal(e)
		}
		if response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("MCP action response is cacheable")
		}
		var out struct {
			Error  json.RawMessage `json:"error"`
			Result struct {
				IsError    bool `json:"isError"`
				Structured struct {
					Data domain.ToolExecutionView `json:"data"`
				} `json:"structuredContent"`
			} `json:"result"`
		}
		if json.Unmarshal(raw, &out) != nil {
			t.Fatal("invalid MCP JSON response")
		}
		ok := response.StatusCode == 200 && len(out.Error) == 0 && !out.Result.IsError
		return out.Result.Structured.Data, ok
	}
	prepare := map[string]any{"task_id": taskID, "idempotency_key": "independent-mcp-action-prepare", "input": in}
	if _, ok := call(newEndpoint(nil), "platform_tool_executions_prepare", prepare, false); ok {
		t.Fatal("default readonly MCP server prepared an external action")
	}
	if _, ok := call(peer, "platform_tool_executions_prepare", prepare, true); ok {
		t.Fatal("foreign MCP credential prepared known tenant task")
	}
	v, ok := call(peer, "platform_tool_executions_prepare", prepare, false)
	if !ok || v.Execution.ID == "" || v.Execution.Status != "awaiting_approval" {
		t.Fatal("exact MCP prepare did not reach authenticated REST")
	}
	replay, ok := call(peer, "platform_tool_executions_prepare", prepare, false)
	if !ok || replay.Execution.ID != v.Execution.ID {
		t.Fatal("different RPC IDs changed durable action occurrence")
	}
	bad := in
	bad.ExpiresInSeconds++
	if _, ok = call(peer, "platform_tool_executions_prepare", map[string]any{"task_id": taskID, "idempotency_key": "independent-mcp-action-prepare", "input": bad}, false); ok {
		t.Fatal("same durable key authorized changed expiry")
	}
	d := domain.DecideToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, ExpectedApprovalVersion: v.Approval.Version, IntentHash: v.Execution.IntentHash, Decision: "approve", Reason: "Explicit MCP principal decision"}
	badDecision := d
	badDecision.IntentHash = strings.Repeat("0", 64)
	if _, ok = call(peer, "platform_tool_executions_decide", map[string]any{"task_id": taskID, "execution_id": v.Execution.ID, "idempotency_key": "wrong-mcp-intent", "input": badDecision}, false); ok {
		t.Fatal("MCP approved another intent")
	}
	v, ok = call(peer, "platform_tool_executions_decide", map[string]any{"task_id": taskID, "execution_id": v.Execution.ID, "idempotency_key": "independent-mcp-decision", "input": d}, false)
	if !ok || v.Execution.Status != "approved" {
		t.Fatal("explicit MCP approval missing")
	}
	dispatch := domain.DispatchToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, ExpectedApprovalVersion: v.Approval.Version, IntentHash: v.Execution.IntentHash, AcknowledgeCost: true}
	dispatchArgs := map[string]any{"task_id": taskID, "execution_id": v.Execution.ID, "idempotency_key": "independent-mcp-dispatch", "input": dispatch}
	v, ok = call(peer, "platform_tool_executions_dispatch", dispatchArgs, false)
	if !ok || v.Execution.Status != "queued" {
		t.Fatal("MCP dispatch did not queue exact approved job")
	}
	var jobs, sent int
	if e := f.store.Pool.QueryRow(context.Background(), "SELECT count(*),count(dispatch_issued_at) FROM tool_executions WHERE organization_id=$1", f.bootstrap.OrganizationID).Scan(&jobs, &sent); e != nil || jobs != 1 || sent != 0 {
		t.Fatal("MCP command multiplied occurrence or sent inline", e)
	}
	if _, e := f.store.Pool.Exec(context.Background(), "UPDATE memberships SET role='viewer' WHERE organization_id=$1 AND principal_id=$2", f.bootstrap.OrganizationID, f.bootstrap.PrincipalID); e != nil {
		t.Fatal(e)
	}
	if _, ok = call(peer, "platform_tool_executions_dispatch", dispatchArgs, false); ok {
		t.Fatal("MCP cached dispatch bypassed current permission")
	}
	if _, ok = call(peer, "platform_tool_executions_get", map[string]any{"task_id": taskID, "execution_id": v.Execution.ID}, false); !ok {
		t.Fatal("current viewer cannot inspect approved action")
	}
}
