// SPDX-License-Identifier: Elastic-2.0
package mcpbridge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agentplatform.local/platform/internal/mcpbridge"
)

func TestAutomationMCPOptInAndExactDelegationPayload(t *testing.T) {
	var mu sync.Mutex
	var requests []mcpbridge.Request
	backend := mcpbridge.BackendFunc(func(_ context.Context, _ mcpbridge.Credentials, r mcpbridge.Request) (mcpbridge.Response, error) {
		mu.Lock()
		if r.Method == http.MethodPost {
			requests = append(requests, r)
		}
		mu.Unlock()
		return mcpbridge.Response{Status: 200, Body: json.RawMessage(`{"data":{"fixture":true}}`)}, nil
	})
	start := func(allowed []string) *httptest.Server {
		s, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: backend, AllowedMutations: allowed})
		if e != nil {
			t.Fatal(e)
		}
		server := httptest.NewTLSServer(s.Handler())
		t.Cleanup(server.Close)
		return server
	}
	readonly := start(nil)
	_, out := rawHTTP(t, readonly.Client(), readonly.URL, wire(1, "tools/list", nil), nil)
	names := map[string]bool{}
	for _, tool := range result(t, out)["tools"].([]any) {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	if !names["platform_automation_get"] || !names["platform_progress_signals_list"] || names["platform_automation_create"] || names["platform_automation_revoke"] {
		t.Fatal("automationcatalog overclaims authority")
	}
	_, out = rawHTTP(t, readonly.Client(), readonly.URL, wire(2, "tools/call", map[string]any{"name": "platform_automation_create", "arguments": map[string]any{}}), nil)
	if out["error"] == nil {
		t.Fatal("delegation enabled bydefault")
	}
	mu.Lock()
	n := len(requests)
	mu.Unlock()
	if n != 0 {
		t.Fatal("disabled mutation reachedbackend")
	}
	server := start([]string{"platform_automation_create", "platform_automation_revoke"})
	input := map[string]any{"expected_task_version": 2, "mode": "read_only", "tool_pins": []any{map[string]any{"tool_id": "lookup", "tool_version": 1, "tool_profile_digest": strings.Repeat("a", 64)}}, "max_model_turns": 2, "max_tool_executions": 3, "max_charge_microusd": "1000000000000", "expires_in_seconds": 60, "acknowledge_cost": true, "reason": "Explicitdelegation"}
	_, out = rawHTTP(t, server.Client(), server.URL, wire(3, "tools/call", map[string]any{"name": "platform_automation_create", "arguments": map[string]any{"task_id": "tsk_example", "idempotency_key": "delegation-1", "input": input}}), nil)
	if out["error"] != nil || result(t, out)["isError"] == true {
		t.Fatalf("optin create rejected: %v", out)
	}
	_, out = rawHTTP(t, server.Client(), server.URL, wire(4, "tools/call", map[string]any{"name": "platform_automation_revoke", "arguments": map[string]any{"task_id": "tsk_example", "idempotency_key": "delegation-2", "input": map[string]any{"grant_id": "aut_example", "expected_version": 1, "reason": "Stopfuturecalls"}}}), nil)
	if out["error"] != nil || result(t, out)["isError"] == true {
		t.Fatalf("optin revoke rejected: %v", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 || requests[0].Method != http.MethodPost || requests[0].Path != "/api/v1/tasks/tsk_example/automation" || requests[0].IdempotencyKey != "delegation-1" || requests[1].Path != "/api/v1/tasks/tsk_example/automation/revoke" {
		t.Fatal("delegationpath/keychanged")
	}
	var actual map[string]any
	if json.Unmarshal(requests[0].Body, &actual) != nil || actual["max_charge_microusd"] != "1000000000000" {
		t.Fatal("MCProundedmoney")
	}
}
