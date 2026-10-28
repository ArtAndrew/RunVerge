// SPDX-License-Identifier: Elastic-2.0
package mcpbridge_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/httpapi"
	"agentplatform.local/platform/internal/mcpbridge"
	"agentplatform.local/platform/internal/storage"
)

func TestSecurityMCPActualStorageScopeCeilingRevocationAndNoSecrets(t *testing.T) {
	s := pgStore(t)
	ctx := context.Background()
	owner, err := s.Bootstrap(ctx, "Security MCP fixture", 100000)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Bootstrap(ctx, "Other security MCP fixture", 100000)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: mcpbridge.HandlerBackend((&httpapi.Server{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()), AllowedMutations: allMutations})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewTLSServer(adapter.Handler())
	defer endpoint.Close()
	serial := 0
	secret := ""
	revoked := false
	call := func(name string, args map[string]any, token string) map[string]any {
		t.Helper()
		serial++
		_, out := rawHTTP(t, endpoint.Client(), endpoint.URL, wire(serial, "tools/call", map[string]any{"name": name, "arguments": args}), func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("X-Organization-ID", owner.OrganizationID)
		})
		if out["error"] != nil {
			code := out["error"].(map[string]any)["code"]
			if revoked && token == secret && name == "platform_identity" && code == float64(-32001) {
				return map[string]any{"isError": true}
			}
			t.Fatalf("unexpected MCP protocol error for %s (code=%v)", name, code)
		}
		if secret != "" {
			b, _ := json.Marshal(out)
			if strings.Contains(string(b), secret) || strings.Contains(string(b), storage.Hash([]byte(secret))) {
				t.Fatal("MCP exposed service credential material")
			}
		}
		return result(t, out)
	}
	data := func(v map[string]any) map[string]any {
		t.Helper()
		if v["isError"] == true {
			t.Fatal("unexpected security API failure")
		}
		return v["structuredContent"].(map[string]any)["data"].(map[string]any)
	}
	policy := data(call("platform_login_policy_get", map[string]any{}, owner.Token))
	if policy["can_require_oidc"] != false {
		t.Fatal("bearer/no-runtime presented as SSO ready")
	}
	policyInput := map[string]any{"expected_version": policy["version"], "mode": "mixed", "oidc_issuer": "", "service_access_enabled": true, "max_service_key_ttl_seconds": 1800, "reason": "Bound new integration key lifetime"}
	if data(call("platform_login_policy_update", map[string]any{"idempotency_key": "policy-lifetime", "input": policyInput}, owner.Token))["max_service_key_ttl_seconds"] != float64(1800) {
		t.Fatal("policy update lost")
	}
	create := map[string]any{"idempotency_key": "service-new", "input": map[string]any{"display_name": "Bounded integration", "role": "member", "reason": "Explicit MCP provisioning"}}
	account := data(call("platform_service_accounts_create", create, owner.Token))
	pid := account["principal_id"].(string)
	if account["principal_kind"] != "service" || data(call("platform_service_accounts_create", create, owner.Token))["principal_id"] != pid {
		t.Fatal("service kind/replay not preserved")
	}
	for _, name := range []string{"platform_service_accounts_list", "platform_security_audit_list"} {
		if call(name, map[string]any{"limit": 1}, owner.Token)["isError"] == true {
			t.Fatal("new directory unavailable")
		}
	}
	if data(call("platform_service_accounts_get", map[string]any{"principal_id": pid}, owner.Token))["principal_id"] != pid {
		t.Fatal("account target changed")
	}
	if call("platform_service_accounts_get", map[string]any{"principal_id": other.PrincipalID}, owner.Token)["isError"] != true {
		t.Fatal("cross organization principal exposed")
	}
	secret, meta, err := s.IssueServiceToken(ctx, owner.OrganizationID, pid, "viewer", time.Minute, "Private integration fixture")
	if err != nil {
		t.Fatal(err)
	}
	identity := data(call("platform_identity", map[string]any{}, secret))
	if identity["principal_kind"] != "service" || identity["role"] != "viewer" {
		t.Fatal("key ceiling was expanded")
	}
	if call("platform_service_accounts_list", map[string]any{}, secret)["isError"] != true {
		t.Fatal("service gained management authority")
	}
	if call("platform_agents_create", map[string]any{"idempotency_key": "viewer-write", "input": map[string]any{"name": "Forbidden", "instructions": "Denied by immutable ceiling"}}, secret)["isError"] != true {
		t.Fatal("viewer ceiling admitted a write")
	}
	if call("platform_service_keys_list", map[string]any{"principal_id": pid, "limit": 1}, owner.Token)["isError"] == true {
		t.Fatal("key list unavailable")
	}
	keyArgs := map[string]any{"principal_id": pid, "key_id": meta.ID}
	if data(call("platform_service_keys_get", keyArgs, owner.Token))["role_ceiling"] != "viewer" {
		t.Fatal("immutable key ceiling absent")
	}
	revokeArgs := map[string]any{"principal_id": pid, "key_id": meta.ID, "idempotency_key": "key-revoke", "input": map[string]any{"expected_version": meta.Version, "reason": "Stop new admissions"}}
	if data(call("platform_service_keys_revoke", revokeArgs, owner.Token))["revoked"] != true {
		t.Fatal("key revocation lost")
	}
	revoked = true
	if call("platform_identity", map[string]any{}, secret)["isError"] != true {
		t.Fatal("revoked key admitted a new request")
	}
	if data(call("platform_service_accounts_update", map[string]any{"principal_id": pid, "idempotency_key": "service-disable", "input": map[string]any{"expected_version": account["version"], "display_name": "Bounded integration", "role": "viewer", "active": false, "reason": "Revoke execution consent"}}, owner.Token))["active"] != false {
		t.Fatal("explicit false lost")
	}
	if call("platform_security_audit_list", map[string]any{"principal_id": pid, "limit": 100}, owner.Token)["isError"] == true {
		t.Fatal("filtered audit unavailable")
	}
	if _, err = s.Pool.Exec(ctx, "UPDATE memberships SET role='viewer' WHERE organization_id=$1 AND principal_id=$2", owner.OrganizationID, owner.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if call("platform_service_accounts_create", create, owner.Token)["isError"] != true {
		t.Fatal("cached creation bypassed current authority")
	}
}
