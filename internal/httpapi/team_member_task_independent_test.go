// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"encoding/json"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

// A missing optional task inherits the parent goal. An explicit empty task is
// not omission under the published JSON Schema/SDK contract (minLength: 1).
// Raw maps intentionally preserve empty fields that Go omitempty would erase.
func TestIndependentTeamMemberTaskPresenceBoundary(t *testing.T) {
	f := independentBrowserAPI(t)
	_, body := f.request("POST", "/api/v1/agents", `{"name":"independent member task fixture","instructions":"fixture","budget_microusd":1000}`, nil, 201)
	var agent struct {
		Data domain.Agent `json:"data"`
	}
	if e := json.Unmarshal(body, &agent); e != nil || agent.Data.ID == "" {
		t.Fatal("fixture agent missing")
	}
	f.request("POST", "/api/v1/agents/"+agent.Data.ID+"/publish", `{"expected_version":1}`, nil, 200)
	for _, item := range []struct {
		name    string
		present bool
		value   string
		want    int
	}{
		{"omitted", false, "", 201},
		{"nonblank", true, "Specific member task", 201},
		{"explicit_empty", true, "", 400},
	} {
		t.Run(item.name, func(t *testing.T) {
			f.t = t
			member := map[string]any{"id": "one", "agent_id": agent.Data.ID, "agent_version": agent.Data.Version}
			if item.present {
				member["task"] = item.value
			}
			request := map[string]any{"name": "independent team task boundary", "instructions": "fixed plan", "budget_microusd": 1000, "team": map[string]any{"pattern": "parallel", "join_policy": "all", "max_depth": 1, "max_children": 1, "max_parallel": 1, "members": []any{member}}}
			encoded, e := json.Marshal(request)
			if e != nil {
				t.Fatal(e)
			}
			_, response := f.request("POST", "/api/v1/agents", string(encoded), nil, item.want)
			if item.want == 400 {
				var errorBody struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if e = json.Unmarshal(response, &errorBody); e != nil || errorBody.Error.Code != "invalid_request" {
					t.Fatal("explicit empty task did not receive invalid_request")
				}
			}
		})
	}
	f.t = t
}
