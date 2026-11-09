// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

func recoveryCostOwned(t *testing.T) {
	t.Helper()
	if os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" {
		t.Skip("fresh owned PostgreSQL harness required")
	}
}

func recoveryCostWant(t *testing.T, tx pgx.Tx) RecoveryCostHistoryReport {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := checkRecoveryCostHistory(ctx, tx, "pg_temp")
	if err != nil {
		t.Fatal("valid historical accounting rejected")
	}
	return report
}

func recoveryCostExec(t *testing.T, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal("owned historical mutation failed")
	}
}

func recoveryCostNegative(t *testing.T, tx pgx.Tx, mutate func(), stillBalanced bool) {
	t.Helper()
	recoveryCostExec(t, tx, "SAVEPOINT cost_case")
	defer recoveryCostExec(t, tx, "ROLLBACK TO SAVEPOINT cost_case")
	mutate()
	if stillBalanced {
		recoveryInvariantWantValid(t, tx)
	}
	if _, err := checkRecoveryCostHistory(context.Background(), tx, "pg_temp"); !errors.Is(err, ErrRecoveryCostHistory) {
		t.Fatal("forged accounting history accepted")
	}
	var one int
	if err := tx.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatal("data refusal poisoned private transaction")
	}
}

// Independent fixture serialization of the historical numeric request. It does
// not call evidenceRequestHash or the recovery hash helper; current wire money
// strings must never replace this stored idempotency contract.
func recoveryCostAuthorHash(e domain.CostEvidence, expected *int) string {
	type evidenceV02 struct {
		Source        string `json:"source"`
		Reference     string `json:"reference"`
		PayloadDigest string `json:"payload_digest"`
		Actual        int64  `json:"actual_microusd"`
		Reason        string `json:"reason,omitempty"`
		Final         bool   `json:"final"`
		Currency      string `json:"currency"`
	}
	body, _ := json.Marshal(struct {
		Evidence evidenceV02
		Expected *int
	}{evidenceV02{e.Source, e.Reference, e.PayloadDigest, e.ActualMicroUSD, e.Reason, e.Final, e.Currency}, expected})
	return Hash(body)
}

func TestRecoveryCostHistoryAuthorBoundary(t *testing.T) {
	if _, err := checkRecoveryCostHistory(nil, nil, "pg_temp"); !errors.Is(err, ErrRecoveryCostHistory) {
		t.Fatal("nil boundary not refused")
	}
	e := domain.CostEvidence{Source: "provider<&>", Reference: "счёт\u2028", PayloadDigest: strings.Repeat("a", 64), ActualMicroUSD: 9007199254740993, Final: true, Currency: "USD"}
	// Exact, independently written byte shape including Go's historical HTML
	// escaping, omitted empty reason, numeric int64 and null expected revision.
	literal := `{"Evidence":{"source":"provider\u003c\u0026\u003e","reference":"счёт\u2028","payload_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","actual_microusd":9007199254740993,"final":true,"currency":"USD"},"Expected":null}`
	if evidenceRequestHash(e, nil) != Hash([]byte(literal)) || recoveryCostAuthorHash(e, nil) != Hash([]byte(literal)) {
		t.Fatal("historical numeric serialization changed")
	}
	current, _ := json.Marshal(struct {
		Evidence domain.CostEvidence
		Expected *int
	}{e, nil})
	if Hash(current) == Hash([]byte(literal)) {
		t.Fatal("fixture did not distinguish current money wire")
	}
}

func TestRecoveryCostHistoryAuthorRealChainsAndForgeries(t *testing.T) {
	recoveryCostOwned(t)
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 10000)
	a := agent(t, s, p)
	l := independentLease(t, s, p, a, "", 10000)
	zero := independentReserve(t, s, l, 0)
	unknown := independentReserve(t, s, l, 100)
	released := independentReserve(t, s, l, 100)
	late := independentReserve(t, s, l, 100)
	charged := independentReserve(t, s, l, 100)
	_ = independentReserve(t, s, l, 100)
	for _, r := range []domain.Reservation{zero, unknown} {
		if _, err := s.MarkCostUnknown(ctx, p.OrganizationID, r.ID, "first", "uncertain synthetic fact"); err != nil {
			t.Fatal("unknown fixture failed")
		}
		if _, err := s.MarkCostUnknown(ctx, p.OrganizationID, r.ID, "second", "another uncertainty fact"); err != nil {
			t.Fatal("second unknown fixture failed")
		}
	}
	for _, r := range []domain.Reservation{released, late} {
		if _, err := s.ReleaseUnsentCost(ctx, p.OrganizationID, r.ID, "before-send"); err != nil {
			t.Fatal("release fixture failed")
		}
	}
	if _, err := s.AccountCost(ctx, p.OrganizationID, late.ID, independentEvidence(20)); err != nil {
		t.Fatal("late cost fixture failed")
	}
	first := independentEvidence(70)
	out, err := s.AccountCost(ctx, p.OrganizationID, charged.ID, first)
	if err != nil {
		t.Fatal("settlement fixture failed")
	}
	decrease := independentEvidence(10)
	decrease.Reason = "legitimate refund <&>"
	out, err = s.AccountAdjustment(ctx, p.OrganizationID, charged.ID, domain.CostAdjustment{ExpectedRevision: out.Reservation.Revision, Evidence: decrease})
	if err != nil {
		t.Fatal("signed decrease fixture failed")
	}
	secondExpected := out.Reservation.Revision
	increase := independentEvidence(40)
	increase.Reason = "final correction"
	out, err = s.AccountAdjustment(ctx, p.OrganizationID, charged.ID, domain.CostAdjustment{ExpectedRevision: secondExpected, Evidence: increase})
	if err != nil {
		t.Fatal("second correction fixture failed")
	}
	// Revocation and budget tightening are valid history, not import rejection.
	if _, err = s.Pool.Exec(ctx, `UPDATE memberships SET active=false WHERE organization_id=$1`, p.OrganizationID); err != nil {
		t.Fatal("revocation fixture failed")
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE tasks SET budget_microusd=1 WHERE organization_id=$1`, p.OrganizationID); err != nil {
		t.Fatal("budget history fixture failed")
	}
	tx := recoveryInvariantStage(t, s)
	recoveryInvariantWantValid(t, tx)
	report := recoveryCostWant(t, tx)
	if report.ModernRequestsChecked != 4 || report.HistoriesChecked != 6 || report.LegacyOpaqueEvidence != 0 || len(report.OpaqueOrganizations) != 0 {
		t.Fatal("wrong complete-history report")
	}
	t.Run("timestamps_are_not_revision_order", func(t *testing.T) {
		recoveryCostExec(t, tx, "SAVEPOINT chronology")
		defer recoveryCostExec(t, tx, "ROLLBACK TO SAVEPOINT chronology")
		recoveryCostExec(t, tx, `UPDATE pg_temp.ledger_entries SET created_at=CASE kind WHEN 'reserve' THEN '2030-01-01'::timestamptz WHEN 'adjustment' THEN '2000-01-01'::timestamptz ELSE '2020-01-01'::timestamptz END`)
		recoveryCostExec(t, tx, `UPDATE pg_temp.cost_evidence SET created_at='1999-01-01'`)
		recoveryCostWant(t, tx)
	})
	for _, tc := range []struct{ name, sql string }{
		{"payload_changed", `UPDATE pg_temp.cost_evidence SET payload_digest=repeat('b',64) WHERE reference=$1`},
		{"amount_changed", `UPDATE pg_temp.cost_evidence SET actual_microusd=actual_microusd+1 WHERE reference=$1`},
		{"reason_changed", `UPDATE pg_temp.cost_evidence SET reason='different reason' WHERE reference=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recoveryCostNegative(t, tx, func() { recoveryCostExec(t, tx, tc.sql, increase.Reference) }, true)
		})
	}
	t.Run("reference_changed_with_matching_ledger_key", func(t *testing.T) {
		recoveryCostNegative(t, tx, func() {
			recoveryCostExec(t, tx, `UPDATE pg_temp.cost_evidence SET reference='new-reference' WHERE reference=$1`, increase.Reference)
			recoveryCostExec(t, tx, `UPDATE pg_temp.ledger_entries SET event_key=$2 WHERE event_key=$1`, "adjustment:"+increase.Source+":"+increase.Reference, "adjustment:"+increase.Source+":new-reference")
		}, true)
	})
	t.Run("balanced_wrong_final_booked_amount", func(t *testing.T) {
		recoveryCostNegative(t, tx, func() {
			recoveryCostExec(t, tx, `UPDATE pg_temp.ledger_entries SET delta_spent_microusd=delta_spent_microusd+7 WHERE event_key=$1`, "adjustment:"+increase.Source+":"+increase.Reference)
			recoveryCostExec(t, tx, `UPDATE pg_temp.reservations SET actual_microusd=actual_microusd+7 WHERE id=$1`, charged.ID)
			recoveryCostExec(t, tx, `UPDATE pg_temp.organizations SET spent_microusd=spent_microusd+7`)
			recoveryCostExec(t, tx, `UPDATE pg_temp.tasks SET spent_microusd=spent_microusd+7`)
		}, true)
	})
	t.Run("correct_hash_wrong_revision_chain", func(t *testing.T) {
		recoveryCostNegative(t, tx, func() {
			wrong := secondExpected + 1
			hash := recoveryCostAuthorHash(increase, &wrong)
			recoveryCostExec(t, tx, `UPDATE pg_temp.cost_evidence SET request_hash=$2 WHERE reference=$1`, increase.Reference, hash)
			recoveryCostExec(t, tx, `UPDATE pg_temp.ledger_entries SET event_hash=$2 WHERE event_key=$1`, "adjustment:"+increase.Source+":"+increase.Reference, hash)
		}, true)
	})
	t.Run("current_string_money_hash_is_not_historical_hash", func(t *testing.T) {
		recoveryCostNegative(t, tx, func() {
			body, _ := json.Marshal(struct {
				Evidence domain.CostEvidence
				Expected *int
			}{increase, &secondExpected})
			hash := Hash(body)
			recoveryCostExec(t, tx, `UPDATE pg_temp.cost_evidence SET request_hash=$2 WHERE reference=$1`, increase.Reference, hash)
			recoveryCostExec(t, tx, `UPDATE pg_temp.ledger_entries SET event_hash=$2 WHERE event_key=$1`, "adjustment:"+increase.Source+":"+increase.Reference, hash)
		}, true)
	})
	t.Run("missing_evidence_but_balanced_ledger", func(t *testing.T) {
		recoveryCostNegative(t, tx, func() {
			recoveryCostExec(t, tx, `DELETE FROM pg_temp.cost_evidence WHERE reference=$1`, increase.Reference)
		}, true)
	})
	t.Run("revision_without_transition", func(t *testing.T) {
		recoveryCostNegative(t, tx, func() {
			recoveryCostExec(t, tx, `UPDATE pg_temp.reservations SET revision=revision+1 WHERE id=$1`, zero.ID)
		}, true)
	})
	t.Run("lost_unknown_zero_history", func(t *testing.T) {
		recoveryCostNegative(t, tx, func() {
			recoveryCostExec(t, tx, `DELETE FROM pg_temp.ledger_entries WHERE reservation_id=$1 AND event_key='unknown:first'`, zero.ID)
		}, true)
	})
	t.Run("modern_cannot_be_unavailable", func(t *testing.T) {
		recoveryCostNegative(t, tx, func() {
			recoveryCostExec(t, tx, `UPDATE pg_temp.cost_evidence SET request_hash='legacy-unavailable' WHERE reference=$1`, increase.Reference)
		}, false)
	})
	t.Run("legacy_label_cannot_hide_existing_booking", func(t *testing.T) {
		recoveryCostNegative(t, tx, func() {
			recoveryCostExec(t, tx, `UPDATE pg_temp.cost_evidence SET id='evi_legacy_99',request_hash='legacy-unavailable' WHERE reference=$1`, increase.Reference)
		}, false)
	})
	t.Run("cancelled", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := checkRecoveryCostHistory(cancelled, tx, "pg_temp"); !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation not preserved")
		}
	})
	t.Run("invalid_schema", func(t *testing.T) {
		if _, err := checkRecoveryCostHistory(ctx, tx, "pg_temp;SELECT"); !errors.Is(err, ErrRecoveryCostHistory) {
			t.Fatal("identifier boundary failed")
		}
	})
	for _, table := range []string{"reservations", "ledger_entries", "cost_evidence", "cost_quarantines", "cost_quarantine_resolutions"} {
		var equal bool
		query := `SELECT NOT EXISTS((SELECT * FROM ` + pgx.Identifier{"pg_temp", table}.Sanitize() + ` EXCEPT SELECT * FROM ` + pgx.Identifier{s.runtimeSchema, table}.Sanitize() + `) UNION ALL (SELECT * FROM ` + pgx.Identifier{s.runtimeSchema, table}.Sanitize() + ` EXCEPT SELECT * FROM ` + pgx.Identifier{"pg_temp", table}.Sanitize() + `))`
		if err := tx.QueryRow(ctx, query).Scan(&equal); err != nil || !equal {
			t.Fatal("validator changed private row data")
		}
	}
}

func TestRecoveryCostHistoryAuthorQuarantineSequences(t *testing.T) {
	recoveryCostOwned(t)
	for _, state := range []string{"held", "released", "settled"} {
		t.Run(state, func(t *testing.T) {
			f := independentQuarantine(t, state)
			tx := recoveryInvariantStage(t, f.s)
			report := recoveryCostWant(t, tx)
			if report.LegacyOpaqueEvidence != 0 {
				t.Fatal("modern quarantine became opaque")
			}
			t.Run("changed_quarantined_payload", func(t *testing.T) {
				recoveryCostNegative(t, tx, func() {
					recoveryCostExec(t, tx, `UPDATE pg_temp.cost_evidence SET payload_digest=repeat('c',64) WHERE id=$1`, f.out.EvidenceID)
				}, true)
			})
			t.Run("changed_base_revision", func(t *testing.T) {
				recoveryCostNegative(t, tx, func() {
					recoveryCostExec(t, tx, `UPDATE pg_temp.cost_quarantines SET base_revision=base_revision+1,revision=revision+1,expected_revision=CASE WHEN expected_revision IS NULL THEN NULL ELSE expected_revision+1 END WHERE id=$1`, f.out.QuarantineID)
					recoveryCostExec(t, tx, `UPDATE pg_temp.reservations SET revision=revision+1 WHERE id=$1`, f.second.ID)
				}, true)
			})
			if err := tx.Rollback(context.Background()); err != nil {
				t.Fatal("pending stage rollback failed")
			}
			correction := independentEvidence(0)
			correction.Reason = "capacity correction"
			if _, err := f.s.AccountAdjustment(context.Background(), f.p.OrganizationID, f.first.Reservation.ID, domain.CostAdjustment{ExpectedRevision: f.first.Reservation.Revision, Evidence: correction}); err != nil {
				t.Fatal("capacity correction failed")
			}
			out, err := f.s.ResolveCostQuarantine(context.Background(), f.p.OrganizationID, f.out.QuarantineID, f.out.Reservation.Revision)
			if err != nil || out.Status != "settled" {
				t.Fatal("resolution failed")
			}
			last := independentEvidence(3)
			last.Reason = "post-resolution refund"
			if _, err = f.s.AccountAdjustment(context.Background(), f.p.OrganizationID, f.second.ID, domain.CostAdjustment{ExpectedRevision: out.Reservation.Revision, Evidence: last}); err != nil {
				t.Fatal("post-resolution correction failed")
			}
			resolved := recoveryInvariantStage(t, f.s)
			recoveryCostWant(t, resolved)
			t.Run("resolution_revision_reordered", func(t *testing.T) {
				recoveryCostNegative(t, resolved, func() {
					recoveryCostExec(t, resolved, `UPDATE pg_temp.cost_quarantine_resolutions SET resulting_revision=resulting_revision+1`)
				}, false)
			})
			t.Run("resolution_delta_changed", func(t *testing.T) {
				recoveryCostNegative(t, resolved, func() {
					recoveryCostExec(t, resolved, `UPDATE pg_temp.ledger_entries SET delta_spent_microusd=delta_spent_microusd+1 WHERE kind='quarantine_resolution'`)
				}, false)
			})
		})
	}
}

func TestRecoveryCostHistoryAuthorSyntheticAndLegacyCheckpoint(t *testing.T) {
	recoveryCostOwned(t)
	s, _ := testStore(t)
	p, _ := identity(t, s, 10000)
	a := agent(t, s, p)
	_ = task(t, s, p, a)
	lease, err := s.ClaimFixture(context.Background(), "cost-history-fixture", time.Minute)
	if err != nil || lease == nil {
		t.Fatal("fixture claim failed")
	}
	if err = s.CompleteFixture(context.Background(), lease, "Тестовый результат\n"+domain.ExecutionInput(*lease)); err != nil {
		t.Fatal("fixture completion failed")
	}
	tx := recoveryInvariantStage(t, s)
	if report := recoveryCostWant(t, tx); report.HistoriesChecked != 1 || report.ModernRequestsChecked != 0 {
		t.Fatal("wrong fixture report")
	}
	t.Run("synthetic_actual_forged_with_balanced_totals", func(t *testing.T) {
		recoveryCostNegative(t, tx, func() {
			recoveryCostExec(t, tx, `UPDATE pg_temp.reservations SET actual_microusd=actual_microusd+1`)
			recoveryCostExec(t, tx, `UPDATE pg_temp.ledger_entries SET delta_spent_microusd=delta_spent_microusd+1 WHERE kind='settle'`)
			recoveryCostExec(t, tx, `UPDATE pg_temp.organizations SET spent_microusd=spent_microusd+1`)
			recoveryCostExec(t, tx, `UPDATE pg_temp.tasks SET spent_microusd=spent_microusd+1`)
		}, true)
	})
	// Exact SQL003 marker/record shape. This is a format fixture, not an old
	// binary run; migration execution is separately tested in the upgrade suite.
	recoveryCostExec(t, tx, `DELETE FROM pg_temp.ledger_entries WHERE kind='reserve'`)
	recoveryCostExec(t, tx, `UPDATE pg_temp.ledger_entries SET id='legacy_'||reservation_id,event_key='legacy.import',event_hash='migration003',kind='legacy_import',delta_reserved_microusd=0`)
	recoveryCostExec(t, tx, `UPDATE pg_temp.reservations SET revision=1`)
	recoveryInvariantWantValid(t, tx)
	report := recoveryCostWant(t, tx)
	if report.LegacyImportFacts != 1 || len(report.OpaqueOrganizations) != 1 || report.OpaqueOrganizations[0] != p.OrganizationID {
		t.Fatal("legacy checkpoint uncertainty omitted")
	}
	t.Run("wrong_checkpoint_marker", func(t *testing.T) {
		recoveryCostNegative(t, tx, func() { recoveryCostExec(t, tx, `UPDATE pg_temp.ledger_entries SET event_hash='not-migration003'`) }, true)
	})
}

func TestRecoveryCostHistoryAuthorBounds(t *testing.T) {
	recoveryCostOwned(t)
	s, _ := testStore(t)
	p, _ := identity(t, s, 10000)
	a := agent(t, s, p)
	l := independentLease(t, s, p, a, "", 10000)
	r := independentReserve(t, s, l, 10)
	if _, err := s.AccountCost(context.Background(), p.OrganizationID, r.ID, independentEvidence(1)); err != nil {
		t.Fatal("bound fixture failed")
	}
	tx := recoveryInvariantStage(t, s)
	t.Run("oversized_text_refused_before_transfer", func(t *testing.T) {
		recoveryCostNegative(t, tx, func() { recoveryCostExec(t, tx, `UPDATE pg_temp.cost_evidence SET reason=repeat('x',300000)`) }, true)
	})
	t.Run("hash_work_bound", func(t *testing.T) {
		work := recoveryCostWork{hashes: recoveryCostMaxHashWork}
		if _, ok := work.hash(independentEvidence(math.MaxInt64), nil); ok {
			t.Fatal("hash bound ignored")
		}
	})
	t.Run("per_history_row_bound", func(t *testing.T) {
		recoveryCostNegative(t, tx, func() {
			recoveryCostExec(t, tx, `INSERT INTO pg_temp.ledger_entries SELECT organization_id,'bound_'||n,reservation_id,task_id,root_id,'unknown:bound_'||n,repeat('a',64),'unknown',0,0,synthetic,created_at FROM pg_temp.ledger_entries CROSS JOIN pg_catalog.generate_series(1,4096) n WHERE event_key='reserve'`)
		}, true)
	})
}

// Apply a real immutable historical prefix in a fresh schema. Current test
// constructors are used only inside the existing bounded historical shim; no
// old production binary or compatibility of old execution authority is claimed.
func recoveryCostHistoricalStore(t *testing.T, stop string) *Store {
	t.Helper()
	recoveryCostOwned(t)
	ctx := context.Background()
	dsn := os.Getenv("TEST_DATABASE_URL")
	base, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal("owned historical base unavailable")
	}
	schema := domain.NewID("test_cost_history")
	if _, err = base.Pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal("owned historical schema creation failed")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("owned historical URL unavailable")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal("owned historical connection failed")
	}
	t.Cleanup(func() {
		s.Close()
		_, err := base.Pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		base.Close()
		if err != nil {
			t.Error("owned historical schema cleanup failed")
		}
	})
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal("historical transaction failed")
	}
	defer tx.Rollback(ctx)
	recoveryCostExec(t, tx, `CREATE TABLE schema_migrations(name text PRIMARY KEY,sha256 text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())`)
	files, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal("embedded migrations unavailable")
	}
	for _, f := range files {
		if f.Name() >= stop {
			continue
		}
		name := "migrations/" + f.Name()
		body, err := migrations.ReadFile(name)
		if err != nil {
			t.Fatal("historical migration unavailable")
		}
		recoveryCostExec(t, tx, string(body))
		recoveryCostExec(t, tx, `INSERT INTO schema_migrations(name,sha256) VALUES($1,$2)`, name, Hash(body))
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal("historical prefix commit failed")
	}
	return s
}

func recoveryCostHistoricalFoundation(t *testing.T, s *Store) (domain.Principal, *domain.Lease) {
	t.Helper()
	ctx := context.Background()
	var b Bootstrap
	historicalFixtureWithoutRecoveryAdmission(t, s, func() {
		var err error
		b, err = s.Bootstrap(ctx, "Historical accounting", 10000)
		if err != nil {
			t.Fatal("historical bootstrap failed")
		}
	})
	p := domain.Principal{ID: b.PrincipalID, OrganizationID: b.OrganizationID, Role: "owner"}
	l := &domain.Lease{OrganizationID: p.OrganizationID, TaskID: domain.NewID("tsk"), RunID: domain.NewID("run"), OperationID: domain.NewID("op")}
	aid := domain.NewID("agt")
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal("historical foundation transaction failed")
	}
	defer tx.Rollback(ctx)
	recoveryCostExec(t, tx, `INSERT INTO agents(organization_id,id,name,instructions,model,budget_microusd,published) VALUES($1,$2,'History','History',$3,10000,true)`, p.OrganizationID, aid, domain.FixtureModel)
	recoveryCostExec(t, tx, `INSERT INTO tasks(organization_id,id,owner_id,agent_id,agent_version,goal,status,budget_microusd) VALUES($1,$2,$3,$4,1,'Historical cost','running',10000)`, p.OrganizationID, l.TaskID, p.ID, aid)
	recoveryCostExec(t, tx, `INSERT INTO runs(organization_id,id,task_id,principal_id,agent_id,agent_version,status) VALUES($1,$2,$3,$4,$5,1,'running')`, p.OrganizationID, l.RunID, l.TaskID, p.ID, aid)
	recoveryCostExec(t, tx, `INSERT INTO operations(organization_id,id,task_id,run_id,status) VALUES($1,$2,$3,$4,'queued')`, p.OrganizationID, l.OperationID, l.TaskID, l.RunID)
	if err = tx.Commit(ctx); err != nil {
		t.Fatal("historical foundation commit failed")
	}
	return p, l
}

func TestRecoveryCostHistoryAuthorActualSQL003Checkpoint(t *testing.T) {
	s := recoveryCostHistoricalStore(t, "003")
	p, l := recoveryCostHistoricalFoundation(t, s)
	ctx := context.Background()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal("pre003 population failed")
	}
	id := domain.NewID("res")
	recoveryCostExec(t, tx, `INSERT INTO reservations(organization_id,id,task_id,operation_id,amount_microusd,actual_microusd,status) VALUES($1,$2,$3,$4,1000,777,'settled')`, p.OrganizationID, id, l.TaskID, l.OperationID)
	recoveryCostExec(t, tx, `UPDATE organizations SET spent_microusd=777 WHERE id=$1`, p.OrganizationID)
	recoveryCostExec(t, tx, `UPDATE tasks SET spent_microusd=777 WHERE organization_id=$1`, p.OrganizationID)
	if err = tx.Commit(ctx); err != nil {
		t.Fatal("pre003 commit failed")
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("actual 003 onward upgrade failed")
	}
	stage := recoveryInvariantStage(t, s)
	recoveryInvariantWantValid(t, stage)
	report := recoveryCostWant(t, stage)
	if report.LegacyImportFacts != 1 || report.ModernRequestsChecked != 0 || report.HistoriesChecked != 1 || len(report.OpaqueOrganizations) != 1 {
		t.Fatal("actual 003 checkpoint not classified")
	}
	var exact bool
	if err := stage.QueryRow(ctx, `SELECT id='legacy_'||reservation_id AND event_hash='migration003' AND delta_spent_microusd=777 FROM pg_temp.ledger_entries`).Scan(&exact); err != nil || !exact {
		t.Fatal("migration did not create expected checkpoint")
	}
}

func TestRecoveryCostHistoryAuthorActualSQL004AndOpaqueFacts(t *testing.T) {
	s := recoveryCostHistoricalStore(t, "004")
	p, l := recoveryCostHistoricalFoundation(t, s)
	ctx := context.Background()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal("pre004 population failed")
	}
	settled, opaque := domain.NewID("res"), domain.NewID("res")
	for _, id := range []string{settled, opaque} {
		action := domain.NewID("act")
		state, revision, amount := "reserved", 1, int64(0)
		var actual any
		if id == settled {
			state, revision, amount, actual = "settled", 3, 100, int64(60)
		}
		recoveryCostExec(t, tx, `INSERT INTO actions(organization_id,id,task_id,operation_id,intent_hash,idempotency_key,status) VALUES($1,$2,$3,$4,$5,$2,'pending')`, p.OrganizationID, action, l.TaskID, l.OperationID, Hash([]byte("historical intent")))
		recoveryCostExec(t, tx, `INSERT INTO reservations(organization_id,id,task_id,operation_id,action_id,category,provider,price_version,amount_microusd,actual_microusd,status,revision) VALUES($1,$2,$3,$4,$5,'model','test-provider-account-A','historical', $6,$7,$8,$9)`, p.OrganizationID, id, l.TaskID, l.OperationID, action, amount, actual, state, revision)
		recoveryCostExec(t, tx, `INSERT INTO ledger_entries(organization_id,id,reservation_id,task_id,root_id,event_key,event_hash,kind,delta_reserved_microusd,delta_spent_microusd) VALUES($1,$2,$3,$4,$4,'reserve','historical-reserve','reserve',$5,0)`, p.OrganizationID, domain.NewID("led"), id, l.TaskID, amount)
	}
	first, adjust, unavailable := independentEvidence(50), independentEvidence(60), independentEvidence(0)
	adjust.Reason = "historical correction"
	for i, e := range []domain.CostEvidence{first, adjust, unavailable} {
		id := settled
		if i == 2 {
			id = opaque
		}
		recoveryCostExec(t, tx, `INSERT INTO cost_evidence(organization_id,source,reference,reservation_id,payload_digest,actual_microusd,reason) VALUES($1,$2,$3,$4,$5,$6,$7)`, p.OrganizationID, e.Source, e.Reference, id, e.PayloadDigest, e.ActualMicroUSD, e.Reason)
		if i == 2 {
			continue
		}
		kind, reserveDelta, spentDelta := "settle", int64(-100), int64(50)
		var expected *int
		if i == 1 {
			revision := 2
			expected = &revision
			kind, reserveDelta, spentDelta = "adjustment", 0, 10
		}
		recoveryCostExec(t, tx, `INSERT INTO ledger_entries(organization_id,id,reservation_id,task_id,root_id,event_key,event_hash,kind,delta_reserved_microusd,delta_spent_microusd) VALUES($1,$2,$3,$4,$4,$5,$6,$7,$8,$9)`, p.OrganizationID, domain.NewID("led"), id, l.TaskID, kind+":"+e.Source+":"+e.Reference, recoveryCostAuthorHash(e, expected), kind, reserveDelta, spentDelta)
	}
	recoveryCostExec(t, tx, `UPDATE organizations SET spent_microusd=60 WHERE id=$1`, p.OrganizationID)
	recoveryCostExec(t, tx, `UPDATE tasks SET spent_microusd=60 WHERE organization_id=$1`, p.OrganizationID)
	if err = tx.Commit(ctx); err != nil {
		t.Fatal("pre004 commit failed")
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("actual 004 onward upgrade failed")
	}
	modern := independentEvidence(0)
	if _, err = s.AccountCost(ctx, p.OrganizationID, opaque, modern); err != nil {
		t.Fatal("post004 modern fact failed")
	}
	stage := recoveryInvariantStage(t, s)
	recoveryInvariantWantValid(t, stage)
	report := recoveryCostWant(t, stage)
	if report.ModernRequestsChecked != 3 || report.LegacyOpaqueEvidence != 1 || report.HistoriesChecked != 1 || report.LegacyImportFacts != 0 || len(report.OpaqueOrganizations) != 1 || report.OpaqueOrganizations[0] != p.OrganizationID {
		t.Fatal("004 numeric/opaque report incorrect")
	}
	var exact bool
	if err = stage.QueryRow(ctx, `SELECT id ~ '^evi_legacy_[1-9][0-9]*$' AND request_hash='legacy-unavailable' FROM pg_temp.cost_evidence WHERE reference=$1`, unavailable.Reference).Scan(&exact); err != nil || !exact {
		t.Fatal("SQL004 unavailable marker fixture not exercised")
	}
	for _, e := range []domain.CostEvidence{first, modern} {
		t.Run("bound_payload_"+map[bool]string{true: "legacy", false: "modern"}[e.Reference == first.Reference], func(t *testing.T) {
			recoveryCostNegative(t, stage, func() {
				recoveryCostExec(t, stage, `UPDATE pg_temp.cost_evidence SET payload_digest=repeat('d',64) WHERE reference=$1`, e.Reference)
			}, true)
		})
	}
	t.Run("opaque_bad_identifier", func(t *testing.T) {
		recoveryCostNegative(t, stage, func() {
			recoveryCostExec(t, stage, `UPDATE pg_temp.cost_evidence SET id='evi_legacy_0' WHERE reference=$1`, unavailable.Reference)
		}, false)
	})
	t.Run("opaque_existing_booking_contradicts_004", func(t *testing.T) {
		recoveryCostNegative(t, stage, func() {
			recoveryCostExec(t, stage, `INSERT INTO pg_temp.ledger_entries SELECT organization_id,'contradictory_legacy',reservation_id,task_id,root_id,$2,repeat('a',64),'settle',0,0,false,created_at FROM pg_temp.ledger_entries WHERE reservation_id=$1 AND event_key='reserve'`, opaque, "settle:"+unavailable.Source+":"+unavailable.Reference)
		}, true)
	})
}
