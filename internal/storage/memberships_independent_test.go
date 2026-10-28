// SPDX-License-Identifier: Elastic-2.0
package storage

// Independent lifecycle regressions use real isolated PostgreSQL schemas and
// public Store commands. Direct SQL is limited to trusted fixture credentials,
// cross-organization setup, observed lock barriers and private-state assertions.
import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
	"agentplatform.local/platform/internal/toolruntime"
)

func independentMemberDecode(t *testing.T, raw json.RawMessage, err error) domain.OrganizationMember {
	t.Helper()
	var v domain.OrganizationMember
	if err != nil || json.Unmarshal(raw, &v) != nil || v.PrincipalID == "" {
		t.Fatal("membership command failed", err)
	}
	return v
}
func independentMemberCreate(t *testing.T, s *Store, p domain.Principal, name, role string) domain.OrganizationMember {
	t.Helper()
	raw, err := s.CreateMember(context.Background(), p, domain.NewID("key"), domain.CreateOrganizationMember{DisplayName: name, Role: role, Reason: "Independent explicit provisioning"})
	return independentMemberDecode(t, raw, err)
}
func independentMemberCredential(t *testing.T, s *Store, org, id string) (domain.Principal, string) {
	t.Helper()
	raw := domain.NewID("independent_private_credential")
	if _, err := s.Pool.Exec(context.Background(), "INSERT INTO access_tokens(token_hash,principal_id) VALUES($1,$2)", Hash([]byte(raw)), id); err != nil {
		t.Fatal(err)
	}
	p, err := s.Authenticate(context.Background(), raw, org)
	if err != nil {
		t.Fatal(err)
	}
	return p, raw
}
func independentMemberUpdate(t *testing.T, s *Store, actor domain.Principal, v domain.OrganizationMember, role string, active bool) domain.OrganizationMember {
	t.Helper()
	raw, err := s.UpdateMember(context.Background(), actor, v.PrincipalID, domain.NewID("key"), domain.UpdateOrganizationMember{ExpectedVersion: v.Version, DisplayName: v.DisplayName, Role: role, Active: &active, Reason: "Independent lifecycle authority transition"})
	return independentMemberDecode(t, raw, err)
}
func independentMemberCurrent(t *testing.T, s *Store, actor domain.Principal, id string) domain.OrganizationMember {
	t.Helper()
	v, err := s.Member(context.Background(), actor, id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func independentMemberCycle(t *testing.T, s *Store, controller domain.Principal, id, intermediate, restored string) {
	t.Helper()
	v := independentMemberCurrent(t, s, controller, id)
	v = independentMemberUpdate(t, s, controller, v, intermediate, true)
	_ = independentMemberUpdate(t, s, controller, v, restored, true)
}
func independentMemberPrivate(t *testing.T, s *Store, org, id string) (epoch, cutoff int64) {
	t.Helper()
	if err := s.Pool.QueryRow(context.Background(), "SELECT security_epoch,credential_cutoff FROM memberships WHERE organization_id=$1 AND principal_id=$2", org, id).Scan(&epoch, &cutoff); err != nil {
		t.Fatal(err)
	}
	return
}
func independentMemberAuthority(t *testing.T, s *Store, p domain.Principal, wantRole string, denied bool) {
	t.Helper()
	tx, err := s.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := authorizeTx(context.Background(), tx, p, false)
	_ = tx.Rollback(context.Background())
	if denied {
		code(t, err, "unauthenticated")
	} else if err != nil || fresh.Role != wantRole {
		t.Fatal("current authority did not match organization membership", fresh.Role, err)
	}
}
func independentMembershipWaitBlocked(t *testing.T, s *Store, blockingPID int32, prefix string, expected int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var count int
		if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)) AND query LIKE $2", blockingPID, prefix+"%").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count >= expected {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("exact lifecycle lock barrier was not reached")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestIndependentMembershipRotationAcrossScopedCutoffCannotMintAuthority(t *testing.T) {
	s, _ := testStore(t)
	owner, _ := identity(t, s, 100000)
	other, _ := identity(t, s, 100000)
	v := independentMemberCreate(t, s, owner, "Local A label", "member")
	actor, bearer := independentMemberCredential(t, s, owner.OrganizationID, v.PrincipalID)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := s.Pool.Exec(ctx, "INSERT INTO memberships(organization_id,principal_id,role,display_name) VALUES($1,$2,'viewer','Local B label')", other.OrganizationID, actor.ID); err != nil {
		t.Fatal(err)
	}
	old := independentSession(t, s, actor, "independent-scoped-rotation", time.Hour)
	replacement := old
	replacement.ID, replacement.TokenHash, replacement.CreatedAt = domain.NewID("ses"), Hash([]byte(domain.NewID("rotation"))), time.Now().UTC().Truncate(time.Microsecond)
	blocker, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(ctx, "SELECT id FROM browser_sessions WHERE token_hash=$1 FOR UPDATE", old.TokenHash); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.RotateSession(ctx, old.TokenHash, replacement) }()
	independentMembershipWaitBlocked(t, s, int32(blocker.Conn().PgConn().PID()), "SELECT "+sessionColumns+currentSessionFrom, 1)
	raw, err := s.RevokeMemberCredentials(ctx, owner, actor.ID, domain.NewID("key"), domain.RevokeMemberCredentials{ExpectedVersion: v.Version, Reason: "Revoke while original session rotation is blocked"})
	v = independentMemberDecode(t, raw, err)
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal("global rotation should still serve the other organization", err)
	}
	var beforeOrigin, afterOrigin, bearerOrigin int64
	var globalActive, mappingActive, tokenRevoked bool
	err = s.Pool.QueryRow(ctx, `SELECT (SELECT credential_generation FROM browser_sessions WHERE token_hash=$1),(SELECT credential_generation FROM browser_sessions WHERE token_hash=$2),(SELECT credential_generation FROM access_tokens WHERE token_hash=$3),(SELECT active FROM principals WHERE id=$4),(SELECT active FROM identity_mappings WHERE issuer=$5 AND subject=$6),(SELECT revoked FROM access_tokens WHERE token_hash=$3)`, old.TokenHash, replacement.TokenHash, Hash([]byte(bearer)), actor.ID, old.Issuer, old.Subject).Scan(&beforeOrigin, &afterOrigin, &bearerOrigin, &globalActive, &mappingActive, &tokenRevoked)
	_, cutoff := independentMemberPrivate(t, s, owner.OrganizationID, actor.ID)
	if err != nil || beforeOrigin != afterOrigin || afterOrigin > cutoff || bearerOrigin > cutoff || !globalActive || !mappingActive || tokenRevoked {
		t.Fatal("rotation minted an origin or org admin changed global authority", err)
	}
	_, err = s.Authenticate(ctx, bearer, owner.OrganizationID)
	code(t, err, "unauthenticated")
	_, err = s.AuthenticateBrowserSession(ctx, replacement.TokenHash, owner.OrganizationID)
	code(t, err, "unauthenticated")
	if _, err = s.Authenticate(ctx, bearer, other.OrganizationID); err != nil {
		t.Fatal("scoped cutoff revoked unrelated bearer membership", err)
	}
	if _, err = s.AuthenticateBrowserSession(ctx, replacement.TokenHash, other.OrganizationID); err != nil {
		t.Fatal("scoped cutoff revoked unrelated browser membership", err)
	}
	orgs, _, err := s.BrowserOrganizations(ctx, replacement.TokenHash, "", 100)
	if err != nil || len(orgs) != 1 || orgs[0].ID != other.OrganizationID {
		t.Fatal("organization discovery ignored scoped cutoff", orgs, err)
	}
	fresh := independentSession(t, s, actor, old.Subject, time.Hour)
	if _, err = s.AuthenticateBrowserSession(ctx, fresh.TokenHash, owner.OrganizationID); err != nil {
		t.Fatal("fresh login did not regain current permitted membership", err)
	}
}

func TestIndependentMembershipActiveCycleKeepsEveryOldCredentialRevoked(t *testing.T) {
	s, _ := testStore(t)
	owner, _ := identity(t, s, 100000)
	other, _ := identity(t, s, 100000)
	v := independentMemberCreate(t, s, owner, "Account being re-enabled", "admin")
	actor, bearer := independentMemberCredential(t, s, owner.OrganizationID, v.PrincipalID)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, "INSERT INTO memberships(organization_id,principal_id,role) VALUES($1,$2,'member')", other.OrganizationID, actor.ID); err != nil {
		t.Fatal(err)
	}
	old := independentSession(t, s, actor, "independent-reenable", time.Hour)
	epoch, cutoff := independentMemberPrivate(t, s, owner.OrganizationID, actor.ID)
	v = independentMemberUpdate(t, s, owner, v, "member", true)
	roleEpoch, roleCutoff := independentMemberPrivate(t, s, owner.OrganizationID, actor.ID)
	if roleEpoch != epoch+1 || roleCutoff != cutoff {
		t.Fatal("role-only edit did not invalidate authority independently of credentials")
	}
	independentMemberAuthority(t, s, actor, "member", false)
	v = independentMemberUpdate(t, s, owner, v, "member", false)
	// A genuine new global login while disabled may still serve B; re-enable
	// must advance the cutoff again, so that intermediate login cannot revive A.
	during := independentSession(t, s, actor, old.Subject, time.Hour)
	v = independentMemberUpdate(t, s, owner, v, "member", true)
	for _, token := range []string{old.TokenHash, during.TokenHash} {
		_, err := s.AuthenticateBrowserSession(ctx, token, owner.OrganizationID)
		code(t, err, "unauthenticated")
		if _, err = s.AuthenticateBrowserSession(ctx, token, other.OrganizationID); err != nil {
			t.Fatal("A deprovision changed B's global session", err)
		}
	}
	_, err := s.Authenticate(ctx, bearer, owner.OrganizationID)
	code(t, err, "unauthenticated")
	fresh, _ := independentMemberCredential(t, s, owner.OrganizationID, actor.ID)
	independentMemberAuthority(t, s, fresh, "member", false)
	afterEpoch, afterCutoff := independentMemberPrivate(t, s, owner.OrganizationID, actor.ID)
	if afterEpoch != roleEpoch+2 || afterCutoff <= roleCutoff || !v.Active {
		t.Fatal("active cycle rewound the authority generation")
	}
}

func TestIndependentMembershipCASAndAuditRemainAtomicUnderContention(t *testing.T) {
	s, _ := testStore(t)
	owner, _ := identity(t, s, 100000)
	v := independentMemberCreate(t, s, owner, "CAS target", "member")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	const count = 8
	start := make(chan struct{})
	done := make(chan error, count)
	yes := true
	for i := 0; i < count; i++ {
		name := domain.NewID("changed_label")
		go func() {
			<-start
			_, err := s.UpdateMember(ctx, owner, v.PrincipalID, domain.NewID("key"), domain.UpdateOrganizationMember{ExpectedVersion: v.Version, DisplayName: name, Role: v.Role, Active: &yes, Reason: "Concurrent stale-version writer"})
			done <- err
		}()
	}
	close(start)
	success := 0
	for i := 0; i < count; i++ {
		err := <-done
		if err == nil {
			success++
		} else {
			code(t, err, "conflict")
		}
	}
	current := independentMemberCurrent(t, s, owner, v.PrincipalID)
	epoch, cutoff := independentMemberPrivate(t, s, owner.OrganizationID, v.PrincipalID)
	entries, _, err := s.MembershipAudit(ctx, owner, v.PrincipalID, "", 100)
	if err != nil || success != 1 || current.Version != v.Version+1 || len(entries) != 2 || epoch != 1 || cutoff != 0 {
		t.Fatal("CAS contention duplicated audit or changed name-only authority", success, len(entries), err)
	}
	if entries[0].Before == nil || entries[0].Before.Version != v.Version || entries[0].After.Version != current.Version || entries[0].After.DisplayName != current.DisplayName {
		t.Fatal("audit did not describe exactly the committed membership change")
	}
	var globalName string
	if err = s.Pool.QueryRow(ctx, "SELECT name FROM principals WHERE id=$1", v.PrincipalID).Scan(&globalName); err != nil || globalName == current.DisplayName {
		t.Fatal("org label renamed the global identity", err)
	}
}

func TestIndependentMembershipGateRechecksExpiryBeforeProvisioning(t *testing.T) {
	s, _ := testStore(t)
	owner, _ := identity(t, s, 100000)
	v := independentMemberCreate(t, s, owner, "Expiring admin", "admin")
	actor, _ := independentMemberCredential(t, s, owner.OrganizationID, v.PrincipalID)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := s.Pool.Exec(ctx, "UPDATE access_tokens SET expires_at=clock_timestamp()+interval '600 milliseconds' WHERE token_hash=$1", actor.TokenHash); err != nil {
		t.Fatal(err)
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
	go func() {
		_, e := s.CreateMember(ctx, actor, domain.NewID("key"), domain.CreateOrganizationMember{DisplayName: "Must not exist", Role: "member", Reason: "Credential expires behind the IAM gate"})
		done <- e
	}()
	independentMembershipWaitBlocked(t, s, int32(blocker.Conn().PgConn().PID()), "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", 1)
	for {
		var expired bool
		if err = s.Pool.QueryRow(ctx, "SELECT expires_at<=clock_timestamp() FROM access_tokens WHERE token_hash=$1", actor.TokenHash).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	code(t, <-done, "unauthenticated")
	var principals, audit, keys int
	if err = s.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM principals),(SELECT count(*) FROM organization_membership_audit),(SELECT count(*) FROM idempotency_keys WHERE principal_id=$1)", actor.ID).Scan(&principals, &audit, &keys); err != nil || principals != 2 || audit != 1 || keys != 0 {
		t.Fatal("expired gated command persisted partial provisioning", principals, audit, keys, err)
	}
}

func TestIndependentMembershipConcurrentOwnersAndAdminsHaveOneGateWinner(t *testing.T) {
	for _, role := range []string{"owner", "admin"} {
		t.Run(role, func(t *testing.T) {
			s, _ := testStore(t)
			original, _ := identity(t, s, 100000)
			a := independentMemberCreate(t, s, original, "First competing administrator", role)
			b := independentMemberCreate(t, s, original, "Second competing administrator", role)
			ap, _ := independentMemberCredential(t, s, original.OrganizationID, a.PrincipalID)
			bp, _ := independentMemberCredential(t, s, original.OrganizationID, b.PrincipalID)
			if role == "owner" {
				old := independentMemberCurrent(t, s, ap, original.ID)
				_ = independentMemberUpdate(t, s, ap, old, "member", true)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			blocker, err := s.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err = blocker.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "organization-membership-administration\x1f"+original.OrganizationID); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 2)
			for _, x := range []struct {
				actor  domain.Principal
				target domain.OrganizationMember
			}{{ap, b}, {bp, a}} {
				x := x
				go func() {
					active := true
					_, e := s.UpdateMember(ctx, x.actor, x.target.PrincipalID, domain.NewID("key"), domain.UpdateOrganizationMember{ExpectedVersion: x.target.Version, DisplayName: x.target.DisplayName, Role: "viewer", Active: &active, Reason: "Reciprocal change queued behind exact IAM gate"})
					done <- e
				}()
			}
			independentMembershipWaitBlocked(t, s, int32(blocker.Conn().PgConn().PID()), "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", 2)
			if err = blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			succeeded := 0
			for i := 0; i < 2; i++ {
				select {
				case e := <-done:
					if e == nil {
						succeeded++
					} else {
						var policy *domain.Error
						if !errors.As(e, &policy) || policy.Code != "forbidden" {
							t.Fatal("reciprocal gate yielded a database error or stale authority", e)
						}
					}
				case <-ctx.Done():
					t.Fatal("reciprocal authority mutation deadlocked")
				}
			}
			var owners, modified int
			if err = s.Pool.QueryRow(ctx, "SELECT count(*) FILTER(WHERE active AND role='owner'),count(*) FILTER(WHERE principal_id=ANY($2::text[]) AND version=2) FROM memberships WHERE organization_id=$1", original.OrganizationID, []string{a.PrincipalID, b.PrincipalID}).Scan(&owners, &modified); err != nil || succeeded != 1 || owners != 1 || modified != 1 {
				t.Fatal("competing changes lost last owner or committed twice", succeeded, owners, modified, err)
			}
		})
	}
}

func TestIndependentMembershipCachedReplyRechecksTargetPolicyAndCAS(t *testing.T) {
	s, _ := testStore(t)
	owner, _ := identity(t, s, 100000)
	a := independentMemberCreate(t, s, owner, "Managing admin", "admin")
	admin, _ := independentMemberCredential(t, s, owner.OrganizationID, a.PrincipalID)
	v := independentMemberCreate(t, s, owner, "Target member", "member")
	ctx := context.Background()
	key := domain.NewID("key")
	yes := true
	in := domain.UpdateOrganizationMember{ExpectedVersion: v.Version, DisplayName: "Updated org label", Role: v.Role, Active: &yes, Reason: "Scoped replay policy"}
	first, err := s.UpdateMember(ctx, admin, v.PrincipalID, key, in)
	v = independentMemberDecode(t, first, err)
	again, err := s.UpdateMember(ctx, admin, v.PrincipalID, key, in)
	replayed := independentMemberDecode(t, again, err)
	firstSnapshot, _ := json.Marshal(v)
	replaySnapshot, _ := json.Marshal(replayed)
	if !bytes.Equal(firstSnapshot, replaySnapshot) {
		t.Fatal("same-key retry did not return its original committed snapshot", err)
	}
	changed := in
	changed.DisplayName = "Another payload"
	_, err = s.UpdateMember(ctx, admin, v.PrincipalID, key, changed)
	code(t, err, "conflict")
	_, err = s.UpdateMember(ctx, admin, v.PrincipalID, domain.NewID("key"), in)
	code(t, err, "conflict")
	v = independentMemberUpdate(t, s, owner, v, "owner", true)
	_, err = s.UpdateMember(ctx, admin, v.PrincipalID, key, in)
	code(t, err, "forbidden")
	entries, _, err := s.MembershipAudit(ctx, owner, v.PrincipalID, "", 100)
	if err != nil || len(entries) != 3 || v.Version != 3 {
		t.Fatal("replay policy failure changed state or appended audit", len(entries), err)
	}
}

func TestIndependentMembershipOldQueuedOperationCannotReviveAfterRoleCycle(t *testing.T) {
	f := independentNativeSetup(t, nil)
	controller := independentCollaborationActor(t, f.s, f.p.OrganizationID, "owner")
	var beforeOwner, beforeActor int64
	if err := f.s.Pool.QueryRow(context.Background(), "SELECT owner_authority_epoch,principal_authority_epoch FROM operations WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&beforeOwner, &beforeActor); err != nil {
		t.Fatal(err)
	}
	independentMemberCycle(t, f.s, controller, f.p.ID, "admin", "owner")
	l, _, err := f.s.ClaimModel(context.Background(), "independent-cycle-model", time.Second)
	if err != nil || l != nil || f.modelCalls.Load() != 0 || f.toolCalls.Load() != 0 {
		t.Fatal("restored role revived an old queued model authority", err)
	}
	var operationStatus, taskStatus string
	var afterOwner, afterActor int64
	if err = f.s.Pool.QueryRow(context.Background(), "SELECT o.status,t.status,o.owner_authority_epoch,o.principal_authority_epoch FROM operations o JOIN tasks t ON t.organization_id=o.organization_id AND t.id=o.task_id WHERE o.organization_id=$1 AND o.task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&operationStatus, &taskStatus, &afterOwner, &afterActor); err != nil || operationStatus != "blocked" || taskStatus != "blocked" || beforeOwner != afterOwner || beforeActor != afterActor {
		t.Fatal("old operation epochs were rewritten instead of stopping", operationStatus, taskStatus, err)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 0, 0)
}

func TestIndependentMembershipOldApprovalAndDispatcherCannotRevive(t *testing.T) {
	for _, phase := range []string{"approval", "dispatcher"} {
		t.Run(phase, func(t *testing.T) {
			f := independentNativeSetup(t, nil)
			admin := independentMemberCreate(t, f.s, f.p, "Independent delegated approver", "admin")
			actor, _ := independentMemberCredential(t, f.s, f.p.OrganizationID, admin.PrincipalID)
			if _, err := f.executeModel(t, f.s); err != nil {
				t.Fatal(err)
			}
			v := f.prepare(t, f.proposal(t))
			decider := actor
			if phase == "dispatcher" {
				decider = f.p
			}
			raw, err := f.s.DecideToolExecution(context.Background(), decider, f.task.ID, v.Execution.ID, domain.NewID("key"), domain.DecideToolExecution{ExpectedTaskVersion: v.Task.Version, ExpectedExecutionVersion: v.Execution.Version, ExpectedApprovalVersion: v.Approval.Version, IntentHash: v.Execution.IntentHash, Decision: "approve", Reason: "Explicit independent human approval"})
			v = independentToolView(t, raw, err)
			if phase == "dispatcher" {
				raw, err = f.s.DispatchToolExecution(context.Background(), actor, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDispatch(v))
				v = independentToolView(t, raw, err)
			}
			independentMemberCycle(t, f.s, f.p, actor.ID, "member", "admin")
			if phase == "approval" {
				_, err = f.s.DispatchToolExecution(context.Background(), f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDispatch(v))
				code(t, err, "forbidden")
			} else {
				l, _, err := f.s.ClaimToolExecution(context.Background(), "independent-cycle-tool", time.Second)
				if err != nil || l != nil {
					t.Fatal("restored dispatcher revived an old queued permit", err)
				}
			}
			if f.toolCalls.Load() != 0 || f.modelCalls.Load() != 1 {
				t.Fatal("stale approval or dispatcher reached a provider")
			}
		})
	}
}

func TestIndependentMembershipFreshContinuationCapturesNewOperationAuthority(t *testing.T) {
	for _, staleInitiator := range []bool{false, true} {
		name := "fresh_continue_on_historical_run"
		if staleInitiator {
			name = "separate_initiator_role_cycle"
		}
		t.Run(name, func(t *testing.T) {
			f := independentNativeSetup(t, nil)
			controller := independentCollaborationActor(t, f.s, f.p.OrganizationID, "owner")
			if _, err := f.executeModel(t, f.s); err != nil {
				t.Fatal(err)
			}
			proposal := f.proposal(t)
			view := f.dispatchAndObserve(t, f.prepare(t, proposal))
			actor := f.p
			if !staleInitiator {
				independentMemberCycle(t, f.s, controller, f.p.ID, "admin", "owner")
			} else {
				v := independentMemberCreate(t, f.s, f.p, "Distinct continuing principal", "admin")
				actor, _ = independentMemberCredential(t, f.s, f.p.OrganizationID, v.PrincipalID)
			}
			raw, err := f.s.ContinueModel(context.Background(), actor, f.task.ID, domain.NewID("key"), independentNativeContinue(proposal, view))
			if err != nil {
				t.Fatal("new explicit continuation rejected fresh authority", err)
			}
			var queued domain.Task
			if json.Unmarshal(raw, &queued) != nil || queued.CurrentRunID != view.Task.CurrentRunID {
				t.Fatal("fresh operation rewrote historical Run identity")
			}
			if staleInitiator {
				independentMemberCycle(t, f.s, f.p, actor.ID, "member", "admin")
			}
			l, c, err := f.s.ClaimModel(context.Background(), "independent-fresh-continue", 30*time.Second)
			if staleInitiator {
				if err != nil || l != nil || f.modelCalls.Load() != 1 {
					t.Fatal("restored separate initiator revived queued operation", err)
				}
				return
			}
			if err != nil || l == nil {
				t.Fatal("new continuation inherited revoked historical Run authority", err)
			}
			r, err := modelruntime.New(f.s, RuntimeModelConfig(c, "independent-native-model-key"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.Execute(context.Background(), l); err != nil || f.modelCalls.Load() != 2 || f.toolCalls.Load() != 1 {
				t.Fatal("fresh operation did not execute exactly once", err)
			}
			var runEpoch, operationEpoch int64
			if err = f.s.Pool.QueryRow(context.Background(), "SELECT r.owner_authority_epoch,o.owner_authority_epoch FROM runs r JOIN operations o ON o.organization_id=r.organization_id AND o.run_id=r.id WHERE r.organization_id=$1 AND r.id=$2 AND o.id=$3", f.p.OrganizationID, queued.CurrentRunID, l.OperationID).Scan(&runEpoch, &operationEpoch); err != nil || runEpoch != 1 || operationEpoch != 3 {
				t.Fatal("fresh continuation rewrote old authority or failed to bind new epoch", runEpoch, operationEpoch, err)
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 720, 0)
		})
	}
}

func TestIndependentMembershipRoleCycleStopsOldGrantButCredentialRevokeDoesNot(t *testing.T) {
	for _, revokeOnly := range []bool{false, true} {
		name := "security_epoch_invalidates_grant"
		if revokeOnly {
			name = "credential_cutoff_preserves_durable_grant"
		}
		t.Run(name, func(t *testing.T) {
			f := independentAutomationSetup(t, nil)
			controller := independentCollaborationActor(t, f.s, f.p.OrganizationID, "owner")
			f.executeModel(t)
			g := f.grant(t, nil)
			if revokeOnly {
				v := independentMemberCurrent(t, f.s, controller, f.p.ID)
				if _, err := f.s.RevokeMemberCredentials(context.Background(), controller, f.p.ID, domain.NewID("key"), domain.RevokeMemberCredentials{ExpectedVersion: v.Version, Reason: "Credential-only revoke must preserve explicit durable delegation"}); err != nil {
					t.Fatal(err)
				}
				independentMemberAuthority(t, f.s, f.p, "", true)
			} else {
				independentMemberCycle(t, f.s, controller, f.p.ID, "admin", "owner")
			}
			f.reconcile(t)
			state, err := f.s.Automation(context.Background(), controller, f.task.ID)
			if err != nil || state.Grant == nil || state.Grant.ID != g.Grant.ID {
				t.Fatal("grant identity changed during member lifecycle", err)
			}
			if !revokeOnly {
				if state.Grant.Status != "stopped" || state.Usage.ToolExecutions != 0 || state.Usage.AllocatedMicroUSD != 0 || f.readCalls.Load() != 0 {
					t.Fatal("old grant revived after role cycle", state)
				}
				independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 300, 0)
				return
			}
			l, c, err := f.s.ClaimToolExecution(context.Background(), "independent-delegation-after-logout", 30*time.Second)
			if err != nil || l == nil {
				t.Fatal("credential revocation incorrectly stopped durable read delegation", err)
			}
			roots := x509.NewCertPool()
			roots.AddCert(f.readPeer.Certificate())
			r, err := toolruntime.New(f.s, toolruntime.Config{Connection: c, Credential: "independent-auto-read-key", TrustedRoots: roots, PersistenceTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.Execute(context.Background(), l); err != nil {
				t.Fatal(err)
			}
			f.reconcile(t)
			f.executeModel(t)
			state, err = f.s.Automation(context.Background(), controller, f.task.ID)
			if err != nil || state.Usage.ModelTurns != 1 || state.Usage.ToolExecutions != 1 || state.Usage.AllocatedMicroUSD != 420 || f.modelCalls.Load() != 2 || f.readCalls.Load() != 1 {
				t.Fatal("durable delegation lost its capped lifetime allocation", state, err)
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 720, 0)
		})
	}
}

type independentMembershipLateToolFact struct {
	*Store
	observed chan struct{}
	release  chan struct{}
}

func (s *independentMembershipLateToolFact) RecordToolOutcome(ctx context.Context, in domain.ToolOutcomeEvidence) (domain.ToolOutcomeReceipt, error) {
	if in.Outcome == "succeeded" {
		close(s.observed)
		select {
		case <-s.release:
		case <-ctx.Done():
			return domain.ToolOutcomeReceipt{}, ctx.Err()
		}
	}
	return s.Store.RecordToolOutcome(ctx, in)
}
func TestIndependentMembershipDeprovisionPreservesLateActualToolFactAndHolds(t *testing.T) {
	f := independentNativeSetup(t, nil)
	controller := independentCollaborationActor(t, f.s, f.p.OrganizationID, "owner")
	_, lease, connection := independentWireToolLease(t, f)
	store := &independentMembershipLateToolFact{Store: f.s, observed: make(chan struct{}), release: make(chan struct{})}
	roots := x509.NewCertPool()
	roots.AddCert(f.toolPeer.Certificate())
	r, err := toolruntime.New(store, toolruntime.Config{Connection: connection, Credential: "independent-native-tool-key", TrustedRoots: roots, PersistenceTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := r.Execute(context.Background(), lease); done <- e }()
	select {
	case <-store.observed:
	case <-time.After(5 * time.Second):
		t.Fatal("actual registered TLS response did not reach factual persistence")
	}
	// The real runtime already parsed the exact synthetic provider response;
	// pause only its final Store call, then revoke authority through the API.
	v := independentMemberCurrent(t, f.s, controller, f.p.ID)
	_ = independentMemberUpdate(t, f.s, controller, v, v.Role, false)
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
	close(store.release)
	if err = <-done; err != nil {
		t.Fatal("lifecycle revocation erased a known late external fact", err)
	}
	view, err := f.s.GetToolExecution(context.Background(), controller, f.task.ID, lease.Execution.ID)
	if err != nil || view.Receipt == nil || view.Receipt.Method != "registered_tool_response" || view.Execution.Status != "succeeded" || f.toolCalls.Load() != 1 || f.modelCalls.Load() != 1 {
		t.Fatal("late fact missing or repeated despite exact registered response", view, err)
	}
	independentMemberAuthority(t, f.s, f.p, "", true)
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
	if next, _, err := f.s.ClaimToolExecution(context.Background(), "independent-after-late-fact", time.Second); err != nil || next != nil {
		t.Fatal("late fact restarted a dispatched occurrence", err)
	}
}
