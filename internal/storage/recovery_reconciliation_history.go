// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"strings"
)

// Exhaustion is not evidence of a broken financial history. Refuse the entire
// plan so map iteration order cannot decide which reservations get blockers.
func recoveryCostContinuationChecked(ctx context.Context, before, after *recoveryCostHistory, work *recoveryCostWork) (bool, error) {
	if ctx.Err() != nil || work.hashes > recoveryCostMaxHashWork {
		return false, ErrRecoveryReconciliation
	}
	valid := recoveryCostContinues(ctx, before, after, work)
	if ctx.Err() != nil || work.hashes > recoveryCostMaxHashWork {
		return false, ErrRecoveryReconciliation
	}
	return valid, nil
}

// Both histories have already passed recoveryCostCheck. This additional check
// starts at the actual old checkpoint: validity of two independent snapshots
// alone must not reorder a new unknown marker ahead of an old settlement.
func recoveryCostContinues(ctx context.Context, before, after *recoveryCostHistory, work *recoveryCostWork) bool {
	if before == nil || after == nil || before.org != after.org || before.id != after.id || after.revision < before.revision {
		return false
	}
	for key, old := range before.ledger {
		current := after.ledger[key]
		if current == nil || *old != *current || old.kind == "legacy_import" {
			return false
		}
	}
	for id, old := range before.evidence {
		current := after.evidence[id]
		if current == nil || old.opaque || current.opaque || old.hash != current.hash {
			return false
		}
	}
	state := recoveryCostState{status: before.status, pending: before.pending, actual: before.actual, revision: before.revision}
	unknowns, releases, fixtureSettles := 0, 0, 0
	reservedOnlyUnknown := false
	for key, l := range after.ledger {
		if before.ledger[key] != nil {
			continue
		}
		switch l.kind {
		case "reserve", "legacy_import":
			return false
		case "dispatch":
			if state.status != "reserved" || state.pending != "" {
				return false
			}
		case "unknown":
			unknowns++
			if strings.HasPrefix(key, "kernel:") || key == "tool.effect.unknown" {
				reservedOnlyUnknown = true
			}
		case "release":
			releases++
		case "settle":
			if after.category == "fixture_legacy" {
				fixtureSettles++
			}
		}
	}
	if reservedOnlyUnknown && (state.status != "reserved" || state.pending != "") {
		return false
	}
	if unknowns > 0 {
		if state.pending != "" || (state.status != "reserved" && state.status != "unknown") {
			return false
		}
		state.status = "unknown"
		state.revision += unknowns
	}
	if releases > 0 {
		if releases != 1 || state.status != "reserved" || state.pending != "" {
			return false
		}
		state.status = "released"
		state.revision++
	}
	if fixtureSettles > 0 {
		if fixtureSettles != 1 || state.status != "reserved" || state.pending != "" {
			return false
		}
		actual := after.amount
		state.actual = &actual
		state.status = "settled"
		state.revision++
	}
	if state.pending != "" {
		oldQ, currentQ := before.quarantines[state.pending], after.quarantines[state.pending]
		if oldQ == nil || currentQ == nil {
			return false
		}
		if currentQ.resolution != nil {
			e := after.evidence[currentQ.evidence]
			if oldQ.resolution != nil || e == nil || *currentQ.resolution != state.revision+1 || currentQ.resolutionLedger == nil || !recoveryCostDeltas(after, state, e.value.ActualMicroUSD, currentQ.resolutionLedger) {
				return false
			}
			actual := e.value.ActualMicroUSD
			state.actual = &actual
			state.status = "settled"
			state.pending = ""
			state.revision++
		}
	}
	remaining := map[string]*recoveryCostEvidence{}
	for id, e := range after.evidence {
		if before.evidence[id] == nil {
			if e.opaque {
				return false
			}
			remaining[id] = e
		}
	}
	for len(remaining) > 0 {
		if ctx.Err() != nil || state.pending != "" {
			return false
		}
		var next *recoveryCostEvidence
		for _, e := range remaining {
			var expected *int
			if state.status == "settled" {
				if e.kind != "adjustment" {
					continue
				}
				expected = &state.revision
			} else if e.kind != "settle" {
				continue
			}
			if e.q != nil && e.q.baseRevision != state.revision {
				continue
			}
			hash, within := work.hash(e.value, expected)
			if !within {
				return false
			}
			if hash == e.hash {
				if next != nil {
					return false
				}
				next = e
			}
		}
		if next == nil || !recoveryCostAdvance(after, &state, next) {
			return false
		}
		delete(remaining, next.id)
	}
	return recoveryCostFinal(after, state)
}
