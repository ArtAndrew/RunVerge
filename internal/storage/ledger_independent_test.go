// SPDX-License-Identifier: Elastic-2.0
package storage

// Independent ledger review tests. These use authenticated-cost *fixtures* and
// real PostgreSQL; they do not claim verification of any external provider bill.
import (
	"context"
	"sync"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
)

// Build a lease directly so these tests never rely on fixture:echo's synthetic
// reservation or its special recovery policy as a real-cost charging contract.
func independentLease(t *testing.T, s *Store, p domain.Principal, a domain.Agent, parent string, budget int64) *domain.Lease {
	t.Helper()
	ctx := context.Background()
	l := &domain.Lease{OrganizationID: p.OrganizationID, TaskID: domain.NewID("tsk"), RunID: domain.NewID("run"), OperationID: domain.NewID("op"), OwnerID: "independent-ledger-worker", Token: 1}
	var parentValue any
	if parent != "" {
		parentValue = parent
	}
	if _, e := s.Pool.Exec(ctx, `INSERT INTO tasks(organization_id,id,owner_id,agent_id,agent_version,goal,status,budget_microusd,parent_id) VALUES($1,$2,$3,$4,$5,'Independent ledger test','running',$6,$7)`, p.OrganizationID, l.TaskID, p.ID, a.ID, a.Version, budget, parentValue); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Pool.Exec(ctx, `INSERT INTO runs(organization_id,id,task_id,principal_id,agent_id,agent_version,status) VALUES($1,$2,$3,$4,$5,$6,'running')`, p.OrganizationID, l.RunID, l.TaskID, p.ID, a.ID, a.Version); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Pool.Exec(ctx, `INSERT INTO operations(organization_id,id,task_id,run_id,status,lease_owner,lease_until,fencing_token) VALUES($1,$2,$3,$4,'leased',$5,clock_timestamp()+interval '5 minutes',1)`, p.OrganizationID, l.OperationID, l.TaskID, l.RunID, l.OwnerID); e != nil {
		t.Fatal(e)
	}
	return l
}
func independentReserveInput(amount int64) domain.ReserveCostRequest {
	return domain.ReserveCostRequest{ChargeKey: domain.NewID("attempt"), Category: "model", Provider: "test-provider-account-A", PriceVersion: "test-final-usd-v1", AmountMicroUSD: amount, IntentHash: Hash([]byte("independent immutable request"))}
}
func independentReserve(t *testing.T, s *Store, l *domain.Lease, amount int64) domain.Reservation {
	t.Helper()
	r, e := s.ReserveCost(context.Background(), l, independentReserveInput(amount))
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func independentEvidence(amount int64) domain.CostEvidence {
	return domain.CostEvidence{Source: "test-provider-account-A", Reference: domain.NewID("cost"), PayloadDigest: Hash([]byte("trusted fixture final statement")), ActualMicroUSD: amount, Final: true, Currency: "USD"}
}
func independentBalances(t *testing.T, s *Store, org string, tasks []string, held, spent int64) {
	t.Helper()
	ctx := context.Background()
	var h, v int64
	if e := s.Pool.QueryRow(ctx, "SELECT reserved_microusd,spent_microusd FROM organizations WHERE id=$1", org).Scan(&h, &v); e != nil {
		t.Fatal(e)
	}
	if h != held || v != spent {
		t.Fatalf("organization balance held=%d spent=%d, want %d/%d", h, v, held, spent)
	}
	for _, id := range tasks {
		if e := s.Pool.QueryRow(ctx, "SELECT reserved_microusd,spent_microusd FROM tasks WHERE organization_id=$1 AND id=$2", org, id).Scan(&h, &v); e != nil {
			t.Fatal(e)
		}
		if h != held || v != spent {
			t.Fatalf("task %s balance held=%d spent=%d, want %d/%d", id, h, v, held, spent)
		}
	}
	if e := s.Pool.QueryRow(ctx, "SELECT COALESCE(sum(delta_reserved_microusd),0),COALESCE(sum(delta_spent_microusd),0) FROM ledger_entries WHERE organization_id=$1", org).Scan(&h, &v); e != nil {
		t.Fatal(e)
	}
	if h != held || v != spent {
		t.Fatalf("journal/projection divergence %d/%d vs %d/%d", h, v, held, spent)
	}
}

func TestIndependentLedgerAncestorAdmissionAndFrozenScopes(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 5000)
	a := agent(t, s, p)
	root := independentLease(t, s, p, a, "", 1000)
	left := independentLease(t, s, p, a, root.TaskID, 700)
	right := independentLease(t, s, p, a, root.TaskID, 700)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, l := range []*domain.Lease{left, right} {
		go func(l *domain.Lease) {
			<-start
			_, e := s.ReserveCost(ctx, l, independentReserveInput(600))
			results <- e
		}(l)
	}
	close(start)
	success := 0
	for range 2 {
		if e := <-results; e == nil {
			success++
		} else {
			code(t, e, "budget_exceeded")
		}
	}
	if success != 1 {
		t.Fatalf("sibling oversubscription: %d successes", success)
	}
	independentBalances(t, s, p.OrganizationID, []string{root.TaskID}, 600, 0)
	var childHeld int64
	if e := s.Pool.QueryRow(ctx, "SELECT sum(reserved_microusd) FROM tasks WHERE organization_id=$1 AND parent_id=$2", p.OrganizationID, root.TaskID).Scan(&childHeld); e != nil || childHeld != 600 {
		t.Fatalf("children hold=%d: %v", childHeld, e)
	}
	if _, e := s.Pool.Exec(ctx, "UPDATE tasks SET parent_id=NULL WHERE organization_id=$1 AND id=$2", p.OrganizationID, left.TaskID); e == nil {
		t.Fatal("reparented immutable lineage")
	}
	if _, e := s.Pool.Exec(ctx, "UPDATE reservations SET scope_task_ids=ARRAY[task_id] WHERE organization_id=$1", p.OrganizationID); e == nil {
		t.Fatal("rewrote frozen accounting scopes")
	}
	if _, e := s.Pool.Exec(ctx, "UPDATE tasks SET budget_microusd=500 WHERE organization_id=$1 AND id=$2", p.OrganizationID, root.TaskID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ReserveCost(ctx, left, independentReserveInput(1)); e == nil {
		t.Fatal("admitted below existing exposure")
	}
	independentBalances(t, s, p.OrganizationID, []string{root.TaskID}, 600, 0)
}

func TestIndependentLedgerUnknownHeldAndReaperExcluded(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 1000)
	a := agent(t, s, p)
	l := independentLease(t, s, p, a, "", 1000)
	r := independentReserve(t, s, l, 800)
	if e := s.PrepareCostDispatch(ctx, l, r.ID); e != nil {
		t.Fatal(e)
	}
	for range 2 {
		if _, e := s.MarkCostUnknown(ctx, p.OrganizationID, r.ID, "transport-1", "response lost"); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.PrepareCostDispatch(ctx, l, r.ID); e == nil {
		t.Fatal("redispatched unknown attempt")
	}
	if _, e := s.ReleaseUnsentCost(ctx, p.OrganizationID, r.ID, "unsafe-release"); e == nil {
		t.Fatal("released dispatched unknown")
	}
	if _, e := s.CancelTask(ctx, p, l.TaskID, "cancel-unknown"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Pool.Exec(ctx, "UPDATE operations SET lease_until=clock_timestamp()-interval '1 second' WHERE organization_id=$1 AND id=$2", p.OrganizationID, l.OperationID); e != nil {
		t.Fatal(e)
	}
	if did, e := s.ReconcileFixtureReservation(ctx); e != nil || did {
		t.Fatalf("fixture reaper touched real-cost fixture: did=%v err=%v", did, e)
	}
	independentBalances(t, s, p.OrganizationID, []string{l.TaskID}, 800, 0)
	if _, e := s.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", p.OrganizationID, p.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Pool.Exec(ctx, "UPDATE organizations SET active=false WHERE id=$1", p.OrganizationID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.SettleCost(ctx, p.OrganizationID, r.ID, independentEvidence(650)); e != nil {
		t.Fatal(e)
	}
	independentBalances(t, s, p.OrganizationID, []string{l.TaskID}, 0, 650)
	// This fixture deliberately revoked both the member and organization.
	// Fresh public reads must deny that credential; trusted accounting facts
	// are inspected directly without reviving access or changing the fixture.
	_, e := s.GetTask(ctx, p, l.TaskID)
	code(t, e, "unauthenticated")
	var status, result string
	e = s.Pool.QueryRow(ctx, "SELECT status,result FROM tasks WHERE organization_id=$1 AND id=$2", p.OrganizationID, l.TaskID).Scan(&status, &result)
	if e != nil || status != "cancelled" || result != "" {
		t.Fatalf("accounting resurrected cancelled task: status=%s result=%q err=%v", status, result, e)
	}
}

func TestIndependentLedgerOverageAndLateCharge(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "overage", true: "released_late_charge"}[late], func(t *testing.T) {
			s, _ := testStore(t)
			ctx := context.Background()
			p, _ := identity(t, s, 100)
			a := agent(t, s, p)
			l := independentLease(t, s, p, a, "", 100)
			r := independentReserve(t, s, l, 80)
			if late {
				if _, e := s.ReleaseUnsentCost(ctx, p.OrganizationID, r.ID, "cancel-before-send"); e != nil {
					t.Fatal(e)
				}
			} else if e := s.PrepareCostDispatch(ctx, l, r.ID); e != nil {
				t.Fatal(e)
			}
			evidence := independentEvidence(130)
			for range 2 {
				got, e := s.SettleCost(ctx, p.OrganizationID, r.ID, evidence)
				if e != nil || got.ActualMicroUSD == nil || *got.ActualMicroUSD != 130 {
					t.Fatalf("lost full actual: %+v %v", got, e)
				}
			}
			independentBalances(t, s, p.OrganizationID, []string{l.TaskID}, 0, 130)
			if _, e := s.ReserveCost(ctx, l, independentReserveInput(0)); e == nil {
				t.Fatal("admitted after financial hold")
			}
			var n int
			if e := s.Pool.QueryRow(ctx, "SELECT count(*) FROM financial_incidents WHERE organization_id=$1 AND reservation_id=$2", p.OrganizationID, r.ID).Scan(&n); e != nil || n != 1 {
				t.Fatalf("incident count=%d: %v", n, e)
			}
		})
	}
}

func TestIndependentLedgerEconomicReplayAndCorrections(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 10000)
	a := agent(t, s, p)
	l := independentLease(t, s, p, a, "", 10000)
	in := independentReserveInput(100)
	const n = 12
	var wg sync.WaitGroup
	errors := make(chan error, n)
	ids := make(chan string, n)
	for range n {
		wg.Add(1)
		go func() { defer wg.Done(); r, e := s.ReserveCost(ctx, l, in); errors <- e; ids <- r.ID }()
	}
	wg.Wait()
	close(errors)
	close(ids)
	for e := range errors {
		if e != nil {
			t.Fatal(e)
		}
	}
	id := ""
	for got := range ids {
		if id == "" {
			id = got
		}
		if got != id {
			t.Fatal("reserve replay created new charge")
		}
	}
	changed := in
	changed.AmountMicroUSD = 101
	if _, e := s.ReserveCost(ctx, l, changed); e == nil {
		t.Fatal("changed reserve replay accepted")
	}
	if e := s.PrepareCostDispatch(ctx, l, id); e != nil {
		t.Fatal(e)
	}
	evidence := independentEvidence(60)
	errors = make(chan error, n)
	for range n {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := s.SettleCost(ctx, p.OrganizationID, id, evidence); errors <- e }()
	}
	wg.Wait()
	close(errors)
	for e := range errors {
		if e != nil {
			t.Fatal(e)
		}
	}
	independentBalances(t, s, p.OrganizationID, []string{l.TaskID}, 0, 60)
	changedEvidence := evidence
	changedEvidence.ActualMicroUSD = 70
	if _, e := s.SettleCost(ctx, p.OrganizationID, id, changedEvidence); e == nil {
		t.Fatal("changed final fact accepted")
	}
	if _, e := s.SettleCost(ctx, p.OrganizationID, id, independentEvidence(60)); e == nil {
		t.Fatal("new evidence silently re-settled charge")
	}
	r, e := scanReservation(s.Pool.QueryRow(ctx, "SELECT "+reservationColumns+" FROM reservations WHERE organization_id=$1 AND id=$2", p.OrganizationID, id))
	if e != nil {
		t.Fatal(e)
	}
	correction := independentEvidence(40)
	correction.Reason = "trusted provider correction"
	adjust := domain.CostAdjustment{ExpectedRevision: r.Revision, Evidence: correction}
	for range 2 {
		if _, e := s.AdjustCost(ctx, p.OrganizationID, id, adjust); e != nil {
			t.Fatal(e)
		}
	}
	independentBalances(t, s, p.OrganizationID, []string{l.TaskID}, 0, 40)
	stale := adjust
	stale.Evidence = independentEvidence(30)
	stale.Evidence.Reason = "stale correction"
	if _, e := s.AdjustCost(ctx, p.OrganizationID, id, stale); e == nil {
		t.Fatal("stale correction accepted")
	}
	other := independentReserve(t, s, l, 100)
	if _, e := s.SettleCost(ctx, p.OrganizationID, other.ID, evidence); e == nil {
		t.Fatal("same economic evidence charged twice")
	}
	independentBalances(t, s, p.OrganizationID, []string{l.TaskID}, 100, 40)
	for _, table := range []string{"ledger_entries", "cost_evidence", "financial_incidents"} {
		// financial_incidents needs a row before its immutable trigger can fire.
		if table == "financial_incidents" {
			if _, e := s.SettleCost(ctx, p.OrganizationID, other.ID, independentEvidence(130)); e != nil {
				t.Fatal(e)
			}
		}
		if _, e := s.Pool.Exec(ctx, "DELETE FROM "+table+" WHERE organization_id=$1", p.OrganizationID); e == nil {
			t.Fatalf("deleted append-only %s", table)
		}
	}
}

func TestIndependentLedgerReleaseDispatchRace(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 100000)
	a := agent(t, s, p)
	l := independentLease(t, s, p, a, "", 100000)
	for range 20 {
		r := independentReserve(t, s, l, 100)
		start := make(chan struct{})
		dispatched := make(chan error, 1)
		released := make(chan error, 1)
		go func() { <-start; dispatched <- s.PrepareCostDispatch(ctx, l, r.ID) }()
		go func() { <-start; _, e := s.ReleaseUnsentCost(ctx, p.OrganizationID, r.ID, "race"); released <- e }()
		close(start)
		d, e := <-dispatched, <-released
		if (d == nil) == (e == nil) {
			t.Fatalf("expected exactly one winner dispatch=%v release=%v", d, e)
		}
		if d == nil {
			if _, e := s.SettleCost(ctx, p.OrganizationID, r.ID, independentEvidence(0)); e != nil {
				t.Fatal(e)
			}
		}
	}
	independentBalances(t, s, p.OrganizationID, []string{l.TaskID}, 0, 0)
}

func TestIndependentLedgerForeignScopeAndLeaseTampering(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	pa, _ := identity(t, s, 1000)
	pb, _ := identity(t, s, 1000)
	aa := agent(t, s, pa)
	ab := agent(t, s, pb)
	la := independentLease(t, s, pa, aa, "", 1000)
	lb := independentLease(t, s, pb, ab, "", 1000)
	r := independentReserve(t, s, la, 100)
	for _, mutation := range []func(*domain.Lease){func(l *domain.Lease) { l.TaskID = lb.TaskID }, func(l *domain.Lease) { l.RunID = lb.RunID }, func(l *domain.Lease) { l.OrganizationID = pb.OrganizationID }, func(l *domain.Lease) { l.Token++ }} {
		copy := *la
		mutation(&copy)
		if _, e := s.ReserveCost(ctx, &copy, independentReserveInput(100)); e == nil {
			t.Fatal("mutated lease admitted")
		}
	}
	if _, e := s.SettleCost(ctx, pb.OrganizationID, r.ID, independentEvidence(50)); e == nil {
		t.Fatal("cross-tenant settlement accepted")
	}
	if e := s.PrepareCostDispatch(ctx, lb, r.ID); e == nil {
		t.Fatal("cross-tenant dispatch accepted")
	}
	wrong := independentEvidence(50)
	wrong.Source = "other-provider-account"
	if _, e := s.SettleCost(ctx, pa.OrganizationID, r.ID, wrong); e == nil {
		t.Fatal("foreign source accepted")
	}
	independentBalances(t, s, pa.OrganizationID, []string{la.TaskID}, 100, 0)
	independentBalances(t, s, pb.OrganizationID, []string{lb.TaskID}, 0, 0)
}

func TestIndependentLedgerExpiredLeaseAfterAccountWait(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, _ := identity(t, s, 1000)
	a := agent(t, s, p)
	l := independentLease(t, s, p, a, "", 1000)
	r := independentReserve(t, s, l, 100)
	if _, e := s.Pool.Exec(ctx, "UPDATE operations SET lease_until=clock_timestamp()+interval '400 milliseconds' WHERE organization_id=$1 AND id=$2", p.OrganizationID, l.OperationID); e != nil {
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
	done := make(chan error, 1)
	go func() { done <- s.PrepareCostDispatch(ctx, l, r.ID) }()
	waitForBlockedQuery(t, s, "SELECT budget_microusd,reserved_microusd,spent_microusd,active")
	for {
		var expired bool
		if e := s.Pool.QueryRow(ctx, "SELECT lease_until < clock_timestamp() FROM operations WHERE organization_id=$1 AND id=$2", p.OrganizationID, l.OperationID).Scan(&expired); e != nil {
			t.Fatal(e)
		}
		if expired {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if e = blocker.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e == nil {
		t.Fatal("dispatch admitted after lease expired while waiting for account lock")
	} else {
		code(t, e, "stale_lease")
	}
	var state string
	if e := s.Pool.QueryRow(ctx, "SELECT status FROM actions WHERE organization_id=$1 AND id=$2", p.OrganizationID, r.ActionID).Scan(&state); e != nil || state != "pending" {
		t.Fatalf("expired lease altered action=%s: %v", state, e)
	}
	independentBalances(t, s, p.OrganizationID, []string{l.TaskID}, 100, 0)
}

func TestIndependentLedgerReserveExpiredDuringAccountWait(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, _ := identity(t, s, 1000)
	a := agent(t, s, p)
	l := independentLease(t, s, p, a, "", 1000)
	if _, e := s.Pool.Exec(ctx, "UPDATE operations SET lease_until=clock_timestamp()+interval '400 milliseconds' WHERE organization_id=$1 AND id=$2", p.OrganizationID, l.OperationID); e != nil {
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
	done := make(chan error, 1)
	go func() { _, e := s.ReserveCost(ctx, l, independentReserveInput(100)); done <- e }()
	waitForBlockedQuery(t, s, "SELECT budget_microusd,reserved_microusd,spent_microusd,active")
	for {
		var expired bool
		if e := s.Pool.QueryRow(ctx, "SELECT lease_until < clock_timestamp() FROM operations WHERE organization_id=$1 AND id=$2", p.OrganizationID, l.OperationID).Scan(&expired); e != nil {
			t.Fatal(e)
		}
		if expired {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if e = blocker.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e == nil {
		t.Fatal("reserve admitted after lease expired while waiting for account lock")
	} else {
		code(t, e, "stale_lease")
	}
	independentBalances(t, s, p.OrganizationID, []string{l.TaskID}, 0, 0)
}

func TestIndependentLedgerRevocationDuringDispatchWait(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, _ := identity(t, s, 1000)
	a := agent(t, s, p)
	l := independentLease(t, s, p, a, "", 1000)
	r := independentReserve(t, s, l, 100)
	blocker, e := s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback(ctx)
	if _, e = blocker.Exec(ctx, "SELECT id FROM reservations WHERE organization_id=$1 AND id=$2 FOR UPDATE", p.OrganizationID, r.ID); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- s.PrepareCostDispatch(ctx, l, r.ID) }()
	waitForBlockedQuery(t, s, "SELECT "+reservationColumns+" FROM reservations WHERE organization_id=")
	// No task token is used to revoke authority: this models trusted IAM state.
	// Admission acquired the authority lock first, so revocation must serialize
	// after its commit instead of passing its permission check in flight.
	revoked := make(chan error, 1)
	go func() {
		_, e := s.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", p.OrganizationID, p.ID)
		revoked <- e
	}()
	waitForBlockedQuery(t, s, "UPDATE memberships SET active=false WHERE organization_id=")
	if e = blocker.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e != nil {
		t.Fatalf("admission that won the authority lock failed: %v", e)
	}
	if e := <-revoked; e != nil {
		t.Fatal(e)
	}
	var state string
	if e := s.Pool.QueryRow(ctx, "SELECT status FROM actions WHERE organization_id=$1 AND id=$2", p.OrganizationID, r.ActionID).Scan(&state); e != nil || state != "sent" {
		t.Fatalf("revocation altered action=%s: %v", state, e)
	}
	code(t, s.PrepareCostDispatch(ctx, l, r.ID), "forbidden")
	_, e = s.ReserveCost(ctx, l, independentReserveInput(1))
	code(t, e, "forbidden")
	independentBalances(t, s, p.OrganizationID, []string{l.TaskID}, 100, 0)
}

func TestIndependentLedgerExpiredLeaseAfterActionWait(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, _ := identity(t, s, 1000)
	a := agent(t, s, p)
	l := independentLease(t, s, p, a, "", 1000)
	r := independentReserve(t, s, l, 100)
	if _, e := s.Pool.Exec(ctx, "UPDATE operations SET lease_until=clock_timestamp()+interval '400 milliseconds' WHERE organization_id=$1 AND id=$2", p.OrganizationID, l.OperationID); e != nil {
		t.Fatal(e)
	}
	blocker, e := s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback(ctx)
	if _, e = blocker.Exec(ctx, "SELECT id FROM actions WHERE organization_id=$1 AND id=$2 FOR UPDATE", p.OrganizationID, r.ActionID); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- s.PrepareCostDispatch(ctx, l, r.ID) }()
	waitForBlockedQuery(t, s, "SELECT status,intent_hash FROM actions WHERE organization_id=")
	for {
		var expired bool
		if e := s.Pool.QueryRow(ctx, "SELECT lease_until < clock_timestamp() FROM operations WHERE organization_id=$1 AND id=$2", p.OrganizationID, l.OperationID).Scan(&expired); e != nil {
			t.Fatal(e)
		}
		if expired {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if e = blocker.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e == nil {
		t.Fatal("dispatch admitted after lease expired while waiting for action lock")
	} else {
		code(t, e, "stale_lease")
	}
	var state string
	if e := s.Pool.QueryRow(ctx, "SELECT status FROM actions WHERE organization_id=$1 AND id=$2", p.OrganizationID, r.ActionID).Scan(&state); e != nil || state != "pending" {
		t.Fatalf("expired lease altered action=%s: %v", state, e)
	}
	independentBalances(t, s, p.OrganizationID, []string{l.TaskID}, 100, 0)
}

func TestIndependentLedgerRejectsNonfinalAndWrongCurrency(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 1000)
	a := agent(t, s, p)
	l := independentLease(t, s, p, a, "", 1000)
	r := independentReserve(t, s, l, 100)
	if e := s.PrepareCostDispatch(ctx, l, r.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.MarkCostUnknown(ctx, p.OrganizationID, r.ID, "initial-timeout", "cost not final"); e != nil {
		t.Fatal(e)
	}
	// Contract 0.3 admits factual int64 values above the policy admission cap.
	// Their positive/overflow coverage lives in quarantine_independent_test.go.
	for _, mutation := range []func(*domain.CostEvidence){func(e *domain.CostEvidence) { e.Final = false }, func(e *domain.CostEvidence) { e.Currency = "" }, func(e *domain.CostEvidence) { e.Currency = "EUR" }, func(e *domain.CostEvidence) { e.ActualMicroUSD = -1 }, func(e *domain.CostEvidence) { e.PayloadDigest = "not-a-digest" }} {
		evidence := independentEvidence(50)
		mutation(&evidence)
		_, e := s.SettleCost(ctx, p.OrganizationID, r.ID, evidence)
		code(t, e, "invalid_request")
		independentBalances(t, s, p.OrganizationID, []string{l.TaskID}, 100, 0)
	}
}
