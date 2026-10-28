// SPDX-License-Identifier: Elastic-2.0
package worker_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/storage"
	"agentplatform.local/platform/internal/worker"
	"github.com/jackc/pgx/v5"
)

// These tests run the actual scheduler, PostgreSQL bridge and HTTP transport.
// The only fake is the paid provider: a local HTTP server with controlled faults.
func workerStore(t *testing.T) *storage.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for real PostgreSQL worker integration")
	}
	ctx := context.Background()
	base, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := domain.NewID("worker_test")
	if _, err = base.Pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		base.Close()
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	store, err := storage.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.Close()
		_, err := base.Pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		base.Close()
		if err != nil {
			t.Error(err)
		}
	})
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store
}

type modelFixture struct {
	store     *storage.Store
	principal domain.Principal
	agent     domain.Agent
	calls     atomic.Int64
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
}

func newModelFixture(t *testing.T) *modelFixture {
	t.Helper()
	f := &modelFixture{store: workerStore(t), entered: make(chan struct{}, 8), release: make(chan struct{})}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer synthetic-test-credential" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.entered <- struct{}{}
		select {
		case <-f.release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"synthetic-response","model":"worker-wire-fixture","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Unverified synthetic candidate"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	t.Cleanup(func() { f.unblock(); provider.Close() })
	ctx := context.Background()
	bootstrap, err := f.store.Bootstrap(ctx, "Worker integration", 100000)
	if err != nil {
		t.Fatal(err)
	}
	f.principal, err = f.store.Authenticate(ctx, bootstrap.Token, bootstrap.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.InstallModelConnection(ctx, domain.ModelConnection{
		ID: "worker-fixture", OrganizationID: bootstrap.OrganizationID, Version: 1, Model: "worker-wire-fixture",
		ProviderAccount: "worker-fixture-account", PriceVersion: "worker-price-v1", Endpoint: provider.URL,
		CredentialEnvironment: "PLATFORM_MODEL_KEY_WORKER_FIXTURE", MaxChargeMicroUSD: 1000,
		MaxInputTokens: 128, InputMicroUSDPerMillion: 1000, OutputMicroUSDPerMillion: 1000, FixedMicroUSD: 1,
		MaxOutputTokens: 32, MaxInputBytes: 4096, MaxOutputBytes: 4096, TimeoutMilliseconds: 5000,
		BoundAssumptions: "Synthetic local HTTP fixture; explicit token limits", TokenLimitField: "max_tokens", AllowInsecureLoopback: true, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	budget := int64(10000)
	raw, err := f.store.CreateAgent(ctx, f.principal, domain.NewID("key"), domain.CreateAgent{Name: "Worker model", Instructions: "Return a candidate", Model: "worker-wire-fixture", BudgetMicroUSD: &budget})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &f.agent); err != nil {
		t.Fatal(err)
	}
	raw, err = f.store.PublishAgent(ctx, f.principal, f.agent.ID, domain.NewID("key"), f.agent.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &f.agent); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *modelFixture) unblock() { f.once.Do(func() { close(f.release) }) }
func (f *modelFixture) task(t *testing.T) domain.Task {
	t.Helper()
	raw, err := f.store.CreateTask(context.Background(), f.principal, domain.NewID("key"), domain.CreateTask{Goal: "Produce one bounded candidate", AgentID: f.agent.ID, BudgetMicroUSD: 10000})
	if err != nil {
		t.Fatal(err)
	}
	var result domain.Task
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func (f *modelFixture) run(t *testing.T, concurrency int) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	w := &worker.Worker{Store: f.store, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ID: domain.NewID("worker"), Concurrency: concurrency, ResolveModelCredential: func(string) (string, error) { return "synthetic-test-credential", nil }}
	go func() { done <- w.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(8 * time.Second):
				t.Error("worker failed to stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}
func (f *modelFixture) waitRequest(t *testing.T) {
	t.Helper()
	select {
	case <-f.entered:
	case <-time.After(8 * time.Second):
		t.Fatal("provider did not receive the request")
	}
}
func await(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("durable expected state did not arrive")
}
func (f *modelFixture) assertHeld(t *testing.T, task domain.Task, wantStatus string, wantArtifacts int) {
	t.Helper()
	ctx := context.Background()
	current, err := f.store.GetTask(ctx, f.principal, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != wantStatus || current.ReservedMicroUSD != 1000 || current.SpentMicroUSD != 0 {
		t.Fatalf("unexpected task state: status=%s reserved=%d spent=%d", current.Status, current.ReservedMicroUSD, current.SpentMicroUSD)
	}
	report, err := f.store.Costs(ctx, f.principal, task.ID, storage.CostCursors{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Reservations) != 1 || report.Reservations[0].Status != "unknown" || report.Reservations[0].ActualMicroUSD != nil || report.UnknownMicroUSD != 1000 {
		t.Fatalf("uncertain request was not retained: %+v", report)
	}
	artifacts, err := f.store.Artifacts(ctx, f.principal, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != wantArtifacts {
		t.Fatalf("unexpected artifacts: %d", len(artifacts))
	}
	if wantArtifacts > 0 && (artifacts[0].Provenance["verified"] != false || artifacts[0].Provenance["verification"] != "awaiting_human") {
		t.Fatal("candidate presented as verified")
	}
}

func TestWorkerConcurrentModelsRetainUnknownCost(t *testing.T) {
	f := newModelFixture(t)
	a, b := f.task(t), f.task(t)
	stop := f.run(t, 2)
	// Both requests must be in flight before either returns. A serial scheduler
	// would time out here instead of passing because it eventually did both jobs.
	f.waitRequest(t)
	f.waitRequest(t)
	f.unblock()
	await(t, func() bool {
		left, e1 := f.store.GetTask(context.Background(), f.principal, a.ID)
		right, e2 := f.store.GetTask(context.Background(), f.principal, b.ID)
		return e1 == nil && e2 == nil && left.Status == "waiting_input" && right.Status == "waiting_input"
	})
	stop()
	f.assertHeld(t, a, "waiting_input", 1)
	f.assertHeld(t, b, "waiting_input", 1)
	if got := f.calls.Load(); got != 2 {
		t.Fatalf("expected one physical request per task, got %d", got)
	}
	// Restarting the scheduler must not turn an unanswered bill into a retry.
	stopRestart := f.run(t, 2)
	time.Sleep(350 * time.Millisecond)
	stopRestart()
	if got := f.calls.Load(); got != 2 {
		t.Fatalf("restart sent another request: %d", got)
	}
}

func TestWorkerCancellationPersistsUnknownAttempt(t *testing.T) {
	f := newModelFixture(t)
	task := f.task(t)
	stop := f.run(t, 2)
	f.waitRequest(t)
	stop()
	f.assertHeld(t, task, "blocked", 0)
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("unexpected provider calls: %d", got)
	}
}

func TestWorkerExpiredLeaseBlocksStaleCandidateWithoutRedispatch(t *testing.T) {
	f := newModelFixture(t)
	task := f.task(t)
	stop := f.run(t, 2)
	f.waitRequest(t)
	_, err := f.store.Pool.Exec(context.Background(), "UPDATE operations SET lease_until=clock_timestamp()-interval '1 second' WHERE organization_id=$1 AND task_id=$2", f.principal.OrganizationID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		current, err := f.store.GetTask(context.Background(), f.principal, task.ID)
		return err == nil && current.Status == "blocked"
	})
	f.unblock()
	stop()
	f.assertHeld(t, task, "blocked", 0)
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("expired lease dispatched again: %d", got)
	}
}

func TestWorkerInvalidConcurrencyRejected(t *testing.T) {
	for _, value := range []int{-1, 65} {
		w := &worker.Worker{Concurrency: value}
		if err := w.Run(context.Background()); err == nil {
			t.Fatalf("accepted concurrency %d", value)
		}
	}
}

func TestWorkerOversizedInputStopsBeforeReservation(t *testing.T) {
	f := newModelFixture(t)
	raw, err := f.store.CreateTask(context.Background(), f.principal, domain.NewID("key"), domain.CreateTask{Goal: strings.Repeat("x", 5000), AgentID: f.agent.ID, BudgetMicroUSD: 10000})
	if err != nil {
		t.Fatal(err)
	}
	var task domain.Task
	if err = json.Unmarshal(raw, &task); err != nil {
		t.Fatal(err)
	}
	stop := f.run(t, 2)
	await(t, func() bool {
		current, err := f.store.GetTask(context.Background(), f.principal, task.ID)
		return err == nil && current.Status == "blocked" && current.WaitingReason == "model_input_rejected"
	})
	stop()
	report, err := f.store.Costs(context.Background(), f.principal, task.ID, storage.CostCursors{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 0 || len(report.Reservations) != 0 || report.ReservedMicroUSD != 0 || report.SpentMicroUSD != 0 {
		t.Fatal("invalid input reached paid execution")
	}
	var attempts int
	if err = f.store.Pool.QueryRow(context.Background(), "SELECT attempts FROM operations WHERE organization_id=$1 AND task_id=$2", f.principal.OrganizationID, task.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("deterministic rejection was retried %d times", attempts)
	}
}
