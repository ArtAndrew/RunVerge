// SPDX-License-Identifier: Elastic-2.0
package mcpbridge_test

// Independent raw-protocol tests: the test peer constructs bytes and HTTP
// metadata directly, without the official MCP client's codec or transport.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"agentplatform.local/platform/internal/mcpbridge"
)

func independentMCPFrame(id, method, params string) []byte {
	if params == "" {
		params = `{}`
	}
	var p map[string]json.RawMessage
	_ = json.Unmarshal([]byte(params), &p)
	if p == nil {
		p = map[string]json.RawMessage{}
	}
	p["_meta"] = json.RawMessage(`{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`)
	encoded, _ := json.Marshal(p)
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `","params":` + string(encoded) + `}`)
}

func independentMCPHTTP(t *testing.T, endpoint *httptest.Server, method, name string, body []byte, change func(*http.Request)) (int, map[string]json.RawMessage, []byte) {
	t.Helper()
	r, e := http.NewRequest(http.MethodPost, endpoint.URL, bytes.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2026-07-28")
	r.Header.Set("MCP-Method", method)
	if name != "" {
		r.Header.Set("MCP-Name", name)
	}
	r.Header.Set("Authorization", "Bearer independent-fixture-only")
	r.Header.Set("X-Organization-ID", "org_independent")
	if change != nil {
		change(r)
	}
	response, e := endpoint.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, 5<<20))
	if e != nil {
		t.Fatal(e)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("MCP response is cacheable")
	}
	var out map[string]json.RawMessage
	_ = json.Unmarshal(raw, &out)
	return response.StatusCode, out, raw
}

func independentMCPFailed(out map[string]json.RawMessage) bool {
	if len(out["error"]) > 0 {
		return true
	}
	var result struct {
		IsError bool `json:"isError"`
	}
	return json.Unmarshal(out["result"], &result) != nil || result.IsError
}

func TestIndependentMCPHTTPEnvelopeAndMetadataCannotDispatch(t *testing.T) {
	var business atomic.Int64
	backend := mcpbridge.BackendFunc(func(_ context.Context, c mcpbridge.Credentials, r mcpbridge.Request) (mcpbridge.Response, error) {
		if c.Token != "independent-fixture-only" || c.OrganizationID != "org_independent" {
			t.Error("transport changed credentials")
		}
		if r.Path != "/api/v1/me" {
			business.Add(1)
		}
		return mcpbridge.Response{Status: 200, Body: json.RawMessage(`{"data":{"ok":true}}`)}, nil
	})
	s, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: backend, AllowedMutations: []string{"platform_tasks_cancel"}})
	if e != nil {
		t.Fatal(e)
	}
	endpoint := httptest.NewTLSServer(s.Handler())
	defer endpoint.Close()
	valid := independentMCPFrame(`"bounded-request"`, "tools/call", `{"name":"platform_tasks_cancel","arguments":{"task_id":"tsk_fixture","idempotency_key":"same-command"}}`)
	for _, item := range []struct {
		name  string
		body  []byte
		alter func(*http.Request)
	}{
		{name: "missing_protocol", alter: func(r *http.Request) { r.Header.Del("MCP-Protocol-Version") }},
		{name: "missing_method", alter: func(r *http.Request) { r.Header.Del("MCP-Method") }},
		{name: "missing_tool_name", alter: func(r *http.Request) { r.Header.Del("MCP-Name") }},
		{name: "wrong_tool_name", alter: func(r *http.Request) { r.Header.Set("MCP-Name", "platform_tasks_get") }},
		{name: "wrong_method", alter: func(r *http.Request) { r.Header.Set("MCP-Method", "tools/list") }},
		{name: "duplicate_method", alter: func(r *http.Request) { r.Header.Add("MCP-Method", "tools/call") }},
		{name: "duplicate_name", alter: func(r *http.Request) { r.Header.Add("MCP-Name", "platform_tasks_cancel") }},
		{name: "duplicate_org", alter: func(r *http.Request) { r.Header.Add("X-Organization-ID", "org_independent") }},
		{name: "comma_bearer", alter: func(r *http.Request) { r.Header.Set("Authorization", "Bearer independent-fixture-only, Bearer other") }},
		{name: "cookie_present", alter: func(r *http.Request) { r.Header.Set("Cookie", "unrelated=value") }},
		{name: "null_origin", alter: func(r *http.Request) { r.Header.Set("Origin", "null") }},
		{name: "no_rpc_id", body: bytes.Replace(valid, []byte(`"id":"bounded-request",`), nil, 1)},
		{name: "null_rpc_id", body: bytes.Replace(valid, []byte(`"bounded-request"`), []byte(`null`), 1)},
		{name: "unsafe_numeric_id", body: bytes.Replace(valid, []byte(`"bounded-request"`), []byte(`9007199254740992`), 1)},
		{name: "batch", body: append(append([]byte{'['}, valid...), ']')},
		{name: "duplicate_argument", body: bytes.Replace(valid, []byte(`"task_id":"tsk_fixture"`), []byte(`"task_id":"tsk_fixture","task_id":"tsk_other"`), 1)},
		{name: "escaped_duplicate", body: bytes.Replace(valid, []byte(`"task_id":"tsk_fixture"`), []byte(`"task_id":"tsk_fixture","task_\u0069d":"tsk_other"`), 1)},
		{name: "null_argument", body: bytes.Replace(valid, []byte(`"tsk_fixture"`), []byte(`null`), 1)},
		{name: "invalid_utf8", body: bytes.Replace(valid, []byte(`tsk_fixture`), []byte{255}, 1)},
		{name: "unpaired_surrogate", body: bytes.Replace(valid, []byte(`tsk_fixture`), []byte(`\ud800`), 1)},
		{name: "duplicate_meta_version", body: bytes.Replace(valid, []byte(`"io.modelcontextprotocol/protocolVersion":"2026-07-28"`), []byte(`"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/protocolVersion":"2025-11-25"`), 1)},
		{name: "input_required_continuation", body: independentMCPFrame(`1`, "tools/call", `{"name":"platform_tasks_cancel","arguments":{"task_id":"tsk_fixture","idempotency_key":"same-command"},"requestState":"state-untrusted"}`)},
		{name: "path_escape", body: independentMCPFrame(`1`, "tools/call", `{"name":"platform_tasks_cancel","arguments":{"task_id":"../me","idempotency_key":"same-command"}}`)},
		{name: "missing_durable_key", body: independentMCPFrame(`1`, "tools/call", `{"name":"platform_tasks_cancel","arguments":{"task_id":"tsk_fixture"}}`)},
	} {
		t.Run(item.name, func(t *testing.T) {
			body := item.body
			if body == nil {
				body = valid
			}
			before := business.Load()
			status, out, _ := independentMCPHTTP(t, endpoint, "tools/call", "platform_tasks_cancel", body, item.alter)
			if status >= 200 && status < 300 && !independentMCPFailed(out) {
				t.Fatal("ambiguous or unsupported request reported success")
			}
			if business.Load() != before {
				t.Fatal("invalid frame dispatched a business request")
			}
		})
	}
	status, out, _ := independentMCPHTTP(t, endpoint, "tools/call", "platform_tasks_cancel", valid, nil)
	if status != 200 || independentMCPFailed(out) || business.Load() != 1 {
		t.Fatal("valid control request did not dispatch once")
	}
}

func TestIndependentMCPDiscoveryAndDisabledTools(t *testing.T) {
	var business atomic.Int64
	backend := mcpbridge.BackendFunc(func(_ context.Context, _ mcpbridge.Credentials, r mcpbridge.Request) (mcpbridge.Response, error) {
		if r.Path != "/api/v1/me" {
			business.Add(1)
		}
		return mcpbridge.Response{Status: 200, Body: json.RawMessage(`{"data":{}}`)}, nil
	})
	s, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: backend})
	if e != nil {
		t.Fatal(e)
	}
	endpoint := httptest.NewTLSServer(s.Handler())
	defer endpoint.Close()
	_, out, _ := independentMCPHTTP(t, endpoint, "server/discover", "", independentMCPFrame(`1`, "server/discover", ""), nil)
	var discovered struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
		Versions     []string                   `json:"supportedVersions"`
	}
	if json.Unmarshal(out["result"], &discovered) != nil || len(discovered.Capabilities) != 1 || discovered.Capabilities["tools"] == nil || len(discovered.Versions) != 1 || discovered.Versions[0] != "2026-07-28" {
		t.Fatal("capability profile is broader than implemented")
	}
	_, out, _ = independentMCPHTTP(t, endpoint, "tools/list", "", independentMCPFrame(`2`, "tools/list", ""), nil)
	var listed struct {
		Tools []struct {
			Name        string `json:"name"`
			Annotations struct {
				ReadOnly bool `json:"readOnlyHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if json.Unmarshal(out["result"], &listed) != nil || len(listed.Tools) != 51 {
		t.Fatal("readonly catalog differs")
	}
	for _, v := range listed.Tools {
		if !v.Annotations.ReadOnly {
			t.Fatal("default tool does not declare readonly")
		}
	}
	for _, name := range allMutations {
		_, out, _ := independentMCPHTTP(t, endpoint, "tools/call", name, independentMCPFrame(`3`, "tools/call", `{"name":"`+name+`","arguments":{}}`), nil)
		if !independentMCPFailed(out) {
			t.Fatal("disabled mutation callable")
		}
	}
	for _, method := range []string{"initialize", "resources/list", "resources/read", "prompts/list", "sampling/createMessage", "elicitation/create", "roots/list", "tasks/list", "subscriptions/listen"} {
		_, out, _ := independentMCPHTTP(t, endpoint, method, "", independentMCPFrame(`4`, method, `{}`), nil)
		if !independentMCPFailed(out) {
			t.Fatalf("unsupported %s accepted", method)
		}
	}
	if business.Load() != 0 {
		t.Fatal("discovery or unsupported tool performed business request")
	}
}

func TestIndependentMCPOutputBoundsAndErrorRedaction(t *testing.T) {
	for _, mode := range []string{"backend_error", "oversize", "invalid_json"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int64
			backend := mcpbridge.BackendFunc(func(_ context.Context, _ mcpbridge.Credentials, r mcpbridge.Request) (mcpbridge.Response, error) {
				if r.Path == "/api/v1/me" {
					return mcpbridge.Response{Status: 200, Body: json.RawMessage(`{"data":{}}`)}, nil
				}
				calls.Add(1)
				switch mode {
				case "backend_error":
					return mcpbridge.Response{}, errors.New("private-provider-key: forbidden-secret")
				case "oversize":
					return mcpbridge.Response{Status: 200, Body: json.RawMessage(`{"data":"` + strings.Repeat("x", mcpbridge.MaxMessageBytes) + `"}`)}, nil
				default:
					return mcpbridge.Response{Status: 200, Body: json.RawMessage(`broken-private-secret`)}, nil
				}
			})
			s, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: backend})
			if e != nil {
				t.Fatal(e)
			}
			endpoint := httptest.NewTLSServer(s.Handler())
			defer endpoint.Close()
			_, out, raw := independentMCPHTTP(t, endpoint, "tools/call", "platform_tasks_get", independentMCPFrame(`1`, "tools/call", `{"name":"platform_tasks_get","arguments":{"task_id":"tsk_fixture"}}`), nil)
			if !independentMCPFailed(out) || calls.Load() != 1 || bytes.Contains(raw, []byte("private")) || len(raw) > 8192 {
				t.Fatal("backend output leaked, retried or escaped response bound")
			}
		})
	}
}
