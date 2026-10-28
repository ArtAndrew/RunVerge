// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

func TestIndependentCurrentIdentityUsesCurrentMembershipAndNoCredentials(t *testing.T) {
	f := independentBrowserAPI(t)
	check := func(role string) {
		t.Helper()
		_, raw := f.request("GET", "/api/v1/me", "", nil, 200)
		var envelope struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &envelope) != nil || len(envelope.Data) != 5 {
			t.Fatal("identity response contains unexpected fields")
		}
		for _, key := range []string{"principal_id", "organization_id", "principal_kind", "role", "permissions"} {
			if envelope.Data[key] == nil {
				t.Fatal("identity is missing public scope")
			}
		}
		var got domain.CurrentIdentity
		data, _ := json.Marshal(envelope.Data)
		if json.Unmarshal(data, &got) != nil || got.PrincipalID != f.bootstrap.PrincipalID || got.OrganizationID != f.bootstrap.OrganizationID || got.PrincipalKind != domain.PrincipalHuman || got.Role != role {
			t.Fatal("identity does not reflect current active membership")
		}
		want, _ := json.Marshal(domain.RolePermissions[role])
		actual, _ := json.Marshal(got.Permissions)
		if !bytes.Equal(want, actual) {
			t.Fatal("permissions differ from current role")
		}
		for _, secret := range []string{f.bootstrap.Token, f.raw, f.csrf} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatal("identity response leaked a credential")
			}
		}
	}
	// Bootstrap deliberately creates the organization's owner, not an admin.
	check("owner")
	for _, query := range []string{"role=admin", "organization_id=other", "limit=1", "x=1&x=2", "x=%ZZ", "x=one;two"} {
		f.request("GET", "/api/v1/me?"+query, "", nil, 400)
	}
	if _, e := f.store.Pool.Exec(context.Background(), "UPDATE memberships SET role='viewer' WHERE organization_id=$1 AND principal_id=$2", f.bootstrap.OrganizationID, f.bootstrap.PrincipalID); e != nil {
		t.Fatal(e)
	}
	check("viewer")
	f.request("GET", "/api/v1/me", "", func(r *http.Request) { r.Header.Set("X-Organization-ID", f.other.OrganizationID) }, 401)
	if _, e := f.store.Pool.Exec(context.Background(), "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", f.bootstrap.OrganizationID, f.bootstrap.PrincipalID); e != nil {
		t.Fatal(e)
	}
	f.request("GET", "/api/v1/me", "", nil, 401)
}

func TestIndependentMaterialHTTPQueriesAreExactAndUnambiguous(t *testing.T) {
	f := independentBrowserAPI(t)
	task := independentMaterialHTTPTask(t, f)
	base := "/api/v1/tasks/" + task.ID
	_, raw := f.request("POST", base+"/materials", `{"name":"query fixture","media_type":"text/plain","content":"exact"}`, nil, 201)
	var created struct {
		Data domain.Material `json:"data"`
	}
	if json.Unmarshal(raw, &created) != nil || created.Data.ID == "" {
		t.Fatal("missing material")
	}
	head := base + "/materials/" + created.Data.ID
	for _, path := range []string{base + "/materials", head + "/revisions", base + "/artifacts"} {
		for _, query := range []string{"unknown=1", "limit=1&limit=2", "cursor=a&cursor=b", "limit=%ZZ", "limit=1;extra=2"} {
			f.request("GET", path+"?"+query, "", nil, 400)
		}
	}
	for _, path := range []string{head, head + "/revisions/1"} {
		for _, query := range []string{"version=1", "limit=1", "unknown=1"} {
			f.request("GET", path+"?"+query, "", nil, 400)
		}
	}
	for _, query := range []string{"version=", "version=1&version=2", "version=1&unknown=1", "version=%ZZ"} {
		f.request("GET", base+"/artifacts/art_unknown?"+query, "", nil, 400)
	}
	for _, item := range []struct{ path, body string }{
		{base + "/materials", `{"name":"reject query","media_type":"text/plain","content":"text"}`},
		{head + "/revisions", `{"expected_version":1,"content":"changed","reason":"Explicit"}`},
		{head + "/publish", `{"expected_task_version":1,"expected_material_version":1,"content_digest":"` + created.Data.HeadContentDigest + `","parent_candidate_id":"cand_missing"}`},
	} {
		f.request("POST", item.path+"?unexpected=1", item.body, nil, 400)
	}
	f.request("GET", head+"/revisions/1", "", nil, 200)
}
