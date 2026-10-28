// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/identity"
)

func independentSecurityHTTPService(t *testing.T, f *independentBrowserFixture, role string) domain.ServiceAccount {
	t.Helper()
	in, _ := json.Marshal(domain.CreateServiceAccount{DisplayName: "Independent TLS service", Role: role, Reason: "Independent public service provisioning"})
	_, body := f.request("POST", "/api/v1/service-accounts", string(in), nil, 201)
	var out struct {
		Data domain.ServiceAccount `json:"data"`
	}
	if json.Unmarshal(body, &out) != nil || out.Data.PrincipalID == "" || out.Data.PrincipalKind != domain.PrincipalService {
		t.Fatal("missing public service identity")
	}
	return out.Data
}

func independentSecurityHTTPPolicy(t *testing.T, f *independentBrowserFixture) domain.OrganizationLoginPolicy {
	t.Helper()
	_, body := f.request("GET", "/api/v1/login-policy", "", nil, 200)
	var out struct {
		Data domain.OrganizationLoginPolicy `json:"data"`
	}
	if json.Unmarshal(body, &out) != nil || out.Data.OrganizationID != f.bootstrap.OrganizationID {
		t.Fatal("missing scoped public policy")
	}
	return out.Data
}

func independentSecurityHTTPPolicyBody(v domain.OrganizationLoginPolicy, mode, issuer string, enabled bool) string {
	in, _ := json.Marshal(domain.UpdateOrganizationLoginPolicy{ExpectedVersion: v.Version, Mode: mode, OIDCIssuer: issuer, ServiceAccessEnabled: &enabled, MaxServiceKeyTTLSeconds: v.MaxServiceKeyTTLSeconds, Reason: "Independent TLS security transition"})
	return string(in)
}

func independentSecurityHTTPDecodePolicy(t *testing.T, body []byte) domain.OrganizationLoginPolicy {
	t.Helper()
	var out struct {
		Data domain.OrganizationLoginPolicy `json:"data"`
	}
	if json.Unmarshal(body, &out) != nil || out.Data.OrganizationID == "" {
		t.Fatal("missing updated policy")
	}
	return out.Data
}

func independentSecurityHTTPNoSecrets(t *testing.T, body []byte, secrets ...string) {
	t.Helper()
	for _, field := range []string{"token_hash", "token", "secret", "credential_generation", "credential_cutoff", "human_authority_epoch", "service_authority_epoch", "security_epoch", "role_scopes"} {
		if bytes.Contains(body, []byte(`"`+field+`"`)) {
			t.Fatal("public security response exposed private credential/authority data", field)
		}
	}
	for _, secret := range secrets {
		if secret != "" && bytes.Contains(body, []byte(secret)) {
			t.Fatal("public security response leaked actual credential bytes")
		}
	}
}

func TestIndependentSecurityHTTPRejectsIdentityAdoptionAmbiguousInputsAndQueries(t *testing.T) {
	f := independentBrowserAPI(t)
	v := independentSecurityHTTPService(t, f, "member")
	base := "/api/v1/service-accounts/" + v.PrincipalID
	for _, bad := range []string{
		`{"display_name":"x","role":"owner","reason":"No service owner"}`,
		`{"display_name":"x","role":"admin","reason":"No service administrator"}`,
		`{"display_name":"x","role":"member","reason":"No adopted principal","principal_id":"existing"}`,
		`{"display_name":"x","role":"member","reason":"No scopes claim","scopes":["*"]}`,
		`{"display_name":"x","role":"member","reason":"No human conversion","principal_kind":"human"}`,
		`{"display_name":"x","role":"member","reason":"No external adoption","issuer":"https://issuer.example.test","subject":"existing"}`,
		`{"display_name":"x","role":"member","reason":"No other organization","organization_id":"other"}`,
	} {
		f.request("POST", "/api/v1/service-accounts", bad, nil, 400)
	}
	for _, bad := range []string{
		`{"expected_version":1,"display_name":"x","role":"member","reason":"Missing active"}`,
		`{"expected_version":1,"display_name":"x","role":"member","active":null,"reason":"Null active"}`,
		`{"expected_version":1,"display_name":"x","role":"member","active":true,"active":false,"reason":"Duplicate active"}`,
		`{"expected_version":1,"display_name":"x","role":"member","active":true,"reason":"Injected epoch","security_epoch":2}`,
	} {
		f.request("POST", base+"/update", bad, nil, 400)
	}
	for _, bad := range []string{
		`{"expected_version":1,"mode":"mixed","oidc_issuer":"","max_service_key_ttl_seconds":3600,"reason":"Missing enabled"}`,
		`{"expected_version":1,"mode":"mixed","oidc_issuer":"","service_access_enabled":null,"max_service_key_ttl_seconds":3600,"reason":"Null enabled"}`,
		`{"expected_version":1,"mode":"mixed","oidc_issuer":"","service_access_enabled":true,"service_access_enabled":false,"max_service_key_ttl_seconds":3600,"reason":"Duplicate enabled"}`,
		`{"expected_version":1,"mode":"oidc_required","oidc_issuer":"https://fake.example.test","service_access_enabled":true,"max_service_key_ttl_seconds":3600,"reason":"Client readiness spoof","ready":true}`,
		`{"expected_version":1,"mode":"mixed","oidc_issuer":"","service_access_enabled":true,"max_service_key_ttl_seconds":3600,"reason":"Injected cutoff","human_credential_cutoff":99}`,
	} {
		f.request("POST", "/api/v1/login-policy/update", bad, nil, 400)
	}
	for _, path := range []string{"/api/v1/service-accounts", base + "/keys", "/api/v1/security-audit"} {
		for _, query := range []string{"unknown=1", "limit=", "limit=1&limit=2", "cursor=", "cursor=x&cursor=y", "limit=%ZZ", "limit=1;unknown=2"} {
			f.request("GET", path+"?"+query, "", nil, 400)
		}
	}
	for _, path := range []string{"/api/v1/login-policy", base} {
		f.request("GET", path+"?limit=1", "", nil, 400)
	}
	f.request("GET", "/api/v1/security-audit?principal_id=", "", nil, 400)
	f.request("POST", base+"/update?x=1", `{"expected_version":1,"display_name":"x","role":"member","active":false,"reason":"No query"}`, nil, 400)
	f.request("POST", "/api/v1/login-policy/update?x=1", independentSecurityHTTPPolicyBody(independentSecurityHTTPPolicy(t, f), "mixed", "", false), nil, 400)
	_, body := f.request("GET", base, "", nil, 200)
	independentSecurityHTTPNoSecrets(t, body, f.bootstrap.Token, f.raw, f.csrf)
	var envelope struct {
		Data domain.ServiceAccount `json:"data"`
	}
	_ = json.Unmarshal(body, &envelope)
	if envelope.Data.Version != 1 || !envelope.Data.Active {
		t.Fatal("invalid HTTP input mutated service authority")
	}
}

func TestIndependentSecurityHTTPActualOIDCPolicyRoundtripAndOrganizationIsolation(t *testing.T) {
	f := independentBrowserAPI(t)
	ctx := context.Background()
	if _, err := f.store.Pool.Exec(ctx, "INSERT INTO memberships(organization_id,principal_id,display_name,role) VALUES($1,$2,'Same human in B','owner')", f.other.OrganizationID, f.bootstrap.PrincipalID); err != nil {
		t.Fatal(err)
	}
	policy := independentSecurityHTTPPolicy(t, f)
	if !policy.CanRequireOIDC || policy.ConfiguredOIDCIssuer == "" || len(policy.ReadinessReasons) != 0 {
		t.Fatal("actual initialized LoginService did not prove readiness", policy)
	}
	enable := independentSecurityHTTPPolicyBody(policy, "oidc_required", policy.ConfiguredOIDCIssuer, policy.ServiceAccessEnabled)
	f.request("POST", "/api/v1/login-policy/update", enable, independentMembershipHTTPBearer(f.bootstrap.Token), 403)
	f.request("POST", "/api/v1/login-policy/update", strings.Replace(enable, policy.ConfiguredOIDCIssuer, "https://wrong-issuer.example.test", 1), nil, 403)
	f.request("POST", "/api/v1/login-policy/update", enable, func(r *http.Request) { r.Header.Del("X-CSRF-Token") }, 403)
	_, body := f.request("POST", "/api/v1/login-policy/update", enable, nil, 200)
	policy = independentSecurityHTTPDecodePolicy(t, body)
	f.request("GET", "/api/v1/me", "", independentMembershipHTTPBearer(f.bootstrap.Token), 401)
	f.request("GET", "/api/v1/me", "", nil, 200)
	f.request("GET", "/api/v1/me", "", func(r *http.Request) {
		r.Header.Del("Cookie")
		r.Header.Set("Authorization", "Bearer "+f.bootstrap.Token)
		r.Header.Set("X-Organization-ID", f.other.OrganizationID)
	}, 200)
	_, body = f.request("POST", "/api/v1/login-policy/update", independentSecurityHTTPPolicyBody(policy, "mixed", "", policy.ServiceAccessEnabled), nil, 200)
	policy = independentSecurityHTTPDecodePolicy(t, body)
	f.request("GET", "/api/v1/me", "", independentMembershipHTTPBearer(f.bootstrap.Token), 401)
	response, rotate := f.request("POST", identity.SessionRotatePath, "", nil, 200)
	independentSecurityHTTPNoSecrets(t, body, f.bootstrap.Token)
	var session struct {
		Data struct {
			CSRF string `json:"csrf_token"`
		} `json:"data"`
	}
	if json.Unmarshal(rotate, &session) != nil || session.Data.CSRF == "" {
		t.Fatal("missing rotated browser CSRF")
	}
	// Rotation is a browser credential only and cannot mint a fresh human bearer.
	f.request("GET", "/api/v1/me", "", independentMembershipHTTPBearer(f.bootstrap.Token), 401)
	for _, cookie := range response.Cookies() {
		if cookie.Name == identity.SessionCookieName {
			f.raw = cookie.Value
		}
	}
	f.csrf = session.Data.CSRF
	f.request("GET", "/api/v1/me", "", nil, 200)
	if policy.Mode != "mixed" || policy.Version != 3 {
		t.Fatal("policy cycle version did not record both transitions")
	}
}

func TestIndependentSecurityHTTPKeyMetadataIsScopedSecretlessAndRevokeReplayRechecksAuthority(t *testing.T) {
	f := independentBrowserAPI(t)
	ctx := context.Background()
	v := independentSecurityHTTPService(t, f, "member")
	otherService := independentSecurityHTTPService(t, f, "viewer")
	secret, key, err := f.store.IssueServiceToken(ctx, v.OrganizationID, v.PrincipalID, "member", time.Hour, "Independent private delivery")
	if err != nil {
		t.Fatal(err)
	}
	_, currentIdentity := f.request("GET", "/api/v1/me", "", independentMembershipHTTPBearer(secret), 200)
	var current struct {
		Data domain.CurrentIdentity `json:"data"`
	}
	if json.Unmarshal(currentIdentity, &current) != nil || current.Data.PrincipalKind != domain.PrincipalService || current.Data.PrincipalID != v.PrincipalID || current.Data.OrganizationID != v.OrganizationID || current.Data.Role != "member" {
		t.Fatal("service current identity was represented as a human or another membership")
	}
	canCreate := false
	for _, permission := range current.Data.Permissions {
		if permission == "tasks.create" {
			canCreate = true
		}
		if permission == "tasks.review_own" || permission == "tasks.review_any" || permission == "identity.manage" || permission == "organization.manage" || permission == "actions.approve_any" {
			t.Fatal("service current identity advertised human-review/admin authority", permission)
		}
	}
	if !canCreate {
		t.Fatal("member service identity lost its declared task creation authority")
	}
	independentSecurityHTTPNoSecrets(t, currentIdentity, secret, f.bootstrap.Token, f.raw)
	base := "/api/v1/service-accounts/" + v.PrincipalID + "/keys/" + key.ID
	_, body := f.request("GET", base, "", nil, 200)
	independentSecurityHTTPNoSecrets(t, body, secret, f.bootstrap.Token, f.raw)
	for _, path := range []string{"/api/v1/service-accounts/" + otherService.PrincipalID + "/keys/" + key.ID, "/api/v1/service-accounts/" + f.other.PrincipalID, "/api/v1/service-accounts/" + f.bootstrap.PrincipalID} {
		f.request("GET", path, "", nil, 404)
	}
	f.request("GET", base, "", func(r *http.Request) {
		r.Header.Set("X-Organization-ID", f.other.OrganizationID)
		r.Header.Del("Cookie")
		r.Header.Set("Authorization", "Bearer "+f.other.Token)
	}, 404)
	for _, path := range []string{"/api/v1/service-accounts", "/api/v1/login-policy", "/api/v1/security-audit"} {
		f.request("GET", path, "", independentMembershipHTTPBearer(secret), 403)
	}
	f.request("POST", "/api/v1/service-accounts", `{"display_name":"Self escalation","role":"member","reason":"A service cannot administer identities"}`, independentMembershipHTTPBearer(secret), 403)
	in := `{"expected_version":1,"reason":"Independent exact key revocation"}`
	idempotency := domain.NewID("key")
	change := func(r *http.Request) { r.Header.Set("Idempotency-Key", idempotency) }
	_, first := f.request("POST", base+"/revoke", in, change, 200)
	_, replay := f.request("POST", base+"/revoke", in, change, 200)
	var a, b struct {
		Data domain.ServiceKey `json:"data"`
	}
	if json.Unmarshal(first, &a) != nil || json.Unmarshal(replay, &b) != nil || !reflect.DeepEqual(a.Data, b.Data) {
		t.Fatal("idempotent revoke changed its complete public key snapshot")
	}
	f.request("POST", base+"/revoke", strings.Replace(in, "Independent exact", "Changed exact", 1), change, 409)
	f.request("GET", "/api/v1/me", "", independentMembershipHTTPBearer(secret), 401)
	_, audit := f.request("GET", "/api/v1/security-audit?principal_id="+v.PrincipalID, "", nil, 200)
	independentSecurityHTTPNoSecrets(t, audit, secret, f.bootstrap.Token, f.raw)
	var entries struct {
		Data []domain.OrganizationSecurityAuditEntry `json:"data"`
	}
	if json.Unmarshal(audit, &entries) != nil {
		t.Fatal("invalid public audit")
	}
	revokes := 0
	for _, e := range entries.Data {
		if e.OrganizationID != v.OrganizationID || e.TargetPrincipalID != v.PrincipalID {
			t.Fatal("filtered security audit crossed its exact subject")
		}
		if e.Action == "service_key.revoked" {
			revokes++
		}
	}
	if revokes != 1 {
		t.Fatal("replay duplicated revocation audit", revokes)
	}
	f.request("GET", "/api/v1/security-audit?principal_id="+f.other.PrincipalID, "", nil, 404)
}

func TestIndependentSecurityHTTPMappingsDoNotInventConfiguredLoginReadiness(t *testing.T) {
	f := independentBrowserAPI(t)
	// The same real database contains an active owner, mapping and browser
	// session, but this second TLS API instance has no initialized LoginService.
	server := httptest.NewTLSServer((&Server{Store: f.store}).Handler())
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 3 * time.Second
	request := func(method, path, body string, want int) []byte {
		t.Helper()
		r, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+f.bootstrap.Token)
		r.Header.Set("X-Organization-ID", f.bootstrap.OrganizationID)
		r.Header.Set(domain.APIContractHeader, domain.ContractVersion)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", domain.NewID("key"))
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal("actual TLS request failed", err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want {
			t.Fatal("unexpected unconfigured-login HTTP status", response.StatusCode, want)
		}
		return data
	}
	policy := independentSecurityHTTPDecodePolicy(t, request("GET", "/api/v1/login-policy", "", 200))
	configured := false
	for _, reason := range policy.ReadinessReasons {
		if reason == "oidc_not_configured" {
			configured = true
		}
	}
	if policy.CanRequireOIDC || policy.ConfiguredOIDCIssuer != "" || !configured {
		t.Fatal("database mappings were mistaken for runtime OIDC readiness", policy)
	}
	actualPolicy := independentSecurityHTTPPolicy(t, f)
	request("POST", "/api/v1/login-policy/update", independentSecurityHTTPPolicyBody(policy, "oidc_required", actualPolicy.ConfiguredOIDCIssuer, policy.ServiceAccessEnabled), 403)
	request("POST", "/api/v1/login-policy/update", independentSecurityHTTPPolicyBody(policy, "mixed", "", false), 200)
}
