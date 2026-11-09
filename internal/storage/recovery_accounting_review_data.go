// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

type recoveryReviewHistory struct {
	history        *recoveryCostHistory
	position       RecoveryFinancialPosition
	immutableRows  map[string]string
	evidenceRows   map[string]string
	quarantineRows map[string]string
	supported      bool
}
type recoveryReviewSnapshot struct {
	organizations map[string]*RecoveryFinancialAccount
	histories     map[recoveryCostKey]*recoveryReviewHistory
}

// Shared across all three views. The report is all-or-error, never silently
// truncated to a reassuring prefix of the financial history.
type recoveryReviewWork struct {
	rows, bytes int
	cost        recoveryCostWork
}

func (w *recoveryReviewWork) add(values ...string) error {
	w.rows++
	for _, v := range values {
		w.bytes += len(v)
	}
	if w.rows > 3*recoveryPlanMaxItems || w.bytes > 32<<20 {
		return ErrRecoveryAccounting
	}
	return nil
}

func readRecoveryReviewSnapshot(ctx context.Context, tx pgx.Tx, schema string, work *recoveryReviewWork) (result recoveryReviewSnapshot, err error) {
	result = recoveryReviewSnapshot{organizations: map[string]*RecoveryFinancialAccount{}, histories: map[recoveryCostKey]*recoveryReviewHistory{}}
	q := func(table string) string { return pgx.Identifier{schema, table}.Sanitize() }
	for _, check := range recoveryInvariantQueries(schema) {
		if check.name != "reservation_projection" && check.name != "organization_projection" && check.name != "task_projection" {
			continue
		}
		var valid bool
		if tx.QueryRow(ctx, check.sql).Scan(&valid) != nil || !valid {
			return result, ErrRecoveryAccounting
		}
	}
	rows, err := tx.Query(ctx, "SELECT id,budget_microusd,reserved_microusd,spent_microusd,financial_hold FROM "+q("organizations")+" ORDER BY id COLLATE \"C\" LIMIT 10001")
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var org string
		var a RecoveryFinancialAccount
		if rows.Scan(&org, &a.Budget, &a.Reserved, &a.Booked, &a.FinancialHold) != nil || len(org) > 128 || len(result.organizations) >= recoveryPlanMaxItems || work.add(org) != nil {
			rows.Close()
			return result, ErrRecoveryAccounting
		}
		result.organizations[org] = &a
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	histories, err := recoveryCostRead(ctx, tx, schema)
	if err != nil {
		return result, err
	}
	for _, h := range histories {
		legacy, opaque, _, valid := recoveryCostCheck(ctx, h, &work.cost)
		if !valid || len(result.histories) >= recoveryPlanMaxItems || work.cost.hashes > recoveryCostMaxHashWork {
			return result, ErrRecoveryAccounting
		}
		a := result.organizations[h.org]
		if a == nil {
			return result, ErrRecoveryAccounting
		}
		if h.status == "unknown" {
			a.UnknownCount++
		}
		if h.pending != "" {
			a.PendingQuarantineCount++
		}
		result.histories[recoveryCostKey{h.org, h.id}] = &recoveryReviewHistory{history: h, position: RecoveryFinancialPosition{Category: h.category, Provider: h.provider, Status: h.status, Amount: h.amount, Actual: h.actual, Revision: h.revision, PendingQuarantineID: h.pending, TaskID: h.task, RootID: h.root, OperationID: h.operation}, immutableRows: map[string]string{}, evidenceRows: map[string]string{}, quarantineRows: map[string]string{}, supported: legacy == 0 && opaque == 0 && h.category != "fixture_legacy"}
	}
	rows, err = tx.Query(ctx, "SELECT organization_id,id,encode(sha256(convert_to((to_jsonb(r)-ARRAY['status','actual_microusd','revision','pending_quarantine_id'])::text,'UTF8')),'hex') FROM "+q("reservations")+" r LIMIT 10001")
	if err != nil {
		return result, err
	}
	count := 0
	for rows.Next() {
		var org, id, hash string
		if rows.Scan(&org, &id, &hash) != nil || work.add(org, id, hash) != nil {
			rows.Close()
			return result, ErrRecoveryAccounting
		}
		h := result.histories[recoveryCostKey{org, id}]
		if h == nil || h.position.ImmutableSHA256 != "" {
			rows.Close()
			return result, ErrRecoveryAccounting
		}
		h.position.ImmutableSHA256 = hash
		count++
	}
	err = rows.Err()
	rows.Close()
	if err != nil || count != len(result.histories) {
		return result, ErrRecoveryAccounting
	}
	for _, table := range []string{"cost_evidence", "ledger_entries", "cost_quarantines", "cost_quarantine_resolutions", "financial_incidents"} {
		key := "jsonb_build_array(organization_id,id)"
		if table == "cost_evidence" {
			key = "jsonb_build_array(organization_id,source,reference)"
		}
		rows, err = tx.Query(ctx, "SELECT organization_id,reservation_id,id,"+key+",encode(sha256(convert_to(to_jsonb(r)::text,'UTF8')),'hex') FROM "+q(table)+" r LIMIT 10001")
		if err != nil {
			return result, err
		}
		count = 0
		for rows.Next() {
			var org, res, id, hash string
			var key json.RawMessage
			if rows.Scan(&org, &res, &id, &key, &hash) != nil || len(key) > 2048 || count >= recoveryPlanMaxItems || work.add(org, res, id, string(key), hash) != nil {
				rows.Close()
				return result, ErrRecoveryAccounting
			}
			h := result.histories[recoveryCostKey{org, res}]
			if h == nil {
				rows.Close()
				return result, ErrRecoveryAccounting
			}
			h.immutableRows[table+"\x00"+string(key)] = hash
			if table == "cost_evidence" {
				h.evidenceRows[id] = hash
			}
			if table == "cost_quarantines" {
				h.quarantineRows[id] = hash
			}
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return result, err
		}
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}

func recoveryReviewSource(original, captured *recoveryReviewHistory, transitions []recoveryFinancialSourceTransition, evidenceID string) recoveryAccountingSource {
	e := captured.history.evidence[evidenceID]
	result := recoveryAccountingSource{original: original.history, later: captured.history, transitions: transitions, evidence: e, rowSHA256: captured.evidenceRows[evidenceID], identitySHA256: original.position.ImmutableSHA256, rowSHA256ByID: captured.evidenceRows, originalEvidenceSHA256: original.evidenceRows[evidenceID]}
	if old := original.history.evidence[evidenceID]; old != nil && old.q != nil {
		result.originalQuarantineSHA256 = original.quarantineRows[old.q.id]
	}
	return result
}
