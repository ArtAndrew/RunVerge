// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

func TestRecoveryAccountingAuthorInspectRealCommitLoss(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			f := evidenceAuthorSetup(t)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			if _, err := f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits()); err != nil {
				t.Fatal(err)
			}
			victim, proxy := independentImportCommitProxy(t, f.target.Pool.Config().ConnString())
			cut := proxy.arm(t, "insert into "+pgx.Identifier{victim.runtimeSchema, "recovery_accounting_batches"}.Sanitize(), mode)
			tx, err := victim.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			held, catalog, err := victim.evidenceTargetTx(ctx, tx, f.spec, f.before.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			inventory, err := recoverySnapshotInventory(ctx, tx, victim.runtimeSchema, true)
			if err != nil {
				t.Fatal(err)
			}
			inventory, err = recoverySourceInventory(inventory, f.before.InventorySHA256)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = victim.verifyRecoveryAccountingBaselineTx(ctx, tx, f.selected, f.before, DefaultRecoverySnapshotLimits(), held, catalog, inventory); err != nil {
				t.Fatal(err)
			}
			fact := domain.CostEvidence{Source: "author-owned-test", Reference: "lost-commit-owned-invoice", PayloadDigest: strings.Repeat("d", 64), ActualMicroUSD: 3, Final: true, Currency: "USD"}
			b := recoveryAccountingBatch{id: strings.TrimPrefix(domain.NewID("accounting"), "accounting_"), evidenceBatchID: f.verified.Link().OperationID, organizationID: f.oldOrg, requestSHA256: evidenceRequestHash(fact, nil), planSHA256: strings.Repeat("c", 64)}
			if err = tx.QueryRow(ctx, "SELECT id,revision FROM "+pgx.Identifier{victim.runtimeSchema, "reservations"}.Sanitize()+" WHERE organization_id=$1 AND status='unknown' AND amount_microusd=37", f.oldOrg).Scan(&b.reservationID, &b.expectedRevision); err != nil {
				t.Fatal(err)
			}
			if _, _, err = victim.recordRecoveryFinancialFactTx(ctx, tx, held, b, fact, nil); err != nil {
				t.Fatal("prepare financial change", err)
			}
			if err = tx.Commit(ctx); err == nil {
				t.Fatal("wire cut did not lose commit acknowledgement")
			}
			proxy.wait(t, cut)
			observer, err := Open(context.Background(), f.target.Pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			defer observer.Close()
			var first RecoveryAccountingInspection
			for i := 0; i < 2; i++ {
				result, err := observer.InspectRecoveryAccounting(ctx, f.selected, f.verified, f.spec, DefaultRecoverySnapshotLimits(), b.id, b.requestSHA256)
				if err != nil || !result.ExecutionHeld || !result.RollbackConfirmed {
					t.Fatal("fresh inspection failed", err)
				}
				if mode == "before_commit" {
					if result.State != "not_recorded" || result.HeadOrdinal != 0 || result.MutationCount != 0 {
						t.Fatal("aborted commit reported as recorded")
					}
				} else if result.State != "recorded" || result.HeadID != b.id || result.HeadOrdinal != 1 || result.BatchOrdinal != 1 || result.MutationCount == 0 {
					t.Fatal("committed change not recognized")
				}
				if i == 0 {
					first = result
				} else if !reflect.DeepEqual(first, result) {
					t.Fatal("inspection changed the outcome")
				}
			}
			if mode == "after_commit" {
				if _, err = observer.InspectRecoveryAccounting(ctx, f.selected, f.verified, f.spec, DefaultRecoverySnapshotLimits(), b.id, strings.Repeat("e", 64)); err == nil {
					t.Fatal("another request claimed this commit")
				}
			}
			wrong := f.spec
			wrong.TargetInstallationID = "other-target"
			if _, err = observer.InspectRecoveryAccounting(ctx, f.selected, f.verified, wrong, DefaultRecoverySnapshotLimits(), b.id, b.requestSHA256); err == nil {
				t.Fatal("wrong target accepted")
			}
			var count int
			if err = observer.Pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{observer.runtimeSchema, "cost_evidence"}.Sanitize()+" WHERE organization_id=$1 AND source=$2 AND reference=$3", f.oldOrg, fact.Source, fact.Reference).Scan(&count); err != nil || (mode == "before_commit" && count != 0) || (mode == "after_commit" && count != 1) {
				t.Fatal("inspection changed invoice count", err)
			}
		})
	}
}
