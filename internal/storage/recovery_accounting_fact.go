// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Only a trusted recovery caller may supply a final fact. The caller first
// authenticates its provenance, verifies the full original->receipt->current
// baseline, and holds the exclusive recovery gate. This private executor never
// starts or commits a transaction, and is not a tenant/API mutation switch.
func (s *Store) recordRecoveryFinancialFactTx(ctx context.Context, tx pgx.Tx, held RecoveryTargetState, b recoveryAccountingBatch, evidence domain.CostEvidence, expected *int) (domain.AccountingOutcome, recoveryAccountingReceipt, error) {
	return s.recordRecoveryFinancialChangeTx(ctx, tx, held, b, evidence, expected, nil)
}

// The shared builder records either a new trusted fact or the application of an
// already recorded quarantined fact. Resolution cannot replace that fact.
func (s *Store) recordRecoveryFinancialChangeTx(ctx context.Context, tx pgx.Tx, held RecoveryTargetState, b recoveryAccountingBatch, evidence domain.CostEvidence, expected *int, resolution *recoveryAccountingResolution) (domain.AccountingOutcome, recoveryAccountingReceipt, error) {
	var empty domain.AccountingOutcome
	var receipt recoveryAccountingReceipt
	if !validEvidence(evidence) || (expected != nil && (*expected < 1 || strings.TrimSpace(evidence.Reason) == "")) || (resolution == nil && b.requestSHA256 != evidenceRequestHash(evidence, expected)) {
		return empty, receipt, ErrRecoveryAccounting
	}
	q := func(table string) string { return pgx.Identifier{s.runtimeSchema, table}.Sanitize() }
	// Existing pure locking/arithmetic helpers use unqualified trusted tables.
	// Explicit pg_temp last prevents temporary material from selecting authority.
	if _, err := tx.Exec(ctx, "SET LOCAL search_path="+pgx.Identifier{s.runtimeSchema}.Sanitize()+",pg_catalog,pg_temp"); err != nil {
		return empty, receipt, ErrRecoveryAccounting
	}
	r, err := scanReservation(tx.QueryRow(ctx, "SELECT "+reservationColumns+" FROM "+q("reservations")+" WHERE organization_id=$1 AND id=$2", b.organizationID, b.reservationID))
	if err != nil || r.Provider != evidence.Source || r.ActionID == "" || r.Category == "fixture_legacy" {
		return empty, receipt, ErrRecoveryAccounting
	}
	var historical bool
	if tx.QueryRow(ctx, "SELECT "+pgx.Identifier{s.runtimeSchema, "recovery_is_historical"}.Sanitize()+"('reservations',jsonb_build_array($1::text,$2::text))", r.OrganizationID, r.ID).Scan(&historical) != nil || !historical {
		return empty, receipt, ErrRecoveryAccounting
	}
	scopes, err := lockCostScopes(ctx, tx, r.OrganizationID, r.TaskID)
	if err != nil {
		return empty, receipt, err
	}
	if resolution != nil {
		out, e := accountingOutcomeTx(ctx, tx, r, resolution.evidenceID)
		if e != nil || out.Status == "settled" {
			return out, receipt, e
		}
		wantStatus := resolution.baseStatus
		if wantStatus == "reserved" {
			wantStatus = "unknown"
		}
		if int64(r.Revision) != b.expectedRevision || r.PendingQuarantineID != resolution.quarantineID || r.Revision != resolution.revision || r.Status != wantStatus || !sameAmount(r.ActualMicroUSD, resolution.baseActual) {
			return empty, receipt, ErrRecoveryAccounting
		}
		reserveDelta, spentDelta := costDeltas(r, evidence.ActualMicroUSD)
		if preflightCostDelta(scopes, reserveDelta, spentDelta) != nil {
			return out, receipt, nil
		}
	} else {
		// Exact fact replay is a read of the recorded outcome, never a second batch.
		var oldEvidence, oldReservation, oldHash string
		err = tx.QueryRow(ctx, "SELECT id,reservation_id,request_hash FROM "+q("cost_evidence")+" WHERE organization_id=$1 AND source=$2 AND reference=$3", r.OrganizationID, evidence.Source, evidence.Reference).Scan(&oldEvidence, &oldReservation, &oldHash)
		if err == nil {
			if oldReservation != r.ID || oldHash != b.requestSHA256 {
				return empty, receipt, ErrRecoveryAccounting
			}
			out, e := accountingOutcomeTx(ctx, tx, r, oldEvidence)
			return out, receipt, e
		}
		if !errors.Is(err, pgx.ErrNoRows) || int64(r.Revision) != b.expectedRevision || r.PendingQuarantineID != "" || (expected == nil && r.Status == "settled") || (expected != nil && (r.Status != "settled" || r.Revision != *expected)) {
			return empty, receipt, ErrRecoveryAccounting
		}
	}
	var actionState string
	if tx.QueryRow(ctx, "SELECT status FROM "+q("actions")+" WHERE organization_id=$1 AND id=$2", r.OrganizationID, r.ActionID).Scan(&actionState) != nil {
		return empty, receipt, ErrRecoveryAccounting
	}
	var now time.Time
	if tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now) != nil {
		return empty, receipt, ErrRecoveryAccounting
	}
	var changes []recoveryAccountingMutation
	insert := func(table string, fields map[string]any) error {
		fields["created_at"] = now
		raw, e := json.Marshal(fields)
		if e != nil {
			return ErrRecoveryAccounting
		}
		m := recoveryAccountingMutation{table: table}
		if tx.QueryRow(ctx, "SELECT to_jsonb(r) FROM jsonb_populate_record(NULL::"+q(table)+",$1::jsonb) r", string(raw)).Scan(&m.after) != nil {
			return ErrRecoveryAccounting
		}
		changes = append(changes, m)
		return nil
	}
	update := func(table, id string, fields map[string]any) error {
		raw, e := json.Marshal(fields)
		if e != nil {
			return ErrRecoveryAccounting
		}
		m := recoveryAccountingMutation{table: table}
		var changed bool
		predicate := "id=$1 AND organization_id=$2"
		args := []any{id, r.OrganizationID, string(raw)}
		if table == "organizations" {
			predicate = "id=$1 AND id=$2"
		}
		if tx.QueryRow(ctx, "SELECT to_jsonb(t),to_jsonb(t)||$3::jsonb,to_jsonb(t) IS DISTINCT FROM (to_jsonb(t)||$3::jsonb) FROM "+q(table)+" t WHERE "+predicate, args...).Scan(&m.before, &m.after, &changed) != nil {
			return ErrRecoveryAccounting
		}
		if changed {
			changes = append(changes, m)
		}
		return nil
	}
	evidenceID := domain.NewID("evi")
	evidenceHash := b.requestSHA256
	if resolution != nil {
		evidenceID, evidenceHash = resolution.evidenceID, resolution.evidenceSHA256
	} else if err = insert("cost_evidence", map[string]any{"organization_id": r.OrganizationID, "id": evidenceID, "source": evidence.Source, "reference": evidence.Reference, "reservation_id": r.ID, "payload_digest": evidence.PayloadDigest, "actual_microusd": evidence.ActualMicroUSD, "reason": evidence.Reason, "request_hash": evidenceHash, "final": evidence.Final, "currency": evidence.Currency}); err != nil {
		return empty, receipt, err
	}
	kind := "settle"
	if expected != nil {
		kind = "adjustment"
	}
	reserveDelta, spentDelta := costDeltas(r, evidence.ActualMicroUSD)
	quarantined := preflightCostDelta(scopes, reserveDelta, spentDelta) != nil
	incidentKind := ""
	if quarantined {
		incidentKind = "projection_overflow"
	} else if r.Status == "released" && evidence.ActualMicroUSD > 0 {
		incidentKind = "late_charge_after_release"
	} else if actionState == "pending" && evidence.ActualMicroUSD > 0 {
		incidentKind = "charge_without_dispatch"
	} else if evidence.ActualMicroUSD > r.AmountMicroUSD {
		incidentKind = "price_bound_exceeded"
	}
	qid := ""
	if quarantined {
		qid = domain.NewID("quar")
		if err = insert("cost_quarantines", map[string]any{"organization_id": r.OrganizationID, "id": qid, "evidence_id": evidenceID, "reservation_id": r.ID, "base_revision": r.Revision, "revision": r.Revision + 1, "base_status": r.Status, "base_actual_microusd": r.ActualMicroUSD, "requested_kind": kind, "expected_revision": expected, "reason": "projection_overflow"}); err != nil {
			return empty, receipt, err
		}
		reserveDelta, spentDelta = 0, 0
	}
	// Account totals change only through the exact reservation deltas. Late
	// source organization totals may include unmatched identities and are never
	// copied. Holds are sticky; task status/version/timestamps stay untouched.
	reserved, err := checkedCounter(scopes.reserved, reserveDelta)
	if err != nil {
		return empty, receipt, err
	}
	spent, err := checkedCounter(scopes.spent, spentDelta)
	if err != nil {
		return empty, receipt, err
	}
	hold := scopes.hold || incidentKind != "" || spent > scopes.budget || reserved > scopes.budget-spent
	if err = update("organizations", r.OrganizationID, map[string]any{"reserved_microusd": reserved, "spent_microusd": spent, "financial_hold": hold}); err != nil {
		return empty, receipt, err
	}
	for _, task := range scopes.tasks {
		reserved, e := checkedCounter(task.ReservedMicroUSD, reserveDelta)
		if e != nil {
			return empty, receipt, e
		}
		spent, e := checkedCounter(task.SpentMicroUSD, spentDelta)
		if e != nil {
			return empty, receipt, e
		}
		hold := task.FinancialHold || incidentKind != "" || spent > task.BudgetMicroUSD || reserved > task.BudgetMicroUSD-spent
		if e = update("tasks", task.ID, map[string]any{"reserved_microusd": reserved, "spent_microusd": spent, "financial_hold": hold}); e != nil {
			return empty, receipt, e
		}
	}
	status := "settled"
	var actual any = evidence.ActualMicroUSD
	var pending any
	if quarantined {
		status = r.Status
		if status == "reserved" {
			status = "unknown"
		}
		actual = r.ActualMicroUSD
		pending = qid
	}
	if err = update("reservations", r.ID, map[string]any{"status": status, "actual_microusd": actual, "revision": r.Revision + 1, "pending_quarantine_id": pending}); err != nil {
		return empty, receipt, err
	}
	if incidentKind != "" {
		if err = insert("financial_incidents", map[string]any{"organization_id": r.OrganizationID, "id": domain.NewID("inc"), "reservation_id": r.ID, "task_id": r.TaskID, "kind": incidentKind, "amount_microusd": evidence.ActualMicroUSD}); err != nil {
			return empty, receipt, err
		}
	}
	key := kind + ":" + evidence.Source + ":" + evidence.Reference
	if quarantined {
		kind, key = "quarantine", "quarantine:"+qid
	}
	if resolution != nil {
		kind, key = "quarantine_resolution", "resolve:"+resolution.quarantineID
		if err = insert("cost_quarantine_resolutions", map[string]any{"organization_id": r.OrganizationID, "id": domain.NewID("qres"), "quarantine_id": resolution.quarantineID, "reservation_id": r.ID, "evidence_id": evidenceID, "resulting_revision": r.Revision + 1}); err != nil {
			return empty, receipt, err
		}
	}
	if err = insert("ledger_entries", map[string]any{"organization_id": r.OrganizationID, "id": domain.NewID("led"), "reservation_id": r.ID, "task_id": r.TaskID, "root_id": r.RootID, "event_key": key, "event_hash": evidenceHash, "kind": kind, "delta_reserved_microusd": reserveDelta, "delta_spent_microusd": spentDelta, "synthetic": false}); err != nil {
		return empty, receipt, err
	}
	receipt, err = s.recordRecoveryAccountingMutationsTx(ctx, tx, held, b, changes)
	if err != nil {
		return empty, receipt, err
	}
	if err = validateRecoveryInvariants(ctx, tx, s.runtimeSchema); err != nil {
		return empty, receipt, err
	}
	if _, err = checkRecoveryCostHistory(ctx, tx, s.runtimeSchema); err != nil {
		return empty, receipt, err
	}
	r, err = scanReservation(tx.QueryRow(ctx, "SELECT "+reservationColumns+" FROM "+q("reservations")+" WHERE organization_id=$1 AND id=$2", r.OrganizationID, r.ID))
	if err != nil {
		return empty, receipt, err
	}
	out := domain.AccountingOutcome{Status: "settled", EvidenceID: evidenceID, Reservation: r}
	if quarantined {
		out.Status, out.QuarantineID = "quarantined", qid
	} else if resolution != nil {
		out.QuarantineID = resolution.quarantineID
	}
	return out, receipt, nil
}
