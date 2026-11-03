// SPDX-License-Identifier: Elastic-2.0
package recoveryprocess

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestObservedSourceCaptureRequiresStoppedExecutorsAndRunningDatabase(t *testing.T) {
	for _, fault := range []string{"none", "running_worker", "stopped_database", "restart", "wrong_binding", "cancelled"} {
		t.Run(fault, func(t *testing.T) {
			c, e, p := sourceFixture(t)
			b := SourceBinding{Version: 1, PlanSHA256: planDigest(p), SourceID: "inst_" + strings.Repeat("a", 32), SnapshotSHA256: strings.Repeat("b", 64), RuntimeImage: p.Containers[1].Image, DatabaseContainer: p.Containers[0].ID, SystemID: "12345", DatabaseOID: 123, ObservedAt: time.Now().UTC()}
			for _, app := range p.Containers[1:] {
				b.Processes = append(b.Processes, SourceProcess{app.ID, strings.Repeat("c", 32)})
			}
			if _, err := c.StopExecutors(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			posts := len(e.posted())
			e.mu.Lock()
			worker := e.rows[p.Containers[2].ID]
			switch fault {
			case "running_worker":
				worker.State.Running = true
				worker.State.Status = "running"
				worker.State.Pid = 10
			case "stopped_database":
				db := e.rows[p.Containers[0].ID]
				db.State.Running = false
				db.State.Status = "exited"
				db.State.Pid = 0
				e.rows[db.ID] = db
			case "restart":
				worker.RestartCount++
			case "wrong_binding":
				b.DatabaseOID = 0
			}
			e.rows[worker.ID] = worker
			e.mu.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if fault == "cancelled" {
				cancel()
			}
			got, err := c.ObserveSourceCapture(ctx, p, b)
			if fault == "none" {
				if err != nil || !SourceCaptureRecordConsistent(got.Record()) || SourceStopRecordConsistent(got.Record()) {
					t.Fatal("capture state rejected or treated as final stop", err)
				}
				copy := got.Record()
				copy.Observation.Containers[1].Stopped = false
				copy.Binding.Processes[0].BootID = "changed"
				if !SourceCaptureRecordConsistent(got.Record()) {
					t.Fatal("opaque observation mutated")
				}
			} else if err == nil || SourceCaptureRecordConsistent(got.Record()) {
				t.Fatal("unsafe capture state accepted")
			}
			if len(e.posted()) != posts {
				t.Fatal("capture observation mutated processes")
			}
		})
	}
}
