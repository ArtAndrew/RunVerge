// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

func independentMaterialHTTPTask(t *testing.T, f *independentBrowserFixture) domain.Task {
	t.Helper()
	_, body := f.request("POST", "/api/v1/agents", `{"name":"independent material HTTP","instructions":"fixture","budget_microusd":1000}`, nil, 201)
	var a struct {
		Data domain.Agent `json:"data"`
	}
	if e := json.Unmarshal(body, &a); e != nil || a.Data.ID == "" {
		t.Fatal("missing fixture agent")
	}
	f.request("POST", "/api/v1/agents/"+a.Data.ID+"/publish", `{"expected_version":1}`, nil, 200)
	raw, _ := json.Marshal(domain.CreateTask{AgentID: a.Data.ID, Goal: "Material HTTP boundary", BudgetMicroUSD: 1000})
	_, body = f.request("POST", "/api/v1/tasks", string(raw), nil, 201)
	var task struct {
		Data domain.Task `json:"data"`
	}
	if e := json.Unmarshal(body, &task); e != nil || task.Data.ID == "" {
		t.Fatal("missing fixture task")
	}
	return task.Data
}

// Raw JSON distinguishes an omitted mandatory content property from valid empty
// draft content. A Go DTO would turn both into the same zero value.
func TestIndependentMaterialHTTPContentPresenceAndShape(t *testing.T) {
	f := independentBrowserAPI(t)
	task := independentMaterialHTTPTask(t, f)
	path := "/api/v1/tasks/" + task.ID + "/materials"
	for _, item := range []struct {
		name, body string
		want       int
	}{
		{"empty_valid", `{"name":"Empty draft","media_type":"text/plain","content":""}`, 201},
		{"missing_content", `{"name":"Missing text","media_type":"text/plain"}`, 400},
		{"null", `{"name":"Null","media_type":"text/plain","content":null}`, 400},
		{"duplicate", `{"name":"Duplicate","media_type":"text/plain","content":"one","content":"two"}`, 400},
		{"case_alias", `{"name":"Alias","media_type":"text/plain","Content":"text"}`, 400},
		{"number", `{"name":"Number","media_type":"text/plain","content":42}`, 400},
		{"forged_author", `{"name":"Forged","media_type":"text/plain","content":"text","created_by":"someone-else"}`, 400},
		{"nul", `{"name":"Nul","media_type":"text/plain","content":"a\u0000b"}`, 400},
		{"invalid_utf8", `{"name":"Invalid UTF8","media_type":"text/plain","content":"` + string([]byte{0xff}) + `"}`, 400},
		{"unpaired_high", `{"name":"High","media_type":"text/plain","content":"\ud800"}`, 400},
		{"unpaired_low", `{"name":"Low","media_type":"text/plain","content":"\udc00"}`, 400},
		{"split_surrogates", `{"name":"Split","media_type":"text/plain","content":"\ud800x\udc00"}`, 400},
		{"valid_pair", `{"name":"Pair","media_type":"text/plain","content":"\ud83d\ude00"}`, 201},
		{"literal_replacement", `{"name":"Replacement","media_type":"text/plain","content":"�"}`, 201},
		{"oversize", `{"name":"Large","media_type":"text/plain","content":"` + strings.Repeat("a", domain.MaxMaterialContentBytes+1) + `"}`, 400},
	} {
		t.Run(item.name, func(t *testing.T) {
			f.t = t
			_, body := f.request("POST", path, item.body, nil, item.want)
			if item.want == 201 {
				var m struct {
					Data domain.Material `json:"data"`
				}
				if e := json.Unmarshal(body, &m); e != nil || m.Data.HeadVersion != 1 {
					t.Fatal("empty draft missing")
				}
			}
		})
	}
	f.t = t
	// Revision has the same required-but-empty-valid content rule.
	_, body := f.request("POST", path, `{"name":"Revision draft","media_type":"text/plain","content":"initial"}`, nil, 201)
	var m struct {
		Data domain.Material `json:"data"`
	}
	if e := json.Unmarshal(body, &m); e != nil || m.Data.ID == "" {
		t.Fatal("revision material missing")
	}
	revisionPath := path + "/" + m.Data.ID + "/revisions"
	f.request("POST", revisionPath, `{"expected_version":1,"reason":"Explicit correction"}`, nil, 400)
	f.request("POST", revisionPath, `{"expected_version":1,"content":"","reason":"Explicitly clear draft"}`, nil, 201)
}

func TestIndependentMaterialHTTPReadScopeCSRFAndPagination(t *testing.T) {
	f := independentBrowserAPI(t)
	task := independentMaterialHTTPTask(t, f)
	otherTask := independentMaterialHTTPTask(t, f)
	path := "/api/v1/tasks/" + task.ID + "/materials"
	create := `{"name":"Scoped draft","media_type":"text/plain","content":"exact initial bytes"}`
	f.request("POST", path, create, func(r *http.Request) { r.Header.Del("X-CSRF-Token") }, 403)
	f.request("POST", path, create, func(r *http.Request) { r.Header.Set("Origin", "https://attacker.invalid") }, 403)
	want := map[string]bool{}
	var first domain.Material
	for i := 0; i < 3; i++ {
		_, raw := f.request("POST", path, create, nil, 201)
		var response struct {
			Data domain.Material `json:"data"`
		}
		if e := json.Unmarshal(raw, &response); e != nil || response.Data.ID == "" {
			t.Fatal("missing material")
		}
		want[response.Data.ID] = true
		if i == 0 {
			first = response.Data
		}
	}
	seen := map[string]bool{}
	cursor := ""
	for i := 0; i < 3; i++ {
		_, raw := f.request("GET", path+"?limit=1&cursor="+url.QueryEscape(cursor), "", nil, 200)
		var response struct {
			Data []domain.Material `json:"data"`
			Next string            `json:"next_cursor"`
		}
		if e := json.Unmarshal(raw, &response); e != nil || len(response.Data) != 1 {
			t.Fatal("invalid material page")
		}
		v := response.Data[0]
		if !want[v.ID] || seen[v.ID] || v.TaskID != task.ID || v.OrganizationID != task.OrganizationID {
			t.Fatal("pagination escaped scope or repeated an item")
		}
		seen[v.ID] = true
		cursor = response.Next
		if (i == 2) != (cursor == "") {
			t.Fatal("material page cursor ended at wrong boundary")
		}
	}
	base := path + "/" + first.ID
	for _, suffix := range []string{"", "/revisions", "/revisions/1"} {
		f.request("GET", base+suffix, "", nil, 200)
		f.request("GET", "/api/v1/tasks/"+otherTask.ID+"/materials/"+first.ID+suffix, "", nil, 404)
		f.request("GET", base+suffix, "", func(r *http.Request) {
			r.Header.Del("Cookie")
			r.Header.Set("Authorization", "Bearer "+f.other.Token)
			r.Header.Set("X-Organization-ID", f.other.OrganizationID)
		}, 404)
	}
	for _, query := range []string{"limit=0", "limit=101", "cursor=e30", "cursor=invalid!"} {
		f.request("GET", path+"?"+query, "", nil, 400)
		f.request("GET", base+"/revisions?"+query, "", nil, 400)
	}
	for _, v := range []string{"0", "129", "-1", "not-version"} {
		f.request("GET", base+"/revisions/"+v, "", nil, 400)
	}
	for version := 1; version < 3; version++ {
		raw, _ := json.Marshal(domain.ReviseMaterial{ExpectedVersion: version, Content: "new exact bytes", Reason: "Explicit change"})
		f.request("POST", base+"/revisions", string(raw), nil, 201)
	}
	versions := map[int]bool{}
	cursor = ""
	for i := 0; i < 3; i++ {
		_, raw := f.request("GET", base+"/revisions?limit=1&cursor="+url.QueryEscape(cursor), "", nil, 200)
		var response struct {
			Data []domain.MaterialRevision `json:"data"`
			Next string                    `json:"next_cursor"`
		}
		if e := json.Unmarshal(raw, &response); e != nil || len(response.Data) != 1 {
			t.Fatal("invalid revision page")
		}
		r := response.Data[0]
		if r.MaterialID != first.ID || r.TaskID != task.ID || r.Version < 1 || r.Version > 3 || versions[r.Version] {
			t.Fatal("revision pagination changed identity or repeated a version")
		}
		versions[r.Version] = true
		cursor = response.Next
		if (i == 2) != (cursor == "") {
			t.Fatal("revision cursor ended at wrong boundary")
		}
	}
	_, raw := f.request("GET", base+"/revisions/1", "", nil, 200)
	var original struct {
		Data domain.MaterialContent `json:"data"`
	}
	if e := json.Unmarshal(raw, &original); e != nil || original.Data.Content != "exact initial bytes" || original.Data.Revision.Version != 1 {
		t.Fatal("exact old revision read returned head content")
	}
}
