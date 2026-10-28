// SPDX-License-Identifier: Elastic-2.0
package mcpbridge_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/httpapi"
	"agentplatform.local/platform/internal/mcpbridge"
	"agentplatform.local/platform/internal/storage"
	"github.com/jackc/pgx/v5"
)

func pgStore(t *testing.T) *storage.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for real PostgreSQL MCP bridge test")
	}
	ctx := context.Background()
	base, e := storage.Open(ctx, dsn)
	if e != nil {
		t.Fatal("test database unavailable")
	}
	schema := domain.NewID("mcp_test")
	if _, e = base.Pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); e != nil {
		base.Close()
		t.Fatal(e)
	}
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal("invalid test database configuration")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, e := storage.Open(ctx, u.String())
	if e != nil {
		t.Fatal("isolated test database unavailable")
	}
	t.Cleanup(func() {
		s.Close()
		_, e := base.Pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		base.Close()
		if e != nil {
			t.Error(e)
		}
	})
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	return s
}
func TestMCPRealPGUsesAPIIdempotencyAuthorityAndTenantBoundaries(t *testing.T) {
	s := pgStore(t)
	ctx := context.Background()
	owner, e := s.Bootstrap(ctx, "MCP local fixture", 100000)
	if e != nil {
		t.Fatal(e)
	}
	other, e := s.Bootstrap(ctx, "Different MCP fixture", 100000)
	if e != nil {
		t.Fatal(e)
	}
	api := (&httpapi.Server{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	real := mcpbridge.HandlerBackend(api)
	var mu sync.Mutex
	lost := false
	backend := mcpbridge.BackendFunc(func(ctx context.Context, c mcpbridge.Credentials, r mcpbridge.Request) (mcpbridge.Response, error) {
		response, e := real.Call(ctx, c, r)
		if r.Method == http.MethodPost && r.Path == "/api/v1/tasks" && e == nil && response.Status < 300 {
			mu.Lock()
			first := !lost
			lost = true
			mu.Unlock()
			if first {
				return mcpbridge.Response{}, errors.New("injected acknowledgement loss after API commit")
			}
		}
		return response, e
	})
	adapter, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: backend, AllowedMutations: allMutations})
	if e != nil {
		t.Fatal(e)
	}
	endpoint := httptest.NewTLSServer(adapter.Handler())
	defer endpoint.Close()
	call := func(id int, name string, args map[string]any, token, org string) map[string]any {
		t.Helper()
		_, out := rawHTTP(t, endpoint.Client(), endpoint.URL, wire(id, "tools/call", map[string]any{"name": name, "arguments": args}), func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("X-Organization-ID", org)
		})
		return result(t, out)
	}
	data := func(r map[string]any) map[string]any {
		t.Helper()
		if r["isError"] == true {
			t.Fatalf("tool failed: %v", r)
		}
		body, ok := r["structuredContent"].(map[string]any)
		if !ok {
			t.Fatal("missing structured API envelope")
		}
		d, ok := body["data"].(map[string]any)
		if !ok {
			t.Fatal("missing API data")
		}
		return d
	}
	current := data(call(0, "platform_identity", map[string]any{}, owner.Token, owner.OrganizationID))
	if current["organization_id"] != owner.OrganizationID || current["principal_id"] == nil || current["permissions"] == nil {
		t.Fatal("current identity did not traverse ordinary API")
	}
	a := data(call(1, "platform_agents_create", map[string]any{"idempotency_key": "agent-create", "input": map[string]any{"name": "MCP fixture", "instructions": "Synthetic echo only", "model": "fixture:echo", "budget_microusd": 10000}}, owner.Token, owner.OrganizationID))
	agentID := a["id"].(string)
	data(call(2, "platform_agents_publish", map[string]any{"agent_id": agentID, "idempotency_key": "agent-publish", "input": map[string]any{"expected_version": int(a["version"].(float64))}}, owner.Token, owner.OrganizationID))
	args := map[string]any{"idempotency_key": "durable-task-create", "input": map[string]any{"goal": "One task across a lost acknowledgement", "agent_id": agentID, "budget_microusd": 10000}}
	if call(3, "platform_tasks_create", args, owner.Token, owner.OrganizationID)["isError"] != true {
		t.Fatal("injected outcome loss was hidden")
	}
	task := data(call(99, "platform_tasks_create", args, owner.Token, owner.OrganizationID))
	id := task["id"].(string)
	var count int
	if e = s.Pool.QueryRow(ctx, "SELECT count(*) FROM tasks WHERE organization_id=$1", owner.OrganizationID).Scan(&count); e != nil || count != 1 {
		t.Fatal("lost acknowledgement created a second task")
	}
	data(call(100, "platform_tasks_get", map[string]any{"task_id": id}, owner.Token, owner.OrganizationID))
	if call(101, "platform_tasks_get", map[string]any{"task_id": id}, other.Token, other.OrganizationID)["isError"] != true {
		t.Fatal("cross organization read succeeded")
	}
	bad := map[string]any{"idempotency_key": "canonical-input", "input": map[string]any{"Goal": "noncanonical", "agent_id": agentID, "budget_microusd": 10000}}
	if call(102, "platform_tasks_create", bad, owner.Token, owner.OrganizationID)["isError"] != true {
		t.Fatal("adapter stripped invalid API fields")
	}

	// Material drafts use the same API transactions and exact immutable revisions.
	material := data(call(110, "platform_materials_create", map[string]any{"task_id": id, "idempotency_key": "material-create", "input": map[string]any{"name": "MCP collaboration draft", "media_type": "text/plain", "content": "original bytes: Привет 🌍"}}, owner.Token, owner.OrganizationID))
	materialID := material["id"].(string)
	data(call(111, "platform_materials_get", map[string]any{"task_id": id, "material_id": materialID}, owner.Token, owner.OrganizationID))
	data(call(112, "platform_materials_revise", map[string]any{"task_id": id, "material_id": materialID, "idempotency_key": "material-revise", "input": map[string]any{"expected_version": 1, "reason": "Explicit correction", "content": "revised bytes"}}, owner.Token, owner.OrganizationID))
	original := data(call(113, "platform_materials_version", map[string]any{"task_id": id, "material_id": materialID, "version": 1}, owner.Token, owner.OrganizationID))
	if original["content"] != "original bytes: Привет 🌍" {
		t.Fatal("exact material content changed")
	}
	if call(114, "platform_materials_revise", map[string]any{"task_id": id, "material_id": materialID, "idempotency_key": "conflicting-revision", "input": map[string]any{"expected_version": 1, "reason": "Stale revision", "content": "must not overwrite"}}, owner.Token, owner.OrganizationID)["isError"] != true {
		t.Fatal("MCP bypassed expected-version conflict")
	}
	if call(115, "platform_materials_version", map[string]any{"task_id": id, "material_id": materialID, "version": 1}, other.Token, other.OrganizationID)["isError"] != true {
		t.Fatal("MCP disclosed foreign material content")
	}
	for _, name := range []string{"platform_materials_list", "platform_materials_revisions"} {
		args := map[string]any{"task_id": id, "limit": 1}
		if name == "platform_materials_revisions" {
			args["material_id"] = materialID
		}
		if call(116, name, args, owner.Token, owner.OrganizationID)["isError"] == true {
			t.Fatal("material pagination failed")
		}
	}
	if call(117, "platform_materials_publish", map[string]any{"task_id": id, "material_id": materialID, "idempotency_key": "not-a-result", "input": map[string]any{"expected_task_version": int(task["version"].(float64)), "expected_material_version": 2, "content_digest": storage.Hash([]byte("revised bytes")), "parent_candidate_id": "cand_nonexistent"}}, owner.Token, owner.OrganizationID)["isError"] != true {
		t.Fatal("draft alone became published candidate")
	}
	cancelled := data(call(103, "platform_tasks_cancel", map[string]any{"task_id": id, "idempotency_key": "cancel-once"}, owner.Token, owner.OrganizationID))
	if cancelled["status"] != "cancelled" {
		t.Fatal("normal cascade cancellation not applied")
	}
	if _, e = s.Pool.Exec(ctx, "UPDATE access_tokens SET revoked=true WHERE token_hash=$1", storage.Hash([]byte(owner.Token))); e != nil {
		t.Fatal(e)
	}
	_, out := rawHTTP(t, endpoint.Client(), endpoint.URL, wire(104, "tools/list", nil), func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+owner.Token)
		r.Header.Set("X-Organization-ID", owner.OrganizationID)
	})
	if out["error"] == nil {
		t.Fatal("MCP kept revoked authority for discovery")
	}
}
