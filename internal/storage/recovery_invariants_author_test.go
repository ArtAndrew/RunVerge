// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

func recoveryInvariantOwned(t *testing.T) {
	t.Helper()
	if os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" {
		t.Skip("fresh owned PostgreSQL harness required")
	}
}

// Mirrors the importer's intended typed temporary-table boundary, not its
// implementation. LIKE retains CHECK/NOT NULL/unique indexes; it does not copy
// application triggers, FKs, sequence defaults or generated/identity behavior.
func recoveryInvariantStage(t *testing.T, s *Store) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal("owned staging transaction unavailable")
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	inventory, err := recoverySnapshotInventory(ctx, tx, s.runtimeSchema, true)
	if err != nil {
		t.Fatal("trusted source inventory unavailable")
	}
	for _, table := range inventory.Tables {
		if !table.Exported {
			continue
		}
		source := pgx.Identifier{s.runtimeSchema, table.Name}.Sanitize()
		target := pgx.Identifier{"pg_temp", table.Name}.Sanitize()
		if _, err = tx.Exec(ctx, "CREATE TEMP TABLE "+target+" (LIKE "+source+" INCLUDING CONSTRAINTS INCLUDING INDEXES) ON COMMIT DROP"); err != nil {
			t.Fatalf("typed stage creation failed: %s", table.Name)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO "+target+" SELECT * FROM "+source); err != nil {
			t.Fatalf("typed stage population failed: %s", table.Name)
		}
	}
	return tx
}

func recoveryInvariantWantValid(t *testing.T, tx pgx.Tx) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Names are safe diagnostic categories. Never print row payloads, the DSN,
	// underlying database messages, or archived operational identifiers.
	for _, q := range recoveryInvariantQueries("pg_temp") {
		var valid bool
		if err := tx.QueryRow(ctx, q.sql).Scan(&valid); err != nil {
			t.Fatalf("invariant query failed: %s", q.name)
		}
		if !valid {
			t.Fatalf("valid historical fixture rejected: %s", q.name)
		}
	}
	if err := validateRecoveryInvariants(ctx, tx, "pg_temp"); err != nil {
		t.Fatal("aggregate invariant check rejected valid fixture")
	}
}

func TestRecoveryInvariantsAuthorSafeBoundary(t *testing.T) {
	if !errors.Is(validateRecoveryInvariants(nil, nil, "pg_temp"), ErrRecoveryInvariant) {
		t.Fatal("nil API input did not fail closed")
	}
	for _, q := range recoveryInvariantQueries("pg_temp") {
		if strings.ContainsAny(q.sql, "{}") || strings.Contains(q.sql, ";") {
			t.Fatal("unexpanded or multi-statement query")
		}
		if !strings.Contains(q.sql, `"pg_temp".`) {
			t.Fatal("query omitted explicit schema")
		}
		for _, verb := range []string{"INSERT ", "UPDATE ", "DELETE ", "ALTER ", "DROP ", "SET "} {
			if strings.Contains(q.sql, verb) {
				t.Fatal("validator query mutates data")
			}
		}
	}
}

func TestRecoveryInvariantsAuthorMoneyLineageAndHistory(t *testing.T) {
	recoveryInvariantOwned(t)
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 10000)
	a := agent(t, s, p)
	root := independentLease(t, s, p, a, "", 10000)
	child := independentLease(t, s, p, a, root.TaskID, 10000)
	zero := independentReserve(t, s, child, 0)
	if _, err := s.MarkCostUnknown(ctx, p.OrganizationID, zero.ID, "zero-unknown", "known test uncertainty"); err != nil {
		t.Fatal("zero unknown fixture failed")
	}
	reserved := independentReserve(t, s, child, 37)
	// A lower historical budget is a valid state. It blocks future admission,
	// independently of whether a sticky financial-hold reason exists.
	if _, err := s.Pool.Exec(ctx, "UPDATE tasks SET budget_microusd=1 WHERE organization_id=$1", p.OrganizationID); err != nil {
		t.Fatal("historical budget fixture failed")
	}
	tx := recoveryInvariantStage(t, s)
	recoveryInvariantWantValid(t, tx)
	cases := []struct {
		name, sql string
		args      []any
	}{
		{"organization_projection", `UPDATE pg_temp.organizations SET reserved_microusd=reserved_microusd+1`, nil},
		{"ancestor_projection", `UPDATE pg_temp.tasks SET reserved_microusd=reserved_microusd+1 WHERE id=$1`, []any{root.TaskID}},
		{"ledger_sum", `UPDATE pg_temp.ledger_entries SET delta_reserved_microusd=delta_reserved_microusd+1 WHERE reservation_id=$1 AND event_key='reserve'`, []any{reserved.ID}},
		{"unknown_zero_ledger_disappearance", `DELETE FROM pg_temp.ledger_entries WHERE reservation_id=$1`, []any{zero.ID}},
		{"wrong_scope_order", `UPDATE pg_temp.reservations SET scope_task_ids=ARRAY[task_id,root_id] WHERE id=$1`, []any{reserved.ID}},
		{"duplicate_scope", `UPDATE pg_temp.reservations SET scope_task_ids=scope_task_ids||task_id WHERE id=$1`, []any{zero.ID}},
		{"wrong_scope_root", `UPDATE pg_temp.reservations SET root_id=task_id WHERE id=$1`, []any{reserved.ID}},
		{"cyclic_root", `UPDATE pg_temp.tasks SET parent_id=id WHERE id=$1`, []any{root.TaskID}},
		{"wrong_child_depth", `UPDATE pg_temp.tasks SET depth=2 WHERE id=$1`, []any{child.TaskID}},
		{"wrong_child_owner", `UPDATE pg_temp.tasks SET owner_id='different_actor' WHERE id=$1`, []any{child.TaskID}},
		{"wrong_ledger_task", `UPDATE pg_temp.ledger_entries SET task_id=$1 WHERE reservation_id=$2`, []any{root.TaskID, reserved.ID}},
		{"wrong_synthetic_flag", `UPDATE pg_temp.ledger_entries SET synthetic=true WHERE reservation_id=$1`, []any{zero.ID}},
		{"tree_counter", `UPDATE pg_temp.task_tree_controls SET used_tasks=used_tasks+1`, nil},
		{"attempt_counter", `UPDATE pg_temp.tasks SET attempt_count=attempt_count+1 WHERE id=$1`, []any{child.TaskID}},
		{"input_digest", `UPDATE pg_temp.runs SET input_context='changed',input_digest=repeat('0',64)`, nil},
		{"future_authority", `UPDATE pg_temp.operations SET principal_authority_epoch=principal_authority_epoch+1`, nil},
		{"future_policy", `UPDATE pg_temp.runs SET owner_policy_epoch=owner_policy_epoch+1`, nil},
		{"missing_policy", `DELETE FROM pg_temp.organization_login_policies`, nil},
		{"missing_actor_membership", `DELETE FROM pg_temp.memberships`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tx.Exec(ctx, "SAVEPOINT invariant_case"); err != nil {
				t.Fatal("savepoint failed")
			}
			defer func() {
				if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT invariant_case"); err != nil {
					t.Fatal("case rollback failed")
				}
			}()
			if tag, err := tx.Exec(ctx, tc.sql, tc.args...); err != nil || tag.RowsAffected() == 0 {
				t.Fatal("negative fixture mutation failed")
			}
			if err := validateRecoveryInvariants(ctx, tx, "pg_temp"); !errors.Is(err, ErrRecoveryInvariant) {
				t.Fatal("inconsistent projection was accepted")
			}
		})
	}
	t.Run("stale_authority_and_inactive_history", func(t *testing.T) {
		if _, err := tx.Exec(ctx, `UPDATE pg_temp.memberships SET security_epoch=2,active=false`); err != nil {
			t.Fatal("stale authority fixture failed")
		}
		if _, err := tx.Exec(ctx, `UPDATE pg_temp.organization_login_policies SET human_authority_epoch=3`); err != nil {
			t.Fatal("stale policy fixture failed")
		}
		recoveryInvariantWantValid(t, tx)
	})
	t.Run("cancelled", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if !errors.Is(validateRecoveryInvariants(cancelled, tx, "pg_temp"), context.Canceled) {
			t.Fatal("cancellation lost")
		}
	})
	t.Run("missing_schema_safe_error", func(t *testing.T) {
		if !errors.Is(validateRecoveryInvariants(ctx, tx, "missing_private_schema"), ErrRecoveryInvariant) {
			t.Fatal("missing schema did not return fixed error")
		}
	})
}

func TestRecoveryInvariantsAuthorQuarantineHistory(t *testing.T) {
	recoveryInvariantOwned(t)
	for _, state := range []string{"held", "released", "settled"} {
		t.Run(state, func(t *testing.T) {
			// This existing fixture uses actual storage accounting, including overflow,
			// valid signed corrections and immutable evidence. These are author tests
			// of the new validator, not an independent acceptance claim.
			f := independentQuarantine(t, state)
			tx := recoveryInvariantStage(t, f.s)
			ctx := context.Background()
			recoveryInvariantWantValid(t, tx)
			for name, sql := range map[string]string{
				"missing_pending":           `UPDATE pg_temp.reservations SET pending_quarantine_id=NULL WHERE pending_quarantine_id IS NOT NULL`,
				"wrong_revision":            `UPDATE pg_temp.reservations SET revision=revision+1 WHERE pending_quarantine_id IS NOT NULL`,
				"cleared_organization_hold": `UPDATE pg_temp.organizations SET financial_hold=false`,
				"cleared_task_hold":         `UPDATE pg_temp.tasks SET financial_hold=false`,
				"missing_quarantine_ledger": `DELETE FROM pg_temp.ledger_entries WHERE kind='quarantine'`,
				"wrong_provider":            `UPDATE pg_temp.cost_evidence SET source='different-provider'`,
			} {
				t.Run(name, func(t *testing.T) {
					if _, err := tx.Exec(ctx, "SAVEPOINT quarantine_case"); err != nil {
						t.Fatal("savepoint failed")
					}
					defer func() {
						if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT quarantine_case"); err != nil {
							t.Fatal("case rollback failed")
						}
					}()
					if tag, err := tx.Exec(ctx, sql); err != nil || tag.RowsAffected() == 0 {
						t.Fatal("quarantine mutation failed")
					}
					if !errors.Is(validateRecoveryInvariants(ctx, tx, "pg_temp"), ErrRecoveryInvariant) {
						t.Fatal("invalid quarantine accepted")
					}
				})
			}
		})
	}
}

func TestRecoveryInvariantsAuthorActualStatusShape(t *testing.T) {
	recoveryInvariantOwned(t)
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 10000)
	a := agent(t, s, p)
	l := independentLease(t, s, p, a, "", 10000)
	byStatus := map[string]domain.Reservation{}
	for _, status := range []string{"reserved", "unknown", "released", "settled"} {
		r := independentReserve(t, s, l, 10)
		var err error
		switch status {
		case "unknown":
			r, err = s.MarkCostUnknown(ctx, p.OrganizationID, r.ID, "actual-status", "synthetic unresolved cost")
		case "released":
			r, err = s.ReleaseUnsentCost(ctx, p.OrganizationID, r.ID, "actual-status")
		case "settled":
			if err = s.PrepareCostDispatch(ctx, l, r.ID); err != nil {
				t.Fatal("settled status dispatch fixture failed")
			}
			r, err = s.SettleCost(ctx, p.OrganizationID, r.ID, independentEvidence(0))
		}
		if err != nil || r.Status != status {
			t.Fatal("actual status fixture failed")
		}
		byStatus[status] = r
	}
	tx := recoveryInvariantStage(t, s)
	recoveryInvariantWantValid(t, tx)
	cases := []struct {
		name, status string
		actual       any
		delta        int64
	}{
		{"reserved_zero", "reserved", int64(0), 0},
		{"reserved_balanced_positive", "reserved", int64(7), 7},
		{"unknown_zero", "unknown", int64(0), 0},
		{"unknown_balanced_positive", "unknown", int64(7), 7},
		{"released_zero", "released", int64(0), 0},
		{"released_balanced_positive", "released", int64(7), 7},
		{"settled_missing_zero", "settled", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tx.Exec(ctx, "SAVEPOINT actual_status_case"); err != nil {
				t.Fatal("status savepoint failed")
			}
			defer func() {
				if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT actual_status_case"); err != nil {
					t.Fatal("status rollback failed")
				}
			}()
			r := byStatus[tc.status]
			if tag, err := tx.Exec(ctx, `UPDATE pg_temp.reservations SET actual_microusd=$2 WHERE id=$1`, r.ID, tc.actual); err != nil || tag.RowsAffected() != 1 {
				t.Fatal("status mutation failed")
			}
			// Preserve every amount projection deliberately. Only the impossible
			// state/actual shape, not an unbalanced counter, should reject these rows.
			if tag, err := tx.Exec(ctx, `UPDATE pg_temp.ledger_entries SET delta_spent_microusd=delta_spent_microusd+$2 WHERE reservation_id=$1 AND event_key='reserve'`, r.ID, tc.delta); err != nil || tag.RowsAffected() != 1 {
				t.Fatal("balanced ledger mutation failed")
			}
			for _, statement := range []string{
				`UPDATE pg_temp.organizations SET spent_microusd=spent_microusd+$1`,
				`UPDATE pg_temp.tasks SET spent_microusd=spent_microusd+$1`,
			} {
				if _, err := tx.Exec(ctx, statement, tc.delta); err != nil {
					t.Fatal("balanced counter mutation failed")
				}
			}
			for _, q := range recoveryInvariantQueries("pg_temp") {
				var valid bool
				if err := tx.QueryRow(ctx, q.sql).Scan(&valid); err != nil || valid != (q.name != "reservation_projection") {
					t.Fatalf("status fixture did not isolate the state invariant: %s", q.name)
				}
			}
			if !errors.Is(validateRecoveryInvariants(ctx, tx, "pg_temp"), ErrRecoveryInvariant) {
				t.Fatal("impossible balanced status was accepted")
			}
		})
	}
}

func TestRecoveryInvariantsAuthorCredentialAndContentBindings(t *testing.T) {
	recoveryInvariantOwned(t)
	s, _ := testStore(t)
	ctx := context.Background()
	p, _ := identity(t, s, 10000)
	a := agent(t, s, p)
	_ = task(t, s, p, a)
	lease, err := s.ClaimFixture(ctx, "recovery-invariant-author", time.Minute)
	if err != nil || lease == nil {
		t.Fatal("candidate fixture claim failed")
	}
	if err = s.CompleteFixture(ctx, lease, "Тестовый результат\n"+domain.ExecutionInput(*lease)); err != nil {
		t.Fatal("candidate fixture failed")
	}
	tx := recoveryInvariantStage(t, s)
	recoveryInvariantWantValid(t, tx)
	cases := map[string]string{
		"candidate_hash":           `UPDATE pg_temp.task_candidates SET content_digest=repeat('0',64)`,
		"artifact_content":         `UPDATE pg_temp.artifacts SET content='different synthetic content'`,
		"outbox_cross_scope":       `UPDATE pg_temp.outbox SET organization_id='different_organization'`,
		"human_token_service_kind": `UPDATE pg_temp.principals SET kind='service',service_organization_id=(SELECT id FROM pg_temp.organizations LIMIT 1)`,
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := tx.Exec(ctx, "SAVEPOINT binding_case"); err != nil {
				t.Fatal("savepoint failed")
			}
			defer func() {
				if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT binding_case"); err != nil {
					t.Fatal("case rollback failed")
				}
			}()
			if tag, err := tx.Exec(ctx, sql); err != nil || tag.RowsAffected() == 0 {
				t.Fatal("binding mutation failed")
			}
			if !errors.Is(validateRecoveryInvariants(ctx, tx, "pg_temp"), ErrRecoveryInvariant) {
				t.Fatal("invalid historical binding accepted")
			}
		})
	}
	if _, err := tx.Exec(ctx, `INSERT INTO pg_temp.principals(id,name,active,kind,service_organization_id) VALUES('service_author','test service',false,'service',$1)`, p.OrganizationID); err != nil {
		t.Fatal("service principal fixture failed")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO pg_temp.memberships SELECT organization_id,'service_author','viewer',false,'test service',version,security_epoch,credential_cutoff,created_at,updated_at,credentials_revoked_at FROM pg_temp.memberships WHERE principal_id=$1`, p.ID); err != nil {
		t.Fatal("service membership fixture failed")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO pg_temp.service_credentials(organization_id,id,principal_id,token_hash,role_ceiling,ttl_seconds,version,revoked,credential_generation,created_at,expires_at)
 VALUES($1,'credential_author','service_author',repeat('f',64),'viewer',60,2,true,200000,'2026-10-28 10:00:00Z','2026-10-28 10:01:00Z')`, p.OrganizationID); err != nil {
		t.Fatal("service credential fixture failed")
	}
	recoveryInvariantWantValid(t, tx)
	for name, sql := range map[string]string{
		"cross_domain_hash_collision":       `UPDATE pg_temp.service_credentials SET token_hash=(SELECT token_hash FROM pg_temp.access_tokens LIMIT 1)`,
		"cross_domain_generation_collision": `UPDATE pg_temp.service_credentials SET credential_generation=(SELECT credential_generation FROM pg_temp.access_tokens LIMIT 1)`,
		"service_membership_owner":          `UPDATE pg_temp.memberships SET role='owner' WHERE principal_id='service_author'`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := tx.Exec(ctx, "SAVEPOINT credential_case"); err != nil {
				t.Fatal("savepoint failed")
			}
			defer func() {
				if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT credential_case"); err != nil {
					t.Fatal("case rollback failed")
				}
			}()
			if _, err := tx.Exec(ctx, sql); err != nil {
				t.Fatal("credential mutation failed")
			}
			if !errors.Is(validateRecoveryInvariants(ctx, tx, "pg_temp"), ErrRecoveryInvariant) {
				t.Fatal("invalid credential identity accepted")
			}
		})
	}
	// Same generation legitimately belongs to multiple rotated sessions only
	// when the identity/authentication/expiry family is unchanged.
	if _, err := tx.Exec(ctx, `INSERT INTO pg_temp.identity_mappings(issuer,subject,principal_id,active,created_at) VALUES('https://identity.invalid','subject',$1,false,'2026-10-28 10:00:00Z')`, p.ID); err != nil {
		t.Fatal("identity fixture failed")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO pg_temp.browser_sessions(id,token_hash,principal_id,issuer,subject,authenticated_at,created_at,expires_at,revoked,credential_generation)
 SELECT x,repeat(x,64),$1,'https://identity.invalid','subject','2026-10-28 10:00:00Z','2026-10-28 10:00:00Z','2026-10-28 11:00:00Z',true,100000 FROM (VALUES('a'),('b')) n(x)`, p.ID); err != nil {
		t.Fatal("rotation fixture failed")
	}
	recoveryInvariantWantValid(t, tx)
	t.Run("two_live_rotation_successors", func(t *testing.T) {
		if _, err := tx.Exec(ctx, "SAVEPOINT rotation_case"); err != nil {
			t.Fatal("savepoint failed")
		}
		defer func() {
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT rotation_case"); err != nil {
				t.Fatal("case rollback failed")
			}
		}()
		if _, err := tx.Exec(ctx, `UPDATE pg_temp.browser_sessions SET revoked=false`); err != nil {
			t.Fatal("rotation mutation failed")
		}
		if !errors.Is(validateRecoveryInvariants(ctx, tx, "pg_temp"), ErrRecoveryInvariant) {
			t.Fatal("two unrevoked successors accepted")
		}
	})
	if _, err := tx.Exec(ctx, `UPDATE pg_temp.browser_sessions SET expires_at=expires_at+interval '1 minute' WHERE id='b'`); err != nil {
		t.Fatal("rotation mutation failed")
	}
	if !errors.Is(validateRecoveryInvariants(ctx, tx, "pg_temp"), ErrRecoveryInvariant) {
		t.Fatal("cross-family credential generation accepted")
	}
}

func TestRecoveryInvariantsAuthorResolvedQuarantine(t *testing.T) {
	recoveryInvariantOwned(t)
	f := independentQuarantine(t, "held")
	ctx := context.Background()
	adjustment := independentEvidence(0)
	adjustment.Reason = "correct prior synthetic accepted fact"
	if _, err := f.s.AccountAdjustment(ctx, f.p.OrganizationID, f.first.Reservation.ID, domain.CostAdjustment{ExpectedRevision: f.first.Reservation.Revision, Evidence: adjustment}); err != nil {
		t.Fatal("quarantine capacity correction fixture failed")
	}
	if out, err := f.s.ResolveCostQuarantine(ctx, f.p.OrganizationID, f.out.QuarantineID, f.out.Reservation.Revision); err != nil || out.Status != "settled" {
		t.Fatal("quarantine resolution fixture failed")
	}
	tx := recoveryInvariantStage(t, f.s)
	recoveryInvariantWantValid(t, tx)
	if _, err := tx.Exec(ctx, `UPDATE pg_temp.cost_quarantine_resolutions SET resulting_revision=resulting_revision+1`); err != nil {
		t.Fatal("resolution mutation failed")
	}
	if !errors.Is(validateRecoveryInvariants(ctx, tx, "pg_temp"), ErrRecoveryInvariant) {
		t.Fatal("wrong quarantine resulting revision accepted")
	}
}
