// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
)

// Construction helpers are shared with the author suites. These independent
// oracles exercise the public review path against real nested PG/HTTP work.
func TestIndependentCollaborationNestedDecisionClosure(t *testing.T) {
	for _, decision := range []string{"accept", "reject", "request_changes"} {
		t.Run(decision, func(t *testing.T) {
			ctx := context.Background()
			model := independentModelSetup(t, false, nil)
			if _, e := model.s.CancelTask(ctx, model.p, model.task.ID, domain.NewID("key")); e != nil {
				t.Fatal(e)
			}
			leaf := domain.Agent{ID: model.task.AgentID, Version: model.task.AgentVersion}
			inner := teamAgent(t, model.s, model.p, "parallel", "all", 2, teamMember("first", leaf), teamMember("second", leaf))
			outer := teamAgent(t, model.s, model.p, "parallel", "all", 2, teamMember("nested", inner), teamMember("last", leaf))
			model.task = task(t, model.s, model.p, outer)
			preacceptedTask, preacceptedDecision := "", ""
			for i := 0; i < 100; i++ {
				teamTick(t, model.s)
				lease, _, e := model.s.ClaimModel(ctx, "independent-team-review", 30*time.Second)
				if e != nil {
					t.Fatal(e)
				}
				if lease != nil {
					out, e := model.runtime(t, model.s).Execute(ctx, lease)
					if e != nil || out.Status != modelruntime.CandidateWaitingInput {
						t.Fatalf("team fixture execution: %s %v", out.Status, e)
					}
					if preacceptedTask == "" {
						childModel := &independentModelFixture{s: model.s, p: model.p, task: lease.Task}
						child := &independentCollaborationFixture{independentModelFixture: childModel}
						child.refresh(t)
						if _, e = model.s.ReviewTask(ctx, model.p, child.task.ID, domain.NewID("key"), child.review("accept")); e != nil {
							t.Fatal(e)
						}
						got, e := model.s.GetTask(ctx, model.p, child.task.ID)
						if e != nil || got.AcceptedDecisionID == "" {
							t.Fatal("pre-consumption child decision missing")
						}
						preacceptedTask, preacceptedDecision = got.ID, got.AcceptedDecisionID
					}
				}
				current, e := model.s.GetTask(ctx, model.p, model.task.ID)
				if e != nil {
					t.Fatal(e)
				}
				if current.CandidateID != "" {
					break
				}
			}
			f := &independentCollaborationFixture{independentModelFixture: model}
			f.refresh(t)
			if model.calls.Load() != 3 || f.current.Status != "waiting_input" || f.current.ReservedMicroUSD != 450 || f.current.SpentMicroUSD != 0 {
				t.Fatal("nested real HTTP work did not retain three unknown liabilities")
			}
			before := f.money(t)
			var descendantMoney string
			if e := f.s.Pool.QueryRow(ctx, "SELECT jsonb_agg(jsonb_build_array(id,reserved_microusd,spent_microusd,financial_hold) ORDER BY id)::text FROM tasks WHERE organization_id=$1 AND root_id=$2", f.p.OrganizationID, f.task.ID).Scan(&descendantMoney); e != nil {
				t.Fatal(e)
			}
			var selectedIDs []string
			rows, e := f.s.Pool.Query(ctx, "SELECT child_task_id FROM team_members WHERE organization_id=$1 AND selected_candidate_id IS NOT NULL ORDER BY child_task_id", f.p.OrganizationID)
			if e != nil {
				t.Fatal(e)
			}
			for rows.Next() {
				var id string
				if e = rows.Scan(&id); e != nil {
					t.Fatal(e)
				}
				selectedIDs = append(selectedIDs, id)
			}
			e = rows.Err()
			rows.Close()
			if e != nil || len(selectedIDs) != 4 {
				t.Fatal("nested selected closure does not contain four exact descendants")
			}
			for _, id := range selectedIDs {
				if id == preacceptedTask {
					continue
				}
				childModel := &independentModelFixture{s: model.s, p: model.p, task: domain.Task{ID: id}}
				child := &independentCollaborationFixture{independentModelFixture: childModel}
				child.refresh(t)
				_, e = f.s.ReviewTask(ctx, f.p, id, domain.NewID("key"), child.review("reject"))
				code(t, e, "conflict")
				_, e = f.s.CancelTask(ctx, f.p, id, domain.NewID("key"))
				code(t, e, "conflict")
			}
			// Failed inheritance must roll back the parent decision as well. The
			// previously accepted child's independent receipt must survive either way.
			if decision == "accept" {
				if _, e = f.s.Pool.Exec(ctx, `CREATE FUNCTION independent_inherited_event_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.type='task.decision_inherited' THEN RAISE EXCEPTION 'independent inherited outbox failure'; END IF; RETURN NEW; END $$`); e != nil {
					t.Fatal(e)
				}
				if _, e = f.s.Pool.Exec(ctx, "CREATE TRIGGER independent_inherited_event_failure BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION independent_inherited_event_failure()"); e != nil {
					t.Fatal(e)
				}
				if _, e = f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), f.review(decision)); e == nil {
					t.Fatal("inherited event fault did not roll back tree review")
				}
				var n int
				if e = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM task_decisions WHERE organization_id=$1", f.p.OrganizationID).Scan(&n); e != nil || n != 1 {
					t.Fatal("failed inherited decision leaked receipts")
				}
				fresh, e := f.s.GetTask(ctx, f.p, f.task.ID)
				if e != nil || fresh.Version != f.current.Version || fresh.Status != "waiting_input" {
					t.Fatal("failed inheritance changed parent state")
				}
				if _, e = f.s.Pool.Exec(ctx, "DROP TRIGGER independent_inherited_event_failure ON events"); e != nil {
					t.Fatal(e)
				}
			}
			if _, e = f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), f.review(decision)); e != nil {
				t.Fatal(e)
			}
			rootDecisions, _, e := f.s.Decisions(ctx, f.p, f.task.ID, "", 100)
			if e != nil || len(rootDecisions) != 1 {
				t.Fatal("root decision receipt missing")
			}
			rootDecision := rootDecisions[0]
			for _, id := range selectedIDs {
				child, e := f.s.GetTask(ctx, f.p, id)
				if e != nil {
					t.Fatal(e)
				}
				ds, _, e := f.s.Decisions(ctx, f.p, id, "", 100)
				if e != nil || len(ds) != 1 {
					t.Fatal("selected descendant has missing/duplicate receipt")
				}
				d := ds[0]
				if id == preacceptedTask {
					if child.Status != "succeeded" || d.ID != preacceptedDecision || child.AcceptedDecisionID != preacceptedDecision || d.Method != "authorized_user_decision" || d.ParentDecisionID != "" || d.Decision != "accept" {
						t.Fatal("parent decision rewrote independently accepted child")
					}
					continue
				}
				if d.ParentDecisionID != rootDecision.ID || d.Method != "inherited_authorized_user_decision" || d.Decision != decision || d.CandidateID != child.CandidateID || d.RunID != child.CurrentRunID || d.PrincipalID != f.p.ID {
					t.Fatal("inherited decision lost exact snapshot/authority/root receipt")
				}
				wanted := "failed"
				if decision == "accept" {
					wanted = "succeeded"
				}
				if child.Status != wanted {
					t.Fatalf("inherited child state=%s want=%s", child.Status, wanted)
				}
				var bound bool
				if e = f.s.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM team_members WHERE organization_id=$1 AND child_task_id=$2 AND selected_candidate_id=$3 AND child_run_id=$4 AND selected_artifact_id=$5 AND selected_artifact_version=$6 AND selected_content_digest=$7)", f.p.OrganizationID, id, d.CandidateID, d.RunID, d.ArtifactID, d.ArtifactVersion, d.ContentDigest).Scan(&bound); e != nil || !bound {
					t.Fatal("decision differs from selected immutable tuple")
				}
			}
			if f.money(t) != before || model.calls.Load() != 3 {
				t.Fatal("nested decision altered finances or triggered external work")
			}
			var after string
			if e = f.s.Pool.QueryRow(ctx, "SELECT jsonb_agg(jsonb_build_array(id,reserved_microusd,spent_microusd,financial_hold) ORDER BY id)::text FROM tasks WHERE organization_id=$1 AND root_id=$2", f.p.OrganizationID, f.task.ID).Scan(&after); e != nil || after != descendantMoney {
				t.Fatal("nested decision changed descendant finances")
			}
		})
	}
}
