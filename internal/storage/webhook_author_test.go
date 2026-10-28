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

type webhookAuthorFixture struct {
	s       *Store
	p       domain.Principal
	task    domain.Task
	profile domain.WebhookDestinationProfile
}

func webhookAuthorSetup(t *testing.T) webhookAuthorFixture {
	t.Helper()
	s, _ := testStore(t)
	p, _ := identity(t, s, 1_000_000)
	a := agent(t, s, p)
	f := webhookAuthorFixture{s: s, p: p, task: task(t, s, p, a)}
	f.profile = domain.WebhookDestinationProfile{Destination: domain.WebhookDestination{ID: "webhook_author", OrganizationID: p.OrganizationID, Version: 1, Name: "Metadata only receiver", SigningKeyID: "author_key_v1", UnmeteredApproved: true, Active: true}, Endpoint: "https://webhook-author.invalid/events", SigningKeyEnvironment: "PLATFORM_WEBHOOK_KEY_AUTHOR", TimeoutSeconds: 2}
	if _, err := s.InstallWebhookDestination(context.Background(), f.profile); err != nil {
		t.Fatal(err)
	}
	t.Log("SQL021 successfully installed in disposable schema; no external receiver or main-profile mutation")
	return f
}

func webhookAuthorInput(max int) domain.CreateWebhookSubscription {
	return domain.CreateWebhookSubscription{DestinationID: "webhook_author", DestinationVersion: 1, EventTypes: []string{"material.created", "task.created", "task.cancelled"}, TTLSeconds: 600, MaxNotifications: max, AcknowledgeMetadataExport: true, Reason: "Explicit bounded task metadata export"}
}

func (f webhookAuthorFixture) subscribe(t *testing.T, max int) domain.WebhookSubscription {
	t.Helper()
	raw, err := f.s.CreateWebhookSubscription(context.Background(), f.p, f.task.ID, domain.NewID("key"), webhookAuthorInput(max))
	return configurationAuthorDecode[domain.WebhookSubscription](t, raw, err)
}

func (f webhookAuthorFixture) material(t *testing.T) {
	t.Helper()
	_, err := f.s.CreateMaterial(context.Background(), f.p, f.task.ID, domain.NewID("key"), domain.CreateMaterial{Name: "Private material", MediaType: "text/plain", Content: "DO_NOT_EXPORT_WEBHOOK_MATERIAL_CONTENT"})
	if err != nil {
		t.Fatal(err)
	}
}

func (f webhookAuthorFixture) count(t *testing.T, table, sub string) int {
	t.Helper()
	if table != "webhook_deliveries" && table != "webhook_attempts" {
		t.Fatal("unsafe test table")
	}
	var n int
	q := "SELECT count(*) FROM " + table + " WHERE organization_id=$1"
	args := []any{f.p.OrganizationID}
	if sub != "" {
		q += " AND subscription_id=$2"
		args = append(args, sub)
	}
	if err := f.s.Pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWebhookAuthorAtomicFanoutAndLifetimeBound(t *testing.T) {
	f := webhookAuthorSetup(t)
	ctx := context.Background()
	in := webhookAuthorInput(2)
	key := domain.NewID("key")
	raw, err := f.s.CreateWebhookSubscription(ctx, f.p, f.task.ID, key, in)
	sub := configurationAuthorDecode[domain.WebhookSubscription](t, raw, err)
	if sub.StartAfterSequence <= 0 || sub.AdmittedNotifications != 0 || f.count(t, "webhook_deliveries", "") != 0 {
		t.Fatal("historical export or invalid initial cutoff")
	}
	replayed, err := f.s.CreateWebhookSubscription(ctx, f.p, f.task.ID, key, in)
	var replayedSub domain.WebhookSubscription
	if err != nil || json.Unmarshal(replayed, &replayedSub) != nil || !reflect.DeepEqual(sub, replayedSub) {
		t.Fatal("creation replay changed durable intent", err)
	}
	f.material(t)
	f.material(t)
	f.material(t)
	current, err := f.s.WebhookSubscription(ctx, f.p, f.task.ID, sub.ID)
	if err != nil || current.Status != "exhausted" || current.AdmittedNotifications != 2 || current.LastAdmittedSequence <= current.StartAfterSequence || current.Version != 3 {
		t.Fatalf("lifetime bound/cutoff %#v %v", current, err)
	}
	if f.count(t, "webhook_deliveries", sub.ID) != 2 || f.count(t, "webhook_attempts", "") != 2 {
		t.Fatal("fanout did not create one initial physical attempt per bounded logical event")
	}
	rows, err := f.s.Pool.Query(ctx, `SELECT d.envelope,d.envelope_digest,d.id,d.event_sequence,e.type,e.created_at,e.payload_version,o.sequence,a.id,a.number,d.current_attempt_id FROM webhook_deliveries d JOIN events e ON e.organization_id=d.organization_id AND e.sequence=d.event_sequence JOIN outbox o ON o.sequence=e.sequence JOIN webhook_attempts a ON a.organization_id=d.organization_id AND a.delivery_id=d.id WHERE d.organization_id=$1`, f.p.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var body, digest, id, kind, attempt, currentID string
		var seq, outseq int64
		var at time.Time
		var pv, number int
		if err = rows.Scan(&body, &digest, &id, &seq, &kind, &at, &pv, &outseq, &attempt, &number, &currentID); err != nil {
			t.Fatal(err)
		}
		var envelope domain.WebhookEnvelope
		if json.Unmarshal([]byte(body), &envelope) != nil || Hash([]byte(body)) != digest || envelope.DeliveryID != id || envelope.Event.Sequence != seq || seq != outseq || envelope.Event.Type != kind || !envelope.Event.CreatedAt.Equal(at) || envelope.Event.PayloadVersion != pv || envelope.Event.TaskID != f.task.ID || envelope.OrganizationID != f.p.OrganizationID || envelope.SubscriptionID != sub.ID || attempt != currentID || number != 1 {
			t.Fatal("event/outbox/envelope/attempt identity diverged")
		}
		if strings.Contains(body, "DO_NOT_EXPORT") || strings.Contains(body, "data") || strings.Contains(body, f.profile.Endpoint) || strings.Contains(body, "goal") {
			t.Fatal("private payload escaped metadata allowlist")
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	var storedReason string
	if err = f.s.Pool.QueryRow(ctx, "SELECT reason_digest FROM webhook_audit WHERE organization_id=$1 AND object_id=$2", f.p.OrganizationID, sub.ID).Scan(&storedReason); err != nil || storedReason != Hash([]byte(in.Reason)) {
		t.Fatal("intent audit missing", err)
	}
	// The exact same source/event/outbox/fanout transaction rolls back together.
	other := f.subscribe(t, 4)
	tx, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = emit(ctx, tx, f.p.OrganizationID, f.task.ID, "task.created", map[string]any{"run_id": f.task.CurrentRunID, "agent_version": f.task.AgentVersion}); err != nil {
		t.Fatal(err)
	}
	var inside int
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM webhook_deliveries WHERE organization_id=$1 AND subscription_id=$2", f.p.OrganizationID, other.ID).Scan(&inside); err != nil || inside != 1 {
		t.Fatal("fanout not atomic inside transaction", err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "webhook_deliveries", other.ID) != 0 {
		t.Fatal("rolled-back notification persisted")
	}
	view, err := f.s.WebhookSubscription(ctx, f.p, f.task.ID, other.ID)
	if err != nil || view.AdmittedNotifications != 0 || view.Version != 1 {
		t.Fatal("rolled-back quota was consumed", err)
	}
}

func TestWebhookAuthorConcurrentSubscriptionQuotaAndLockOrder(t *testing.T) {
	f := webhookAuthorSetup(t)
	ctx := context.Background()
	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.s.CreateWebhookSubscription(ctx, f.p, f.task.ID, domain.NewID("key"), webhookAuthorInput(4))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	accepted, rejected := 0, 0
	for err := range errs {
		if err == nil {
			accepted++
		} else {
			var e *domain.Error
			if !errors.As(err, &e) || e.Code != "invalid_transition" {
				t.Fatal(err)
			}
			rejected++
		}
	}
	if accepted != 8 || rejected != 4 {
		t.Fatalf("racing quota accepted=%d rejected=%d", accepted, rejected)
	}
	// Ledger-style organization→task locking must make progress while another
	// subscription is waiting for the organization. A task-before-org design
	// would deadlock here when the first transaction requests FOR UPDATE.
	blocked, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Rollback(ctx)
	if _, err = blocked.Exec(ctx, "SELECT id FROM organizations WHERE id=$1 FOR UPDATE", f.p.OrganizationID); err != nil {
		t.Fatal(err)
	}
	probe, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := f.s.CreateWebhookSubscription(probe, f.p, f.task.ID, domain.NewID("key"), webhookAuthorInput(4))
		done <- err
	}()
	// Wait for a real database waiter, not an elapsed sleep assumption.
	deadline := time.Now().Add(time.Second)
	for {
		var waiting bool
		err = f.s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock' AND query LIKE 'SELECT active FROM organizations WHERE id=%')`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("subscription never reached organization gate")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err = blocked.Exec(probe, "SELECT id FROM tasks WHERE organization_id=$1 AND id=$2 FOR UPDATE", f.p.OrganizationID, f.task.ID); err != nil {
		t.Fatal("subscription held a task lock while waiting for organization", err)
	}
	if err = blocked.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	code(t, <-done, "invalid_transition")
}

func TestWebhookAuthorTenantCurrentRoleEpochAndRetirement(t *testing.T) {
	f := webhookAuthorSetup(t)
	ctx := context.Background()
	sub := f.subscribe(t, 4)
	other, _ := identity(t, f.s, 1_000_000)
	_, err := f.s.CreateWebhookSubscription(ctx, other, f.task.ID, domain.NewID("key"), webhookAuthorInput(4))
	code(t, err, "not_found")
	_, err = f.s.WebhookSubscription(ctx, other, f.task.ID, sub.ID)
	code(t, err, "not_found")
	account := securityAuthorAccount(t, f.s, f.p, "member")
	token, _, err := f.s.IssueServiceToken(ctx, f.p.OrganizationID, account.PrincipalID, "member", time.Minute, "No implicit metadata export")
	if err != nil {
		t.Fatal(err)
	}
	service, err := f.s.Authenticate(ctx, token, f.p.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	service.Role = "owner" // A forged cached role is never current authority.
	_, err = f.s.CreateWebhookSubscription(ctx, service, f.task.ID, domain.NewID("key"), webhookAuthorInput(4))
	code(t, err, "forbidden")
	key := domain.NewID("key")
	in := webhookAuthorInput(4)
	_, err = f.s.CreateWebhookSubscription(ctx, f.p, f.task.ID, key, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Pool.Exec(ctx, "UPDATE memberships SET role='member' WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, f.p.ID); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.CreateWebhookSubscription(ctx, f.p, f.task.ID, key, in)
	code(t, err, "forbidden")
	if _, err = f.s.Pool.Exec(ctx, "UPDATE memberships SET role='owner' WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, f.p.ID); err != nil {
		t.Fatal(err)
	}
	view, err := f.s.WebhookSubscription(ctx, f.p, f.task.ID, sub.ID)
	if err != nil || view.Status != "invalidated" {
		t.Fatal("restoring role revived old metadata-export consent", err, view.Status)
	}
	f.profile.Destination.Version = 2
	if _, err = f.s.InstallWebhookDestination(ctx, f.profile); err != nil {
		t.Fatal(err)
	}
	f.profile.Destination.Version = 1
	old, err := f.s.InstallWebhookDestination(ctx, f.profile)
	if err != nil || old.Active {
		t.Fatal("reinstall reactivated retired receiver", err)
	}
	_, err = f.s.CreateWebhookSubscription(ctx, f.p, f.task.ID, domain.NewID("key"), webhookAuthorInput(4))
	code(t, err, "forbidden")
}

func TestWebhookAuthorRevokeStopsOnlyProvenUnsent(t *testing.T) {
	f := webhookAuthorSetup(t)
	ctx := context.Background()
	sub := f.subscribe(t, 8)
	f.material(t)
	f.material(t)
	var sentID, queuedID string
	rows, err := f.s.Pool.Query(ctx, "SELECT id FROM webhook_attempts WHERE organization_id=$1 ORDER BY created_at,id", f.p.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 2 {
		t.Fatal(ids)
	}
	sentID, queuedID = ids[0], ids[1]
	if _, err = f.s.Pool.Exec(ctx, `UPDATE webhook_attempts SET status='leased',worker_id='author_worker',lease_until=clock_timestamp()+interval '1 minute',fencing_token=1 WHERE organization_id=$1 AND id=$2`, f.p.OrganizationID, sentID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Pool.Exec(ctx, `UPDATE webhook_attempts SET status='sent',sent_at=clock_timestamp(),signature_timestamp=extract(epoch FROM clock_timestamp())::bigint WHERE organization_id=$1 AND id=$2`, f.p.OrganizationID, sentID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Pool.Exec(ctx, `UPDATE webhook_attempts SET status='unknown',unknown_at=clock_timestamp(),reason_code='connection_lost' WHERE organization_id=$1 AND id=$2`, f.p.OrganizationID, sentID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Pool.Exec(ctx, `UPDATE webhook_deliveries SET status='unknown',version=version+1 WHERE organization_id=$1 AND current_attempt_id=$2`, f.p.OrganizationID, sentID); err != nil {
		t.Fatal(err)
	}
	current, err := f.s.WebhookSubscription(ctx, f.p, f.task.ID, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := f.s.RevokeWebhookSubscription(ctx, f.p, f.task.ID, sub.ID, domain.NewID("key"), domain.RevokeWebhookSubscription{ExpectedVersion: current.Version, Reason: "Stop future task notifications"})
	view := configurationAuthorDecode[domain.WebhookSubscription](t, raw, err)
	if view.Status != "revoked" || len(view.AvailableCommands) != 0 {
		t.Fatal(view)
	}
	var status string
	var sentAt, unknownAt *time.Time
	if err = f.s.Pool.QueryRow(ctx, "SELECT status,sent_at,unknown_at FROM webhook_attempts WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, sentID).Scan(&status, &sentAt, &unknownAt); err != nil || status != "unknown" || sentAt == nil || unknownAt == nil {
		t.Fatal("revoke erased uncertain sent history", err)
	}
	if err = f.s.Pool.QueryRow(ctx, "SELECT status FROM webhook_attempts WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, queuedID).Scan(&status); err != nil || status != "stopped" {
		t.Fatal("revoke did not stop proven unsent work", err)
	}
	// This is a database fact probe, not a network/worker acceptance claim.
	if _, err = f.s.Pool.Exec(ctx, `UPDATE webhook_attempts SET status='acknowledged',http_status=204,observed_at=clock_timestamp(),reason_code='http_acknowledged' WHERE organization_id=$1 AND id=$2`, f.p.OrganizationID, sentID); err != nil {
		t.Fatal("late receipt could not resolve uncertainty after revoke", err)
	}
	if err = f.s.Pool.QueryRow(ctx, "SELECT unknown_at FROM webhook_attempts WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, sentID).Scan(&sentAt); err != nil || sentAt == nil || !sentAt.Equal(*unknownAt) {
		t.Fatal("late receipt erased unknown history", err)
	}
	f.material(t)
	if f.count(t, "webhook_deliveries", sub.ID) != 2 {
		t.Fatal("revoked subscription admitted a notification")
	}
}

func TestWebhookAuthorImmutableSQLBoundaries(t *testing.T) {
	f := webhookAuthorSetup(t)
	ctx := context.Background()
	sub := f.subscribe(t, 4)
	f.material(t)
	var delivery, attempt string
	if err := f.s.Pool.QueryRow(ctx, "SELECT id,current_attempt_id FROM webhook_deliveries WHERE organization_id=$1 AND subscription_id=$2", f.p.OrganizationID, sub.ID).Scan(&delivery, &attempt); err != nil {
		t.Fatal(err)
	}
	probes := []struct {
		name, q string
		args    []any
	}{
		{"envelope", `UPDATE webhook_deliveries SET envelope=envelope||' ',envelope_digest=encode(sha256(convert_to(envelope||' ','UTF8')),'hex'),version=version+1 WHERE organization_id=$1 AND id=$2`, []any{f.p.OrganizationID, delivery}},
		{"attempt_pointer", `UPDATE webhook_deliveries SET attempt_count=attempt_count+1,version=version+1 WHERE organization_id=$1 AND id=$2`, []any{f.p.OrganizationID, delivery}},
		{"last_sequence_without_counter", `UPDATE webhook_subscriptions SET last_admitted_sequence=last_admitted_sequence+1,version=version+1 WHERE organization_id=$1 AND id=$2`, []any{f.p.OrganizationID, sub.ID}},
		{"body_profile", `UPDATE webhook_destinations SET profile=jsonb_set(profile,'{endpoint}','"https://retarget.invalid/"') WHERE organization_id=$1`, []any{f.p.OrganizationID}},
		{"claim_to_ack", `UPDATE webhook_attempts SET status='acknowledged',http_status=200,observed_at=clock_timestamp() WHERE organization_id=$1 AND id=$2`, []any{f.p.OrganizationID, attempt}},
		{"delete_history", `DELETE FROM webhook_attempts WHERE organization_id=$1 AND id=$2`, []any{f.p.OrganizationID, attempt}},
	}
	for _, probe := range probes {
		t.Run(probe.name, func(t *testing.T) {
			if _, err := f.s.Pool.Exec(ctx, probe.q, probe.args...); err == nil {
				t.Fatal("forbidden SQL mutation accepted")
			}
		})
	}
	// Duplicate raw JSON properties must not bypass the metadata allowlist via
	// JSONB's last-key-wins parsing. Probe INSERT in a transaction which first
	// removes only these disposable test rows after disabling no-delete guards.
	t.Run("raw_duplicate_property", func(t *testing.T) {
		tx, err := f.s.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		for _, q := range []string{"ALTER TABLE webhook_attempts DISABLE TRIGGER webhook_attempt_no_delete", "ALTER TABLE webhook_deliveries DISABLE TRIGGER webhook_delivery_no_delete"} {
			if _, err = tx.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = tx.Exec(ctx, `CREATE TEMP TABLE raw_notification_copy ON COMMIT DROP AS SELECT * FROM webhook_deliveries WHERE organization_id=$1 AND id=$2`, f.p.OrganizationID, delivery); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "DELETE FROM webhook_attempts WHERE organization_id=$1 AND delivery_id=$2", f.p.OrganizationID, delivery); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "DELETE FROM webhook_deliveries WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, delivery); err != nil {
			t.Fatal(err)
		}
		// All other intent is held equal, including subscription count/sequence.
		if _, err = tx.Exec(ctx, "ALTER TABLE webhook_subscriptions DISABLE TRIGGER webhook_subscription_guard"); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "UPDATE webhook_subscriptions SET admitted_notifications=0,last_admitted_sequence=0 WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, sub.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `UPDATE raw_notification_copy SET envelope='{"delivery_id":"DO_NOT_EXPORT",'||substring(envelope FROM 2)`); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `UPDATE raw_notification_copy SET envelope_digest=encode(sha256(convert_to(envelope,'UTF8')),'hex')`); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, "INSERT INTO webhook_deliveries SELECT * FROM raw_notification_copy")
		if err == nil || !strings.Contains(err.Error(), "invalid webhook delivery intent") {
			t.Fatal("raw duplicate property not rejected by exact envelope binding", err)
		}
	})
}

func TestWebhookAuthorSentFenceLateReceiptAndAttemptLifetime(t *testing.T) {
	f := webhookAuthorSetup(t)
	ctx := context.Background()
	sub := f.subscribe(t, 4)
	f.material(t)
	var delivery, attempt string
	if err := f.s.Pool.QueryRow(ctx, "SELECT id,current_attempt_id FROM webhook_deliveries WHERE organization_id=$1 AND subscription_id=$2", f.p.OrganizationID, sub.ID).Scan(&delivery, &attempt); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.s.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE webhook_attempts SET status='leased',worker_id='author_worker',lease_until=clock_timestamp()+interval '1 minute',fencing_token=1 WHERE organization_id=$1 AND id=$2`, f.p.OrganizationID, attempt)
	exec(`UPDATE webhook_attempts SET status='sent',sent_at=clock_timestamp(),signature_timestamp=extract(epoch FROM clock_timestamp())::bigint WHERE organization_id=$1 AND id=$2`, f.p.OrganizationID, attempt)
	exec(`UPDATE webhook_deliveries SET status='sent',version=version+1 WHERE organization_id=$1 AND id=$2`, f.p.OrganizationID, delivery)
	for _, probe := range []struct{ name, set string }{
		{"sent_fence", "fencing_token=fencing_token+1"},
		{"sent_worker", "worker_id='replacement'"},
		{"sent_lease", "lease_until=lease_until+interval '1 second'"},
		{"sent_recovery", "recoveries=recoveries+1"},
		{"sent_requeue", "status='queued'"},
	} {
		t.Run(probe.name, func(t *testing.T) {
			if _, err := f.s.Pool.Exec(ctx, "UPDATE webhook_attempts SET "+probe.set+" WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, attempt); err == nil {
				t.Fatal("sent authority/history was rewritten")
			}
		})
	}
	exec(`UPDATE webhook_attempts SET status='unknown',unknown_at=clock_timestamp(),reason_code='transport_unknown' WHERE organization_id=$1 AND id=$2`, f.p.OrganizationID, attempt)
	exec(`UPDATE webhook_deliveries SET status='unknown',version=version+1 WHERE organization_id=$1 AND id=$2`, f.p.OrganizationID, delivery)
	for _, probe := range []struct{ name, set string }{
		{"unknown_resend", "status='sent'"},
		{"unknown_history_clear", "unknown_at=NULL"},
		{"unknown_history_rewrite", "unknown_at=unknown_at+interval '1 second'"},
		{"unknown_observed", "observed_at=clock_timestamp()"},
		{"ack_http500", "status='acknowledged',http_status=500,observed_at=clock_timestamp()"},
		{"not_ack_http204", "status='not_acknowledged',http_status=204,observed_at=clock_timestamp()"},
	} {
		t.Run(probe.name, func(t *testing.T) {
			if _, err := f.s.Pool.Exec(ctx, "UPDATE webhook_attempts SET "+probe.set+" WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, attempt); err == nil {
				t.Fatal("unknown/HTTP fact was falsified")
			}
		})
	}
	newID := "wha_" + Hash([]byte(delivery+"\x1f2"))
	tx, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE webhook_deliveries SET status='queued',attempt_count=2,current_attempt_id=$3,version=version+1 WHERE organization_id=$1 AND id=$2`, f.p.OrganizationID, delivery, newID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO webhook_attempts(organization_id,id,task_id,delivery_id,number) VALUES($1,$2,$3,$4,2)`, f.p.OrganizationID, newID, f.task.ID, delivery); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var unknownAt time.Time
	if err = f.s.Pool.QueryRow(ctx, "SELECT unknown_at FROM webhook_attempts WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, attempt).Scan(&unknownAt); err != nil {
		t.Fatal(err)
	}
	// A receipt for the older exact physical attempt remains factual. It is
	// never permission to send the queued replacement or to erase uncertainty.
	exec(`UPDATE webhook_attempts SET status='acknowledged',http_status=204,observed_at=clock_timestamp(),reason_code='http_acknowledged' WHERE organization_id=$1 AND id=$2`, f.p.OrganizationID, attempt)
	exec(`UPDATE webhook_attempts SET status='stopped',reason_code='already_acknowledged' WHERE organization_id=$1 AND id=$2`, f.p.OrganizationID, newID)
	exec(`UPDATE webhook_deliveries SET status='acknowledged',version=version+1 WHERE organization_id=$1 AND id=$2`, f.p.OrganizationID, delivery)
	var count, currentUnknown, everUnknown, acked int
	var at time.Time
	if err = f.s.Pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE status='unknown'),count(*) FILTER(WHERE unknown_at IS NOT NULL),count(*) FILTER(WHERE status='acknowledged') FROM webhook_attempts WHERE organization_id=$1 AND delivery_id=$2`, f.p.OrganizationID, delivery).Scan(&count, &currentUnknown, &everUnknown, &acked); err != nil || count != 2 || currentUnknown != 0 || everUnknown != 1 || acked != 1 {
		t.Fatal("late fact hid physical attempt history", count, currentUnknown, everUnknown, acked, err)
	}
	if err = f.s.Pool.QueryRow(ctx, "SELECT unknown_at FROM webhook_attempts WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, attempt).Scan(&at); err != nil || !at.Equal(unknownAt) {
		t.Fatal("historical unknown timestamp changed", err)
	}
	if _, err = f.s.Pool.Exec(ctx, `UPDATE webhook_deliveries SET status='unknown',version=version+1 WHERE organization_id=$1 AND id=$2`, f.p.OrganizationID, delivery); err == nil {
		t.Fatal("aggregate acknowledgement was undone")
	}
	if _, err = f.s.Pool.Exec(ctx, `UPDATE webhook_deliveries SET status='queued',attempt_count=3,current_attempt_id=$3,version=version+1 WHERE organization_id=$1 AND id=$2`, f.p.OrganizationID, delivery, "wha_"+Hash([]byte(delivery+"\x1f3"))); err == nil {
		t.Fatal("acknowledged delivery allocated a new retry")
	}
}
