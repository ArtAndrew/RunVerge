// SPDX-License-Identifier: Elastic-2.0
package mcpbridge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"agentplatform.local/platform/internal/mcpbridge"
)

func TestCapabilityMCPToolReadsExactlyOneScopedRouteWithoutOptIn(t *testing.T) {
	var calls, probes atomic.Int64
	backend := mcpbridge.BackendFunc(func(_ context.Context, c mcpbridge.Credentials, r mcpbridge.Request) (mcpbridge.Response, error) {
		if c != credentials || r.Method != http.MethodGet || len(r.Query) != 0 || len(r.Body) != 0 || r.IdempotencyKey != "" {
			t.Error("capability tool crossed its fixed read boundary")
		}
		if r.Path == "/api/v1/me" {
			// The server reauthenticates before discovery and calls. This is a
			// separate read, not another invocation of the capability operation.
			probes.Add(1)
			return mcpbridge.Response{Status: 200, Body: json.RawMessage(`{"data":{"fixture":true}}`)}, nil
		}
		if r.Path != "/api/v1/capabilities" {
			t.Error("unexpected backend route")
		}
		calls.Add(1)
		return mcpbridge.Response{Status: 200, Body: json.RawMessage(`{"data":{"catalog_version":"1","read_only":true}}`)}, nil
	})
	bridge, err := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(bridge.Handler())
	defer server.Close()
	status, out := rawHTTP(t, server.Client(), server.URL, wire(1, "tools/call", map[string]any{"name": "platform_capabilities", "arguments": map[string]any{}}), nil)
	if status != 200 || out["error"] != nil || calls.Load() != 1 || probes.Load() != 1 {
		t.Fatal("registered capability tool was not reachable through default read-only policy")
	}
	result(t, out)
	for _, args := range []map[string]any{{"organization_id": "org_other"}, {"limit": 1}, {"idempotency_key": "no-write"}} {
		status, out = rawHTTP(t, server.Client(), server.URL, wire(2, "tools/call", map[string]any{"name": "platform_capabilities", "arguments": args}), nil)
		if status != 400 || out["error"] == nil || calls.Load() != 1 {
			t.Fatal("capability tool accepted authority, pagination or mutation input")
		}
	}
}
