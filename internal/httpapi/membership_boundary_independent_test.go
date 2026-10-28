// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/identity"
	"agentplatform.local/platform/internal/storage"
)

func independentMembershipHTTPDecode(t *testing.T, body []byte) domain.OrganizationMember {
	t.Helper()
	var envelope struct {
		Data domain.OrganizationMember `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Data.PrincipalID == "" || envelope.Data.PrincipalKind != domain.PrincipalHuman {
		t.Fatal("missing public membership result")
	}
	return envelope.Data
}
func independentMembershipHTTPCreate(t *testing.T, f *independentBrowserFixture, name, role string) domain.OrganizationMember {
	t.Helper()
	in, _ := json.Marshal(domain.CreateOrganizationMember{DisplayName: name, Role: role, Reason: "Independent public HTTP provisioning"})
	_, body := f.request("POST", "/api/v1/members", string(in), nil, 201)
	return independentMembershipHTTPDecode(t, body)
}
func independentMembershipHTTPBody(v domain.OrganizationMember, active bool, role string) string {
	in, _ := json.Marshal(domain.UpdateOrganizationMember{ExpectedVersion: v.Version, DisplayName: v.DisplayName, Role: role, Active: &active, Reason: "Independent public HTTP lifecycle transition"})
	return string(in)
}
func independentMembershipHTTPBearer(raw string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Del("Cookie"); r.Header.Set("Authorization", "Bearer "+raw) }
}
func independentMembershipHTTPSession(t *testing.T, f *independentBrowserFixture, pid, subject string) string {
	t.Helper()
	ctx := context.Background()
	var issuer string
	if err := f.store.Pool.QueryRow(ctx, "SELECT issuer FROM identity_mappings WHERE principal_id=$1", f.bootstrap.PrincipalID).Scan(&issuer); err != nil {
		t.Fatal(err)
	}
	if err := f.store.LinkIdentity(ctx, issuer, subject, pid); err != nil {
		t.Fatal(err)
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		t.Fatal(err)
	}
	raw := base64.RawURLEncoding.EncodeToString(secret[:])
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := f.store.CreateSession(ctx, identity.BrowserSession{ID: domain.NewID("ses"), TokenHash: storage.Hash([]byte(raw)), PrincipalID: pid, Issuer: issuer, Subject: subject, AuthenticatedAt: now, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, ""); err != nil {
		t.Fatal(err)
	}
	return raw
}
func independentMembershipPublicShape(t *testing.T, body []byte, secrets ...string) {
	t.Helper()
	var envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		t.Fatal("invalid membership envelope")
	}
	allowed := map[string]bool{"organization_id": true, "principal_id": true, "principal_kind": true, "display_name": true, "role": true, "active": true, "principal_active": true, "version": true, "created_at": true, "updated_at": true, "credentials_revoked_at": true, "available_commands": true, "assignable_roles": true}
	for key := range envelope.Data {
		if !allowed[key] {
			t.Fatal("membership response exposed a private or unknown field", key)
		}
	}
	var kind string
	if json.Unmarshal(envelope.Data["principal_kind"], &kind) != nil || kind != domain.PrincipalHuman {
		t.Fatal("human membership response has no exact human identity kind")
	}
	for _, secret := range secrets {
		if secret != "" && bytes.Contains(body, []byte(secret)) {
			t.Fatal("membership response leaked a fixture credential")
		}
	}
}

func TestIndependentMembershipHTTPRequiresExplicitInputsAndExactQueries(t *testing.T) {
	f := independentBrowserAPI(t)
	v := independentMembershipHTTPCreate(t, f, "Public target", "member")
	base := "/api/v1/members/" + v.PrincipalID
	for _, bad := range []string{
		`{"expected_version":1,"display_name":"changed","role":"member","reason":"Missing active"}`,
		`{"expected_version":1,"display_name":"changed","role":"member","active":null,"reason":"Null active"}`,
		`{"expected_version":1,"display_name":"changed","role":"member","active":true,"active":false,"reason":"Duplicate active"}`,
		`{"expected_version":1,"display_name":"changed","role":"member","active":"false","reason":"String active"}`,
		`{"expected_version":1,"display_name":"changed","role":"member","active":true,"reason":"Injected epoch","security_epoch":9}`,
		`{"expected_version":1,"display_name":"changed","role":"member","active":true,"reason":"Injected cutoff","credential_cutoff":99999}`,
		`{"expected_version":1,"display_name":"\ud800","role":"member","active":true,"reason":"Invalid Unicode"}`,
	} {
		f.request("POST", base+"/update", bad, nil, 400)
	}
	for _, bad := range []string{
		`{"display_name":"new","role":"member","reason":"Injected identity","principal_id":"existing"}`,
		`{"display_name":"new","role":"member","reason":"Injected org","organization_id":"other"}`,
		`{"display_name":"new","role":"member","reason":"Email adoption","email":"someone@example.test"}`,
		`{"display_name":"new","role":"member","reason":"Subject adoption","issuer":"https://issuer.example.test","subject":"victim"}`,
		`{"display_name":"new","role":"member","reason":"Injected credential","credential_generation":9007199254740993}`,
	} {
		f.request("POST", "/api/v1/members", bad, nil, 400)
	}
	for _, path := range []string{"/api/v1/members", "/api/v1/membership-audit"} {
		for _, query := range []string{"unknown=1", "limit=", "limit=1&limit=2", "cursor=", "cursor=x&cursor=y", "limit=%ZZ", "limit=1;unknown=2"} {
			f.request("GET", path+"?"+query, "", nil, 400)
		}
	}
	for _, path := range []string{base, "/api/v1/members/capabilities"} {
		f.request("GET", path+"?limit=1", "", nil, 400)
	}
	f.request("GET", "/api/v1/membership-audit?principal_id=", "", nil, 400)
	f.request("GET", "/api/v1/membership-audit?principal_id=a&principal_id=b", "", nil, 400)
	for _, item := range []struct{ path, body string }{{"/api/v1/members", `{"display_name":"no","role":"member","reason":"No query"}`}, {base + "/update", independentMembershipHTTPBody(v, false, "member")}, {base + "/revoke-credentials", `{"expected_version":1,"reason":"No query"}`}} {
		f.request("POST", item.path+"?x=1", item.body, nil, 400)
	}
	_, body := f.request("GET", base, "", nil, 200)
	current := independentMembershipHTTPDecode(t, body)
	if current.Version != 1 || !current.Active {
		t.Fatal("rejected HTTP input changed membership")
	}
	independentMembershipPublicShape(t, body, f.bootstrap.Token, f.raw, f.csrf)
	var audit, principals int
	if err := f.store.Pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM organization_membership_audit WHERE organization_id=$1),(SELECT count(*) FROM principals)", f.bootstrap.OrganizationID).Scan(&audit, &principals); err != nil || audit != 1 || principals != 3 {
		t.Fatal("rejected HTTP input left partial identity/audit", audit, principals, err)
	}
}

func TestIndependentMembershipHTTPEnforcesOwnerSelfTenantAndCSRFAuthority(t *testing.T) {
	f := independentBrowserAPI(t)
	admin := independentMembershipHTTPCreate(t, f, "Public admin", "admin")
	target := independentMembershipHTTPCreate(t, f, "Public member", "member")
	ctx := context.Background()
	raw := domain.NewID("independent_private_admin_token")
	if _, err := f.store.Pool.Exec(ctx, "INSERT INTO access_tokens(token_hash,principal_id) VALUES($1,$2)", storage.Hash([]byte(raw)), admin.PrincipalID); err != nil {
		t.Fatal(err)
	}
	_, body := f.request("GET", "/api/v1/members/"+f.bootstrap.PrincipalID, "", nil, 200)
	owner := independentMembershipHTTPDecode(t, body)
	if len(owner.AvailableCommands) != 0 || len(owner.AssignableRoles) != 0 {
		t.Fatal("self management was advertised")
	}
	ownerPath := "/api/v1/members/" + owner.PrincipalID
	f.request("POST", ownerPath+"/update", independentMembershipHTTPBody(owner, false, owner.Role), nil, 409)
	f.request("POST", ownerPath+"/revoke-credentials", `{"expected_version":1,"reason":"Self revocation"}`, nil, 409)
	f.request("POST", ownerPath+"/update", independentMembershipHTTPBody(owner, true, "admin"), independentMembershipHTTPBearer(raw), 403)
	f.request("POST", ownerPath+"/revoke-credentials", `{"expected_version":1,"reason":"Owner credential attack"}`, independentMembershipHTTPBearer(raw), 403)
	f.request("POST", "/api/v1/members", `{"display_name":"Illegal owner","role":"owner","reason":"Escalation"}`, independentMembershipHTTPBearer(raw), 403)
	foreign := "/api/v1/members/" + f.other.PrincipalID
	f.request("GET", foreign, "", nil, 404)
	f.request("POST", foreign+"/update", independentMembershipHTTPBody(target, false, "member"), nil, 404)
	f.request("POST", foreign+"/revoke-credentials", `{"expected_version":1,"reason":"Foreign revoke"}`, nil, 404)
	f.request("GET", "/api/v1/membership-audit?principal_id="+f.other.PrincipalID, "", nil, 404)
	f.request("POST", "/api/v1/members/"+target.PrincipalID+"/update", independentMembershipHTTPBody(target, false, target.Role), func(r *http.Request) { r.Header.Del("X-CSRF-Token") }, 403)
	f.request("GET", "/api/v1/members", "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+raw) }, 401)
	f.request("GET", "/api/v1/members", "", func(r *http.Request) { r.Header.Add("X-Organization-ID", f.other.OrganizationID) }, 401)
	if _, err := f.store.Pool.Exec(ctx, "UPDATE memberships SET role='member' WHERE organization_id=$1 AND principal_id=$2", f.bootstrap.OrganizationID, admin.PrincipalID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/members", "/api/v1/members/" + target.PrincipalID, "/api/v1/membership-audit"} {
		f.request("GET", path, "", independentMembershipHTTPBearer(raw), 403)
	}
	f.request("POST", "/api/v1/members", `{"display_name":"No admin","role":"member","reason":"Current role check"}`, independentMembershipHTTPBearer(raw), 403)
}

func TestIndependentMembershipHTTPReenablePreservesOtherOrgAndRequiresFreshLogin(t *testing.T) {
	f := independentBrowserAPI(t)
	v := independentMembershipHTTPCreate(t, f, "Shared browser principal", "member")
	ctx := context.Background()
	if _, err := f.store.Pool.Exec(ctx, "INSERT INTO memberships(organization_id,principal_id,role,display_name) VALUES($1,$2,'viewer','Independent B label')", f.other.OrganizationID, v.PrincipalID); err != nil {
		t.Fatal(err)
	}
	old := independentMembershipHTTPSession(t, f, v.PrincipalID, "independent-http-shared")
	user := func(org, cookie string) func(*http.Request) {
		return func(r *http.Request) {
			r.Header.Set("Cookie", identity.SessionCookieName+"="+cookie)
			r.Header.Set("X-Organization-ID", org)
		}
	}
	f.request("GET", "/api/v1/me", "", user(f.bootstrap.OrganizationID, old), 200)
	path := "/api/v1/members/" + v.PrincipalID
	_, body := f.request("POST", path+"/update", independentMembershipHTTPBody(v, false, v.Role), nil, 200)
	v = independentMembershipHTTPDecode(t, body)
	f.request("GET", "/api/v1/me", "", user(f.bootstrap.OrganizationID, old), 401)
	f.request("GET", "/api/v1/me", "", user(f.other.OrganizationID, old), 200)
	_, body = f.request("POST", path+"/update", independentMembershipHTTPBody(v, true, v.Role), nil, 200)
	v = independentMembershipHTTPDecode(t, body)
	f.request("GET", "/api/v1/me", "", user(f.bootstrap.OrganizationID, old), 401)
	f.request("GET", "/api/v1/me", "", user(f.other.OrganizationID, old), 200)
	_, orgs := f.request("GET", "/auth/organizations", "", user(f.bootstrap.OrganizationID, old), 200)
	if bytes.Contains(orgs, []byte(f.bootstrap.OrganizationID)) || !bytes.Contains(orgs, []byte(f.other.OrganizationID)) {
		t.Fatal("public organization discovery resurrected old access")
	}
	fresh := independentMembershipHTTPSession(t, f, v.PrincipalID, "independent-http-shared")
	f.request("GET", "/api/v1/me", "", user(f.bootstrap.OrganizationID, fresh), 200)
	_, body = f.request("GET", path, "", nil, 200)
	independentMembershipPublicShape(t, body, old, fresh, f.bootstrap.Token, f.raw)
	for _, private := range []string{"credential_generation", "credential_cutoff", "security_epoch", "issuer", "subject", "token_hash"} {
		if strings.Contains(string(body), `"`+private+`"`) {
			t.Fatal("private lifecycle state escaped HTTP boundary", private)
		}
	}
}
