// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/webhookruntime"
)

type independentWebhookRuntimeFixture struct {
	independentWebhookStore
	dsn, key string
	calls    atomic.Int64
}

func independentWebhookRuntimeSetup(t *testing.T, httpStatus int, ttl ...int) *independentWebhookRuntimeFixture {
	t.Helper()
	f := &independentWebhookRuntimeFixture{}
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.calls.Add(1); w.WriteHeader(httpStatus) }))
	t.Cleanup(peer.Close)
	s, dsn := testStore(t)
	p, _ := identity(t, s, 1_000_000)
	a := agent(t, s, p)
	f.independentWebhookStore = independentWebhookStore{s: s, p: p, a: a, task: task(t, s, p, a)}
	f.dsn = dsn
	f.profile = domain.WebhookDestinationProfile{Destination: domain.WebhookDestination{ID: "runtime_independent", OrganizationID: p.OrganizationID, Version: 1, Name: "Independent local TLS receiver", SigningKeyID: "runtime_key", UnmeteredApproved: true, Active: true}, Endpoint: peer.URL + "/notification", SigningKeyEnvironment: "PLATFORM_WEBHOOK_KEY_RUNTIME_INDEPENDENT", AllowPrivateNetwork: true, TimeoutSeconds: 2, TrustedCAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: peer.Certificate().Raw}))}
	if _, err := s.InstallWebhookDestination(context.Background(), f.profile); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	f.key = base64.RawURLEncoding.EncodeToString(key)
	in := f.input()
	if len(ttl) != 0 {
		in.TTLSeconds = ttl[0]
	}
	f.subscribe(t, f.p, f.task.ID, in)
	f.material(t)
	return f
}

func (f *independentWebhookRuntimeFixture) claim(t *testing.T, duration time.Duration) *domain.WebhookLease {
	t.Helper()
	l, _, err := f.s.ClaimWebhook(context.Background(), "independent_worker", duration)
	if err != nil || l == nil {
		t.Fatal("independent claim unavailable", err)
	}
	return l
}

func (f *independentWebhookRuntimeFixture) runtime(t *testing.T, backend webhookruntime.Store) *webhookruntime.Runtime {
	t.Helper()
	r, err := webhookruntime.New(backend, webhookruntime.Config{Profile: f.profile, EncodedSigningKey: f.key, PersistenceTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func independentWebhookObservation(l *domain.WebhookLease, status int) domain.WebhookObservation {
	return domain.WebhookObservation{OrganizationID: l.OrganizationID, TaskID: l.TaskID, DeliveryID: l.DeliveryID, AttemptID: l.AttemptID, WorkerID: l.WorkerID, Token: l.Token, EnvelopeDigest: l.EnvelopeDigest, HTTPStatus: status}
}

func (f *independentWebhookRuntimeFixture) mark(t *testing.T, l *domain.WebhookLease) domain.WebhookDispatchPermit {
	t.Helper()
	p, err := f.s.PrepareWebhookDispatch(context.Background(), l)
	if err != nil {
		t.Fatal(err)
	}
	if p.ValidFor <= 0 || p.ValidFor > 2*time.Second || p.NotAfter.After(l.LeaseUntil) || p.NotAfter.Sub(p.IssuedAt) != p.ValidFor {
		t.Fatal("unbounded dispatch permit")
	}
	return p
}

func (f *independentWebhookRuntimeFixture) observe(t *testing.T, l *domain.WebhookLease, status int) domain.WebhookDelivery {
	t.Helper()
	d, err := f.s.RecordWebhookObservation(context.Background(), independentWebhookObservation(l, status))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (f *independentWebhookRuntimeFixture) retry(t *testing.T, d domain.WebhookDelivery) domain.WebhookDelivery {
	t.Helper()
	in := domain.RetryWebhookDelivery{ExpectedVersion: d.Version, ExpectedAttemptID: d.CurrentAttemptID, EnvelopeDigest: d.EnvelopeDigest, AcknowledgePossibleDuplicate: true, Reason: "Independent explicit duplicate-risk consent"}
	raw, err := f.s.RetryWebhookDelivery(context.Background(), f.p, f.task.ID, d.ID, domain.NewID("key"), in)
	return independentWebhookDecode[domain.WebhookDelivery](t, raw, err)
}

func TestIndependentWebhookRuntimeRejectsStaleForeignAndAlteredFences(t *testing.T) {
	f := independentWebhookRuntimeSetup(t, 204)
	ctx := context.Background()
	l := f.claim(t, 10*time.Second)
	for name, change := range map[string]func(*domain.WebhookLease){
		"token": func(v *domain.WebhookLease) { v.Token++ }, "worker": func(v *domain.WebhookLease) { v.WorkerID = "another_worker" },
		"task": func(v *domain.WebhookLease) { v.TaskID = domain.NewID("tsk") }, "org": func(v *domain.WebhookLease) { v.OrganizationID = domain.NewID("org") },
		"subscription": func(v *domain.WebhookLease) { v.SubscriptionID = domain.NewID("whs") }, "delivery": func(v *domain.WebhookLease) { v.DeliveryID = domain.NewID("whd") },
		"attempt": func(v *domain.WebhookLease) { v.AttemptID = domain.NewID("wha") }, "profile": func(v *domain.WebhookLease) { v.ProfileDigest = Hash([]byte("foreign profile")) },
		"envelope": func(v *domain.WebhookLease) { v.Envelope = append(append([]byte{}, v.Envelope...), 32) }, "lease": func(v *domain.WebhookLease) { v.LeaseUntil = v.LeaseUntil.Add(time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			v := *l
			change(&v)
			if _, err := f.s.PrepareWebhookDispatch(ctx, &v); err == nil {
				t.Fatal("altered durable fence admitted")
			}
		})
	}
	var marked int
	if err := f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook_attempts WHERE organization_id=$1 AND sent_at IS NOT NULL", f.p.OrganizationID).Scan(&marked); err != nil || marked != 0 {
		t.Fatal("rejected fence marked a send", err)
	}
	f.mark(t, l)
	if _, err := f.s.PrepareWebhookDispatch(ctx, l); err == nil {
		t.Fatal("second permit issued for same physical attempt")
	}
	d := f.observe(t, l, 0)
	if d.Status != "unknown" || d.UnknownAttempts != 1 || d.EverUnknownAttempts != 1 {
		t.Fatal("unknown fact missing")
	}
	for i := 0; i < 3; i++ {
		if lease, _, err := f.s.ClaimWebhook(ctx, "other_worker", time.Second); err != nil || lease != nil {
			t.Fatal("unknown automatically claimed", err)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("state-only test unexpectedly transmitted")
	}
}

func TestIndependentWebhookRuntimeLateAckStopsOnlyUnsentReplacement(t *testing.T) {
	for _, markedReplacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmarked", true: "marked"}[markedReplacement], func(t *testing.T) {
			f := independentWebhookRuntimeSetup(t, 204)
			ctx := context.Background()
			first := f.claim(t, 10*time.Second)
			f.mark(t, first)
			d := f.observe(t, first, 0)
			replacement := f.retry(t, d)
			var next *domain.WebhookLease
			if markedReplacement {
				next = f.claim(t, 10*time.Second)
				f.mark(t, next)
			}
			d = f.observe(t, first, 204)
			if d.Status != "acknowledged" || d.UnknownAttempts != 0 || d.EverUnknownAttempts != 1 || d.AcknowledgedAttempts != 1 || d.AttemptCount != 2 {
				t.Fatal("old ACK lost independent history", d.Status)
			}
			var state string
			if err := f.s.Pool.QueryRow(ctx, "SELECT status FROM webhook_attempts WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, replacement.CurrentAttemptID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if markedReplacement {
				if state != "sent" {
					t.Fatal("old ACK rewrote already sent replacement")
				}
				d = f.observe(t, next, 0)
				if d.Status != "acknowledged" || d.UnknownAttempts != 1 || d.EverUnknownAttempts != 2 || d.AcknowledgedAttempts != 1 {
					t.Fatal("replacement timeout lost aggregate ACK or history")
				}
			} else if state != "stopped" {
				t.Fatal("unmarked replacement survived old ACK", state)
			}
			var before time.Time
			if err := f.s.Pool.QueryRow(ctx, "SELECT observed_at FROM webhook_attempts WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, first.AttemptID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			version := d.Version
			for _, status := range []int{204, 0, 204} {
				same := f.observe(t, first, status)
				if same.Version != version {
					t.Fatal("duplicate factual persistence churned history")
				}
			}
			var after time.Time
			if err := f.s.Pool.QueryRow(ctx, "SELECT observed_at FROM webhook_attempts WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, first.AttemptID).Scan(&after); err != nil || !before.Equal(after) {
				t.Fatal("duplicate observation rewrote time", err)
			}
			if _, err := f.s.RecordWebhookObservation(ctx, independentWebhookObservation(first, 500)); err == nil {
				t.Fatal("contrary definitive response overwrote ACK")
			}
			_, err := f.s.RetryWebhookDelivery(ctx, f.p, f.task.ID, d.ID, domain.NewID("key"), domain.RetryWebhookDelivery{ExpectedVersion: d.Version, ExpectedAttemptID: d.CurrentAttemptID, EnvelopeDigest: d.EnvelopeDigest, AcknowledgePossibleDuplicate: true, Reason: "Forbidden further duplicate"})
			code(t, err, "conflict")
			if lease, _, err := f.s.ClaimWebhook(ctx, "after_ack", time.Second); err != nil || lease != nil {
				t.Fatal("ACK aggregate admitted new request", err)
			}
		})
	}
}

func TestIndependentWebhookRuntimeRecoveryAndLifetimeCaps(t *testing.T) {
	f := independentWebhookRuntimeSetup(t, 204)
	ctx := context.Background()
	const contenders = 4
	leases := make(chan *domain.WebhookLease, contenders)
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, _, e := f.s.ClaimWebhook(ctx, "claim_racer", time.Second)
			leases <- l
			errs <- e
		}()
	}
	wg.Wait()
	close(leases)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var original *domain.WebhookLease
	won := 0
	for l := range leases {
		if l != nil {
			won++
			original = l
		}
	}
	if won != 1 {
		t.Fatal("multiple claim winners", won)
	}
	for recovery := 1; recovery <= 5; recovery++ {
		if _, err := f.s.Pool.Exec(ctx, "UPDATE webhook_attempts SET lease_until=clock_timestamp()-interval '1 millisecond' WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, original.AttemptID); err != nil {
			t.Fatal(err)
		}
		current := f.claim(t, time.Second)
		if current.Token != int64(recovery+1) {
			t.Fatal("recovery did not fence old worker")
		}
		if _, err := f.s.PrepareWebhookDispatch(ctx, original); err == nil {
			t.Fatal("expired old fence admitted")
		}
	}
	if _, err := f.s.Pool.Exec(ctx, "UPDATE webhook_attempts SET lease_until=clock_timestamp()-interval '1 millisecond' WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, original.AttemptID); err != nil {
		t.Fatal(err)
	}
	if l, _, err := f.s.ClaimWebhook(ctx, "sixth_recovery", time.Second); err != nil || l != nil {
		t.Fatal("sixth recovery admitted", err)
	}
	d, err := f.s.WebhookDelivery(ctx, f.p, f.task.ID, original.DeliveryID)
	if err != nil || d.Status != "stopped" || d.AttemptCount != 1 {
		t.Fatal("recovery cap lost lifetime", err)
	}
	for n := 2; n <= 8; n++ {
		d = f.retry(t, d)
		l := f.claim(t, 10*time.Second)
		f.mark(t, l)
		d = f.observe(t, l, 503)
		if d.AttemptCount != n || d.Status != "not_acknowledged" {
			t.Fatal("explicit retry counter diverged")
		}
		if l, _, err := f.s.ClaimWebhook(ctx, "no_503_retry", time.Second); err != nil || l != nil {
			t.Fatal("non2xx automatically retried", err)
		}
	}
	_, err = f.s.RetryWebhookDelivery(ctx, f.p, f.task.ID, d.ID, domain.NewID("key"), domain.RetryWebhookDelivery{ExpectedVersion: d.Version, ExpectedAttemptID: d.CurrentAttemptID, EnvelopeDigest: d.EnvelopeDigest, AcknowledgePossibleDuplicate: true, Reason: "Ninth attempt forbidden"})
	code(t, err, "conflict")
	if f.calls.Load() != 0 {
		t.Fatal("state probes unexpectedly transmitted")
	}
}

func independentWebhookWaitForBlock(t *testing.T, s *Store, blocker int) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var blocked bool
		if err := s.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))", blocker).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no real admission waiter")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestIndependentWebhookRuntimeRechecksProfileAfterOrganizationWait(t *testing.T) {
	f := independentWebhookRuntimeSetup(t, 204)
	ctx := context.Background()
	l := f.claim(t, 10*time.Second)
	block, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer block.Rollback(ctx)
	var pid int
	if err = block.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err = block.Exec(ctx, "SELECT id FROM organizations WHERE id=$1 FOR UPDATE", f.p.OrganizationID); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := f.s.PrepareWebhookDispatch(bounded, l); done <- e }()
	independentWebhookWaitForBlock(t, f.s, pid)
	if _, err = block.Exec(ctx, "UPDATE webhook_destinations SET active=false WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.profile.Destination.ID); err != nil {
		t.Fatal(err)
	}
	if err = block.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil {
		t.Fatal("retired profile passed after-lock admission")
	}
	var marked bool
	if err = f.s.Pool.QueryRow(ctx, "SELECT sent_at IS NOT NULL FROM webhook_attempts WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, l.AttemptID).Scan(&marked); err != nil || marked {
		t.Fatal("retirement marked a request", err)
	}
}

func TestIndependentWebhookRuntimeRechecksEpochAfterAuthorityWait(t *testing.T) {
	f := independentWebhookRuntimeSetup(t, 204)
	ctx := context.Background()
	f.member(t, "owner") // retain another owner while cycling original consent
	l := f.claim(t, 10*time.Second)
	block, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer block.Rollback(ctx)
	var pid int
	if err = block.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err = block.Exec(ctx, "SELECT principal_id FROM memberships WHERE organization_id=$1 AND principal_id=$2 FOR UPDATE", f.p.OrganizationID, f.p.ID); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := f.s.PrepareWebhookDispatch(bounded, l); done <- e }()
	independentWebhookWaitForBlock(t, f.s, pid)
	// Admin still has management permission, so the immutable epoch rather than
	// an outright missing current permission must invalidate the old consent.
	if _, err = block.Exec(ctx, "UPDATE memberships SET role='admin' WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, f.p.ID); err != nil {
		t.Fatal(err)
	}
	if err = block.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil {
		t.Fatal("changed epoch survived authority-lock wait")
	}
	if _, err = f.runtime(t, f.s).Execute(ctx, l); err == nil {
		t.Fatal("stale epoch transmitted")
	}
	if err = f.s.StopWebhookUnsent(ctx, l, "execution_not_admitted"); err != nil {
		t.Fatal("revoked consent blocked known-unsent cleanup", err)
	}
	if f.calls.Load() != 0 {
		t.Fatal("authority-wait denial sent receiver bytes")
	}
}

func TestIndependentWebhookRuntimeRechecksExpiryAfterOrganizationWait(t *testing.T) {
	f := independentWebhookRuntimeSetup(t, 204, 1)
	ctx := context.Background()
	l := f.claim(t, 10*time.Second)
	d, err := f.s.WebhookDelivery(ctx, f.p, f.task.ID, l.DeliveryID)
	if err != nil || !l.LeaseUntil.Equal(d.ExpiresAt) {
		t.Fatal("lease not clipped to exact subscription expiry", err)
	}
	block, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer block.Rollback(ctx)
	var pid int
	if err = block.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err = block.Exec(ctx, "SELECT id FROM organizations WHERE id=$1 FOR UPDATE", f.p.OrganizationID); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := f.s.PrepareWebhookDispatch(bounded, l); done <- e }()
	independentWebhookWaitForBlock(t, f.s, pid)
	if _, err = block.Exec(ctx, "SELECT pg_sleep(1.05)"); err != nil {
		t.Fatal(err)
	}
	if err = block.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil {
		t.Fatal("expired consent passed after-lock clock check")
	}
	if lease, _, err := f.s.ClaimWebhook(ctx, "expired_cleanup", time.Second); err != nil || lease != nil {
		t.Fatal("expired unmarked work reclaimed", err)
	}
	d, err = f.s.WebhookDelivery(ctx, f.p, f.task.ID, l.DeliveryID)
	if err != nil || d.Status != "expired" || f.calls.Load() != 0 {
		t.Fatal("expired marker sent or was not cleaned", err)
	}
}

type independentWebhookDelayedPermit struct {
	*Store
	delay time.Duration
}

func (s independentWebhookDelayedPermit) PrepareWebhookDispatch(ctx context.Context, l *domain.WebhookLease) (domain.WebhookDispatchPermit, error) {
	p, e := s.Store.PrepareWebhookDispatch(ctx, l)
	if e == nil {
		time.Sleep(s.delay)
	}
	return p, e
}

func TestIndependentWebhookRuntimeShortDelayedPermitCannotTransmit(t *testing.T) {
	f := independentWebhookRuntimeSetup(t, 204)
	l := f.claim(t, time.Second)
	r := f.runtime(t, independentWebhookDelayedPermit{Store: f.s, delay: 1100 * time.Millisecond})
	if _, err := r.Execute(context.Background(), l); err == nil {
		t.Fatal("delayed expired permit accepted")
	}
	d, err := f.s.WebhookDelivery(context.Background(), f.p, f.task.ID, l.DeliveryID)
	if err != nil || d.Status != "unknown" || f.calls.Load() != 0 {
		t.Fatal("expired marked permit sent or lost conservative uncertainty", err)
	}
}

func TestIndependentWebhookRuntimePgwireMarkerCommitLossNoIO(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			f := independentWebhookRuntimeSetup(t, 204)
			l := f.claim(t, 10*time.Second)
			victim, proxy := independentCommitProxyStore(t, f.dsn)
			cut := proxy.arm(t, "set status='sent',sent_at", mode)
			if _, err := f.runtime(t, victim).Execute(context.Background(), l); err == nil {
				t.Fatal("marker commit cut returned successful send")
			}
			proxy.wait(t, cut)
			var marked bool
			var state string
			if err := f.s.Pool.QueryRow(context.Background(), "SELECT sent_at IS NOT NULL,status FROM webhook_attempts WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, l.AttemptID).Scan(&marked, &state); err != nil {
				t.Fatal(err)
			}
			if marked != (mode == "after_commit") || f.calls.Load() != 0 {
				t.Fatal("commit marker and physical requests diverged", marked, f.calls.Load())
			}
			if mode == "after_commit" {
				if state != "unknown" {
					t.Fatal("marker commit uncertainty not retained", state)
				}
				for i := 0; i < 2; i++ {
					if next, _, err := f.s.ClaimWebhook(context.Background(), "no_resend", time.Second); err != nil || next != nil {
						t.Fatal("ambiguous committed marker replayed", err)
					}
				}
			}
		})
	}
}

func TestIndependentWebhookRuntimePgwireObservationLossNeverResends(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			f := independentWebhookRuntimeSetup(t, 204)
			l := f.claim(t, time.Second)
			victim, proxy := independentCommitProxyStore(t, f.dsn)
			cut := proxy.arm(t, "observed_at=case", mode)
			if _, err := f.runtime(t, victim).Execute(context.Background(), l); err == nil {
				t.Fatal("observation commit cut hidden")
			}
			proxy.wait(t, cut)
			if f.calls.Load() != 1 {
				t.Fatal("observation cut duplicated or lost physical request", f.calls.Load())
			}
			if mode == "before_commit" {
				time.Sleep(1050 * time.Millisecond)
				if _, err := f.s.ReconcileWebhookSent(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			d, err := f.s.WebhookDelivery(context.Background(), f.p, f.task.ID, l.DeliveryID)
			if err != nil {
				t.Fatal(err)
			}
			want := "unknown"
			if mode == "after_commit" {
				want = "acknowledged"
			}
			if d.Status != want {
				t.Fatal("durable observation classification wrong", d.Status)
			}
			for i := 0; i < 2; i++ {
				if next, _, err := f.s.ClaimWebhook(context.Background(), "no_response_replay", time.Second); err != nil || next != nil {
					t.Fatal("lost observation triggered another physical request", err)
				}
			}
			f.observe(t, l, 204)
			if f.calls.Load() != 1 {
				t.Fatal("late database repair resent receiver request")
			}
		})
	}
}
