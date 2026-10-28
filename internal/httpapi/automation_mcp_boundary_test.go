// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
)

// Real TLS MCP -> ordinary REST authorization -> real PostgreSQL consent.
// No worker is started: discovery/consent cannot itself perform a tool effect.
func TestAutomationMCPRealAPIDelegationIdempotencyAndCurrentAuthority(t *testing.T) {
	f := independentAutomaticWorkerSetup(t)
	backend := mcpbridge.HandlerBackend((&Server{Store: f.store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler())
	adapter, err := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: backend, AllowedMutations: []string{"platform_automation_create", "platform_automation_revoke"}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(adapter.Handler())
	defer server.Close()
	seq := 0
	call := func(name, key string, input any) map[string]any {
		t.Helper()
		seq++
		args := map[string]any{"task_id": f.task.ID}
		if key != "" {
			args["idempotency_key"] = key
			args["input"] = input
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": seq, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": mcpbridge.ProtocolVersion, "io.modelcontextprotocol/clientCapabilities": map[string]any{}}}})
		r, _ := http.NewRequest("POST", server.URL, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("MCP-Protocol-Version", mcpbridge.ProtocolVersion)
		r.Header.Set("MCP-Method", "tools/call")
		r.Header.Set("MCP-Name", name)
		r.Header.Set("Authorization", "Bearer "+f.bootstrap.Token)
		r.Header.Set("X-Organization-ID", f.bootstrap.OrganizationID)
		response, e := server.Client().Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		var out struct {
			Result map[string]any `json:"result"`
			Error  any            `json:"error"`
		}
		if json.NewDecoder(response.Body).Decode(&out) != nil || response.StatusCode != 200 || out.Error != nil {
			t.Fatal("MCPtransportfailed")
		}
		return out.Result
	}
	state := func(result map[string]any) domain.TaskAutomationState {
		t.Helper()
		if result["isError"] == true {
			t.Fatalf("MCPAPIcommandfailed:%v", result)
		}
		envelope, ok := result["structuredContent"].(map[string]any)
		if !ok {
			t.Fatal("missingMCPstructuredresponse")
		}
		body, _ := json.Marshal(envelope["data"])
		var value domain.TaskAutomationState
		if json.Unmarshal(body, &value) != nil {
			t.Fatal("badautomationresponse")
		}
		return value
	}
	before := state(call("platform_automation_get", "", nil))
	if before.Grant == nil || before.Grant.ID != f.grant.Grant.ID {
		t.Fatal("MCPreadchangedgrant")
	}
	revoked := state(call("platform_automation_revoke", "revoke-existing", domain.RevokeTaskAutomation{GrantID: before.Grant.ID, ExpectedVersion: before.Grant.Version, Reason: "ExplicitMCPoperatorrevocation"}))
	if revoked.Grant.Status != "revoked" {
		t.Fatal("MCPgrantnotrevoked")
	}
	g := before.Grant
	in := domain.CreateTaskAutomation{ExpectedTaskVersion: f.task.Version, Mode: g.Mode, ToolPins: g.ToolPins, MaxModelTurns: g.MaxModelTurns, MaxToolExecutions: g.MaxToolExecutions, MaxChargeMicroUSD: g.MaxChargeMicroUSD, ExpiresInSeconds: 60, AcknowledgeCost: true, Reason: "ExplicitMCPdurabledelegation"}
	created := state(call("platform_automation_create", "new-grant", in))
	replay := state(call("platform_automation_create", "new-grant", in))
	if created.Grant == nil || created.Grant.ID == g.ID || replay.Grant.ID != created.Grant.ID || created.Usage != (domain.AutomationUsage{}) {
		t.Fatal("MCPidempotencyorusagechanged")
	}
	forged, _ := json.Marshal(in)
	var fields map[string]any
	_ = json.Unmarshal(forged, &fields)
	fields["principal_id"] = "forged-principal"
	if call("platform_automation_create", "forged-authority", fields)["isError"] != true {
		t.Fatal("MCPinjectedprincipalaccepted")
	}
	if _, err = f.store.Pool.Exec(context.Background(), "UPDATE memberships SET role='viewer' WHERE organization_id=$1 AND principal_id=$2", f.bootstrap.OrganizationID, f.bootstrap.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if call("platform_automation_revoke", "stale-role", domain.RevokeTaskAutomation{GrantID: created.Grant.ID, ExpectedVersion: created.Grant.Version, Reason: "Mustdenystalerole"})["isError"] != true {
		t.Fatal("MCPstaleroleauthorizedmutation")
	}
	after := state(call("platform_automation_get", "", nil))
	if len(after.AvailableCommands) != 0 || after.Grant.Status != "active" || f.readCalls.Load() != 0 || f.modelCalls.Load() != 0 {
		t.Fatal("MCPreadgrantedauthorityorcausedeffect")
	}
}
