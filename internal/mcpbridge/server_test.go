// SPDX-License-Identifier: Elastic-2.0
package mcpbridge_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
)

var credentials = mcpbridge.Credentials{Token: "fixture-token", OrganizationID: "org_fixture"}
var allMutations = []string{"platform_login_policy_update", "platform_service_accounts_create", "platform_service_accounts_update", "platform_service_keys_revoke", "platform_members_create", "platform_members_update", "platform_members_revoke_credentials", "platform_agents_create", "platform_agents_publish", "platform_tasks_create", "platform_tasks_cancel", "platform_tasks_review", "platform_tasks_retry", "platform_materials_create", "platform_materials_revise", "platform_materials_publish", "platform_tool_executions_prepare", "platform_tool_executions_decide", "platform_tool_executions_dispatch", "platform_tool_executions_reconcile", "platform_model_continue", "platform_automation_create", "platform_automation_revoke"}

func init() {
	allMutations = append(allMutations, "platform_configuration_grants_create", "platform_configuration_grants_revoke", "platform_configuration_plans_create", "platform_configuration_plans_validate", "platform_configuration_plans_approve", "platform_configuration_plans_apply", "platform_configuration_plans_verify")
}

func wire(id any, method string, args map[string]any) []byte {
	if args == nil {
		args = map[string]any{}
	}
	args["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": mcpbridge.ProtocolVersion, "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": args})
	return b
}
func rawHTTP(t *testing.T, client *http.Client, endpoint string, body []byte, alter func(*http.Request)) (int, map[string]any) {
	t.Helper()
	r, e := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	var envelope struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &envelope)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", mcpbridge.ProtocolVersion)
	r.Header.Set("MCP-Method", envelope.Method)
	if envelope.Params.Name != "" {
		r.Header.Set("MCP-Name", envelope.Params.Name)
	}
	r.Header.Set("Authorization", "Bearer "+credentials.Token)
	r.Header.Set("X-Organization-ID", credentials.OrganizationID)
	if alter != nil {
		alter(r)
	}
	response, e := client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	data, e := io.ReadAll(io.LimitReader(response.Body, 5<<20))
	if e != nil {
		t.Fatal(e)
	}
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return response.StatusCode, out
}
func fakeBackend() mcpbridge.Backend {
	return mcpbridge.BackendFunc(func(ctx context.Context, c mcpbridge.Credentials, r mcpbridge.Request) (mcpbridge.Response, error) {
		if c != credentials {
			return mcpbridge.Response{Status: 401, Body: json.RawMessage(`{"error":{"code":"unauthorized"}}`)}, nil
		}
		return mcpbridge.Response{Status: 200, Body: json.RawMessage(`{"data":{"fixture":true}}`)}, nil
	})
}
func result(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	r, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing result: %v", out)
	}
	return r
}
func TestServerRawHTTPModernCatalogAndPolicies(t *testing.T) {
	s, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: fakeBackend(), AllowedOrigins: []string{"https://workspace.example"}})
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewTLSServer(s.Handler())
	defer server.Close()
	status, out := rawHTTP(t, server.Client(), server.URL, wire(1, "server/discover", nil), nil)
	if status != 200 {
		t.Fatalf("discovery status=%d: %v", status, out)
	}
	r := result(t, out)
	versions := r["supportedVersions"].([]any)
	caps := r["capabilities"].(map[string]any)
	if len(versions) != 1 || versions[0] != mcpbridge.ProtocolVersion || len(caps) != 1 || caps["tools"] == nil {
		t.Fatalf("overclaimed capabilities: %v", r)
	}
	_, out = rawHTTP(t, server.Client(), server.URL, wire(2, "tools/list", nil), nil)
	tools := result(t, out)["tools"].([]any)
	if len(tools) != 51 {
		t.Fatalf("read-only catalog=%d", len(tools))
	}
	for _, tool := range tools {
		b, _ := json.Marshal(tool)
		if bytes.Contains(b, []byte("platform_tasks_review")) {
			t.Fatal("mutation appeared without opt-in")
		}
	}
	_, out = rawHTTP(t, server.Client(), server.URL, wire(3, "tools/call", map[string]any{"name": "platform_tasks_get", "arguments": map[string]any{"task_id": "tsk_example"}}), nil)
	if result(t, out)["isError"] == true {
		t.Fatal("read failed")
	}
	_, out = rawHTTP(t, server.Client(), server.URL, wire(4, "tools/call", map[string]any{"name": "platform_tasks_create", "arguments": map[string]any{}}), nil)
	if out["error"] == nil {
		t.Fatal("disabled mutation accepted")
	}
	for _, tc := range []struct {
		name  string
		alter func(*http.Request)
		body  []byte
	}{
		{"wrong_origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, nil},
		{"cookie_auth", func(r *http.Request) { r.Header.Set("Cookie", "any=ambient") }, nil},

		{"duplicate_protocol", func(r *http.Request) { r.Header.Add("MCP-Protocol-Version", mcpbridge.ProtocolVersion) }, nil},
		{"lone_surrogate", nil, []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"arguments":{"x":"\ud800"}}}`)},
		{"tool_without_request_id", nil, []byte(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"platform_tasks_create","arguments":{"idempotency_key":"silent","input":{"goal":"silent write"}}}}`)},
		{"cancel_with_request_id", nil, []byte(`{"jsonrpc":"2.0","id":9,"method":"notifications/cancelled","params":{"requestId":1}}`)},
		{"case_shadow", nil, []byte(`{"jsonrpc":"2.0","id":7,"ID":8,"method":"tools/list","params":{}}`)},
		{"duplicate_bearer", func(r *http.Request) { r.Header.Add("Authorization", "Bearer another") }, nil},
		{"mismatched_header", func(r *http.Request) { r.Header.Set("MCP-Method", "tools/call") }, nil},
		{"wrong_version", func(r *http.Request) { r.Header.Set("MCP-Protocol-Version", "2025-11-25") }, nil},
		{"duplicate_json", nil, []byte(`{"jsonrpc":"2.0","id":1,"id":2,"method":"tools/list","params":{}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if body == nil {
				body = wire(5, "tools/list", nil)
			}
			status, out := rawHTTP(t, server.Client(), server.URL, body, tc.alter)
			if status == 200 && out["error"] == nil {
				t.Fatalf("invalid request accepted: %v", out)
			}
		})
	}
	status, out = rawHTTP(t, server.Client(), server.URL, []byte(`{"jsonrpc":"2.0","id":6,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"old","version":"1"},"capabilities":{}}}`), func(r *http.Request) { r.Header.Del("MCP-Protocol-Version") })
	if status == 200 && out["error"] == nil {
		t.Fatal("legacy initialized")
	}
}
func TestServerMutationsKeepDurableKeyAndRawCanonicalInput(t *testing.T) {
	var mu sync.Mutex
	var writes []mcpbridge.Request
	backend := mcpbridge.BackendFunc(func(_ context.Context, _ mcpbridge.Credentials, r mcpbridge.Request) (mcpbridge.Response, error) {
		if r.Method == http.MethodPost {
			mu.Lock()
			writes = append(writes, r)
			mu.Unlock()
		}
		return mcpbridge.Response{Status: 200, Body: json.RawMessage(`{"data":{"id":"same_task"}}`)}, nil
	})
	s, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: backend, AllowedMutations: allMutations})
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewTLSServer(s.Handler())
	defer server.Close()
	_, out := rawHTTP(t, server.Client(), server.URL, wire(1, "tools/list", nil), nil)
	if len(result(t, out)["tools"].([]any)) != 81 {
		t.Fatal("full catalog not mapped")
	}
	args := map[string]any{"name": "platform_tasks_create", "arguments": map[string]any{"idempotency_key": "stable-key", "input": map[string]any{"goal": "Exact input", "agent_id": "agt_fixture", "budget_microusd": 1000}}}
	for _, id := range []any{17, "new-rpc-id"} {
		_, out = rawHTTP(t, server.Client(), server.URL, wire(id, "tools/call", args), nil)
		result(t, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(writes) != 2 || writes[0].IdempotencyKey != "stable-key" || writes[1].IdempotencyKey != writes[0].IdempotencyKey || !bytes.Equal(writes[0].Body, writes[1].Body) {
		t.Fatal("RPC id replaced durable key or body changed")
	}
}
func TestServerStdioWireReauthCancellationAndBounds(t *testing.T) {
	var mu sync.Mutex
	authorized := true
	backend := mcpbridge.BackendFunc(func(ctx context.Context, _ mcpbridge.Credentials, r mcpbridge.Request) (mcpbridge.Response, error) {
		mu.Lock()
		ok := authorized
		mu.Unlock()
		if !ok {
			return mcpbridge.Response{Status: 401}, nil
		}
		if r.Path == "/api/v1/tasks/tsk_slow" {
			<-ctx.Done()
			return mcpbridge.Response{}, ctx.Err()
		}
		return mcpbridge.Response{Status: 200, Body: json.RawMessage(`{"data":[]}`)}, nil
	})
	s, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: backend, RequestTimeout: 200 * time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	server, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.ServeIO(ctx, credentials, server, server) }()
	reader := bufio.NewReader(client)
	send := func(id int, method string, args map[string]any) map[string]any {
		t.Helper()
		client.SetDeadline(time.Now().Add(2 * time.Second))
		b := append(wire(id, method, args), '\n')
		if _, e = client.Write(b); e != nil {
			t.Fatal(e)
		}
		line, e := reader.ReadBytes('\n')
		if e != nil {
			t.Fatal(e)
		}
		var out map[string]any
		if json.Unmarshal(line, &out) != nil {
			t.Fatal("non JSON stdio output")
		}
		return out
	}
	result(t, send(1, "server/discover", nil))
	out := send(2, "tools/call", map[string]any{"name": "platform_tasks_get", "arguments": map[string]any{"task_id": "tsk_slow"}})
	if result(t, out)["isError"] != true {
		t.Fatal("deadline not bounded")
	}
	mu.Lock()
	authorized = false
	mu.Unlock()
	out = send(3, "tools/list", nil)
	if out["error"] == nil {
		t.Fatal("stdio cached authority after revocation")
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stdio EOF did not close")
	}
}
func TestHandlerBackendCannotEscapeAPIOrExceedBounds(t *testing.T) {
	var called int
	b := mcpbridge.HandlerBackend(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.URL.Path != "/api/v1/tasks" || r.Header.Get("Authorization") != "Bearer "+credentials.Token || r.Header.Get("Cookie") != "" || r.Header.Get(domain.APIContractHeader) != domain.ContractVersion {
			t.Error("wrong internal request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(domain.APIContractHeader, domain.ContractVersion)
		w.Write([]byte(`{"data":[]}`))
	}))
	for _, path := range []string{"https://evil.example/", "/api/v1/tasks/../secrets", "/api/v1/account-cost", "/auth/session", "/api/v1/tasks/tsk_x/secret"} {
		if _, e := b.Call(context.Background(), credentials, mcpbridge.Request{Method: http.MethodGet, Path: path}); e == nil {
			t.Fatalf("escaped allowed routes %s", path)
		}
	}
	if called != 0 {
		t.Fatal("disallowed backend invoked")
	}
	if _, e := b.Call(context.Background(), credentials, mcpbridge.Request{Method: http.MethodGet, Path: "/api/v1/tasks"}); e != nil {
		t.Fatal(e)
	}
	huge := mcpbridge.HandlerBackend(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(strings.Repeat("x", mcpbridge.MaxMessageBytes+1)))
	}))
	if _, e := huge.Call(context.Background(), credentials, mcpbridge.Request{Method: http.MethodGet, Path: "/api/v1/tasks"}); e == nil {
		t.Fatal("unbounded response accepted")
	}
}

func TestServerHTTPDisconnectCancelsOnlyRequestAndFreesCapacity(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	var once sync.Once
	backend := mcpbridge.BackendFunc(func(ctx context.Context, _ mcpbridge.Credentials, r mcpbridge.Request) (mcpbridge.Response, error) {
		if r.Path == "/api/v1/tasks/tsk_wait" {
			once.Do(func() { close(started) })
			<-ctx.Done()
			close(cancelled)
			return mcpbridge.Response{}, ctx.Err()
		}
		if strings.HasSuffix(r.Path, "/cancel") {
			t.Error("transport cancellation became task cancellation")
		}
		return mcpbridge.Response{Status: 200, Body: json.RawMessage(`{"data":[]}`)}, nil
	})
	adapter, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: backend, MaxConcurrent: 1, RequestTimeout: 5 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewTLSServer(adapter.Handler())
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := wire(10, "tools/call", map[string]any{"name": "platform_tasks_get", "arguments": map[string]any{"task_id": "tsk_wait"}})
	req, e := http.NewRequestWithContext(ctx, "POST", server.URL, bytes.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", mcpbridge.ProtocolVersion)
	req.Header.Set("MCP-Method", "tools/call")
	req.Header.Set("MCP-Name", "platform_tasks_get")
	req.Header.Set("Authorization", "Bearer "+credentials.Token)
	req.Header.Set("X-Organization-ID", credentials.OrganizationID)
	done := make(chan struct{})
	go func() {
		response, _ := server.Client().Do(req)
		if response != nil {
			response.Body.Close()
		}
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request never entered backend")
	}
	_, out := rawHTTP(t, server.Client(), server.URL, wire(11, "tools/list", nil), nil)
	if out["error"] == nil {
		t.Fatal("server concurrency unbounded")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel handler")
	}
	<-done
	// Cancellation has completed before another request can use the same slot.
	deadline := time.Now().Add(time.Second)
	for {
		_, out = rawHTTP(t, server.Client(), server.URL, wire(12, "tools/list", nil), nil)
		if out["result"] != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelled request leaked capacity")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestServerStdioCancellationAndMalformedFrames(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		started, ended := make(chan struct{}), make(chan struct{})
		adapter, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: mcpbridge.BackendFunc(func(ctx context.Context, _ mcpbridge.Credentials, r mcpbridge.Request) (mcpbridge.Response, error) {
			if r.Path == "/api/v1/tasks/tsk_wait" {
				close(started)
				<-ctx.Done()
				close(ended)
				return mcpbridge.Response{}, ctx.Err()
			}
			return mcpbridge.Response{Status: 200, Body: json.RawMessage(`{"data":[]}`)}, nil
		}), RequestTimeout: 5 * time.Second})
		if e != nil {
			t.Fatal(e)
		}
		server, client := net.Pipe()
		defer client.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- adapter.ServeIO(ctx, credentials, server, server) }()
		client.SetDeadline(time.Now().Add(2 * time.Second))
		request := append(wire(21, "tools/call", map[string]any{"name": "platform_tasks_get", "arguments": map[string]any{"task_id": "tsk_wait"}}), '\n')
		if _, e = client.Write(request); e != nil {
			t.Fatal(e)
		}
		<-started
		if _, e = client.Write([]byte("{\"jsonrpc\":\"2.0\",\"method\":\"notifications/cancelled\",\"params\":{\"requestId\":21}}\n")); e != nil {
			t.Fatal(e)
		}
		select {
		case <-ended:
		case <-time.After(time.Second):
			t.Fatal("stdio cancellation did not stop handler")
		}
		if _, e = client.Write(append(wire(22, "tools/list", nil), '\n')); e != nil {
			t.Fatal(e)
		}
		line, e := bufio.NewReader(client).ReadBytes('\n')
		if e != nil {
			t.Fatal(e)
		}
		var response map[string]any
		json.Unmarshal(line, &response)
		if response["id"] != float64(22) {
			t.Fatalf("cancelled request emitted a late response: %s", line)
		}
		client.Close()
		<-done
	})
	for _, line := range []string{`{"jsonrpc":"2.0","id":1,"id":2,"method":"tools/list","params":{}}`, strings.Repeat("x", mcpbridge.MaxMessageBytes+1)} {
		t.Run("malformed", func(t *testing.T) {
			adapter, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: fakeBackend()})
			if e != nil {
				t.Fatal(e)
			}
			server, client := net.Pipe()
			defer client.Close()
			done := make(chan error, 1)
			go func() { done <- adapter.ServeIO(context.Background(), credentials, server, server) }()
			client.SetDeadline(time.Now().Add(2 * time.Second))
			_, _ = client.Write([]byte(line + "\n"))
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("invalid frame did not close bounded stream")
			}
		})
	}
}

func TestServerStdioCancelledGenerationDoesNotSuppressReusedID(t *testing.T) {
	firstStarted, firstCancelled, releaseFirst := make(chan struct{}), make(chan struct{}), make(chan struct{})
	secondStarted, releaseSecond := make(chan struct{}), make(chan struct{})
	adapter, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: mcpbridge.BackendFunc(func(ctx context.Context, _ mcpbridge.Credentials, r mcpbridge.Request) (mcpbridge.Response, error) {
		switch r.Path {
		case "/api/v1/tasks/tsk_first":
			close(firstStarted)
			<-ctx.Done()
			close(firstCancelled)
			<-releaseFirst
		case "/api/v1/tasks/tsk_second":
			close(secondStarted)
			<-releaseSecond
		}
		return mcpbridge.Response{Status: 200, Body: json.RawMessage(`{"data":{"path":"` + r.Path + `"}}`)}, nil
	}), RequestTimeout: 2 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	server, client := net.Pipe()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- adapter.ServeIO(ctx, credentials, server, server) }()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(client)
	send := func(id int, path string) {
		t.Helper()
		if _, err := client.Write(append(wire(id, "tools/call", map[string]any{"name": "platform_tasks_get", "arguments": map[string]any{"task_id": path}}), '\n')); err != nil {
			t.Fatal(err)
		}
	}
	send(31, "tsk_first")
	<-firstStarted
	if _, e = client.Write([]byte("{\"jsonrpc\":\"2.0\",\"method\":\"notifications/cancelled\",\"params\":{\"requestId\":31}}\n")); e != nil {
		t.Fatal(e)
	}
	<-firstCancelled
	send(31, "tsk_second")
	<-secondStarted
	close(releaseFirst)
	if _, e = client.Write(append(wire(32, "tools/list", nil), '\n')); e != nil {
		t.Fatal(e)
	}
	line, e := reader.ReadBytes('\n')
	if e != nil {
		t.Fatal(e)
	}
	var out map[string]any
	json.Unmarshal(line, &out)
	if out["id"] != float64(32) {
		t.Fatalf("old generation responded: %s", line)
	}
	close(releaseSecond)
	line, e = reader.ReadBytes('\n')
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(line, []byte("tsk_second")) {
		t.Fatalf("new generation lost or replaced: %s", line)
	}
	json.Unmarshal(line, &out)
	if out["id"] != float64(31) {
		t.Fatal("external ID not restored")
	}
	client.Close()
	<-done
}

func TestServerStdioBlockedOutputHasDeadline(t *testing.T) {
	adapter, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: fakeBackend(), RequestTimeout: 80 * time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() { done <- adapter.ServeIO(context.Background(), credentials, server, server) }()
	client.SetWriteDeadline(time.Now().Add(time.Second))
	if _, e = client.Write(append(wire(41, "tools/list", nil), '\n')); e != nil {
		t.Fatal(e)
	}
	// Peer deliberately never reads. Closing the transport must unblock both the
	// output write and the independent input read, without relying on peer EOF.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("blocked stdout exceeded bounded deadline")
	}
}
