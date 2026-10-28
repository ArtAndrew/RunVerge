// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for real PostgreSQL integration")
	}
	ctx := context.Background()
	base, e := Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := domain.NewID("test")
	if _, e = base.Pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	isolated := u.String()
	s, e := Open(ctx, isolated)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		s.Close()
		_, e := base.Pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		base.Close()
		if e != nil {
			t.Error(e)
		}
	})
	return s, isolated
}
func identity(t *testing.T, s *Store, budget int64) (domain.Principal, Bootstrap) {
	t.Helper()
	b, e := s.Bootstrap(context.Background(), "Test organization", budget)
	if e != nil {
		t.Fatal(e)
	}
	p, e := s.Authenticate(context.Background(), b.Token, b.OrganizationID)
	if e != nil {
		t.Fatal(e)
	}
	return p, b
}
func agent(t *testing.T, s *Store, p domain.Principal) domain.Agent {
	t.Helper()
	value := int64(1_000_000)
	raw, e := s.CreateAgent(context.Background(), p, domain.NewID("key"), domain.CreateAgent{Name: "Fixture", Instructions: "Echo for testing", BudgetMicroUSD: &value})
	if e != nil {
		t.Fatal(e)
	}
	var a domain.Agent
	if e = json.Unmarshal(raw, &a); e != nil {
		t.Fatal(e)
	}
	raw, e = s.PublishAgent(context.Background(), p, a.ID, domain.NewID("key"), a.Version)
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &a); e != nil {
		t.Fatal(e)
	}
	return a
}
func task(t *testing.T, s *Store, p domain.Principal, a domain.Agent) domain.Task {
	t.Helper()
	raw, e := s.CreateTask(context.Background(), p, domain.NewID("key"), domain.CreateTask{Goal: "A bounded test", AgentID: a.ID, BudgetMicroUSD: 10_000})
	if e != nil {
		t.Fatal(e)
	}
	var v domain.Task
	if e = json.Unmarshal(raw, &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func code(t *testing.T, e error, want string) {
	t.Helper()
	var v *domain.Error
	if !errors.As(e, &v) || v.Code != want {
		t.Fatalf("wanted error %s, got %v", want, e)
	}
}

func TestTenantIsolationAndDurableIdempotency(t *testing.T) {
	s, dsn := testStore(t)
	ctx := context.Background()
	pa, ba := identity(t, s, 1_000_000)
	pb, _ := identity(t, s, 1_000_000)
	a := agent(t, s, pa)
	in := domain.CreateTask{Goal: "Persist me", AgentID: a.ID, BudgetMicroUSD: 10_000}
	key := domain.NewID("key")
	const n = 12
	var wg sync.WaitGroup
	ids := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, e := s.CreateTask(ctx, pa, key, in)
			if e != nil {
				errs <- e
				return
			}
			var x domain.Task
			if e = json.Unmarshal(raw, &x); e != nil {
				errs <- e
				return
			}
			ids <- x.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	id := ""
	for current := range ids {
		if id == "" {
			id = current
		}
		if current != id {
			t.Fatal("duplicate logical task")
		}
	}
	in.Goal = "Changed"
	_, e := s.CreateTask(ctx, pa, key, in)
	code(t, e, "conflict")
	_, e = s.GetTask(ctx, pb, id)
	code(t, e, "not_found")
	_, _, e = s.Events(ctx, pb, id, 0, 50)
	code(t, e, "not_found")
	_, e = s.Artifacts(ctx, pb, id)
	code(t, e, "not_found")
	_, e = s.CreateTask(ctx, pb, key, domain.CreateTask{Goal: "steal agent", AgentID: a.ID, BudgetMicroUSD: 1000})
	code(t, e, "not_found")
	_, e = s.Authenticate(ctx, ba.Token, pb.OrganizationID)
	code(t, e, "unauthenticated")
	other, e := Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	v, e := other.GetTask(ctx, pa, id)
	if e != nil || v.Status != "queued" {
		t.Fatalf("not durable: %v %v", v, e)
	}
	rows, _, e := s.ListTasks(ctx, pa, "", 50)
	if e != nil || len(rows) != 1 {
		t.Fatalf("wanted 1 task: %d %v", len(rows), e)
	}
	var count int
	e = s.Pool.QueryRow(ctx, "SELECT count(*) FROM outbox").Scan(&count)
	if e != nil || count != 1 {
		t.Fatalf("state/event/outbox mismatch: %d %v", count, e)
	}
}
func TestFencingAndSingleSettlement(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 100_000)
	a := agent(t, s, p)
	v := task(t, s, p, a)
	first, e := s.ClaimFixture(ctx, "worker-one", time.Minute)
	if e != nil || first == nil {
		t.Fatalf("claim: %v", e)
	}
	if _, e = s.Pool.Exec(ctx, "UPDATE operations SET lease_until=now()-interval '1 second' WHERE organization_id=$1 AND id=$2", p.OrganizationID, first.OperationID); e != nil {
		t.Fatal(e)
	}
	second, e := s.ClaimFixture(ctx, "worker-two", time.Minute)
	if e != nil || second == nil {
		t.Fatalf("reclaim: %v", e)
	}
	if second.Token <= first.Token {
		t.Fatal("fence did not increase")
	}
	output := "Тестовый результат\n" + v.Goal
	code(t, s.CompleteFixture(ctx, first, output), "stale_lease")
	code(t, s.CompleteFixture(ctx, second, "model claims success"), "invalid_request")
	if e = s.CompleteFixture(ctx, second, output); e != nil {
		t.Fatal(e)
	}
	code(t, s.CompleteFixture(ctx, second, output), "stale_lease")
	got, e := s.GetTask(ctx, p, v.ID)
	if e != nil || got.Status != "succeeded" || got.SpentMicroUSD != domain.FixtureCost || got.ReservedMicroUSD != 0 {
		t.Fatalf("bad settled task: %+v %v", got, e)
	}
	artifacts, e := s.Artifacts(ctx, p, v.ID)
	if e != nil || len(artifacts) != 1 {
		t.Fatalf("wanted one artifact: %d %v", len(artifacts), e)
	}
}
func TestConcurrentBudgetReservations(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, domain.FixtureCost)
	a := agent(t, s, p)
	for i := 0; i < 12; i++ {
		task(t, s, p, a)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	leases := []*domain.Lease{}
	errs := []error{}
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, e := s.ClaimFixture(ctx, domain.NewID("worker"), time.Minute)
			mu.Lock()
			defer mu.Unlock()
			if e != nil {
				errs = append(errs, e)
			}
			if l != nil {
				leases = append(leases, l)
			}
		}()
	}
	wg.Wait()
	for _, e := range errs {
		t.Error(e)
	}
	if len(leases) != 1 {
		t.Fatalf("wanted 1 funded lease, got %d", len(leases))
	}
	var reserved, spent int64
	if e := s.Pool.QueryRow(ctx, "SELECT reserved_microusd,spent_microusd FROM organizations WHERE id=$1", p.OrganizationID).Scan(&reserved, &spent); e != nil {
		t.Fatal(e)
	}
	if reserved != domain.FixtureCost || spent != 0 {
		t.Fatalf("budget bypass: reserved=%d spent=%d", reserved, spent)
	}
	l := leases[0]
	if e := s.CompleteFixture(ctx, l, "Тестовый результат\n"+l.Task.Goal); e != nil {
		t.Fatal(e)
	}
}
func TestCancellationRetainsInFlightReservation(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 100_000)
	a := agent(t, s, p)
	v := task(t, s, p, a)
	l, e := s.ClaimFixture(ctx, "worker", time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.CancelTask(ctx, p, v.ID, "cancel-1"); e != nil {
		t.Fatal(e)
	}
	got, e := s.GetTask(ctx, p, v.ID)
	if e != nil || got.ReservedMicroUSD != domain.FixtureCost || got.Status != "cancelled" {
		t.Fatalf("in-flight reserve lost: %+v %v", got, e)
	}
	code(t, s.CheckLease(ctx, l), "stale_lease")
	// 0.4 fences a cancelled operation. A stale worker cannot complete its
	// result or accounting; exposure must remain until explicit reconciliation.
	code(t, s.CompleteFixture(ctx, l, "Тестовый результат\n"+v.Goal), "stale_lease")
	got, e = s.GetTask(ctx, p, v.ID)
	if e != nil || got.Status != "cancelled" || got.ReservedMicroUSD != domain.FixtureCost || got.SpentMicroUSD != 0 {
		t.Fatalf("stale completion changed cancellation exposure: %+v %v", got, e)
	}
	// This recovery is valid only for the deterministic synthetic fixture.
	// Unknown model/provider costs do not use this reconciliation policy.
	if reconciled, err := s.ReconcileFixtureReservation(ctx); err != nil || !reconciled {
		t.Fatalf("fixture accounting not reconciled: %v", err)
	}
	got, e = s.GetTask(ctx, p, v.ID)
	if e != nil || got.Status != "cancelled" || got.ReservedMicroUSD != 0 || got.SpentMicroUSD != domain.FixtureCost {
		t.Fatalf("bad cancellation accounting: %+v %v", got, e)
	}
	if reconciled, err := s.ReconcileFixtureReservation(ctx); err != nil || reconciled {
		t.Fatalf("fixture cancellation charged twice: %v", err)
	}
	artifacts, e := s.Artifacts(ctx, p, v.ID)
	if e != nil || len(artifacts) != 0 {
		t.Fatal("cancelled run published result")
	}
}
func TestRevocationAndRoleRecheck(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	p, b := identity(t, s, 100_000)
	a := agent(t, s, p)
	v := task(t, s, p, a)
	if _, e := s.Pool.Exec(ctx, "UPDATE memberships SET role='viewer' WHERE organization_id=$1 AND principal_id=$2", p.OrganizationID, p.ID); e != nil {
		t.Fatal(e)
	}
	_, e := s.CreateTask(ctx, p, "new", domain.CreateTask{Goal: "should fail", AgentID: a.ID, BudgetMicroUSD: 1000})
	code(t, e, "forbidden")
	l, e := s.ClaimFixture(ctx, "worker", time.Minute)
	if e != nil || l != nil {
		t.Fatalf("revoked role dispatched: %v %v", l, e)
	}
	got, e := s.GetTask(ctx, p, v.ID)
	if e != nil || got.Status != "blocked" || got.WaitingReason != "authorization_revoked" {
		t.Fatalf("not blocked: %+v %v", got, e)
	}
	if _, e = s.Pool.Exec(ctx, "UPDATE access_tokens SET revoked=true WHERE token_hash=$1", Hash([]byte(b.Token))); e != nil {
		t.Fatal(e)
	}
	_, e = s.Authenticate(ctx, b.Token, b.OrganizationID)
	code(t, e, "unauthenticated")
}
func TestUntrustedLeaseCannotVerifyForgedGoal(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 100_000)
	a := agent(t, s, p)
	v := task(t, s, p, a)
	other := task(t, s, p, a)
	l, e := s.ClaimFixture(ctx, "worker", time.Minute)
	if e != nil || l == nil {
		t.Fatal(e)
	}
	l.Task.Goal = "forged goal"
	code(t, s.CompleteFixture(ctx, l, "Тестовый результат\nforged goal"), "invalid_request")
	savedID := l.TaskID
	l.TaskID = other.ID
	code(t, s.CompleteFixture(ctx, l, "Тестовый результат\n"+other.Goal), "stale_lease")
	l.TaskID = savedID
	if e = s.CompleteFixture(ctx, l, "Тестовый результат\n"+v.Goal); e != nil {
		t.Fatal(e)
	}
}
func TestCancelledLostWorkerReconciliation(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 100_000)
	a := agent(t, s, p)
	v := task(t, s, p, a)
	l, e := s.ClaimFixture(ctx, "lost-worker", time.Minute)
	if e != nil || l == nil {
		t.Fatal(e)
	}
	if _, e = s.CancelTask(ctx, p, v.ID, "cancel"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Pool.Exec(ctx, "UPDATE operations SET lease_until=now()-interval '1 second' WHERE organization_id=$1 AND id=$2", p.OrganizationID, l.OperationID); e != nil {
		t.Fatal(e)
	}
	did, e := s.ReconcileFixtureReservation(ctx)
	if e != nil || !did {
		t.Fatalf("not reconciled: %v %v", did, e)
	}
	did, e = s.ReconcileFixtureReservation(ctx)
	if e != nil || did {
		t.Fatalf("duplicate reconciliation: %v %v", did, e)
	}
	got, e := s.GetTask(ctx, p, v.ID)
	if e != nil || got.Status != "cancelled" || got.ReservedMicroUSD != 0 || got.SpentMicroUSD != domain.FixtureCost {
		t.Fatalf("bad reconciliation: %+v %v", got, e)
	}
	code(t, s.CompleteFixture(ctx, l, "Тестовый результат\n"+v.Goal), "stale_lease")
}
func TestCancelAndClaimDoNotDeadlock(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, _ := identity(t, s, 1_000_000)
	a := agent(t, s, p)
	for i := 0; i < 20; i++ {
		v := task(t, s, p, a)
		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() { <-start; _, e := s.CancelTask(ctx, p, v.ID, domain.NewID("cancel")); errs <- e }()
		go func() { <-start; _, e := s.ClaimFixture(ctx, domain.NewID("worker"), time.Minute); errs <- e }()
		close(start)
		for j := 0; j < 2; j++ {
			if e := <-errs; e != nil {
				t.Fatalf("concurrent cancellation failed: %v", e)
			}
		}
	}
}

func TestCrashChild(t *testing.T) {
	mode := os.Getenv("AP_STORAGE_CRASH_CHILD")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	ctx := context.Background()
	s, e := Open(ctx, os.Getenv("AP_CRASH_DSN"))
	if e != nil {
		t.Fatal("child database open failed")
	}
	defer s.Close()
	var p domain.Principal
	if e = json.Unmarshal([]byte(os.Getenv("AP_CRASH_PRINCIPAL")), &p); e != nil {
		t.Fatal(e)
	}
	if mode == "after_commit" {
		_, e = s.CreateTask(ctx, p, "crash-key", domain.CreateTask{Goal: "Survive process termination", AgentID: os.Getenv("AP_CRASH_AGENT"), BudgetMicroUSD: 10000})
		if e != nil {
			t.Fatal(e)
		}
	} else {
		tx, e := s.Pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		// Intentionally uncommitted state, event and outbox. The parent kills this
		// process after the barrier to verify rollback by the actual database.
		id := os.Getenv("AP_CRASH_TASK")
		_, e = tx.Exec(ctx, `INSERT INTO tasks(organization_id,id,owner_id,agent_id,agent_version,goal,status,budget_microusd) VALUES($1,$2,$3,$4,1,'Uncommitted','queued',10000)`, p.OrganizationID, id, p.ID, os.Getenv("AP_CRASH_AGENT"))
		if e != nil {
			t.Fatal(e)
		}
		// The event remains deliberately uncommitted, but still exercises the
		// registered writer payload rather than bypassing its new type checks.
		if e = emit(ctx, tx, p.OrganizationID, id, "task.created", map[string]any{"run_id": domain.NewID("run"), "agent_version": 1}); e != nil {
			t.Fatal(e)
		}
	}
	if e = os.WriteFile(os.Getenv("AP_CRASH_BARRIER"), []byte("ready"), 0600); e != nil {
		t.Fatal(e)
	}
	// The transaction in before_commit must stay alive until SIGKILL. Rollback
	// defer is deliberately prevented by process termination.
	for {
		time.Sleep(time.Second)
	}
}
func TestProcessCrashBeforeAndAfterCommit(t *testing.T) {
	s, dsn := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 100_000)
	a := agent(t, s, p)
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			barrier := filepath.Join(t.TempDir(), "barrier")
			id := domain.NewID("tsk")
			principal, _ := json.Marshal(p)
			command := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.timeout=20s")
			command.Env = append(os.Environ(), "AP_STORAGE_CRASH_CHILD="+mode, "AP_CRASH_DSN="+dsn, "AP_CRASH_PRINCIPAL="+string(principal), "AP_CRASH_AGENT="+a.ID, "AP_CRASH_TASK="+id, "AP_CRASH_BARRIER="+barrier)
			if e := command.Start(); e != nil {
				t.Fatal(e)
			}
			defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, e := os.Stat(barrier); e == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("child did not reach crash barrier")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if e := command.Process.Kill(); e != nil {
				t.Fatal(e)
			}
			_ = command.Wait()
			if mode == "before_commit" {
				_, e := s.GetTask(ctx, p, id)
				code(t, e, "not_found")
				var n int
				if e = s.Pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE organization_id=$1 AND task_id=$2", p.OrganizationID, id).Scan(&n); e != nil || n != 0 {
					t.Fatal("uncommitted event survived")
				}
			} else {
				list, _, e := s.ListTasks(ctx, p, "", 50)
				if e != nil || len(list) != 1 {
					t.Fatalf("committed task lost: %d %v", len(list), e)
				}
				replay, e := s.CreateTask(ctx, p, "crash-key", domain.CreateTask{Goal: "Survive process termination", AgentID: a.ID, BudgetMicroUSD: 10000})
				if e != nil {
					t.Fatal(e)
				}
				var task domain.Task
				if e = json.Unmarshal(replay, &task); e != nil || task.ID != list[0].ID {
					t.Fatal("retry after crash duplicated command")
				}
				var n int
				if e = s.Pool.QueryRow(ctx, "SELECT count(*) FROM outbox WHERE organization_id=$1", p.OrganizationID).Scan(&n); e != nil || n != 1 {
					t.Fatal("committed outbox missing")
				}
			}
		})
	}
}

func TestAdmissionLimitIsAtomic(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 100000)
	a := agent(t, s, p)
	if _, e := s.Pool.Exec(ctx, "UPDATE organizations SET pending_task_limit=1 WHERE id=$1", p.OrganizationID); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.CreateTask(ctx, p, domain.NewID("key"), domain.CreateTask{Goal: "bounded queue", AgentID: a.ID, BudgetMicroUSD: 1000})
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		} else {
			code(t, e, "unavailable")
		}
	}
	if success != 1 {
		t.Fatalf("admission bypass: %d", success)
	}
}

func waitForBlockedQuery(t *testing.T, s *Store, prefix string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var count int
		e := s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE $1", prefix+"%").Scan(&count)
		if e != nil {
			t.Fatal(e)
		}
		if count > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("expected forced lock barrier not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func TestForcedCancelClaimLockOrder(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, _ := identity(t, s, 100000)
	a := agent(t, s, p)
	v := task(t, s, p, a)
	blocker, e := s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback(ctx)
	if _, e = blocker.Exec(ctx, "SELECT id FROM organizations WHERE id=$1 FOR UPDATE", p.OrganizationID); e != nil {
		t.Fatal(e)
	}
	claimed := make(chan error, 1)
	go func() { _, e := s.ClaimFixture(ctx, "forced-worker", time.Minute); claimed <- e }()
	waitForBlockedQuery(t, s, "SELECT budget_microusd,reserved_microusd,spent_microusd,active")
	cancelled := make(chan error, 1)
	go func() { _, e := s.CancelTask(ctx, p, v.ID, "forced-cancel"); cancelled <- e }()
	waitForBlockedQuery(t, s, "SELECT o.id FROM operations o JOIN tasks")
	// Claim owns the operation and waits for org. Correct cancel waits for the
	// operation without holding task. The old task->op order deadlocks here.
	if e = blocker.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-claimed; e != nil {
		t.Fatal(e)
	}
	if e = <-cancelled; e != nil {
		t.Fatal(e)
	}
	got, e := s.GetTask(ctx, p, v.ID)
	if e != nil || got.Status != "cancelled" {
		t.Fatalf("cancel lost: %+v %v", got, e)
	}
}

func TestEventCursorCannotOvertakeUncommittedEvent(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, _ := identity(t, s, 100000)
	a := agent(t, s, p)
	one := task(t, s, p, a)
	two := task(t, s, p, a)
	first, e := s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer first.Rollback(ctx)
	if e = emit(ctx, first, p.OrganizationID, one.ID, "task.blocked", map[string]any{"reason": "barrier.first"}); e != nil {
		t.Fatal(e)
	}
	second, e := s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer second.Rollback(ctx)
	done := make(chan error, 1)
	go func() {
		if e := emit(ctx, second, p.OrganizationID, two.ID, "task.blocked", map[string]any{"reason": "barrier.second"}); e != nil {
			done <- e
			return
		}
		done <- second.Commit(ctx)
	}()
	waitForBlockedQuery(t, s, "SELECT id FROM organizations WHERE id=")
	var count int
	if e = s.Pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE organization_id=$1 AND type='task.blocked' AND data->>'reason' LIKE 'barrier.%'", p.OrganizationID).Scan(&count); e != nil || count != 0 {
		t.Fatal("later cursor became visible too early")
	}
	if e = first.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	var before, after int64
	if e = s.Pool.QueryRow(ctx, "SELECT min(sequence),max(sequence) FROM events WHERE organization_id=$1 AND type='task.blocked' AND data->>'reason' LIKE 'barrier.%'", p.OrganizationID).Scan(&before, &after); e != nil || before >= after {
		t.Fatal("event order invalid")
	}
}
