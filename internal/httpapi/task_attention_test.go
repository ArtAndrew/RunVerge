// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
)

func TestTaskAttentionUsesCurrentOrganizationAndStablePages(t *testing.T) {
	f := independentBrowserAPI(t)
	ctx := context.Background()
	anchor := independentEventHTTPTask(t, f.store, f.bootstrap)
	other := independentEventHTTPTask(t, f.store, f.other)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.store.Pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	tied := now.Add(-2 * time.Hour)
	const draft = "tsk_attention_z"
	const approval = "tsk_attention_a"
	const blocked = "tsk_attention_blocked"
	const otherBlocked = "tsk_other_attention"
	insert := func(base domain.Task, id, status, reason, parent string, created time.Time) {
		t.Helper()
		exec(`INSERT INTO tasks(organization_id,id,owner_id,agent_id,agent_version,goal,status,waiting_reason,budget_microusd,parent_id,created_at)
			VALUES($1,$2,$3,$4,$5,$2,$6,$7,10000,NULLIF($8,''),$9)`,
			base.OrganizationID, id, base.OwnerID, base.AgentID, base.AgentVersion, status, reason, parent, created)
	}
	insert(anchor, draft, "waiting_input", "model_candidate_pending", "", tied)
	insert(anchor, approval, "waiting_approval", "tool_approval_pending", anchor.ID, tied)
	insert(anchor, blocked, "blocked", "", "", tied.Add(-time.Hour))
	insert(anchor, "tsk_attention_sent", "waiting_approval", "tool_execution_pending", "", now)
	insert(anchor, "tsk_attention_dependency", "waiting_input", "team_dependency_pending", anchor.ID, now)
	insert(anchor, "tsk_attention_team_draft", "waiting_input", "model_candidate_pending", anchor.ID, now)
	insert(other, otherBlocked, "blocked", "external_action_unknown", "", now)
	exec(`INSERT INTO tasks(organization_id,id,owner_id,agent_id,agent_version,goal,status,budget_microusd,created_at)
		SELECT $1,'tsk_done_'||n,$2,$3,$4,'Synthetic completed task','succeeded',10000,$5
		FROM generate_series(1,55) n`, anchor.OrganizationID, anchor.OwnerID, anchor.AgentID, anchor.AgentVersion, now.Add(-time.Hour))
	exec("UPDATE memberships SET role='viewer' WHERE organization_id=$1 AND principal_id=$2", anchor.OrganizationID, anchor.OwnerID)

	type taskPage struct {
		Data       []domain.Task `json:"data"`
		NextCursor string        `json:"next_cursor"`
		TaskView   string        `json:"task_view"`
	}
	const base = "/api/v1/tasks"
	const view = base + "?view=attention"
	read := func(path, wantView, wantOrganization string, change func(*http.Request)) (taskPage, []string) {
		t.Helper()
		response, raw := f.request("GET", path, "", change, 200)
		var page taskPage
		if json.Unmarshal(raw, &page) != nil || page.TaskView != wantView || page.Data == nil ||
			response.Header.Get(domain.APIContractHeader) != domain.ContractVersion {
			t.Fatalf("invalid task page envelope for %q", wantView)
		}
		var ids []string
		for _, item := range page.Data {
			if item.OrganizationID != wantOrganization {
				t.Fatal("task page crossed the organization boundary")
			}
			ids = append(ids, item.ID)
		}
		return page, ids
	}
	ordinary, ids := read(base, "", anchor.OrganizationID, nil)
	if len(ids) != 50 || ordinary.NextCursor == "" || slices.Contains(ids, draft) {
		t.Fatal("ordinary first page did not exercise the old-task boundary")
	}
	older, olderIDs := read(base+"?cursor="+url.QueryEscape(ordinary.NextCursor), "", anchor.OrganizationID, nil)
	if older.NextCursor != "" || !slices.Contains(olderIDs, draft) {
		t.Fatal("ordinary cursor no longer reaches the old task")
	}
	path := view + "&limit=1"
	first, ids := read(path, domain.TaskViewAttention, anchor.OrganizationID, nil)
	if first.NextCursor == "" || !slices.Equal(ids, []string{draft}) {
		t.Fatalf("first attention page missed the old draft: %v", ids)
	}
	secondPath := fmt.Sprintf("%s&cursor=%s", path, url.QueryEscape(first.NextCursor))
	second, ids := read(secondPath, domain.TaskViewAttention, anchor.OrganizationID, nil)
	if second.NextCursor == "" || !slices.Equal(ids, []string{approval}) {
		t.Fatalf("tied timestamps did not page by task ID: %v", ids)
	}
	last, ids := read(path+"&cursor="+url.QueryEscape(second.NextCursor), domain.TaskViewAttention, anchor.OrganizationID, nil)
	if last.NextCursor != "" || !slices.Equal(ids, []string{blocked}) {
		t.Fatalf("queue did not filter in-flight operations and internal stages: %v", ids)
	}

	otherCredential := func(r *http.Request) {
		r.Header.Del("Cookie")
		r.Header.Set("Authorization", "Bearer "+f.other.Token)
		r.Header.Set("X-Organization-ID", f.other.OrganizationID)
	}
	_, ids = read(view, domain.TaskViewAttention, other.OrganizationID, otherCredential)
	if !slices.Equal(ids, []string{otherBlocked}) {
		t.Fatal("second organization did not see its own queue")
	}
	f.request("GET", secondPath, "", otherCredential, 400)
	f.request("GET", view, "", func(r *http.Request) { r.Header.Set("X-Organization-ID", f.other.OrganizationID) }, 401)
	for _, malformed := range []string{
		base + "?cursor=" + url.QueryEscape(first.NextCursor),
		view + "&cursor=" + url.QueryEscape(ordinary.NextCursor),
		base + "?view=", base + "?view=all", base + "?view=%ZZ",
		view + "&view=attention", view + "&limit=1&limit=2", view + "&cursor=a&cursor=b", view + "&owner_id=other",
	} {
		f.request("GET", malformed, "", nil, 400)
	}

	exec("UPDATE tasks SET status='succeeded',waiting_reason='' WHERE organization_id=$1 AND id=$2", anchor.OrganizationID, draft)
	_, ids = read(path, domain.TaskViewAttention, anchor.OrganizationID, nil)
	if !slices.Equal(ids, []string{approval}) {
		t.Fatal("the queue did not reflect the current task state")
	}
	exec("UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", anchor.OrganizationID, anchor.OwnerID)
	f.request("GET", secondPath, "", nil, 401)
}
