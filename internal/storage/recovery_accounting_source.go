// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"

	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
)

type recoveryAccountingInputs struct {
	selected, late  *recoveryarchive.VerifiedArchive
	verified        *recoveryarchive.VerifiedCaptureLink
	original, later RecoverySnapshotReport
	held            RecoveryTargetState
	catalog         RecoverySemanticCatalog
	inventory       RecoverySnapshotInventory
	limits          RecoverySnapshotLimits
}

type recoveryAccountingSource struct {
	original, later *recoveryCostHistory
	transitions     []recoveryFinancialSourceTransition
	evidence        *recoveryCostEvidence
	step            *recoveryFinancialSourceTransition
	rowSHA256       string
	identitySHA256  string
	rowSHA256ByID   map[string]string
}

// The caller has authenticated the archives and exact committed evidence and
// holds the recovery gate. Staging here is independent of the now-reconciled
// target: source continuity always starts at the actual original snapshot.
func (s *Store) loadRecoveryAccountingSourceTx(ctx context.Context, tx pgx.Tx, in recoveryAccountingInputs, org, reservation, evidenceID string) (result recoveryAccountingSource, resultErr error) {
	var originalRows map[string]string
	for index, archive := range []*recoveryarchive.VerifiedArchive{in.selected, in.late} {
		snapshot := in.original
		if index == 1 {
			snapshot = in.later
		}
		inventory, err := recoverySourceInventory(in.inventory, snapshot.InventorySHA256)
		if err != nil {
			return result, err
		}
		staged, err := s.stageRecoveryTypedDataTx(ctx, tx, archive, snapshot, in.limits, in.held, in.catalog, inventory)
		if err != nil {
			return result, err
		}
		histories, err := recoveryCostRead(ctx, tx, staged.schema)
		if err != nil {
			return result, err
		}
		var h *recoveryCostHistory
		for _, candidate := range histories {
			if candidate.org == org && candidate.id == reservation {
				h = candidate
				break
			}
		}
		if h == nil || h.category == "fixture_legacy" {
			return result, ErrRecoveryAccounting
		}
		legacy, opaque, _, valid := recoveryCostCheck(ctx, h, &recoveryCostWork{})
		if !valid || legacy != 0 || opaque != 0 {
			return result, ErrRecoveryAccounting
		}
		q := func(table string) string { return pgx.Identifier{staged.schema, table}.Sanitize() }
		var identity string
		if tx.QueryRow(ctx, "SELECT encode(sha256(convert_to((to_jsonb(r)-ARRAY['status','actual_microusd','revision','pending_quarantine_id'])::text,'UTF8')),'hex') FROM "+q("reservations")+" r WHERE organization_id=$1 AND id=$2", org, reservation).Scan(&identity) != nil {
			return result, ErrRecoveryAccounting
		}
		digests, err := recoveryAccountingSourceDigestsTx(ctx, tx, staged.schema, org, reservation)
		if err != nil {
			return result, err
		}
		if index == 0 {
			result.original, result.identitySHA256, originalRows = h, identity, digests
		} else {
			if identity != result.identitySHA256 {
				return result, ErrRecoveryAccounting
			}
			// Hashes include complete immutable rows, not just request hashes.
			for key, old := range originalRows {
				if digests[key] != old {
					return result, ErrRecoveryAccounting
				}
			}
			result.later, result.evidence = h, h.evidence[evidenceID]
			if result.evidence == nil {
				return result, ErrRecoveryAccounting
			}
			if tx.QueryRow(ctx, "SELECT encode(sha256(convert_to(to_jsonb(e)::text,'UTF8')),'hex') FROM "+q("cost_evidence")+" e WHERE organization_id=$1 AND reservation_id=$2 AND id=$3", org, reservation, evidenceID).Scan(&result.rowSHA256) != nil {
				return result, ErrRecoveryAccounting
			}
			rows, err := tx.Query(ctx, "SELECT id,encode(sha256(convert_to(to_jsonb(e)::text,'UTF8')),'hex') FROM "+q("cost_evidence")+" e WHERE organization_id=$1 AND reservation_id=$2 LIMIT 10001", org, reservation)
			if err != nil {
				return result, ErrRecoveryAccounting
			}
			result.rowSHA256ByID = map[string]string{}
			for rows.Next() {
				var id, hash string
				if rows.Scan(&id, &hash) != nil || len(result.rowSHA256ByID) >= recoveryPlanMaxItems {
					rows.Close()
					return result, ErrRecoveryAccounting
				}
				result.rowSHA256ByID[id] = hash
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return result, ErrRecoveryAccounting
			}
		}
		if err = dropRecoveryAccountingStageTx(ctx, tx, staged, snapshot); err != nil {
			return result, err
		}
	}
	work := &recoveryCostWork{}
	var valid bool
	result.transitions, valid = recoveryCostContinuationTransitions(ctx, result.original, result.later, work)
	if !valid || ctx.Err() != nil || work.hashes > recoveryCostMaxHashWork {
		return result, ErrRecoveryAccounting
	}
	for index := range result.transitions {
		step := &result.transitions[index]
		if step.kind == "fact" && step.evidence.id == evidenceID {
			result.step = step
		}
	}
	if result.step == nil && result.original.evidence[evidenceID] == nil {
		return result, ErrRecoveryAccounting
	}
	return result, nil
}

// Fixed financial relations and complete physical keys. A later archive must
// retain every old row unchanged, including its identity and timestamps.
func recoveryAccountingSourceDigestsTx(ctx context.Context, tx pgx.Tx, schema, org, reservation string) (map[string]string, error) {
	result := map[string]string{}
	for _, table := range []string{"cost_evidence", "ledger_entries", "cost_quarantines", "cost_quarantine_resolutions", "financial_incidents"} {
		key := "jsonb_build_array(organization_id,id)"
		if table == "cost_evidence" {
			key = "jsonb_build_array(organization_id,source,reference)"
		}
		rows, err := tx.Query(ctx, "SELECT "+key+",encode(sha256(convert_to(to_jsonb(r)::text,'UTF8')),'hex') FROM "+pgx.Identifier{schema, table}.Sanitize()+" r WHERE organization_id=$1 AND reservation_id=$2 LIMIT 10001", org, reservation)
		if err != nil {
			return nil, ErrRecoveryAccounting
		}
		for rows.Next() {
			var key json.RawMessage
			var hash string
			if rows.Scan(&key, &hash) != nil || len(key) > 2048 || len(result) >= recoveryPlanMaxItems {
				rows.Close()
				return nil, ErrRecoveryAccounting
			}
			result[table+"\x00"+string(key)] = hash
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, ErrRecoveryAccounting
		}
	}
	return result, nil
}
