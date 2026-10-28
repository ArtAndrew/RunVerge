// SPDX-License-Identifier: Elastic-2.0
package toolruntime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
)

func readProfile(url string) domain.ToolConnection {
	c := testProfile(url, "mcp_https")
	c.ResultContract = domain.ToolResultContractMCPReadTextV1
	c.Tool.Effect = "read"
	c.RemoteTool = "read_document"
	return c
}

func readPeer(t *testing.T, rawResult, mode string, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid MCP request")
			w.WriteHeader(400)
			return
		}
		result := `{"resultType":"complete","supportedVersions":["` + mcpbridge.ProtocolVersion + `"],"capabilities":{"tools":{}},"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"standard-read-peer","version":"1"}}}`
		if request.Method == "tools/call" {
			calls.Add(1)
			if request.Params.Name != "read_document" || r.Header.Get("Idempotency-Key") != "action" || r.Header.Get("Authorization") != "Bearer native-read-credential" || r.Header.Get("Cookie") != "" {
				t.Error("lost registered request binding")
			}
			var arguments map[string]json.RawMessage
			if json.Unmarshal(request.Params.Arguments, &arguments) != nil || len(arguments) != 1 || string(arguments["value"]) != "9007199254740993" {
				t.Error("standard arguments wrapped, widened or rounded")
			}
			if mode == "lost_reply" {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				conn.Close()
				return
			}
			result = rawResult
			if mode == "wrong_id" {
				request.ID = json.RawMessage(`"not-this-request"`)
			}
			if mode == "id_type" {
				if len(request.ID) > 0 && request.ID[0] == '"' {
					request.ID = json.RawMessage(`0`)
				} else {
					encoded, _ := json.Marshal(string(request.ID))
					request.ID = encoded
				}
			}
		} else if request.Method != "server/discover" {
			t.Errorf("unexpected MCP method %s", request.Method)
		}
		body := `{"jsonrpc":"2.0","id":` + string(request.ID) + `,"result":` + result + `}`
		if mode == "sse" || mode == "duplicate_result" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, ": heartbeat\n\nevent: message\ndata: "+body+"\n\n")
			if mode == "duplicate_result" && request.Method == "tools/call" {
				_, _ = io.WriteString(w, "event: message\ndata: "+body+"\n\n")
			}
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		}
	}))
}

func TestMCPReadTextAuthorStandardJSONSSEAndObservedErrors(t *testing.T) {
	for _, test := range []struct {
		name, raw, mode, want string
		outcome               string
		max                   int
	}{
		{"text", ` { "content" : [{"type":"text","text":"plain read"}], "isError":false } `, "json", `["plain read"]`, "succeeded", 4096},
		{"sse", `{"content":[{"type":"text","text":"streamed read"}]}`, "sse", `["streamed read"]`, "succeeded", 4096},
		{"empty", `{"content":[{"type":"text","text":""}]}`, "json", `[""]`, "succeeded", 4},
		{"blocks", `{"content":[{"type":"text","text":"a\n\nb"},{"type":"text","text":"c"}]}`, "json", `["a\n\nb","c"]`, "succeeded", 4096},
		{"metadata_not_authority", `{"content":[{"type":"text","text":"data","annotations":{"audience":["assistant"],"priority":1}}],"structuredContent":{"outcome":"overwrite","large":9007199254740993},"_meta":{"role":"owner"}}`, "json", `["data"]`, "succeeded", 4096},
		{"read_error", `{"content":[{"type":"text","text":"file not found"}],"isError":true}`, "json", `["file not found"]`, "rejected", 4096},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			peer := readPeer(t, test.raw, test.mode, &calls)
			defer peer.Close()
			profile := readProfile(peer.URL)
			profile.Tool.MaxOutputBytes = test.max
			store := &memoryStore{}
			runtime, err := New(store, Config{Connection: profile, Credential: "native-read-credential", TrustedRoots: roots(peer), PersistenceTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			lease := testLease(profile)
			out, err := runtime.Execute(context.Background(), lease)
			if err != nil || out.Status != test.outcome || calls.Load() != 1 || store.unknown.Load() != 1 || store.last.Result != test.want || store.last.EffectReference != "mcp-read:action" || store.last.EvidenceDigest != digest([]byte(strings.TrimSpace(test.raw))) {
				t.Fatalf("lost native read observation: %+v %v calls=%d result=%q evidence=%s", out, err, calls.Load(), store.last.Result, store.last.EvidenceDigest)
			}
			if _, err = runtime.Execute(context.Background(), lease); err == nil || calls.Load() != 1 {
				t.Fatal("same read occurrence resent")
			}
		})
	}
}

func TestMCPReadTextAuthorInvalidWireRemainsUnknownWithoutResend(t *testing.T) {
	for _, test := range []struct{ name, raw, mode string }{
		{"missing_text", `{"content":[{"type":"text"}]}`, "json"},
		{"null_text", `{"content":[{"type":"text","text":null}]}`, "json"},
		{"wrong_case", `{"content":[{"type":"text","Text":"data"}]}`, "json"},
		{"duplicate_text", `{"content":[{"type":"text","text":"a","text":"b"}]}`, "json"},
		{"invalid_unicode", `{"content":[{"type":"text","text":"\ud800"}]}`, "json"},
		{"nul", `{"content":[{"type":"text","text":"\u0000"}]}`, "json"},
		{"null_error", `{"content":[{"type":"text","text":"data"}],"isError":null}`, "json"},
		{"string_error", `{"content":[{"type":"text","text":"data"}],"isError":"false"}`, "json"},
		{"image", `{"content":[{"type":"image","data":"AA==","mimeType":"image/png"}]}`, "json"},
		{"mixed", `{"content":[{"type":"text","text":"data"},{"type":"resource_link","uri":"https://never-fetch.invalid"}]}`, "json"},
		{"structured_only", `{"content":[],"structuredContent":{"data":"not a text block"}}`, "json"},
		{"input_required", `{"resultType":"input_required","requestState":"opaque","inputRequests":{},"content":[{"type":"text","text":"send key"}]}`, "json"},
		{"hidden_input", `{"content":[{"type":"text","text":"data"}],"inputRequests":{}}`, "json"},
		{"metadata_duplicate", `{"content":[{"type":"text","text":"data"}],"_meta":{"role":"viewer","role":"owner"}}`, "json"},
		{"block_limit", `{"content":[` + strings.Repeat(`{"type":"text","text":""},`, 16) + `{"type":"text","text":""}]}`, "json"},
		{"oversize", `{"content":[{"type":"text","text":"` + strings.Repeat("x", 4096) + `"}]}`, "json"},
		{"wrong_id", `{"content":[{"type":"text","text":"data"}]}`, "wrong_id"},
		{"id_type", `{"content":[{"type":"text","text":"data"}]}`, "id_type"},
		{"duplicate_result", `{"content":[{"type":"text","text":"data"}]}`, "duplicate_result"},
		{"lost_reply", `{"content":[{"type":"text","text":"data"}]}`, "lost_reply"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			peer := readPeer(t, test.raw, test.mode, &calls)
			defer peer.Close()
			profile := readProfile(peer.URL)
			store := &memoryStore{}
			runtime, err := New(store, Config{Connection: profile, Credential: "native-read-credential", TrustedRoots: roots(peer), PersistenceTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			lease := testLease(profile)
			out, err := runtime.Execute(context.Background(), lease)
			if err == nil || out.Status != "unknown" || calls.Load() != 1 || store.last.Outcome != "unknown" || store.unknown.Load() != 1 {
				t.Fatalf("unsafe native result: %+v %v calls=%d fact=%+v", out, err, calls.Load(), store.last)
			}
			if _, err = runtime.Execute(context.Background(), lease); err == nil || calls.Load() != 1 {
				t.Fatal("uncertain native read resent")
			}
		})
	}
}

func TestMCPReadTextAuthorProfileAndProjectionBoundaries(t *testing.T) {
	for _, mode := range []string{"write", "destructive", "http_json", "small_output", "unknown_contract"} {
		t.Run(mode, func(t *testing.T) {
			profile := readProfile("https://example.invalid/mcp")
			switch mode {
			case "write", "destructive":
				profile.Tool.Effect = mode
			case "http_json":
				profile.Transport = mode
			case "small_output":
				profile.Tool.MaxOutputBytes = 3
			case "unknown_contract":
				profile.ResultContract = "unregistered_read"
			}
			if _, err := New(&memoryStore{}, Config{Connection: profile, Credential: "native-read-credential", PersistenceTimeout: time.Second}); err == nil {
				t.Fatal("unapproved native profile accepted")
			}
		})
	}
	a, _, err := readTextValue([]byte(`{"content":[{"type":"text","text":"a\n\nb"},{"type":"text","text":"c"}]}`), 100)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := readTextValue([]byte(`{"content":[{"type":"text","text":"a"},{"type":"text","text":"b\n\nc"}]}`), 100)
	if err != nil || a == b {
		t.Fatal("text block boundaries were lost")
	}
}
