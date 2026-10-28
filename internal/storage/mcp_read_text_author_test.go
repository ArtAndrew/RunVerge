// SPDX-License-Identifier: Elastic-2.0
package storage

// Author integration tests use real isolated PostgreSQL schemas and the pinned
// upstream MCP SDK's HTTP server. Model candidates remain synthetic fixtures.
import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func authorMCPReadReady(t *testing.T, peer *httptest.Server, charge int64) (*independentToolFixture, *domain.ToolLease, domain.ToolConnection) {
	t.Helper()
	f := independentToolSetup(t, false)
	c := f.tool
	c.Tool.Version++
	c.Tool.Effect = "read"
	c.Tool.MaxChargeMicroUSD = charge
	c.Transport = "mcp_https"
	c.ResultContract = domain.ToolResultContractMCPReadTextV1
	c.RemoteTool = "read_document"
	c.Endpoint = peer.URL
	c.CredentialEnvironment = ""
	c.TrustedCAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: peer.Certificate().Raw}))
	var err error
	f.tool, err = f.s.InstallToolConnection(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	v := f.prepare(t)
	v = f.decide(t, v, f.p, "approve")
	f.dispatch(t, v)
	l, pinned, err := f.s.ClaimToolExecution(context.Background(), "standard-read-worker", 30*time.Second)
	if err != nil || l == nil {
		t.Fatalf("missing MCP read lease: %v", err)
	}
	if pinned.ResultContract != c.ResultContract || pinned.Tool.Effect != "read" || pinned.Tool.MaxChargeMicroUSD != charge {
		t.Fatal("database lost registered read profile")
	}
	return f, l, pinned
}

func TestAuthorMCPReadSDKServerPersistsObservationWithoutAcceptanceOrSettlement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		charge  int64
		isError bool
	}{
		{"read", 120, false}, {"explicit_zero_tariff", 0, false}, {"observed_read_error", 120, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := mcp.NewServer(&mcp.Implementation{Name: "standard-read-integration", Version: "1"}, nil)
			server.AddTool(&mcp.Tool{Name: "read_document", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls.Add(1)
				if string(req.Params.Arguments) != "{}" {
					t.Error("custom envelope sent to standard MCP tool")
				}
				return &mcp.CallToolResult{IsError: tc.isError, Content: []mcp.Content{&mcp.TextContent{Text: "Read data, not task acceptance"}, &mcp.TextContent{Text: ""}}}, nil
			})
			// The pinned 2026-07-28 protocol requires stateless HTTP servers;
			// the SDK's historical default is stateful and rejects this version.
			peer := httptest.NewTLSServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: tc.charge == 0}))
			defer peer.Close()
			f, lease, profile := authorMCPReadReady(t, peer, tc.charge)
			ctx := context.Background()
			out, err := authorToolRuntime(t, f.s, profile).Execute(ctx, lease)
			want := "succeeded"
			if tc.isError {
				want = "rejected"
			}
			if err != nil || out.Status != want || calls.Load() != 1 {
				t.Fatalf("standard SDK read: %+v %v calls=%d", out, err, calls.Load())
			}
			// A new DB connection observes the durable receipt, not runtime memory.
			reopened, err := Open(ctx, f.dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			v, err := reopened.GetToolExecution(ctx, f.p, f.task.ID, lease.Execution.ID)
			if err != nil || v.Receipt == nil || v.Execution.Status != want || v.Task.Status != "waiting_input" || v.Task.CandidateID != lease.Execution.CandidateID || v.Task.CurrentRunID != lease.Execution.RunID {
				t.Fatalf("read changed source candidate or lost receipt: %+v %v", v, err)
			}
			var content string
			if err = reopened.Pool.QueryRow(ctx, "SELECT content FROM artifacts WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, v.Receipt.ArtifactID).Scan(&content); err != nil || content != `["Read data, not task acceptance",""]` || v.Receipt.ContentDigest != Hash([]byte(content)) {
				t.Fatal("lost lossless persisted read artifact", content, err)
			}
			var reservation, action string
			var held int64
			if err = reopened.Pool.QueryRow(ctx, "SELECT r.status,a.status,r.amount_microusd FROM reservations r JOIN actions a ON a.organization_id=r.organization_id AND a.id=r.action_id WHERE r.organization_id=$1 AND r.id=$2", f.p.OrganizationID, lease.Execution.ReservationID).Scan(&reservation, &action, &held); err != nil || reservation != "unknown" || action != "unknown" || held != tc.charge {
				t.Fatal("read observation settled money", reservation, action, held, err)
			}
			other, _ := identity(t, reopened, 1000)
			if _, err = reopened.GetToolExecution(ctx, other, f.task.ID, lease.Execution.ID); err == nil {
				t.Fatal("cross-tenant read receipt exposed")
			}
			if _, err = authorToolRuntime(t, reopened, profile).Execute(ctx, lease); err == nil || calls.Load() != 1 {
				t.Fatal("persisted read resent")
			}
		})
	}
}

func TestAuthorMCPReadLostReplyRemainsDurablyUnknown(t *testing.T) {
	var calls atomic.Int32
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			t.Error("invalid RPC request")
			return
		}
		if req.Method == "tools/call" {
			calls.Add(1)
			c, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			c.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"resultType": "complete", "supportedVersions": []string{"2026-07-28"}, "capabilities": map[string]any{"tools": map[string]any{}}}})
	}))
	defer peer.Close()
	f, lease, profile := authorMCPReadReady(t, peer, 120)
	ctx := context.Background()
	out, err := authorToolRuntime(t, f.s, profile).Execute(ctx, lease)
	if err == nil || out.Status != "unknown" || calls.Load() != 1 {
		t.Fatal("lost reply did not remain unknown", out, err)
	}
	v, err := f.s.GetToolExecution(ctx, f.p, f.task.ID, lease.Execution.ID)
	if err != nil || v.Receipt != nil || v.Task.Status != "blocked" || v.Task.WaitingReason != "external_action_unknown" {
		t.Fatal("lost reply created receipt or unblocked task", err)
	}
	if _, err = f.s.ReconcileToolUnsent(ctx); err != nil {
		t.Fatal(err)
	}
	next, _, err := f.s.ClaimToolExecution(ctx, "new-worker", time.Second*30)
	if err != nil || next != nil {
		t.Fatal("unknown read reclaimed", err)
	}
	if _, err = authorToolRuntime(t, f.s, profile).Execute(ctx, lease); err == nil || calls.Load() != 1 {
		t.Fatal("unknown read resent")
	}
	f.balance(t, 270, 0)
}

func TestAuthorMCPReadRevokedMembershipCannotSendClaimedJob(t *testing.T) {
	var requests atomic.Int32
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer peer.Close()
	f, lease, profile := authorMCPReadReady(t, peer, 120)
	ctx := context.Background()
	if _, err := f.s.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, f.p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := authorToolRuntime(t, f.s, profile).Execute(ctx, lease); err == nil || requests.Load() != 0 {
		t.Fatal("revoked principal sent a previously claimed native read")
	}
	var sent bool
	if err := f.s.Pool.QueryRow(ctx, "SELECT dispatch_issued_at IS NOT NULL FROM actions WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, lease.Execution.ActionID).Scan(&sent); err != nil || sent {
		t.Fatal("revoked native read marked sent", err)
	}
}
