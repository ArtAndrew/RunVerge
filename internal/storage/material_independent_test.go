// SPDX-License-Identifier: Elastic-2.0
package storage

// Independent 0.5 tests. Local HTTP model output is only an unverified fixture;
// financial statements below are explicitly trusted internal test evidence.
import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
	"github.com/jackc/pgx/v5/pgconn"
)

func independentMaterialCreate(t *testing.T, s *Store, p domain.Principal, taskID, content, base string) domain.Material {
	t.Helper()
	raw, e := s.CreateMaterial(context.Background(), p, taskID, domain.NewID("key"), domain.CreateMaterial{Name: "Independent shared draft", MediaType: "text/plain", Content: content, BaseCandidateID: base})
	if e != nil {
		t.Fatal(e)
	}
	var m domain.Material
	if e = json.Unmarshal(raw, &m); e != nil || m.ID == "" || m.TaskID != taskID || m.OrganizationID != p.OrganizationID || m.HeadVersion != 1 || m.HeadContentDigest != Hash([]byte(content)) || m.CreatedBy != p.ID {
		t.Fatalf("invalid material creation receipt: %v", e)
	}
	return m
}
func independentMaterialRevise(t *testing.T, s *Store, p domain.Principal, taskID string, m domain.Material, content string) domain.Material {
	t.Helper()
	raw, e := s.ReviseMaterial(context.Background(), p, taskID, m.ID, domain.NewID("key"), domain.ReviseMaterial{ExpectedVersion: m.HeadVersion, Content: content, Reason: "Explicit independent correction"})
	if e != nil {
		t.Fatal(e)
	}
	var next domain.Material
	if e = json.Unmarshal(raw, &next); e != nil || next.HeadVersion != m.HeadVersion+1 || next.HeadContentDigest != Hash([]byte(content)) {
		t.Fatal("revision head mismatch")
	}
	return next
}
func independentMaterialPublish(t *testing.T, f *independentCollaborationFixture, m domain.Material) domain.Task {
	t.Helper()
	raw, e := f.s.PublishMaterial(context.Background(), f.p, f.task.ID, m.ID, domain.NewID("key"), domain.PublishMaterial{ExpectedTaskVersion: f.current.Version, ExpectedMaterialVersion: m.HeadVersion, ContentDigest: m.HeadContentDigest, ParentCandidateID: f.candidate.ID})
	if e != nil {
		t.Fatal(e)
	}
	var next domain.Task
	if e = json.Unmarshal(raw, &next); e != nil || next.ID != f.task.ID {
		t.Fatal("publication task missing")
	}
	return next
}
func independentMaterialAssertSameTask(t *testing.T, a, b domain.Task) {
	t.Helper()
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if string(x) != string(y) {
		t.Fatal("draft mutation changed task snapshot")
	}
}

func TestIndependentMaterialReadsReauthorizeHistoricalPrincipal(t *testing.T) {
	for _, kind := range []string{"token", "expiry", "membership", "principal", "browser_session", "browser_mapping"} {
		t.Run(kind, func(t *testing.T) {
			f := independentCollaborationSetup(t)
			ctx := context.Background()
			m := independentMaterialCreate(t, f.s, f.p, f.task.ID, "private revision bytes", f.candidate.ID)
			p := f.p
			if kind == "browser_session" || kind == "browser_mapping" {
				session := independentSession(t, f.s, p, "material-read-authority", time.Hour)
				var e error
				p, e = f.s.AuthenticateBrowserSession(ctx, session.TokenHash, p.OrganizationID)
				if e != nil {
					t.Fatal(e)
				}
			}
			reads := []struct {
				name string
				read func() error
			}{
				{"materials", func() error { _, _, e := f.s.Materials(ctx, p, f.task.ID, "", 10); return e }},
				{"head", func() error { _, e := f.s.GetMaterial(ctx, p, f.task.ID, m.ID); return e }},
				{"revisions", func() error { _, _, e := f.s.MaterialRevisions(ctx, p, f.task.ID, m.ID, "", 10); return e }},
				{"exact_revision", func() error { _, e := f.s.MaterialVersion(ctx, p, f.task.ID, m.ID, 1); return e }},
				{"exact_artifact", func() error {
					_, e := f.s.ArtifactVersion(ctx, p, f.task.ID, f.candidate.ArtifactID, f.candidate.ArtifactVersion)
					return e
				}},
				{"artifacts_page", func() error { _, _, e := f.s.ArtifactsPage(ctx, p, f.task.ID, "", 10); return e }},
			}
			// Caller-supplied role is never authority: current DB membership wins.
			p.Role = "not-a-real-role"
			for _, item := range reads {
				if e := item.read(); e != nil {
					t.Fatalf("current authorized %s failed: %v", item.name, e)
				}
			}
			var e error
			switch kind {
			case "token":
				_, e = f.s.Pool.Exec(ctx, "UPDATE access_tokens SET revoked=true WHERE token_hash=$1", p.TokenHash)
			case "expiry":
				_, e = f.s.Pool.Exec(ctx, "UPDATE access_tokens SET expires_at=clock_timestamp()-interval '1 second' WHERE token_hash=$1", p.TokenHash)
			case "membership":
				_, e = f.s.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", p.OrganizationID, p.ID)
			case "principal":
				_, e = f.s.Pool.Exec(ctx, "UPDATE principals SET active=false WHERE id=$1", p.ID)
			case "browser_session":
				e = f.s.RevokeSession(ctx, p.TokenHash)
			case "browser_mapping":
				_, e = f.s.Pool.Exec(ctx, "UPDATE identity_mappings SET active=false WHERE principal_id=$1", p.ID)
			}
			if e != nil {
				t.Fatal(e)
			}
			p.Role = "admin"
			for _, item := range reads {
				code(t, item.read(), "unauthenticated")
			}
		})
	}
}

func TestIndependentMaterialRevisionCASAndReplay(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx := context.Background()
	money := f.money(t)
	taskBefore := f.current
	m := independentMaterialCreate(t, f.s, f.p, f.task.ID, "initial exact bytes\n", f.candidate.ID)
	key := domain.NewID("key")
	request := domain.ReviseMaterial{ExpectedVersion: 1, Content: "second version", Reason: "same command replay"}
	var wg sync.WaitGroup
	errors := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := f.s.ReviseMaterial(ctx, f.p, f.task.ID, m.ID, key, request)
			errors <- e
		}()
	}
	wg.Wait()
	close(errors)
	for e := range errors {
		if e != nil {
			t.Fatal(e)
		}
	}
	changed := request
	changed.Content = "different bytes same key"
	_, e := f.s.ReviseMaterial(ctx, f.p, f.task.ID, m.ID, key, changed)
	code(t, e, "conflict")
	admin := independentCollaborationActor(t, f.s, f.p.OrganizationID, "admin")
	type outcome struct {
		actor string
		err   error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for _, actor := range []domain.Principal{f.p, admin} {
		go func(p domain.Principal) {
			<-start
			_, e := f.s.ReviseMaterial(ctx, p, f.task.ID, m.ID, domain.NewID("key"), domain.ReviseMaterial{ExpectedVersion: 2, Content: "author " + p.ID, Reason: "concurrent edit"})
			results <- outcome{p.ID, e}
		}(actor)
	}
	close(start)
	winner := ""
	conflicts := 0
	for range 2 {
		r := <-results
		if r.err == nil {
			if winner != "" {
				t.Fatal("both conflicting edits committed")
			}
			winner = r.actor
		} else {
			code(t, r.err, "conflict")
			conflicts++
		}
	}
	if winner == "" || conflicts != 1 {
		t.Fatal("CAS did not produce one winner and one conflict")
	}
	current, e := f.s.MaterialVersion(ctx, f.p, f.task.ID, m.ID, 3)
	if e != nil || current.Content != "author "+winner || current.Revision.PrincipalID != winner || current.Revision.BaseVersion != 2 {
		t.Fatal("winning revision lost bytes/author/base")
	}
	base := 1
	raw, e := f.s.ReviseMaterial(ctx, f.p, f.task.ID, m.ID, domain.NewID("key"), domain.ReviseMaterial{ExpectedVersion: 3, Content: "Explicitly merged text", Reason: "Reviewed the concurrent versions", MergeBaseVersion: &base})
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &m); e != nil || m.HeadVersion != 4 {
		t.Fatal("merge did not append fourth revision")
	}
	merged, e := f.s.MaterialVersion(ctx, f.p, f.task.ID, m.ID, 4)
	if e != nil || merged.Revision.BaseVersion != 3 || merged.Revision.MergeBaseVersion == nil || *merged.Revision.MergeBaseVersion != 1 {
		t.Fatal("explicit merge lineage lost")
	}
	original, e := f.s.MaterialVersion(ctx, f.p, f.task.ID, m.ID, 1)
	if e != nil || original.Content != "initial exact bytes\n" {
		t.Fatal("old revision changed")
	}
	revisions, next, e := f.s.MaterialRevisions(ctx, f.p, f.task.ID, m.ID, "", 100)
	if e != nil || len(revisions) != 4 || next != "" {
		t.Fatal("replay/concurrent edit duplicated revisions")
	}
	after, e := f.s.GetTask(ctx, f.p, f.task.ID)
	if e != nil {
		t.Fatal(e)
	}
	independentMaterialAssertSameTask(t, taskBefore, after)
	if f.money(t) != money || f.calls.Load() != 1 {
		t.Fatal("draft editing changed money or dispatched work")
	}
}

func TestIndependentMaterialCurrentPermissionBeforeReplay(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx := context.Background()
	admin := independentCollaborationActor(t, f.s, f.p.OrganizationID, "admin")
	request := domain.CreateMaterial{Name: "Admin-authored draft on another owner's task", MediaType: "text/plain", Content: "authorized text"}
	key := domain.NewID("key")
	raw, e := f.s.CreateMaterial(ctx, admin, f.task.ID, key, request)
	if e != nil {
		t.Fatal(e)
	}
	var m domain.Material
	if e = json.Unmarshal(raw, &m); e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.Pool.Exec(ctx, "UPDATE memberships SET role='member' WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, admin.ID); e != nil {
		t.Fatal(e)
	}
	_, e = f.s.CreateMaterial(ctx, admin, f.task.ID, key, request)
	code(t, e, "forbidden")
	_, e = f.s.ReviseMaterial(ctx, admin, f.task.ID, m.ID, domain.NewID("key"), domain.ReviseMaterial{ExpectedVersion: 1, Content: "unauthorized overwrite", Reason: "not the owner"})
	code(t, e, "forbidden")
	_, e = f.s.PublishMaterial(ctx, admin, f.task.ID, m.ID, domain.NewID("key"), domain.PublishMaterial{ExpectedTaskVersion: f.current.Version, ExpectedMaterialVersion: 1, ContentDigest: Hash([]byte(request.Content)), ParentCandidateID: f.candidate.ID})
	code(t, e, "forbidden")
	original, e := f.s.MaterialVersion(ctx, f.p, f.task.ID, m.ID, 1)
	if e != nil || original.Content != request.Content || original.Revision.PrincipalID != admin.ID {
		t.Fatal("revocation overwrote original authority receipt")
	}
}

func TestIndependentMaterialPublicationSnapshotAndTerminalDraft(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx := context.Background()
	before := f.money(t)
	old := f.candidate
	oldReview := f.review("accept")
	m := independentMaterialCreate(t, f.s, f.p, f.task.ID, "A revised exact result", old.ID)
	request := domain.PublishMaterial{ExpectedTaskVersion: f.current.Version, ExpectedMaterialVersion: m.HeadVersion, ContentDigest: m.HeadContentDigest, ParentCandidateID: old.ID}
	for _, bad := range []domain.PublishMaterial{
		{ExpectedTaskVersion: request.ExpectedTaskVersion - 1, ExpectedMaterialVersion: 1, ContentDigest: request.ContentDigest, ParentCandidateID: old.ID},
		{ExpectedTaskVersion: request.ExpectedTaskVersion, ExpectedMaterialVersion: 2, ContentDigest: request.ContentDigest, ParentCandidateID: old.ID},
		{ExpectedTaskVersion: request.ExpectedTaskVersion, ExpectedMaterialVersion: 1, ContentDigest: Hash([]byte("substitution")), ParentCandidateID: old.ID},
		{ExpectedTaskVersion: request.ExpectedTaskVersion, ExpectedMaterialVersion: 1, ContentDigest: request.ContentDigest, ParentCandidateID: domain.NewID("cand")},
	} {
		if _, e := f.s.PublishMaterial(ctx, f.p, f.task.ID, m.ID, domain.NewID("key"), bad); e == nil {
			t.Fatal("mismatched publication snapshot accepted")
		}
	}
	key := domain.NewID("key")
	if _, e := f.s.PublishMaterial(ctx, f.p, f.task.ID, m.ID, key, request); e != nil {
		t.Fatal(e)
	}
	f.refresh(t)
	first := f.candidate
	if first.Source != "material" || first.ParentCandidateID != old.ID || first.ReviewBasisCandidateID != old.ID || first.MaterialID != m.ID || first.MaterialVersion != m.HeadVersion || first.PublishedBy != f.p.ID || first.ArtifactID == old.ArtifactID || first.RunID != old.RunID || first.OperationID != old.OperationID || f.current.Result != "" || f.current.AttemptCount != 1 {
		t.Fatal("publication lost exact lineage or created execution/result")
	}
	_, e := f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), oldReview)
	code(t, e, "conflict")
	m = independentMaterialRevise(t, f.s, f.p, f.task.ID, m, "Second revised result")
	independentMaterialPublish(t, f, m)
	f.refresh(t)
	second := f.candidate
	if second.ParentCandidateID != first.ID || second.ReviewBasisCandidateID != old.ID || second.ArtifactID == first.ArtifactID {
		t.Fatal("material successor lost earliest generated review basis")
	}
	// Historical exact replay is not permission to restore an older current pointer.
	if _, e = f.s.PublishMaterial(ctx, f.p, f.task.ID, m.ID, key, request); e != nil {
		t.Fatal(e)
	}
	f.refresh(t)
	if f.candidate.ID != second.ID {
		t.Fatal("cached publication resurrected a superseded candidate")
	}
	candidates, _, e := f.s.Candidates(ctx, f.p, f.task.ID, "", 100)
	if e != nil || len(candidates) != 3 {
		t.Fatal("publication replay duplicated candidate history")
	}
	if _, e = f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), f.review("accept")); e != nil {
		t.Fatal(e)
	}
	accepted, e := f.s.GetTask(ctx, f.p, f.task.ID)
	if e != nil || accepted.Result != "Second revised result" || accepted.AcceptedDecisionID == "" {
		t.Fatal("accepted result did not bind edited bytes")
	}
	m = independentMaterialRevise(t, f.s, f.p, f.task.ID, m, "Later draft, not accepted")
	after, e := f.s.GetTask(ctx, f.p, f.task.ID)
	if e != nil {
		t.Fatal(e)
	}
	independentMaterialAssertSameTask(t, accepted, after)
	_, e = f.s.PublishMaterial(ctx, f.p, f.task.ID, m.ID, domain.NewID("key"), domain.PublishMaterial{ExpectedTaskVersion: after.Version, ExpectedMaterialVersion: m.HeadVersion, ContentDigest: m.HeadContentDigest, ParentCandidateID: second.ID})
	if e == nil {
		t.Fatal("terminal publication rewrote accepted result")
	}
	if f.money(t) != before || f.calls.Load() != 1 {
		t.Fatal("publication/terminal drafts changed financial facts or sent work")
	}
}

func TestIndependentMaterialPublicationRechecksPermissionOnReplay(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx := context.Background()
	m := independentMaterialCreate(t, f.s, f.p, f.task.ID, "Published by authorized administrator", f.candidate.ID)
	admin := independentCollaborationActor(t, f.s, f.p.OrganizationID, "admin")
	key := domain.NewID("key")
	request := domain.PublishMaterial{ExpectedTaskVersion: f.current.Version, ExpectedMaterialVersion: m.HeadVersion, ContentDigest: m.HeadContentDigest, ParentCandidateID: f.candidate.ID}
	if _, e := f.s.PublishMaterial(ctx, admin, f.task.ID, m.ID, key, request); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.Pool.Exec(ctx, "UPDATE memberships SET role='member' WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, admin.ID); e != nil {
		t.Fatal(e)
	}
	_, e := f.s.PublishMaterial(ctx, admin, f.task.ID, m.ID, key, request)
	code(t, e, "forbidden")
}

func TestIndependentMaterialConcurrentPublicationAndRollback(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx := context.Background()
	m := independentMaterialCreate(t, f.s, f.p, f.task.ID, "One proposed result", f.candidate.ID)
	before := f.money(t)
	request := domain.PublishMaterial{ExpectedTaskVersion: f.current.Version, ExpectedMaterialVersion: m.HeadVersion, ContentDigest: m.HeadContentDigest, ParentCandidateID: f.candidate.ID}
	key := domain.NewID("key")
	if _, e := f.s.Pool.Exec(ctx, `CREATE FUNCTION independent_material_outbox_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'independent material publication failure'; END $$`); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.Pool.Exec(ctx, "CREATE TRIGGER independent_material_outbox_failure BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION independent_material_outbox_failure()"); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.PublishMaterial(ctx, f.p, f.task.ID, m.ID, key, request); e == nil {
		t.Fatal("outbox fault did not roll back publication")
	}
	current, e := f.s.GetTask(ctx, f.p, f.task.ID)
	if e != nil {
		t.Fatal(e)
	}
	independentMaterialAssertSameTask(t, f.current, current)
	candidates, _, e := f.s.Candidates(ctx, f.p, f.task.ID, "", 100)
	if e != nil || len(candidates) != 1 {
		t.Fatal("failed publication leaked candidate")
	}
	artifacts, e := f.s.Artifacts(ctx, f.p, f.task.ID)
	if e != nil || len(artifacts) != 1 {
		t.Fatal("failed publication leaked artifact")
	}
	if _, e = f.s.Pool.Exec(ctx, "DROP TRIGGER independent_material_outbox_failure ON outbox"); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := f.s.PublishMaterial(ctx, f.p, f.task.ID, m.ID, key, request)
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	for e := range results {
		if e != nil {
			t.Fatal(e)
		}
	}
	reopened, e := Open(ctx, f.dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	if _, e = reopened.PublishMaterial(ctx, f.p, f.task.ID, m.ID, key, request); e != nil {
		t.Fatal(e)
	}
	candidates, _, e = f.s.Candidates(ctx, f.p, f.task.ID, "", 100)
	if e != nil || len(candidates) != 2 {
		t.Fatal("concurrent/restart publication duplicated candidate")
	}
	artifacts, e = f.s.Artifacts(ctx, f.p, f.task.ID)
	if e != nil || len(artifacts) != 2 {
		t.Fatal("concurrent/restart publication duplicated physical artifact")
	}
	if f.money(t) != before || f.calls.Load() != 1 {
		t.Fatal("publication replay/failure changed money")
	}
}

func TestIndependentMaterialFinancialNoninterference(t *testing.T) {
	for _, kind := range []string{"unknown", "settled", "overage", "quarantine"} {
		t.Run(kind, func(t *testing.T) {
			f := independentCollaborationSetup(t)
			ctx := context.Background()
			if kind != "unknown" {
				var rid string
				if e := f.s.Pool.QueryRow(ctx, "SELECT id FROM reservations WHERE organization_id=$1 AND operation_id=$2", f.p.OrganizationID, f.candidate.OperationID).Scan(&rid); e != nil {
					t.Fatal(e)
				}
				amount := int64(25)
				if kind == "overage" {
					amount = 2000
				}
				if kind == "quarantine" {
					lease := independentLease(t, f.s, f.p, f.lease.Agent, "", 1000)
					other := independentReserve(t, f.s, lease, 1)
					out, e := f.s.AccountCost(ctx, f.p.OrganizationID, other.ID, independentEvidence(math.MaxInt64))
					if e != nil || out.Status != "settled" {
						t.Fatal("quarantine setup failed", e)
					}
					amount = 1
				}
				ev := independentEvidence(amount)
				ev.Source = f.c.ProviderAccount
				out, e := f.s.AccountCost(ctx, f.p.OrganizationID, rid, ev)
				if e != nil || (kind == "quarantine" && out.Status != "quarantined") {
					t.Fatal("financial fixture failed", e)
				}
				f.refresh(t)
			}
			before := f.money(t)
			taskBefore := f.current
			m := independentMaterialCreate(t, f.s, f.p, f.task.ID, "Draft under financial restriction", f.candidate.ID)
			m = independentMaterialRevise(t, f.s, f.p, f.task.ID, m, "Revised under financial restriction")
			same, e := f.s.GetTask(ctx, f.p, f.task.ID)
			if e != nil {
				t.Fatal(e)
			}
			independentMaterialAssertSameTask(t, taskBefore, same)
			independentMaterialPublish(t, f, m)
			f.refresh(t)
			if _, e = f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), f.review("accept")); e != nil {
				t.Fatal(e)
			}
			if f.money(t) != before || f.calls.Load() != 1 {
				t.Fatal("editing/publication/review altered financial facts")
			}
		})
	}
}

func TestIndependentMaterialTenantBindingAndHistory(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx := context.Background()
	m := independentMaterialCreate(t, f.s, f.p, f.task.ID, "Historical content", f.candidate.ID)
	foreign, _ := identity(t, f.s, 100000)
	foreignAgent := agent(t, f.s, foreign)
	foreignTask := task(t, f.s, foreign, foreignAgent)
	if _, e := f.s.GetMaterial(ctx, foreign, f.task.ID, m.ID); e == nil {
		t.Fatal("foreign tenant read material")
	}
	if _, e := f.s.MaterialVersion(ctx, foreign, f.task.ID, m.ID, 1); e == nil {
		t.Fatal("foreign tenant read revision")
	}
	if _, e := f.s.CreateMaterial(ctx, foreign, foreignTask.ID, domain.NewID("key"), domain.CreateMaterial{Name: "foreign source", MediaType: "text/plain", Content: "x", BaseCandidateID: f.candidate.ID}); e == nil {
		t.Fatal("foreign base candidate accepted")
	}
	if _, e := f.s.ReviseMaterial(ctx, foreign, foreignTask.ID, m.ID, domain.NewID("key"), domain.ReviseMaterial{ExpectedVersion: 1, Content: "cross scope", Reason: "not authorized"}); e == nil {
		t.Fatal("revision rebound to another task")
	}
	for _, mutation := range []struct {
		sql  string
		args []any
	}{
		{"UPDATE material_revisions SET content='overwritten' WHERE organization_id=$1", []any{f.p.OrganizationID}},
		{"DELETE FROM material_revisions WHERE organization_id=$1", []any{f.p.OrganizationID}},
		{"UPDATE materials SET task_id=$2 WHERE organization_id=$1 AND id=$3", []any{f.p.OrganizationID, foreignTask.ID, m.ID}},
	} {
		_, e := f.s.Pool.Exec(ctx, mutation.sql, mutation.args...)
		var pgError *pgconn.PgError
		if !errors.As(e, &pgError) || pgError.Code != "23514" {
			t.Fatalf("immutable material history/scope not guarded by constraint: %v", e)
		}
	}
	// Re-read exact bytes so a rejected SQL statement is not the only oracle.
	got, e := f.s.MaterialVersion(ctx, f.p, f.task.ID, m.ID, 1)
	if e != nil || got.Content != "Historical content" {
		t.Fatal("history bytes not preserved")
	}
}

func TestIndependentMaterialNestedReviewBasisAndManagedConsumption(t *testing.T) {
	ctx := context.Background()
	model := independentModelSetup(t, false, nil)
	if _, e := model.s.CancelTask(ctx, model.p, model.task.ID, domain.NewID("key")); e != nil {
		t.Fatal(e)
	}
	leaf := domain.Agent{ID: model.task.AgentID, Version: model.task.AgentVersion}
	inner := teamAgent(t, model.s, model.p, "parallel", "all", 2, teamMember("left", leaf), teamMember("right", leaf))
	outer := teamAgent(t, model.s, model.p, "parallel", "all", 2, teamMember("nested", inner), teamMember("single", leaf))
	model.task = task(t, model.s, model.p, outer)
	children, _, e := model.s.Children(ctx, model.p, model.task.ID, "", 100)
	if e != nil {
		t.Fatal(e)
	}
	nestedID := ""
	for _, child := range children {
		if child.AgentID == inner.ID {
			nestedID = child.ID
		}
	}
	if nestedID == "" {
		t.Fatal("nested task missing")
	}
	leafEdited, nestedEdited := "", ""
	basisByTask := map[string]string{}
	publishedByTask := map[string]string{}
	for i := 0; i < 100; i++ {
		teamTick(t, model.s)
		nested, e := model.s.GetTask(ctx, model.p, nestedID)
		if e != nil {
			t.Fatal(e)
		}
		if nested.CandidateID != "" && nestedEdited == "" {
			sub := &independentCollaborationFixture{independentModelFixture: &independentModelFixture{s: model.s, p: model.p, task: nested}}
			sub.refresh(t)
			basisByTask[nested.ID] = sub.candidate.ID
			material := independentMaterialCreate(t, model.s, model.p, nested.ID, "Edited nested team summary; this is not a coordinator manifest", sub.candidate.ID)
			independentMaterialPublish(t, sub, material)
			sub.refresh(t)
			nestedEdited = nested.ID
			publishedByTask[nested.ID] = sub.candidate.ID
		}
		lease, _, e := model.s.ClaimModel(ctx, "independent-material-team", 30*time.Second)
		if e != nil {
			t.Fatal(e)
		}
		if lease != nil {
			out, e := model.runtime(t, model.s).Execute(ctx, lease)
			if e != nil || out.Status != modelruntime.CandidateWaitingInput {
				t.Fatal("model fixture did not create candidate", e)
			}
			if leafEdited == "" {
				sub := &independentCollaborationFixture{independentModelFixture: &independentModelFixture{s: model.s, p: model.p, task: lease.Task}}
				sub.refresh(t)
				basisByTask[sub.task.ID] = sub.candidate.ID
				material := independentMaterialCreate(t, model.s, model.p, sub.task.ID, "Edited leaf candidate", sub.candidate.ID)
				independentMaterialPublish(t, sub, material)
				sub.refresh(t)
				leafEdited = sub.task.ID
				publishedByTask[sub.task.ID] = sub.candidate.ID
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
	if leafEdited == "" || nestedEdited == "" || model.calls.Load() != 3 {
		t.Fatal("nested fixture did not include both pre-consumption material edits")
	}
	root := &independentCollaborationFixture{independentModelFixture: model}
	root.refresh(t)
	basisByTask[root.task.ID] = root.candidate.ID
	before := root.money(t)
	var scopeMoney string
	if e = model.s.Pool.QueryRow(ctx, "SELECT jsonb_agg(jsonb_build_array(id,reserved_microusd,spent_microusd,financial_hold) ORDER BY id)::text FROM tasks WHERE organization_id=$1 AND root_id=$2", model.p.OrganizationID, model.task.ID).Scan(&scopeMoney); e != nil {
		t.Fatal(e)
	}
	// A consumed managed child's private draft may advance, but its selected
	// workflow candidate cannot be replaced or gain an independent decision.
	consumed := &independentCollaborationFixture{independentModelFixture: &independentModelFixture{s: model.s, p: model.p, task: domain.Task{ID: leafEdited}}}
	consumed.refresh(t)
	private := independentMaterialCreate(t, model.s, model.p, leafEdited, "Late private draft", consumed.candidate.ID)
	_, e = model.s.PublishMaterial(ctx, model.p, leafEdited, private.ID, domain.NewID("key"), domain.PublishMaterial{ExpectedTaskVersion: consumed.current.Version, ExpectedMaterialVersion: private.HeadVersion, ContentDigest: private.HeadContentDigest, ParentCandidateID: consumed.candidate.ID})
	code(t, e, "conflict")
	rootMaterial := independentMaterialCreate(t, model.s, model.p, model.task.ID, "Edited root final result, deliberately not JSON", root.candidate.ID)
	independentMaterialPublish(t, root, rootMaterial)
	root.refresh(t)
	publishedByTask[root.task.ID] = root.candidate.ID
	if _, e = model.s.ReviewTask(ctx, model.p, model.task.ID, domain.NewID("key"), root.review("accept")); e != nil {
		t.Fatal(e)
	}
	for taskID, candidateID := range publishedByTask {
		current, e := model.s.GetTask(ctx, model.p, taskID)
		if e != nil || current.Status != "succeeded" || current.CandidateID != candidateID || current.AcceptedDecisionID == "" {
			t.Fatal("edited team closure did not close exact selected task")
		}
		candidates, _, e := model.s.Candidates(ctx, model.p, taskID, "", 100)
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, c := range candidates {
			if c.ID == candidateID {
				found = true
				if c.Source != "material" || c.ReviewBasisCandidateID != basisByTask[taskID] {
					t.Fatal("nested material lost original review basis")
				}
			}
		}
		if !found {
			t.Fatal("material candidate absent from history")
		}
		ds, _, e := model.s.Decisions(ctx, model.p, taskID, "", 100)
		if e != nil || len(ds) != 1 || ds[0].CandidateID != candidateID {
			t.Fatal("nested inherited decision used basis text instead of selected edited snapshot")
		}
		if taskID != model.task.ID && ds[0].Method != "inherited_authorized_user_decision" {
			t.Fatal("missing inherited attribution")
		}
	}
	var pending int
	if e = model.s.Pool.QueryRow(ctx, "SELECT count(*) FROM tasks WHERE organization_id=$1 AND root_id=$2 AND status<>'succeeded'", model.p.OrganizationID, model.task.ID).Scan(&pending); e != nil || pending != 0 {
		t.Fatal("edited team root accepted while descendants remain pending")
	}
	final, e := model.s.GetTask(ctx, model.p, model.task.ID)
	if e != nil || final.Result != "Edited root final result, deliberately not JSON" {
		t.Fatal("edited final text replaced by basis manifest")
	}
	var after string
	if e = model.s.Pool.QueryRow(ctx, "SELECT jsonb_agg(jsonb_build_array(id,reserved_microusd,spent_microusd,financial_hold) ORDER BY id)::text FROM tasks WHERE organization_id=$1 AND root_id=$2", model.p.OrganizationID, model.task.ID).Scan(&after); e != nil || after != scopeMoney || root.money(t) != before || model.calls.Load() != 3 {
		t.Fatal("nested material decisions changed money or executed more work")
	}
}

func TestIndependentMaterialStorageLimitsAreAtomic(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx := context.Background()
	before := f.money(t)
	taskBefore := f.current
	documents := []domain.Material{}
	for i := 0; i < domain.MaxMaterialsPerTask; i++ {
		documents = append(documents, independentMaterialCreate(t, f.s, f.p, f.task.ID, "x", f.candidate.ID))
	}
	if _, e := f.s.CreateMaterial(ctx, f.p, f.task.ID, domain.NewID("key"), domain.CreateMaterial{Name: "Over document cap", MediaType: "text/plain", Content: "x"}); e == nil {
		t.Fatal("task document cap bypassed")
	}
	m := documents[0]
	for m.HeadVersion < domain.MaxMaterialRevisions {
		m = independentMaterialRevise(t, f.s, f.p, f.task.ID, m, "small bounded revision")
	}
	if _, e := f.s.ReviseMaterial(ctx, f.p, f.task.ID, m.ID, domain.NewID("key"), domain.ReviseMaterial{ExpectedVersion: m.HeadVersion, Content: "beyond limit", Reason: "must not reset"}); e == nil {
		t.Fatal("material revision cap bypassed")
	}
	// Trusted fixture positions the monotonic counter near its limit without
	// allocating 256MiB. It tests atomic quota admission, not disk consumption.
	if _, e := f.s.Pool.Exec(ctx, "UPDATE material_storage SET used_bytes=max_bytes-2 WHERE organization_id=$1", f.p.OrganizationID); e != nil {
		t.Fatal(e)
	}
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, doc := range documents[1:3] {
		go func(m domain.Material) {
			<-start
			_, e := f.s.ReviseMaterial(ctx, f.p, f.task.ID, m.ID, domain.NewID("key"), domain.ReviseMaterial{ExpectedVersion: 1, Content: "x", Reason: "r"})
			results <- e
		}(doc)
	}
	close(start)
	admitted := 0
	for range 2 {
		if e := <-results; e == nil {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("two bytes remaining admitted %d two-byte revisions", admitted)
	}
	var used, maximum int64
	if e := f.s.Pool.QueryRow(ctx, "SELECT used_bytes,max_bytes FROM material_storage WHERE organization_id=$1", f.p.OrganizationID).Scan(&used, &maximum); e != nil || used != maximum {
		t.Fatal("quota projection not exact")
	}
	heads := 0
	for _, doc := range documents[1:3] {
		current, e := f.s.GetMaterial(ctx, f.p, f.task.ID, doc.ID)
		if e != nil {
			t.Fatal(e)
		}
		heads += current.HeadVersion
	}
	if heads != 3 {
		t.Fatal("quota loser partially advanced head")
	}
	after, e := f.s.GetTask(ctx, f.p, f.task.ID)
	if e != nil {
		t.Fatal(e)
	}
	independentMaterialAssertSameTask(t, taskBefore, after)
	if f.money(t) != before {
		t.Fatal("storage quotas changed financial state")
	}
}

func TestIndependentMaterialPublicationCapSurvivesNewRun(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx := context.Background()
	m := independentMaterialCreate(t, f.s, f.p, f.task.ID, "Reusable explicitly published text", f.candidate.ID)
	for i := 0; i < domain.MaxMaterialPublications; i++ {
		independentMaterialPublish(t, f, m)
		f.refresh(t)
	}
	before := f.money(t)
	current := f.current
	request := domain.PublishMaterial{ExpectedTaskVersion: current.Version, ExpectedMaterialVersion: m.HeadVersion, ContentDigest: m.HeadContentDigest, ParentCandidateID: f.candidate.ID}
	if _, e := f.s.PublishMaterial(ctx, f.p, f.task.ID, m.ID, domain.NewID("key"), request); e == nil {
		t.Fatal("publication cap bypassed")
	}
	after, e := f.s.GetTask(ctx, f.p, f.task.ID)
	if e != nil {
		t.Fatal(e)
	}
	independentMaterialAssertSameTask(t, current, after)
	if f.money(t) != before {
		t.Fatal("rejected publication changed financial state")
	}
	if _, e = f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), f.review("request_changes")); e != nil {
		t.Fatal(e)
	}
	blocked, e := f.s.GetTask(ctx, f.p, f.task.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.RetryTask(ctx, f.p, f.task.ID, domain.NewID("key"), domain.RetryTaskRequest{ExpectedVersion: blocked.Version, Reason: "New explicit model attempt", Feedback: "Produce a new draft", AcknowledgeNewCost: true}); e != nil {
		t.Fatal(e)
	}
	lease, _, e := f.s.ClaimModel(ctx, "independent-material-new-run", 30*time.Second)
	if e != nil || lease == nil {
		t.Fatal("new explicit run not claimable", e)
	}
	out, e := f.runtime(t, f.s).Execute(ctx, lease)
	if e != nil || out.Status != modelruntime.CandidateWaitingInput {
		t.Fatal("new explicit model run failed", e)
	}
	f.refresh(t)
	request = domain.PublishMaterial{ExpectedTaskVersion: f.current.Version, ExpectedMaterialVersion: m.HeadVersion, ContentDigest: m.HeadContentDigest, ParentCandidateID: f.candidate.ID}
	if _, e = f.s.PublishMaterial(ctx, f.p, f.task.ID, m.ID, domain.NewID("key"), request); e == nil {
		t.Fatal("new run reset lifetime material publication cap")
	}
	var publications int
	if e = f.s.Pool.QueryRow(ctx, "SELECT publications FROM material_publication_counts WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&publications); e != nil || publications != domain.MaxMaterialPublications {
		t.Fatal("lifetime publication count changed")
	}
	if f.current.AttemptCount != 2 || f.calls.Load() != 2 {
		t.Fatal("editorial publications consumed model run slots or sent extra work")
	}
}

func TestIndependentMaterialExpiryDuringTreeGateWait(t *testing.T) {
	f := independentCollaborationSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	m := independentMaterialCreate(t, f.s, f.p, f.task.ID, "original draft", f.candidate.ID)
	if _, e := f.s.Pool.Exec(ctx, "UPDATE access_tokens SET expires_at=clock_timestamp()+interval '400 milliseconds' WHERE token_hash=$1", f.p.TokenHash); e != nil {
		t.Fatal(e)
	}
	blocker, e := f.s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback(ctx)
	var pid int
	if e = blocker.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); e != nil {
		t.Fatal(e)
	}
	if _, e = blocker.Exec(ctx, "SELECT root_id FROM task_tree_controls WHERE organization_id=$1 AND root_id=$2 FOR NO KEY UPDATE", f.p.OrganizationID, f.task.ID); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() {
		_, e := f.s.ReviseMaterial(ctx, f.p, f.task.ID, m.ID, domain.NewID("key"), domain.ReviseMaterial{ExpectedVersion: 1, Content: "expired writer text", Reason: "must roll back"})
		done <- e
	}()
	independentCollaborationWaiters(t, f.s, pid, 1)
	for {
		var expired bool
		if e = f.s.Pool.QueryRow(ctx, "SELECT expires_at<clock_timestamp() FROM access_tokens WHERE token_hash=$1", f.p.TokenHash).Scan(&expired); e != nil {
			t.Fatal(e)
		}
		if expired {
			break
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if e = blocker.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	code(t, <-done, "unauthenticated")
	var head, revisions int
	if e = f.s.Pool.QueryRow(ctx, "SELECT head_version,(SELECT count(*) FROM material_revisions WHERE organization_id=$1 AND material_id=$2) FROM materials WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, m.ID).Scan(&head, &revisions); e != nil || head != 1 || revisions != 1 {
		t.Fatal("expired transaction committed draft/head")
	}
}

func TestIndependentMaterialPublishVersusReviewForcedOrder(t *testing.T) {
	for _, first := range []string{"publish", "review"} {
		t.Run(first, func(t *testing.T) {
			f := independentCollaborationSetup(t)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			m := independentMaterialCreate(t, f.s, f.p, f.task.ID, "Revised exact bytes", f.candidate.ID)
			before := f.money(t)
			request := domain.PublishMaterial{ExpectedTaskVersion: f.current.Version, ExpectedMaterialVersion: m.HeadVersion, ContentDigest: m.HeadContentDigest, ParentCandidateID: f.candidate.ID}
			review := f.review("accept")
			blocker, e := f.s.Pool.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer blocker.Rollback(ctx)
			var pid int
			if e = blocker.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); e != nil {
				t.Fatal(e)
			}
			if _, e = blocker.Exec(ctx, "SELECT root_id FROM task_tree_controls WHERE organization_id=$1 AND root_id=$2 FOR NO KEY UPDATE", f.p.OrganizationID, f.task.ID); e != nil {
				t.Fatal(e)
			}
			published, reviewed := make(chan error, 1), make(chan error, 1)
			publish := func() {
				_, e := f.s.PublishMaterial(ctx, f.p, f.task.ID, m.ID, domain.NewID("key"), request)
				published <- e
			}
			decide := func() { _, e := f.s.ReviewTask(ctx, f.p, f.task.ID, domain.NewID("key"), review); reviewed <- e }
			if first == "publish" {
				go publish()
			} else {
				go decide()
			}
			independentCollaborationWaiters(t, f.s, pid, 1)
			if first == "publish" {
				go decide()
			} else {
				go publish()
			}
			independentCollaborationWaiters(t, f.s, pid, 2)
			if e = blocker.Commit(ctx); e != nil {
				t.Fatal(e)
			}
			pe, re := <-published, <-reviewed
			current, e := f.s.GetTask(ctx, f.p, f.task.ID)
			if e != nil {
				t.Fatal(e)
			}
			ds, _, e := f.s.Decisions(ctx, f.p, f.task.ID, "", 100)
			if e != nil {
				t.Fatal(e)
			}
			if first == "publish" {
				if pe != nil || re == nil || current.Status != "waiting_input" || current.CandidateID == f.candidate.ID || len(ds) != 0 {
					t.Fatal("publish-first did not invalidate stale review")
				}
			} else {
				if re != nil || pe == nil || current.Status != "succeeded" || current.CandidateID != f.candidate.ID || len(ds) != 1 {
					t.Fatal("review-first result overwritten by late publication")
				}
			}
			if f.money(t) != before || f.calls.Load() != 1 {
				t.Fatal("structural race affected money or dispatched work")
			}
		})
	}
}
