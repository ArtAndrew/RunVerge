// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/identity"
)

// This test checks the newly wired HTTP routes with real TLS/PG and a
// trusted-created browser session. It does not claim full OIDC flow evidence.
func TestIndependentCollaborationHTTPUsesExistingAuthAndTenantBoundary(t *testing.T) {
	f := independentBrowserAPI(t)
	createTask := func(foreign bool) domain.Task {
		change := func(r *http.Request) {}
		if foreign {
			change = func(r *http.Request) {
				r.Header.Del("Cookie")
				r.Header.Set("Authorization", "Bearer "+f.other.Token)
				r.Header.Set("X-Organization-ID", f.other.OrganizationID)
			}
		}
		_, body := f.request("POST", "/api/v1/agents", `{"name":"collaboration boundary fixture","instructions":"fixture","budget_microusd":1000}`, change, 201)
		var agent struct {
			Data domain.Agent `json:"data"`
		}
		if json.Unmarshal(body, &agent) != nil || agent.Data.ID == "" {
			t.Fatal("missing fixture agent")
		}
		f.request("POST", "/api/v1/agents/"+agent.Data.ID+"/publish", `{"expected_version":1}`, change, 200)
		encoded, _ := json.Marshal(domain.CreateTask{Goal: "independent route boundary", AgentID: agent.Data.ID, BudgetMicroUSD: 1000})
		_, body = f.request("POST", "/api/v1/tasks", string(encoded), change, 201)
		var task struct {
			Data domain.Task `json:"data"`
		}
		if json.Unmarshal(body, &task) != nil || task.Data.ID == "" {
			t.Fatal("missing fixture task")
		}
		return task.Data
	}
	own, foreign := createTask(false), createTask(true)
	for _, kind := range []string{"candidates", "decisions", "children"} {
		t.Run(kind, func(t *testing.T) {
			f.t = t
			f.request("GET", "/api/v1/tasks/"+own.ID+"/"+kind, "", nil, 200)
			f.request("GET", "/api/v1/tasks/"+foreign.ID+"/"+kind, "", nil, 404)
			f.request("GET", "/api/v1/tasks/"+own.ID+"/"+kind, "", func(r *http.Request) { r.Header.Set("X-Organization-ID", f.other.OrganizationID) }, 401)
			f.request("GET", "/api/v1/tasks/"+own.ID+"/"+kind, "", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 401)
		})
	}
	f.t = t
	review := `{"expected_version":1,"candidate_id":"unknown","artifact_id":"unknown","artifact_version":1,"content_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","decision":"accept"}`
	retry := `{"expected_version":1,"reason":"explicit retry","feedback":"correction","acknowledge_new_cost":true}`
	for _, route := range []struct{ name, body string }{{"review", review}, {"retry", retry}} {
		t.Run(route.name, func(t *testing.T) {
			f.t = t
			path := "/api/v1/tasks/" + own.ID + "/" + route.name
			f.request("POST", path, route.body, func(r *http.Request) { r.Header.Del("X-CSRF-Token") }, 403)
			f.request("POST", path, route.body, func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example.test") }, 403)
			f.request("POST", path, route.body, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+f.bootstrap.Token) }, 401)
			f.request("POST", path, route.body, func(r *http.Request) { r.Header.Add("Cookie", identity.SessionCookieName+"=\"unterminated") }, 401)
			f.request("POST", "/api/v1/tasks/"+foreign.ID+"/"+route.name, route.body, nil, 404)
		})
	}
	f.t = t
}
