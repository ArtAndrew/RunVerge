// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// These are real guard/executor tests, not acceptance of the pending financial
// semantic builder. A sticky hold is the one harmless prescribed update here.
func TestRecoveryAccountingAuthorExactMutationGuards(t *testing.T) {
	f := evidenceAuthorSetup(t)
	ctx := context.Background()
	if _, err := f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	start := func(t *testing.T) (pgx.Tx, RecoveryTargetState, recoveryAccountingBatch, recoveryAccountingMutation) {
		t.Helper()
		tx, err := f.target.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(ctx) })
		held, _, err := f.target.evidenceTargetTx(ctx, tx, f.spec, f.before.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "SET LOCAL search_path="+pgx.Identifier{f.target.runtimeSchema}.Sanitize()+",pg_catalog,pg_temp"); err != nil {
			t.Fatal(err)
		}
		b := recoveryAccountingBatch{id: strings.TrimPrefix(domain.NewID("accounting"), "accounting_"), evidenceBatchID: f.verified.Link().OperationID, organizationID: f.oldOrg, requestSHA256: strings.Repeat("a", 64), planSHA256: strings.Repeat("b", 64)}
		if tx.QueryRow(ctx, "SELECT id,revision FROM reservations WHERE organization_id=$1 AND status='unknown' AND amount_microusd=37", f.oldOrg).Scan(&b.reservationID, &b.expectedRevision) != nil {
			t.Fatal("reservation fixture missing")
		}
		m := recoveryAccountingMutation{table: "organizations"}
		if tx.QueryRow(ctx, "SELECT to_jsonb(o),to_jsonb(o)||jsonb_build_object('financial_hold',true) FROM organizations o WHERE id=$1 AND NOT financial_hold", f.oldOrg).Scan(&m.before, &m.after) != nil {
			t.Fatal("organization fixture already held")
		}
		return tx, held, b, m
	}
	prescribe := func(t *testing.T, tx pgx.Tx, held RecoveryTargetState, b recoveryAccountingBatch, m recoveryAccountingMutation, wrongBefore bool) error {
		t.Helper()
		if wrongBefore {
			m.before = m.after
		}
		_, err := tx.Exec(ctx, `INSERT INTO recovery_accounting_batches(id,restore_id,target_installation_id,evidence_batch_id,organization_id,reservation_id,expected_revision,ordinal,request_sha256,plan_sha256,mutation_count,mutation_bytes)
 VALUES($1,$2,$3,$4,$5,$6,$7,1,$8,$9,1,octet_length(($10::jsonb)::text)+octet_length(($11::jsonb)::text))`, b.id, held.RestoreID, held.InstallationID, b.evidenceBatchID, b.organizationID, b.reservationID, b.expectedRevision, b.requestSHA256, b.planSHA256, string(m.before), string(m.after))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO recovery_accounting_mutations(batch_id,ordinal,table_name,operation,key_parts,before_row,after_row)
 VALUES($1,0,'organizations','UPDATE',jsonb_build_array($2::text),$3::jsonb,$4::jsonb)`, b.id, b.organizationID, string(m.before), string(m.after))
		return err
	}
	t.Run("wrong_actual_baseline", func(t *testing.T) {
		tx, h, b, m := start(t)
		independentRecoverySQLState(t, prescribe(t, tx, h, b, m, true), "55000")
	})
	t.Run("unconsumed_batch_cannot_commit", func(t *testing.T) {
		tx, h, b, m := start(t)
		if err := prescribe(t, tx, h, b, m, false); err != nil {
			t.Fatal(err)
		}
		independentRecoverySQLState(t, tx.Commit(ctx), "55000")
	})
	t.Run("temporary_observer_lookalike", func(t *testing.T) {
		tx, h, b, m := start(t)
		if err := prescribe(t, tx, h, b, m, false); err != nil {
			t.Fatal(err)
		}
		qualified := pgx.Identifier{f.target.runtimeSchema, "organizations"}.Sanitize()
		if _, err := tx.Exec(ctx, "CREATE TEMP TABLE organizations (LIKE "+qualified+") ON COMMIT DROP"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO pg_temp.organizations SELECT r.* FROM jsonb_populate_record(NULL::"+qualified+",$1::jsonb) r", string(m.before)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "CREATE TRIGGER spoof_observe AFTER UPDATE ON pg_temp.organizations FOR EACH ROW EXECUTE FUNCTION "+pgx.Identifier{f.target.runtimeSchema, "recovery_accounting_observe"}.Sanitize()+"()"); err != nil {
			t.Fatal(err)
		}
		_, err := tx.Exec(ctx, "UPDATE pg_temp.organizations SET financial_hold=true")
		independentRecoverySQLState(t, err, "55000")
	})
	t.Run("nested_forged_consumption", func(t *testing.T) {
		tx, h, b, m := start(t)
		if err := prescribe(t, tx, h, b, m, false); err != nil {
			t.Fatal(err)
		}
		// A private trigger supplies the same depth as the real observer. It must
		// still fail because the real organization's financial state is unchanged.
		if _, err := tx.Exec(ctx, "CREATE TEMP TABLE forged_accounting(batch_id text) ON COMMIT DROP"); err != nil {
			t.Fatal(err)
		}
		applied := pgx.Identifier{f.target.runtimeSchema, "recovery_accounting_applied"}.Sanitize()
		mutations := pgx.Identifier{f.target.runtimeSchema, "recovery_accounting_mutations"}.Sanitize()
		if _, err := tx.Exec(ctx, `CREATE FUNCTION pg_temp.forge_accounting() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN
 INSERT INTO `+applied+` SELECT batch_id,ordinal,encode(sha256(convert_to(before_row::text,'UTF8')),'hex'),encode(sha256(convert_to(after_row::text,'UTF8')),'hex') FROM `+mutations+` WHERE batch_id=NEW.batch_id;
 RETURN NULL; END $body$;
 CREATE TRIGGER forge AFTER INSERT ON pg_temp.forged_accounting FOR EACH ROW EXECUTE FUNCTION pg_temp.forge_accounting()`); err != nil {
			t.Fatal(err)
		}
		_, err := tx.Exec(ctx, "INSERT INTO pg_temp.forged_accounting VALUES($1)", b.id)
		independentRecoverySQLState(t, err, "55000")
	})
	t.Run("extra_financial_or_identity_field", func(t *testing.T) {
		tx, h, b, m := start(t)
		if err := prescribe(t, tx, h, b, m, false); err != nil {
			t.Fatal(err)
		}
		_, err := tx.Exec(ctx, "UPDATE organizations SET financial_hold=true,budget_microusd=budget_microusd+1 WHERE id=$1", f.oldOrg)
		independentRecoverySQLState(t, err, "55000")
	})
	t.Run("early_completeness_does_not_allow_reconsumption", func(t *testing.T) {
		tx, h, b, m := start(t)
		if _, err := f.target.recordRecoveryAccountingMutationsTx(ctx, tx, h, b, []recoveryAccountingMutation{m}); err != nil {
			t.Fatal(err)
		}
		_, err := tx.Exec(ctx, "UPDATE organizations SET financial_hold=true WHERE id=$1", f.oldOrg)
		independentRecoverySQLState(t, err, "55000")
	})
	t.Run("typed_executor_commits_only_prescribed_hold", func(t *testing.T) {
		tx, h, b, m := start(t)
		unchanged := func() json.RawMessage {
			var result json.RawMessage
			if tx.QueryRow(ctx, `SELECT jsonb_build_object('tasks',(SELECT jsonb_agg(to_jsonb(t) ORDER BY organization_id,id) FROM tasks t),'actions',(SELECT jsonb_agg(to_jsonb(a) ORDER BY organization_id,id) FROM actions a),'events',(SELECT count(*) FROM events),'outbox',(SELECT count(*) FROM outbox))`).Scan(&result) != nil {
				t.Fatal("execution observation failed")
			}
			return result
		}
		before := unchanged()
		receipt, err := f.target.recordRecoveryAccountingMutationsTx(ctx, tx, h, b, []recoveryAccountingMutation{m})
		if err != nil || receipt.Ordinal != 1 || receipt.MutationCount != 1 {
			t.Fatal("exact mutation failed", err)
		}
		if string(before) != string(unchanged()) {
			t.Fatal("financial hold changed execution history")
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if f.target.CheckRuntimeAdmission(ctx) != RecoveryHeld {
			t.Fatal("accounting enabled work")
		}
		var count int
		if f.target.Pool.QueryRow(ctx, "SELECT count(*) FROM recovery_accounting_applied WHERE batch_id=$1", b.id).Scan(&count) != nil || count != 1 {
			t.Fatal("committed mutation receipt missing")
		}
	})
}
