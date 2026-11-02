// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

func TestRecoveryAccountingAuthorChainExplainsOnlyExactChanges(t *testing.T) {
	f := evidenceAuthorSetup(t)
	ctx := context.Background()
	if _, err := f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal(err)
	}
	q := func(table string) string { return pgx.Identifier{f.target.runtimeSchema, table}.Sanitize() }
	begin := func() (pgx.Tx, RecoveryTargetState, RecoverySemanticCatalog) {
		t.Helper()
		tx, err := f.target.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held, catalog, err := f.target.evidenceTargetTx(ctx, tx, f.spec, f.before.SHA256)
		if err != nil {
			tx.Rollback(ctx)
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(ctx) })
		return tx, held, catalog
	}
	verify := func(tx pgx.Tx, held RecoveryTargetState, catalog RecoverySemanticCatalog) (recoveryAccountingReceipt, error) {
		t.Helper()
		inventory, err := recoverySnapshotInventory(ctx, tx, f.target.runtimeSchema, true)
		if err != nil {
			t.Fatal(err)
		}
		inventory, err = recoverySourceInventory(inventory, f.before.InventorySHA256)
		if err != nil {
			t.Fatal(err)
		}
		return f.target.verifyRecoveryAccountingBaselineTx(ctx, tx, f.selected, f.before, DefaultRecoverySnapshotLimits(), held, catalog, inventory)
	}
	tx, held, catalog := begin()
	if head, err := verify(tx, held, catalog); err != nil || head.ID != "" {
		t.Fatal("unchanged original baseline", err)
	}
	_ = tx.Rollback(ctx)
	var previous string
	for index, table := range []string{"organizations", "tasks"} {
		tx, held, _ = begin()
		b := recoveryAccountingBatch{id: strings.TrimPrefix(domain.NewID("accounting"), "accounting_"), evidenceBatchID: f.verified.Link().OperationID, organizationID: f.oldOrg, requestSHA256: strings.Repeat("a", 64), planSHA256: strings.Repeat("b", 64)}
		var taskID string
		if tx.QueryRow(ctx, "SELECT id,revision,task_id FROM "+q("reservations")+" WHERE organization_id=$1 AND status='unknown' AND amount_microusd=37", f.oldOrg).Scan(&b.reservationID, &b.expectedRevision, &taskID) != nil {
			t.Fatal("missing reservation")
		}
		m := recoveryAccountingMutation{table: table}
		query := "SELECT to_jsonb(r),to_jsonb(r)||jsonb_build_object('financial_hold',true) FROM " + q(table) + " r WHERE id=$1 AND NOT financial_hold"
		args := []any{f.oldOrg}
		if table == "tasks" {
			query += " AND organization_id=$2"
			args = []any{taskID, f.oldOrg}
		}
		if err := tx.QueryRow(ctx, query, args...).Scan(&m.before, &m.after); err != nil {
			t.Fatal(err)
		}
		receipt, err := f.target.recordRecoveryAccountingMutationsTx(ctx, tx, held, b, []recoveryAccountingMutation{m})
		if err != nil || receipt.Ordinal != int64(index+1) || receipt.PreviousID != previous {
			t.Fatal("incorrect receipt chain", err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		previous = receipt.ID
		tx, held, catalog = begin()
		head, err := verify(tx, held, catalog)
		if err != nil || head.ID != previous || head.Ordinal != int64(index+1) {
			t.Fatal("applied baseline not reconstructed", err)
		}
		// A second verification on the same backend must not encounter leftover
		// temporary relations from the first verified snapshot.
		if second, err := verify(tx, held, catalog); err != nil || second != head {
			t.Fatal("chain verification not repeatable", err)
		}
		_ = tx.Rollback(ctx)
	}
	tx, held, catalog = begin()
	// Owned negative corruption outside the supported operator path. Restore
	// every guard before validation; the catalogue alone must not hide a row edit.
	for _, statement := range []string{"ALTER TABLE " + q("organizations") + " DISABLE TRIGGER USER", "UPDATE " + q("organizations") + " SET name='unexplained synthetic replacement'", "ALTER TABLE " + q("organizations") + " ENABLE TRIGGER USER"} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := verify(tx, held, catalog); err == nil {
		t.Fatal("unexplained identity change accepted")
	}
	_ = tx.Rollback(ctx)
	if f.target.CheckRuntimeAdmission(ctx) != RecoveryHeld {
		t.Fatal("verification admitted execution")
	}
}
