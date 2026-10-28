// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
	"agentplatform.local/platform/internal/storage"
)

type independentCapabilitiesTransport func(*http.Request) (*http.Response, error)

func (f independentCapabilitiesTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func independentCapabilitiesDecode(t *testing.T, raw []byte) domain.ProductCapabilities {
	t.Helper()
	var envelope struct {
		Data domain.ProductCapabilities `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Data.CatalogVersion != "1" {
		t.Fatal("missing capability observation")
	}
	return envelope.Data
}

func TestIndependentCapabilitiesHTTPReadsNoExternalEffectsAndCannotEscalateService(t *testing.T) {
	f := independentBrowserAPI(t)
	ctx := context.Background()
	account := independentSecurityHTTPService(t, f, "member")
	token, _, err := f.store.IssueServiceToken(ctx, account.OrganizationID, account.PrincipalID, "viewer", time.Hour, "Independent ceiling test")
	if err != nil {
		t.Fatal(err)
	}
	var external atomic.Int64
	oldTransport := http.DefaultTransport
	http.DefaultTransport = independentCapabilitiesTransport(func(*http.Request) (*http.Response, error) {
		external.Add(1)
		return nil, errors.New("catalogue must not probe peers")
	})
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	counts := func() string {
		var result string
		if err := f.store.Pool.QueryRow(ctx, `SELECT json_build_array((SELECT count(*) FROM tasks),(SELECT count(*) FROM operations),(SELECT count(*) FROM actions),(SELECT count(*) FROM reservations),(SELECT count(*) FROM ledger_entries),(SELECT count(*) FROM configuration_plans),(SELECT count(*) FROM configuration_verifications),(SELECT count(*) FROM configuration_grants),(SELECT count(*) FROM idempotency_keys))::text`).Scan(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := counts()
	_, raw := f.request("GET", "/api/v1/capabilities", "", independentMembershipHTTPBearer(token), 200)
	v := independentCapabilitiesDecode(t, raw)
	if v.Identity.PrincipalID != account.PrincipalID || v.Identity.OrganizationID != account.OrganizationID || v.Identity.PrincipalKind != "service" || v.Identity.Role != "viewer" || !reflect.DeepEqual(v.Identity.Permissions, domain.EffectivePermissions(domain.Principal{Kind: "service", Role: "viewer"})) {
		t.Fatal("catalogue ignored immutable service key ceiling or active tenant")
	}
	independentSecurityHTTPNoSecrets(t, raw, token, f.bootstrap.Token, f.raw, f.csrf)
	if external.Load() != 0 || counts() != before {
		t.Fatal("capability read probed a peer, queued work, reserved funds or wrote durable command state")
	}
	// Discovery changes no route authority. Management metadata is not self-readable.
	for _, path := range []string{"/api/v1/login-policy", "/api/v1/service-accounts/" + account.PrincipalID, "/api/v1/service-accounts/" + account.PrincipalID + "/keys"} {
		f.request("GET", path, "", independentMembershipHTTPBearer(token), 403)
	}
	f.request("POST", "/api/v1/service-accounts", `{"display_name":"No self promotion","role":"member","reason":"Catalogue is not a grant"}`, independentMembershipHTTPBearer(token), 403)
	f.request("GET", "/api/v1/configuration/catalog", "", independentMembershipHTTPBearer(token), 200)
	f.request("GET", "/api/v1/configuration/destinations", "", independentMembershipHTTPBearer(token), 403)
	// A different authorized tenant gets its own identity observation; the first
	// service's organization selector never becomes proof of membership.
	f.request("GET", "/api/v1/capabilities", "", func(r *http.Request) {
		independentMembershipHTTPBearer(token)(r)
		r.Header.Set("X-Organization-ID", f.other.OrganizationID)
	}, 401)
	_, otherRaw := f.request("GET", "/api/v1/capabilities", "", func(r *http.Request) {
		independentMembershipHTTPBearer(f.other.Token)(r)
		r.Header.Set("X-Organization-ID", f.other.OrganizationID)
	}, 200)
	if other := independentCapabilitiesDecode(t, otherRaw); other.Identity.OrganizationID != f.other.OrganizationID || other.Identity.PrincipalID != f.other.PrincipalID {
		t.Fatal("tenant/principal observation was reused across callers")
	}
}

func TestIndependentCapabilitiesHTTPRejectsQueriesBodiesVersionsAndRevokedCredentials(t *testing.T) {
	f := independentBrowserAPI(t)
	for _, query := range []string{"role=owner", "organization_id=other", "limit=1", "x=1&x=2", "x=%ZZ", "x=one;two"} {
		f.request("GET", "/api/v1/capabilities?"+query, "", nil, 400)
	}
	f.request("GET", "/api/v1/capabilities", `{"principal_id":"other","role":"owner"}`, nil, 400)
	f.request("GET", "/api/v1/capabilities", "", func(r *http.Request) { r.Header.Del(domain.APIContractHeader) }, 409)
	f.request("GET", "/api/v1/capabilities", "", func(r *http.Request) { r.Header.Set(domain.APIContractHeader, "0.12.0") }, 409)
	f.request("GET", "/api/v1/capabilities", "", func(r *http.Request) { r.Header.Add(domain.APIContractHeader, domain.ContractVersion) }, 409)
	ctx := context.Background()
	account := independentSecurityHTTPService(t, f, "member")
	token, key, err := f.store.IssueServiceToken(ctx, account.OrganizationID, account.PrincipalID, "member", time.Hour, "Independent catalogue revocation")
	if err != nil {
		t.Fatal(err)
	}
	_, raw := f.request("GET", "/api/v1/capabilities", "", independentMembershipHTTPBearer(token), 200)
	prior := independentCapabilitiesDecode(t, raw)
	if prior.Identity.Role != "member" {
		t.Fatal("wrong initial membership")
	}
	_, err = f.store.Pool.Exec(ctx, "UPDATE memberships SET role='viewer' WHERE organization_id=$1 AND principal_id=$2", account.OrganizationID, account.PrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	_, raw = f.request("GET", "/api/v1/capabilities", "", independentMembershipHTTPBearer(token), 200)
	if current := independentCapabilitiesDecode(t, raw); current.Identity.Role != "viewer" || strings.Join(current.Identity.Permissions, ",") == strings.Join(prior.Identity.Permissions, ",") {
		t.Fatal("catalogue retained stale role permissions")
	}
	in, _ := json.Marshal(domain.RevokeServiceKey{ExpectedVersion: key.Version, Reason: "Independent revocation after observation"})
	f.request("POST", "/api/v1/service-accounts/"+account.PrincipalID+"/keys/"+key.ID+"/revoke", string(in), nil, 200)
	f.request("GET", "/api/v1/capabilities", "", independentMembershipHTTPBearer(token), 401)
}

func TestIndependentCapabilitiesHTTPConfigurationGrantIsSeparateAndRevokedHistoryRemainsReadable(t *testing.T) {
	f := independentBrowserAPI(t)
	ctx := context.Background()
	account := independentSecurityHTTPService(t, f, "member")
	token, _, err := f.store.IssueServiceToken(ctx, account.OrganizationID, account.PrincipalID, "member", time.Hour, "Independent exact configuration scope")
	if err != nil {
		t.Fatal(err)
	}
	_, beforeRaw := f.request("GET", "/api/v1/capabilities", "", independentMembershipHTTPBearer(token), 200)
	before := independentCapabilitiesDecode(t, beforeRaw)
	f.request("GET", "/api/v1/configuration/destinations", "", independentMembershipHTTPBearer(token), 403)
	// A static trusted-host profile is enough. No peer is started or contacted;
	// this test never starts a worker or requests connection verification.
	endpoint := "https://unused-capability-fixture.invalid/mcp"
	purpose, err := f.store.InstallCredentialPurposeProfile(ctx, domain.CredentialPurposeProfile{ID: domain.NewID("purpose"), OrganizationID: account.OrganizationID, Version: 1, Kind: "mcp", EndpointDigest: storage.Hash([]byte(endpoint)), CredentialEnvironment: "PLATFORM_MCP_KEY_CAPABILITY_FIXTURE", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	destination, err := f.store.InstallConfigurationDestination(ctx, domain.ConfigurationDestinationProfile{Destination: domain.ConfigurationDestination{ID: domain.NewID("destination"), OrganizationID: account.OrganizationID, Version: 1, Kind: "mcp", Name: "Never contacted capability fixture", TargetID: domain.NewID("mcp"), CredentialPurposeID: purpose.ID, CredentialPurposeVersion: 1, RequiresApproval: true, DiscoveryUnmeteredApproved: true, Active: true}, MCP: &domain.MCPConnectionProfile{Endpoint: endpoint, TimeoutSeconds: 1}})
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(domain.CreateConfigurationGrant{SubjectID: account.PrincipalID, Actions: []string{"inspect"}, Destinations: []domain.ConfigurationDestinationRef{{ID: destination.Destination.ID, Version: 1}}, TTLSeconds: 300, MaxPlans: 1, MaxVerifications: 1, MaxAllocatedChargeMicroUSD: 0, Reason: "Independent explicit owner consent"})
	_, raw := f.request("POST", "/api/v1/configuration/grants", string(input), nil, 201)
	var granted struct {
		Data domain.CapabilityEnvelope `json:"data"`
	}
	if json.Unmarshal(raw, &granted) != nil || granted.Data.ID == "" {
		t.Fatal("missing exact delegated grant")
	}
	f.request("GET", "/api/v1/configuration/destinations", "", independentMembershipHTTPBearer(token), 200)
	_, afterRaw := f.request("GET", "/api/v1/capabilities", "", independentMembershipHTTPBearer(token), 200)
	after := independentCapabilitiesDecode(t, afterRaw)
	if !reflect.DeepEqual(before.Identity, after.Identity) || !reflect.DeepEqual(before.Operations, after.Operations) {
		t.Fatal("static support/role catalogue incorrectly turned configuration grant into role authority")
	}
	revoke, _ := json.Marshal(domain.RevokeConfigurationGrant{ExpectedVersion: granted.Data.Version, Reason: "Subject revokes exact delegated execution consent"})
	f.request("POST", "/api/v1/configuration/grants/"+granted.Data.ID+"/revoke", string(revoke), independentMembershipHTTPBearer(token), 200)
	f.request("GET", "/api/v1/configuration/destinations", "", independentMembershipHTTPBearer(token), 403)
	f.request("GET", "/api/v1/configuration/grants/"+granted.Data.ID, "", independentMembershipHTTPBearer(token), 200)
	f.request("GET", "/api/v1/capabilities", "", independentMembershipHTTPBearer(token), 200)
}

// Exercise the actual backend allowlist independently of the tool catalogue.
// No socket, PostgreSQL or fake route authorization is needed for this boundary.
func TestIndependentCapabilitiesMCPBackendAllowsOnlyBodylessUnkeyedFixedRead(t *testing.T) {
	var reached int
	credentials := mcpbridge.Credentials{Token: "independent-synthetic-mcp-capability-key", OrganizationID: "org_independent_capability"}
	backend := mcpbridge.HandlerBackend(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/capabilities" || r.URL.RawQuery != "" || r.ContentLength != 0 || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("Authorization") != "Bearer "+credentials.Token || r.Header.Get("X-Organization-ID") != credentials.OrganizationID || r.Header.Get(domain.APIContractHeader) != domain.ContractVersion {
			t.Error("backend changed fixed read shape or authenticated context")
		}
		w.Header().Set(domain.APIContractHeader, domain.ContractVersion)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"read_only":true}}`))
	}))
	ctx := context.Background()
	valid := mcpbridge.Request{Method: http.MethodGet, Path: "/api/v1/capabilities"}
	if response, err := backend.Call(ctx, credentials, valid); err != nil || response.Status != 200 || reached != 1 {
		t.Fatal("fixed capability read was not passed through the normal handler")
	}
	for _, bad := range []mcpbridge.Request{
		{Method: http.MethodPost, Path: valid.Path, Body: json.RawMessage(`{}`), IdempotencyKey: "explicit-but-unsupported"},
		{Method: http.MethodGet, Path: valid.Path, Query: url.Values{"limit": []string{"1"}}},
		{Method: http.MethodGet, Path: valid.Path, Query: url.Values{"organization_id": []string{"org_other"}}},
		{Method: http.MethodGet, Path: valid.Path, Body: json.RawMessage(`{}`)},
		{Method: http.MethodGet, Path: valid.Path, IdempotencyKey: "no-mutation-key"},
		{Method: http.MethodGet, Path: valid.Path + "/apply"},
	} {
		if _, err := backend.Call(ctx, credentials, bad); err == nil || reached != 1 {
			t.Errorf("backend admitted unsupported capability request: method=%s path=%s", bad.Method, bad.Path)
		}
	}
}
