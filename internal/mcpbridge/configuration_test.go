// SPDX-License-Identifier: Elastic-2.0
package mcpbridge_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agentplatform.local/platform/internal/mcpbridge"
)

func TestConfigurationMCPMutationRequiresOptInAndPreservesExactScopeAndCostCommand(t *testing.T) {
	var mu sync.Mutex
	var writes []mcpbridge.Request
	backend := mcpbridge.BackendFunc(func(_ context.Context, _ mcpbridge.Credentials, in mcpbridge.Request) (mcpbridge.Response, error) {
		mu.Lock()
		if in.Method == "POST" {
			writes = append(writes, in)
		}
		mu.Unlock()
		return mcpbridge.Response{Status: 200, Body: json.RawMessage(`{"data":{"id":"cfg_probe"}}`)}, nil
	})
	input := map[string]any{"expected_plan_version": 2, "plan_digest": strings.Repeat("a", 64), "operation_id": "model", "max_charge_microusd": "150", "acknowledge_cost": true, "reason": "Explicit connection check"}
	args := map[string]any{"name": "platform_configuration_plans_verify", "arguments": map[string]any{"plan_id": "cfg_plan", "idempotency_key": "same-explicit-key", "input": input}}
	for _, enabled := range []bool{false, true} {
		allowed := []string{}
		if enabled {
			allowed = []string{"platform_configuration_plans_verify"}
		}
		bridge, err := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: backend, AllowedMutations: allowed})
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewTLSServer(bridge.Handler())
		_, out := rawHTTP(t, server.Client(), server.URL, wire(1, "tools/call", args), nil)
		server.Close()
		if !enabled && out["error"] == nil {
			t.Fatal("configuration mutation available without explicit opt-in")
		}
		if enabled {
			result(t, out)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(writes) != 1 || writes[0].Method != "POST" || writes[0].Path != "/api/v1/configuration/plans/cfg_plan/verify" || writes[0].IdempotencyKey != "same-explicit-key" {
		t.Fatal("configuration action crossed wrong boundary or was retried")
	}
	wanted, _ := json.Marshal(input)
	if string(writes[0].Body) != string(wanted) {
		t.Fatal("exact decimal cost/intent changed")
	}
}
