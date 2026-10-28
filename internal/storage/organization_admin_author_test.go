// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
)

func memberAuthorCreate(t *testing.T, s *Store, p domain.Principal, name, role string) domain.OrganizationMember {
	t.Helper()
	raw, err := s.CreateMember(context.Background(), p, domain.NewID("key"), domain.CreateOrganizationMember{DisplayName: name, Role: role, Reason: "Explicit author test provisioning"})
	if err != nil {
		t.Fatal(err)
	}
	var v domain.OrganizationMember
	if err = json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func memberAuthorCredential(t *testing.T, s *Store, org, pid string) domain.Principal {
	t.Helper()
	token := domain.NewID("author_private_token")
	if _, err := s.Pool.Exec(context.Background(), "INSERT INTO access_tokens(token_hash,principal_id) VALUES($1,$2)", Hash([]byte(token)), pid); err != nil {
		t.Fatal(err)
	}
	p, err := s.Authenticate(context.Background(), token, org)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func memberAuthorUpdate(t *testing.T, s *Store, p domain.Principal, v domain.OrganizationMember, role string, active bool) domain.OrganizationMember {
	t.Helper()
	raw, err := s.UpdateMember(context.Background(), p, v.PrincipalID, domain.NewID("key"), domain.UpdateOrganizationMember{ExpectedVersion: v.Version, DisplayName: v.DisplayName, Role: role, Active: &active, Reason: "Explicit author test authority change"})
	if err != nil {
		t.Fatal(err)
	}
	var next domain.OrganizationMember
	if err = json.Unmarshal(raw, &next); err != nil {
		t.Fatal(err)
	}
	return next
}
func TestMembershipAuthorLifecycleIdempotencyAndAudit(t *testing.T) {
	s, _ := testStore(t)
	p, _ := identity(t, s, 100000)
	ctx := context.Background()
	in := domain.CreateOrganizationMember{DisplayName: "Directory label; not a verified email", Role: "member", Reason: "Provision a local principal"}
	key := domain.NewID("key")
	first, err := s.CreateMember(ctx, p, key, in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateMember(ctx, p, key, in)
	if err != nil {
		t.Fatal(err)
	}
	var firstValue, secondValue any
	if json.Unmarshal(first, &firstValue) != nil || json.Unmarshal(second, &secondValue) != nil || !reflect.DeepEqual(firstValue, secondValue) {
		t.Fatal("idempotent provisioning changed its saved response")
	}
	var v domain.OrganizationMember
	if err = json.Unmarshal(first, &v); err != nil {
		t.Fatal(err)
	}
	var principals, mappings, audit int
	if err = s.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM principals),(SELECT count(*) FROM identity_mappings),(SELECT count(*) FROM organization_membership_audit)").Scan(&principals, &mappings, &audit); err != nil || principals != 2 || mappings != 0 || audit != 1 {
		t.Fatal("provisioning invented identity or duplicated audit", err)
	}
	before := v
	v = memberAuthorUpdate(t, s, p, v, "viewer", true)
	if v.Version != before.Version+1 || !v.UpdatedAt.After(before.UpdatedAt) || v.CredentialsRevokedAt != nil {
		t.Fatal("role change failed versioning or incorrectly revoked credentials")
	}
	raw, err := s.RevokeMemberCredentials(ctx, p, v.PrincipalID, domain.NewID("key"), domain.RevokeMemberCredentials{ExpectedVersion: v.Version, Reason: "Revoke organization credentials only"})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &v); err != nil || v.CredentialsRevokedAt == nil {
		t.Fatal("missing revocation audit timestamp", err)
	}
	entries, next, err := s.MembershipAudit(ctx, p, v.PrincipalID, "", 2)
	if err != nil || len(entries) != 2 || next == "" || entries[0].Action != "member.credentials_revoked" || entries[0].Before == nil {
		t.Fatal("audit pagination/state incomplete", err)
	}
	older, _, err := s.MembershipAudit(ctx, p, v.PrincipalID, next, 2)
	if err != nil || len(older) != 1 || older[0].Before != nil || older[0].Action != "member.created" {
		t.Fatal("initial audit lost", err)
	}
	_, err = s.Pool.Exec(ctx, "DELETE FROM organization_membership_audit WHERE organization_id=$1", p.OrganizationID)
	if err == nil {
		t.Fatal("audit history is mutable")
	}
}
func TestMembershipAuthorLocalCutoffAndNoRoleLogout(t *testing.T) {
	s, _ := testStore(t)
	owner, _ := identity(t, s, 100000)
	other, _ := identity(t, s, 100000)
	ctx := context.Background()
	v := memberAuthorCreate(t, s, owner, "Shared principal", "member")
	actor := memberAuthorCredential(t, s, owner.OrganizationID, v.PrincipalID)
	if _, err := s.Pool.Exec(ctx, "INSERT INTO memberships(organization_id,principal_id,role,display_name) VALUES($1,$2,'member','Other organization label')", other.OrganizationID, v.PrincipalID); err != nil {
		t.Fatal(err)
	}
	v = memberAuthorUpdate(t, s, owner, v, "viewer", true)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := authorizeTx(ctx, tx, actor, false)
	_ = tx.Rollback(ctx)
	if err != nil || fresh.Role != "viewer" {
		t.Fatal("role change logged user out instead of updating role", err)
	}
	_, err = s.RevokeMemberCredentials(ctx, owner, v.PrincipalID, domain.NewID("key"), domain.RevokeMemberCredentials{ExpectedVersion: v.Version, Reason: "Target organization cutoff"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err = s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = authorizeTx(ctx, tx, actor, false)
	_ = tx.Rollback(ctx)
	code(t, err, "unauthenticated")
	actor.OrganizationID = other.OrganizationID
	tx, err = s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = authorizeTx(ctx, tx, actor, false)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatal("organization revoke denied another organization", err)
	}
	_ = memberAuthorCredential(t, s, owner.OrganizationID, v.PrincipalID)
}
func TestMembershipAuthorDirectoryOwnerAndSelfBoundaries(t *testing.T) {
	s, _ := testStore(t)
	owner, _ := identity(t, s, 100000)
	other, _ := identity(t, s, 100000)
	ctx := context.Background()
	admin := memberAuthorCreate(t, s, owner, "Admin", "admin")
	ap := memberAuthorCredential(t, s, owner.OrganizationID, admin.PrincipalID)
	member := memberAuthorCreate(t, s, owner, "Member", "member")
	mp := memberAuthorCredential(t, s, owner.OrganizationID, member.PrincipalID)
	_, _, err := s.Members(ctx, mp, "", 10)
	code(t, err, "forbidden")
	caps, err := s.MembershipCapabilities(ctx, mp)
	if err != nil || caps.CanCreateMembers || len(caps.AssignableRoles) != 0 || caps.SelfManagementAllowed {
		t.Fatal("member received admin capabilities", err)
	}
	_, err = s.CreateMember(ctx, ap, domain.NewID("key"), domain.CreateOrganizationMember{DisplayName: "Illegal owner", Role: "owner", Reason: "Attempt privilege escalation"})
	code(t, err, "forbidden")
	ov, err := s.Member(ctx, owner, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	_, err = s.UpdateMember(ctx, ap, owner.ID, domain.NewID("key"), domain.UpdateOrganizationMember{ExpectedVersion: ov.Version, DisplayName: ov.DisplayName, Role: "admin", Active: &yes, Reason: "Attempt owner modification"})
	code(t, err, "forbidden")
	_, err = s.RevokeMemberCredentials(ctx, owner, owner.ID, domain.NewID("key"), domain.RevokeMemberCredentials{ExpectedVersion: ov.Version, Reason: "Self revoke"})
	code(t, err, "invalid_transition")
	_, err = s.Member(ctx, owner, other.ID)
	code(t, err, "not_found")
	_, err = s.UpdateMember(ctx, owner, member.PrincipalID, domain.NewID("key"), domain.UpdateOrganizationMember{ExpectedVersion: member.Version, DisplayName: member.DisplayName, Role: "member", Reason: "Missing required active"})
	code(t, err, "invalid_request")
	_, err = s.UpdateMember(ctx, owner, member.PrincipalID, domain.NewID("key"), domain.UpdateOrganizationMember{ExpectedVersion: member.Version, DisplayName: member.DisplayName, Role: "member", Active: &yes, Reason: "No change"})
	code(t, err, "invalid_transition")
}
func TestMembershipAuthorReciprocalWritersAndLastOwner(t *testing.T) {
	for _, role := range []string{"admin", "owner"} {
		t.Run(role, func(t *testing.T) {
			s, _ := testStore(t)
			owner, _ := identity(t, s, 100000)
			a := memberAuthorCreate(t, s, owner, "First administrator", role)
			b := memberAuthorCreate(t, s, owner, "Second administrator", role)
			ap := memberAuthorCredential(t, s, owner.OrganizationID, a.PrincipalID)
			bp := memberAuthorCredential(t, s, owner.OrganizationID, b.PrincipalID)
			if role == "owner" {
				if _, err := s.Pool.Exec(context.Background(), "UPDATE memberships SET role='admin' WHERE organization_id=$1 AND principal_id=$2", owner.OrganizationID, owner.ID); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			start := make(chan struct{})
			done := make(chan error, 2)
			active := true
			for _, x := range []struct {
				p domain.Principal
				v domain.OrganizationMember
			}{{ap, b}, {bp, a}} {
				x := x
				go func() {
					<-start
					_, err := s.UpdateMember(ctx, x.p, x.v.PrincipalID, domain.NewID("key"), domain.UpdateOrganizationMember{ExpectedVersion: x.v.Version, DisplayName: x.v.DisplayName, Role: "viewer", Active: &active, Reason: "Concurrent reciprocal authority change"})
					done <- err
				}()
			}
			close(start)
			successes := 0
			for i := 0; i < 2; i++ {
				select {
				case err := <-done:
					if err == nil {
						successes++
					} else {
						var e *domain.Error
						if !errors.As(err, &e) || (e.Code != "forbidden" && e.Code != "unauthenticated" && e.Code != "invalid_transition") {
							t.Fatal("unexpected concurrent admin result", err)
						}
					}
				case <-ctx.Done():
					t.Fatal("IAM writers deadlocked")
				}
			}
			if successes != 1 {
				t.Fatal("concurrent authority changes did not serialize", successes)
			}
			if role == "owner" {
				var count int
				if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM memberships WHERE organization_id=$1 AND active AND role='owner'", owner.OrganizationID).Scan(&count); err != nil || count != 1 {
					t.Fatal("last owner lost", err)
				}
			}
		})
	}
}
func TestMembershipAuthorCachedReplyRechecksCurrentActor(t *testing.T) {
	s, _ := testStore(t)
	owner, _ := identity(t, s, 100000)
	v := memberAuthorCreate(t, s, owner, "Admin", "admin")
	p := memberAuthorCredential(t, s, owner.OrganizationID, v.PrincipalID)
	key := domain.NewID("key")
	in := domain.CreateOrganizationMember{DisplayName: "New member", Role: "member", Reason: "Cache reauthorization"}
	if _, err := s.CreateMember(context.Background(), p, key, in); err != nil {
		t.Fatal(err)
	}
	v = memberAuthorUpdate(t, s, owner, v, "member", true)
	_, err := s.CreateMember(context.Background(), p, key, in)
	code(t, err, "forbidden")
}

func memberAuthorRoleCycle(t *testing.T, s *Store, target domain.Principal) {
	t.Helper()
	v := memberAuthorCreate(t, s, target, "Separate managing owner", "owner")
	manager := memberAuthorCredential(t, s, target.OrganizationID, v.PrincipalID)
	targetMember, err := s.Member(context.Background(), manager, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	targetMember = memberAuthorUpdate(t, s, manager, targetMember, "viewer", true)
	_ = memberAuthorUpdate(t, s, manager, targetMember, "owner", true)
}
func TestMembershipAuthorModelConsentCannotReviveAfterRoleCycle(t *testing.T) {
	for _, phase := range []string{"queued", "claimed", "reserved"} {
		t.Run(phase, func(t *testing.T) {
			f := independentNativeSetup(t, nil)
			ctx := context.Background()
			var lease *domain.Lease
			var request domain.ReserveCostRequest
			var reservation domain.Reservation
			if phase != "queued" {
				var err error
				lease, _, err = f.s.ClaimModel(ctx, "membership-author-model", 30*time.Second)
				if err != nil || lease == nil {
					t.Fatal("initial model claim", err)
				}
				runtime, err := modelruntime.New(f.s, RuntimeModelConfig(f.model, "independent-native-model-key"))
				if err != nil {
					t.Fatal(err)
				}
				key, intent, err := runtime.DescribeIntent(*lease)
				if err != nil {
					t.Fatal(err)
				}
				request = domain.ReserveCostRequest{ChargeKey: key, Category: "model", Provider: f.model.ProviderAccount, PriceVersion: f.model.PriceVersion, AmountMicroUSD: f.model.MaxChargeMicroUSD, IntentHash: intent}
				if phase == "reserved" {
					reservation, err = f.s.ReserveCost(ctx, lease, request)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			memberAuthorRoleCycle(t, f.s, f.p)
			switch phase {
			case "queued":
				next, _, err := f.s.ClaimModel(ctx, "after-role-cycle", 30*time.Second)
				if err != nil || next != nil {
					t.Fatal("queued old consent revived", err)
				}
				task, err := f.s.GetTask(ctx, f.p, f.task.ID)
				if err != nil || task.Status != "blocked" || task.WaitingReason != "authorization_revoked" {
					t.Fatal("invalid consent not made visible", err)
				}
				raw, err := f.s.RetryTask(ctx, f.p, task.ID, domain.NewID("key"), domain.RetryTaskRequest{ExpectedVersion: task.Version, AcknowledgeNewCost: true, Reason: "Fresh explicit consent after restored role"})
				if err != nil {
					t.Fatal("fresh root retry unavailable", err)
				}
				var retry domain.Task
				if err = json.Unmarshal(raw, &retry); err != nil || retry.CurrentRunID == task.CurrentRunID {
					t.Fatal("retry reused invalid old consent", err)
				}
				next, _, err = f.s.ClaimModel(ctx, "fresh-consent", 30*time.Second)
				if err != nil || next == nil || next.RunID != retry.CurrentRunID {
					t.Fatal("fresh retry did not obtain fresh operation authority", err)
				}
			case "claimed":
				_, err := f.s.ReserveCost(ctx, lease, request)
				code(t, err, "forbidden")
				code(t, f.s.CheckLease(ctx, lease), "stale_lease")
			case "reserved":
				_, err := f.s.PrepareModelDispatch(ctx, lease, reservation.ID)
				code(t, err, "forbidden")
				if err = f.s.PauseModelAfterRuntimeError(ctx, lease, "model_attempt_requires_attention"); err != nil {
					t.Fatal(err)
				}
				if did, err := f.s.ReconcileModelUnsent(ctx); err != nil || !did {
					t.Fatal("proved-unsent revoked attempt leaked hold", err)
				}
			}
			var marked int
			if err := f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM actions WHERE organization_id=$1 AND dispatch_issued_at IS NOT NULL", f.p.OrganizationID).Scan(&marked); err != nil || marked != 0 || f.modelCalls.Load() != 0 {
				t.Fatal("role cycle dispatched old work", err)
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 0, 0)
		})
	}
}
func TestMembershipAuthorEpochRevokesWaitingGrantButCredentialCutoffDoesNot(t *testing.T) {
	for _, kind := range []string{"role_cycle", "credential_cutoff"} {
		t.Run(kind, func(t *testing.T) {
			f := independentAutomationSetup(t, func(f *independentAutomationFixture) { f.toolCharge = 17 })
			ctx := context.Background()
			f.executeModel(t)
			state := f.grant(t, func(in *domain.CreateTaskAutomation) { in.Mode = "prepare_and_continue" })
			f.reconcile(t)
			jobs := f.executions(t)
			if len(jobs) != 1 || jobs[0].Status != "awaiting_approval" {
				t.Fatal("initial prepared grant job absent")
			}
			if kind == "role_cycle" {
				memberAuthorRoleCycle(t, f.s, f.p)
				f.reconcile(t)
				state, err := f.s.Automation(ctx, f.p, f.task.ID)
				if err != nil || state.Grant == nil || state.Grant.Status != "stopped" {
					t.Fatal("old epoch grant revived after role restoration", err)
				}
				if did, err := f.s.ReconcileToolUnsent(ctx); err != nil || !did {
					t.Fatal("stale grant unsent reserve not cleaned", err)
				}
				independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 300, 0)
				if state.Usage.ToolExecutions != 1 || state.Usage.AllocatedMicroUSD != 17 || f.readCalls.Load() != 0 {
					t.Fatal("epoch cleanup reset usage or sent tool")
				}
			} else {
				managerMember := memberAuthorCreate(t, f.s, f.p, "Credential managing owner", "owner")
				manager := memberAuthorCredential(t, f.s, f.p.OrganizationID, managerMember.PrincipalID)
				target, err := f.s.Member(ctx, manager, f.p.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.s.RevokeMemberCredentials(ctx, manager, f.p.ID, domain.NewID("key"), domain.RevokeMemberCredentials{ExpectedVersion: target.Version, Reason: "Credential revocation does not revoke durable grant"}); err != nil {
					t.Fatal(err)
				}
				if did, err := f.s.ReconcileTaskAutomation(ctx); err != nil || did {
					t.Fatal("credential-only cutoff revoked durable consent", err)
				}
				tx, err := f.s.Pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				task, err := f.s.GetTask(ctx, manager, f.task.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = currentAutomationAuthorityTx(ctx, tx, *state.Grant, task); err != nil {
					_ = tx.Rollback(ctx)
					t.Fatal("durable delegation borrowed an expired credential", err)
				}
				_ = tx.Rollback(ctx)
				independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 317, 0)
			}
		})
	}
}

func TestMembershipAuthorStaleManualApprovalCleansBeforeFreshContinuation(t *testing.T) {
	f := independentNativeSetup(t, nil)
	ctx := context.Background()
	if _, err := f.executeModel(t, f.s); err != nil {
		t.Fatal(err)
	}
	proposal := f.proposal(t)
	view := f.decide(t, f.prepare(t, proposal), "approve")
	runID := view.Task.CurrentRunID
	memberAuthorRoleCycle(t, f.s, f.p)
	_, err := f.s.DispatchToolExecution(ctx, f.p, f.task.ID, view.Execution.ID, domain.NewID("key"), independentToolDispatch(view))
	code(t, err, "forbidden")
	if did, err := f.s.ReconcileToolUnsent(ctx); err != nil || !did {
		t.Fatal("stale unbound approval retained its unsent reservation", err)
	}
	view, err = f.s.GetToolExecution(ctx, f.p, f.task.ID, view.Execution.ID)
	if err != nil || view.Execution.Status != "cancelled" || view.Execution.Reason != "authority_epoch_changed" || view.Receipt != nil || view.Task.CurrentRunID != runID || f.toolCalls.Load() != 0 {
		t.Fatal("cleanup fabricated effect or lost continuation source", err)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 300, 0)
	raw, err := f.s.ContinueModel(ctx, f.p, f.task.ID, domain.NewID("key"), independentNativeContinue(proposal, view))
	if err != nil {
		t.Fatal("fresh explicit continuation unavailable", err)
	}
	var next domain.Task
	if err = json.Unmarshal(raw, &next); err != nil || next.CurrentRunID != runID || next.Status != "queued" {
		t.Fatal("explicit continuation changed historical Run", err)
	}
	lease, _, err := f.s.ClaimModel(ctx, "fresh-explicit-continuation", 30*time.Second)
	if err != nil || lease == nil || lease.RunID != runID {
		t.Fatal("fresh operation reused stale Run authority", err)
	}
	var actor string
	var epoch, current int64
	if err = f.s.Pool.QueryRow(ctx, "SELECT o.authority_principal_id,o.principal_authority_epoch,m.security_epoch FROM operations o JOIN memberships m ON m.organization_id=o.organization_id AND m.principal_id=o.authority_principal_id WHERE o.organization_id=$1 AND o.id=$2", f.p.OrganizationID, lease.OperationID).Scan(&actor, &epoch, &current); err != nil || actor != f.p.ID || epoch != current {
		t.Fatal("new operation did not pin fresh explicit actor", err)
	}
}
func TestMembershipAuthorTrustedTokenIssuerAndExactCleanup(t *testing.T) {
	s, _ := testStore(t)
	owner, _ := identity(t, s, 100000)
	v := memberAuthorCreate(t, s, owner, "Provisioned viewer", "viewer")
	ctx := context.Background()
	token, expires, err := s.IssueMemberToken(ctx, owner.OrganizationID, v.PrincipalID, time.Hour)
	if err != nil || token == "" || !expires.After(time.Now()) {
		t.Fatal("trusted issuance failed", err)
	}
	p, err := s.Authenticate(ctx, token, owner.OrganizationID)
	if err != nil || p.ID != v.PrincipalID || p.Role != "viewer" {
		t.Fatal("new member token not usable", err)
	}
	var generation, cutoff int64
	if err = s.Pool.QueryRow(ctx, "SELECT t.credential_generation,m.credential_cutoff FROM access_tokens t JOIN memberships m ON m.principal_id=t.principal_id WHERE t.token_hash=$1 AND m.organization_id=$2", Hash([]byte(token)), owner.OrganizationID).Scan(&generation, &cutoff); err != nil || generation <= cutoff {
		t.Fatal("token origin not database-issued", err)
	}
	_, err = s.RevokeMemberCredentials(ctx, owner, v.PrincipalID, domain.NewID("key"), domain.RevokeMemberCredentials{ExpectedVersion: v.Version, Reason: "Cut off previously issued member token"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Authenticate(ctx, token, owner.OrganizationID)
	code(t, err, "unauthenticated")
	replacement, _, err := s.IssueMemberToken(ctx, owner.OrganizationID, v.PrincipalID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authenticate(ctx, replacement, owner.OrganizationID); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeIssuedToken(ctx, Hash([]byte(replacement))); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeIssuedToken(ctx, Hash([]byte(replacement))); err != nil {
		t.Fatal("exact token cleanup not idempotent", err)
	}
	_, err = s.Authenticate(ctx, replacement, owner.OrganizationID)
	code(t, err, "unauthenticated")
	if _, err = s.Member(ctx, owner, v.PrincipalID); err != nil {
		t.Fatal("exact token cleanup revoked unrelated owner credentials", err)
	}
	if _, _, err = s.IssueMemberToken(ctx, owner.OrganizationID, v.PrincipalID, 25*time.Hour); err == nil {
		t.Fatal("unbounded member token accepted")
	}
}

func TestMembershipAuthorAncestorIssuerRevokesUnsentChildWithDifferentActors(t *testing.T) {
	base := independentToolSetup(t, false)
	ctx := context.Background()
	if _, err := base.s.CancelTask(ctx, base.p, base.task.ID, domain.NewID("key")); err != nil {
		t.Fatal(err)
	}
	leaf := domain.Agent{ID: base.task.AgentID, Version: base.task.AgentVersion}
	definition := teamAgent(t, base.s, base.p, "parallel", "all", 1, teamMember("member", leaf))
	root := task(t, base.s, base.p, definition)
	executeChild := func() domain.Task {
		teamTick(t, base.s)
		l, _, err := base.s.ClaimModel(ctx, "membership-ancestor-child", 30*time.Second)
		if err != nil || l == nil || l.Task.RootID != root.ID {
			t.Fatal("managed child claim", err)
		}
		if _, err = base.runtime(t, base.s).Execute(ctx, l); err != nil {
			t.Fatal(err)
		}
		return l.Task
	}
	executeChild()
	teamTick(t, base.s)
	var err error
	root, err = base.s.GetTask(ctx, base.p, root.ID)
	if err != nil || root.CandidateID == "" {
		t.Fatal("first team candidate missing", err)
	}
	candidates, _, err := base.s.Candidates(ctx, base.p, root.ID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var candidate domain.TaskCandidate
	for _, v := range candidates {
		if v.ID == root.CandidateID {
			candidate = v
		}
	}
	if candidate.ID == "" {
		t.Fatal("root candidate not listed")
	}
	_, err = base.s.ReviewTask(ctx, base.p, root.ID, domain.NewID("key"), domain.ReviewTaskRequest{ExpectedVersion: root.Version, CandidateID: candidate.ID, ArtifactID: candidate.ArtifactID, ArtifactVersion: candidate.ArtifactVersion, ContentDigest: candidate.ContentDigest, Decision: "request_changes", Reason: "Fresh root initiation by a different administrator"})
	if err != nil {
		t.Fatal(err)
	}
	root, err = base.s.GetTask(ctx, base.p, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	adminMember := memberAuthorCreate(t, base.s, base.p, "Different root initiator", "admin")
	admin := memberAuthorCredential(t, base.s, base.p.OrganizationID, adminMember.PrincipalID)
	raw, err := base.s.RetryTask(ctx, admin, root.ID, domain.NewID("key"), domain.RetryTaskRequest{ExpectedVersion: root.Version, AcknowledgeNewCost: true, Reason: "Explicit new administrator initiation", Feedback: "Review the next bounded candidate"})
	if err != nil || json.Unmarshal(raw, &root) != nil {
		t.Fatal("different-admin root retry", err)
	}
	childTask := executeChild()
	child := &independentToolFixture{independentCollaborationFixture: &independentCollaborationFixture{independentModelFixture: &independentModelFixture{s: base.s, p: base.p, task: childTask}}, tool: base.tool}
	child.refresh(t)
	view := child.decide(t, child.prepare(t), base.p, "approve")
	var unchangedOwner, unchangedCreator int64
	if err = base.s.Pool.QueryRow(ctx, "SELECT owner_authority_epoch,creator_authority_epoch FROM tool_executions WHERE organization_id=$1 AND id=$2", base.p.OrganizationID, view.Execution.ID).Scan(&unchangedOwner, &unchangedCreator); err != nil {
		t.Fatal(err)
	}
	adminMember = memberAuthorUpdate(t, base.s, base.p, adminMember, "viewer", true)
	_ = memberAuthorUpdate(t, base.s, base.p, adminMember, "admin", true)
	if did, err := base.s.ReconcileToolUnsent(ctx); err != nil || !did {
		t.Fatal("ancestor-only invalid consent kept child unsent hold", err)
	}
	after, err := base.s.GetToolExecution(ctx, base.p, childTask.ID, view.Execution.ID)
	if err != nil || after.Execution.Status != "cancelled" || after.Execution.Reason != "authority_epoch_changed" || after.Receipt != nil {
		t.Fatal("ancestor cleanup failed proof-unsent cancellation", err)
	}
	var released string
	var ownerEpoch, creatorEpoch int64
	if err = base.s.Pool.QueryRow(ctx, "SELECT r.status,j.owner_authority_epoch,j.creator_authority_epoch FROM reservations r JOIN tool_executions j ON j.organization_id=r.organization_id AND j.reservation_id=r.id WHERE j.organization_id=$1 AND j.id=$2", base.p.OrganizationID, view.Execution.ID).Scan(&released, &ownerEpoch, &creatorEpoch); err != nil || released != "released" || ownerEpoch != unchangedOwner || creatorEpoch != unchangedCreator {
		t.Fatal("cleanup changed child actors or failed hold release", err)
	}
}
