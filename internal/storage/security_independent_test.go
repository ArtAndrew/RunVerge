// SPDX-License-Identifier: Elastic-2.0
package storage

// Independent security lifecycle regressions use isolated real PostgreSQL
// schemas and actual local model/TLS tool peers. A directly supplied trusted
// runtime issuer tests Store admission, not IdP configuration readiness; the
// separate HTTP tests exercise an initialized LoginService.
import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	oidcidentity "agentplatform.local/platform/internal/identity"
	"agentplatform.local/platform/internal/modelruntime"
	"agentplatform.local/platform/internal/toolruntime"
)

func independentSecurityService(t *testing.T, s *Store, p domain.Principal, role string) domain.ServiceAccount {
	t.Helper()
	raw, err := s.CreateServiceAccount(context.Background(), p, domain.NewID("key"), domain.CreateServiceAccount{DisplayName: "Independent service identity", Role: role, Reason: "Explicit independent service provisioning"})
	if err != nil {
		t.Fatal(err)
	}
	var v domain.ServiceAccount
	if err = json.Unmarshal(raw, &v); err != nil || v.PrincipalID == "" || v.PrincipalKind != domain.PrincipalService {
		t.Fatal("missing service identity", err)
	}
	return v
}

func independentSecurityKey(t *testing.T, s *Store, v domain.ServiceAccount, ceiling string, ttl time.Duration) (domain.Principal, string, domain.ServiceKey) {
	t.Helper()
	raw, key, err := s.IssueServiceToken(context.Background(), v.OrganizationID, v.PrincipalID, ceiling, ttl, "Independent trusted-host credential issuance")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Authenticate(context.Background(), raw, v.OrganizationID)
	if err != nil || p.ID != v.PrincipalID || p.Kind != domain.PrincipalService {
		t.Fatal("issued service key did not authenticate its exact service", err)
	}
	return p, raw, key
}

func independentSecurityServiceUpdate(t *testing.T, s *Store, p domain.Principal, v domain.ServiceAccount, role string, active bool) domain.ServiceAccount {
	t.Helper()
	raw, err := s.UpdateServiceAccount(context.Background(), p, v.PrincipalID, domain.NewID("key"), domain.UpdateServiceAccount{ExpectedVersion: v.Version, DisplayName: v.DisplayName, Role: role, Active: &active, Reason: "Independent service authority transition"})
	if err != nil {
		t.Fatal(err)
	}
	var next domain.ServiceAccount
	if err = json.Unmarshal(raw, &next); err != nil {
		t.Fatal(err)
	}
	return next
}

func independentSecurityPolicy(t *testing.T, s *Store, p domain.Principal, runtime domain.LoginPolicyRuntime) domain.OrganizationLoginPolicy {
	t.Helper()
	v, err := s.LoginPolicy(context.Background(), p, runtime)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func independentSecurityPolicyUpdate(t *testing.T, s *Store, p domain.Principal, v domain.OrganizationLoginPolicy, mode, issuer string, enabled bool, ttl int, runtime domain.LoginPolicyRuntime) domain.OrganizationLoginPolicy {
	t.Helper()
	raw, err := s.UpdateLoginPolicy(context.Background(), p, domain.NewID("key"), domain.UpdateOrganizationLoginPolicy{ExpectedVersion: v.Version, Mode: mode, OIDCIssuer: issuer, ServiceAccessEnabled: &enabled, MaxServiceKeyTTLSeconds: ttl, Reason: "Independent organization security transition"}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	var next domain.OrganizationLoginPolicy
	if err = json.Unmarshal(raw, &next); err != nil {
		t.Fatal(err)
	}
	return next
}

func independentSecurityBrowser(t *testing.T, s *Store, p domain.Principal) (domain.Principal, oidcidentity.BrowserSession, domain.LoginPolicyRuntime) {
	t.Helper()
	session := independentSession(t, s, p, domain.NewID("independent-security-subject"), time.Hour)
	browser, err := s.AuthenticateBrowserSession(context.Background(), session.TokenHash, p.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	return browser, session, domain.LoginPolicyRuntime{TrustedIssuer: session.Issuer}
}

func independentSecurityModeCycle(t *testing.T, s *Store, browser domain.Principal, runtime domain.LoginPolicyRuntime) {
	t.Helper()
	v := independentSecurityPolicy(t, s, browser, runtime)
	v = independentSecurityPolicyUpdate(t, s, browser, v, "oidc_required", runtime.TrustedIssuer, v.ServiceAccessEnabled, v.MaxServiceKeyTTLSeconds, runtime)
	_ = independentSecurityPolicyUpdate(t, s, browser, v, "mixed", "", v.ServiceAccessEnabled, v.MaxServiceKeyTTLSeconds, runtime)
}

func TestIndependentSecurityPolicyRoundtripDoesNotReviveHumanBearerOrQueuedConsent(t *testing.T) {
	f := independentModelSetup(t, false, nil)
	ctx := context.Background()
	other, _ := identity(t, f.s, 100000)
	if _, err := f.s.Pool.Exec(ctx, "INSERT INTO memberships(organization_id,principal_id,role,display_name) VALUES($1,$2,'viewer','Shared principal in B')", other.OrganizationID, f.p.ID); err != nil {
		t.Fatal(err)
	}
	browser, session, runtime := independentSecurityBrowser(t, f.s, f.p)
	policy := independentSecurityPolicy(t, f.s, browser, runtime)
	policy = independentSecurityPolicyUpdate(t, f.s, browser, policy, "oidc_required", runtime.TrustedIssuer, policy.ServiceAccessEnabled, policy.MaxServiceKeyTTLSeconds, runtime)
	independentMemberAuthority(t, f.s, f.p, "", true)
	otherActor := f.p
	otherActor.OrganizationID = other.OrganizationID
	independentMemberAuthority(t, f.s, otherActor, "viewer", false)
	if _, err := f.s.AuthenticateBrowserSession(ctx, session.TokenHash, f.p.OrganizationID); err != nil {
		t.Fatal("matching OIDC browser was denied by its policy", err)
	}
	policy = independentSecurityPolicyUpdate(t, f.s, browser, policy, "mixed", "", policy.ServiceAccessEnabled, policy.MaxServiceKeyTTLSeconds, runtime)
	independentMemberAuthority(t, f.s, f.p, "", true)
	independentMemberAuthority(t, f.s, otherActor, "viewer", false)
	rotation := session
	rotation.ID, rotation.TokenHash = domain.NewID("ses"), Hash([]byte(domain.NewID("private-browser-rotation")))
	rotation.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
	if err := f.s.RotateSession(ctx, session.TokenHash, rotation); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AuthenticateBrowserSession(ctx, rotation.TokenHash, f.p.OrganizationID); err != nil {
		t.Fatal("human bearer cutoff incorrectly revoked a browser rotation", err)
	}
	fresh, _, err := f.s.IssueMemberToken(ctx, f.p.OrganizationID, f.p.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Authenticate(ctx, fresh, f.p.OrganizationID); err != nil {
		t.Fatal("fresh mixed-mode login did not regain admission", err)
	}
	lease, _, err := f.s.ClaimModel(ctx, "independent-human-policy-cycle", time.Second)
	if err != nil || lease != nil || f.calls.Load() != 0 {
		t.Fatal("policy roundtrip revived old queued human consent", err)
	}
	var taskState, operationState string
	if err = f.s.Pool.QueryRow(ctx, "SELECT t.status,o.status FROM tasks t JOIN operations o ON o.organization_id=t.organization_id AND o.task_id=t.id WHERE t.organization_id=$1 AND t.id=$2", f.p.OrganizationID, f.task.ID).Scan(&taskState, &operationState); err != nil || taskState != "blocked" || operationState != "blocked" {
		t.Fatal("old human job was not durably blocked", taskState, operationState, err)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 0, 0)
	otherPolicy := independentSecurityPolicy(t, f.s, other, domain.LoginPolicyRuntime{})
	if otherPolicy.Mode != "mixed" || otherPolicy.Version != 1 {
		t.Fatal("A policy changed B", otherPolicy)
	}
}

func TestIndependentSecurityViewerKeyCeilingAndAlternateHumanPaths(t *testing.T) {
	s, _ := testStore(t)
	owner, _ := identity(t, s, 100000)
	other, _ := identity(t, s, 100000)
	ctx := context.Background()
	a := agent(t, s, owner)
	v := independentSecurityService(t, s, owner, "viewer")
	_, old, key := independentSecurityKey(t, s, v, "viewer", time.Hour)
	v = independentSecurityServiceUpdate(t, s, owner, v, "member", true)
	p, err := s.Authenticate(ctx, old, owner.OrganizationID)
	if err != nil || p.Role != "viewer" {
		t.Fatal("promotion expanded an old viewer key", p.Role, err)
	}
	_, err = s.CreateTask(ctx, p, domain.NewID("key"), domain.CreateTask{Goal: "Must not acquire work through promotion", AgentID: a.ID, BudgetMicroUSD: 1000})
	code(t, err, "forbidden")
	if _, err = s.Authenticate(ctx, old, other.OrganizationID); err == nil {
		t.Fatal("service credential crossed its home organization")
	}
	_, err = s.ServiceAccount(ctx, other, v.PrincipalID)
	code(t, err, "not_found")
	wrongParent := independentSecurityService(t, s, owner, "member")
	_, err = s.ServiceKey(ctx, owner, wrongParent.PrincipalID, key.ID)
	code(t, err, "not_found")
	_, _, err = s.IssueMemberToken(ctx, owner.OrganizationID, v.PrincipalID, time.Hour)
	if err == nil {
		t.Fatal("service identity acquired a legacy human bearer")
	}
	if err = s.LinkIdentity(ctx, "https://issuer.example.test", "service-must-not-be-human", v.PrincipalID); err == nil {
		t.Fatal("service identity acquired an OIDC mapping")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err = s.CreateSession(ctx, oidcidentity.BrowserSession{ID: domain.NewID("ses"), TokenHash: Hash([]byte("private-invalid-service-browser")), PrincipalID: v.PrincipalID, Issuer: "https://issuer.example.test", Subject: "service-must-not-be-human", AuthenticatedAt: now, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, ""); err == nil {
		t.Fatal("service identity acquired a browser session")
	}
	yes := true
	_, err = s.UpdateMember(ctx, owner, v.PrincipalID, domain.NewID("key"), domain.UpdateOrganizationMember{ExpectedVersion: v.Version, DisplayName: v.DisplayName, Role: "owner", Active: &yes, Reason: "Attempt human-membership promotion bypass"})
	if err == nil {
		t.Fatal("generic member route promoted a service to owner")
	}
	var mappings, legacy int
	if err = s.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM identity_mappings WHERE principal_id=$1),(SELECT count(*) FROM access_tokens WHERE principal_id=$1)", v.PrincipalID).Scan(&mappings, &legacy); err != nil || mappings != 0 || legacy != 0 {
		t.Fatal("rejected alternate path left human authority", mappings, legacy, err)
	}
	member, _, _ := independentSecurityKey(t, s, v, "member", time.Hour)
	created := task(t, s, member, a)
	if created.OwnerID != v.PrincipalID {
		t.Fatal("service work was attributed to another identity")
	}
	forged := member
	forged.Kind, forged.Role = domain.PrincipalHuman, "owner"
	_, err = s.CreateMember(ctx, forged, domain.NewID("key"), domain.CreateOrganizationMember{DisplayName: "Must not trust supplied kind", Role: "owner", Reason: "Attempt stale/forged principal metadata"})
	code(t, err, "forbidden")
	for _, permission := range []string{"tasks.review_own", "identity.manage", "configuration.manage", "organization.manage", "actions.approve_any"} {
		if domain.PrincipalHasPermission(member, permission) {
			t.Fatal("service received human/admin permission", permission)
		}
	}
	for _, kind := range []string{"", "unknown", "HUMAN"} {
		forged := owner
		forged.Kind = kind
		if len(domain.EffectivePermissions(forged)) != 0 {
			t.Fatal("unknown identity kind gained permissions", kind)
		}
	}
}

func TestIndependentSecurityServiceKeyExpiryAndRevokeDoNotCancelAcceptedModelWork(t *testing.T) {
	for _, boundary := range []string{"expiry", "revoke"} {
		t.Run(boundary, func(t *testing.T) {
			f := independentModelSetup(t, false, nil)
			ctx := context.Background()
			if _, err := f.s.CancelTask(ctx, f.p, f.task.ID, domain.NewID("key")); err != nil {
				t.Fatal(err)
			}
			v := independentSecurityService(t, f.s, f.p, "member")
			ttl := time.Hour
			if boundary == "expiry" {
				ttl = time.Second
			}
			actor, raw, key := independentSecurityKey(t, f.s, v, "member", ttl)
			in := domain.CreateTask{Goal: "Durable service job survives admission credential loss", AgentID: f.task.AgentID, BudgetMicroUSD: 1000}
			body, err := f.s.CreateTask(ctx, actor, domain.NewID("key"), in)
			if err != nil {
				t.Fatal(err)
			}
			var job domain.Task
			if json.Unmarshal(body, &job) != nil {
				t.Fatal("invalid service task")
			}
			if boundary == "revoke" {
				_, err = f.s.RevokeServiceKey(ctx, f.p, v.PrincipalID, key.ID, domain.NewID("key"), domain.RevokeServiceKey{ExpectedVersion: key.Version, Reason: "Independent admission-only key revocation"})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				deadline := time.Now().Add(4 * time.Second)
				for {
					var expired bool
					if err = f.s.Pool.QueryRow(ctx, "SELECT clock_timestamp()>=$1", key.ExpiresAt).Scan(&expired); err != nil {
						t.Fatal(err)
					}
					if expired {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("service key did not naturally expire")
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			if _, err = f.s.Authenticate(ctx, raw, v.OrganizationID); err == nil {
				t.Fatal("expired/revoked key admitted a new request")
			}
			lease, _, err := f.s.ClaimModel(ctx, "independent-durable-service", time.Second)
			if err != nil || lease == nil || lease.TaskID != job.ID {
				t.Fatal("key lifecycle cancelled independent accepted consent", err)
			}
			out, err := f.runtime(t, f.s).Execute(ctx, lease)
			if err != nil || out.Status != modelruntime.CandidateWaitingInput || f.calls.Load() != 1 {
				t.Fatal("accepted service work did not make exactly one model request", out.Status, err)
			}
			fresh, _, _ := independentSecurityKey(t, f.s, v, "member", time.Hour)
			current, err := f.s.GetTask(ctx, f.p, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			candidates, _, err := f.s.Candidates(ctx, f.p, job.ID, "", 10)
			if err != nil || len(candidates) != 1 {
				t.Fatal("missing actual service model candidate", err)
			}
			c := candidates[0]
			_, err = f.s.ReviewTask(ctx, fresh, job.ID, domain.NewID("key"), domain.ReviewTaskRequest{ExpectedVersion: current.Version, CandidateID: c.ID, ArtifactID: c.ArtifactID, ArtifactVersion: c.ArtifactVersion, ContentDigest: c.ContentDigest, Decision: "accept", Reason: "Service must not impersonate human review"})
			code(t, err, "forbidden")
			independentBalances(t, f.s, f.p.OrganizationID, []string{job.ID}, 150, 0)
		})
	}
}

func TestIndependentSecurityServiceTransitionsDoNotReviveKeysJobsOrAffectHumanWork(t *testing.T) {
	for _, boundary := range []string{"account", "organization_policy", "role_cycle"} {
		t.Run(boundary, func(t *testing.T) {
			f := independentModelSetup(t, false, nil)
			ctx := context.Background()
			v := independentSecurityService(t, f.s, f.p, "member")
			actor, old, _ := independentSecurityKey(t, f.s, v, "member", time.Hour)
			raw, err := f.s.CreateTask(ctx, actor, domain.NewID("key"), domain.CreateTask{Goal: "Old service consent cannot return", AgentID: f.task.AgentID, BudgetMicroUSD: 1000})
			if err != nil {
				t.Fatal(err)
			}
			var oldJob domain.Task
			if json.Unmarshal(raw, &oldJob) != nil {
				t.Fatal("invalid service job")
			}
			if boundary == "account" {
				v = independentSecurityServiceUpdate(t, f.s, f.p, v, v.Role, false)
				v = independentSecurityServiceUpdate(t, f.s, f.p, v, v.Role, true)
			} else if boundary == "role_cycle" {
				v = independentSecurityServiceUpdate(t, f.s, f.p, v, "viewer", true)
				v = independentSecurityServiceUpdate(t, f.s, f.p, v, "member", true)
			} else {
				policy := independentSecurityPolicy(t, f.s, f.p, domain.LoginPolicyRuntime{})
				policy = independentSecurityPolicyUpdate(t, f.s, f.p, policy, policy.Mode, policy.OIDCIssuer, false, policy.MaxServiceKeyTTLSeconds, domain.LoginPolicyRuntime{})
				if _, _, err = f.s.IssueServiceToken(ctx, v.OrganizationID, v.PrincipalID, "member", time.Hour, "Must not issue while organization service access disabled"); err == nil {
					t.Fatal("disabled service policy still issued credentials")
				}
				_ = independentSecurityPolicyUpdate(t, f.s, f.p, policy, policy.Mode, policy.OIDCIssuer, true, policy.MaxServiceKeyTTLSeconds, domain.LoginPolicyRuntime{})
			}
			if _, err = f.s.Authenticate(ctx, old, v.OrganizationID); boundary == "role_cycle" {
				if err != nil {
					t.Fatal("role-only transition incorrectly revoked admission credential", err)
				}
			} else if err == nil {
				t.Fatal("off/on transition resurrected an old service key")
			}
			lease, _, err := f.s.ClaimModel(ctx, "independent-unaffected-human-job", time.Second)
			if err != nil || lease == nil || lease.TaskID != f.task.ID {
				t.Fatal("service transition revoked unrelated human consent", err)
			}
			if _, err = f.runtime(t, f.s).Execute(ctx, lease); err != nil || f.calls.Load() != 1 {
				t.Fatal("human job failed after service-only transition", err)
			}
			lease, _, err = f.s.ClaimModel(ctx, "independent-old-service-job", time.Second)
			if err != nil || lease != nil || f.calls.Load() != 1 {
				t.Fatal("off/on transition revived an old service operation", err)
			}
			var state string
			if err = f.s.Pool.QueryRow(ctx, "SELECT status FROM tasks WHERE organization_id=$1 AND id=$2", v.OrganizationID, oldJob.ID).Scan(&state); err != nil || state != "blocked" {
				t.Fatal("old service consent was not durably stopped", state, err)
			}
			fresh, _, _ := independentSecurityKey(t, f.s, v, "member", time.Hour)
			_ = task(t, f.s, fresh, domain.Agent{ID: f.task.AgentID})
			lease, _, err = f.s.ClaimModel(ctx, "independent-fresh-service-consent", time.Second)
			if err != nil || lease == nil || lease.TaskID == oldJob.ID {
				t.Fatal("fresh service consent failed to replace revoked authority", err)
			}
			if _, err = f.runtime(t, f.s).Execute(ctx, lease); err != nil || f.calls.Load() != 2 {
				t.Fatal("fresh service job did not execute exactly once", err)
			}
			var held, spent int64
			if err = f.s.Pool.QueryRow(ctx, "SELECT reserved_microusd,spent_microusd FROM tasks WHERE organization_id=$1 AND id=$2", v.OrganizationID, oldJob.ID).Scan(&held, &spent); err != nil || held != 0 || spent != 0 {
				t.Fatal("stopped old job allocated money", held, spent, err)
			}
			independentBalances(t, f.s, f.p.OrganizationID, nil, 300, 0)
		})
	}
}

func TestIndependentSecurityTTLPolicyAffectsIssuanceWithoutRevokingOldCredentials(t *testing.T) {
	s, _ := testStore(t)
	owner, _ := identity(t, s, 100000)
	ctx := context.Background()
	v := independentSecurityService(t, s, owner, "member")
	_, token, key := independentSecurityKey(t, s, v, "member", time.Hour)
	policy := independentSecurityPolicy(t, s, owner, domain.LoginPolicyRuntime{})
	_ = independentSecurityPolicyUpdate(t, s, owner, policy, policy.Mode, policy.OIDCIssuer, policy.ServiceAccessEnabled, 2, domain.LoginPolicyRuntime{})
	if _, err := s.Authenticate(ctx, token, v.OrganizationID); err != nil {
		t.Fatal("TTL issuance cap retroactively revoked an existing key", err)
	}
	current, err := s.ServiceKey(ctx, owner, v.PrincipalID, key.ID)
	if err != nil || !current.ExpiresAt.Equal(key.ExpiresAt) || current.Version != key.Version || current.Revoked {
		t.Fatal("TTL change rewrote existing credential metadata", err)
	}
	if _, _, err = s.IssueServiceToken(ctx, v.OrganizationID, v.PrincipalID, "member", 3*time.Second, "Exceeds current issuance ceiling"); err == nil {
		t.Fatal("issuance ignored the current TTL ceiling")
	}
	_, bounded, err := s.IssueServiceToken(ctx, v.OrganizationID, v.PrincipalID, "viewer", 2*time.Second, "At current TTL ceiling")
	if err != nil || bounded.ExpiresAt.Sub(bounded.CreatedAt) != 2*time.Second {
		t.Fatal("database did not bind the admitted lifetime", err)
	}
	var count int
	if err = s.Pool.QueryRow(ctx, "SELECT count(*) FROM service_credentials WHERE organization_id=$1 AND principal_id=$2", v.OrganizationID, v.PrincipalID).Scan(&count); err != nil || count != 2 {
		t.Fatal("rejected issuance left a credential", count, err)
	}
}

func TestIndependentSecurityHumanPolicyDoesNotRevokeServiceOnlyAuthority(t *testing.T) {
	f := independentModelSetup(t, false, nil)
	ctx := context.Background()
	if _, err := f.s.CancelTask(ctx, f.p, f.task.ID, domain.NewID("key")); err != nil {
		t.Fatal(err)
	}
	v := independentSecurityService(t, f.s, f.p, "member")
	actor, raw, _ := independentSecurityKey(t, f.s, v, "member", time.Hour)
	job := task(t, f.s, actor, domain.Agent{ID: f.task.AgentID})
	browser, _, runtime := independentSecurityBrowser(t, f.s, f.p)
	independentSecurityModeCycle(t, f.s, browser, runtime)
	if _, err := f.s.Authenticate(ctx, raw, v.OrganizationID); err != nil {
		t.Fatal("human login policy cut off an unrelated service credential", err)
	}
	lease, _, err := f.s.ClaimModel(ctx, "independent-service-under-human-policy", time.Second)
	if err != nil || lease == nil || lease.TaskID != job.ID {
		t.Fatal("human policy epoch revoked service-only background consent", err)
	}
	if _, err = f.runtime(t, f.s).Execute(ctx, lease); err != nil || f.calls.Load() != 1 {
		t.Fatal("unaffected service work failed or repeated", err)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{job.ID}, 150, 0)
}

func TestIndependentSecurityPolicyReadinessCannotSurviveMappingLossBehindIAMGate(t *testing.T) {
	s, _ := testStore(t)
	owner, _ := identity(t, s, 100000)
	browser, session, runtime := independentSecurityBrowser(t, s, owner)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	policy := independentSecurityPolicy(t, s, browser, runtime)
	yes := true
	in := domain.UpdateOrganizationLoginPolicy{ExpectedVersion: policy.Version, Mode: "oidc_required", OIDCIssuer: runtime.TrustedIssuer, ServiceAccessEnabled: &yes, MaxServiceKeyTTLSeconds: policy.MaxServiceKeyTTLSeconds, Reason: "Require current owner OIDC linkage"}
	for _, attempt := range []struct {
		actor   domain.Principal
		runtime domain.LoginPolicyRuntime
	}{{owner, runtime}, {browser, domain.LoginPolicyRuntime{}}, {browser, domain.LoginPolicyRuntime{TrustedIssuer: "https://wrong-issuer.example.test"}}} {
		_, err := s.UpdateLoginPolicy(ctx, attempt.actor, domain.NewID("key"), in, attempt.runtime)
		code(t, err, "forbidden")
	}
	blocker, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "organization-membership-administration\x1f"+owner.OrganizationID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := s.UpdateLoginPolicy(ctx, browser, domain.NewID("key"), in, runtime); done <- e }()
	independentMembershipWaitBlocked(t, s, int32(blocker.Conn().PgConn().PID()), "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", 1)
	if _, err = blocker.Exec(ctx, "UPDATE identity_mappings SET active=false WHERE issuer=$1 AND subject=$2", session.Issuer, session.Subject); err != nil {
		t.Fatal(err)
	}
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	code(t, <-done, "unauthenticated")
	var version, audit int
	if err = s.Pool.QueryRow(ctx, "SELECT (SELECT version FROM organization_login_policies WHERE organization_id=$1),(SELECT count(*) FROM organization_security_audit WHERE organization_id=$1)", owner.OrganizationID).Scan(&version, &audit); err != nil || version != 1 || audit != 0 {
		t.Fatal("stale readiness committed policy or audit", version, audit, err)
	}
}

func TestIndependentSecurityPolicyCycleStopsQueuedToolWithoutReleasingModelLiability(t *testing.T) {
	f := independentNativeSetup(t, nil)
	ctx := context.Background()
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	v := f.prepare(t, f.proposal(t))
	v = f.decide(t, v, "approve")
	raw, err := f.s.DispatchToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDispatch(v))
	v = independentToolView(t, raw, err)
	browser, _, runtime := independentSecurityBrowser(t, f.s, f.p)
	independentSecurityModeCycle(t, f.s, browser, runtime)
	worked, err := f.s.ReconcileToolUnsent(ctx)
	if err != nil || !worked {
		t.Fatal("policy-invalid unsent job was not promptly reconciled", err)
	}
	view, err := f.s.GetToolExecution(ctx, browser, f.task.ID, v.Execution.ID)
	if err != nil || view.Execution.Status != "cancelled" || view.Receipt != nil {
		t.Fatal("policy epoch did not stop exact unsent job", view.Execution.Status, err)
	}
	if f.modelCalls.Load() != 1 || f.toolCalls.Load() != 0 {
		t.Fatal("revoked queued tool reached provider")
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 300, 0)
}

func TestIndependentSecurityPolicyChangePreservesExactLateTLSFactAndUnsettledHolds(t *testing.T) {
	f := independentNativeSetup(t, nil)
	ctx := context.Background()
	browser, _, runtime := independentSecurityBrowser(t, f.s, f.p)
	_, lease, connection := independentWireToolLease(t, f)
	store := &independentMembershipLateToolFact{Store: f.s, observed: make(chan struct{}), release: make(chan struct{})}
	roots := x509.NewCertPool()
	roots.AddCert(f.toolPeer.Certificate())
	r, err := toolruntime.New(store, toolruntime.Config{Connection: connection, Credential: "independent-native-tool-key", TrustedRoots: roots, PersistenceTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := r.Execute(ctx, lease); done <- e }()
	select {
	case <-store.observed:
	case <-time.After(5 * time.Second):
		t.Fatal("actual TLS receipt did not reach the factual persistence barrier")
	}
	independentSecurityModeCycle(t, f.s, browser, runtime)
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
	close(store.release)
	if err = <-done; err != nil {
		t.Fatal("current policy erased a known late external fact", err)
	}
	view, err := f.s.GetToolExecution(ctx, browser, f.task.ID, lease.Execution.ID)
	if err != nil || view.Receipt == nil || view.Receipt.Method != "registered_tool_response" || view.Execution.Status != "succeeded" || f.toolCalls.Load() != 1 || f.modelCalls.Load() != 1 {
		t.Fatal("late fact lost, repeated or replaced with operator assertion", err)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
	if next, _, err := f.s.ClaimToolExecution(ctx, "independent-after-policy-late-fact", time.Second); err != nil || next != nil {
		t.Fatal("late fact restored dispatch authority", err)
	}
	var before int
	if err = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM tool_outcome_receipts WHERE organization_id=$1", f.p.OrganizationID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 1 {
		t.Fatal("late registered fact was not unique", before)
	}
}

func TestIndependentSecurityPolicyChangeCannotEraseAlreadyUnknownToolLiability(t *testing.T) {
	f := independentNativeSetup(t, func(f *independentNativeFixture) { f.toolUnknown = true })
	ctx := context.Background()
	browser, _, runtime := independentSecurityBrowser(t, f.s, f.p)
	_, lease, connection := independentWireToolLease(t, f)
	out, err := independentWireToolRuntime(t, f, f.s, connection).Execute(ctx, lease)
	var runtimeError *toolruntime.Error
	if !errors.As(err, &runtimeError) || runtimeError.Code != "external_outcome_unknown" || out.Status != "unknown" || f.toolCalls.Load() != 1 {
		t.Fatal("actual TLS disconnect did not produce one unknown attempt", err)
	}
	var modelUnknown, toolUnknown int64
	if err = f.s.Pool.QueryRow(ctx, "SELECT COALESCE(sum(amount_microusd) FILTER(WHERE category='model'),0),COALESCE(sum(amount_microusd) FILTER(WHERE category='tool'),0) FROM reservations WHERE organization_id=$1 AND status='unknown'", f.p.OrganizationID).Scan(&modelUnknown, &toolUnknown); err != nil || modelUnknown != 300 || toolUnknown != 120 {
		t.Fatal("fixture lacks exact model300 and tool120 unknown liabilities", modelUnknown, toolUnknown, err)
	}
	initialCosts, err := f.s.Costs(ctx, browser, f.task.ID, CostCursors{Limit: 100})
	if err != nil || initialCosts.UnknownMicroUSD != 420 {
		t.Fatal("initial public costs do not contain both unknown liabilities", initialCosts.UnknownMicroUSD, err)
	}
	liability := func() string {
		t.Helper()
		var raw string
		if err := f.s.Pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'job',(SELECT to_jsonb(e) FROM tool_executions e WHERE e.organization_id=$1 AND e.id=$2),
 'actions',(SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM actions a WHERE a.organization_id=$1),
 'reservations',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM reservations r WHERE r.organization_id=$1),
 'ledger',(SELECT jsonb_agg(to_jsonb(l) ORDER BY id) FROM ledger_entries l WHERE l.organization_id=$1))::text`, f.p.OrganizationID, lease.Execution.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := liability()
	independentSecurityModeCycle(t, f.s, browser, runtime)
	if worked, err := f.s.ReconcileToolUnsent(ctx); err != nil || worked {
		t.Fatal("policy lifecycle treated unknown sent effect as proven unsent", err)
	}
	if before != liability() {
		t.Fatal("policy lifecycle rewrote an unknown effect or released its liability")
	}
	view, err := f.s.GetToolExecution(ctx, browser, f.task.ID, lease.Execution.ID)
	if err != nil || view.Execution.Status != "unknown" || view.Receipt != nil {
		t.Fatal("unknown effect acquired a fictional outcome", err)
	}
	costs, err := f.s.Costs(ctx, browser, f.task.ID, CostCursors{Limit: 100})
	if err != nil || costs.UnknownMicroUSD != 420 || costs.UnknownMicroUSD != initialCosts.UnknownMicroUSD {
		t.Fatal("public costs lost exact unknown hold", costs.UnknownMicroUSD, err)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
	if next, _, err := f.s.ClaimToolExecution(ctx, "independent-policy-unknown-no-resend", time.Second); err != nil || next != nil || f.toolCalls.Load() != 1 {
		t.Fatal("policy roundtrip resent an unknown effect", err)
	}
}

func TestIndependentSecurityDatabaseRejectsServiceKindHomeAndCredentialMutation(t *testing.T) {
	s, _ := testStore(t)
	owner, _ := identity(t, s, 100000)
	other, _ := identity(t, s, 100000)
	v := independentSecurityService(t, s, owner, "member")
	_, _, key := independentSecurityKey(t, s, v, "viewer", time.Hour)
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{"UPDATE principals SET kind='human',service_organization_id=NULL WHERE id=$1", []any{v.PrincipalID}},
		{"UPDATE principals SET service_organization_id=$2 WHERE id=$1", []any{v.PrincipalID, other.OrganizationID}},
		{"INSERT INTO memberships(organization_id,principal_id,display_name,role) VALUES($1,$2,'illegal foreign service','member')", []any{other.OrganizationID, v.PrincipalID}},
		{"UPDATE memberships SET role='owner' WHERE organization_id=$1 AND principal_id=$2", []any{v.OrganizationID, v.PrincipalID}},
		{"INSERT INTO access_tokens(token_hash,principal_id) VALUES($1,$2)", []any{Hash([]byte("independent-illegal-service-human-token")), v.PrincipalID}},
		{"UPDATE service_credentials SET role_ceiling='member' WHERE organization_id=$1 AND id=$2", []any{v.OrganizationID, key.ID}},
		{"UPDATE service_credentials SET expires_at=expires_at+interval '1 hour' WHERE organization_id=$1 AND id=$2", []any{v.OrganizationID, key.ID}},
		{"UPDATE organization_login_policies SET human_authority_epoch=99 WHERE organization_id=$1", []any{v.OrganizationID}},
	} {
		_, err := s.Pool.Exec(context.Background(), statement.sql, statement.args...)
		var pgError interface{ SQLState() string }
		if !errors.As(err, &pgError) || pgError.SQLState() != "23514" {
			t.Fatal("private authority mutation escaped a database invariant", err)
		}
	}
}
