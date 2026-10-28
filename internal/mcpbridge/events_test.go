// SPDX-License-Identifier: Elastic-2.0
package mcpbridge_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/httpapi"
	"agentplatform.local/platform/internal/mcpbridge"
)

func TestMCPEventsUseVersionedScopedReplayWithoutCommands(t *testing.T) {
	s := pgStore(t)
	ctx := context.Background()
	b, err := s.Bootstrap(ctx, "MCP event fixture", 100000)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Authenticate(ctx, b.Token, b.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.CreateAgent(ctx, p, "events-agent", domain.CreateAgent{Name: "Events fixture", Instructions: "Synthetic only"})
	if err != nil {
		t.Fatal(err)
	}
	var a domain.Agent
	if json.Unmarshal(raw, &a) != nil {
		t.Fatal("agent")
	}
	if _, err = s.PublishAgent(ctx, p, a.ID, "events-publish", a.Version); err != nil {
		t.Fatal(err)
	}
	raw, err = s.CreateTask(ctx, p, "events-task", domain.CreateTask{AgentID: a.ID, Goal: "Observe durable replay", BudgetMicroUSD: 10000})
	if err != nil {
		t.Fatal(err)
	}
	var task domain.Task
	if json.Unmarshal(raw, &task) != nil {
		t.Fatal("task")
	}
	api := (&httpapi.Server{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	adapter, err := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: mcpbridge.HandlerBackend(api)})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(adapter.Handler())
	defer server.Close()
	serial := 0
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		serial++
		_, out := rawHTTP(t, server.Client(), server.URL, wire(serial, "tools/call", map[string]any{"name": name, "arguments": args}), func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+b.Token)
			r.Header.Set("X-Organization-ID", b.OrganizationID)
		})
		if out["error"] != nil {
			t.Fatalf("MCP call rejected %v", out["error"])
		}
		r := result(t, out)
		if r["isError"] == true {
			t.Fatalf("API read failed %v", r)
		}
		return r["structuredContent"].(map[string]any)
	}
	compat := call("platform_api_compatibility", map[string]any{})["data"].(map[string]any)
	if compat["api_version"] != domain.ContractVersion || compat["header_required"] != true {
		t.Fatal("compatibility bypassed versioned API")
	}
	snapshot := call("platform_event_snapshot", map[string]any{"task_id": task.ID})["data"].(map[string]any)
	floor := snapshot["replay_cursor"].(string)
	page := call("platform_event_feed", map[string]any{"task_id": task.ID, "cursor": floor, "limit": 1})["data"].(map[string]any)
	events := page["events"].([]any)
	if len(events) != 1 {
		t.Fatal("missing committed event")
	}
	e := events[0].(map[string]any)
	if _, ok := e["sequence"].(string); !ok || e["payload_version"] != float64(1) || e["data"] == nil {
		t.Fatal("typed event lost exact wire format")
	}
	old := call("platform_tasks_events", map[string]any{"task_id": task.ID, "after": "9223372036854775807"})["data"].([]any)
	if len(old) != 0 {
		t.Fatal("full int64 after coordinate changed")
	}
	serial++
	_, bad := rawHTTP(t, server.Client(), server.URL, wire(serial, "tools/call", map[string]any{"name": "platform_tasks_events", "arguments": map[string]any{"task_id": task.ID, "after": 1}}), func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+b.Token)
		r.Header.Set("X-Organization-ID", b.OrganizationID)
	})
	if bad["error"] == nil {
		t.Fatal("numeric after admitted by MCP schema")
	}
	var head int64
	if err = s.Pool.QueryRow(ctx, "SELECT max(sequence) FROM events WHERE organization_id=$1 AND task_id=$2", b.OrganizationID, task.ID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdvanceEventReplayWindow(ctx, b.OrganizationID, task.ID, head, "MCP replay fixture boundary"); err != nil {
		t.Fatal(err)
	}
	reset := call("platform_event_feed", map[string]any{"task_id": task.ID, "cursor": floor})["data"].(map[string]any)
	if reset["reset_required"] != true || reset["cursor"] != "" || len(reset["events"].([]any)) != 0 {
		t.Fatal("MCP silently skipped expired history")
	}
	fresh := call("platform_event_snapshot", map[string]any{"task_id": task.ID})["data"].(map[string]any)
	if fresh["window"].(map[string]any)["version"] != float64(2) {
		t.Fatal("replay window revision did not survive adapter")
	}
	var reserved, spent int64
	if err = s.Pool.QueryRow(ctx, "SELECT reserved_microusd,spent_microusd FROM organizations WHERE id=$1", b.OrganizationID).Scan(&reserved, &spent); err != nil || reserved != 0 || spent != 0 {
		t.Fatal("event reads changed finances", err)
	}
}

func TestMCPBackendRejectsUnselectedResponseRevision(t *testing.T) {
	for _, version := range []string{"", "0.10.0"} {
		b := mcpbridge.HandlerBackend(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(domain.APIContractHeader, version)
			_, _ = w.Write([]byte(`{"data":{}}`))
		}))
		if _, err := b.Call(context.Background(), credentials, mcpbridge.Request{Method: http.MethodGet, Path: "/api/v1/compatibility"}); err == nil {
			t.Fatal("incompatible API response was accepted")
		}
	}
}
