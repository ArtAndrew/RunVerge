// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
)

type independentWebhookStore struct {
	s       *Store
	p       domain.Principal
	a       domain.Agent
	task    domain.Task
	profile domain.WebhookDestinationProfile
}

func independentWebhookDecode[T any](t *testing.T, raw []byte, err error) T {
	t.Helper()
	var value T
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func independentWebhookSetup(t *testing.T) independentWebhookStore {
	t.Helper()
	s, _ := testStore(t)
	p, _ := identity(t, s, 1_000_000)
	a := agent(t, s, p)
	f := independentWebhookStore{s: s, p: p, a: a, task: task(t, s, p, a)}
	f.profile = domain.WebhookDestinationProfile{Destination: domain.WebhookDestination{ID: "independent_receiver", OrganizationID: p.OrganizationID, Version: 1, Name: "Approved metadata destination", SigningKeyID: "independent_key_v1", UnmeteredApproved: true, Active: true}, Endpoint: "https://independent-receiver.invalid/private-path", SigningKeyEnvironment: "PLATFORM_WEBHOOK_KEY_INDEPENDENT", TimeoutSeconds: 2, AllowPrivateNetwork: true}
	if _, err := s.InstallWebhookDestination(context.Background(), f.profile); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f independentWebhookStore) input() domain.CreateWebhookSubscription {
	return domain.CreateWebhookSubscription{DestinationID: f.profile.Destination.ID, DestinationVersion: 1, EventTypes: []string{"task.created", "material.created"}, TTLSeconds: 600, MaxNotifications: 8, AcknowledgeMetadataExport: true, Reason: "Independent explicit metadata export consent"}
}

func (f independentWebhookStore) subscribe(t *testing.T, p domain.Principal, taskID string, in domain.CreateWebhookSubscription) domain.WebhookSubscription {
	t.Helper()
	raw, err := f.s.CreateWebhookSubscription(context.Background(), p, taskID, domain.NewID("key"), in)
	return independentWebhookDecode[domain.WebhookSubscription](t, raw, err)
}

func (f independentWebhookStore) member(t *testing.T, role string) (domain.OrganizationMember, domain.Principal) {
	t.Helper()
	raw, err := f.s.CreateMember(context.Background(), f.p, domain.NewID("key"), domain.CreateOrganizationMember{DisplayName: "Independent reviewer fixture", Role: role, Reason: "Isolated test identity"})
	m := independentWebhookDecode[domain.OrganizationMember](t, raw, err)
	return m, independentWebhookCredential(t, f.s, f.p.OrganizationID, m.PrincipalID)
}

func independentWebhookCredential(t *testing.T, s *Store, org, id string) domain.Principal {
	t.Helper()
	token, _, err := s.IssueMemberToken(context.Background(), org, id, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Authenticate(context.Background(), token, org)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func independentWebhookMemberUpdate(t *testing.T, s *Store, actor domain.Principal, m domain.OrganizationMember, role string, active bool) domain.OrganizationMember {
	t.Helper()
	raw, err := s.UpdateMember(context.Background(), actor, m.PrincipalID, domain.NewID("key"), domain.UpdateOrganizationMember{ExpectedVersion: m.Version, DisplayName: m.DisplayName, Role: role, Active: &active, Reason: "Independent authority cycle"})
	return independentWebhookDecode[domain.OrganizationMember](t, raw, err)
}

func (f independentWebhookStore) material(t *testing.T) {
	t.Helper()
	_, err := f.s.CreateMaterial(context.Background(), f.p, f.task.ID, domain.NewID("key"), domain.CreateMaterial{Name: "Private source", MediaType: "text/plain", Content: "INDEPENDENT_WEBHOOK_PRIVATE_CONTENT"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIndependentWebhookStorageCanonicalIdentityAndTenant(t *testing.T) {
	f := independentWebhookSetup(t)
	ctx := context.Background()
	m, cachedViewer := f.member(t, "viewer")
	m = independentWebhookMemberUpdate(t, f.s, f.p, m, "admin", true)
	key := domain.NewID("key")
	in := f.input()
	// Cached viewer metadata must not reject a credential whose current role is admin.
	raw, err := f.s.CreateWebhookSubscription(ctx, cachedViewer, f.task.ID, key, in)
	sub := independentWebhookDecode[domain.WebhookSubscription](t, raw, err)
	m = independentWebhookMemberUpdate(t, f.s, f.p, m, "viewer", true)
	forged := cachedViewer
	forged.Role = "owner"
	forged.Kind = domain.PrincipalHuman
	forged.ID = f.p.ID
	_, err = f.s.CreateWebhookSubscription(ctx, forged, f.task.ID, key, in)
	code(t, err, "forbidden")
	_, err = f.s.WebhookSubscription(ctx, forged, f.task.ID, sub.ID)
	code(t, err, "forbidden")
	_, _, err = f.s.WebhookDestinations(ctx, forged, "", 10)
	code(t, err, "forbidden")
	_, err = f.s.RevokeWebhookSubscription(ctx, forged, f.task.ID, sub.ID, domain.NewID("key"), domain.RevokeWebhookSubscription{ExpectedVersion: sub.Version, Reason: "Forged cached authority"})
	code(t, err, "forbidden")
	other, _ := identity(t, f.s, 100000)
	_, err = f.s.CreateWebhookSubscription(ctx, other, f.task.ID, domain.NewID("key"), in)
	code(t, err, "not_found")
	_, err = f.s.WebhookSubscription(ctx, other, f.task.ID, sub.ID)
	code(t, err, "not_found")
	_, _, err = f.s.WebhookSubscriptions(ctx, other, f.task.ID, "", 10)
	code(t, err, "not_found")
	_, err = f.s.RevokeWebhookSubscription(ctx, other, f.task.ID, sub.ID, domain.NewID("key"), domain.RevokeWebhookSubscription{ExpectedVersion: sub.Version, Reason: "Wrong organization"})
	code(t, err, "not_found")
	selected := other
	selected.OrganizationID = f.p.OrganizationID
	_, _, err = f.s.WebhookDestinations(ctx, selected, "", 10)
	code(t, err, "unauthenticated")
	raw, err = f.s.CreateServiceAccount(ctx, f.p, domain.NewID("key"), domain.CreateServiceAccount{DisplayName: "Independent integration", Role: "member", Reason: "Valid attenuated fixture"})
	account := independentWebhookDecode[domain.ServiceAccount](t, raw, err)
	for _, ceiling := range []string{"viewer", "member"} {
		token, _, err := f.s.IssueServiceToken(ctx, f.p.OrganizationID, account.PrincipalID, ceiling, time.Minute, "Independent valid key")
		if err != nil {
			t.Fatal(err)
		}
		service, err := f.s.Authenticate(ctx, token, f.p.OrganizationID)
		if err != nil {
			t.Fatal(err)
		}
		service.Kind = domain.PrincipalHuman
		service.Role = "owner"
		_, err = f.s.CreateWebhookSubscription(ctx, service, f.task.ID, domain.NewID("key"), in)
		code(t, err, "forbidden")
		_, _, err = f.s.WebhookDestinations(ctx, service, "", 10)
		code(t, err, "forbidden")
	}
	var count int
	if err = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook_subscriptions WHERE organization_id=$1", f.p.OrganizationID).Scan(&count); err != nil || count != 1 {
		t.Fatal("denials created durable export consent", count, err)
	}
}

func TestIndependentWebhookStorageDistinctOwnerCreatorEpochsAndPolicy(t *testing.T) {
	f := independentWebhookSetup(t)
	ctx := context.Background()
	creatorMember, creator := f.member(t, "admin")
	_, secondOwner := f.member(t, "owner")
	sub := f.subscribe(t, creator, f.task.ID, f.input())
	owner, err := f.s.Member(ctx, secondOwner, f.p.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner = independentWebhookMemberUpdate(t, f.s, secondOwner, owner, "admin", true)
	owner = independentWebhookMemberUpdate(t, f.s, secondOwner, owner, "owner", true)
	view, err := f.s.WebhookSubscription(ctx, secondOwner, f.task.ID, sub.ID)
	if err != nil || view.Status != "invalidated" {
		t.Fatal("source owner restore revived old consent", view.Status, err)
	}
	fresh := f.subscribe(t, creator, f.task.ID, f.input())
	creatorMember = independentWebhookMemberUpdate(t, f.s, secondOwner, creatorMember, "member", true)
	creatorMember = independentWebhookMemberUpdate(t, f.s, secondOwner, creatorMember, "admin", true)
	view, err = f.s.WebhookSubscription(ctx, secondOwner, f.task.ID, fresh.ID)
	if err != nil || view.Status != "invalidated" {
		t.Fatal("creator restore revived old consent", view.Status, err)
	}
	// Login credential revocation does not revoke newly captured durable consent.
	creator = independentWebhookCredential(t, f.s, f.p.OrganizationID, creatorMember.PrincipalID)
	durable := f.subscribe(t, creator, f.task.ID, f.input())
	_, err = f.s.RevokeMemberCredentials(ctx, secondOwner, creatorMember.PrincipalID, domain.NewID("key"), domain.RevokeMemberCredentials{ExpectedVersion: creatorMember.Version, Reason: "Revoke login only"})
	if err != nil {
		t.Fatal(err)
	}
	view, err = f.s.WebhookSubscription(ctx, secondOwner, f.task.ID, durable.ID)
	if err != nil || view.Status != "active" {
		t.Fatal("login revocation destroyed durable consent", view.Status, err)
	}
	// Fixture state transition exercises the existing SQL epoch guard; this is
	// not a claim that browser/SSO configuration was accepted through its API.
	if _, err = f.s.Pool.Exec(ctx, "UPDATE organization_login_policies SET mode='oidc_required',oidc_issuer='https://independent-idp.invalid/' WHERE organization_id=$1", f.p.OrganizationID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.RecoverLoginPolicy(ctx, f.p.OrganizationID, "Independent policy epoch invalidation"); err != nil {
		t.Fatal(err)
	}
	secondOwner = independentWebhookCredential(t, f.s, f.p.OrganizationID, secondOwner.ID)
	view, err = f.s.WebhookSubscription(ctx, secondOwner, f.task.ID, durable.ID)
	if err != nil || view.Status != "invalidated" {
		t.Fatal("policy epoch did not invalidate old consent", view.Status, err)
	}
}

func TestIndependentWebhookStorageOrganizationQuotaAndExpiry(t *testing.T) {
	f := independentWebhookSetup(t)
	ctx := context.Background()
	var first domain.WebhookSubscription
	for taskIndex := 0; taskIndex < 4; taskIndex++ {
		taskID := f.task.ID
		if taskIndex > 0 {
			taskID = task(t, f.s, f.p, f.a).ID
		}
		for i := 0; i < 8; i++ {
			sub := f.subscribe(t, f.p, taskID, f.input())
			if taskIndex == 0 && i == 0 {
				first = sub
			}
		}
	}
	fifth := task(t, f.s, f.p, f.a)
	_, err := f.s.CreateWebhookSubscription(ctx, f.p, fifth.ID, domain.NewID("key"), f.input())
	code(t, err, "invalid_transition")
	_, err = f.s.RevokeWebhookSubscription(ctx, f.p, f.task.ID, first.ID, domain.NewID("key"), domain.RevokeWebhookSubscription{ExpectedVersion: first.Version, Reason: "Free one accepting slot"})
	if err != nil {
		t.Fatal(err)
	}
	short := f.input()
	short.TTLSeconds = 1
	soon := f.subscribe(t, f.p, fifth.ID, short)
	if _, err = f.s.Pool.Exec(ctx, "SELECT pg_sleep(1.05)"); err != nil {
		t.Fatal(err)
	}
	view, err := f.s.WebhookSubscription(ctx, f.p, fifth.ID, soon.ID)
	if err != nil || view.Status != "expired" {
		t.Fatal("expiry not exposed", view.Status, err)
	}
	f.subscribe(t, f.p, fifth.ID, f.input())
	var accepting int
	if err = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook_subscriptions WHERE organization_id=$1 AND status='active' AND expires_at>clock_timestamp()", f.p.OrganizationID).Scan(&accepting); err != nil || accepting != 32 {
		t.Fatal("bounded quota not maintained", accepting, err)
	}
}

func TestIndependentWebhookStorageDisabledOwnerCreatorCannotRevive(t *testing.T) {
	f := independentWebhookSetup(t)
	ctx := context.Background()
	creatorMember, creator := f.member(t, "admin")
	_, secondOwner := f.member(t, "owner")
	sourceBound := f.subscribe(t, creator, f.task.ID, f.input())
	owner, err := f.s.Member(ctx, secondOwner, f.p.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner = independentWebhookMemberUpdate(t, f.s, secondOwner, owner, "owner", false)
	view, err := f.s.WebhookSubscription(ctx, secondOwner, f.task.ID, sourceBound.ID)
	if err != nil || view.Status != "invalidated" {
		t.Fatal("disabled source owner retained consent", view.Status, err)
	}
	owner = independentWebhookMemberUpdate(t, f.s, secondOwner, owner, "owner", true)
	view, err = f.s.WebhookSubscription(ctx, secondOwner, f.task.ID, sourceBound.ID)
	if err != nil || view.Status != "invalidated" {
		t.Fatal("owner re-enable revived old consent", view.Status, err)
	}
	fresh := f.subscribe(t, creator, f.task.ID, f.input())
	creatorMember = independentWebhookMemberUpdate(t, f.s, secondOwner, creatorMember, "admin", false)
	view, err = f.s.WebhookSubscription(ctx, secondOwner, f.task.ID, fresh.ID)
	if err != nil || view.Status != "invalidated" {
		t.Fatal("disabled creator retained consent", view.Status, err)
	}
	creatorMember = independentWebhookMemberUpdate(t, f.s, secondOwner, creatorMember, "admin", true)
	view, err = f.s.WebhookSubscription(ctx, secondOwner, f.task.ID, fresh.ID)
	if err != nil || view.Status != "invalidated" {
		t.Fatal("creator re-enable revived old consent", view.Status, err)
	}
	_, err = f.s.CreateWebhookSubscription(ctx, creator, f.task.ID, domain.NewID("key"), f.input())
	code(t, err, "unauthenticated")
	creator = independentWebhookCredential(t, f.s, f.p.OrganizationID, creatorMember.PrincipalID)
	newConsent := f.subscribe(t, creator, f.task.ID, f.input())
	if newConsent.Status != "active" {
		t.Fatal("fresh explicit consent did not capture new epochs")
	}
}

func TestIndependentWebhookStorageOrganizationBeforeTaskLock(t *testing.T) {
	f := independentWebhookSetup(t)
	ctx := context.Background()
	block, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer block.Rollback(ctx)
	var blocker int
	if err = block.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	if _, err = block.Exec(ctx, "SELECT id FROM organizations WHERE id=$1 FOR UPDATE", f.p.OrganizationID); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, e := f.s.CreateWebhookSubscription(bounded, f.p, f.task.ID, domain.NewID("key"), f.input())
		done <- e
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var waiting bool
		err = f.s.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) AND query LIKE 'SELECT active FROM organizations WHERE id=%')", blocker).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no actual waiter at own organization gate")
		}
		time.Sleep(5 * time.Millisecond)
	}
	quick, stop := context.WithTimeout(ctx, time.Second)
	_, err = block.Exec(quick, "SELECT id FROM tasks WHERE organization_id=$1 AND id=$2 FOR UPDATE", f.p.OrganizationID, f.task.ID)
	stop()
	if err != nil {
		t.Fatal("waiting subscription took task lock ahead of organization", err)
	}
	if err = block.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal("ordered subscription did not complete", err)
	}
}

func TestIndependentWebhookStorageSafePaginationAndProfileRetirement(t *testing.T) {
	f := independentWebhookSetup(t)
	ctx := context.Background()
	sub := f.subscribe(t, f.p, f.task.ID, f.input())
	for version := 2; version <= 4; version++ {
		p := f.profile
		p.Destination.Version = version
		p.Destination.SigningKeyID = fmt.Sprintf("independent_key_v%d", version)
		if _, err := f.s.InstallWebhookDestination(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	retired, err := f.s.InstallWebhookDestination(ctx, f.profile)
	if err != nil || retired.Active {
		t.Fatal("same version reactivated retired profile", err)
	}
	altered := f.profile
	altered.Endpoint = "https://other.invalid/"
	if _, err = f.s.InstallWebhookDestination(ctx, altered); err == nil {
		t.Fatal("immutable version retarget accepted")
	}
	_, err = f.s.CreateWebhookSubscription(ctx, f.p, f.task.ID, domain.NewID("key"), f.input())
	code(t, err, "forbidden")
	view, err := f.s.WebhookSubscription(ctx, f.p, f.task.ID, sub.ID)
	if err != nil || view.Status != "invalidated" {
		t.Fatal("retired destination consent remained eligible", view.Status, err)
	}
	seen := map[int]bool{}
	cursor := ""
	for page := 0; page < 5; page++ {
		values, next, err := f.s.WebhookDestinations(ctx, f.p, cursor, 1)
		if err != nil || len(values) != 1 {
			t.Fatal("bounded destination page failed", err)
		}
		v := values[0]
		if seen[v.Version] || v.OrganizationID != f.p.OrganizationID {
			t.Fatal("pagination repeated or crossed tenant")
		}
		seen[v.Version] = true
		raw, _ := json.Marshal(v)
		var keys map[string]any
		if json.Unmarshal(raw, &keys) != nil || len(keys) != 8 {
			t.Fatal("public descriptor fields diverged")
		}
		for _, forbidden := range []string{f.profile.Endpoint, f.profile.SigningKeyEnvironment, "trusted_ca_pem", "allow_private_network", "profile_digest"} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatal("private profile reached public descriptor")
			}
		}
		if next == "" {
			break
		}
		if next == cursor {
			t.Fatal("nonadvancing cursor")
		}
		cursor = next
	}
	if len(seen) != 4 {
		t.Fatal("pagination omitted a profile version", len(seen))
	}
	other, _ := identity(t, f.s, 100000)
	values, next, err := f.s.WebhookDestinations(ctx, other, cursor, 10)
	if err != nil || len(values) != 0 || next != "" {
		t.Fatal("foreign cursor disclosed destination", err)
	}
}

func TestIndependentWebhookStorageRevokePreservesSentFacts(t *testing.T) {
	f := independentWebhookSetup(t)
	ctx := context.Background()
	sub := f.subscribe(t, f.p, f.task.ID, f.input())
	for i := 0; i < 3; i++ {
		f.material(t)
	}
	rows, err := f.s.Pool.Query(ctx, "SELECT id FROM webhook_attempts WHERE organization_id=$1 ORDER BY created_at,id", f.p.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 3 {
		t.Fatal("missing fanout")
	}
	for i, id := range ids[1:] {
		if _, err = f.s.Pool.Exec(ctx, "UPDATE webhook_attempts SET status='leased',worker_id='independent_worker',lease_until=clock_timestamp()+interval '1 minute',fencing_token=1 WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, id); err != nil {
			t.Fatal(err)
		}
		if _, err = f.s.Pool.Exec(ctx, "UPDATE webhook_attempts SET status='sent',sent_at=clock_timestamp(),signature_timestamp=floor(extract(epoch FROM clock_timestamp()))::bigint WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, id); err != nil {
			t.Fatal(err)
		}
		state := "sent"
		if i == 1 {
			state = "unknown"
			if _, err = f.s.Pool.Exec(ctx, "UPDATE webhook_attempts SET status='unknown',unknown_at=clock_timestamp(),reason_code='transport_unknown' WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, id); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = f.s.Pool.Exec(ctx, "UPDATE webhook_deliveries SET status=$3,version=version+1 WHERE organization_id=$1 AND current_attempt_id=$2", f.p.OrganizationID, id, state); err != nil {
			t.Fatal(err)
		}
	}
	current, err := f.s.WebhookSubscription(ctx, f.p, f.task.ID, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.RevokeWebhookSubscription(ctx, f.p, f.task.ID, sub.ID, domain.NewID("key"), domain.RevokeWebhookSubscription{ExpectedVersion: current.Version, Reason: "Independent stop future export"})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"stopped", "sent", "unknown"} {
		var state string
		if err = f.s.Pool.QueryRow(ctx, "SELECT status FROM webhook_attempts WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, ids[i]).Scan(&state); err != nil || state != want {
			t.Fatal("revoke misclassified physical fact", i, state, err)
		}
	}
	if _, err = f.s.Pool.Exec(ctx, "UPDATE webhook_attempts SET status='acknowledged',observed_at=clock_timestamp(),http_status=204,reason_code='http_acknowledged' WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, ids[2]); err != nil {
		t.Fatal("late exact fact rejected after revoke", err)
	}
	var history int
	if err = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook_attempts WHERE organization_id=$1 AND unknown_at IS NOT NULL", f.p.OrganizationID).Scan(&history); err != nil || history != 1 {
		t.Fatal("unknown history lost", err)
	}
	f.material(t)
	var n int
	if err = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook_deliveries WHERE organization_id=$1 AND subscription_id=$2", f.p.OrganizationID, sub.ID).Scan(&n); err != nil || n != 3 {
		t.Fatal("revoked export admitted new notification", n, err)
	}
	var reserved, spent int64
	if err = f.s.Pool.QueryRow(ctx, "SELECT reserved_microusd,spent_microusd FROM tasks WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.task.ID).Scan(&reserved, &spent); err != nil || reserved != 0 || spent != 0 {
		t.Fatal("notification changed money", err)
	}
}

func TestIndependentWebhookStorageRejectsInvalidPolicyBeforePersistence(t *testing.T) {
	f := independentWebhookSetup(t)
	ctx := context.Background()
	for name, change := range map[string]func(*domain.CreateWebhookSubscription){
		"zero_ttl": func(i *domain.CreateWebhookSubscription) { i.TTLSeconds = 0 }, "ttl_overflow": func(i *domain.CreateWebhookSubscription) { i.TTLSeconds = 1 << 62 },
		"zero_count": func(i *domain.CreateWebhookSubscription) { i.MaxNotifications = 0 }, "too_many": func(i *domain.CreateWebhookSubscription) { i.MaxNotifications = 1025 },
		"no_export_consent": func(i *domain.CreateWebhookSubscription) { i.AcknowledgeMetadataExport = false }, "unknown_type": func(i *domain.CreateWebhookSubscription) { i.EventTypes = []string{"not.registered"} },
		"duplicate_type": func(i *domain.CreateWebhookSubscription) { i.EventTypes = []string{"task.created", "task.created"} }, "utf8_reason": func(i *domain.CreateWebhookSubscription) { i.Reason = string([]byte{0xff}) },
		"nul_reason": func(i *domain.CreateWebhookSubscription) { i.Reason = "reason\x00secret" }, "wide_reason": func(i *domain.CreateWebhookSubscription) { i.Reason = strings.Repeat("ж", 2049) },
	} {
		t.Run(name, func(t *testing.T) {
			in := f.input()
			change(&in)
			_, err := f.s.CreateWebhookSubscription(ctx, f.p, f.task.ID, domain.NewID("key"), in)
			code(t, err, "invalid_request")
		})
	}
	var n int
	if err := f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook_subscriptions WHERE organization_id=$1", f.p.OrganizationID).Scan(&n); err != nil || n != 0 {
		t.Fatal("invalid policy persisted", n, err)
	}
	kinds := []string{}
	for kind := range domain.PublicEventPayloadRegistry().Types {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	boundary := f.input()
	boundary.EventTypes = kinds[:17]
	_, err := f.s.CreateWebhookSubscription(ctx, f.p, f.task.ID, domain.NewID("key"), boundary)
	code(t, err, "invalid_request")
	boundary.EventTypes = kinds[:16]
	boundary.TTLSeconds = 86400
	boundary.MaxNotifications = 1024
	boundary.Reason = strings.Repeat("ж", 2048)
	accepted := f.subscribe(t, f.p, f.task.ID, boundary)
	if len(accepted.EventTypes) != 16 || accepted.MaxNotifications != 1024 || accepted.ExpiresAt.Sub(accepted.CreatedAt) != 24*time.Hour {
		t.Fatal("exact documented bounds were rejected or altered")
	}
}
