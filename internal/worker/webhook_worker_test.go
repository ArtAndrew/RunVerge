// SPDX-License-Identifier: Elastic-2.0
package worker_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/worker"
)

func webhookWorkerDecode[T any](t *testing.T, raw []byte, err error) T {
	t.Helper()
	var v T
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestWebhookWorkerAuthorActualSchedulerBoundAndTerminalSource(t *testing.T) {
	s := workerStore(t)
	ctx := context.Background()
	b, err := s.Bootstrap(ctx, "Webhook worker test", 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Authenticate(ctx, b.Token, b.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.CreateAgent(ctx, p, domain.NewID("key"), domain.CreateAgent{Name: "Terminal notification source", Instructions: "Fixture only"})
	a := webhookWorkerDecode[domain.Agent](t, raw, err)
	raw, err = s.PublishAgent(ctx, p, a.ID, domain.NewID("key"), a.Version)
	a = webhookWorkerDecode[domain.Agent](t, raw, err)
	raw, err = s.CreateTask(ctx, p, domain.NewID("key"), domain.CreateTask{AgentID: a.ID, Goal: "Private goal never sent", BudgetMicroUSD: 1000})
	task := webhookWorkerDecode[domain.Task](t, raw, err)
	if _, err = s.CancelTask(ctx, p, task.ID, domain.NewID("key")); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var calls, active, peak atomic.Int64
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		if err != nil || len(body) > 4096 || strings.Contains(string(body), task.Goal) || r.Header.Get("Webhook-Key-ID") != "worker_key_v1" || r.Header.Get("Webhook-Signature") == "" {
			t.Error("worker transport metadata/key binding failed")
			w.WriteHeader(400)
			return
		}
		entered <- struct{}{}
		select {
		case <-release:
			w.WriteHeader(204)
		case <-r.Context().Done():
			return
		}
	}))
	t.Cleanup(peer.Close)
	profile := domain.WebhookDestinationProfile{Destination: domain.WebhookDestination{ID: "worker_receiver", OrganizationID: p.OrganizationID, Version: 1, Name: "Worker local TLS fixture", SigningKeyID: "worker_key_v1", UnmeteredApproved: true, Active: true}, Endpoint: peer.URL, SigningKeyEnvironment: "PLATFORM_WEBHOOK_KEY_WORKER", TrustedCAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: peer.Certificate().Raw})), AllowPrivateNetwork: true, TimeoutSeconds: 3}
	if _, err = s.InstallWebhookDestination(ctx, profile); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateWebhookSubscription(ctx, p, task.ID, domain.NewID("key"), domain.CreateWebhookSubscription{DestinationID: profile.Destination.ID, DestinationVersion: 1, EventTypes: []string{"material.created"}, TTLSeconds: 60, MaxNotifications: 8, AcknowledgeMetadataExport: true, Reason: "Explicit terminal Task metadata subscription"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err = s.CreateMaterial(ctx, p, task.ID, domain.NewID("key"), domain.CreateMaterial{Name: "Private draft", MediaType: "text/plain", Content: "NEVER_EXPORTED_WORKER_CONTENT"}); err != nil {
			t.Fatal(err)
		}
	}
	encoded := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{17}, 32))
	var keyLookups atomic.Int64
	var logs bytes.Buffer
	newWorker := func(enabled bool) *worker.Worker {
		return &worker.Worker{Store: s, Concurrency: 2, EnableWebhooks: enabled, Log: slog.New(slog.NewTextHandler(&logs, nil)), ResolveWebhookSigningKey: func(name string) (string, error) {
			if name != "PLATFORM_WEBHOOK_KEY_WORKER" {
				t.Error("worker resolved an unapproved reference")
				return "", domain.Forbidden
			}
			keyLookups.Add(1)
			return encoded, nil
		}}
	}
	// Existing0.13 workers remain unaffected until the internal prerelease
	// switch is explicitly enabled; no new public feature is advertised yet.
	off, stopOff := context.WithCancel(ctx)
	offDone := make(chan error, 1)
	go func() { offDone <- newWorker(false).Run(off) }()
	time.Sleep(250 * time.Millisecond)
	stopOff()
	if err = <-offDone; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || keyLookups.Load() != 0 {
		t.Fatal("disabled feature resolved keys/dispatched")
	}
	run, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- newWorker(true).Run(run) }()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("bounded worker never dispatched")
		}
	}
	select {
	case <-entered:
		t.Fatal("third request exceeded two shared worker slots")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	deadline := time.Now().Add(4 * time.Second)
	for {
		var acked int
		if err = s.Pool.QueryRow(ctx, "SELECT count(*) FROM webhook_deliveries WHERE organization_id=$1 AND status='acknowledged'", p.OrganizationID).Scan(&acked); err != nil {
			t.Fatal(err)
		}
		if acked == 6 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("actual worker did not acknowledge all notifications", acked)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if peak.Load() > 2 || calls.Load() != 6 || keyLookups.Load() != 6 {
		t.Fatal("worker concurrency/request count changed", peak.Load(), calls.Load(), keyLookups.Load())
	}
	current, err := s.GetTask(ctx, p, task.ID)
	if err != nil || current.Status != "cancelled" || current.SpentMicroUSD != 0 || current.ReservedMicroUSD != 0 {
		t.Fatal("notification changed source Task or monetary state", err)
	}
	if strings.Contains(logs.String(), encoded) || strings.Contains(logs.String(), peer.URL) || strings.Contains(logs.String(), task.Goal) {
		t.Fatal("worker logs leaked private request configuration")
	}
	t.Log("actual scheduler/TLS/PostgreSQL:6 receipts,peak2 shared slots,terminal source preserved,no notification financial effect")
}
