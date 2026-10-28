// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/webhookruntime"
	"github.com/jackc/pgx/v5"
)

type webhookRuntimeFixture struct {
	webhookAuthorFixture
	encoded   string
	key       []byte
	calls     atomic.Int64
	mode      string
	committed chan struct{}
}

func webhookRuntimeSetup(t *testing.T, mode string) *webhookRuntimeFixture {
	t.Helper()
	f := &webhookRuntimeFixture{webhookAuthorFixture: webhookAuthorSetup(t), mode: mode, key: bytesForWebhookAuthorKey(), committed: make(chan struct{}, 4)}
	f.encoded = base64.RawURLEncoding.EncodeToString(f.key)
	if _, err := f.s.Pool.Exec(context.Background(), `CREATE TABLE author_webhook_receiver_inbox(organization_id text NOT NULL,delivery_id text NOT NULL,digest text NOT NULL,body text NOT NULL,PRIMARY KEY(organization_id,delivery_id))`); err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.calls.Add(1)
		body, err := io.ReadAll(io.LimitReader(r.Body, domain.MaxWebhookEnvelopeBytes+1))
		if err != nil || len(body) > domain.MaxWebhookEnvelopeBytes || r.Method != "POST" || r.Header.Get("Authorization") != "" || r.ProtoMajor != 1 {
			t.Error("receiver got invalid request shape")
			w.WriteHeader(400)
			return
		}
		for _, name := range []string{"Webhook-Key-ID", "Webhook-ID", "Webhook-Attempt-ID", "Webhook-Timestamp", "Webhook-Signature"} {
			if len(r.Header.Values(name)) != 1 || strings.Contains(r.Header.Get(name), ",") {
				t.Error("receiver got ambiguous signature headers")
				w.WriteHeader(400)
				return
			}
		}
		// Independent HMAC calculation from the frozen contract; never invoke
		// production Verify from this receiver fixture.
		mac := hmac.New(sha256.New, f.key)
		_, _ = mac.Write([]byte("agent-platform-webhook-v1\n" + r.Header.Get("Webhook-Key-ID") + "\n" + r.Header.Get("Webhook-Timestamp") + "\n" + r.Header.Get("Webhook-ID") + "\n" + r.Header.Get("Webhook-Attempt-ID") + "\n"))
		_, _ = mac.Write(body)
		at, e := strconv.ParseInt(r.Header.Get("Webhook-Timestamp"), 10, 64)
		var envelope domain.WebhookEnvelope
		if e != nil || at < time.Now().Unix()-300 || at > time.Now().Unix()+300 || r.Header.Get("Webhook-Key-ID") != f.profile.Destination.SigningKeyID || !hmac.Equal([]byte(r.Header.Get("Webhook-Signature")), []byte("v1="+hex.EncodeToString(mac.Sum(nil)))) || json.Unmarshal(body, &envelope) != nil || envelope.DeliveryID != r.Header.Get("Webhook-ID") || envelope.OrganizationID != f.p.OrganizationID || envelope.Event.TaskID != f.task.ID {
			t.Error("receiver HMAC/envelope identity failed")
			w.WriteHeader(400)
			return
		}
		tx, e := f.s.Pool.Begin(r.Context())
		if e != nil {
			t.Error("receiver begin failed")
			w.WriteHeader(503)
			return
		}
		defer tx.Rollback(context.Background())
		var stored string
		e = tx.QueryRow(r.Context(), `INSERT INTO author_webhook_receiver_inbox(organization_id,delivery_id,digest,body) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING RETURNING digest`, envelope.OrganizationID, envelope.DeliveryID, Hash(body), string(body)).Scan(&stored)
		if errors.Is(e, pgx.ErrNoRows) {
			e = tx.QueryRow(r.Context(), "SELECT digest FROM author_webhook_receiver_inbox WHERE organization_id=$1 AND delivery_id=$2", envelope.OrganizationID, envelope.DeliveryID).Scan(&stored)
		}
		if e != nil || stored != Hash(body) {
			t.Error("receiver durable duplicate/body mismatch")
			w.WriteHeader(409)
			return
		}
		if e = tx.Commit(r.Context()); e != nil {
			t.Error("receiver commit failed")
			w.WriteHeader(503)
			return
		}
		if mode == "block_after_commit" && n == 1 {
			f.committed <- struct{}{}
			<-r.Context().Done()
			return
		}
		if mode == "unknown_first" && n == 1 {
			conn, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
				return
			}
			_ = conn.Close()
			return
		}
		if mode == "non2xx" {
			w.WriteHeader(503)
			return
		}
		if mode == "redirect" {
			w.Header().Set("Location", r.URL.String()+"/must-not-follow")
			w.WriteHeader(307)
			return
		}
		if mode == "echo" {
			w.Header().Set("X-Untrusted-Echo", f.encoded)
			w.Header().Set("Content-Length", "1000000000")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(f.encoded + " " + r.Header.Get("Webhook-Signature")))
			return
		}
		w.WriteHeader(204)
	}))
	t.Cleanup(peer.Close)
	f.profile.Destination.Version = 2
	f.profile.Endpoint = peer.URL
	f.profile.AllowPrivateNetwork = true
	f.profile.TrustedCAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: peer.Certificate().Raw}))
	if mode == "bad_ca" {
		f.profile.TrustedCAPEM = ""
	}
	if mode == "private_denied" {
		f.profile.AllowPrivateNetwork = false
	}
	if _, err := f.s.InstallWebhookDestination(context.Background(), f.profile); err != nil {
		t.Fatal(err)
	}
	in := webhookAuthorInput(16)
	in.DestinationVersion = 2
	in.EventTypes = []string{"material.created"}
	if _, err := f.s.CreateWebhookSubscription(context.Background(), f.p, f.task.ID, domain.NewID("key"), in); err != nil {
		t.Fatal(err)
	}
	f.material(t)
	return f
}
func bytesForWebhookAuthorKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}
func (f *webhookRuntimeFixture) claim(t *testing.T) *domain.WebhookLease {
	t.Helper()
	l, profile, err := f.s.ClaimWebhook(context.Background(), "author_webhook_worker", 45*time.Second)
	if err != nil || l == nil || webhookruntime.ProfileDigest(profile) != l.ProfileDigest {
		t.Fatal("webhook claim failed", err)
	}
	return l
}
func (f *webhookRuntimeFixture) runtime(t *testing.T, store webhookruntime.Store) *webhookruntime.Runtime {
	t.Helper()
	r, err := webhookruntime.New(store, webhookruntime.Config{Profile: f.profile, EncodedSigningKey: f.encoded, PersistenceTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func (f *webhookRuntimeFixture) delivery(t *testing.T, id string) domain.WebhookDelivery {
	t.Helper()
	d, err := f.s.WebhookDelivery(context.Background(), f.p, f.task.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func (f *webhookRuntimeFixture) retry(t *testing.T, d domain.WebhookDelivery) domain.WebhookDelivery {
	t.Helper()
	raw, err := f.s.RetryWebhookDelivery(context.Background(), f.p, f.task.ID, d.ID, domain.NewID("key"), domain.RetryWebhookDelivery{ExpectedVersion: d.Version, ExpectedAttemptID: d.CurrentAttemptID, EnvelopeDigest: d.EnvelopeDigest, AcknowledgePossibleDuplicate: true, Reason: "Explicit duplicate-risk retry to same receiver"})
	return configurationAuthorDecode[domain.WebhookDelivery](t, raw, err)
}

func TestWebhookRuntimeAuthorTLSUnknownRetryDurableInbox(t *testing.T) {
	f := webhookRuntimeSetup(t, "unknown_first")
	ctx := context.Background()
	first := f.claim(t)
	d, err := f.runtime(t, f.s).Execute(ctx, first)
	if err == nil || d.Status != "unknown" || d.UnknownAttempts != 1 || d.EverUnknownAttempts != 1 || f.calls.Load() != 1 {
		t.Fatal("unknown receipt state incorrect", d.Status, err)
	}
	for i := 0; i < 3; i++ {
		l, _, e := f.s.ClaimWebhook(ctx, "another_worker", time.Second)
		if e != nil || l != nil {
			t.Fatal("unknown physical request automatically reclaimed", e)
		}
	}
	d = f.retry(t, f.delivery(t, first.DeliveryID))
	second := f.claim(t)
	if second.AttemptID == first.AttemptID || string(second.Envelope) != string(first.Envelope) || second.DeliveryID != first.DeliveryID || d.AttemptCount != 2 {
		t.Fatal("retry retargeted stable logical delivery/body")
	}
	d, err = f.runtime(t, f.s).Execute(ctx, second)
	if err != nil || d.Status != "acknowledged" || d.AttemptCount != 2 || d.UnknownAttempts != 1 || d.EverUnknownAttempts != 1 || d.AcknowledgedAttempts != 1 || f.calls.Load() != 2 {
		t.Fatal("explicit retry/ack history incorrect", err, d.Status)
	}
	var inbox int
	if err = f.s.Pool.QueryRow(ctx, "SELECT count(*) FROM author_webhook_receiver_inbox").Scan(&inbox); err != nil || inbox != 1 {
		t.Fatal("receiver processed logical duplicate", err, inbox)
	}
	// An exact late fact resolves the first uncertainty without clearing history.
	d, err = f.s.RecordWebhookObservation(ctx, domain.WebhookObservation{OrganizationID: first.OrganizationID, TaskID: first.TaskID, DeliveryID: first.DeliveryID, AttemptID: first.AttemptID, WorkerID: first.WorkerID, Token: first.Token, EnvelopeDigest: first.EnvelopeDigest, HTTPStatus: 204})
	if err != nil || d.Status != "acknowledged" || d.UnknownAttempts != 0 || d.EverUnknownAttempts != 1 || d.AcknowledgedAttempts != 2 {
		t.Fatal("late old receipt erased history", err)
	}
	attempts, next, err := f.s.WebhookAttempts(ctx, f.p, f.task.ID, d.ID, "", 1)
	if err != nil || len(attempts) != 1 || next == "" {
		t.Fatal("attempt pagination failed", err)
	}
	more, end, err := f.s.WebhookAttempts(ctx, f.p, f.task.ID, d.ID, next, 1)
	if err != nil || len(more) != 1 || end != "" || more[0].ID == attempts[0].ID {
		t.Fatal("attempt history pagination duplicated/skipped", err)
	}
	t.Log("real TLS:2 physical requests,1 durable receiver inbox row,2 distinct attempts,one frozen body; no automatic unknown resend")
}

func TestWebhookRuntimeAuthorTransportFailureBoundaries(t *testing.T) {
	for _, mode := range []string{"redirect", "non2xx", "bad_ca", "private_denied", "echo"} {
		t.Run(mode, func(t *testing.T) {
			f := webhookRuntimeSetup(t, mode)
			l := f.claim(t)
			d, err := f.runtime(t, f.s).Execute(context.Background(), l)
			want, calls := "not_acknowledged", int64(1)
			if mode == "bad_ca" || mode == "private_denied" {
				want, calls = "unknown", 0
				if err == nil {
					t.Fatal("blocked transport pretended success")
				}
			}
			if mode == "echo" {
				want = "acknowledged"
			}
			if d.Status != want || f.calls.Load() != calls {
				t.Fatal("transport retried/followed redirect or misclassified status", d.Status, f.calls.Load())
			}
			for i := 0; i < 2; i++ {
				again, _, e := f.s.ClaimWebhook(context.Background(), "no_resend_worker", time.Second)
				if e != nil || again != nil {
					t.Fatal("HTTP outcome caused auto resend", e)
				}
			}
			raw, _ := json.Marshal(d)
			if strings.Contains(string(raw), f.encoded) || strings.Contains(string(raw), f.profile.Endpoint) {
				t.Fatal("receiver echo/key/URL escaped public delivery")
			}
		})
	}
}

type webhookAuthorFaultStore struct {
	*Store
	mode string
}

func (f webhookAuthorFaultStore) PrepareWebhookDispatch(ctx context.Context, l *domain.WebhookLease) (domain.WebhookDispatchPermit, error) {
	if f.mode == "before_marker" {
		return domain.WebhookDispatchPermit{}, errors.New("untrusted commit failure canary")
	}
	p, err := f.Store.PrepareWebhookDispatch(ctx, l)
	if err != nil {
		return p, err
	}
	if f.mode == "commit_ack_lost" {
		return p, errors.New("untrusted commit failure canary")
	}
	if f.mode == "expired" {
		p.ValidFor = time.Millisecond
		p.NotAfter = p.IssuedAt.Add(p.ValidFor)
		time.Sleep(5 * time.Millisecond)
	}
	if f.mode == "foreign_permit" {
		p.AttemptID = "another_attempt"
	}
	return p, nil
}
func TestWebhookRuntimeAuthorCommitAcknowledgmentAndPermitFailures(t *testing.T) {
	for _, mode := range []string{"before_marker", "commit_ack_lost", "expired", "foreign_permit"} {
		t.Run(mode, func(t *testing.T) {
			f := webhookRuntimeSetup(t, "ok")
			l := f.claim(t)
			_, err := f.runtime(t, webhookAuthorFaultStore{Store: f.s, mode: mode}).Execute(context.Background(), l)
			if err == nil || strings.Contains(err.Error(), "canary") || f.calls.Load() != 0 {
				t.Fatal("ambiguous marker/invalid permit dispatched or leaked backend error", err)
			}
			d := f.delivery(t, l.DeliveryID)
			want := "unknown"
			if mode == "before_marker" {
				want = "leased"
			}
			if d.Status != want {
				t.Fatal("marker fact disagreed with uncertainty", d.Status, want)
			}
			if mode != "before_marker" {
				again, _, err := f.s.ClaimWebhook(context.Background(), "must_not_replay", time.Second)
				if err != nil || again != nil {
					t.Fatal("uncertain marker automatically reclaimed", err)
				}
			}
		})
	}
}

func TestWebhookRuntimeAuthorCompetingClaimsFencesAndRecoveryBound(t *testing.T) {
	f := webhookAuthorSetup(t)
	f.subscribe(t, 4)
	f.material(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	leases := make(chan *domain.WebhookLease, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, _, err := f.s.ClaimWebhook(ctx, domain.NewID("claimant"), time.Second)
			leases <- l
			errs <- err
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
	var first, current *domain.WebhookLease
	assigned := 0
	for l := range leases {
		if l != nil {
			assigned++
			first = l
			current = l
		}
	}
	if assigned != 1 {
		t.Fatal("multiple claims assigned one physical attempt", assigned)
	}
	for i := 0; i < 5; i++ {
		if _, err := f.s.Pool.Exec(ctx, "UPDATE webhook_attempts SET lease_until=clock_timestamp()-interval '1 second' WHERE organization_id=$1 AND id=$2", current.OrganizationID, current.AttemptID); err != nil {
			t.Fatal(err)
		}
		l, _, err := f.s.ClaimWebhook(ctx, "recovery_worker", time.Second)
		if err != nil || l == nil || l.Token <= current.Token {
			t.Fatal("proven-unsent recovery failed", err)
		}
		current = l
	}
	if _, err := f.s.PrepareWebhookDispatch(ctx, first); err == nil {
		t.Fatal("old worker fence obtained a send permit")
	}
	if _, err := f.s.Pool.Exec(ctx, "UPDATE webhook_attempts SET lease_until=clock_timestamp()-interval '1 second' WHERE organization_id=$1 AND id=$2", current.OrganizationID, current.AttemptID); err != nil {
		t.Fatal(err)
	}
	l, _, err := f.s.ClaimWebhook(ctx, "exhausted_worker", time.Second)
	if err != nil || l != nil {
		t.Fatal("unbounded automatic unsent recovery", err)
	}
	d, err := f.s.WebhookDelivery(ctx, f.p, f.task.ID, current.DeliveryID)
	if err != nil || d.Status != "stopped" || d.AttemptCount != 1 {
		t.Fatal("recovery bound lost original attempt history", err, d.Status)
	}
}
