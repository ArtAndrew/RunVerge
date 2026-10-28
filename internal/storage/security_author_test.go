// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
)

func securityAuthorAccount(t *testing.T, s *Store, p domain.Principal, role string) domain.ServiceAccount {
	t.Helper()
	raw, err := s.CreateServiceAccount(context.Background(), p, domain.NewID("key"), domain.CreateServiceAccount{DisplayName: "Bounded organizational integration", Role: role, Reason: "Author explicit provisioning"})
	if err != nil {
		t.Fatal(err)
	}
	var v domain.ServiceAccount
	if err = json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func securityAuthorPolicyInput(v domain.OrganizationLoginPolicy) domain.UpdateOrganizationLoginPolicy {
	enabled := v.ServiceAccessEnabled
	return domain.UpdateOrganizationLoginPolicy{ExpectedVersion: v.Version, Mode: v.Mode, OIDCIssuer: v.OIDCIssuer, ServiceAccessEnabled: &enabled, MaxServiceKeyTTLSeconds: v.MaxServiceKeyTTLSeconds, Reason: "Author explicit policy change"}
}
func securityAuthorPolicy(t *testing.T, s *Store, p domain.Principal) domain.OrganizationLoginPolicy {
	t.Helper()
	v, err := s.LoginPolicy(context.Background(), p, domain.LoginPolicyRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestSecurityAuthorServiceMetadataAuditAndCompensation(t *testing.T) {
	s, _ := testStore(t)
	p, _ := identity(t, s, 100000)
	other, _ := identity(t, s, 100000)
	ctx := context.Background()
	in := domain.CreateServiceAccount{DisplayName: "Nightly integration", Role: "member", Reason: "Explicit provisioning"}
	key := domain.NewID("key")
	first, err := s.CreateServiceAccount(ctx, p, key, in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateServiceAccount(ctx, p, key, in)
	if err != nil {
		t.Fatal(err)
	}
	var a, b domain.ServiceAccount
	if json.Unmarshal(first, &a) != nil || json.Unmarshal(second, &b) != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("idempotent creation changed metadata")
	}
	raw, meta, err := s.IssueServiceToken(ctx, p.OrganizationID, a.PrincipalID, "viewer", time.Minute, "Private delivery")
	if err != nil {
		t.Fatal(err)
	}
	published, _ := json.Marshal(meta)
	if strings.Contains(string(published), raw) || strings.Contains(string(published), Hash([]byte(raw))) || strings.Contains(string(published), "token") {
		t.Fatal("public metadata contains credential material")
	}
	_, err = s.ServiceKey(ctx, other, a.PrincipalID, meta.ID)
	code(t, err, "not_found")
	entries, next, err := s.SecurityAudit(ctx, p, a.PrincipalID, "", 1)
	if err != nil || len(entries) != 1 || next == "" || entries[0].Action != "service_key.issued" || entries[0].ActorType != "trusted_host" {
		t.Fatal("security audit pagination/issuer missing", err)
	}
	older, _, err := s.SecurityAudit(ctx, p, a.PrincipalID, next, 10)
	if err != nil || len(older) != 1 || older[0].Action != "service_account.created" || older[0].ActorID != p.ID {
		t.Fatal("creation audit missing", err)
	}
	if err = s.RevokeIssuedServiceToken(ctx, Hash([]byte(raw)), "Delivery failed"); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeIssuedServiceToken(ctx, Hash([]byte(raw)), "Compensation replay"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Authenticate(ctx, raw, p.OrganizationID)
	code(t, err, "unauthenticated")
	v, err := s.ServiceKey(ctx, p, a.PrincipalID, meta.ID)
	if err != nil || v.Version != 2 || !v.Revoked || v.Status != "revoked" {
		t.Fatal("exact compensation did not revoke key", err)
	}
	entries, _, err = s.SecurityAudit(ctx, p, a.PrincipalID, "", 10)
	if err != nil || len(entries) != 3 {
		t.Fatal("compensation duplicated or lost audit", err)
	}
	if _, err = s.Pool.Exec(ctx, "DELETE FROM organization_security_audit WHERE organization_id=$1", p.OrganizationID); err == nil {
		t.Fatal("security audit deletable")
	}
	if _, err = s.Pool.Exec(ctx, "UPDATE organization_security_audit SET reason='rewrite' WHERE organization_id=$1", p.OrganizationID); err == nil {
		t.Fatal("security audit rewritable")
	}
}
func TestSecurityAuthorPolicyCASAndSeparateEpochs(t *testing.T) {
	s, _ := testStore(t)
	p, _ := identity(t, s, 100000)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := securityAuthorPolicy(t, s, p)
	off := securityAuthorPolicyInput(v)
	disabled := false
	off.ServiceAccessEnabled = &disabled
	ttl := securityAuthorPolicyInput(v)
	ttl.MaxServiceKeyTTLSeconds = 1800
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, in := range []domain.UpdateOrganizationLoginPolicy{off, ttl} {
		wg.Add(1)
		go func(in domain.UpdateOrganizationLoginPolicy) {
			defer wg.Done()
			<-start
			_, err := s.UpdateLoginPolicy(ctx, p, domain.NewID("key"), in, domain.LoginPolicyRuntime{})
			results <- err
		}(in)
	}
	close(start)
	wg.Wait()
	close(results)
	ok, conflicts := 0, 0
	for err := range results {
		if err == nil {
			ok++
		} else {
			var de *domain.Error
			if !errors.As(err, &de) || de.Code != "conflict" {
				t.Fatal("unexpected CAS result", err)
			}
			conflicts++
		}
	}
	if ok != 1 || conflicts != 1 {
		t.Fatal("concurrent policies both committed", ok, conflicts)
	}
	current := securityAuthorPolicy(t, s, p)
	var human, service, cutoff int64
	if err := s.Pool.QueryRow(ctx, "SELECT human_authority_epoch,service_authority_epoch,service_credential_cutoff FROM organization_login_policies WHERE organization_id=$1", p.OrganizationID).Scan(&human, &service, &cutoff); err != nil {
		t.Fatal(err)
	}
	if current.Version != 2 || human != 1 || (!current.ServiceAccessEnabled && (service != 2 || cutoff < 1)) || (current.ServiceAccessEnabled && (service != 1 || cutoff != 0)) {
		t.Fatal("policy epochs crossed authority domains")
	}
	entries, _, err := s.SecurityAudit(ctx, p, "", "", 100)
	if err != nil || len(entries) != 1 {
		t.Fatal("rejected CAS wrote audit", err)
	}
}
func TestSecurityAuthorExactBrowserPolicyAndNonRevivingBearer(t *testing.T) {
	s, _ := testStore(t)
	p, b := identity(t, s, 100000)
	ctx := context.Background()
	a := agent(t, s, p)
	oldTask := task(t, s, p, a)
	ses := independentSession(t, s, p, "author-policy-owner", time.Hour)
	browser, err := s.AuthenticateBrowserSession(ctx, ses.TokenHash, p.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := domain.LoginPolicyRuntime{TrustedIssuer: ses.Issuer}
	v := securityAuthorPolicy(t, s, p)
	in := securityAuthorPolicyInput(v)
	in.Mode = "oidc_required"
	in.OIDCIssuer = ses.Issuer
	_, err = s.UpdateLoginPolicy(ctx, p, domain.NewID("key"), in, runtime)
	code(t, err, "forbidden")
	_, err = s.UpdateLoginPolicy(ctx, browser, domain.NewID("key"), in, domain.LoginPolicyRuntime{})
	code(t, err, "forbidden")
	mismatch := in
	mismatch.OIDCIssuer = "https://other.example.test"
	_, err = s.UpdateLoginPolicy(ctx, browser, domain.NewID("key"), mismatch, runtime)
	code(t, err, "forbidden")
	raw, err := s.UpdateLoginPolicy(ctx, browser, domain.NewID("key"), in, runtime)
	if err != nil {
		t.Fatal(err)
	}
	var current domain.OrganizationLoginPolicy
	if json.Unmarshal(raw, &current) != nil || !current.CanRequireOIDC {
		t.Fatal("policy response lost readiness")
	}
	_, err = s.Authenticate(ctx, b.Token, p.OrganizationID)
	code(t, err, "unauthenticated")
	if _, err = s.AuthenticateBrowserSession(ctx, ses.TokenHash, p.OrganizationID); err != nil {
		t.Fatal("exact owner session denied", err)
	}
	l, err := s.ClaimFixture(ctx, "policy-epoch-worker", time.Second)
	if err != nil || l != nil {
		t.Fatal("old human admission revived after policy transition", err)
	}
	blocked, err := s.GetTask(ctx, browser, oldTask.ID)
	if err != nil || blocked.Status != "blocked" || blocked.WaitingReason != "authorization_revoked" {
		t.Fatal("old task not durably stopped", blocked.Status, blocked.WaitingReason, err)
	}
	if _, err = s.RecoverLoginPolicy(ctx, p.OrganizationID, "Audited trusted-host recovery"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Authenticate(ctx, b.Token, p.OrganizationID)
	code(t, err, "unauthenticated")
	token, _, err := s.IssueMemberToken(ctx, p.OrganizationID, p.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Authenticate(ctx, token, p.OrganizationID)
	if err != nil || fresh.Kind != domain.PrincipalHuman {
		t.Fatal("fresh human admission unavailable", err)
	}
	created := task(t, s, fresh, a)
	l, err = s.ClaimFixture(ctx, "fresh-policy-worker", time.Second)
	if err != nil || l == nil || l.TaskID != created.ID {
		t.Fatal("fresh root work unavailable", err)
	}
	var h, se int64
	if err = s.Pool.QueryRow(ctx, "SELECT human_authority_epoch,service_authority_epoch FROM organization_login_policies WHERE organization_id=$1", p.OrganizationID).Scan(&h, &se); err != nil || h != 3 || se != 1 {
		t.Fatal("recovery changed unrelated service epoch", err)
	}
}
func TestSecurityAuthorKeyRoleCeilingAndAdmissionOnlyRevocation(t *testing.T) {
	s, _ := testStore(t)
	owner, _ := identity(t, s, 100000)
	ctx := context.Background()
	svc := securityAuthorAccount(t, s, owner, "member")
	published := agent(t, s, owner)
	viewerRaw, _, err := s.IssueServiceToken(ctx, owner.OrganizationID, svc.PrincipalID, "viewer", time.Minute, "Bounded read-only automation")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := s.Authenticate(ctx, viewerRaw, owner.OrganizationID)
	if err != nil || viewer.Kind != domain.PrincipalService || viewer.Role != "viewer" {
		t.Fatal("viewer ceiling not applied", err)
	}
	_, err = s.CreateTask(ctx, viewer, domain.NewID("key"), domain.CreateTask{AgentID: published.ID, Goal: "Must not run", BudgetMicroUSD: 10000})
	code(t, err, "forbidden")
	raw, meta, err := s.IssueServiceToken(ctx, owner.OrganizationID, svc.PrincipalID, "member", time.Minute, "Explicit durable admission")
	if err != nil {
		t.Fatal(err)
	}
	service, err := s.Authenticate(ctx, raw, owner.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	accepted := task(t, s, service, published)
	_, err = s.RevokeServiceKey(ctx, owner, svc.PrincipalID, meta.ID, domain.NewID("key"), domain.RevokeServiceKey{ExpectedVersion: meta.Version, Reason: "Stop further admissions"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Authenticate(ctx, raw, owner.OrganizationID)
	code(t, err, "unauthenticated")
	l, err := s.ClaimFixture(ctx, "accepted-after-key-revoke", time.Second)
	if err != nil || l == nil || l.TaskID != accepted.ID {
		t.Fatal("key revoke cancelled accepted durable command", err)
	}
	if err = s.CompleteFixture(ctx, l, "Тестовый результат\n"+domain.ExecutionInput(*l)); err != nil {
		t.Fatal(err)
	}
	candidate, err := s.GetTask(ctx, owner, accepted.ID)
	if err != nil || candidate.Status != "succeeded" || candidate.CandidateID == "" || candidate.SpentMicroUSD != domain.FixtureCost {
		t.Fatal("durable accepted work lost candidate", err)
	}
	// A fresh service credential still cannot turn a model result into a human decision.
	freshRaw, _, err := s.IssueServiceToken(ctx, owner.OrganizationID, svc.PrincipalID, "member", time.Minute, "Review denial probe")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Authenticate(ctx, freshRaw, owner.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	candidates, _, err := s.Candidates(ctx, owner, accepted.ID, "", 10)
	if err != nil || len(candidates) != 1 {
		t.Fatal(err)
	}
	c := candidates[0]
	_, err = s.ReviewTask(ctx, fresh, accepted.ID, domain.NewID("key"), domain.ReviewTaskRequest{ExpectedVersion: candidate.Version, CandidateID: c.ID, ArtifactID: c.ArtifactID, ArtifactVersion: c.ArtifactVersion, ContentDigest: c.ContentDigest, Decision: "accept"})
	code(t, err, "forbidden")
	for _, kind := range []string{"", "system", "unknown"} {
		if len(domain.EffectivePermissions(domain.Principal{Role: "owner", Kind: kind})) != 0 {
			t.Fatal("unknown kind acquired permissions")
		}
	}
}
func TestSecurityAuthorServicePolicyCycleCannotReviveOldOperation(t *testing.T) {
	s, _ := testStore(t)
	owner, _ := identity(t, s, 100000)
	ctx := context.Background()
	svc := securityAuthorAccount(t, s, owner, "member")
	a := agent(t, s, owner)
	raw, _, err := s.IssueServiceToken(ctx, owner.OrganizationID, svc.PrincipalID, "member", time.Minute, "Durable service consent")
	if err != nil {
		t.Fatal(err)
	}
	service, err := s.Authenticate(ctx, raw, owner.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	old := task(t, s, service, a)
	v := securityAuthorPolicy(t, s, owner)
	in := securityAuthorPolicyInput(v)
	off := false
	in.ServiceAccessEnabled = &off
	if _, err = s.UpdateLoginPolicy(ctx, owner, domain.NewID("key"), in, domain.LoginPolicyRuntime{}); err != nil {
		t.Fatal(err)
	}
	v = securityAuthorPolicy(t, s, owner)
	in = securityAuthorPolicyInput(v)
	on := true
	in.ServiceAccessEnabled = &on
	if _, err = s.UpdateLoginPolicy(ctx, owner, domain.NewID("key"), in, domain.LoginPolicyRuntime{}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Authenticate(ctx, raw, owner.OrganizationID)
	code(t, err, "unauthenticated")
	l, err := s.ClaimFixture(ctx, "no-service-epoch-resurrection", time.Second)
	if err != nil || l != nil {
		t.Fatal("service policy off-on revived old operation", err)
	}
	current, err := s.GetTask(ctx, owner, old.ID)
	if err != nil || current.WaitingReason != "authorization_revoked" || current.ReservedMicroUSD != 0 {
		t.Fatal("old service operation did not stop safely", err)
	}
	human := task(t, s, owner, a)
	l, err = s.ClaimFixture(ctx, "unrelated-human-still-works", time.Second)
	if err != nil || l == nil || l.TaskID != human.ID {
		t.Fatal("service policy denied unrelated human work", err)
	}
}

func TestSecurityAuthorLateFixtureFactSurvivesPolicyRevocation(t *testing.T) {
	s, _ := testStore(t)
	owner, _ := identity(t, s, 100000)
	ctx := context.Background()
	svc := securityAuthorAccount(t, s, owner, "member")
	a := agent(t, s, owner)
	raw, _, err := s.IssueServiceToken(ctx, owner.OrganizationID, svc.PrincipalID, "member", time.Minute, "Admit exact bounded synthetic work")
	if err != nil {
		t.Fatal(err)
	}
	service, err := s.Authenticate(ctx, raw, owner.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	accepted := task(t, s, service, a)
	l, err := s.ClaimFixture(ctx, "late-policy-fact", time.Minute)
	if err != nil || l == nil || l.TaskID != accepted.ID {
		t.Fatal("fixture not admitted", err)
	}
	v := securityAuthorPolicy(t, s, owner)
	in := securityAuthorPolicyInput(v)
	off := false
	in.ServiceAccessEnabled = &off
	if _, err = s.UpdateLoginPolicy(ctx, owner, domain.NewID("key"), in, domain.LoginPolicyRuntime{}); err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteFixture(ctx, l, "Тестовый результат\n"+domain.ExecutionInput(*l)); err != nil {
		t.Fatal("known synthetic accounting rejected after policy revocation", err)
	}
	current, err := s.GetTask(ctx, owner, accepted.ID)
	if err != nil || current.Status != "blocked" || current.WaitingReason != "authorization_revoked" || current.ReservedMicroUSD != 0 || current.SpentMicroUSD != domain.FixtureCost || current.CandidateID != "" {
		t.Fatal("late fact was lost or revoked publication accepted", err)
	}
	var status string
	var actual int64
	if err = s.Pool.QueryRow(ctx, "SELECT status,actual_microusd FROM reservations WHERE organization_id=$1 AND id=$2", owner.OrganizationID, l.ReservationID).Scan(&status, &actual); err != nil || status != "settled" || actual != domain.FixtureCost {
		t.Fatal("exact liability not conserved", status, actual, err)
	}
}
