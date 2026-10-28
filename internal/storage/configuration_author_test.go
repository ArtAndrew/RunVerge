// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
	"agentplatform.local/platform/internal/modelruntime"
)

type configurationAuthorFixture struct {
	s *Store
	p domain.Principal
	domain.ConfigurationDestinationProfile
	purpose domain.CredentialPurposeProfile
}

func configurationAuthorDecode[T any](t *testing.T, raw json.RawMessage, err error) T {
	t.Helper()
	var v T
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func configurationAuthorSetup(t *testing.T) configurationAuthorFixture {
	t.Helper()
	s, _ := testStore(t)
	p, _ := identity(t, s, 1_000_000)
	ctx := context.Background()
	endpoint := "https://configuration-author.invalid/mcp"
	purpose, err := s.InstallCredentialPurposeProfile(ctx, domain.CredentialPurposeProfile{ID: "author_mcp_purpose", OrganizationID: p.OrganizationID, Version: 1, Kind: "mcp", EndpointDigest: Hash([]byte(endpoint)), CredentialEnvironment: "PLATFORM_MCP_KEY_AUTHOR", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.InstallConfigurationDestination(ctx, domain.ConfigurationDestinationProfile{Destination: domain.ConfigurationDestination{ID: "author_mcp_destination", OrganizationID: p.OrganizationID, Version: 1, Kind: "mcp", Name: "Approved metadata endpoint", TargetID: "author_mcp", CredentialPurposeID: purpose.ID, CredentialPurposeVersion: purpose.Version, DiscoveryUnmeteredApproved: true, Active: true}, MCP: &domain.MCPConnectionProfile{Endpoint: endpoint, TimeoutSeconds: 2}})
	if err != nil {
		t.Fatal(err)
	}
	return configurationAuthorFixture{s: s, p: p, ConfigurationDestinationProfile: d, purpose: purpose}
}
func (f configurationAuthorFixture) account(t *testing.T, label string) (domain.Principal, domain.ServiceAccount) {
	t.Helper()
	a := securityAuthorAccount(t, f.s, f.p, "member")
	token, _, err := f.s.IssueServiceToken(context.Background(), f.p.OrganizationID, a.PrincipalID, "member", time.Minute, "Author bounded configuration actor "+label)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.s.Authenticate(context.Background(), token, f.p.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	return p, a
}
func (f configurationAuthorFixture) grant(t *testing.T, issuer domain.Principal, subject string, parent *domain.CapabilityEnvelope, plans int, refs ...domain.ConfigurationDestinationRef) domain.CapabilityEnvelope {
	t.Helper()
	if len(refs) == 0 {
		refs = []domain.ConfigurationDestinationRef{{ID: f.Destination.ID, Version: f.Destination.Version}}
	}
	in := domain.CreateConfigurationGrant{SubjectID: subject, Actions: append([]string{}, domain.ConfigurationActions...), Destinations: refs, TTLSeconds: 600, MaxPlans: plans, MaxVerifications: 4, MaxAllocatedChargeMicroUSD: 1000, Reason: "Explicit bounded author configuration delegation"}
	if parent != nil {
		in.ParentGrantID = parent.ID
		in.ExpectedParentVersion = parent.Version
		in.TTLSeconds = 300
	}
	raw, err := f.s.CreateConfigurationGrant(context.Background(), issuer, domain.NewID("key"), in)
	return configurationAuthorDecode[domain.CapabilityEnvelope](t, raw, err)
}
func (f configurationAuthorFixture) plan(t *testing.T, p domain.Principal, grant string, ops ...domain.ConfigurationOperationInput) domain.ConfigurationPlan {
	t.Helper()
	catalog, err := f.s.ConfigurationCatalog(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) == 0 {
		active := true
		ops = []domain.ConfigurationOperationInput{{ID: "connect", DestinationID: f.Destination.ID, DestinationVersion: f.Destination.Version, Active: &active}}
	}
	raw, err := f.s.CreateConfigurationPlan(context.Background(), p, domain.NewID("key"), domain.CreateConfigurationPlan{GrantID: grant, BaseRevision: catalog.Revision, TTLSeconds: 300, Operations: ops, Reason: "Exact author configuration intent"})
	return configurationAuthorDecode[domain.ConfigurationPlan](t, raw, err)
}
func (f configurationAuthorFixture) validate(t *testing.T, p domain.Principal, plan domain.ConfigurationPlan) domain.ConfigurationValidation {
	t.Helper()
	raw, err := f.s.ValidateConfigurationPlan(context.Background(), p, plan.ID, domain.NewID("key"), domain.ValidateConfigurationPlan{ExpectedVersion: plan.Version, PlanDigest: plan.Digest})
	return configurationAuthorDecode[domain.ConfigurationValidation](t, raw, err)
}
func (f configurationAuthorFixture) apply(t *testing.T, p domain.Principal, plan domain.ConfigurationPlan, validation domain.ConfigurationValidation) domain.ConfigurationPlan {
	t.Helper()
	raw, err := f.s.ApplyConfigurationPlan(context.Background(), p, plan.ID, domain.NewID("key"), domain.ApplyConfigurationPlan{ExpectedVersion: plan.Version, PlanDigest: plan.Digest, ValidationID: validation.ID, Reason: "Apply exact previously validated configuration"})
	return configurationAuthorDecode[domain.ConfigurationPlan](t, raw, err)
}
func (f configurationAuthorFixture) verify(t *testing.T, p domain.Principal, plan domain.ConfigurationPlan, operation string, charge int64) domain.ConfigurationVerification {
	t.Helper()
	ack := true
	raw, err := f.s.StartConfigurationVerification(context.Background(), p, plan.ID, domain.NewID("key"), domain.StartConfigurationVerification{ExpectedPlanVersion: plan.Version, PlanDigest: plan.Digest, OperationID: operation, MaxChargeMicroUSD: charge, AcknowledgeCost: &ack, Reason: "One explicit bounded connection check"})
	return configurationAuthorDecode[domain.ConfigurationVerification](t, raw, err)
}

func TestConfigurationAuthorHostProfilesNeverReactivateOrRebind(t *testing.T) {
	f := configurationAuthorSetup(t)
	ctx := context.Background()
	p2 := f.purpose
	p2.Version = 2
	p2.Active = true
	if _, err := f.s.InstallCredentialPurposeProfile(ctx, p2); err != nil {
		t.Fatal(err)
	}
	old, err := f.s.InstallCredentialPurposeProfile(ctx, f.purpose)
	if err != nil || old.Active {
		t.Fatal("same-version replay reactivated retired purpose", err)
	}
	d2 := f.ConfigurationDestinationProfile
	d2.Destination.Version = 2
	d2.Destination.CredentialPurposeVersion = 2
	d2.Destination.Active = true
	if _, err = f.s.InstallConfigurationDestination(ctx, d2); err != nil {
		t.Fatal(err)
	}
	oldDestination, err := f.s.InstallConfigurationDestination(ctx, f.ConfigurationDestinationProfile)
	if err != nil || oldDestination.Destination.Active {
		t.Fatal("same-version replay reactivated destination", err)
	}
	changed := d2
	changed.Destination.Version = 3
	changed.Destination.TargetID = "different_slot"
	_, err = f.s.InstallConfigurationDestination(ctx, changed)
	code(t, err, "conflict")
	changed = d2
	changed.Destination.Version = 3
	copied := *d2.MCP
	copied.Endpoint = "https://different-configuration-author.invalid/mcp"
	changed.MCP = &copied
	_, err = f.s.InstallConfigurationDestination(ctx, changed)
	code(t, err, "conflict")
	bad := p2
	bad.Version = 3
	bad.CredentialEnvironment = "DATABASE_URL"
	_, err = f.s.InstallCredentialPurposeProfile(ctx, bad)
	code(t, err, "invalid_request")
	bad = p2
	bad.Version = 4
	_, err = f.s.InstallCredentialPurposeProfile(ctx, bad)
	code(t, err, "conflict")
	var count int
	if err = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM configuration_audit WHERE organization_id=$1", f.p.OrganizationID).Scan(&count); err != nil || count != 4 {
		t.Fatal("replays or rejected installs wrote audit", count, err)
	}
}

func TestConfigurationAuthorDelegationSharedCapRaceAndViewerCacheDenial(t *testing.T) {
	f := configurationAuthorSetup(t)
	ctx := context.Background()
	a, _ := f.account(t, "parent")
	b, _ := f.account(t, "child")
	c, _ := f.account(t, "sibling")
	root := f.grant(t, f.p, a.ID, nil, 1)
	left := f.grant(t, a, b.ID, &root, 1)
	right := f.grant(t, a, c.ID, &root, 1)
	catalog, err := f.s.ConfigurationCatalog(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	active := true
	in := domain.CreateConfigurationPlan{BaseRevision: catalog.Revision, TTLSeconds: 100, Operations: []domain.ConfigurationOperationInput{{ID: "connect", DestinationID: f.Destination.ID, DestinationVersion: 1, Active: &active}}, Reason: "Concurrent descendants share root cap"}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, candidate := range []struct {
		p domain.Principal
		g string
	}{{b, left.ID}, {c, right.ID}} {
		wg.Add(1)
		go func(p domain.Principal, g string) {
			defer wg.Done()
			<-start
			input := in
			input.GrantID = g
			_, e := f.s.CreateConfigurationPlan(ctx, p, domain.NewID("key"), input)
			errs <- e
		}(candidate.p, candidate.g)
	}
	close(start)
	wg.Wait()
	close(errs)
	success, denied := 0, 0
	for e := range errs {
		if e == nil {
			success++
		} else if de, ok := e.(*domain.Error); ok && de.Code == "budget_exceeded" {
			denied++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || denied != 1 {
		t.Fatal("descendants bypassed shared cap", success, denied)
	}
	observed, err := f.s.ConfigurationGrant(ctx, f.p, root.ID)
	if err != nil || observed.UsedPlans != 1 {
		t.Fatal("root cumulative usage changed", observed.UsedPlans, err)
	}
	// A fresh viewer key must not replay an earlier member-key mutation.
	key := "viewer-cache-negative"
	leafInput := domain.CreateConfigurationGrant{SubjectID: a.ID, Actions: []string{"inspect", "plan", "validate", "apply", "verify"}, Destinations: []domain.ConfigurationDestinationRef{{ID: f.Destination.ID, Version: 1}}, TTLSeconds: 300, MaxPlans: 2, MaxVerifications: 2, MaxAllocatedChargeMicroUSD: 0, Reason: "Separate cache boundary grant"}
	raw, err := f.s.CreateConfigurationGrant(ctx, f.p, domain.NewID("key"), leafInput)
	g := configurationAuthorDecode[domain.CapabilityEnvelope](t, raw, err)
	input := in
	input.GrantID = g.ID
	raw, err = f.s.CreateConfigurationPlan(ctx, a, key, input)
	_ = configurationAuthorDecode[domain.ConfigurationPlan](t, raw, err)
	token, _, err := f.s.IssueServiceToken(ctx, f.p.OrganizationID, a.ID, "viewer", time.Minute, "Author viewer ceiling denial")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := f.s.Authenticate(ctx, token, f.p.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.CreateConfigurationPlan(ctx, viewer, key, input)
	code(t, err, "forbidden")
}

func TestConfigurationAuthorMCPLeaseFenceUnknownAndLateFactAfterRevoke(t *testing.T) {
	f := configurationAuthorSetup(t)
	ctx := context.Background()
	actor, _ := f.account(t, "MCP worker")
	g := f.grant(t, f.p, actor.ID, nil, 4)
	plan := f.plan(t, actor, g.ID)
	validation := f.validate(t, actor, plan)
	if !validation.Valid {
		t.Fatal("static plan invalid", validation.Decision)
	}
	plan = f.apply(t, actor, plan, validation)
	v := f.verify(t, actor, plan, "connect", 0)
	lease, _, err := f.s.ClaimConfigurationVerification(ctx, "author_mcp_first", time.Second)
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var expired bool
		if err = f.s.Pool.QueryRow(ctx, "SELECT lease_until<=clock_timestamp() FROM configuration_verifications WHERE organization_id=$1 AND id=$2", v.OrganizationID, v.ID).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("natural lease expiry not observed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	newLease, _, err := f.s.ClaimConfigurationVerification(ctx, "author_mcp_second", time.Minute)
	if err != nil || newLease == nil || newLease.Token <= lease.Token {
		t.Fatal("proven-unsent lease not reclaimed", err)
	}
	_, err = f.s.PrepareConfigurationVerificationDispatch(ctx, lease)
	code(t, err, "stale_lease")
	forged := *newLease
	forged.ProfileDigest = Hash([]byte("another allowed or tampered private profile"))
	_, err = f.s.PrepareConfigurationVerificationDispatch(ctx, &forged)
	code(t, err, "forbidden")
	var marked bool
	if err = f.s.Pool.QueryRow(ctx, "SELECT dispatch_started_at IS NOT NULL FROM configuration_verifications WHERE organization_id=$1 AND id=$2", v.OrganizationID, v.ID).Scan(&marked); err != nil || marked {
		t.Fatal("forged profile pin consumed original dispatch", err)
	}
	permit, err := f.s.PrepareConfigurationVerificationDispatch(ctx, newLease)
	if err != nil || permit.FencingToken != newLease.Token || permit.ValidFor <= 0 || permit.ValidFor > 2*time.Second || permit.Scope != newLease.Scope || permit.ResourceVersion != newLease.ResourceVersion || permit.ProfileDigest != newLease.ProfileDigest {
		t.Fatal("invalid durable admission", err)
	}
	if err = f.s.PauseConfigurationVerification(ctx, newLease, "acknowledgement_unavailable"); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.RevokeConfigurationGrant(ctx, f.p, g.ID, domain.NewID("key"), domain.RevokeConfigurationGrant{ExpectedVersion: g.Version, Reason: "Revoke after original dispatch"})
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := f.s.ClaimConfigurationVerification(ctx, "author_mcp_restart", time.Minute)
	if err != nil || again != nil {
		t.Fatal("unknown job was resent", err)
	}
	in := domain.ConfigurationProbeObservation{OrganizationID: v.OrganizationID, VerificationID: v.ID, WorkerID: newLease.WorkerID, FencingToken: newLease.Token, Outcome: "response_received", ProtocolVersion: mcpbridge.ProtocolVersion, ToolCount: 2, CatalogDigest: Hash([]byte("bounded safe catalog"))}
	late, err := f.s.RecordConfigurationVerificationObservation(ctx, in)
	if err != nil || late.Status != "response_received" || late.CatalogDigest != in.CatalogDigest {
		t.Fatal("late exact observation lost", err)
	}
	in.WorkerID = "forged_worker"
	_, err = f.s.RecordConfigurationVerificationObservation(ctx, in)
	code(t, err, "forbidden")
	audit, _, err := f.s.ConfigurationAudit(ctx, actor, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(audit)
	if strings.Contains(string(wire), f.MCP.Endpoint) || strings.Contains(string(wire), f.purpose.CredentialEnvironment) {
		t.Fatal("private host details entered public audit")
	}
	state, err := f.s.ConfigurationGrant(ctx, f.p, g.ID)
	if err != nil || state.UsedVerifications != 1 {
		t.Fatal("revocation reset verification usage", err)
	}
}

func TestConfigurationAuthorOwnerApprovalAndAtomicMultiResourceRollback(t *testing.T) {
	f := configurationAuthorSetup(t)
	ctx := context.Background()
	model := domain.ModelConnection{ID: "author_model", OrganizationID: f.p.OrganizationID, Version: 1, Model: "author-model", ProviderAccount: "author-provider", PriceVersion: "author-price", Endpoint: "http://127.0.0.1:9", MaxChargeMicroUSD: 300, MaxInputTokens: 100, MaxOutputTokens: 64, InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 2_000_000, FixedMicroUSD: 7, MaxInputBytes: 65536, MaxOutputBytes: 65536, TimeoutMilliseconds: 2000, BoundAssumptions: "Synthetic static configuration only; no inference", TokenLimitField: "max_tokens", AllowInsecureLoopback: true}
	purpose, err := f.s.InstallCredentialPurposeProfile(ctx, domain.CredentialPurposeProfile{ID: "author_model_purpose", OrganizationID: f.p.OrganizationID, Version: 1, Kind: "model", EndpointDigest: Hash([]byte(model.Endpoint)), CredentialEnvironment: "PLATFORM_MODEL_KEY_CONFIGURATION_AUTHOR", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.s.InstallConfigurationDestination(ctx, domain.ConfigurationDestinationProfile{Destination: domain.ConfigurationDestination{ID: "author_model_destination", OrganizationID: f.p.OrganizationID, Version: 1, Kind: "model", Name: "Approved model slot", TargetID: model.ID, CredentialPurposeID: purpose.ID, CredentialPurposeVersion: 1, RequiresApproval: true, MaxVerifyChargeMicroUSD: 500, Model: model.Model, Active: true}, Model: &model})
	if err != nil {
		t.Fatal(err)
	}
	actor, _ := f.account(t, "atomic apply")
	g := f.grant(t, f.p, actor.ID, nil, 4, domain.ConfigurationDestinationRef{ID: f.Destination.ID, Version: 1}, domain.ConfigurationDestinationRef{ID: d.Destination.ID, Version: 1})
	active := true
	plan := f.plan(t, actor, g.ID, domain.ConfigurationOperationInput{ID: "model", DestinationID: d.Destination.ID, DestinationVersion: 1, Active: &active}, domain.ConfigurationOperationInput{ID: "mcp", DestinationID: f.Destination.ID, DestinationVersion: 1, Active: &active})
	validation := f.validate(t, actor, plan)
	if !validation.Valid || validation.Decision.Outcome != "requires_approval" {
		t.Fatal("approval created circular validation", validation.Decision)
	}
	apply := domain.ApplyConfigurationPlan{ExpectedVersion: plan.Version, PlanDigest: plan.Digest, ValidationID: validation.ID, Reason: "Exact atomic approved change"}
	_, err = f.s.ApplyConfigurationPlan(ctx, actor, plan.ID, domain.NewID("key"), apply)
	code(t, err, "forbidden")
	_, err = f.s.ApproveConfigurationPlan(ctx, f.p, plan.ID, domain.NewID("key"), domain.ApproveConfigurationPlan{ExpectedVersion: plan.Version, PlanDigest: plan.Digest, ValidationID: validation.ID, Reason: "Owner reviewed exact multi-resource diff"})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := f.s.ConfigurationCatalog(ctx, f.p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Pool.Exec(ctx, `CREATE FUNCTION author_config_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'author atomic fault'; END $$; CREATE TRIGGER author_config_fail BEFORE INSERT ON mcp_connections FOR EACH ROW EXECUTE FUNCTION author_config_fail()`); err != nil {
		t.Fatal(err)
	}
	key := domain.NewID("key")
	_, err = f.s.ApplyConfigurationPlan(ctx, actor, plan.ID, key, apply)
	if err == nil {
		t.Fatal("injected second-resource failure did not abort")
	}
	var models, bindings int
	if err = f.s.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM model_connections WHERE organization_id=$1),(SELECT count(*) FROM configuration_resource_bindings WHERE organization_id=$1)", f.p.OrganizationID).Scan(&models, &bindings); err != nil || models != 0 || bindings != 0 {
		t.Fatal("partial apply survived", models, bindings, err)
	}
	after, err := f.s.ConfigurationCatalog(ctx, f.p)
	if err != nil || after.Revision != catalog.Revision {
		t.Fatal("failed apply advanced revision", err)
	}
	if _, err = f.s.Pool.Exec(ctx, "DROP TRIGGER author_config_fail ON mcp_connections; DROP FUNCTION author_config_fail()"); err != nil {
		t.Fatal(err)
	}
	raw, err := f.s.ApplyConfigurationPlan(ctx, actor, plan.ID, key, apply)
	applied := configurationAuthorDecode[domain.ConfigurationPlan](t, raw, err)
	if applied.Version != 2 || applied.Status != "applied" {
		t.Fatal("valid atomic apply did not complete")
	}
	raw, err = f.s.ApplyConfigurationPlan(ctx, actor, plan.ID, key, apply)
	again := configurationAuthorDecode[domain.ConfigurationPlan](t, raw, err)
	if again.ReceiptID != applied.ReceiptID {
		t.Fatal("apply replay created another receipt")
	}
	resources, _, err := f.s.ConfigurationResources(ctx, actor, "", 10)
	if err != nil || len(resources) != 2 {
		t.Fatal("delegated inspection lost exact resources", len(resources), err)
	}
}

func TestConfigurationAuthorModelProbeReservesBeforeHTTPAndScrubsCredentialEcho(t *testing.T) {
	f := configurationAuthorSetup(t)
	ctx := context.Background()
	secret := "configuration-author-private-model-key"
	var calls atomic.Int64
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var marked bool
		var reserved int64
		if err := f.s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reservations r JOIN actions a ON a.organization_id=r.organization_id AND a.id=r.action_id WHERE r.organization_id=$1 AND r.category='model' AND r.status='reserved' AND a.dispatch_issued_at IS NOT NULL), reserved_microusd FROM organizations WHERE id=$1`, f.p.OrganizationID).Scan(&marked, &reserved); err != nil || !marked || reserved != 300 {
			t.Error("HTTP preceded durable dispatch/reservation", marked, reserved, err)
		}
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("wrong approved credential")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": secret, "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": secret}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}})
	}))
	t.Cleanup(peer.Close)
	model := domain.ModelConnection{ID: "probe_model", OrganizationID: f.p.OrganizationID, Version: 1, Model: "author-probe-model", ProviderAccount: "author-probe-provider", PriceVersion: "author-probe-price", Endpoint: peer.URL, MaxChargeMicroUSD: 300, MaxInputTokens: 100, MaxOutputTokens: 64, InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 2_000_000, FixedMicroUSD: 7, MaxInputBytes: 65536, MaxOutputBytes: 65536, TimeoutMilliseconds: 2000, BoundAssumptions: "Synthetic live local probe, no provider billing claim", TokenLimitField: "max_tokens", AllowInsecureLoopback: true}
	purpose, err := f.s.InstallCredentialPurposeProfile(ctx, domain.CredentialPurposeProfile{ID: "probe_purpose", OrganizationID: f.p.OrganizationID, Version: 1, Kind: "model", EndpointDigest: Hash([]byte(peer.URL)), CredentialEnvironment: "PLATFORM_MODEL_KEY_AUTHOR_PROBE", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.s.InstallConfigurationDestination(ctx, domain.ConfigurationDestinationProfile{Destination: domain.ConfigurationDestination{ID: "probe_destination", OrganizationID: f.p.OrganizationID, Version: 1, Kind: "model", Name: "Approved exact model probe", TargetID: model.ID, CredentialPurposeID: purpose.ID, CredentialPurposeVersion: 1, MaxVerifyChargeMicroUSD: 300, Model: model.Model, Active: true}, Model: &model})
	if err != nil {
		t.Fatal(err)
	}
	active := true
	plan := f.plan(t, f.p, "", domain.ConfigurationOperationInput{ID: "model", DestinationID: d.Destination.ID, DestinationVersion: 1, Active: &active})
	validation := f.validate(t, f.p, plan)
	plan = f.apply(t, f.p, plan, validation)
	v := f.verify(t, f.p, plan, "model", 300)
	l, connection, err := f.s.ClaimModel(ctx, "author_model_probe_worker", time.Minute)
	if err != nil || l == nil || l.TaskID != v.TaskID || l.ConfigurationVerificationID != v.ID {
		t.Fatal("model probe did not use ordinary pinned task", err)
	}
	runtime, err := modelruntime.New(f.s, RuntimeModelConfig(connection, secret))
	if err != nil {
		t.Fatal(err)
	}
	out, err := runtime.Execute(ctx, l)
	if err != nil || out.Status != modelruntime.CandidateWaitingInput {
		t.Fatal("safe successful model probe publication failed", out.Status, err)
	}
	observed, err := f.s.ConfigurationVerification(ctx, f.p, v.ID)
	if err != nil || observed.Status != "response_received" || observed.CostStatus != "unknown" || !configurationDigest.MatchString(observed.ResponseDigest) {
		t.Fatal("probe outcome conflated receipt and accounting", observed, err)
	}
	artifacts, err := f.s.Artifacts(ctx, f.p, v.TaskID)
	if err != nil || len(artifacts) != 1 {
		t.Fatal("safe probe candidate missing", err)
	}
	wire, _ := json.Marshal(artifacts)
	if strings.Contains(string(wire), secret) || !strings.Contains(string(wire), "Response content was discarded") {
		t.Fatal("credential echo escaped into public artifact")
	}
	var leaked bool
	if err = f.s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE organization_id=$1 AND task_id=$2 AND data::text LIKE '%'||$3||'%') OR EXISTS(SELECT 1 FROM configuration_verifications WHERE organization_id=$1 AND id=$4 AND (body::text LIKE '%'||$3||'%' OR outcome::text LIKE '%'||$3||'%'))`, f.p.OrganizationID, v.TaskID, secret, v.ID).Scan(&leaked); err != nil || leaked {
		t.Fatal("credential echo entered durable event/verification", err)
	}
	again, _, err := f.s.ClaimModel(ctx, "author_probe_restart", time.Minute)
	if err != nil || again != nil || calls.Load() != 1 {
		t.Fatal("completed probe was sent again", err)
	}
	// Retirement must prevent new admission/discovery as well as late dispatch.
	purpose.Version = 2
	purpose.Active = true
	if _, err = f.s.InstallCredentialPurposeProfile(ctx, purpose); err != nil {
		t.Fatal(err)
	}
	available, _, err := f.s.Models(ctx, f.p, "", 100)
	if err != nil || len(available) != 0 {
		t.Fatal("retired purpose remained a usable discovered model", len(available), err)
	}
	budget := int64(300)
	_, err = f.s.CreateAgent(ctx, f.p, domain.NewID("key"), domain.CreateAgent{Name: "Rejected retired purpose", Instructions: "No inference", Model: model.Model, BudgetMicroUSD: &budget})
	code(t, err, "forbidden")
	_, err = f.s.CreateTask(ctx, f.p, domain.NewID("key"), domain.CreateTask{Goal: "Reject new admission using historical agent", AgentID: l.Agent.ID, BudgetMicroUSD: 300})
	code(t, err, "forbidden")
}

func TestConfigurationAuthorEqualTTLAttenuatesBeforeDigestAndDoesNotBecomeSQLFailure(t *testing.T) {
	f := configurationAuthorSetup(t)
	ctx := context.Background()
	parentActor, _ := f.account(t, "TTLparent")
	childActor, _ := f.account(t, "TTLchild")
	parent := f.grant(t, f.p, parentActor.ID, nil, 4)
	raw, err := f.s.CreateConfigurationGrant(ctx, parentActor, domain.NewID("key"), domain.CreateConfigurationGrant{SubjectID: childActor.ID, ParentGrantID: parent.ID, ExpectedParentVersion: parent.Version, Actions: append([]string{}, domain.ConfigurationActions...), Destinations: []domain.ConfigurationDestinationRef{{ID: f.Destination.ID, Version: 1}}, TTLSeconds: 600, MaxPlans: 4, MaxVerifications: 4, MaxAllocatedChargeMicroUSD: 1000, Reason: "Requested TTL is an upper bound, not a new parent deadline"})
	child := configurationAuthorDecode[domain.CapabilityEnvelope](t, raw, err)
	if !child.ExpiresAt.Equal(parent.ExpiresAt) {
		t.Fatal("equal duration renewed parent lifetime", child.ExpiresAt, parent.ExpiresAt)
	}
	catalog, err := f.s.ConfigurationCatalog(ctx, childActor)
	if err != nil {
		t.Fatal(err)
	}
	active := true
	raw, err = f.s.CreateConfigurationPlan(ctx, childActor, domain.NewID("key"), domain.CreateConfigurationPlan{GrantID: child.ID, BaseRevision: catalog.Revision, TTLSeconds: 600, Operations: []domain.ConfigurationOperationInput{{ID: "sameTTL", DestinationID: f.Destination.ID, DestinationVersion: 1, Active: &active}}, Reason: "Plan expiry clips before digest and DB insertion"})
	plan := configurationAuthorDecode[domain.ConfigurationPlan](t, raw, err)
	if !plan.ExpiresAt.Equal(parent.ExpiresAt) || !configurationDigest.MatchString(plan.Digest) {
		t.Fatal("plan deadline was not attenuated before sealing")
	}
	validation := f.validate(t, childActor, plan)
	if !validation.Valid {
		t.Fatal("clipped plan did not validate", validation.Decision)
	}
}

func TestConfigurationAuthorLogicalTargetInspectionAndExplicitFalseDisable(t *testing.T) {
	f := configurationAuthorSetup(t)
	ctx := context.Background()
	plan := f.plan(t, f.p, "")
	_ = f.apply(t, f.p, plan, f.validate(t, f.p, plan))
	purpose := f.purpose
	purpose.Version = 2
	purpose.Active = true
	if _, err := f.s.InstallCredentialPurposeProfile(ctx, purpose); err != nil {
		t.Fatal(err)
	}
	d2 := f.ConfigurationDestinationProfile
	d2.Destination.Version = 2
	d2.Destination.CredentialPurposeVersion = 2
	d2.Destination.Active = true
	if _, err := f.s.InstallConfigurationDestination(ctx, d2); err != nil {
		t.Fatal(err)
	}
	actor, _ := f.account(t, "rotation inspection")
	g := f.grant(t, f.p, actor.ID, nil, 4, domain.ConfigurationDestinationRef{ID: f.Destination.ID, Version: 2})
	resources, _, err := f.s.ConfigurationResources(ctx, actor, "", 10)
	if err != nil || len(resources) != 1 || resources[0].Version != 1 || resources[0].Scope.DestinationVersion != 1 {
		t.Fatal("new approved scope cannot inspect old logical target CAS", len(resources), err)
	}
	destinations, _, err := f.s.ConfigurationDestinations(ctx, actor, "", 10)
	if err != nil || len(destinations) != 1 || destinations[0].Version != 2 {
		t.Fatal("destination listing widened beyond exact grant", len(destinations), err)
	}
	disabled := false
	plan = f.plan(t, actor, g.ID, domain.ConfigurationOperationInput{ID: "disable", DestinationID: f.Destination.ID, DestinationVersion: 2, ExpectedResourceVersion: resources[0].Version, Active: &disabled})
	if len(plan.Diff) != 1 || len(plan.Diff[0].RetiredVersions) != 1 || plan.Diff[0].RetiredVersions[0] != 1 || plan.Diff[0].Active {
		t.Fatal("disable diff omitted live old version", plan.Diff)
	}
	plan = f.apply(t, actor, plan, f.validate(t, actor, plan))
	var activeCount int
	if err = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM mcp_connections WHERE organization_id=$1 AND id=$2 AND active", f.p.OrganizationID, f.Destination.TargetID).Scan(&activeCount); err != nil || activeCount != 0 {
		t.Fatal("active=false left previous target live", activeCount, err)
	}
	ack := true
	_, err = f.s.StartConfigurationVerification(ctx, actor, plan.ID, domain.NewID("key"), domain.StartConfigurationVerification{ExpectedPlanVersion: plan.Version, PlanDigest: plan.Digest, OperationID: "disable", MaxChargeMicroUSD: 0, AcknowledgeCost: &ack, Reason: "Disabled targets cannot be checked"})
	code(t, err, "conflict")
	// Composite immutable profile versions must have round-trippable cursors,
	// even when the trusted host uses a maximum-length identifier.
	long := d2
	long.Destination.ID = strings.Repeat("d", 128)
	long.Destination.Version = 1
	long.Destination.TargetID = strings.Repeat("t", 128)
	if _, err = f.s.InstallConfigurationDestination(ctx, long); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 5; page++ {
		values, next, e := f.s.ConfigurationDestinations(ctx, f.p, cursor, 1)
		if e != nil {
			t.Fatal(e)
		}
		for _, v := range values {
			key := v.ID + ":" + configurationVersion(v.Version)
			if seen[key] {
				t.Fatal("profile pagination repeated a row")
			}
			seen[key] = true
		}
		cursor = next
		if cursor == "" {
			break
		}
	}
	if len(seen) != 3 || cursor != "" {
		t.Fatal("maximum identifier broke scoped pagination", len(seen))
	}
}

func TestConfigurationAuthorRetiredPurposeStopsBoundResourceAndRevokedUnsentCleanup(t *testing.T) {
	f := configurationAuthorSetup(t)
	ctx := context.Background()
	actor, _ := f.account(t, "unsent revoke")
	g := f.grant(t, f.p, actor.ID, nil, 4)
	plan := f.plan(t, actor, g.ID)
	plan = f.apply(t, actor, plan, f.validate(t, actor, plan))
	v := f.verify(t, actor, plan, "connect", 0)
	lease, _, err := f.s.ClaimConfigurationVerification(ctx, "author_unsent_worker", time.Minute)
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	_, err = f.s.RevokeConfigurationGrant(ctx, f.p, g.ID, domain.NewID("key"), domain.RevokeConfigurationGrant{ExpectedVersion: 1, Reason: "Stop leased but proven-unsent verification"})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := f.s.ReconcileConfigurationVerifications(ctx)
	if err != nil || !changed {
		t.Fatal("unsent grant revoke was not reconciled", err)
	}
	observed, err := f.s.ConfigurationVerification(ctx, actor, v.ID)
	if err != nil || observed.Status != "cancelled" || observed.CostStatus != "unmetered" {
		t.Fatal("revoked unsent job kept running", err)
	}
	_, err = f.s.PrepareConfigurationVerificationDispatch(ctx, lease)
	if err == nil {
		t.Fatal("revoked original lease admitted dispatch")
	}
	tx, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = checkConfigurationResourceAdmissionTx(ctx, tx, f.p.OrganizationID, "mcp", f.Destination.TargetID, 1); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	purpose := f.purpose
	purpose.Version = 2
	purpose.Active = true
	if _, err = f.s.InstallCredentialPurposeProfile(ctx, purpose); err != nil {
		t.Fatal(err)
	}
	tx, err = f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	err = checkConfigurationResourceAdmissionTx(ctx, tx, f.p.OrganizationID, "mcp", f.Destination.TargetID, 1)
	code(t, err, "forbidden")
}
