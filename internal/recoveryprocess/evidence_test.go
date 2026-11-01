// SPDX-License-Identifier: Elastic-2.0
package recoveryprocess

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRecoveryProcessEvidenceKeepsDatabaseUntilFinalStop(t *testing.T) {
	c, e, p := sourceFixture(t)
	b := SourceBinding{Version: 1, PlanSHA256: planDigest(p), SourceID: "inst_" + strings.Repeat("a", 32), SnapshotSHA256: strings.Repeat("b", 64), RuntimeImage: p.Containers[1].Image, DatabaseContainer: p.Containers[0].ID, DatabaseOID: 123, SystemID: "synthetic", ObservedAt: time.Now()}
	for _, app := range p.Containers[1:] {
		b.Processes = append(b.Processes, SourceProcess{app.ID, strings.Repeat("c", 32)})
	}
	r := e.rows[b.DatabaseContainer]
	r.Config.Env = []string{"POSTGRES_USER=runverge_admin", "POSTGRES_DB=runverge", "POSTGRES_PASSWORD=synthetic-only"}
	e.rows[r.ID] = r
	ctx := context.Background()
	if _, err := c.EvidenceDatabaseURL(ctx, p, b); err == nil {
		t.Fatal("capture before stopping executors")
	}
	if _, err := c.StopDatabaseAfterEvidence(ctx, p); err == nil || len(e.posted()) != 0 {
		t.Fatal("database stopped before executors")
	}
	if _, err := c.PreflightEvidenceDatabaseURL(ctx, p, b); err != nil {
		t.Fatal(err)
	}
	o, err := c.StopExecutors(ctx, p)
	if err != nil || o.ProjectProcessesStopped || o.ExternalEffectsSettled {
		t.Fatal("wrong partial-stop observation", err)
	}
	for _, post := range e.posted() {
		if post == b.DatabaseContainer+"/stop" {
			t.Fatal("database stopped before capture")
		}
	}
	if _, err = c.EvidenceDatabaseURL(ctx, p, b); err != nil {
		t.Fatal(err)
	}
	// A new unknown bridge peer prevents evidence, even with exact container IDs.
	members := e.network["Containers"].(map[string]any)
	members[strings.Repeat("f", 64)] = map[string]any{}
	if _, err = c.EvidenceDatabaseURL(ctx, p, b); err == nil {
		t.Fatal("foreign peer accepted")
	}
	delete(members, strings.Repeat("f", 64))
	o, err = c.StopDatabaseAfterEvidence(ctx, p)
	if err != nil || !o.ProjectProcessesStopped || o.ExternalExecutionFenced {
		t.Fatal("final stop", err)
	}
	if _, err = c.EvidenceDatabaseURL(ctx, p, b); err == nil {
		t.Fatal("stopped database accepted")
	}
}
