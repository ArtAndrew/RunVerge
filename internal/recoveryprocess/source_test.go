// SPDX-License-Identifier: Elastic-2.0
package recoveryprocess

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sourceFixture(t *testing.T) (*Client, *engineFixture, Intent) {
	t.Helper()
	c, e := fixture(t)
	members := map[string]any{}
	for id, r := range e.rows {
		service := r.Config.Labels["com.docker.compose.service"]
		r.State.StartedAt = "2026-11-01T00:00:00Z"
		address := "172.19.0." + strings.TrimLeft(id, "0")
		endpoint := strings.Repeat("d", 63) + strings.TrimLeft(id, "0")
		raw, _ := json.Marshal(map[string]any{"Networks": map[string]any{"synthetic_default": map[string]any{"NetworkID": strings.Repeat("c", 64), "EndpointID": endpoint, "IPAddress": address, "Aliases": []string{service}}}})
		if json.Unmarshal(raw, &r.NetworkSettings) != nil {
			t.Fatal("fixture network")
		}
		r.Config.Image = sourceDatabaseImage
		r.Config.Entrypoint = []string{"/app/bin/platform"}
		r.Config.Env = []string{"DATABASE_URL=postgres://runverge:synthetic@db:5432/runverge?sslmode=disable", "RUNVERGE_RECOVERY_CONTROL=container_v1"}
		if service == "api" {
			r.Config.Cmd = []string{"serve", "--container-preview", "--listen", "0.0.0.0:8080"}
		} else if service == "worker" {
			r.Config.Cmd = []string{"worker", "--concurrency", "1"}
		}
		e.rows[id] = r
		members[id] = map[string]any{"EndpointID": endpoint, "IPv4Address": address + "/16"}
	}
	e.network = map[string]any{"ID": strings.Repeat("c", 64), "Name": "synthetic_default", "Driver": "bridge", "Scope": "local", "Labels": map[string]string{"com.docker.compose.project": "synthetic"}, "Containers": members}
	p, err := c.Plan(context.Background(), "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	return c, e, p
}
func TestRecoveryProcessSourceProfileRejectsIndirectOrUnknownExecutors(t *testing.T) {
	for _, fault := range []string{"none", "missing_endpoint", "wrong_endpoint", "wrong_address", "foreign_peer", "extra_exec", "db_alias", "indirect_database", "pg_override", "wrong_entrypoint", "restarted"} {
		t.Run(fault, func(t *testing.T) {
			c, e, p := sourceFixture(t)
			worker := p.Containers[2].ID
			r := e.rows[worker]
			members := e.network["Containers"].(map[string]any)
			switch fault {
			case "missing_endpoint":
				delete(members, worker)
			case "wrong_endpoint":
				members[worker].(map[string]any)["EndpointID"] = strings.Repeat("e", 64)
			case "wrong_address":
				members[worker].(map[string]any)["IPv4Address"] = "172.19.0.99/16"
			case "foreign_peer":
				members[strings.Repeat("f", 64)] = map[string]any{}
			case "extra_exec":
				r.ExecIDs = []string{strings.Repeat("e", 64)}
			case "db_alias":
				ep := r.NetworkSettings.Networks["synthetic_default"]
				ep.Aliases = append(ep.Aliases, "db")
				r.NetworkSettings.Networks["synthetic_default"] = ep
			case "indirect_database":
				r.Config.Env[0] = "DATABASE_URL=postgres://runverge:synthetic@proxy:5432/runverge?sslmode=disable"
			case "pg_override":
				r.Config.Env = append(r.Config.Env, "PGHOST=elsewhere")
			case "wrong_entrypoint":
				r.Config.Entrypoint = []string{"/untrusted"}
			case "restarted":
				r.State.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
			}
			e.rows[worker] = r
			_, _, err := c.sourceProfile(context.Background(), p, "sha256:"+strings.Repeat("a", 64))
			if (err == nil) != (fault == "none") {
				t.Fatalf("fault %s accepted=%t", fault, err == nil)
			}
			if len(e.posted()) != 0 {
				t.Fatal("profile check mutated container")
			}
		})
	}
}
func TestRecoveryProcessBoundObservationRejectsRestartAndKeepsExternalUnknown(t *testing.T) {
	c, e, p := sourceFixture(t)
	b := SourceBinding{Version: 1, PlanSHA256: planDigest(p), SourceID: "inst_" + strings.Repeat("a", 32), SnapshotSHA256: strings.Repeat("b", 64), RuntimeImage: p.Containers[1].Image, DatabaseContainer: p.Containers[0].ID, DatabaseOID: 123, SystemID: "synthetic", ObservedAt: time.Now()}
	for _, app := range p.Containers[1:] {
		b.Processes = append(b.Processes, SourceProcess{app.ID, strings.Repeat("c", 32)})
	}
	if _, err := c.Stop(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	ob, err := c.InspectBoundSource(context.Background(), p, b)
	if err != nil || !ob.BoundSourceProcessesStopped || !ob.SourceInstallationBound || ob.ExternalExecutionFenced || ob.ExternalEffectsSettled {
		t.Fatal("incorrect bounded observation", err)
	}
	e.mu.Lock()
	r := e.rows[p.Containers[2].ID]
	r.RestartCount++
	e.rows[r.ID] = r
	e.mu.Unlock()
	if _, err = c.InspectBoundSource(context.Background(), p, b); err == nil {
		t.Fatal("restarted process accepted")
	}
}
func TestRecoveryProcessWitnessStreamRejectsTruncationAndStderr(t *testing.T) {
	frame := func(channel byte, body string) []byte {
		raw := make([]byte, 8)
		raw[0] = channel
		binary.BigEndian.PutUint32(raw[4:], uint32(len(body)))
		return append(raw, body...)
	}
	good := append(frame(1, "first"), frame(1, "second")...)
	if out, err := witnessOutput(good); err != nil || string(out) != "firstsecond" {
		t.Fatal("valid stream")
	}
	for _, raw := range [][]byte{frame(2, "private error"), good[:len(good)-1], append(good, 1), frame(1, strings.Repeat("x", 8193))} {
		if out, err := witnessOutput(raw); err == nil || out != nil {
			t.Fatal("invalid stream accepted")
		}
	}
}
