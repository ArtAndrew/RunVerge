// SPDX-License-Identifier: Elastic-2.0
package recoveryprocess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type engineFixture struct {
	mu        sync.Mutex
	rows      map[string]inspect
	daemon    string
	posts     []string
	drop      string
	dropAfter bool
}

func fixture(t *testing.T) (*Client, *engineFixture) {
	t.Helper()
	e := &engineFixture{rows: map[string]inspect{}, daemon: "trusted-local-daemon"}
	for i, service := range []string{"db", "api", "worker"} {
		raw := fmt.Sprintf(`{"Id":%q,"Name":%q,"Created":"2026-10-30T00:00:00Z","Image":%q,"Config":{"User":"10001:10001","Labels":{"com.docker.compose.project":"synthetic","com.docker.compose.service":%q}},"State":{"Status":"running","Running":true,"Pid":10},"HostConfig":{"ReadonlyRootfs":true,"CapDrop":["ALL"],"RestartPolicy":{"Name":"always"}}}`, fmt.Sprintf("%064d", i+1), "/synthetic-"+service, "sha256:"+strings.Repeat("a", 64), service)
		var r inspect
		if json.Unmarshal([]byte(raw), &r) != nil {
			t.Fatal("fixture")
		}
		e.rows[r.ID] = r
	}
	srv := httptest.NewServer(http.HandlerFunc(e.serve))
	t.Cleanup(srv.Close)
	transport := srv.Client().Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	c := &Client{http: &http.Client{Transport: rewriteTransport{base: srv.URL, transport: transport}}}
	return c, e
}

type rewriteTransport struct {
	base      string
	transport http.RoundTripper
}

func (r rewriteTransport) RoundTrip(q *http.Request) (*http.Response, error) {
	clone := q.Clone(q.Context())
	u := *q.URL
	clone.URL = &u
	clone.URL.Host = strings.TrimPrefix(r.base, "http://")
	return r.transport.RoundTrip(clone)
}
func (e *engineFixture) serve(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1.45")
	w.Header().Set("Content-Type", "application/json")
	if path == "/info" {
		json.NewEncoder(w).Encode(map[string]any{"ID": e.daemon, "OSType": "linux", "Swarm": map[string]string{"LocalNodeState": "inactive"}})
		return
	}
	if path == "/containers/json" {
		if r.URL.Query().Get("all") != "1" || !strings.Contains(r.URL.Query().Get("filters"), "com.docker.compose.project=synthetic") {
			w.WriteHeader(400)
			return
		}
		rows := []map[string]string{}
		for id := range e.rows {
			rows = append(rows, map[string]string{"Id": id})
		}
		json.NewEncoder(w).Encode(rows)
		return
	}
	parts := strings.Split(path, "/")
	if len(parts) != 4 {
		w.WriteHeader(404)
		return
	}
	id, action := parts[2], parts[3]
	row, ok := e.rows[id]
	if !ok {
		w.WriteHeader(404)
		return
	}
	if r.Method == "GET" && action == "json" {
		json.NewEncoder(w).Encode(row)
		return
	}
	if r.Method != "POST" {
		w.WriteHeader(400)
		return
	}
	e.posts = append(e.posts, id+"/"+action)
	drop := e.drop == id+"/"+action
	if !drop || e.dropAfter {
		switch action {
		case "update":
			var body struct {
				RestartPolicy struct {
					Name              string
					MaximumRetryCount int
				}
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.RestartPolicy.Name != "no" || body.RestartPolicy.MaximumRetryCount != 0 {
				w.WriteHeader(400)
				return
			}
			row.HostConfig.RestartPolicy.Name = "no"
			row.HostConfig.RestartPolicy.MaximumRetryCount = 0
		case "stop":
			if r.URL.Query().Get("t") != "0" || r.URL.Query().Get("signal") != "SIGKILL" {
				w.WriteHeader(400)
				return
			}
			row.State.Status = "exited"
			row.State.Running = false
			row.State.Pid = 0
		default:
			w.WriteHeader(400)
			return
		}
	}
	if drop {
		e.drop = ""
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
		return
	}
	if action == "stop" {
		w.WriteHeader(204)
	} else {
		w.Write([]byte(`{"Warnings":null}`))
	}
}

func (e *engineFixture) posted() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.posts)
}

func TestRecoveryProcessShutdownAndFreshInspection(t *testing.T) {
	c, e := fixture(t)
	ctx := context.Background()
	p, err := c.Plan(ctx, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if len(e.posted()) != 0 {
		t.Fatal("planning mutated engine")
	}
	before, err := c.Inspect(ctx, p)
	if err != nil || before.ProjectProcessesStopped {
		t.Fatal("initial state")
	}
	o, err := c.Stop(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if !o.ProjectProcessesStopped || o.SourceInstallationBound || o.ExternalExecutionFenced || o.ExternalEffectsSettled {
		t.Fatal("scope overclaim")
	}
	want := []string{p.Containers[0].ID + "/update", p.Containers[1].ID + "/update", p.Containers[2].ID + "/update", p.Containers[2].ID + "/stop", p.Containers[1].ID + "/stop", p.Containers[0].ID + "/stop"}
	if !slices.Equal(e.posted(), want) {
		t.Fatal("unexpected mutation order")
	}
	fresh := &Client{http: c.http}
	o, err = fresh.Inspect(ctx, p)
	if err != nil || !o.ProjectProcessesStopped || len(e.posted()) != len(want) {
		t.Fatal("fresh inspect")
	}
}

func TestRecoveryProcessUnknownAcknowledgmentDoesNotRetry(t *testing.T) {
	for _, action := range []string{"update", "stop"} {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/after_%t", action, after), func(t *testing.T) {
				c, e := fixture(t)
				p, err := c.Plan(context.Background(), "synthetic")
				if err != nil {
					t.Fatal(err)
				}
				// DB is first update and last stop. Last-stop loss can leave everything
				// stopped, but the mutation call still must return unknown.
				e.mu.Lock()
				e.drop = p.Containers[0].ID + "/" + action
				e.dropAfter = after
				e.mu.Unlock()
				o, err := c.Stop(context.Background(), p)
				if !errors.Is(err, ErrUnknown) || o.ProjectProcessesStopped || o.OperationID != "" {
					t.Fatal("uncertain success")
				}
				posts := len(e.posted())
				wantPosts := 1
				if action == "stop" {
					wantPosts = 6
				}
				if posts != wantPosts {
					t.Fatal("unexpected retry or later mutation")
				}
				o, err = (&Client{http: c.http}).Inspect(context.Background(), p)
				if err != nil || o.ProjectProcessesStopped != (action == "stop" && after) || len(e.posted()) != posts {
					t.Fatal("fresh inspection did not resolve observed state")
				}
			})
		}
	}
}

func TestRecoveryProcessRefusesChangedOrUnsafeInventory(t *testing.T) {
	for _, change := range []string{"extra", "missing", "daemon", "name", "image", "paused", "restarting", "dead", "privileged", "device_rules", "hostpid", "expired", "duplicate"} {
		t.Run(change, func(t *testing.T) {
			c, e := fixture(t)
			p, err := c.Plan(context.Background(), "synthetic")
			if err != nil {
				t.Fatal(err)
			}
			id := p.Containers[0].ID
			e.mu.Lock()
			r := e.rows[id]
			switch change {
			case "extra":
				e.rows[strings.Repeat("f", 64)] = r
			case "missing":
				delete(e.rows, id)
			case "daemon":
				e.daemon = "another-daemon"
			case "name":
				r.Name = "/replacement"
				e.rows[id] = r
			case "image":
				r.Image = "sha256:" + strings.Repeat("b", 64)
				e.rows[id] = r
			case "paused":
				r.State.Paused = true
				r.State.Status = "paused"
			case "restarting":
				r.State.Restarting = true
				r.State.Status = "restarting"
			case "dead":
				r.State.Dead = true
			case "privileged":
				r.HostConfig.Privileged = true
			case "device_rules":
				r.HostConfig.DeviceCgroupRules = []string{"c *:* rwm"}
			case "hostpid":
				r.HostConfig.PidMode = "host"
			case "expired":
				p.CreatedAt = time.Now().Add(-16 * time.Minute)
			case "duplicate":
				p.Containers[1] = p.Containers[0]
			}
			e.mu.Unlock()
			if _, err = c.Stop(context.Background(), p); err == nil || len(e.posted()) != 0 {
				t.Fatal("unsafe mutation")
			}
		})
	}
}
