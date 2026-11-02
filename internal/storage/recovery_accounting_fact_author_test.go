// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

func TestRecoveryAccountingAuthorTrustedFactsAndCorrections(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		name := "settle_adjust_zero"
		if overflow {
			name = "overflow_quarantine"
		}
		t.Run(name, func(t *testing.T) {
			f := evidenceAuthorSetup(t)
			ctx := context.Background()
			if _, err := f.target.IngestRecoveryEvidence(ctx, f.selected, f.late, f.verified, f.spec, DefaultRecoverySnapshotLimits()); err != nil {
				t.Fatal(err)
			}
			q := func(table string) string { return pgx.Identifier{f.target.runtimeSchema, table}.Sanitize() }
			var reservation, zeroReservation string
			if f.target.Pool.QueryRow(ctx, "SELECT id FROM "+q("reservations")+" WHERE organization_id=$1 AND status='unknown' AND amount_microusd=37", f.oldOrg).Scan(&reservation) != nil {
				t.Fatal("paid reservation missing")
			}
			if f.target.Pool.QueryRow(ctx, "SELECT id FROM "+q("reservations")+" WHERE organization_id=$1 AND status='unknown' AND amount_microusd=0", f.oldOrg).Scan(&zeroReservation) != nil {
				t.Fatal("zero reservation missing")
			}
			execution := func() json.RawMessage {
				t.Helper()
				var result json.RawMessage
				query := `SELECT jsonb_build_object('tasks',(SELECT jsonb_agg(to_jsonb(t)-ARRAY['reserved_microusd','spent_microusd','financial_hold'] ORDER BY organization_id,id) FROM ` + q("tasks") + ` t),'actions',(SELECT jsonb_agg(to_jsonb(a) ORDER BY organization_id,id) FROM ` + q("actions") + ` a),'events',(SELECT count(*) FROM ` + q("events") + `),'outbox',(SELECT count(*) FROM ` + q("outbox") + `))`
				if f.target.Pool.QueryRow(ctx, query).Scan(&result) != nil {
					t.Fatal("execution snapshot failed")
				}
				return result
			}
			originalExecution := execution()
			apply := func(id string, evidence domain.CostEvidence, expected *int, wantErr bool) (domain.AccountingOutcome, recoveryAccountingReceipt) {
				t.Helper()
				tx, err := f.target.Pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				held, catalog, err := f.target.evidenceTargetTx(ctx, tx, f.spec, f.before.SHA256)
				if err != nil {
					t.Fatal(err)
				}
				inventory, err := recoverySnapshotInventory(ctx, tx, f.target.runtimeSchema, true)
				if err != nil {
					t.Fatal(err)
				}
				inventory, err = recoverySourceInventory(inventory, f.before.InventorySHA256)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.target.verifyRecoveryAccountingBaselineTx(ctx, tx, f.selected, f.before, DefaultRecoverySnapshotLimits(), held, catalog, inventory); err != nil {
					t.Fatal("pre-apply baseline", err)
				}
				b := recoveryAccountingBatch{id: strings.TrimPrefix(domain.NewID("accounting"), "accounting_"), evidenceBatchID: f.verified.Link().OperationID, organizationID: f.oldOrg, reservationID: id, requestSHA256: evidenceRequestHash(evidence, expected), planSHA256: strings.Repeat("c", 64)}
				if tx.QueryRow(ctx, "SELECT revision FROM "+q("reservations")+" WHERE organization_id=$1 AND id=$2", f.oldOrg, id).Scan(&b.expectedRevision) != nil {
					t.Fatal("revision missing")
				}
				out, receipt, err := f.target.recordRecoveryFinancialFactTx(ctx, tx, held, b, evidence, expected)
				if wantErr {
					if err == nil {
						t.Fatal("conflicting fact accepted")
					}
					return out, receipt
				}
				if err != nil {
					t.Fatal("financial fact apply", err)
				}
				if err = tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if string(originalExecution) != string(execution()) {
					t.Fatal("billing changed execution/authority history")
				}
				if f.target.CheckRuntimeAdmission(ctx) != RecoveryHeld {
					t.Fatal("billing enabled work")
				}
				return out, receipt
			}
			fact := domain.CostEvidence{Source: "author-owned-test", Reference: "recovery-owned-invoice", PayloadDigest: strings.Repeat("d", 64), ActualMicroUSD: 7, Final: true, Currency: "USD"}
			if overflow {
				fact.ActualMicroUSD = math.MaxInt64
			}
			out, first := apply(reservation, fact, nil, false)
			if first.Ordinal != 1 {
				t.Fatal("first receipt missing")
			}
			if overflow {
				if out.Status != "quarantined" || out.Reservation.ActualMicroUSD != nil || out.QuarantineID == "" || out.Reservation.PendingQuarantineID != out.QuarantineID {
					t.Fatal("overflow lost original liability/fact")
				}
			} else if out.Status != "settled" || out.Reservation.ActualMicroUSD == nil || *out.Reservation.ActualMicroUSD != 7 {
				t.Fatal("invoice not booked")
			}
			replay, noChange := apply(reservation, fact, nil, false)
			if noChange.ID != "" || replay.EvidenceID != out.EvidenceID || replay.Status != out.Status {
				t.Fatal("same invoice applied twice")
			}
			conflict := fact
			conflict.PayloadDigest = strings.Repeat("e", 64)
			apply(reservation, conflict, nil, true)
			if !overflow {
				revision := out.Reservation.Revision
				correction := fact
				correction.Reference = "recovery-negative-correction"
				correction.ActualMicroUSD = 3
				correction.Reason = "owned correction"
				adjusted, second := apply(reservation, correction, &revision, false)
				if second.Ordinal != 2 || second.PreviousID != first.ID || adjusted.Reservation.ActualMicroUSD == nil || *adjusted.Reservation.ActualMicroUSD != 3 {
					t.Fatal("second financial transition failed")
				}
				zero := fact
				zero.Reference = "recovery-confirmed-zero"
				zero.ActualMicroUSD = 0
				confirmed, third := apply(zeroReservation, zero, nil, false)
				if third.Ordinal != 3 || confirmed.Reservation.ActualMicroUSD == nil || *confirmed.Reservation.ActualMicroUSD != 0 || confirmed.Reservation.Status != "settled" {
					t.Fatal("known zero confused with unknown")
				}
				// Reopen the baseline in a fresh transaction after all commits.
				apply(zeroReservation, zero, nil, false)
			}
		})
	}
}
