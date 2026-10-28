// SPDX-License-Identifier: Elastic-2.0
package storage

// The independent fixture cuts a real PostgreSQL TCP connection at a bounded,
// observed COMMIT boundary. Authentication and query/parameter packets are
// forwarded opaquely and never logged. It is limited to disposable loopback
// schemas; it cannot attach to the main application schema or remote databases.
import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const independentPgwireMaxFrame = 8 << 20

type independentCommitCutPlan struct {
	match string
	mode  string
	done  chan string
}
type independentCommitCutProxy struct {
	listener    net.Listener
	target      string
	mu          sync.Mutex
	plan        *independentCommitCutPlan
	connections map[net.Conn]net.Conn
	closed      bool
	wg          sync.WaitGroup
}
type independentPgwireConnection struct {
	proxy          *independentCommitCutProxy
	client, server net.Conn
	mu             sync.Mutex
	pending        *independentCommitCutPlan
	matched        bool
	statements     map[string]string
	statementBytes int
	once           sync.Once
}

func independentCommitProxyStore(t *testing.T, dsn string) (*Store, *independentCommitCutProxy) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid isolated proxy configuration")
	}
	q := u.Query()
	schema := q.Get("search_path")
	ip := net.ParseIP(u.Hostname())
	if !strings.HasPrefix(schema, "test_") || strings.ContainsAny(schema, ", ") || ip == nil || !ip.IsLoopback() || u.Port() == "" {
		t.Fatal("commit proxy requires one disposable loopback schema")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("cannot start local commit proxy")
	}
	p := &independentCommitCutProxy{listener: listener, target: net.JoinHostPort(u.Hostname(), u.Port()), connections: map[net.Conn]net.Conn{}}
	p.wg.Add(1)
	go p.accept()
	t.Cleanup(func() {
		p.close()
		done := make(chan struct{})
		go func() { p.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("commit proxy did not stop")
		}
	})
	u.Host = listener.Addr().String()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal("cannot open isolated proxy store")
	}
	t.Cleanup(store.Close)
	return store, p
}
func (p *independentCommitCutProxy) arm(t *testing.T, match, mode string) *independentCommitCutPlan {
	t.Helper()
	if match == "" || (mode != "before_commit" && mode != "after_commit") {
		t.Fatal("invalid commit cut plan")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.plan != nil {
		t.Fatal("commit cut was already armed")
	}
	v := &independentCommitCutPlan{match: strings.ToLower(match), mode: mode, done: make(chan string, 1)}
	p.plan = v
	return v
}
func (p *independentCommitCutProxy) wait(t *testing.T, plan *independentCommitCutPlan) {
	t.Helper()
	select {
	case stage := <-plan.done:
		if stage != plan.mode {
			t.Fatal("expected commit boundary was not observed", stage)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("armed real PostgreSQL commit boundary was not reached")
	}
}
func (p *independentCommitCutProxy) close() {
	p.mu.Lock()
	p.closed = true
	_ = p.listener.Close()
	for client, server := range p.connections {
		_ = client.Close()
		_ = server.Close()
	}
	p.mu.Unlock()
}
func (p *independentCommitCutProxy) accept() {
	defer p.wg.Done()
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.DialTimeout("tcp", p.target, time.Second)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		if p.closed || len(p.connections) >= 16 {
			p.mu.Unlock()
			_ = client.Close()
			_ = server.Close()
			continue
		}
		p.connections[client] = server
		p.mu.Unlock()
		c := &independentPgwireConnection{proxy: p, client: client, server: server, statements: map[string]string{}}
		p.wg.Add(1)
		go c.run()
	}
}
func (c *independentPgwireConnection) close() {
	c.once.Do(func() { _ = c.client.Close(); _ = c.server.Close() })
}
func (c *independentPgwireConnection) run() {
	defer c.proxy.wg.Done()
	defer func() { c.close(); c.proxy.mu.Lock(); delete(c.proxy.connections, c.client); c.proxy.mu.Unlock() }()
	_ = c.client.SetDeadline(time.Now().Add(25 * time.Second))
	_ = c.server.SetDeadline(time.Now().Add(25 * time.Second))
	var header [4]byte
	if _, err := io.ReadFull(c.client, header[:]); err != nil {
		return
	}
	size := binary.BigEndian.Uint32(header[:])
	if size < 8 || size > 65536 {
		return
	}
	startup := make([]byte, int(size)-4)
	if _, err := io.ReadFull(c.client, startup); err != nil {
		return
	}
	code := binary.BigEndian.Uint32(startup[:4])
	if code != 196608 && code != 80877102 {
		return
	}
	if _, err := c.server.Write(append(header[:], startup...)); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { c.frontend(); done <- struct{}{} }()
	go func() { c.backend(); done <- struct{}{} }()
	<-done
	c.close()
	<-done
}
func independentPgwireFrame(conn net.Conn) (byte, []byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return 0, nil, nil, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size < 4 || size > independentPgwireMaxFrame {
		return 0, nil, nil, errors.New("bounded PostgreSQL frame rejected")
	}
	payload := make([]byte, int(size)-4)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return 0, nil, nil, err
	}
	frame := append(header[:], payload...)
	return header[0], payload, frame, nil
}
func independentPgwireCString(raw []byte) (string, []byte, bool) {
	at := bytes.IndexByte(raw, 0)
	if at < 0 {
		return "", nil, false
	}
	return string(raw[:at]), raw[at+1:], true
}
func (c *independentPgwireConnection) frontend() {
	for {
		kind, payload, frame, err := independentPgwireFrame(c.client)
		if err != nil {
			return
		}
		query := ""
		switch kind {
		case 'Q':
			query, _, _ = independentPgwireCString(payload)
		case 'P':
			name, tail, ok := independentPgwireCString(payload)
			if !ok {
				return
			}
			sql, _, ok := independentPgwireCString(tail)
			if !ok {
				return
			}
			newBytes := c.statementBytes - len(c.statements[name]) + len(sql)
			if len(sql) > 65536 || newBytes > 1<<20 || (len(c.statements) >= 1024 && c.statements[name] == "") {
				return
			}
			c.statementBytes = newBytes
			c.statements[name] = sql
			query = sql
		case 'B':
			_, tail, ok := independentPgwireCString(payload)
			if !ok {
				return
			}
			name, _, ok := independentPgwireCString(tail)
			if !ok {
				return
			}
			query = c.statements[name]
		}
		if len(query) > 65536 {
			return
		}
		normalized := strings.ToLower(strings.TrimSpace(query))
		c.proxy.mu.Lock()
		plan := c.proxy.plan
		if normalized == "begin" || normalized == "rollback" {
			c.matched = false
		}
		if plan != nil && query != "" && strings.Contains(normalized, plan.match) {
			c.matched = true
		}
		cut := plan != nil && c.matched && normalized == "commit"
		if cut {
			c.proxy.plan = nil
			c.matched = false
		}
		c.proxy.mu.Unlock()
		if cut {
			if plan.mode == "before_commit" {
				plan.done <- "before_commit"
				c.close()
				return
			}
			c.mu.Lock()
			c.pending = plan
			c.mu.Unlock()
		}
		if _, err = c.server.Write(frame); err != nil {
			return
		}
	}
}
func (c *independentPgwireConnection) backend() {
	for {
		kind, payload, frame, err := independentPgwireFrame(c.server)
		if err != nil {
			return
		}
		c.mu.Lock()
		plan := c.pending
		if plan != nil && kind == 'C' && bytes.Equal(payload, []byte("COMMIT\x00")) {
			c.pending = nil
			c.mu.Unlock()
			plan.done <- "after_commit"
			c.close()
			return
		}
		if plan != nil && kind == 'E' {
			c.pending = nil
			c.mu.Unlock()
			plan.done <- "database_rejected_commit"
		} else {
			c.mu.Unlock()
		}
		if _, err = c.client.Write(frame); err != nil {
			return
		}
	}
}

// Kept deliberately small: this control validates the proxy's actual transport
// behavior independently of a model/tool runtime or its cleanup implementation.
func TestIndependentPgwireCommitCutControl(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			s, dsn := testStore(t)
			victim, proxy := independentCommitProxyStore(t, dsn)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := s.Pool.Exec(ctx, "CREATE TABLE independent_ack_control(id integer PRIMARY KEY,value integer NOT NULL)"); err != nil {
				t.Fatal(err)
			}
			plan := proxy.arm(t, "insert into independent_ack_control", mode)
			tx, err := victim.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err = tx.Exec(ctx, "INSERT INTO independent_ack_control(id,value) VALUES($1,$2)", 1, 17); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err == nil {
				t.Fatal("real pgx commit unexpectedly acknowledged through the cut")
			}
			proxy.wait(t, plan)
			var count int
			if err = s.Pool.QueryRow(ctx, "SELECT count(*) FROM independent_ack_control").Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode == "after_commit" {
				want = 1
			}
			if count != want {
				t.Fatal("wire boundary did not match durable PostgreSQL outcome", mode, count, want)
			}
			if _, err = victim.Pool.Exec(ctx, "INSERT INTO independent_ack_control(id,value) VALUES($1,$2)", 2, 23); err != nil {
				t.Fatal("pgx pool did not recover after closed wire connection", err)
			}
			if _, err = s.Pool.Exec(ctx, "DROP TABLE independent_ack_control"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
