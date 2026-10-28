// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

var ErrRecoveryCostHistory = errors.New("recovery cost history validation failed")

// RecoveryCostHistoryReport distinguishes checked request bindings and surviving
// transition chains from opaque migration facts. It does not attest invoices,
// pre-003 history, historical admission capacity, or post-backup external facts.
// Every organization with a legacy checkpoint or unavailable evidence hash stays
// explicitly affected; callers must preserve that uncertainty in recovery state.
type RecoveryCostHistoryReport struct {
	ModernRequestsChecked int64    `json:"modern_requests_checked"`
	HistoriesChecked      int64    `json:"histories_checked"`
	LegacyImportFacts     int64    `json:"legacy_import_facts"`
	LegacyOpaqueEvidence  int64    `json:"legacy_opaque_evidence"`
	OpaqueOrganizations   []string `json:"opaque_organizations"`
}

const (
	recoveryCostMaxRows       = 200000
	recoveryCostMaxBytes      = 64 << 20
	recoveryCostMaxPerHistory = 4096
	recoveryCostMaxHashWork   = 1000000
	recoveryCostMaxOpaqueOrgs = 4096
)

var recoveryCostLegacyEvidenceID = regexp.MustCompile(`^evi_legacy_[1-9][0-9]*$`)

type recoveryCostKey struct{ org, id string }
type recoveryCostLedger struct {
	id, key, hash, kind, task, root string
	reserved, spent                 int64
	synthetic                       bool
}
type recoveryCostEvidence struct {
	id, hash string
	value    domain.CostEvidence
	q        *recoveryCostQuarantine
	ledger   *recoveryCostLedger
	kind     string
	opaque   bool
}
type recoveryCostQuarantine struct {
	id, evidence, status, kind, reason string
	baseRevision, revision             int
	actual                             *int64
	expected                           *int
	resolution                         *int
	ledger, resolutionLedger           *recoveryCostLedger
}
type recoveryCostHistory struct {
	org, id, operation, task, root, category, provider, status, pending string
	amount                                                              int64
	actual                                                              *int64
	revision                                                            int
	ledger                                                              map[string]*recoveryCostLedger
	evidence                                                            map[string]*recoveryCostEvidence
	quarantines                                                         map[string]*recoveryCostQuarantine
	rows                                                                int
}
type recoveryCostState struct {
	status, pending string
	actual          *int64
	revision        int
}
type recoveryCostWork struct{ hashes int }

func (w *recoveryCostWork) hash(e domain.CostEvidence, expected *int) (string, bool) {
	w.hashes++
	if w.hashes > recoveryCostMaxHashWork {
		return "", false
	}
	// Do not marshal the current public DTO: its money is a decimal string.
	// This function deliberately retains the actual numeric 0.2 request shape,
	// original field order/omitempty and capitalized Evidence/Expected envelope.
	return evidenceRequestHash(e, expected), true
}

// checkRecoveryCostHistory is SELECT-only. The caller first pins the exact
// schema and validates typed values, declarative/FK constraints and aggregate
// projections, then supplies exclusively owned private staging and a safe
// pg_catalog-first search_path. It neither repairs rows nor invokes accounting
// APIs. Limits bound acquisition, per-history work and hash reconstruction;
// they are a recovery profile, not new runtime/history admission restrictions.
func checkRecoveryCostHistory(ctx context.Context, tx pgx.Tx, schema string) (RecoveryCostHistoryReport, error) {
	empty := RecoveryCostHistoryReport{}
	if ctx == nil || tx == nil || !recoverySnapshotName.MatchString(schema) {
		return empty, ErrRecoveryCostHistory
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	histories, err := recoveryCostRead(ctx, tx, schema)
	if err != nil {
		return empty, recoverySnapshotError(ctx, ErrRecoveryCostHistory)
	}
	report := RecoveryCostHistoryReport{OpaqueOrganizations: []string{}}
	opaqueOrgs := map[string]bool{}
	work := &recoveryCostWork{}
	for _, h := range histories {
		if ctx.Err() != nil {
			return empty, ctx.Err()
		}
		legacy, opaque, checked, ok := recoveryCostCheck(ctx, h, work)
		if !ok {
			return empty, recoverySnapshotError(ctx, ErrRecoveryCostHistory)
		}
		report.LegacyImportFacts += legacy
		report.LegacyOpaqueEvidence += opaque
		report.ModernRequestsChecked += checked
		if opaque == 0 {
			report.HistoriesChecked++
		}
		if legacy != 0 || opaque != 0 {
			opaqueOrgs[h.org] = true
			if len(opaqueOrgs) > recoveryCostMaxOpaqueOrgs {
				return empty, ErrRecoveryCostHistory
			}
		}
	}
	for org := range opaqueOrgs {
		report.OpaqueOrganizations = append(report.OpaqueOrganizations, org)
	}
	sort.Strings(report.OpaqueOrganizations)
	return report, nil
}

// No nil/error wrapper intentionally discards the supported-opaque report.
// A caller must propagate this report, even though the current stage rolls back.

func recoveryCostRead(ctx context.Context, tx pgx.Tx, schema string) ([]*recoveryCostHistory, error) {
	byKey := map[recoveryCostKey]*recoveryCostHistory{}
	histories := []*recoveryCostHistory{}
	rowCount, byteCount := 0, 0
	// All identifiers/limits below are compile-time constants. Oversized text is
	// replaced with NULL before transfer; scanning a nonnullable string fails.
	bounded := func(name string, max string) string {
		return "CASE WHEN pg_catalog.octet_length(" + name + ")<=" + max + " THEN " + name + " ELSE NULL END"
	}
	query := func(table string, columns ...string) (pgx.Rows, error) {
		return tx.Query(ctx, "SELECT "+strings.Join(columns, ",")+" FROM "+pgx.Identifier{schema, table}.Sanitize()+" LIMIT 200001")
	}
	account := func(h *recoveryCostHistory, fields ...string) bool {
		rowCount++
		byteCount += 256 // conservative fixed Go/number/pointer overhead per row
		for _, v := range fields {
			byteCount += len(v)
		}
		if h != nil {
			h.rows++
			if h.rows > recoveryCostMaxPerHistory {
				return false
			}
		}
		return rowCount <= recoveryCostMaxRows && byteCount <= recoveryCostMaxBytes
	}
	rows, err := query("reservations", bounded("organization_id", "128"), bounded("id", "128"), bounded("operation_id", "128"), bounded("task_id", "128"), bounded("root_id", "128"), bounded("category", "128"), bounded("provider", "128"), bounded("status", "32"), "amount_microusd", "actual_microusd", "revision", bounded("COALESCE(pending_quarantine_id,'')", "128"))
	if err != nil {
		return nil, ErrRecoveryCostHistory
	}
	for rows.Next() {
		h := &recoveryCostHistory{ledger: map[string]*recoveryCostLedger{}, evidence: map[string]*recoveryCostEvidence{}, quarantines: map[string]*recoveryCostQuarantine{}}
		if rows.Scan(&h.org, &h.id, &h.operation, &h.task, &h.root, &h.category, &h.provider, &h.status, &h.amount, &h.actual, &h.revision, &h.pending) != nil || !account(h, h.org, h.id, h.operation, h.task, h.root, h.category, h.provider, h.status, h.pending) || byKey[recoveryCostKey{h.org, h.id}] != nil {
			rows.Close()
			return nil, ErrRecoveryCostHistory
		}
		byKey[recoveryCostKey{h.org, h.id}] = h
		histories = append(histories, h)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, ErrRecoveryCostHistory
	}
	rows, err = query("ledger_entries", bounded("organization_id", "128"), bounded("reservation_id", "128"), bounded("id", "128"), bounded("event_key", "512"), bounded("event_hash", "128"), bounded("kind", "64"), bounded("task_id", "128"), bounded("root_id", "128"), "delta_reserved_microusd", "delta_spent_microusd", "synthetic")
	if err != nil {
		return nil, ErrRecoveryCostHistory
	}
	for rows.Next() {
		var org, id string
		l := &recoveryCostLedger{}
		if rows.Scan(&org, &id, &l.id, &l.key, &l.hash, &l.kind, &l.task, &l.root, &l.reserved, &l.spent, &l.synthetic) != nil {
			rows.Close()
			return nil, ErrRecoveryCostHistory
		}
		h := byKey[recoveryCostKey{org, id}]
		if h == nil || !account(h, org, id, l.id, l.key, l.hash, l.kind, l.task, l.root) || h.ledger[l.key] != nil {
			rows.Close()
			return nil, ErrRecoveryCostHistory
		}
		h.ledger[l.key] = l
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, ErrRecoveryCostHistory
	}
	rows, err = query("cost_evidence", bounded("organization_id", "128"), bounded("reservation_id", "128"), bounded("id", "128"), bounded("request_hash", "128"), bounded("source", "128"), bounded("reference", "256"), bounded("payload_digest", "64"), "actual_microusd", bounded("reason", "256"), "final", bounded("currency", "16"))
	if err != nil {
		return nil, ErrRecoveryCostHistory
	}
	for rows.Next() {
		var org, id string
		e := &recoveryCostEvidence{}
		if rows.Scan(&org, &id, &e.id, &e.hash, &e.value.Source, &e.value.Reference, &e.value.PayloadDigest, &e.value.ActualMicroUSD, &e.value.Reason, &e.value.Final, &e.value.Currency) != nil {
			rows.Close()
			return nil, ErrRecoveryCostHistory
		}
		h := byKey[recoveryCostKey{org, id}]
		if h == nil || !account(h, org, id, e.id, e.hash, e.value.Source, e.value.Reference, e.value.PayloadDigest, e.value.Reason, e.value.Currency) || h.evidence[e.id] != nil {
			rows.Close()
			return nil, ErrRecoveryCostHistory
		}
		h.evidence[e.id] = e
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, ErrRecoveryCostHistory
	}
	rows, err = query("cost_quarantines", bounded("organization_id", "128"), bounded("reservation_id", "128"), bounded("id", "128"), bounded("evidence_id", "128"), "base_revision", "revision", bounded("base_status", "32"), "base_actual_microusd", bounded("requested_kind", "32"), "expected_revision", bounded("reason", "64"))
	if err != nil {
		return nil, ErrRecoveryCostHistory
	}
	for rows.Next() {
		var org, id string
		q := &recoveryCostQuarantine{}
		if rows.Scan(&org, &id, &q.id, &q.evidence, &q.baseRevision, &q.revision, &q.status, &q.actual, &q.kind, &q.expected, &q.reason) != nil {
			rows.Close()
			return nil, ErrRecoveryCostHistory
		}
		h := byKey[recoveryCostKey{org, id}]
		if h == nil || !account(h, org, id, q.id, q.evidence, q.status, q.kind, q.reason) || h.quarantines[q.id] != nil || h.evidence[q.evidence] == nil || h.evidence[q.evidence].q != nil {
			rows.Close()
			return nil, ErrRecoveryCostHistory
		}
		h.quarantines[q.id] = q
		h.evidence[q.evidence].q = q
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, ErrRecoveryCostHistory
	}
	rows, err = query("cost_quarantine_resolutions", bounded("organization_id", "128"), bounded("reservation_id", "128"), bounded("quarantine_id", "128"), bounded("evidence_id", "128"), "resulting_revision")
	if err != nil {
		return nil, ErrRecoveryCostHistory
	}
	for rows.Next() {
		var org, id, qid, eid string
		var revision int
		if rows.Scan(&org, &id, &qid, &eid, &revision) != nil {
			rows.Close()
			return nil, ErrRecoveryCostHistory
		}
		h := byKey[recoveryCostKey{org, id}]
		if h == nil || !account(h, org, id, qid, eid) || h.quarantines[qid] == nil || h.quarantines[qid].evidence != eid || h.quarantines[qid].resolution != nil {
			rows.Close()
			return nil, ErrRecoveryCostHistory
		}
		h.quarantines[qid].resolution = &revision
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, ErrRecoveryCostHistory
	}
	return histories, nil
}

func recoveryCostCheck(ctx context.Context, h *recoveryCostHistory, work *recoveryCostWork) (legacy, opaque, checked int64, ok bool) {
	if h.revision < 1 || h.amount < 0 || (h.actual != nil && *h.actual < 0) || (h.status == "settled") != (h.actual != nil) {
		return 0, 0, 0, false
	}
	used := map[string]bool{}
	unknowns, reservedOnlyUnknowns, releases := 0, 0, 0
	var origin *recoveryCostLedger
	for key, l := range h.ledger {
		if l.task != h.task || l.root != h.root || l.synthetic != (h.category == "fixture_legacy") {
			return 0, 0, 0, false
		}
		switch l.kind {
		case "reserve", "legacy_import":
			if origin != nil {
				return 0, 0, 0, false
			}
			origin = l
			used[key] = true
		case "unknown":
			if l.reserved != 0 || l.spent != 0 || !validDigest(l.hash) {
				return 0, 0, 0, false
			}
			if strings.HasPrefix(key, "unknown:") && len(key) > len("unknown:") {
				// Generic MarkCostUnknown may record multiple independent facts.
			} else if strings.HasPrefix(key, "kernel:") && len(key) > len("kernel:") && h.category == "model" {
				reservedOnlyUnknowns++
				if l.hash != digestValue(strings.TrimPrefix(key, "kernel:")) {
					return 0, 0, 0, false
				}
			} else if key == "tool.effect.unknown" && h.category == "tool" {
				reservedOnlyUnknowns++
			} else {
				return 0, 0, 0, false
			}
			unknowns++
			used[key] = true
		case "release":
			if h.category == "fixture_legacy" || l.reserved != -h.amount || l.spent != 0 {
				return 0, 0, 0, false
			}
			if strings.HasPrefix(key, "release:") && len(key) > len("release:") {
				if l.hash != digestValue("not_dispatched") {
					return 0, 0, 0, false
				}
			} else if h.category != "tool" || (key != "tool.runtime.unsent" && key != "tool.denied.unsent" && key != "tool.cancel.unsent") || !validDigest(l.hash) {
				return 0, 0, 0, false
			}
			releases++
			used[key] = true
		case "dispatch":
			if key != "dispatch" || h.category == "fixture_legacy" || l.reserved != 0 || l.spent != 0 || !validDigest(l.hash) {
				return 0, 0, 0, false
			}
			used[key] = true
		case "settle", "adjustment", "quarantine", "quarantine_resolution":
		default:
			return 0, 0, 0, false
		}
	}
	if origin == nil || releases > 1 || reservedOnlyUnknowns > 1 || (releases > 0 && h.ledger["dispatch"] != nil) {
		return 0, 0, 0, false
	}
	if origin.kind == "legacy_import" {
		if h.category != "fixture_legacy" || origin.id != "legacy_"+h.id || origin.key != "legacy.import" || origin.hash != "migration003" || origin.spent < 0 || (origin.reserved != 0 && origin.reserved != h.amount) {
			return 0, 0, 0, false
		}
		legacy = 1
	} else if origin.spent != 0 || origin.reserved != h.amount || (h.category == "fixture_legacy" && (origin.key != "fixture.reserve" || origin.hash != digestValue(h.operation) || h.amount != domain.FixtureCost)) || (h.category != "fixture_legacy" && origin.key != "reserve") {
		return 0, 0, 0, false
	}
	if h.category == "fixture_legacy" {
		if len(h.evidence) != 0 || len(h.quarantines) != 0 || h.pending != "" {
			return 0, 0, 0, false
		}
		states := []recoveryCostState{{status: "reserved", revision: 1}}
		if legacy != 0 {
			states = nil
			if origin.spent == 0 && origin.reserved == h.amount {
				states = append(states, recoveryCostState{status: "reserved", revision: 1}, recoveryCostState{status: "unknown", revision: 1})
			}
			if origin.reserved == 0 {
				actual := origin.spent
				states = append(states, recoveryCostState{status: "settled", actual: &actual, revision: 1})
				if actual == 0 {
					states = append(states, recoveryCostState{status: "released", revision: 1})
				}
			}
		}
		for _, state := range states {
			if recoveryCostFixture(h, state, unknowns, used) {
				return legacy, 0, 0, true
			}
		}
		return 0, 0, 0, false
	}
	for _, e := range h.evidence {
		if !validEvidence(e.value) || e.value.Source != h.provider {
			return 0, 0, 0, false
		}
		settle := h.ledger["settle:"+e.value.Source+":"+e.value.Reference]
		adjust := h.ledger["adjustment:"+e.value.Source+":"+e.value.Reference]
		if e.hash == "legacy-unavailable" {
			// Exact SQL004 COALESCE marker: neither matching old event key existed.
			// This is only a format classification; it cannot prove provenance.
			if !recoveryCostLegacyEvidenceID.MatchString(e.id) || e.q != nil || settle != nil || adjust != nil {
				return 0, 0, 0, false
			}
			e.opaque = true
			opaque++
			continue
		}
		if !validDigest(e.hash) || (settle != nil && adjust != nil) {
			return 0, 0, 0, false
		}
		if e.q != nil {
			q := e.q
			if settle != nil || adjust != nil || q.baseRevision < 1 || q.revision != q.baseRevision+1 || q.reason != "projection_overflow" || (q.status == "settled") != (q.actual != nil) || (q.actual != nil && *q.actual < 0) || (q.kind == "adjustment") != (q.status == "settled") {
				return 0, 0, 0, false
			}
			if (q.kind == "settle" && q.expected != nil) || (q.kind == "adjustment" && (q.expected == nil || *q.expected != q.baseRevision)) || (q.kind != "settle" && q.kind != "adjustment") {
				return 0, 0, 0, false
			}
			q.ledger = h.ledger["quarantine:"+q.id]
			if q.ledger == nil || q.ledger.kind != "quarantine" || q.ledger.hash != e.hash || q.ledger.reserved != 0 || q.ledger.spent != 0 || used[q.ledger.key] {
				return 0, 0, 0, false
			}
			used[q.ledger.key] = true
			q.resolutionLedger = h.ledger["resolve:"+q.id]
			if (q.resolution == nil) != (q.resolutionLedger == nil) {
				return 0, 0, 0, false
			}
			if q.resolution != nil {
				l := q.resolutionLedger
				if *q.resolution != q.revision+1 || l.kind != "quarantine_resolution" || l.hash != e.hash || used[l.key] || !recoveryCostDeltas(h, recoveryCostState{status: q.status, actual: q.actual}, e.value.ActualMicroUSD, l) {
					return 0, 0, 0, false
				}
				used[l.key] = true
			}
			e.kind = q.kind
		} else {
			e.ledger, e.kind = settle, "settle"
			if adjust != nil {
				e.ledger, e.kind = adjust, "adjustment"
			}
			if e.ledger == nil || e.ledger.kind != e.kind || e.ledger.hash != e.hash || used[e.ledger.key] {
				return 0, 0, 0, false
			}
			used[e.ledger.key] = true
		}
		if e.kind == "adjustment" && strings.TrimSpace(e.value.Reason) == "" {
			return 0, 0, 0, false
		}
		checked++
	}
	if opaque != 0 {
		// Missing old bindings do not justify inventing a revision chain. Every
		// surviving modern request must still bind to its ledger/q and a feasible
		// numeric revision; ordinary projection/FK checks remain prerequisites.
		for _, e := range h.evidence {
			if e.opaque {
				continue
			}
			if !recoveryCostOpaqueBinding(ctx, h, e, work) {
				return 0, 0, 0, false
			}
		}
		return 0, opaque, checked, true
	}
	if len(used) != len(h.ledger) {
		return 0, 0, 0, false
	}
	state := recoveryCostState{status: "reserved", revision: 1 + unknowns + releases}
	if unknowns != 0 {
		state.status = "unknown"
	}
	if releases != 0 {
		state.status = "released"
	}
	remaining := make(map[string]*recoveryCostEvidence, len(h.evidence))
	for id, e := range h.evidence {
		remaining[id] = e
	}
	for len(remaining) != 0 {
		if ctx.Err() != nil || state.pending != "" {
			return 0, 0, 0, false
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
				return 0, 0, 0, false
			}
			if hash == e.hash {
				if next != nil { // two transitions cannot consume one revision
					return 0, 0, 0, false
				}
				next = e
			}
		}
		if next == nil || !recoveryCostAdvance(h, &state, next) {
			return 0, 0, 0, false
		}
		delete(remaining, next.id)
	}
	return 0, 0, checked, recoveryCostFinal(h, state)
}

func recoveryCostDeltas(h *recoveryCostHistory, state recoveryCostState, actual int64, l *recoveryCostLedger) bool {
	prior, reserved := int64(0), int64(0)
	if state.actual != nil {
		prior = *state.actual
	}
	if state.status == "reserved" || state.status == "unknown" {
		reserved = -h.amount
	}
	// Both factual amounts are nonnegative int64; their difference cannot overflow.
	return l.reserved == reserved && l.spent == actual-prior
}

func recoveryCostAdvance(h *recoveryCostHistory, s *recoveryCostState, e *recoveryCostEvidence) bool {
	if e.q != nil {
		q := e.q
		if q.baseRevision != s.revision || q.status != s.status || !sameAmount(q.actual, s.actual) {
			return false
		}
		s.revision++
		if s.status == "reserved" {
			s.status = "unknown"
		}
		s.pending = q.id
		if q.resolution == nil {
			return true
		}
		if *q.resolution != s.revision+1 || !recoveryCostDeltas(h, *s, e.value.ActualMicroUSD, q.resolutionLedger) {
			return false
		}
	} else if !recoveryCostDeltas(h, *s, e.value.ActualMicroUSD, e.ledger) {
		return false
	}
	actual := e.value.ActualMicroUSD
	s.status, s.actual, s.pending, s.revision = "settled", &actual, "", s.revision+1
	return true
}

func recoveryCostFinal(h *recoveryCostHistory, s recoveryCostState) bool {
	return h.status == s.status && h.pending == s.pending && h.revision == s.revision && sameAmount(h.actual, s.actual)
}

func recoveryCostFixture(h *recoveryCostHistory, s recoveryCostState, unknowns int, used map[string]bool) bool {
	if unknowns != 0 {
		if s.status != "reserved" && s.status != "unknown" {
			return false
		}
		s.status, s.revision = "unknown", s.revision+unknowns
	}
	settles := 0
	for key, l := range h.ledger {
		if used[key] {
			continue
		}
		if l.kind != "settle" || (key != "fixture.settle" && key != "fixture.reconcile") || l.hash != digestValue(h.operation) || s.status != "reserved" || !recoveryCostDeltas(h, s, h.amount, l) {
			return false
		}
		actual := h.amount
		s.status, s.actual, s.revision = "settled", &actual, s.revision+1
		settles++
	}
	return settles <= 1 && recoveryCostFinal(h, s)
}

func recoveryCostOpaqueBinding(ctx context.Context, h *recoveryCostHistory, e *recoveryCostEvidence, w *recoveryCostWork) bool {
	var expected *int
	if e.q != nil {
		expected = e.q.expected
		if e.q.revision > h.revision || (e.q.resolution != nil && *e.q.resolution > h.revision) {
			return false
		}
	} else if e.kind == "settle" {
		if e.ledger.spent != e.value.ActualMicroUSD || (e.ledger.reserved != 0 && e.ledger.reserved != -h.amount) {
			return false
		}
	} else {
		// Check that the signed delta implies a representable nonnegative prior.
		delta, actual := e.ledger.spent, e.value.ActualMicroUSD
		if e.ledger.reserved != 0 || (delta >= 0 && delta > actual) || (delta < 0 && actual > math.MaxInt64+delta) {
			return false
		}
		for revision := 1; revision < h.revision; revision++ {
			if ctx.Err() != nil {
				return false
			}
			hash, within := w.hash(e.value, &revision)
			if !within {
				return false
			}
			if hash == e.hash {
				return true
			}
		}
		return false
	}
	hash, within := w.hash(e.value, expected)
	return within && hash == e.hash
}
