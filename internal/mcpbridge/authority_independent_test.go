// SPDX-License-Identifier: Elastic-2.0
package mcpbridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/httpapi"
	"agentplatform.local/platform/internal/mcpbridge"
	"agentplatform.local/platform/internal/storage"
)

func TestIndependentMCPRealPGLostAcknowledgementAndCurrentReplayAuthority(t *testing.T) {
	s := pgStore(t)
	ctx := context.Background()
	b, e := s.Bootstrap(ctx, "Independent MCP owner", 10000)
	if e != nil {
		t.Fatal(e)
	}
	foreign, e := s.Bootstrap(ctx, "Independent MCP foreign", 10000)
	if e != nil {
		t.Fatal(e)
	}
	p, e := s.Authenticate(ctx, b.Token, b.OrganizationID)
	if e != nil {
		t.Fatal(e)
	}
	agentBudget := int64(1000)
	aRaw, e := s.CreateAgent(ctx, p, domain.NewID("key"), domain.CreateAgent{Name: "MCP independent fixture", Instructions: "Synthetic fixture only", BudgetMicroUSD: &agentBudget})
	if e != nil {
		t.Fatal(e)
	}
	var a domain.Agent
	if json.Unmarshal(aRaw, &a) != nil {
		t.Fatal("invalid fixture agent")
	}
	if _, e = s.PublishAgent(ctx, p, a.ID, domain.NewID("key"), a.Version); e != nil {
		t.Fatal(e)
	}
	taskRaw, e := s.CreateTask(ctx, p, domain.NewID("key"), domain.CreateTask{AgentID: a.ID, Goal: "Independent MCP draft scope", BudgetMicroUSD: 1000})
	if e != nil {
		t.Fatal(e)
	}
	var task domain.Task
	if json.Unmarshal(taskRaw, &task) != nil {
		t.Fatal("invalid fixture task")
	}
	real := mcpbridge.HandlerBackend((&httpapi.Server{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler())
	var lose atomic.Bool
	lose.Store(true)
	var actualWrites atomic.Int64
	var revokeAfterProbe atomic.Bool
	backend := mcpbridge.BackendFunc(func(ctx context.Context, c mcpbridge.Credentials, r mcpbridge.Request) (mcpbridge.Response, error) {
		out, e := real.Call(ctx, c, r)
		if r.Path == "/api/v1/me" && revokeAfterProbe.CompareAndSwap(true, false) {
			if _, err := s.Pool.Exec(ctx, "UPDATE access_tokens SET revoked=true WHERE token_hash=$1", storage.Hash([]byte(b.Token))); err != nil {
				return mcpbridge.Response{}, err
			}
		}
		if r.Method == http.MethodPost {
			actualWrites.Add(1)
			if e == nil && out.Status == 201 && lose.CompareAndSwap(true, false) {
				return mcpbridge.Response{}, errors.New("independent acknowledgement loss after durable commit")
			}
		}
		return out, e
	})
	adapter, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: backend, AllowedMutations: []string{"platform_materials_create"}})
	if e != nil {
		t.Fatal(e)
	}
	endpoint := httptest.NewTLSServer(adapter.Handler())
	defer endpoint.Close()
	call := func(id, name, arguments, token, org string) map[string]json.RawMessage {
		t.Helper()
		_, out, _ := independentMCPHTTP(t, endpoint, "tools/call", name, independentMCPFrame(id, "tools/call", `{"name":"`+name+`","arguments":`+arguments+`}`), func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("X-Organization-ID", org)
		})
		return out
	}
	args := `{"task_id":"` + task.ID + `","idempotency_key":"independent-durable-command","input":{"name":"Private local draft","media_type":"text/plain","content":"exact private bytes"}}`
	if !independentMCPFailed(call(`1`, "platform_materials_create", args, b.Token, b.OrganizationID)) {
		t.Fatal("lost acknowledgement reported success")
	}
	if actualWrites.Load() != 1 {
		t.Fatal("adapter automatically retried after unknown outcome")
	}
	var count int
	if e = s.Pool.QueryRow(ctx, "SELECT count(*) FROM materials WHERE organization_id=$1", b.OrganizationID).Scan(&count); e != nil || count != 1 {
		t.Fatal("lost acknowledgement did not preserve exactly one committed draft")
	}
	out := call(`"different-wire-id"`, "platform_materials_create", args, b.Token, b.OrganizationID)
	if independentMCPFailed(out) {
		t.Fatal("explicit durable replay failed")
	}
	var receipt struct {
		Structured struct {
			Data domain.Material `json:"data"`
		} `json:"structuredContent"`
	}
	if json.Unmarshal(out["result"], &receipt) != nil || receipt.Structured.Data.ID == "" {
		t.Fatal("missing replay receipt")
	}
	m := receipt.Structured.Data
	if e = s.Pool.QueryRow(ctx, "SELECT count(*) FROM materials WHERE organization_id=$1", b.OrganizationID).Scan(&count); e != nil || count != 1 {
		t.Fatal("RPC ID change bypassed durable idempotency")
	}
	read := `{"task_id":"` + task.ID + `","material_id":"` + m.ID + `","version":1}`
	if !independentMCPFailed(call(`3`, "platform_materials_version", read, foreign.Token, foreign.OrganizationID)) {
		t.Fatal("foreign tenant read draft")
	}
	if _, e = s.Pool.Exec(ctx, "UPDATE memberships SET role='viewer' WHERE organization_id=$1 AND principal_id=$2", b.OrganizationID, b.PrincipalID); e != nil {
		t.Fatal(e)
	}
	if !independentMCPFailed(call(`4`, "platform_materials_create", args, b.Token, b.OrganizationID)) {
		t.Fatal("cached receipt bypassed current downgraded permissions")
	}
	if independentMCPFailed(call(`5`, "platform_materials_version", read, b.Token, b.OrganizationID)) {
		t.Fatal("authorized viewer cannot read")
	}
	// The probe succeeds, then credential revocation commits before the actual
	// tool request. The ordinary route must independently reject its read.
	revokeAfterProbe.Store(true)
	if !independentMCPFailed(call(`6`, "platform_materials_version", read, b.Token, b.OrganizationID)) {
		t.Fatal("probe authentication was reused after revocation")
	}
	_, out, _ = independentMCPHTTP(t, endpoint, "tools/list", "", independentMCPFrame(`7`, "tools/list", `{}`), func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+b.Token)
		r.Header.Set("X-Organization-ID", b.OrganizationID)
	})
	if !independentMCPFailed(out) {
		t.Fatal("revoked credentials retain discovery")
	}
	var reserved, spent int64
	if e = s.Pool.QueryRow(ctx, "SELECT reserved_microusd,spent_microusd FROM organizations WHERE id=$1", b.OrganizationID).Scan(&reserved, &spent); e != nil || reserved != 0 || spent != 0 {
		t.Fatal("draft/transport retry changed money")
	}
}
