// SPDX-License-Identifier: Elastic-2.0
package mcpbridge_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agentplatform.local/platform/internal/httpapi"
	"agentplatform.local/platform/internal/mcpbridge"
)

func TestMembershipMCPRealPGCommandsScopeReplayAndCurrentRole(t *testing.T) {
	s := pgStore(t)
	ctx := context.Background()
	owner, e := s.Bootstrap(ctx, "MCP membership fixture", 10000)
	if e != nil {
		t.Fatal(e)
	}
	other, e := s.Bootstrap(ctx, "Other organization fixture", 10000)
	if e != nil {
		t.Fatal(e)
	}
	adapter, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: mcpbridge.HandlerBackend((&httpapi.Server{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()), AllowedMutations: allMutations})
	if e != nil {
		t.Fatal(e)
	}
	endpoint := httptest.NewTLSServer(adapter.Handler())
	defer endpoint.Close()
	serial := 0
	call := func(name string, args map[string]any, token string) map[string]any {
		t.Helper()
		serial++
		_, out := rawHTTP(t, endpoint.Client(), endpoint.URL, wire(serial, "tools/call", map[string]any{"name": name, "arguments": args}), func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("X-Organization-ID", owner.OrganizationID)
		})
		if out["error"] != nil {
			t.Fatal("unexpected MCP protocol error")
		}
		return result(t, out)
	}
	data := func(v map[string]any) map[string]any {
		t.Helper()
		if v["isError"] == true {
			t.Fatal("unexpected membership API failure")
		}
		return v["structuredContent"].(map[string]any)["data"].(map[string]any)
	}
	if data(call("platform_members_capabilities", map[string]any{}, owner.Token))["can_create_members"] != true {
		t.Fatal("owner capabilities missing")
	}
	create := map[string]any{"idempotency_key": "mcp-member-new", "input": map[string]any{"display_name": "Controlled member", "role": "member", "reason": "MCP fixture provisioning"}}
	member := data(call("platform_members_create", create, owner.Token))
	pid := member["principal_id"].(string)
	if replay := data(call("platform_members_create", create, owner.Token)); replay["principal_id"] != pid {
		t.Fatal("replay duplicated principal")
	}
	if call("platform_members_get", map[string]any{"principal_id": other.PrincipalID}, owner.Token)["isError"] != true {
		t.Fatal("cross-organization principal exposed")
	}
	if data(call("platform_members_get", map[string]any{"principal_id": pid}, owner.Token))["principal_id"] != pid {
		t.Fatal("get target changed")
	}
	if call("platform_members_list", map[string]any{"limit": 1}, owner.Token)["isError"] == true {
		t.Fatal("directory unavailable")
	}
	token, _, e := s.IssueMemberToken(ctx, owner.OrganizationID, pid, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	if data(call("platform_members_capabilities", map[string]any{}, token))["can_create_members"] != false {
		t.Fatal("member acquired management capability")
	}
	if call("platform_members_list", map[string]any{}, token)["isError"] != true {
		t.Fatal("member directory permission bypass")
	}
	member = data(call("platform_members_update", map[string]any{"principal_id": pid, "idempotency_key": "mcp-member-disable", "input": map[string]any{"expected_version": member["version"], "display_name": "Controlled member", "role": "viewer", "active": false, "reason": "Exact false over MCP"}}, owner.Token))
	if member["active"] != false || member["role"] != "viewer" {
		t.Fatal("explicit false lost")
	}
	if call("platform_membership_audit_list", map[string]any{"principal_id": pid, "limit": 100}, owner.Token)["isError"] == true {
		t.Fatal("filtered audit unavailable")
	}
	if call("platform_members_revoke_credentials", map[string]any{"principal_id": pid, "idempotency_key": "mcp-member-revoke", "input": map[string]any{"expected_version": member["version"], "reason": "Credential-only revocation"}}, owner.Token)["isError"] == true {
		t.Fatal("revoke route unavailable")
	}
	if _, e = s.Pool.Exec(ctx, "UPDATE memberships SET role='viewer' WHERE organization_id=$1 AND principal_id=$2", owner.OrganizationID, owner.PrincipalID); e != nil {
		t.Fatal(e)
	}
	if call("platform_members_create", create, owner.Token)["isError"] != true {
		t.Fatal("cached command bypassed current role")
	}
}
