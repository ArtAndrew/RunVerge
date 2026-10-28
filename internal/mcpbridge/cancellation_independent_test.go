// SPDX-License-Identifier: Elastic-2.0
package mcpbridge_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"agentplatform.local/platform/internal/mcpbridge"
)

func independentMCPStream(t *testing.T, backend mcpbridge.Backend) (net.Conn, *bufio.Reader) {
	t.Helper()
	s, e := mcpbridge.NewServer(mcpbridge.ServerConfig{Backend: backend, RequestTimeout: 3 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	server, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- s.ServeIO(ctx, mcpbridge.Credentials{Token: "independent-fixture-only", OrganizationID: "org_independent"}, server, server)
	}()
	t.Cleanup(func() {
		cancel()
		client.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("stdio stream did not shut down")
		}
	})
	client.SetDeadline(time.Now().Add(2 * time.Second))
	return client, bufio.NewReader(client)
}
func independentMCPWrite(t *testing.T, c net.Conn, raw []byte) {
	t.Helper()
	if _, e := c.Write(append(raw, '\n')); e != nil {
		t.Fatal(e)
	}
}
func independentMCPRead(t *testing.T, r *bufio.Reader) map[string]json.RawMessage {
	t.Helper()
	raw, e := r.ReadBytes('\n')
	if e != nil {
		t.Fatal(e)
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(raw, &out) != nil {
		t.Fatal("invalid stdio response")
	}
	return out
}

func TestIndependentMCPStdioCancellationAliasCannotTargetOtherGeneration(t *testing.T) {
	firstStarted, secondStarted := make(chan struct{}), make(chan struct{})
	firstCancelled, secondCancelled := make(chan struct{}), make(chan struct{})
	backend := mcpbridge.BackendFunc(func(ctx context.Context, _ mcpbridge.Credentials, r mcpbridge.Request) (mcpbridge.Response, error) {
		switch r.Path {
		case "/api/v1/tasks/tsk_first":
			close(firstStarted)
			<-ctx.Done()
			close(firstCancelled)
			return mcpbridge.Response{}, ctx.Err()
		case "/api/v1/tasks/tsk_second":
			close(secondStarted)
			<-ctx.Done()
			close(secondCancelled)
			return mcpbridge.Response{}, ctx.Err()
		default:
			return mcpbridge.Response{Status: 200, Body: json.RawMessage(`{"data":{}}`)}, nil
		}
	})
	c, r := independentMCPStream(t, backend)
	independentMCPWrite(t, c, independentMCPFrame(`100`, "tools/call", `{"name":"platform_tasks_get","arguments":{"task_id":"tsk_first"}}`))
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first request not running")
	}
	independentMCPWrite(t, c, independentMCPFrame(`200`, "tools/call", `{"name":"platform_tasks_get","arguments":{"task_id":"tsk_second"}}`))
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second request not running")
	}
	// The lowercase alias sorts after canonical requestId when the adapter
	// remarshal occurs. It must not overwrite the fresh internal generation 1
	// with generation 2 in the SDK's case-insensitive decoder.
	independentMCPWrite(t, c, []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":100,"requestid":2}}`))
	select {
	case <-secondCancelled:
		t.Fatal("case-shadow cancellation targeted a different live request")
	case <-firstCancelled:
	case <-time.After(time.Second):
		t.Fatal("canonical target cancellation not delivered")
	}
	independentMCPWrite(t, c, independentMCPFrame(`300`, "tools/list", `{}`))
	out := independentMCPRead(t, r)
	if string(out["id"]) != "300" || independentMCPFailed(out) {
		t.Fatal("cancelled target emitted response or stream routing changed")
	}
	select {
	case <-secondCancelled:
		t.Fatal("unrelated request was cancelled")
	default:
	}
}

func TestIndependentMCPStdioCancelledIDReuseHasFreshGeneration(t *testing.T) {
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	backend := mcpbridge.BackendFunc(func(ctx context.Context, _ mcpbridge.Credentials, r mcpbridge.Request) (mcpbridge.Response, error) {
		if r.Path == "/api/v1/tasks/tsk_old" {
			close(started)
			<-ctx.Done()
			close(cancelled)
			<-release
			return mcpbridge.Response{Status: 200, Body: json.RawMessage(`{"data":{"generation":"old"}}`)}, nil
		}
		return mcpbridge.Response{Status: 200, Body: json.RawMessage(`{"data":{"generation":"new"}}`)}, nil
	})
	c, r := independentMCPStream(t, backend)
	var released sync.Once
	defer released.Do(func() { close(release) })
	independentMCPWrite(t, c, independentMCPFrame(`"reuse"`, "tools/call", `{"name":"platform_tasks_get","arguments":{"task_id":"tsk_old"}}`))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("old request not running")
	}
	independentMCPWrite(t, c, []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"reuse"}}`))
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("old request not cancelled")
	}
	independentMCPWrite(t, c, independentMCPFrame(`"reuse"`, "tools/call", `{"name":"platform_tasks_get","arguments":{"task_id":"tsk_new"}}`))
	out := independentMCPRead(t, r)
	var result struct {
		Structured struct {
			Data struct {
				Generation string `json:"generation"`
			} `json:"data"`
		} `json:"structuredContent"`
	}
	if string(out["id"]) != `"reuse"` || json.Unmarshal(out["result"], &result) != nil || result.Structured.Data.Generation != "new" {
		t.Fatal("reused request ID got old outcome")
	}
	released.Do(func() { close(release) })
	independentMCPWrite(t, c, independentMCPFrame(`"barrier"`, "tools/list", `{}`))
	out = independentMCPRead(t, r)
	if string(out["id"]) != `"barrier"` {
		t.Fatal("cancelled generation produced a later response")
	}
}
