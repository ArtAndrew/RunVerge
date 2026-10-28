// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/storage"
)

func independentEventHTTPTask(t *testing.T, s *storage.Store, b storage.Bootstrap) domain.Task {
	t.Helper()
	ctx := context.Background()
	p, err := s.Authenticate(ctx, b.Token, b.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.CreateAgent(ctx, p, domain.NewID("key"), domain.CreateAgent{Name: "Independent event fixture", Instructions: "Return the fixture input"})
	if err != nil {
		t.Fatal(err)
	}
	var a domain.Agent
	if json.Unmarshal(raw, &a) != nil {
		t.Fatal("invalid fixture agent")
	}
	if _, err = s.PublishAgent(ctx, p, a.ID, domain.NewID("key"), a.Version); err != nil {
		t.Fatal(err)
	}
	raw, err = s.CreateTask(ctx, p, domain.NewID("key"), domain.CreateTask{AgentID: a.ID, Goal: "Observe bounded task history", BudgetMicroUSD: 10000})
	var v domain.Task
	if err != nil || json.Unmarshal(raw, &v) != nil || v.ID == "" {
		t.Fatal("invalid fixture task", err)
	}
	return v
}

func independentEventHTTPError(t *testing.T, body []byte, want string) {
	t.Helper()
	var v struct {
		Error struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &v) != nil || v.Error.Code != want || v.Error.Retryable {
		t.Fatalf("unexpected public error shape/code/retryability: wanted %s", want)
	}
}

func independentEventHTTPSnapshot(t *testing.T, f *independentBrowserFixture, id string) domain.TaskEventSnapshot {
	t.Helper()
	_, body := f.request("GET", "/api/v1/tasks/"+id+"/event-snapshot", "", nil, 200)
	var value struct {
		Data domain.TaskEventSnapshot `json:"data"`
	}
	if json.Unmarshal(body, &value) != nil || value.Data.TaskID != id || value.Data.Task.ID != id || value.Data.OrganizationID != f.bootstrap.OrganizationID || value.Data.Cursor == "" {
		t.Fatal("invalid scoped snapshot response")
	}
	return value.Data
}

func TestIndependentEventHTTPVersionCheckedAfterAuthBeforeDecodeOrReplay(t *testing.T) {
	f := independentBrowserAPI(t)
	ctx := context.Background()
	var beforeKeys, beforeAgents int
	if err := f.store.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM idempotency_keys),(SELECT count(*) FROM agents)").Scan(&beforeKeys, &beforeAgents); err != nil {
		t.Fatal(err)
	}
	badVersions := [][]string{nil, {""}, {"0.10.0"}, {"0.11.0"}, {"0.11. 0"}, {"0.14.0, 0.14.0"}, {domain.ContractVersion, domain.ContractVersion}, {strings.Repeat("x", 1025)}}
	for i, values := range badVersions {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			f.t = t
			response, body := f.request("POST", "/api/v1/agents", `{"invalid JSON before decoder`, func(r *http.Request) {
				r.Header.Del(domain.APIContractHeader)
				if values != nil {
					r.Header[http.CanonicalHeaderKey(domain.APIContractHeader)] = values
				}
				r.Header.Set("Idempotency-Key", "independent-version-no-commit")
			}, 409)
			if response.Header.Get(domain.APIContractHeader) != domain.ContractVersion {
				t.Fatal("incompatible response omitted the current contract revision", i)
			}
			independentEventHTTPError(t, body, "incompatible_client")
		})
	}
	f.t = t
	var afterKeys, afterAgents int
	if err := f.store.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM idempotency_keys),(SELECT count(*) FROM agents)").Scan(&afterKeys, &afterAgents); err != nil || afterKeys != beforeKeys || afterAgents != beforeAgents {
		t.Fatal("incompatible client reached decoding or committed command state", err)
	}
	// Authentication remains first: the version header cannot turn an invalid
	// credential into a contract-version oracle for a protected resource.
	_, body := f.request("GET", "/api/v1/me", "", func(r *http.Request) {
		r.Header.Del("Cookie")
		r.Header.Set("Authorization", "Bearer invalid")
		r.Header.Del(domain.APIContractHeader)
	}, 401)
	independentEventHTTPError(t, body, "unauthenticated")
	input := `{"name":"Versioned independent command","instructions":"Fixture only"}`
	_, original := f.request("POST", "/api/v1/agents", input, func(r *http.Request) { r.Header.Set("Idempotency-Key", "independent-committed-version") }, 201)
	_, body = f.request("POST", "/api/v1/agents", input, func(r *http.Request) {
		r.Header.Set("Idempotency-Key", "independent-committed-version")
		r.Header.Set(domain.APIContractHeader, "0.10.0")
	}, 409)
	independentEventHTTPError(t, body, "incompatible_client")
	_, repeated := f.request("POST", "/api/v1/agents", input, func(r *http.Request) { r.Header.Set("Idempotency-Key", "independent-committed-version") }, 201)
	var originalReply, repeatedReply any
	decodeExact := func(body []byte, target *any) error {
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.UseNumber()
		return decoder.Decode(target)
	}
	if decodeExact(original, &originalReply) != nil || decodeExact(repeated, &repeatedReply) != nil || !reflect.DeepEqual(originalReply, repeatedReply) {
		t.Fatal("version rejection replaced the durable original command reply")
	}
	_, discovery := f.request("GET", "/api/v1/compatibility", "", func(r *http.Request) {
		r.Header.Del("Cookie")
		r.Header.Del(domain.APIContractHeader)
	}, 200)
	var compatibility struct {
		Data domain.APICompatibility `json:"data"`
	}
	if json.Unmarshal(discovery, &compatibility) != nil || compatibility.Data.APIVersion != domain.ContractVersion || !compatibility.Data.HeaderRequired || len(compatibility.Data.SupportedVersions) != 1 || compatibility.Data.SupportedVersions[0] != domain.ContractVersion {
		t.Fatal("public discovery did not explain the actual required revision")
	}
}

func TestIndependentEventHTTPStrictScopeAndExplicitRetentionReset(t *testing.T) {
	f := independentBrowserAPI(t)
	v := independentEventHTTPTask(t, f.store, f.bootstrap)
	other := independentEventHTTPTask(t, f.store, f.other)
	second := independentEventHTTPTask(t, f.store, f.bootstrap)
	before := independentEventHTTPSnapshot(t, f, v.ID)
	path := "/api/v1/tasks/" + v.ID
	for _, suffix := range []string{"/event-snapshot?cursor=x", "/event-snapshot?limit=1", "/event-feed?unknown=x", "/event-feed?cursor=", "/event-feed?cursor=x&cursor=x", "/event-feed?limit=0", "/event-feed?limit=101", "/event-feed?limit=1&limit=1", "/event-feed?limit=", "/event-feed?cursor=garbage", "/event-stream?limit=1", "/event-stream?cursor="} {
		_, body := f.request("GET", path+suffix, "", nil, 400)
		independentEventHTTPError(t, body, "invalid_request")
	}
	for _, suffix := range []string{"/event-snapshot", "/event-feed?cursor=garbage", "/event-stream?cursor=garbage"} {
		_, body := f.request("GET", "/api/v1/tasks/"+other.ID+suffix, "", nil, 404)
		independentEventHTTPError(t, body, "not_found")
	}
	_, body := f.request("GET", "/api/v1/tasks/"+second.ID+"/event-feed?cursor="+url.QueryEscape(before.Cursor), "", nil, 400)
	independentEventHTTPError(t, body, "invalid_request")
	if _, err := f.store.AdvanceEventReplayWindow(context.Background(), f.bootstrap.OrganizationID, v.ID, before.Window.ThroughSequence, "Independent HTTP logical retention"); err != nil {
		t.Fatal(err)
	}
	_, body = f.request("GET", path+"/event-feed?cursor="+url.QueryEscape(before.ReplayCursor), "", nil, 200)
	var batch struct {
		Data domain.TaskEventBatch `json:"data"`
	}
	if json.Unmarshal(body, &batch) != nil || !batch.Data.ResetRequired || batch.Data.ResetReason != "history_expired" || batch.Data.Cursor != "" || len(batch.Data.Events) != 0 {
		t.Fatal("expired feed hid a replay gap or implicitly advanced the consumer")
	}
	fresh := independentEventHTTPSnapshot(t, f, v.ID)
	_, body = f.request("GET", path+"/event-feed?cursor="+url.QueryEscape(fresh.Cursor), "", nil, 200)
	if json.Unmarshal(body, &batch) != nil || batch.Data.ResetRequired || len(batch.Data.Events) != 0 || batch.Data.Cursor != fresh.Cursor {
		t.Fatal("snapshot could not recover an empty retained feed")
	}
}

type independentEventSSE struct {
	event string
	id    string
	frame domain.TaskEventStreamFrame
}

func independentEventReadSSE(reader *bufio.Reader) (independentEventSSE, error) {
	var out independentEventSSE
	var data string
	bytesRead := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return out, err
		}
		bytesRead += len(line)
		if bytesRead > domain.MaxEventFrameBytes+2048 {
			return out, errors.New("SSE frame exceeded declared bound")
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if data == "" {
				bytesRead = 0
				continue
			}
			if err = json.Unmarshal([]byte(data), &out.frame); err != nil {
				return out, err
			}
			return out, nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			out.event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "id: ") {
			out.id = strings.TrimPrefix(line, "id: ")
		} else if strings.HasPrefix(line, "data: ") {
			if data != "" {
				return out, errors.New("unexpected multiline SSE JSON")
			}
			data = strings.TrimPrefix(line, "data: ")
		} else {
			return out, errors.New("unexpected SSE field")
		}
	}
}

func independentEventOpenStream(t *testing.T, f *independentBrowserFixture, task, query string, change func(*http.Request)) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	r, err := http.NewRequestWithContext(ctx, "GET", f.origin+"/api/v1/tasks/"+task+"/event-stream"+query, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+f.bootstrap.Token)
	r.Header.Set("X-Organization-ID", f.bootstrap.OrganizationID)
	r.Header.Set(domain.APIContractHeader, domain.ContractVersion)
	r.Header.Set("Accept", "text/event-stream")
	if change != nil {
		change(r)
	}
	client := &http.Client{Transport: f.client.Transport, CheckRedirect: f.client.CheckRedirect}
	response, err := client.Do(r)
	if err != nil {
		cancel()
		t.Fatal("actual TLS event stream request failed", err)
	}
	return response, cancel
}

func independentEventRequireBatch(t *testing.T, frame independentEventSSE, org, task string) domain.TaskEventBatch {
	t.Helper()
	if frame.event != "platform.events" || frame.frame.Kind != "batch" || frame.frame.FrameVersion != 1 || frame.frame.Error != nil || frame.frame.Batch == nil {
		t.Fatal("invalid complete SSE batch frame")
	}
	b := *frame.frame.Batch
	if b.OrganizationID != org || b.TaskID != task || b.ResetRequired || b.Cursor == "" || frame.id != b.Cursor {
		t.Fatal("SSE id/cursor was missing or crossed its exact scope")
	}
	for _, e := range b.Events {
		if e.OrganizationID != org || e.TaskID != task {
			t.Fatal("foreign event inside a scoped batch")
		}
	}
	return b
}

// A real accepted TCP connection has deliberately small socket buffers. This
// makes an unread TLS response encounter backpressure with a bounded fixture,
// without replacing the actual ResponseWriter or modifying server deadlines.
type independentEventSocketState struct {
	written      atomic.Int64
	writeStarted atomic.Int64
	maxWriteTime atomic.Int64
	deadline     atomic.Int64
	timeouts     atomic.Int64
	firstTimeout atomic.Int64
	closed       atomic.Bool
}

type independentEventObservedConn struct {
	net.Conn
	state *independentEventSocketState
}

func (c *independentEventObservedConn) Write(body []byte) (int, error) {
	start := time.Now()
	c.state.writeStarted.Store(start.UnixNano())
	n, err := c.Conn.Write(body)
	c.state.written.Add(int64(n))
	duration := time.Since(start).Nanoseconds()
	for old := c.state.maxWriteTime.Load(); duration > old && !c.state.maxWriteTime.CompareAndSwap(old, duration); old = c.state.maxWriteTime.Load() {
	}
	c.state.writeStarted.Store(0)
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		c.state.timeouts.Add(1)
		c.state.firstTimeout.CompareAndSwap(0, duration)
	}
	return n, err
}

func (c *independentEventObservedConn) SetWriteDeadline(deadline time.Time) error {
	c.state.deadline.Store(deadline.UnixNano())
	return c.Conn.SetWriteDeadline(deadline)
}

func (c *independentEventObservedConn) Close() error {
	c.state.closed.Store(true)
	return c.Conn.Close()
}

type independentEventSmallWriteListener struct {
	net.Listener
	accepted atomic.Int32
	first    *independentEventSocketState
}

func (l *independentEventSmallWriteListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tcp, ok := c.(*net.TCPConn); err == nil && ok {
		if setErr := tcp.SetWriteBuffer(1024); setErr != nil {
			c.Close()
			return nil, setErr
		}
	}
	if err == nil && l.accepted.Add(1) == 1 {
		c = &independentEventObservedConn{Conn: c, state: l.first}
	}
	return c, err
}

func TestIndependentEventTLSReconnectDoesNotAcceptAnIncompleteFrame(t *testing.T) {
	f := independentBrowserAPI(t)
	v := independentEventHTTPTask(t, f.store, f.bootstrap)
	snapshot := independentEventHTTPSnapshot(t, f, v.ID)
	response, stop := independentEventOpenStream(t, f, v.ID, "?cursor="+url.QueryEscape(snapshot.Cursor), nil)
	defer stop()
	defer response.Body.Close()
	if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatal("actual TLS response was not a successful SSE stream", response.StatusCode)
	}
	reader := bufio.NewReaderSize(response.Body, domain.MaxEventFrameBytes+2048)
	first, err := independentEventReadSSE(reader)
	if err != nil {
		t.Fatal(err)
	}
	initial := independentEventRequireBatch(t, first, f.bootstrap.OrganizationID, v.ID)
	if initial.Cursor != snapshot.Cursor || len(initial.Events) != 0 {
		t.Fatal("initial stream changed a snapshot already at its head")
	}
	f.request("POST", "/api/v1/tasks/"+v.ID+"/cancel", `{}`, nil, 200)
	// Read only the event/id lines. Even if the OS received the whole frame,
	// the consumer has not validated or applied its JSON and keeps the old id.
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.HasPrefix(line, "id: ") {
			break
		}
	}
	response.Body.Close()
	stop()
	reconnected, end := independentEventOpenStream(t, f, v.ID, "", func(r *http.Request) { r.Header.Set("Last-Event-ID", initial.Cursor) })
	defer end()
	defer reconnected.Body.Close()
	if reconnected.StatusCode != 200 {
		t.Fatal("Last-Event-ID reconnect failed", reconnected.StatusCode)
	}
	complete, err := independentEventReadSSE(bufio.NewReaderSize(reconnected.Body, domain.MaxEventFrameBytes+2048))
	if err != nil {
		t.Fatal(err)
	}
	batch := independentEventRequireBatch(t, complete, f.bootstrap.OrganizationID, v.ID)
	if len(batch.Events) != 1 || batch.Events[0].Type != "task.cancelled" || batch.Cursor == initial.Cursor {
		t.Fatal("reconnect lost the event from the incomplete previous frame")
	}
	_, wire := f.request("GET", "/api/v1/tasks/"+v.ID+"/event-feed?cursor="+url.QueryEscape(initial.Cursor), "", nil, 200)
	var exact struct {
		Data domain.TaskEventBatch `json:"data"`
	}
	if json.Unmarshal(wire, &exact) != nil || len(exact.Data.Events) != 1 || exact.Data.Cursor != batch.Cursor || exact.Data.Events[0].ID != batch.Events[0].ID || exact.Data.Events[0].Sequence != batch.Events[0].Sequence {
		t.Fatal("SSE reconnect invented a new occurrence instead of replaying the durable event")
	}
}

func TestIndependentEventTLSResetAndLastEventIDCannotChangeScope(t *testing.T) {
	f := independentBrowserAPI(t)
	v := independentEventHTTPTask(t, f.store, f.bootstrap)
	other := independentEventHTTPTask(t, f.store, f.bootstrap)
	snapshot := independentEventHTTPSnapshot(t, f, v.ID)
	otherSnapshot := independentEventHTTPSnapshot(t, f, other.ID)
	for _, change := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("Last-Event-ID", "") },
		func(r *http.Request) { r.Header["Last-Event-Id"] = []string{snapshot.Cursor, snapshot.Cursor} },
		func(r *http.Request) { r.Header.Set("Last-Event-ID", strings.Repeat("x", 1025)) },
		func(r *http.Request) { r.Header.Set("Last-Event-ID", otherSnapshot.Cursor) },
	} {
		response, stop := independentEventOpenStream(t, f, v.ID, "?cursor="+url.QueryEscape(snapshot.Cursor), change)
		body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		response.Body.Close()
		stop()
		if err != nil || response.StatusCode != 400 {
			t.Fatal("ambiguous/foreign Last-Event-ID was accepted", response.StatusCode, err)
		}
		independentEventHTTPError(t, body, "invalid_request")
	}
	if _, err := f.store.AdvanceEventReplayWindow(context.Background(), f.bootstrap.OrganizationID, v.ID, snapshot.Window.ThroughSequence, "Independent stream reset"); err != nil {
		t.Fatal(err)
	}
	response, stop := independentEventOpenStream(t, f, v.ID, "?cursor="+url.QueryEscape(snapshot.ReplayCursor), nil)
	defer stop()
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("expired stream did not return its explicit reset frame", response.StatusCode)
	}
	frame, err := independentEventReadSSE(bufio.NewReaderSize(response.Body, domain.MaxEventFrameBytes+2048))
	if err != nil || frame.event != "platform.reset" || frame.id != "" || frame.frame.Kind != "reset_required" || frame.frame.FrameVersion != 1 || frame.frame.Error != nil || frame.frame.Batch == nil {
		t.Fatal("invalid reset envelope or unsafe cursor advancement", err)
	}
	b := frame.frame.Batch
	if b.OrganizationID != f.bootstrap.OrganizationID || b.TaskID != v.ID || !b.ResetRequired || b.ResetReason != "history_expired" || b.Cursor != "" || len(b.Events) != 0 {
		t.Fatal("reset frame advanced or changed the exact feed")
	}
}

func TestIndependentEventTLSRevocationAfterBlockedInitialReadLeaksNoBatch(t *testing.T) {
	f := independentBrowserAPI(t)
	v := independentEventHTTPTask(t, f.store, f.bootstrap)
	snapshot := independentEventHTTPSnapshot(t, f, v.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	barrier, err := f.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback(ctx)
	if _, err = barrier.Exec(ctx, "LOCK TABLE events IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequestWithContext(ctx, "GET", f.origin+"/api/v1/tasks/"+v.ID+"/event-stream?cursor="+url.QueryEscape(snapshot.Cursor), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+f.bootstrap.Token)
	r.Header.Set("X-Organization-ID", f.bootstrap.OrganizationID)
	r.Header.Set(domain.APIContractHeader, domain.ContractVersion)
	r.Header.Set("Accept", "text/event-stream")
	type result struct {
		response *http.Response
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := (&http.Client{Transport: f.client.Transport}).Do(r)
		done <- result{response, err}
	}()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		err = f.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
 AND wait_event_type='Lock' AND $1::integer=ANY(pg_blocking_pids(pid)) AND query LIKE 'SELECT after_sequence,version,%')`, barrier.Conn().PgConn().PID()).Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("actual initial replay never reached its exact lock barrier")
		case <-ticker.C:
		}
	}
	if _, err = f.store.Pool.Exec(ctx, "UPDATE access_tokens SET revoked=true WHERE token_hash=$1", storage.Hash([]byte(f.bootstrap.Token))); err != nil {
		t.Fatal(err)
	}
	if err = barrier.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	observed := <-done
	if observed.err != nil {
		t.Fatal(observed.err)
	}
	defer observed.response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(observed.response.Body, 1<<20))
	if err != nil || observed.response.StatusCode != 401 || strings.Contains(string(body), "platform.events") {
		t.Fatal("revoked token received prepared events after the blocked read", observed.response.StatusCode, err)
	}
	independentEventHTTPError(t, body, "unauthenticated")
	// The separate browser credential still works. Reading and rejecting a
	// stream neither changes the task nor manufactures financial activity.
	f.request("GET", "/api/v1/tasks/"+v.ID, "", nil, 200)
	var actions, reserves int
	if err = f.store.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM actions),(SELECT count(*) FROM reservations)").Scan(&actions, &reserves); err != nil || actions != 0 || reserves != 0 {
		t.Fatal("stream access changed execution/accounting facts", err)
	}
}

func TestIndependentEventTLSIdleRevocationStopsBeforeNewEvent(t *testing.T) {
	f := independentBrowserAPI(t)
	v := independentEventHTTPTask(t, f.store, f.bootstrap)
	snapshot := independentEventHTTPSnapshot(t, f, v.ID)
	response, stop := independentEventOpenStream(t, f, v.ID, "?cursor="+url.QueryEscape(snapshot.Cursor), nil)
	defer stop()
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("stream did not start", response.StatusCode)
	}
	reader := bufio.NewReaderSize(response.Body, domain.MaxEventFrameBytes+2048)
	first, err := independentEventReadSSE(reader)
	if err != nil {
		t.Fatal(err)
	}
	independentEventRequireBatch(t, first, f.bootstrap.OrganizationID, v.ID)
	if _, err = f.store.Pool.Exec(context.Background(), "UPDATE access_tokens SET revoked=true WHERE token_hash=$1", storage.Hash([]byte(f.bootstrap.Token))); err != nil {
		t.Fatal(err)
	}
	// Commit the revocation before the new event: a wakeup is a hint to replay,
	// never a replacement for the stream's next current authorization check.
	f.request("POST", "/api/v1/tasks/"+v.ID+"/cancel", `{}`, nil, 200)
	for {
		frame, err := independentEventReadSSE(reader)
		if err != nil {
			t.Fatal(err)
		}
		// An empty frame admitted before revocation can already be buffered by
		// the network. Its old coordinate is harmless; no new fact may leak.
		if frame.frame.Kind == "batch" {
			b := independentEventRequireBatch(t, frame, f.bootstrap.OrganizationID, v.ID)
			if len(b.Events) != 0 || b.Cursor != snapshot.Cursor {
				t.Fatal("stream exposed the event committed after token revocation")
			}
			continue
		}
		if frame.event != "platform.error" || frame.id != "" || frame.frame.Kind != "error" || frame.frame.Batch != nil || frame.frame.Error == nil || frame.frame.Error.Code != "unauthenticated" || frame.frame.Error.Retryable {
			t.Fatal("active stream sent post-revocation data or a resumable error id")
		}
		break
	}
}

func TestIndependentEventTLSServiceExpiryIsRecheckedWithoutNewEvents(t *testing.T) {
	f := independentBrowserAPI(t)
	v := independentEventHTTPTask(t, f.store, f.bootstrap)
	snapshot := independentEventHTTPSnapshot(t, f, v.ID)
	ctx := context.Background()
	p, err := f.store.Authenticate(ctx, f.bootstrap.Token, f.bootstrap.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := f.store.CreateServiceAccount(ctx, p, domain.NewID("key"), domain.CreateServiceAccount{DisplayName: "Independent event observer", Role: "viewer", Reason: "Bounded live read expiry proof"})
	var account domain.ServiceAccount
	if err != nil || json.Unmarshal(raw, &account) != nil || account.PrincipalID == "" {
		t.Fatal("invalid real service account fixture", err)
	}
	token, key, err := f.store.IssueServiceToken(ctx, f.bootstrap.OrganizationID, account.PrincipalID, "viewer", 2*time.Second, "Natural event observer expiry")
	if err != nil {
		t.Fatal(err)
	}
	response, stop := independentEventOpenStream(t, f, v.ID, "?cursor="+url.QueryEscape(snapshot.Cursor), func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) })
	defer stop()
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("service viewer could not start a permitted stream", response.StatusCode)
	}
	reader := bufio.NewReaderSize(response.Body, domain.MaxEventFrameBytes+2048)
	for {
		frame, err := independentEventReadSSE(reader)
		if err != nil {
			t.Fatal(err)
		}
		if frame.frame.Kind == "batch" {
			b := independentEventRequireBatch(t, frame, f.bootstrap.OrganizationID, v.ID)
			if b.Cursor != snapshot.Cursor || len(b.Events) != 0 {
				t.Fatal("idle service stream invented events")
			}
			continue
		}
		if frame.event != "platform.error" || frame.id != "" || frame.frame.Error == nil || frame.frame.Error.Code != "unauthenticated" || frame.frame.Error.Retryable {
			t.Fatal("idle heartbeat did not stop the naturally expired service credential")
		}
		var expired bool
		if err = f.store.Pool.QueryRow(ctx, "SELECT clock_timestamp()>$1", key.ExpiresAt).Scan(&expired); err != nil || !expired {
			t.Fatal("fixture did not reach actual database-clock expiry", err)
		}
		break
	}
	// A key's natural expiry is an admission boundary. It never cancels or
	// otherwise changes the durable task merely because a reader disconnected.
	f.request("GET", "/api/v1/tasks/"+v.ID, "", nil, 200)
}

func TestIndependentEventTLSSlowReaderReleasesCapacityWithoutHoldingDatabase(t *testing.T) {
	f := independentBrowserAPI(t)
	v := independentEventHTTPTask(t, f.store, f.bootstrap)
	ctx, cancel := context.WithTimeout(context.Background(), 19*time.Second)
	defer cancel()
	// Historical fixtures are explicitly v0 and retain their outbox pairs.
	// The bounded 2.4MiB body exceeds the intentionally tiny TCP buffers.
	if _, err := f.store.Pool.Exec(ctx, `WITH inserted AS (
 INSERT INTO events(organization_id,id,task_id,type,data,payload_version)
 SELECT $1,'evt_slow_'||i::text,$2,'historical.slow',jsonb_build_object('opaque',repeat('x',12000)),0
 FROM generate_series(1,200) i RETURNING sequence,organization_id)
 INSERT INTO outbox(sequence,organization_id) SELECT sequence,organization_id FROM inserted`, f.bootstrap.OrganizationID, v.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := independentEventHTTPSnapshot(t, f, v.ID)
	api := &Server{Store: f.store}
	handler := api.Handler()
	slowReturned := make(chan struct{})
	var slowReturnedAt atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/tasks/"+v.ID+"/event-stream" && r.URL.RawQuery == "" {
			defer func() { slowReturnedAt.Store(time.Now().UnixNano()); close(slowReturned) }()
		}
		handler.ServeHTTP(w, r)
	}))
	observedSocket := new(independentEventSocketState)
	server.Listener = &independentEventSmallWriteListener{Listener: server.Listener, first: observedSocket}
	server.StartTLS()
	t.Cleanup(server.Close)
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, addr)
		if tcp, ok := c.(*net.TCPConn); err == nil && ok {
			if e := tcp.SetReadBuffer(1024); e != nil {
				c.Close()
				return nil, e
			}
		}
		return c, err
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}
	open := func(cursor string) (*http.Response, error) {
		r, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/v1/tasks/"+v.ID+"/event-stream"+cursor, nil)
		if err != nil {
			return nil, err
		}
		r.Header.Set("Authorization", "Bearer "+f.bootstrap.Token)
		r.Header.Set("X-Organization-ID", f.bootstrap.OrganizationID)
		r.Header.Set(domain.APIContractHeader, domain.ContractVersion)
		return client.Do(r)
	}
	// Use a raw real TLS/HTTP client for the stalled connection. There is no
	// HTTP transport background reader that could prefetch/consume its body.
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	rawTCP, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", serverURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	if err = rawTCP.(*net.TCPConn).SetReadBuffer(1024); err != nil {
		rawTCP.Close()
		t.Fatal(err)
	}
	tlsConfig := transport.TLSClientConfig.Clone()
	tlsConfig.ServerName = serverURL.Hostname()
	slowConn := tls.Client(rawTCP, tlsConfig)
	defer slowConn.Close()
	if err = slowConn.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest("GET", server.URL+"/api/v1/tasks/"+v.ID+"/event-stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.bootstrap.Token)
	request.Header.Set("X-Organization-ID", f.bootstrap.OrganizationID)
	request.Header.Set(domain.APIContractHeader, domain.ContractVersion)
	if err = request.Write(slowConn); err != nil {
		t.Fatal(err)
	}
	slow, err := http.ReadResponse(bufio.NewReaderSize(slowConn, 4096), request)
	if err != nil || slow.StatusCode != 200 {
		t.Fatal("bounded slow-reader stream did not start", err)
	}
	defer func() { slowConn.Close(); slow.Body.Close() }()
	// Never read slow.Body. The other three streams are already at head and
	// can send only a tiny initial batch/heartbeats, so they remain healthy.
	for i := 0; i < 3; i++ {
		response, err := open("?cursor=" + url.QueryEscape(snapshot.Cursor))
		if err != nil || response.StatusCode != 200 {
			t.Fatal("healthy capacity fixture did not start", err)
		}
		defer response.Body.Close()
		frame, err := independentEventReadSSE(bufio.NewReaderSize(response.Body, domain.MaxEventFrameBytes+2048))
		if err != nil {
			t.Fatal(err)
		}
		independentEventRequireBatch(t, frame, f.bootstrap.OrganizationID, v.ID)
	}
	rejected, err := open("?cursor=" + url.QueryEscape(snapshot.Cursor))
	if err != nil || rejected.StatusCode != 503 {
		t.Fatal("fifth concurrent stream bypassed the per-principal bound", err)
	}
	io.Copy(io.Discard, io.LimitReader(rejected.Body, 1<<20))
	rejected.Body.Close()
	if acquired := f.store.Pool.Stat().AcquiredConns(); acquired != 0 {
		t.Fatal("streams retained a PostgreSQL pool connection while waiting for socket progress", acquired)
	}
	canaryCtx, canaryCancel := context.WithTimeout(ctx, time.Second)
	defer canaryCancel()
	if err = f.store.Pool.Ping(canaryCtx); err != nil {
		t.Fatal("slow socket blocked independent database work", err)
	}
	f.request("GET", "/api/v1/me", "", nil, 200)
	// TCP can keep making progress while its finite kernel buffers fill. The
	// three-second write deadline starts at each Write, not at HTTP headers.
	// Go's net/http chunkWriter synchronously calls TLS.Close on write failure;
	// its close_notify adds up to five seconds before the handler returns.
	// Keep physical admission until then: do not free slots for still-running
	// TLS handlers. Eight seconds of I/O+cleanup get a one-second jitter bound.
	deadline := time.Now().Add(8 * time.Second)
	var blockedSince time.Time
	checkedWhileBlocked := false
	for {
		if began := observedSocket.writeStarted.Load(); began != 0 && time.Since(time.Unix(0, began)) >= 300*time.Millisecond && !checkedWhileBlocked {
			blockedSince = time.Unix(0, began)
			deadline = blockedSince.Add(9 * time.Second)
			checkedWhileBlocked = true
			canary, endCanary := context.WithTimeout(ctx, time.Second)
			if err = f.store.Pool.Ping(canary); err != nil {
				endCanary()
				t.Fatal("a genuinely blocked socket held the database", err)
			}
			endCanary()
			f.request("GET", "/api/v1/me", "", nil, 200)
			poolDeadline := time.Now().Add(100 * time.Millisecond)
			for f.store.Pool.Stat().AcquiredConns() != 0 && time.Now().Before(poolDeadline) {
				time.Sleep(time.Millisecond)
			}
			if f.store.Pool.Stat().AcquiredConns() != 0 {
				t.Fatal("blocked socket retained a PostgreSQL pool connection")
			}
		}
		available, openErr := open("?cursor=" + url.QueryEscape(snapshot.Cursor))
		if openErr != nil {
			t.Fatal(openErr)
		}
		if available.StatusCode == 200 {
			available.Body.Close()
			select {
			case <-slowReturned:
			case <-time.After(250 * time.Millisecond):
				t.Fatal("physical stream admission was released before the stalled handler returned")
			}
			if !blockedSince.IsZero() && time.Unix(0, slowReturnedAt.Load()).Sub(blockedSince) > 9*time.Second {
				t.Fatal("stalled reader capacity was not released within the write-deadline bound")
			}
			break
		}
		failureBody, readFailure := io.ReadAll(io.LimitReader(available.Body, 1<<20))
		available.Body.Close()
		if available.StatusCode != 503 || time.Now().After(deadline) {
			returned := false
			select {
			case <-slowReturned:
				returned = true
			default:
			}
			api.eventStreams.Lock()
			total := api.eventStreams.total
			api.eventStreams.Unlock()
			stack := make([]byte, 256<<10)
			n := runtime.Stack(stack, true)
			for _, goroutine := range strings.Split(string(stack[:n]), "\n\n") {
				if strings.Contains(goroutine, "taskEventStream(") || strings.Contains(goroutine, "independentEventObservedConn)") {
					t.Log(goroutine)
				}
			}
			t.Fatalf("stalled stream did not release its slot: status=%d returned=%v admission_total=%d socket_bytes=%d active_write_start=%d max_write_ns=%d deadline_ns=%d timeouts=%d closed=%v response=%s read_error=%v", available.StatusCode, returned, total, observedSocket.written.Load(), observedSocket.writeStarted.Load(), observedSocket.maxWriteTime.Load(), observedSocket.deadline.Load(), observedSocket.timeouts.Load(), observedSocket.closed.Load(), failureBody, readFailure)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	if !checkedWhileBlocked || observedSocket.timeouts.Load() == 0 || observedSocket.maxWriteTime.Load() < int64(2*time.Second) {
		t.Fatal("test did not exercise a real blocked socket write and deadline")
	}
	if first := time.Duration(observedSocket.firstTimeout.Load()); first < 2*time.Second || first > 3500*time.Millisecond {
		t.Fatal("application socket I/O did not respect its three-second deadline", first)
	}
	t.Logf("actual socket first_timeout=%s maximum_write_including_TLS_cleanup=%s handler_return_after_block=%s encrypted_bytes=%d; independent database and HTTP reads remained available", time.Duration(observedSocket.firstTimeout.Load()), time.Duration(observedSocket.maxWriteTime.Load()), time.Unix(0, slowReturnedAt.Load()).Sub(blockedSince), observedSocket.written.Load())
}
