// SPDX-License-Identifier: Elastic-2.0
package recoveryprocess

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestObservedSourceStopRejectsDriftWithoutMutation(t *testing.T) {
	for _, fault := range []string{"none", "restart_then_stop", "started_at", "added", "missing", "daemon", "restart_policy", "partial_stop", "cancelled"} {
		t.Run(fault, func(t *testing.T) {
			c, e, p := sourceFixture(t)
			if _, err := c.Stop(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			// Read-only observation must not inherit destructive stop's 15m age limit.
			p.CreatedAt = time.Now().UTC().Add(-24 * time.Hour)
			b := SourceBinding{Version: 1, PlanSHA256: planDigest(p), SourceID: "inst_" + strings.Repeat("a", 32), SnapshotSHA256: strings.Repeat("b", 64), RuntimeImage: p.Containers[1].Image, DatabaseContainer: p.Containers[0].ID, DatabaseOID: 123, SystemID: "12345", ObservedAt: p.CreatedAt.Add(time.Second)}
			for _, app := range p.Containers[1:] {
				b.Processes = append(b.Processes, SourceProcess{app.ID, strings.Repeat("c", 32)})
			}
			prior, err := c.ObserveSourceStop(context.Background(), p, b)
			if err != nil {
				t.Fatal(err)
			}
			x := prior.Record()
			x.Intent.Containers[0].ID = "mutated"
			x.Binding.Processes[0].BootID = "mutated"
			x.Observation.Containers[0].Stopped = false
			if !SourceStopRecordConsistent(prior.Record()) {
				t.Fatal("caller mutated opaque observation")
			}
			posts := len(e.posted())
			e.mu.Lock()
			r := e.rows[p.Containers[2].ID]
			switch fault {
			case "restart_then_stop":
				r.RestartCount++
			case "started_at":
				r.State.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
			case "added":
				e.rows[strings.Repeat("f", 64)] = r
			case "missing":
				delete(e.rows, p.Containers[0].ID)
			case "daemon":
				e.daemon = "different-daemon"
			case "restart_policy":
				r.HostConfig.RestartPolicy.Name = "always"
			case "partial_stop":
				r.State.Running = true
				r.State.Status = "running"
				r.State.Pid = 10
			}
			e.rows[r.ID] = r
			e.mu.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if fault == "cancelled" {
				cancel()
			}
			fresh, err := c.ObserveSourceStop(ctx, p, b)
			if fault == "none" {
				if err != nil || !SameSourceStop(prior.Record(), fresh.Record()) {
					t.Fatal("unchanged source rejected", err)
				}
			} else if err == nil || SourceStopRecordConsistent(fresh.Record()) {
				t.Fatal("drift returned a fresh proof")
			}
			if len(e.posted()) != posts {
				t.Fatal("observation mutated Docker state")
			}
		})
	}
}
