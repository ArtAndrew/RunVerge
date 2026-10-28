// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/configurationruntime"
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
	"agentplatform.local/platform/internal/modelruntime"
	"github.com/jackc/pgx/v5"
)

func independentConfigurationDecode[T any](t *testing.T, raw json.RawMessage, err error) T {
	t.Helper()
	var value T
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func independentConfigurationGrant(t *testing.T, f independentConfigurationSchema, issuer domain.Principal, subject string, parent *domain.CapabilityEnvelope, plans, checks int, charge int64, refs ...domain.ConfigurationDestinationRef) domain.CapabilityEnvelope {
	t.Helper()
	if len(refs) == 0 {
		refs = []domain.ConfigurationDestinationRef{{ID: f.dest.Destination.ID, Version: f.dest.Destination.Version}}
	}
	in := domain.CreateConfigurationGrant{SubjectID: subject, Actions: append([]string{}, domain.ConfigurationActions...), Destinations: refs,
		TTLSeconds: 1800, MaxPlans: plans, MaxVerifications: checks, MaxAllocatedChargeMicroUSD: charge, Reason: "Independent bounded configuration authority"}
	if parent != nil {
		in.ParentGrantID, in.ExpectedParentVersion, in.TTLSeconds = parent.ID, parent.Version, 900
	}
	raw, err := f.s.CreateConfigurationGrant(context.Background(), issuer, domain.NewID("key"), in)
	return independentConfigurationDecode[domain.CapabilityEnvelope](t, raw, err)
}

func independentConfigurationPlan(t *testing.T, f independentConfigurationSchema, actor domain.Principal, grant string, operations ...domain.ConfigurationOperationInput) (domain.ConfigurationPlan, domain.CreateConfigurationPlan) {
	t.Helper()
	catalog, err := f.s.ConfigurationCatalog(context.Background(), actor)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) == 0 {
		active := true
		operations = []domain.ConfigurationOperationInput{{ID: "connect", DestinationID: f.dest.Destination.ID, DestinationVersion: f.dest.Destination.Version, Active: &active}}
	}
	in := domain.CreateConfigurationPlan{GrantID: grant, BaseRevision: catalog.Revision, TTLSeconds: 300,
		Operations: operations, Reason: "Independent exact configuration plan"}
	raw, err := f.s.CreateConfigurationPlan(context.Background(), actor, domain.NewID("key"), in)
	return independentConfigurationDecode[domain.ConfigurationPlan](t, raw, err), in
}

func independentConfigurationValidate(t *testing.T, f independentConfigurationSchema, actor domain.Principal, plan domain.ConfigurationPlan) domain.ConfigurationValidation {
	t.Helper()
	raw, err := f.s.ValidateConfigurationPlan(context.Background(), actor, plan.ID, domain.NewID("key"), domain.ValidateConfigurationPlan{ExpectedVersion: plan.Version, PlanDigest: plan.Digest})
	v := independentConfigurationDecode[domain.ConfigurationValidation](t, raw, err)
	if !v.Valid {
		t.Fatal("fresh independently prepared plan was invalid", v.Decision.ReasonCodes)
	}
	return v
}

func independentConfigurationApprove(t *testing.T, f independentConfigurationSchema, owner domain.Principal, plan domain.ConfigurationPlan, validation domain.ConfigurationValidation) {
	t.Helper()
	_, err := f.s.ApproveConfigurationPlan(context.Background(), owner, plan.ID, domain.NewID("key"), domain.ApproveConfigurationPlan{
		ExpectedVersion: plan.Version, PlanDigest: plan.Digest, ValidationID: validation.ID, Reason: "Independent owner exact-diff approval"})
	if err != nil {
		t.Fatal(err)
	}
}

func independentConfigurationApply(t *testing.T, f independentConfigurationSchema, actor domain.Principal, plan domain.ConfigurationPlan, validation domain.ConfigurationValidation) domain.ConfigurationPlan {
	t.Helper()
	if validation.Decision.Outcome == "requires_approval" {
		independentConfigurationApprove(t, f, f.owner, plan, validation)
	}
	raw, err := f.s.ApplyConfigurationPlan(context.Background(), actor, plan.ID, domain.NewID("key"), domain.ApplyConfigurationPlan{
		ExpectedVersion: plan.Version, PlanDigest: plan.Digest, ValidationID: validation.ID, Reason: "Independent apply exact validated diff"})
	return independentConfigurationDecode[domain.ConfigurationPlan](t, raw, err)
}

func independentConfigurationStart(t *testing.T, f independentConfigurationSchema, actor domain.Principal, plan domain.ConfigurationPlan, operation, key string, charge int64) (domain.ConfigurationVerification, domain.StartConfigurationVerification) {
	t.Helper()
	ack := true
	in := domain.StartConfigurationVerification{ExpectedPlanVersion: plan.Version, PlanDigest: plan.Digest, OperationID: operation,
		MaxChargeMicroUSD: charge, AcknowledgeCost: &ack, Reason: "One independent explicitly authorized verification"}
	raw, err := f.s.StartConfigurationVerification(context.Background(), actor, plan.ID, key, in)
	return independentConfigurationDecode[domain.ConfigurationVerification](t, raw, err), in
}

func independentConfigurationMCPPeer(t *testing.T, secret string, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("fixture received a credential other than its exact approved key")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var in struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			t.Error("bad bounded discovery request")
			return
		}
		result := map[string]any{"resultType": "complete", "_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "independent-fixture", "version": "1"}}}
		switch in.Method {
		case "server/discover":
			result["supportedVersions"] = []string{mcpbridge.ProtocolVersion}
			result["capabilities"] = map[string]any{"tools": map[string]any{}}
		case "tools/list":
			result["tools"] = []any{map[string]any{"name": "untrusted_fixture_name", "description": "not authority", "inputSchema": map[string]any{"type": "object"}}}
		default:
			t.Error("configuration discovery attempted a non-discovery method", in.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": in.ID, "result": result})
	}))
	t.Cleanup(peer.Close)
	return peer
}

func independentConfigurationInstallMCP(t *testing.T, f independentConfigurationSchema, peer *httptest.Server) domain.ConfigurationProbeConfig {
	t.Helper()
	purpose, err := f.s.InstallCredentialPurposeProfile(context.Background(), domain.CredentialPurposeProfile{ID: domain.NewID("purpose"),
		OrganizationID: f.owner.OrganizationID, Version: 1, Kind: "mcp", EndpointDigest: Hash([]byte(peer.URL)),
		CredentialEnvironment: "PLATFORM_MCP_KEY_INDEPENDENT_AUTHORITY", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.s.InstallConfigurationDestination(context.Background(), domain.ConfigurationDestinationProfile{Destination: domain.ConfigurationDestination{
		ID: domain.NewID("destination"), OrganizationID: f.owner.OrganizationID, Version: 1, Kind: "mcp", Name: "Independent exact TLS discovery",
		TargetID: domain.NewID("mcp"), CredentialPurposeID: purpose.ID, CredentialPurposeVersion: 1, Active: true, DiscoveryUnmeteredApproved: true},
		MCP: &domain.MCPConnectionProfile{Endpoint: peer.URL, TrustedCAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: peer.Certificate().Raw})), AllowPrivateNetwork: true, TimeoutSeconds: 3}})
	if err != nil {
		t.Fatal(err)
	}
	return domain.ConfigurationProbeConfig{Destination: d, Purpose: purpose}
}

func TestIndependentConfigurationRuntimeCannotSwapAnotherApprovedDestinationIntoLease(t *testing.T) {
	f := independentConfigurationSchemaFixture(t)
	ctx := context.Background()
	var callsA, callsB atomic.Int64
	peerA := independentConfigurationMCPPeer(t, "independent-synthetic-key-A", &callsA)
	peerB := independentConfigurationMCPPeer(t, "independent-synthetic-key-B", &callsB)
	a := independentConfigurationInstallMCP(t, f, peerA)
	b := independentConfigurationInstallMCP(t, f, peerB)
	actor, _, _ := independentSecurityKey(t, f.s, f.service, "member", time.Hour)
	g := independentConfigurationGrant(t, f, f.owner, actor.ID, nil, 2, 2, 0, domain.ConfigurationDestinationRef{ID: a.Destination.Destination.ID, Version: 1})
	active := true
	plan, _ := independentConfigurationPlan(t, f, actor, g.ID, domain.ConfigurationOperationInput{ID: "connectA", DestinationID: a.Destination.Destination.ID, DestinationVersion: 1, Active: &active})
	validation := independentConfigurationValidate(t, f, actor, plan)
	plan = independentConfigurationApply(t, f, actor, plan, validation)
	v, _ := independentConfigurationStart(t, f, actor, plan, "connectA", domain.NewID("key"), 0)
	lease, actual, err := f.s.ClaimConfigurationVerification(ctx, "independent_swap_worker", time.Minute)
	if err != nil || lease == nil || lease.ID != v.ID || actual.Destination.Destination.ID != a.Destination.Destination.ID {
		t.Fatal("did not claim exact independently granted destinationA", err)
	}
	// B is a genuine different owner-approved profile, but the caller has no B
	// grant. Mixing worker inputs must not send B's credential or record it as A.
	out, runErr := configurationruntime.Execute(ctx, f.s, lease, configurationruntime.Config{Probe: b, Credential: "independent-synthetic-key-B"})
	var marked bool
	if err = f.s.Pool.QueryRow(ctx, "SELECT dispatch_started_at IS NOT NULL FROM configuration_verifications WHERE organization_id=$1 AND id=$2", v.OrganizationID, v.ID).Scan(&marked); err != nil {
		t.Fatal(err)
	}
	if runErr == nil || out.Status != "not_dispatched" || marked || callsA.Load() != 0 || callsB.Load() != 0 {
		t.Fatalf("wrong profile was admitted: status=%s marked=%v callsA=%d callsB=%d error=%v", out.Status, marked, callsA.Load(), callsB.Load(), runErr)
	}
}

func TestIndependentConfigurationViewerCeilingDeniesEveryCachedWrite(t *testing.T) {
	f := independentConfigurationSchemaFixture(t)
	ctx := context.Background()
	actor, _, _ := independentSecurityKey(t, f.s, f.service, "member", time.Hour)
	viewer, _, _ := independentSecurityKey(t, f.s, f.service, "viewer", time.Hour)
	g := independentConfigurationGrant(t, f, f.owner, actor.ID, nil, 3, 3, 0)
	catalog, err := f.s.ConfigurationCatalog(ctx, actor)
	if err != nil {
		t.Fatal(err)
	}
	active := true
	in := domain.CreateConfigurationPlan{GrantID: g.ID, BaseRevision: catalog.Revision, TTLSeconds: 300,
		Operations: []domain.ConfigurationOperationInput{{ID: "connect", DestinationID: f.dest.Destination.ID, DestinationVersion: 1, Active: &active}}, Reason: "Exact cached member-key plan"}
	planKey := domain.NewID("key")
	raw, err := f.s.CreateConfigurationPlan(ctx, actor, planKey, in)
	plan := independentConfigurationDecode[domain.ConfigurationPlan](t, raw, err)
	_, err = f.s.CreateConfigurationPlan(ctx, viewer, planKey, in)
	code(t, err, "forbidden")
	validationKey := domain.NewID("key")
	validationIn := domain.ValidateConfigurationPlan{ExpectedVersion: plan.Version, PlanDigest: plan.Digest}
	raw, err = f.s.ValidateConfigurationPlan(ctx, actor, plan.ID, validationKey, validationIn)
	validation := independentConfigurationDecode[domain.ConfigurationValidation](t, raw, err)
	_, err = f.s.ValidateConfigurationPlan(ctx, viewer, plan.ID, validationKey, validationIn)
	code(t, err, "forbidden")
	independentConfigurationApprove(t, f, f.owner, plan, validation)
	_, err = f.s.ApproveConfigurationPlan(ctx, actor, plan.ID, domain.NewID("key"), domain.ApproveConfigurationPlan{ExpectedVersion: plan.Version, PlanDigest: plan.Digest, ValidationID: validation.ID, Reason: "Service must never approve"})
	code(t, err, "forbidden")
	applyKey := domain.NewID("key")
	applyIn := domain.ApplyConfigurationPlan{ExpectedVersion: plan.Version, PlanDigest: plan.Digest, ValidationID: validation.ID, Reason: "One exact application"}
	raw, err = f.s.ApplyConfigurationPlan(ctx, actor, plan.ID, applyKey, applyIn)
	plan = independentConfigurationDecode[domain.ConfigurationPlan](t, raw, err)
	_, err = f.s.ApplyConfigurationPlan(ctx, viewer, plan.ID, applyKey, applyIn)
	code(t, err, "forbidden")
	verifyKey := domain.NewID("key")
	v, verifyIn := independentConfigurationStart(t, f, actor, plan, "connect", verifyKey, 0)
	_, err = f.s.StartConfigurationVerification(ctx, viewer, plan.ID, verifyKey, verifyIn)
	code(t, err, "forbidden")
	_, err = f.s.StartConfigurationVerification(ctx, viewer, plan.ID, domain.NewID("key"), verifyIn)
	code(t, err, "forbidden")
	raw, err = f.s.StartConfigurationVerification(ctx, actor, plan.ID, verifyKey, verifyIn)
	replayed := independentConfigurationDecode[domain.ConfigurationVerification](t, raw, err)
	if replayed.ID != v.ID {
		t.Fatal("exact member replay created a new job")
	}
	_, err = f.s.StartConfigurationVerification(ctx, actor, plan.ID, domain.NewID("key"), verifyIn)
	code(t, err, "conflict")
	state, err := f.s.ConfigurationGrant(ctx, f.owner, g.ID)
	if err != nil || state.UsedPlans != 1 || state.UsedVerifications != 1 {
		t.Fatal("denied/replayed commands consumed or reset grant usage", state.UsedPlans, state.UsedVerifications, err)
	}
	var count int
	if err = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM configuration_verifications WHERE organization_id=$1", f.owner.OrganizationID).Scan(&count); err != nil || count != 1 {
		t.Fatal("new key or weaker key created duplicate verification", count, err)
	}
}

func independentConfigurationInstallModel(t *testing.T, f independentConfigurationSchema, endpoint string) (domain.ConfigurationProbeConfig, domain.ModelConnection) {
	t.Helper()
	model := domain.ModelConnection{ID: domain.NewID("model"), OrganizationID: f.owner.OrganizationID, Version: 1,
		Model: domain.NewID("syntheticmodel"), ProviderAccount: "independent-test-provider", PriceVersion: "synthetic-fixed17",
		Endpoint: endpoint, MaxChargeMicroUSD: 17, MaxInputTokens: 1024, FixedMicroUSD: 17, MaxOutputTokens: 16,
		MaxInputBytes: 4096, MaxOutputBytes: 4096, TimeoutMilliseconds: 3000,
		BoundAssumptions: "Local synthetic fixed17 profile; usage is not a provider bill", TokenLimitField: "max_tokens", AllowInsecureLoopback: strings.HasPrefix(endpoint, "http://")}
	purpose, err := f.s.InstallCredentialPurposeProfile(context.Background(), domain.CredentialPurposeProfile{ID: domain.NewID("purpose"), OrganizationID: f.owner.OrganizationID,
		Version: 1, Kind: "model", EndpointDigest: Hash([]byte(endpoint)), CredentialEnvironment: "PLATFORM_MODEL_KEY_INDEPENDENT_CONFIG", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	dest, err := f.s.InstallConfigurationDestination(context.Background(), domain.ConfigurationDestinationProfile{Destination: domain.ConfigurationDestination{
		ID: domain.NewID("destination"), OrganizationID: f.owner.OrganizationID, Version: 1, Kind: "model", TargetID: model.ID,
		Name: "Independent bounded model connection", Model: model.Model, CredentialPurposeID: purpose.ID, CredentialPurposeVersion: 1,
		RequiresApproval: true, MaxVerifyChargeMicroUSD: 17, Active: true}, Model: &model})
	if err != nil {
		t.Fatal(err)
	}
	return domain.ConfigurationProbeConfig{Destination: dest, Purpose: purpose}, model
}

func TestIndependentConfigurationDescendantVerificationRacePreservesRootExposure(t *testing.T) {
	f := independentConfigurationSchemaFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, _, _ := independentSecurityKey(t, f.s, f.service, "member", time.Hour)
	bAccount := independentSecurityService(t, f.s, f.owner, "member")
	cAccount := independentSecurityService(t, f.s, f.owner, "member")
	b, _, _ := independentSecurityKey(t, f.s, bAccount, "member", time.Hour)
	c, _, _ := independentSecurityKey(t, f.s, cAccount, "member", time.Hour)
	x, _ := independentConfigurationInstallModel(t, f, "http://127.0.0.1:9/never-called")
	y, _ := independentConfigurationInstallModel(t, f, "http://127.0.0.1:9/never-called")
	refs := []domain.ConfigurationDestinationRef{{ID: x.Destination.Destination.ID, Version: 1}, {ID: y.Destination.Destination.ID, Version: 1}}
	root := independentConfigurationGrant(t, f, f.owner, a.ID, nil, 2, 2, 17, refs...)
	left := independentConfigurationGrant(t, f, a, b.ID, &root, 2, 2, 17, refs[0])
	right := independentConfigurationGrant(t, f, a, c.ID, &root, 2, 2, 17, refs[1])
	active := true
	plans := make([]domain.ConfigurationPlan, 2)
	for i, candidate := range []struct {
		actor domain.Principal
		grant string
		ref   domain.ConfigurationDestinationRef
	}{{b, left.ID, refs[0]}, {c, right.ID, refs[1]}} {
		plan, _ := independentConfigurationPlan(t, f, candidate.actor, candidate.grant,
			domain.ConfigurationOperationInput{ID: "model", DestinationID: candidate.ref.ID, DestinationVersion: 1, Active: &active})
		plans[i] = independentConfigurationApply(t, f, candidate.actor, plan, independentConfigurationValidate(t, f, candidate.actor, plan))
	}
	ack := true
	type answer struct {
		index int
		v     domain.ConfigurationVerification
		err   error
	}
	start := make(chan struct{})
	answers := make(chan answer, 2)
	keys := []string{domain.NewID("key"), domain.NewID("key")}
	actors := []domain.Principal{b, c}
	for i := range plans {
		go func(i int) {
			<-start
			raw, err := f.s.StartConfigurationVerification(ctx, actors[i], plans[i].ID, keys[i], domain.StartConfigurationVerification{ExpectedPlanVersion: 2, PlanDigest: plans[i].Digest,
				OperationID: "model", MaxChargeMicroUSD: 17, AcknowledgeCost: &ack, Reason: "Concurrent descendants share cumulative exposure"})
			var v domain.ConfigurationVerification
			if err == nil {
				err = json.Unmarshal(raw, &v)
			}
			answers <- answer{i, v, err}
		}(i)
	}
	close(start)
	winner, denied := -1, 0
	for range plans {
		select {
		case answer := <-answers:
			if answer.err == nil {
				if winner >= 0 {
					t.Fatal("two descendants exceeded the same root exposure cap")
				}
				winner = answer.index
			} else {
				code(t, answer.err, "budget_exceeded")
				denied++
			}
		case <-ctx.Done():
			t.Fatal("shared exposure race did not finish", ctx.Err())
		}
	}
	if winner < 0 || denied != 1 {
		t.Fatal("shared exposure race lacked one exact winner", winner, denied)
	}
	state, err := f.s.ConfigurationGrant(ctx, f.owner, root.ID)
	if err != nil || state.UsedPlans != 2 || state.UsedVerifications != 1 || state.AllocatedChargeMicroUSD != 17 {
		t.Fatal("root cumulative allocation mismatch", state.UsedPlans, state.UsedVerifications, state.AllocatedChargeMicroUSD, err)
	}
	var tasks, runs, operations, reservations int
	if err = f.s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tasks WHERE organization_id=$1),(SELECT count(*) FROM runs WHERE organization_id=$1),
 (SELECT count(*) FROM operations WHERE organization_id=$1),(SELECT count(*) FROM reservations WHERE organization_id=$1)`, f.owner.OrganizationID).Scan(&tasks, &runs, &operations, &reservations); err != nil || tasks != 1 || runs != 1 || operations != 1 || reservations != 0 {
		t.Fatal("loser leaked a task/operation/reserve", tasks, runs, operations, reservations, err)
	}
}

func TestIndependentConfigurationGrantRevocationAfterObservedLockWaitRejectsAdmission(t *testing.T) {
	f := independentConfigurationSchemaFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	actor, _, _ := independentSecurityKey(t, f.s, f.service, "member", time.Hour)
	g := independentConfigurationGrant(t, f, f.owner, actor.ID, nil, 2, 2, 0)
	catalog, err := f.s.ConfigurationCatalog(ctx, actor)
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	var pid int32
	if err = blocker.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	// A real authoritative revocation remains uncommitted while the public
	// command initially reads the prior grant and later waits for its row lock.
	if _, err = blocker.Exec(ctx, "UPDATE configuration_grants SET revoked=true WHERE organization_id=$1 AND id=$2", g.OrganizationID, g.ID); err != nil {
		t.Fatal(err)
	}
	active := true
	result := make(chan error, 1)
	go func() {
		_, err := f.s.CreateConfigurationPlan(ctx, actor, domain.NewID("key"), domain.CreateConfigurationPlan{GrantID: g.ID, BaseRevision: catalog.Revision, TTLSeconds: 300,
			Operations: []domain.ConfigurationOperationInput{{ID: "connect", DestinationID: f.dest.Destination.ID, DestinationVersion: 1, Active: &active}}, Reason: "Observe current revocation after grant-row lock wait"})
		result <- err
	}()
	independentMembershipWaitBlocked(t, f.s, pid, "SELECT body,version,revoked,used_plans", 1)
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-result:
		code(t, err, "forbidden")
	case <-ctx.Done():
		t.Fatal("command remained blocked after authoritative revocation")
	}
	var plans, usage int
	if err = f.s.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM configuration_plans WHERE organization_id=$1),used_plans FROM configuration_grants WHERE organization_id=$1 AND id=$2", g.OrganizationID, g.ID).Scan(&plans, &usage); err != nil || plans != 0 || usage != 0 {
		t.Fatal("stale pre-wait grant admitted work", plans, usage, err)
	}
}

func TestIndependentConfigurationRuntimeRejectsForgedLeaseAndPrivateProfilePins(t *testing.T) {
	f := independentConfigurationSchemaFixture(t)
	ctx := context.Background()
	var callsA, callsB atomic.Int64
	a := independentConfigurationInstallMCP(t, f, independentConfigurationMCPPeer(t, "independent-synthetic-key-A", &callsA))
	b := independentConfigurationInstallMCP(t, f, independentConfigurationMCPPeer(t, "independent-synthetic-key-B", &callsB))
	actor, _, _ := independentSecurityKey(t, f.s, f.service, "member", time.Hour)
	g := independentConfigurationGrant(t, f, f.owner, actor.ID, nil, 2, 2, 0, domain.ConfigurationDestinationRef{ID: a.Destination.Destination.ID, Version: 1})
	active := true
	plan, _ := independentConfigurationPlan(t, f, actor, g.ID, domain.ConfigurationOperationInput{ID: "connectA", DestinationID: a.Destination.Destination.ID, DestinationVersion: 1, Active: &active})
	plan = independentConfigurationApply(t, f, actor, plan, independentConfigurationValidate(t, f, actor, plan))
	v, _ := independentConfigurationStart(t, f, actor, plan, "connectA", domain.NewID("key"), 0)
	lease, actual, err := f.s.ClaimConfigurationVerification(ctx, "independent_private_pin_worker", time.Minute)
	if err != nil || lease == nil || lease.ID != v.ID {
		t.Fatal(err)
	}
	for _, name := range []string{"forged_lease_and_real_profileB", "changed_endpoint_and_matching_digest", "changed_CA", "changed_network_policy", "changed_credential_reference", "forged_resource_version"} {
		t.Run(name, func(t *testing.T) {
			l := *lease
			probe := actual
			mcp := *probe.Destination.MCP
			probe.Destination.MCP = &mcp
			credential := "independent-synthetic-key-A"
			switch name {
			case "forged_lease_and_real_profileB":
				probe, credential = b, "independent-synthetic-key-B"
				l.Scope = configurationScope(b.Destination.Destination)
				l.ProfileDigest = domain.ConfigurationProbeDigest(probe, l.ResourceVersion)
			case "changed_endpoint_and_matching_digest":
				probe.Destination.MCP.Endpoint = b.Destination.MCP.Endpoint
				probe.Purpose.EndpointDigest = Hash([]byte(probe.Destination.MCP.Endpoint))
			case "changed_CA":
				probe.Destination.MCP.TrustedCAPEM += "\n"
			case "changed_network_policy":
				probe.Destination.MCP.AllowPrivateNetwork = false
			case "changed_credential_reference":
				probe.Purpose.CredentialEnvironment = "PLATFORM_MCP_KEY_OTHER_PRIVATE_REFERENCE"
			case "forged_resource_version":
				l.ResourceVersion++
				l.ProfileDigest = domain.ConfigurationProbeDigest(probe, l.ResourceVersion)
			}
			_, err := configurationruntime.Execute(ctx, f.s, &l, configurationruntime.Config{Probe: probe, Credential: credential})
			var marked bool
			if queryErr := f.s.Pool.QueryRow(ctx, "SELECT dispatch_started_at IS NOT NULL FROM configuration_verifications WHERE organization_id=$1 AND id=$2", v.OrganizationID, v.ID).Scan(&marked); queryErr != nil {
				t.Fatal(queryErr)
			}
			if err == nil || marked || callsA.Load() != 0 || callsB.Load() != 0 {
				t.Fatal("forged private pins caused marker or external request", marked, callsA.Load(), callsB.Load(), err)
			}
		})
	}
	out, err := configurationruntime.Execute(ctx, f.s, lease, configurationruntime.Config{Probe: actual, Credential: "independent-synthetic-key-A"})
	if err != nil || out.Status != "response_received" || callsA.Load() != 2 || callsB.Load() != 0 {
		t.Fatal("genuine exact profile lost usability", out.Status, callsA.Load(), callsB.Load(), err)
	}
}

func TestIndependentConfigurationOwnerApprovalCannotReviveAfterRoleCycle(t *testing.T) {
	f := independentConfigurationSchemaFixture(t)
	ctx := context.Background()
	actor, _, _ := independentSecurityKey(t, f.s, f.service, "member", time.Hour)
	g := independentConfigurationGrant(t, f, f.owner, actor.ID, nil, 2, 2, 0)
	raw, err := f.s.CreateMember(ctx, f.owner, domain.NewID("key"), domain.CreateOrganizationMember{DisplayName: "Independent second approver", Role: "owner", Reason: "Independent owner approval lifecycle"})
	member := independentConfigurationDecode[domain.OrganizationMember](t, raw, err)
	token, _, err := f.s.IssueMemberToken(ctx, f.owner.OrganizationID, member.PrincipalID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	approver, err := f.s.Authenticate(ctx, token, f.owner.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := independentConfigurationPlan(t, f, actor, g.ID)
	validation := independentConfigurationValidate(t, f, actor, plan)
	independentConfigurationApprove(t, f, approver, plan, validation)
	active := true
	for _, role := range []string{"member", "owner"} {
		raw, err = f.s.UpdateMember(ctx, f.owner, member.PrincipalID, domain.NewID("key"), domain.UpdateOrganizationMember{
			ExpectedVersion: member.Version, DisplayName: member.DisplayName, Role: role, Active: &active, Reason: "Independent irreversible approval epoch cycle"})
		member = independentConfigurationDecode[domain.OrganizationMember](t, raw, err)
	}
	apply := domain.ApplyConfigurationPlan{ExpectedVersion: plan.Version, PlanDigest: plan.Digest, ValidationID: validation.ID, Reason: "Old approval must stay invalid"}
	key := domain.NewID("key")
	_, err = f.s.ApplyConfigurationPlan(ctx, actor, plan.ID, key, apply)
	code(t, err, "forbidden")
	var resources int
	if err = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM mcp_connections WHERE organization_id=$1", f.owner.OrganizationID).Scan(&resources); err != nil || resources != 0 {
		t.Fatal("stale owner approval installed resource", resources, err)
	}
	independentConfigurationApprove(t, f, approver, plan, validation)
	raw, err = f.s.ApplyConfigurationPlan(ctx, actor, plan.ID, key, apply)
	applied := independentConfigurationDecode[domain.ConfigurationPlan](t, raw, err)
	if applied.Status != "applied" || applied.Version != 2 {
		t.Fatal("fresh explicit owner approval did not restore a valid path")
	}
}

const independentConfigurationProbeSecret = "independent-synthetic-configuration-model-key"

type independentConfigurationModel struct {
	f            independentConfigurationSchema
	actor        domain.Principal
	profile      domain.ConfigurationProbeConfig
	model        domain.ModelConnection
	verification domain.ConfigurationVerification
	lease        *domain.Lease
	calls        atomic.Int64
}

func independentConfigurationModelSetup(t *testing.T, reply func(http.ResponseWriter, *http.Request)) *independentConfigurationModel {
	t.Helper()
	f := &independentConfigurationModel{f: independentConfigurationSchemaFixture(t)}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+independentConfigurationProbeSecret {
			t.Error("model fixture did not receive exact approved credential")
		}
		var modelMarker, verificationMarker bool
		var held int64
		if err := f.f.s.Pool.QueryRow(context.Background(), `SELECT reserved_microusd,
 EXISTS(SELECT 1 FROM actions WHERE organization_id=$1 AND dispatch_issued_at IS NOT NULL),
 EXISTS(SELECT 1 FROM configuration_verifications WHERE organization_id=$1 AND kind='model' AND dispatch_started_at IS NOT NULL)
 FROM organizations WHERE id=$1`, f.f.owner.OrganizationID).Scan(&held, &modelMarker, &verificationMarker); err != nil || held != 17 || !modelMarker || !verificationMarker {
			t.Error("model request preceded exact reservation or either durable marker", held, modelMarker, verificationMarker, err)
		}
		if reply != nil {
			reply(w, r)
		} else {
			independentModelReply(w, r)
		}
	}))
	t.Cleanup(peer.Close)
	f.profile, _ = independentConfigurationInstallModel(t, f.f, peer.URL)
	f.actor, _, _ = independentSecurityKey(t, f.f.s, f.f.service, "member", time.Hour)
	g := independentConfigurationGrant(t, f.f, f.f.owner, f.actor.ID, nil, 2, 2, 17, domain.ConfigurationDestinationRef{ID: f.profile.Destination.Destination.ID, Version: 1})
	active := true
	plan, _ := independentConfigurationPlan(t, f.f, f.actor, g.ID, domain.ConfigurationOperationInput{ID: "model", DestinationID: f.profile.Destination.Destination.ID, DestinationVersion: 1, Active: &active})
	plan = independentConfigurationApply(t, f.f, f.actor, plan, independentConfigurationValidate(t, f.f, f.actor, plan))
	f.verification, _ = independentConfigurationStart(t, f.f, f.actor, plan, "model", domain.NewID("key"), 17)
	var err error
	f.lease, f.model, err = f.f.s.ClaimModel(context.Background(), "independent_configuration_model_worker", time.Minute)
	if err != nil || f.lease == nil || f.lease.TaskID != f.verification.TaskID || f.lease.ConfigurationVerificationID != f.verification.ID {
		t.Fatal("ordinary model worker did not claim exact configuration task", err)
	}
	return f
}

func (f *independentConfigurationModel) runtime(t *testing.T) *modelruntime.Runtime {
	t.Helper()
	runtime, err := modelruntime.New(f.f.s, RuntimeModelConfig(f.model, independentConfigurationProbeSecret))
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func (f *independentConfigurationModel) reserve(t *testing.T) domain.Reservation {
	t.Helper()
	key, intent, err := f.runtime(t).DescribeIntent(*f.lease)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.f.s.ReserveCost(context.Background(), f.lease, domain.ReserveCostRequest{ChargeKey: key, Category: "model", Provider: f.model.ProviderAccount,
		PriceVersion: f.model.PriceVersion, AmountMicroUSD: 17, IntentHash: intent})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestIndependentConfigurationPurposeRetirementStopsExistingModelBeforeReserveAndDispatch(t *testing.T) {
	for _, phase := range []string{"before_reserve", "after_reserve_before_dispatch"} {
		t.Run(phase, func(t *testing.T) {
			f := independentConfigurationModelSetup(t, nil)
			ctx := context.Background()
			var reservation domain.Reservation
			if phase == "after_reserve_before_dispatch" {
				reservation = f.reserve(t)
			}
			purpose := f.profile.Purpose
			purpose.Version, purpose.Active = 2, true
			if _, err := f.f.s.InstallCredentialPurposeProfile(ctx, purpose); err != nil {
				t.Fatal(err)
			}
			var active bool
			if err := f.f.s.Pool.QueryRow(ctx, "SELECT active FROM model_connections WHERE organization_id=$1 AND id=$2 AND version=1", f.f.owner.OrganizationID, f.model.ID).Scan(&active); err != nil || !active {
				t.Fatal("test must keep copied model profile active while retiring only its purpose", active, err)
			}
			if phase == "before_reserve" {
				_, err := f.runtime(t).Execute(ctx, f.lease)
				independentModelError(t, err, "reservation_not_admitted", false)
			} else {
				_, err := f.f.s.PrepareModelDispatch(ctx, f.lease, reservation.ID)
				code(t, err, "forbidden")
			}
			if err := f.f.s.PauseModelAfterRuntimeError(ctx, f.lease, "model_attempt_requires_attention"); err != nil {
				t.Fatal(err)
			}
			if phase == "after_reserve_before_dispatch" {
				changed, err := f.f.s.ReconcileModelUnsent(ctx)
				if err != nil || !changed {
					t.Fatal("durably proven-unsent reservation was not released", changed, err)
				}
			}
			var held int64
			var marked bool
			if err := f.f.s.Pool.QueryRow(ctx, `SELECT reserved_microusd,EXISTS(SELECT 1 FROM actions WHERE organization_id=$1 AND dispatch_issued_at IS NOT NULL)
 FROM organizations WHERE id=$1`, f.f.owner.OrganizationID).Scan(&held, &marked); err != nil || held != 0 || marked || f.calls.Load() != 0 {
				t.Fatal("retired purpose admitted network, marker or retained proven-unsent reserve", held, marked, f.calls.Load(), err)
			}
		})
	}
}

func TestIndependentConfigurationModelEchoAndLateDisableKeepOnlySafeFactsAndLiability(t *testing.T) {
	for _, disable := range []bool{false, true} {
		name := "successful_echo_scrub"
		if disable {
			name = "late_response_after_account_disable"
		}
		t.Run(name, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			f := independentConfigurationModelSetup(t, func(w http.ResponseWriter, _ *http.Request) {
				close(entered)
				<-release
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": independentConfigurationProbeSecret,
					"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": independentConfigurationProbeSecret}, "finish_reason": "stop"}},
					"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}})
			})
			type result struct {
				out modelruntime.Outcome
				err error
			}
			completed := make(chan result, 1)
			go func() { out, err := f.runtime(t).Execute(context.Background(), f.lease); completed <- result{out, err} }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("actual model request did not reach bounded peer")
			}
			if disable {
				independentSecurityServiceUpdate(t, f.f.s, f.f.owner, f.f.service, "member", false)
			}
			once.Do(func() { close(release) })
			var got result
			select {
			case got = <-completed:
			case <-time.After(6 * time.Second):
				t.Fatal("model response fact did not finish")
			}
			if disable {
				var typed *modelruntime.ExecutionError
				if !errors.As(got.err, &typed) || typed.Code != "candidate_not_committed" || !typed.Unknown {
					t.Fatal("revoked actor published candidate or lost safe receipt", got.out.Status, got.err)
				}
				if err := f.f.s.PauseModelAfterRuntimeError(context.Background(), f.lease, "model_attempt_requires_attention"); err != nil {
					t.Fatal(err)
				}
			} else if got.err != nil || got.out.Status != modelruntime.CandidateWaitingInput {
				t.Fatal("ordinary safe probe response did not publish", got.out.Status, got.err)
			}
			v, err := f.f.s.ConfigurationVerification(context.Background(), f.f.owner, f.verification.ID)
			if err != nil || v.Status != "response_received" || v.CostStatus != "unknown" || len(v.ResponseDigest) != 64 {
				t.Fatal("late structural fact or independent accounting was lost", v.Status, v.CostStatus, err)
			}
			costs, err := f.f.s.Costs(context.Background(), f.f.owner, v.TaskID, CostCursors{Limit: 100})
			if err != nil || costs.ReservedMicroUSD != 17 || costs.UnknownMicroUSD != 17 || costs.SpentMicroUSD != 0 {
				t.Fatal("probe success or revocation erased monetary liability", costs.ReservedMicroUSD, costs.UnknownMicroUSD, costs.SpentMicroUSD, err)
			}
			artifacts, err := f.f.s.Artifacts(context.Background(), f.f.owner, v.TaskID)
			if err != nil || (disable && len(artifacts) != 0) || (!disable && len(artifacts) != 1) {
				t.Fatal("publication ignored current authority", len(artifacts), err)
			}
			rows, err := f.f.s.Pool.Query(context.Background(), "SELECT tablename FROM pg_tables WHERE schemaname=current_schema() ORDER BY tablename")
			if err != nil {
				t.Fatal(err)
			}
			tables := []string{}
			for rows.Next() {
				var table string
				if err = rows.Scan(&table); err != nil {
					t.Fatal(err)
				}
				tables = append(tables, table)
			}
			rows.Close()
			for _, table := range tables {
				var leaked bool
				if err = f.f.s.Pool.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM "+pgx.Identifier{table}.Sanitize()+" x WHERE to_jsonb(x)::text LIKE $1)", "%"+independentConfigurationProbeSecret+"%").Scan(&leaked); err != nil || leaked {
					t.Fatal("raw credential echo entered durable table", table, leaked, err)
				}
			}
			if _, err = f.f.s.ReconcileModelUnsent(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err = f.f.s.ReconcileConfigurationVerifications(context.Background()); err != nil {
				t.Fatal(err)
			}
			lease, _, err := f.f.s.ClaimModel(context.Background(), "independent_probe_restart", time.Minute)
			if err != nil || lease != nil || f.calls.Load() != 1 {
				t.Fatal("probe response was resent on restart", lease != nil, f.calls.Load(), err)
			}
		})
	}
}

func TestIndependentConfigurationPlanClipsGrantExpiryAndRejectsAliasedTarget(t *testing.T) {
	f := independentConfigurationSchemaFixture(t)
	ctx := context.Background()
	alias := f.dest
	alias.Destination.ID = domain.NewID("destination")
	alias.Destination.Version = 1
	alias.Destination.Active = true
	if _, err := f.s.InstallConfigurationDestination(ctx, alias); err != nil {
		t.Fatal(err)
	}
	actor, _, _ := independentSecurityKey(t, f.s, f.service, "member", time.Hour)
	raw, err := f.s.CreateConfigurationGrant(ctx, f.owner, domain.NewID("key"), domain.CreateConfigurationGrant{
		SubjectID: actor.ID, Actions: []string{"inspect", "plan", "validate", "apply", "verify"},
		Destinations: []domain.ConfigurationDestinationRef{{ID: f.dest.Destination.ID, Version: 1}, {ID: alias.Destination.ID, Version: 1}},
		TTLSeconds:   10, MaxPlans: 2, MaxVerifications: 2, Reason: "Short grant must bound accepted plan expiry"})
	g := independentConfigurationDecode[domain.CapabilityEnvelope](t, raw, err)
	catalog, err := f.s.ConfigurationCatalog(ctx, actor)
	if err != nil {
		t.Fatal(err)
	}
	active := true
	in := domain.CreateConfigurationPlan{GrantID: g.ID, BaseRevision: catalog.Revision, TTLSeconds: 300,
		Operations: []domain.ConfigurationOperationInput{{ID: "one", DestinationID: f.dest.Destination.ID, DestinationVersion: 1, Active: &active}},
		Reason:     "Requested plan lifetime is an upper bound under grant expiry"}
	key := domain.NewID("key")
	raw, err = f.s.CreateConfigurationPlan(ctx, actor, key, in)
	plan := independentConfigurationDecode[domain.ConfigurationPlan](t, raw, err)
	if !plan.ExpiresAt.Equal(g.ExpiresAt) || !plan.ExpiresAt.After(plan.CreatedAt) {
		t.Fatal("plan did not clip to actual delegated lifetime", plan.ExpiresAt, g.ExpiresAt)
	}
	raw, err = f.s.CreateConfigurationPlan(ctx, actor, key, in)
	replay := independentConfigurationDecode[domain.ConfigurationPlan](t, raw, err)
	if replay.ID != plan.ID || replay.Digest != plan.Digest || !replay.ExpiresAt.Equal(plan.ExpiresAt) {
		t.Fatal("replay renewed clipped plan lifetime or changed intent")
	}
	in.Operations = append(in.Operations, domain.ConfigurationOperationInput{ID: "alias", DestinationID: alias.Destination.ID, DestinationVersion: 1, Active: &active})
	_, err = f.s.CreateConfigurationPlan(ctx, actor, domain.NewID("key"), in)
	code(t, err, "invalid_request")
	state, err := f.s.ConfigurationGrant(ctx, f.owner, g.ID)
	if err != nil || state.UsedPlans != 1 {
		t.Fatal("alias rejection or replay changed cumulative grant usage", state.UsedPlans, err)
	}
}
