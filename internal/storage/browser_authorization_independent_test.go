// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	oidcidentity "agentplatform.local/platform/internal/identity"
)

func TestIndependentBrowserAuthorizationDoesNotInvertIdentityLockOrder(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, _ := identity(t, s, 1000)
	session := independentSession(t, s, p, "lock-order-subject", time.Hour)
	browser, e := s.AuthenticateBrowserSession(ctx, session.TokenHash, p.OrganizationID)
	if e != nil {
		t.Fatal(e)
	}
	// This reproduces the first lock of the actual LinkIdentity implementation:
	// principal FOR UPDATE, followed by identity mapping FOR UPDATE.
	linker, e := s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer linker.Rollback(ctx)
	if _, e = linker.Exec(ctx, "SELECT active FROM principals WHERE id=$1 FOR UPDATE", p.ID); e != nil {
		t.Fatal(e)
	}
	mutation := make(chan error, 1)
	go func() {
		_, e := s.CreateAgent(ctx, browser, domain.NewID("key"), domain.CreateAgent{Name: "lock-order", Instructions: "independent fixture"})
		mutation <- e
	}()
	waitForBlockedQuery(t, s, "SELECT p.id,m.organization_id,m.role,p.kind FROM browser_sessions")
	probe, stop := context.WithTimeout(ctx, 250*time.Millisecond)
	_, lockError := linker.Exec(probe, "SELECT principal_id FROM identity_mappings WHERE issuer=$1 AND subject=$2 FOR UPDATE", session.Issuer, session.Subject)
	stop()
	// End the fixture transaction before reporting a failure, so the other real
	// storage operation cannot remain stranded during schema cleanup.
	if lockError != nil {
		_ = linker.Rollback(ctx)
	} else if e = linker.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-mutation:
		if e != nil {
			t.Fatalf("mutation failed after fixture lock release: %v", e)
		}
	case <-ctx.Done():
		t.Fatal("mutation remained blocked")
	}
	if lockError != nil {
		t.Fatalf("browser authorization held mapping while waiting for principal: identity-link lock order is inverted (%v)", lockError)
	}
}
func TestIndependentBrowserAuthorizationRechecksRevokedAndChangedRoles(t *testing.T) {
	for _, kind := range []string{"role", "session", "mapping", "membership"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := testStore(t)
			ctx := context.Background()
			p, _ := identity(t, s, 1000)
			x := independentSession(t, s, p, "current-authority", time.Hour)
			browser, e := s.AuthenticateBrowserSession(ctx, x.TokenHash, p.OrganizationID)
			if e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "role":
				_, e = s.Pool.Exec(ctx, "UPDATE memberships SET role='viewer' WHERE principal_id=$1 AND organization_id=$2", p.ID, p.OrganizationID)
			case "session":
				e = s.RevokeSession(ctx, x.TokenHash)
			case "mapping":
				_, e = s.Pool.Exec(ctx, "UPDATE identity_mappings SET active=false WHERE issuer=$1 AND subject=$2", x.Issuer, x.Subject)
			case "membership":
				_, e = s.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE principal_id=$1 AND organization_id=$2", p.ID, p.OrganizationID)
			}
			if e != nil {
				t.Fatal(e)
			}
			_, e = s.CreateAgent(ctx, browser, domain.NewID("key"), domain.CreateAgent{Name: "stale authority", Instructions: "fixture"})
			want := "unauthenticated"
			if kind == "role" {
				want = "forbidden"
			}
			code(t, e, want)
			var count int
			if e = s.Pool.QueryRow(ctx, "SELECT count(*) FROM agents WHERE organization_id=$1", p.OrganizationID).Scan(&count); e != nil || count != 0 {
				t.Fatal("stale browser authority created data")
			}
		})
	}
}
func TestIndependentBrowserRevocationSerializesBehindAdmittedMutation(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, _ := identity(t, s, 1000)
	x := independentSession(t, s, p, "revocation-order", time.Hour)
	browser, e := s.AuthenticateBrowserSession(ctx, x.TokenHash, p.OrganizationID)
	if e != nil {
		t.Fatal(e)
	}
	blocker, e := s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback(ctx)
	if _, e = blocker.Exec(ctx, "SELECT id FROM organizations WHERE id=$1 FOR UPDATE", p.OrganizationID); e != nil {
		t.Fatal(e)
	}
	mutate := make(chan error, 1)
	go func() {
		_, e := s.CreateAgent(ctx, browser, domain.NewID("key"), domain.CreateAgent{Name: "already admitted", Instructions: "fixture"})
		mutate <- e
	}()
	waitForBlockedQuery(t, s, "SELECT active FROM organizations WHERE id=")
	revoke := make(chan error, 1)
	go func() { revoke <- s.RevokeSession(ctx, x.TokenHash) }()
	waitForBlockedQuery(t, s, "UPDATE browser_sessions SET revoked=true WHERE token_hash=")
	if e = blocker.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-mutate; e != nil {
		t.Fatal(e)
	}
	if e = <-revoke; e != nil {
		t.Fatal(e)
	}
	_, e = s.GetSession(ctx, x.TokenHash)
	if !errors.Is(e, oidcidentity.ErrSessionInvalid) {
		t.Fatal("session not revoked after serialization")
	}
	_, e = s.CreateAgent(ctx, browser, domain.NewID("key"), domain.CreateAgent{Name: "after revoke", Instructions: "fixture"})
	code(t, e, "unauthenticated")
}

func TestIndependentBrowserAuthorizationExpiryAfterAuthorityLockWait(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, _ := identity(t, s, 1000)
	session := independentSession(t, s, p, "expiry-authority-lock", 400*time.Millisecond)
	browser, e := s.AuthenticateBrowserSession(ctx, session.TokenHash, p.OrganizationID)
	if e != nil {
		t.Fatal(e)
	}
	blocker, e := s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback(ctx)
	if _, e = blocker.Exec(ctx, "SELECT id FROM principals WHERE id=$1 FOR UPDATE", p.ID); e != nil {
		t.Fatal(e)
	}
	result := make(chan error, 1)
	go func() {
		_, e := s.CreateAgent(ctx, browser, domain.NewID("key"), domain.CreateAgent{Name: "expired while waiting", Instructions: "fixture"})
		result <- e
	}()
	waitForBlockedQuery(t, s, "SELECT p.id,m.organization_id,m.role,p.kind FROM browser_sessions")
	// No row changes: lock waiting alone must not turn an expired session into
	// a fresh admission, even though PostgreSQL does not perform an update EPQ.
	timer := time.NewTimer(time.Until(session.ExpiresAt) + 25*time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if e = blocker.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	e = <-result
	code(t, e, "unauthenticated")
	var count int
	if e = s.Pool.QueryRow(ctx, "SELECT count(*) FROM agents WHERE organization_id=$1", p.OrganizationID).Scan(&count); e != nil || count != 0 {
		t.Fatal("expired browser admission wrote data")
	}
}
