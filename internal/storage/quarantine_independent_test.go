// SPDX-License-Identifier: Elastic-2.0
package storage

// These are independent tests of trusted internal cost evidence with real
// PostgreSQL. No provider invoice authenticity or production readiness is implied.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

type quarantineFixture struct {
	s        *Store
	dsn      string
	p        domain.Principal
	l        *domain.Lease
	first    domain.AccountingOutcome
	second   domain.Reservation
	evidence domain.CostEvidence
	out      domain.AccountingOutcome
}

func independentQuarantine(t *testing.T, state string) quarantineFixture {
	t.Helper()
	s, dsn := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 10000)
	a := agent(t, s, p)
	l := independentLease(t, s, p, a, "", 10000)
	first := independentReserve(t, s, l, 50)
	second := independentReserve(t, s, l, 50)
	if e := s.PrepareCostDispatch(ctx, l, first.ID); e != nil {
		t.Fatal(e)
	}
	switch state {
	case "released":
		var e error
		second, e = s.ReleaseUnsentCost(ctx, p.OrganizationID, second.ID, "released-before-cost")
		if e != nil {
			t.Fatal(e)
		}
	case "settled":
		if e := s.PrepareCostDispatch(ctx, l, second.ID); e != nil {
			t.Fatal(e)
		}
		booked, e := s.AccountCost(ctx, p.OrganizationID, second.ID, independentEvidence(10))
		if e != nil || booked.Status != "settled" {
			t.Fatalf("prepare correction: %+v %v", booked, e)
		}
		second = booked.Reservation
	case "held":
		if e := s.PrepareCostDispatch(ctx, l, second.ID); e != nil {
			t.Fatal(e)
		}
	default:
		t.Fatal("unknown fixture state")
	}
	booked, e := s.AccountCost(ctx, p.OrganizationID, first.ID, independentEvidence(math.MaxInt64-25))
	if e != nil || booked.Status != "settled" {
		t.Fatalf("near-cap fact not booked: %+v %v", booked, e)
	}
	evidence := independentEvidence(50)
	var out domain.AccountingOutcome
	if state == "settled" {
		evidence.Reason = "trusted upward correction"
		out, e = s.AccountAdjustment(ctx, p.OrganizationID, second.ID, domain.CostAdjustment{ExpectedRevision: second.Revision, Evidence: evidence})
	} else {
		out, e = s.AccountCost(ctx, p.OrganizationID, second.ID, evidence)
	}
	if e != nil || out.Status != "quarantined" || out.EvidenceID == "" || out.QuarantineID == "" || out.Reservation.PendingQuarantineID != out.QuarantineID {
		t.Fatalf("fact was not durably quarantined: %+v %v", out, e)
	}
	return quarantineFixture{s: s, dsn: dsn, p: p, l: l, first: booked, second: second, evidence: evidence, out: out}
}
func quarantineRows(t *testing.T, s *Store, org, table string) int {
	t.Helper()
	var count int
	if e := s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE organization_id=$1", org).Scan(&count); e != nil {
		t.Fatal(e)
	}
	return count
}
func quarantineReplay(t *testing.T, f quarantineFixture, state string) domain.AccountingOutcome {
	t.Helper()
	var out domain.AccountingOutcome
	var e error
	if state == "settled" {
		out, e = f.s.AccountAdjustment(context.Background(), f.p.OrganizationID, f.second.ID, domain.CostAdjustment{ExpectedRevision: f.second.Revision, Evidence: f.evidence})
	} else {
		out, e = f.s.AccountCost(context.Background(), f.p.OrganizationID, f.second.ID, f.evidence)
	}
	if e != nil {
		t.Fatal(e)
	}
	return out
}

func TestIndependentQuarantineFullInt64Facts(t *testing.T) {
	for _, actual := range []int64{domain.MaxMoney + 1, 1<<53 + 1, math.MaxInt64} {
		t.Run(fmt.Sprint(actual), func(t *testing.T) {
			s, _ := testStore(t)
			ctx := context.Background()
			p, _ := identity(t, s, 1000)
			a := agent(t, s, p)
			l := independentLease(t, s, p, a, "", 1000)
			r := independentReserve(t, s, l, 100)
			if e := s.PrepareCostDispatch(ctx, l, r.ID); e != nil {
				t.Fatal(e)
			}
			out, e := s.AccountCost(ctx, p.OrganizationID, r.ID, independentEvidence(actual))
			if e != nil || out.Status != "settled" || out.EvidenceID == "" || out.Reservation.ActualMicroUSD == nil || *out.Reservation.ActualMicroUSD != actual {
				t.Fatalf("full factual amount lost: %+v %v", out, e)
			}
			independentBalances(t, s, p.OrganizationID, []string{l.TaskID}, 0, actual)
			if quarantineRows(t, s, p.OrganizationID, "cost_quarantines") != 0 {
				t.Fatal("representable fact unnecessarily quarantined")
			}
			if _, e := s.ReserveCost(ctx, l, independentReserveInput(0)); e == nil {
				t.Fatal("new admission after true overage")
			}
		})
	}
}

func TestIndependentQuarantinePreservesHeldSettledReleasedState(t *testing.T) {
	for _, state := range []string{"held", "settled", "released"} {
		t.Run(state, func(t *testing.T) {
			f := independentQuarantine(t, state)
			held, spent := int64(0), int64(math.MaxInt64-25)
			if state == "held" {
				held = 50
				if f.out.Reservation.Status != "unknown" || f.out.Reservation.ActualMicroUSD != nil {
					t.Fatalf("held quarantine changed cost facts: %+v", f.out.Reservation)
				}
			}
			if state == "settled" {
				spent += 10
				if f.out.Reservation.Status != "settled" || f.out.Reservation.ActualMicroUSD == nil || *f.out.Reservation.ActualMicroUSD != 10 {
					t.Fatalf("pending correction overwrote booked actual: %+v", f.out.Reservation)
				}
			}
			if state == "released" && f.out.Reservation.Status != "released" {
				t.Fatal("released quarantine invented a cost-state transition")
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.l.TaskID}, held, spent)
			beforeEntries := quarantineRows(t, f.s, f.p.OrganizationID, "ledger_entries")
			beforeEvents := quarantineRows(t, f.s, f.p.OrganizationID, "events")
			for range 3 {
				replay := quarantineReplay(t, f, state)
				if replay.Status != "quarantined" || replay.EvidenceID != f.out.EvidenceID || replay.QuarantineID != f.out.QuarantineID || replay.Reservation.Revision != f.out.Reservation.Revision {
					t.Fatal("unstable quarantined fact replay")
				}
				out, e := f.s.ResolveCostQuarantine(context.Background(), f.p.OrganizationID, f.out.QuarantineID, f.out.Reservation.Revision)
				if e != nil || out.Status != "quarantined" || out.Reservation.Revision != f.out.Reservation.Revision {
					t.Fatalf("still-overflowing resolve changed state: %+v %v", out, e)
				}
			}
			if quarantineRows(t, f.s, f.p.OrganizationID, "cost_quarantines") != 1 || quarantineRows(t, f.s, f.p.OrganizationID, "cost_quarantine_resolutions") != 0 {
				t.Fatal("repeated pending resolution created records")
			}
			if quarantineRows(t, f.s, f.p.OrganizationID, "ledger_entries") != beforeEntries || quarantineRows(t, f.s, f.p.OrganizationID, "events") != beforeEvents {
				t.Fatal("pending replay churned journal/outbox events")
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.l.TaskID}, held, spent)
		})
	}
}

func TestIndependentQuarantineConcurrentReplayAndResolveOnce(t *testing.T) {
	f := independentQuarantine(t, "held")
	ctx := context.Background()
	const n = 12
	var wg sync.WaitGroup
	outcomes := make(chan domain.AccountingOutcome, n)
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, e := f.s.AccountCost(ctx, f.p.OrganizationID, f.second.ID, f.evidence)
			outcomes <- out
			errs <- e
		}()
	}
	wg.Wait()
	close(outcomes)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	for out := range outcomes {
		if out.EvidenceID != f.out.EvidenceID || out.QuarantineID != f.out.QuarantineID {
			t.Fatal("concurrent replay changed evidence identity")
		}
	}
	if quarantineRows(t, f.s, f.p.OrganizationID, "cost_evidence") != 2 || quarantineRows(t, f.s, f.p.OrganizationID, "cost_quarantines") != 1 {
		t.Fatal("concurrent economic fact duplicated")
	}
	correction := independentEvidence(20)
	correction.Reason = "legitimate cost correction frees counter capacity"
	if out, e := f.s.AccountAdjustment(ctx, f.p.OrganizationID, f.first.Reservation.ID, domain.CostAdjustment{ExpectedRevision: f.first.Reservation.Revision, Evidence: correction}); e != nil || out.Status != "settled" {
		t.Fatalf("capacity correction: %+v %v", out, e)
	}
	code(t, func() error {
		_, e := f.s.ResolveCostQuarantine(ctx, f.p.OrganizationID, f.out.QuarantineID, f.out.Reservation.Revision-1)
		return e
	}(), "conflict")
	outcomes = make(chan domain.AccountingOutcome, n)
	errs = make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, e := f.s.ResolveCostQuarantine(ctx, f.p.OrganizationID, f.out.QuarantineID, f.out.Reservation.Revision)
			outcomes <- out
			errs <- e
		}()
	}
	wg.Wait()
	close(outcomes)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	for out := range outcomes {
		if out.Status != "settled" || out.EvidenceID != f.out.EvidenceID || out.Reservation.ActualMicroUSD == nil || *out.Reservation.ActualMicroUSD != 50 || out.Reservation.PendingQuarantineID != "" {
			t.Fatalf("bad resolved outcome: %+v", out)
		}
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.l.TaskID}, 0, 70)
	if quarantineRows(t, f.s, f.p.OrganizationID, "cost_quarantine_resolutions") != 1 {
		t.Fatal("resolution posted more than once")
	}
	var hold bool
	if e := f.s.Pool.QueryRow(ctx, "SELECT financial_hold FROM organizations WHERE id=$1", f.p.OrganizationID).Scan(&hold); e != nil || !hold {
		t.Fatalf("resolution silently cleared financial hold: %v", e)
	}
	replayed, e := f.s.AccountCost(ctx, f.p.OrganizationID, f.second.ID, f.evidence)
	if e != nil || replayed.Status != "settled" || replayed.EvidenceID != f.out.EvidenceID {
		t.Fatalf("evidence replay forgot its resolved disposition: %+v %v", replayed, e)
	}
}

func TestIndependentQuarantinePendingFactGuardsAndWrapper(t *testing.T) {
	f := independentQuarantine(t, "held")
	ctx := context.Background()
	changed := f.evidence
	changed.ActualMicroUSD++
	if _, e := f.s.AccountCost(ctx, f.p.OrganizationID, f.second.ID, changed); e == nil {
		t.Fatal("changed same economic identity accepted")
	}
	if _, e := f.s.AccountCost(ctx, f.p.OrganizationID, f.second.ID, independentEvidence(0)); e == nil {
		t.Fatal("new final fact bypassed pending quarantine")
	}
	if _, e := f.s.ReleaseUnsentCost(ctx, f.p.OrganizationID, f.second.ID, "release-pending"); e == nil {
		t.Fatal("pending liability released")
	}
	if e := f.s.PrepareCostDispatch(ctx, f.l, f.second.ID); e == nil {
		t.Fatal("pending liability redispatched")
	}
	if _, e := f.s.MarkCostUnknown(ctx, f.p.OrganizationID, f.second.ID, "after-quarantine", "cannot change posting base"); e == nil {
		// Metadata-only unknown reporting may be allowed, but it cannot invalidate
		// the immutable pending base/revision used by Resolve.
		var revision int
		if e := f.s.Pool.QueryRow(ctx, "SELECT revision FROM reservations WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.second.ID).Scan(&revision); e != nil || revision != f.out.Reservation.Revision {
			t.Fatal("unknown reporting invalidated pending posting base")
		}
	}
	_, e := f.s.SettleCost(ctx, f.p.OrganizationID, f.second.ID, f.evidence)
	var q *QuarantinedError
	if !errors.As(e, &q) || q.Outcome.EvidenceID != f.out.EvidenceID || q.Outcome.QuarantineID != f.out.QuarantineID {
		t.Fatalf("legacy caller loses durable quarantine outcome: %T", e)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.l.TaskID}, 50, math.MaxInt64-25)
}

func TestIndependentQuarantineAllScopesPreflightAndReadVisibility(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 10000)
	foreign, _ := identity(t, s, 10000)
	a := agent(t, s, p)
	root := independentLease(t, s, p, a, "", 10000)
	left := independentLease(t, s, p, a, root.TaskID, 5000)
	right := independentLease(t, s, p, a, root.TaskID, 5000)
	one := independentReserve(t, s, left, 50)
	two := independentReserve(t, s, right, 50)
	three := independentReserve(t, s, right, 50)
	for _, pair := range []struct {
		l *domain.Lease
		r domain.Reservation
	}{{left, one}, {right, two}, {right, three}} {
		if e := s.PrepareCostDispatch(ctx, pair.l, pair.r.ID); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := s.AccountCost(ctx, p.OrganizationID, one.ID, independentEvidence(math.MaxInt64-25)); e != nil {
		t.Fatal(e)
	}
	for _, r := range []domain.Reservation{two, three} {
		out, e := s.AccountCost(ctx, p.OrganizationID, r.ID, independentEvidence(math.MaxInt64))
		if e != nil || out.Status != "quarantined" {
			t.Fatalf("ancestor overflow not retained: %+v %v", out, e)
		}
	}
	independentBalances(t, s, p.OrganizationID, []string{root.TaskID}, 100, math.MaxInt64-25)
	var leafHeld, leafSpent int64
	if e := s.Pool.QueryRow(ctx, "SELECT reserved_microusd,spent_microusd FROM tasks WHERE organization_id=$1 AND id=$2", p.OrganizationID, right.TaskID).Scan(&leafHeld, &leafSpent); e != nil || leafHeld != 100 || leafSpent != 0 {
		t.Fatalf("partially mutated leaf: %d/%d %v", leafHeld, leafSpent, e)
	}
	for _, taskID := range []string{root.TaskID, right.TaskID} {
		report, e := s.Costs(ctx, p, taskID, CostCursors{Limit: 1})
		if e != nil || report.PendingQuarantineCount != 2 || !report.Incomplete || len(report.Quarantines) != 1 || report.NextQuarantineCursor == "" {
			t.Fatalf("pending read unavailable/inexact: %+v %v", report, e)
		}
		next, e := s.Costs(ctx, p, taskID, CostCursors{Limit: 1, Quarantine: report.NextQuarantineCursor})
		if e != nil || len(next.Quarantines) != 1 || next.Quarantines[0].ID == report.Quarantines[0].ID || next.NextQuarantineCursor != "" {
			t.Fatalf("pending cursor failed: %+v %v", next, e)
		}
		wire, e := json.Marshal(report)
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(string(wire), `"source":`) || strings.Contains(string(wire), `"reference":`) || strings.Contains(string(wire), `"payload_digest":`) {
			t.Fatal("ordinary inspection disclosed raw evidence metadata")
		}
	}
	if report, e := s.Costs(ctx, p, left.TaskID, CostCursors{Limit: 10}); e != nil || report.PendingQuarantineCount != 0 || report.Incomplete {
		t.Fatalf("sibling pending liability leaked into subtree: %+v %v", report, e)
	}
	if _, e := s.Costs(ctx, foreign, root.TaskID, CostCursors{Limit: 10}); e == nil {
		t.Fatal("foreign tenant read quarantine")
	}
}

func TestIndependentQuarantineRestartRevocationAndImmutableHistory(t *testing.T) {
	f := independentQuarantine(t, "held")
	ctx := context.Background()
	if _, e := f.s.CancelTask(ctx, f.p, f.l.TaskID, "cancel-pending"); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", f.p.OrganizationID, f.p.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := f.s.Pool.Exec(ctx, "UPDATE organizations SET active=false WHERE id=$1", f.p.OrganizationID); e != nil {
		t.Fatal(e)
	}
	reopened, e := Open(ctx, f.dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	f.s = reopened
	if out := quarantineReplay(t, f, "held"); out.EvidenceID != f.out.EvidenceID || out.QuarantineID != f.out.QuarantineID {
		t.Fatal("reopen lost economic identity")
	}
	correction := independentEvidence(20)
	correction.Reason = "trusted statement after revocation"
	if _, e := reopened.AccountAdjustment(ctx, f.p.OrganizationID, f.first.Reservation.ID, domain.CostAdjustment{ExpectedRevision: f.first.Reservation.Revision, Evidence: correction}); e != nil {
		t.Fatal(e)
	}
	if out, e := reopened.ResolveCostQuarantine(ctx, f.p.OrganizationID, f.out.QuarantineID, f.out.Reservation.Revision); e != nil || out.Status != "settled" {
		t.Fatalf("trusted resolution after revocation failed: %+v %v", out, e)
	}
	// Public observation remains denied after member and organization
	// revocation. Inspect the trusted accounting result without restoring them.
	_, e = reopened.GetTask(ctx, f.p, f.l.TaskID)
	code(t, e, "unauthenticated")
	var status, result string
	e = reopened.Pool.QueryRow(ctx, "SELECT status,result FROM tasks WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, f.l.TaskID).Scan(&status, &result)
	if e != nil || status != "cancelled" || result != "" {
		t.Fatal("accounting resurrected task", status, e)
	}
	for _, table := range []string{"cost_evidence", "cost_quarantines", "cost_quarantine_resolutions"} {
		if _, e := reopened.Pool.Exec(ctx, "DELETE FROM "+table+" WHERE organization_id=$1", f.p.OrganizationID); e == nil {
			t.Fatalf("deleted immutable %s", table)
		}
	}
	independentBalances(t, reopened, f.p.OrganizationID, []string{f.l.TaskID}, 0, 70)
}

func TestIndependentQuarantineWirePreservesExactMoney(t *testing.T) {
	actual := int64(math.MaxInt64)
	negative := int64(-math.MaxInt64)
	for _, value := range []any{domain.Task{ReservedMicroUSD: actual, SpentMicroUSD: actual}, domain.Reservation{AmountMicroUSD: 100, ActualMicroUSD: &actual}, independentEvidence(actual), domain.LedgerEntry{DeltaReservedMicroUSD: negative, DeltaSpentMicroUSD: actual}, domain.FinancialIncident{AmountMicroUSD: actual}, domain.CostReport{ReservedMicroUSD: actual, SpentMicroUSD: actual, UnknownMicroUSD: actual}, domain.CostQuarantine{ActualMicroUSD: actual}} {
		wire, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		var decoded map[string]any
		if e = json.Unmarshal(wire, &decoded); e != nil {
			t.Fatal(e)
		}
		for key, v := range decoded {
			if strings.HasSuffix(key, "microusd") && key != "budget_microusd" {
				text, ok := v.(string)
				if !ok {
					t.Fatalf("money %s is not exact decimal string: %T", key, v)
				}
				if text != fmt.Sprint(actual) && text != fmt.Sprint(negative) && text != "100" {
					t.Fatalf("unexpected monetary lexeme: %s", text)
				}
			}
		}
	}
	// Policy inputs remain bounded JSON numbers, unlike unrestricted facts.
	wire, e := json.Marshal(independentReserveInput(domain.MaxMoney))
	if e != nil {
		t.Fatal(e)
	}
	var input map[string]any
	json.Unmarshal(wire, &input)
	if _, ok := input["amount_microusd"].(float64); !ok {
		t.Fatal("admission policy input lost its documented numeric representation")
	}
}

// Construct a genuine 003 schema: replay compatibility must not be inferred
// solely from facts first inserted by the new 004 code.
func independentStoreAt003(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for real PostgreSQL integration")
	}
	base, e := Open(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := domain.NewID("test_legacy")
	if _, e = base.Pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); e != nil {
		base.Close()
		t.Fatal(e)
	}
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, e := Open(ctx, u.String())
	if e != nil {
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
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "CREATE TABLE schema_migrations(name text PRIMARY KEY,sha256 text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())"); e != nil {
		t.Fatal(e)
	}
	files, e := migrations.ReadDir("migrations")
	if e != nil {
		t.Fatal(e)
	}
	for _, file := range files {
		if file.Name() >= "004" {
			continue
		}
		name := "migrations/" + file.Name()
		body, e := migrations.ReadFile(name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(ctx, string(body)); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(ctx, "INSERT INTO schema_migrations(name,sha256) VALUES($1,$2)", name, Hash(body)); e != nil {
			t.Fatal(e)
		}
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	return s
}
func TestIndependentQuarantineMigrationPreservesNumericEvidenceReplay(t *testing.T) {
	s := independentStoreAt003(t)
	ctx := context.Background()
	// Exact003 staging predates current authentication's016 credential fields.
	// Use a current constructor only during a bounded test-only callback; this
	// emulates the old gate's absence and is not historical binary execution.
	var bootstrap Bootstrap
	historicalFixtureWithoutRecoveryAdmission(t, s, func() {
		var err error
		bootstrap, err = s.Bootstrap(ctx, "Historical numeric evidence", 1000)
		if err != nil {
			t.Fatal(err)
		}
	})
	p := domain.Principal{ID: bootstrap.PrincipalID, OrganizationID: bootstrap.OrganizationID, Role: "owner", TokenHash: Hash([]byte(bootstrap.Token)), CredentialKind: "bearer"}
	// Keep the complete numeric evidence fixture on the actual 003 schema.
	// The current CreateAgent API requires later columns, so insert only the
	// historical agent fields; do not migrate before creating old evidence.
	a := domain.Agent{ID: domain.NewID("agt"), Version: 1}
	if _, e := s.Pool.Exec(ctx, "INSERT INTO agents(organization_id,id,name,instructions,model,budget_microusd,published) VALUES($1,$2,'Legacy fixture','Historical immutable agent',$3,1000,true)", p.OrganizationID, a.ID, domain.FixtureModel); e != nil {
		t.Fatal(e)
	}
	l := independentLease(t, s, p, a, "", 1000)
	settledID, unknownID := domain.NewID("res"), domain.NewID("res")
	for _, id := range []string{settledID, unknownID} {
		action := domain.NewID("act")
		if _, e := s.Pool.Exec(ctx, "INSERT INTO actions(organization_id,id,task_id,operation_id,intent_hash,idempotency_key,status) VALUES($1,$2,$3,$4,$5,$2,'confirmed')", p.OrganizationID, action, l.TaskID, l.OperationID, Hash([]byte("legacy intent"))); e != nil {
			t.Fatal(e)
		}
		state, revision := "unknown", 1
		var actual any
		if id == settledID {
			state = "settled"
			revision = 3
			actual = int64(60)
		}
		if _, e := s.Pool.Exec(ctx, "INSERT INTO reservations(organization_id,id,task_id,operation_id,action_id,category,provider,price_version,amount_microusd,actual_microusd,status,revision) VALUES($1,$2,$3,$4,$5,'model','test-provider-account-A','test-final-usd-v1',100,$6,$7,$8)", p.OrganizationID, id, l.TaskID, l.OperationID, action, actual, state, revision); e != nil {
			t.Fatal(e)
		}
		if _, e := s.Pool.Exec(ctx, "INSERT INTO ledger_entries(organization_id,id,reservation_id,task_id,root_id,event_key,event_hash,kind,delta_reserved_microusd,delta_spent_microusd) VALUES($1,$2,$3,$4,$4,'reserve','legacy-reserve','reserve',100,0)", p.OrganizationID, domain.NewID("led"), id, l.TaskID); e != nil {
			t.Fatal(e)
		}
	}
	// Field order, capitalized envelope keys, numeric amount and omitted empty
	// reason reproduce the actual contract 0.2 hashing shape independently.
	type legacyEvidence struct {
		Source         string `json:"source"`
		Reference      string `json:"reference"`
		PayloadDigest  string `json:"payload_digest"`
		ActualMicroUSD int64  `json:"actual_microusd"`
		Reason         string `json:"reason,omitempty"`
		Final          bool   `json:"final"`
		Currency       string `json:"currency"`
	}
	first := independentEvidence(50)
	correction := independentEvidence(60)
	correction.Reason = "legacy correction"
	oldRevision := 2
	for i, ev := range []domain.CostEvidence{first, correction} {
		var expected *int
		kind := "settle"
		deltaHeld, deltaSpent := int64(-100), int64(50)
		if i == 1 {
			expected = &oldRevision
			kind = "adjustment"
			deltaHeld = 0
			deltaSpent = 10
		}
		body, e := json.Marshal(struct {
			Evidence legacyEvidence
			Expected *int
		}{legacyEvidence{ev.Source, ev.Reference, ev.PayloadDigest, ev.ActualMicroUSD, ev.Reason, ev.Final, ev.Currency}, expected})
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(string(body), `"actual_microusd":"`) {
			t.Fatal("legacy fixture accidentally used new wire encoding")
		}
		if _, e = s.Pool.Exec(ctx, "INSERT INTO cost_evidence(organization_id,source,reference,reservation_id,payload_digest,actual_microusd,reason) VALUES($1,$2,$3,$4,$5,$6,$7)", p.OrganizationID, ev.Source, ev.Reference, settledID, ev.PayloadDigest, ev.ActualMicroUSD, ev.Reason); e != nil {
			t.Fatal(e)
		}
		if _, e = s.Pool.Exec(ctx, "INSERT INTO ledger_entries(organization_id,id,reservation_id,task_id,root_id,event_key,event_hash,kind,delta_reserved_microusd,delta_spent_microusd) VALUES($1,$2,$3,$4,$4,$5,$6,$7,$8,$9)", p.OrganizationID, domain.NewID("led"), settledID, l.TaskID, kind+":"+ev.Source+":"+ev.Reference, Hash(body), kind, deltaHeld, deltaSpent); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := s.Pool.Exec(ctx, "UPDATE organizations SET reserved_microusd=100,spent_microusd=60 WHERE id=$1", p.OrganizationID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Pool.Exec(ctx, "UPDATE tasks SET reserved_microusd=100,spent_microusd=60 WHERE organization_id=$1 AND id=$2", p.OrganizationID, l.TaskID); e != nil {
		t.Fatal(e)
	}
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	before := quarantineRows(t, s, p.OrganizationID, "ledger_entries")
	initial, e := s.AccountCost(ctx, p.OrganizationID, settledID, first)
	if e != nil || initial.Status != "settled" || initial.EvidenceID == "" {
		t.Fatalf("old numeric settle replay lost: %+v %v", initial, e)
	}
	adjusted, e := s.AccountAdjustment(ctx, p.OrganizationID, settledID, domain.CostAdjustment{ExpectedRevision: oldRevision, Evidence: correction})
	if e != nil || adjusted.Status != "settled" || adjusted.EvidenceID == "" || adjusted.EvidenceID == initial.EvidenceID {
		t.Fatalf("old numeric adjustment replay lost: %+v %v", adjusted, e)
	}
	if initial.Reservation.ActualMicroUSD == nil || *initial.Reservation.ActualMicroUSD != 60 || initial.Reservation.Revision != 3 {
		t.Fatal("old replay regressed current projection")
	}
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	replay, e := s.AccountCost(ctx, p.OrganizationID, settledID, first)
	if e != nil || replay.EvidenceID != initial.EvidenceID {
		t.Fatal("second migration changed backfilled evidence identity")
	}
	if quarantineRows(t, s, p.OrganizationID, "cost_evidence") != 2 || quarantineRows(t, s, p.OrganizationID, "ledger_entries") != before {
		t.Fatal("upgrade replay duplicated economic history")
	}
	independentBalances(t, s, p.OrganizationID, []string{l.TaskID}, 100, 60)
	if _, e := s.Pool.Exec(ctx, "UPDATE cost_evidence SET reason='overwrite' WHERE organization_id=$1", p.OrganizationID); e == nil {
		t.Fatal("migration failed to restore evidence immutability")
	}
}

func TestIndependentQuarantineTenantAndEconomicBinding(t *testing.T) {
	f := independentQuarantine(t, "held")
	ctx := context.Background()
	foreign, _ := identity(t, f.s, 1000)
	if _, e := f.s.AccountCost(ctx, foreign.OrganizationID, f.second.ID, f.evidence); e == nil {
		t.Fatal("cross-tenant cost binding accepted")
	}
	if _, e := f.s.ResolveCostQuarantine(ctx, foreign.OrganizationID, f.out.QuarantineID, f.out.Reservation.Revision); e == nil {
		t.Fatal("cross-tenant quarantine resolved")
	}
	if _, e := f.s.AccountCost(ctx, f.p.OrganizationID, f.first.Reservation.ID, f.evidence); e == nil {
		t.Fatal("same economic observation moved to another reservation")
	}
	for _, change := range []func(*domain.CostEvidence){func(e *domain.CostEvidence) { e.Final = false }, func(e *domain.CostEvidence) { e.Currency = "EUR" }, func(e *domain.CostEvidence) { e.PayloadDigest = Hash([]byte("different original body")) }, func(e *domain.CostEvidence) { e.Source = "different-account" }} {
		mutated := f.evidence
		change(&mutated)
		if _, e := f.s.AccountCost(ctx, f.p.OrganizationID, f.second.ID, mutated); e == nil {
			t.Fatal("changed evidence identity/meaning accepted")
		}
	}
	a := agent(t, f.s, foreign)
	l := independentLease(t, f.s, foreign, a, "", 1000)
	r := independentReserve(t, f.s, l, 100)
	if e := f.s.PrepareCostDispatch(ctx, l, r.ID); e != nil {
		t.Fatal(e)
	}
	out, e := f.s.AccountCost(ctx, foreign.OrganizationID, r.ID, f.evidence)
	if e != nil || out.Status != "settled" || out.EvidenceID == f.out.EvidenceID {
		t.Fatalf("tenant-scoped economic key incorrectly global: %+v %v", out, e)
	}
	if quarantineRows(t, f.s, f.p.OrganizationID, "cost_evidence") != 2 || quarantineRows(t, f.s, f.p.OrganizationID, "cost_quarantines") != 1 {
		t.Fatal("rejected rebindings changed original history")
	}
}

func TestIndependentQuarantineResolveCorrectionAndLateCharge(t *testing.T) {
	for _, state := range []string{"settled", "released"} {
		t.Run(state, func(t *testing.T) {
			f := independentQuarantine(t, state)
			ctx := context.Background()
			adjust := independentEvidence(20)
			adjust.Reason = "free capacity for pending posting"
			if _, e := f.s.AccountAdjustment(ctx, f.p.OrganizationID, f.first.Reservation.ID, domain.CostAdjustment{ExpectedRevision: f.first.Reservation.Revision, Evidence: adjust}); e != nil {
				t.Fatal(e)
			}
			out, e := f.s.ResolveCostQuarantine(ctx, f.p.OrganizationID, f.out.QuarantineID, f.out.Reservation.Revision)
			if e != nil || out.Status != "settled" || out.Reservation.ActualMicroUSD == nil || *out.Reservation.ActualMicroUSD != 50 {
				t.Fatalf("resolve %s: %+v %v", state, out, e)
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.l.TaskID}, 0, 70)
			var deltaHeld, deltaSpent int64
			if e := f.s.Pool.QueryRow(ctx, "SELECT delta_reserved_microusd,delta_spent_microusd FROM ledger_entries WHERE organization_id=$1 AND reservation_id=$2 AND kind='quarantine_resolution'", f.p.OrganizationID, f.second.ID).Scan(&deltaHeld, &deltaSpent); e != nil {
				t.Fatal(e)
			}
			want := int64(50)
			if state == "settled" {
				want = 40
			}
			if deltaHeld != 0 || deltaSpent != want {
				t.Fatalf("resolved delta recomputed from wrong booked base: %d/%d", deltaHeld, deltaSpent)
			}
			report, e := f.s.Costs(ctx, f.p, f.l.TaskID, CostCursors{Limit: 10})
			if e != nil || report.Incomplete || report.PendingQuarantineCount != 0 || len(report.Quarantines) != 1 || report.Quarantines[0].Status != "resolved" || report.Quarantines[0].ResolutionID == "" {
				t.Fatalf("resolved history visibility: %+v %v", report, e)
			}
		})
	}
}

func independentFailAccountingOutbox(t *testing.T, s *Store, eventType string) {
	t.Helper()
	ctx := context.Background()
	// These are test-owned schema objects. The trigger aborts only the chosen
	// accounting event's outbox insert, after preceding transaction writes.
	sql := fmt.Sprintf(`CREATE FUNCTION independent_reject_accounting_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF EXISTS(SELECT 1 FROM events WHERE sequence=NEW.sequence AND type='%s') THEN RAISE EXCEPTION 'independent forced outbox failure'; END IF;
 RETURN NEW; END $$;
 CREATE TRIGGER independent_accounting_outbox BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION independent_reject_accounting_outbox()`, eventType)
	if _, e := s.Pool.Exec(ctx, sql); e != nil {
		t.Fatal(e)
	}
}
func independentRemoveOutboxFailure(t *testing.T, s *Store) {
	t.Helper()
	if _, e := s.Pool.Exec(context.Background(), "DROP TRIGGER independent_accounting_outbox ON outbox; DROP FUNCTION independent_reject_accounting_outbox()"); e != nil {
		t.Fatal(e)
	}
}
func TestIndependentQuarantineOutboxFailureRollsBackAcceptedFact(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 10000)
	a := agent(t, s, p)
	l := independentLease(t, s, p, a, "", 10000)
	first := independentReserve(t, s, l, 50)
	second := independentReserve(t, s, l, 50)
	for _, r := range []domain.Reservation{first, second} {
		if e := s.PrepareCostDispatch(ctx, l, r.ID); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := s.AccountCost(ctx, p.OrganizationID, first.ID, independentEvidence(math.MaxInt64-25)); e != nil {
		t.Fatal(e)
	}
	evidence := independentEvidence(50)
	beforeEvents := quarantineRows(t, s, p.OrganizationID, "events")
	beforeLedger := quarantineRows(t, s, p.OrganizationID, "ledger_entries")
	independentFailAccountingOutbox(t, s, "cost.quarantined")
	if out, e := s.AccountCost(ctx, p.OrganizationID, second.ID, evidence); e == nil || out.EvidenceID != "" || out.Status != "" {
		t.Fatal("failed commit path falsely acknowledged durable fact")
	}
	if quarantineRows(t, s, p.OrganizationID, "cost_evidence") != 1 || quarantineRows(t, s, p.OrganizationID, "cost_quarantines") != 0 || quarantineRows(t, s, p.OrganizationID, "events") != beforeEvents || quarantineRows(t, s, p.OrganizationID, "ledger_entries") != beforeLedger {
		t.Fatal("outbox failure partially committed accepted fact")
	}
	var pending *string
	var revision int
	var status string
	if e := s.Pool.QueryRow(ctx, "SELECT pending_quarantine_id,revision,status FROM reservations WHERE organization_id=$1 AND id=$2", p.OrganizationID, second.ID).Scan(&pending, &revision, &status); e != nil || pending != nil || revision != second.Revision || status != "reserved" {
		t.Fatalf("failed accept changed reservation: %v %d %s %v", pending, revision, status, e)
	}
	independentBalances(t, s, p.OrganizationID, []string{l.TaskID}, 50, math.MaxInt64-25)
	independentRemoveOutboxFailure(t, s)
	out, e := s.AccountCost(ctx, p.OrganizationID, second.ID, evidence)
	if e != nil || out.Status != "quarantined" {
		t.Fatalf("same unaccepted fact could not retry: %+v %v", out, e)
	}
}
func TestIndependentQuarantineOutboxFailureRollsBackResolution(t *testing.T) {
	f := independentQuarantine(t, "held")
	ctx := context.Background()
	adjust := independentEvidence(20)
	adjust.Reason = "free projection capacity"
	if _, e := f.s.AccountAdjustment(ctx, f.p.OrganizationID, f.first.Reservation.ID, domain.CostAdjustment{ExpectedRevision: f.first.Reservation.Revision, Evidence: adjust}); e != nil {
		t.Fatal(e)
	}
	beforeEvents := quarantineRows(t, f.s, f.p.OrganizationID, "events")
	beforeLedger := quarantineRows(t, f.s, f.p.OrganizationID, "ledger_entries")
	independentFailAccountingOutbox(t, f.s, "cost.quarantine_resolution")
	if out, e := f.s.ResolveCostQuarantine(ctx, f.p.OrganizationID, f.out.QuarantineID, f.out.Reservation.Revision); e == nil || out.Status != "" {
		t.Fatal("failed resolution falsely acknowledged posting")
	}
	if quarantineRows(t, f.s, f.p.OrganizationID, "cost_quarantine_resolutions") != 0 || quarantineRows(t, f.s, f.p.OrganizationID, "events") != beforeEvents || quarantineRows(t, f.s, f.p.OrganizationID, "ledger_entries") != beforeLedger {
		t.Fatal("failed resolution partly committed history")
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.l.TaskID}, 50, 20)
	if out := quarantineReplay(t, f, "held"); out.Status != "quarantined" || out.Reservation.Revision != f.out.Reservation.Revision {
		t.Fatal("failed resolution changed pending state")
	}
	independentRemoveOutboxFailure(t, f.s)
	if out, e := f.s.ResolveCostQuarantine(ctx, f.p.OrganizationID, f.out.QuarantineID, f.out.Reservation.Revision); e != nil || out.Status != "settled" {
		t.Fatalf("failed resolution could not retry: %+v %v", out, e)
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.l.TaskID}, 0, 70)
}
