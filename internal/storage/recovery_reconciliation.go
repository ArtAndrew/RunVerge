// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
)

var ErrRecoveryReconciliation = errors.New("recovery reconciliation plan unavailable; target remains held")

// A plan is a reproducible review input, not a clearance or an invoice. A future
// apply must recompute it under the same gate instead of trusting editable JSON.
type RecoveryReconciliationPlan struct {
	Version                int                             `json:"version"`
	Algorithm              string                          `json:"algorithm"`
	PlanSHA256             string                          `json:"plan_sha256"`
	SourceID               string                          `json:"source_id"`
	TargetID               string                          `json:"target_id"`
	RestoreID              string                          `json:"restore_id"`
	BatchID                string                          `json:"batch_id"`
	SelectedReceiptSHA256  string                          `json:"selected_receipt_sha256"`
	EvidenceReceiptSHA256  string                          `json:"evidence_receipt_sha256"`
	SourceSchemaSHA256     string                          `json:"source_schema_sha256"`
	EvidenceSchemaSHA256   string                          `json:"evidence_schema_sha256"`
	SelectedSnapshotSHA256 string                          `json:"selected_snapshot_sha256"`
	EvidenceSnapshotSHA256 string                          `json:"evidence_snapshot_sha256"`
	CaptureLinkSHA256      string                          `json:"capture_link_sha256"`
	TargetCatalogSHA256    string                          `json:"target_catalog_sha256"`
	SelectedCutUTC         string                          `json:"selected_cut_utc"`
	EvidenceCutUTC         string                          `json:"evidence_cut_utc"`
	Organizations          []RecoveryFinancialOrganization `json:"organizations"`
	Reservations           []RecoveryFinancialReservation  `json:"reservations"`
	NewFacts               []RecoveryFinancialFact         `json:"new_facts"`
	Blockers               []RecoveryFinancialBlocker      `json:"blockers"`
	ReconciliationState    string                          `json:"reconciliation_state"`
	ExecutionHeld          bool                            `json:"execution_held"`
	RollbackConfirmed      bool                            `json:"rollback_confirmed"`
}
type RecoveryFinancialOrganization struct {
	OrganizationID string                    `json:"organization_id"`
	Before         *RecoveryFinancialAccount `json:"before"`
	After          *RecoveryFinancialAccount `json:"after"`
}
type RecoveryFinancialAccount struct {
	Budget                 int64 `json:"budget_microusd,string"`
	Reserved               int64 `json:"reserved_microusd,string"`
	Booked                 int64 `json:"booked_microusd,string"`
	FinancialHold          bool  `json:"financial_hold"`
	UnknownCount           int64 `json:"unknown_count,string"`
	PendingQuarantineCount int64 `json:"pending_quarantine_count,string"`
}
type RecoveryFinancialReservation struct {
	OrganizationID string                     `json:"organization_id"`
	ReservationID  string                     `json:"reservation_id"`
	Before         *RecoveryFinancialPosition `json:"before"`
	After          *RecoveryFinancialPosition `json:"after"`
}
type RecoveryFinancialPosition struct {
	Category            string `json:"category"`
	Provider            string `json:"provider"`
	Status              string `json:"status"`
	Amount              int64  `json:"amount_microusd,string"`
	Actual              *int64 `json:"actual_microusd,string"`
	Revision            int    `json:"revision"`
	PendingQuarantineID string `json:"pending_quarantine_id"`
	TaskID              string `json:"task_id"`
	RootID              string `json:"root_id"`
	OperationID         string `json:"operation_id"`
	ImmutableSHA256     string `json:"immutable_sha256"`
}
type RecoveryFinancialFact struct {
	OrganizationID string `json:"organization_id"`
	Table          string `json:"table"`
	KeySHA256      string `json:"key_sha256"`
	RowSHA256      string `json:"row_sha256"`
	RecordID       string `json:"record_id"`
	ReservationID  string `json:"reservation_id"`
	EvidenceID     string `json:"evidence_id"`
	Kind           string `json:"kind"`
	Actual         *int64 `json:"actual_microusd,string"`
	Synthetic      *bool  `json:"synthetic"`
	// Only ledger entries have deltas. Null is distinct from a known zero.
	ReservedDelta *int64 `json:"reserved_delta_microusd,string"`
	BookedDelta   *int64 `json:"booked_delta_microusd,string"`
}
type RecoveryFinancialBlocker struct {
	Code           string `json:"code"`
	OrganizationID string `json:"organization_id,omitempty"`
	ReservationID  string `json:"reservation_id,omitempty"`
	Table          string `json:"table,omitempty"`
	KeySHA256      string `json:"key_sha256,omitempty"`
}

const recoveryPlanMaxItems = 10000
const recoveryPlanMaxBytes = 8 << 20

func (s *Store) PlanRecoveryReconciliation(ctx context.Context, selected, late *recoveryarchive.VerifiedArchive, verified *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits) (result RecoveryReconciliationPlan, resultErr error) {
	if !limits.valid() {
		return result, ErrRecoveryReconciliation
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	original, later, err := bindRecoveryEvidence(ctx, selected, late, verified, spec, limits)
	if err != nil {
		return result, err
	}
	connection, err := s.Pool.Acquire(ctx)
	if err != nil {
		return result, ErrRecoveryReconciliation
	}
	defer connection.Release()
	tx, err := connection.Begin(ctx)
	if err != nil {
		return result, ErrRecoveryReconciliation
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if tx.Rollback(cleanup) != nil {
			_ = connection.Conn().Close(cleanup)
			result, resultErr = RecoveryReconciliationPlan{}, ErrRecoveryStageCleanup
		} else if resultErr == nil {
			result.RollbackConfirmed = true
			// The digest excludes itself and includes confirmed rollback.
			raw, e := json.Marshal(result)
			if e != nil || len(raw)+64 > recoveryPlanMaxBytes {
				result, resultErr = RecoveryReconciliationPlan{}, ErrRecoveryReconciliation
				return
			}
			result.PlanSHA256 = Hash(raw)
		}
	}()
	held, catalog, err := s.evidenceTargetTx(ctx, tx, spec, original.SHA256)
	if err != nil {
		return result, err
	}
	if _, err = s.inspectRecoveryEvidenceTx(ctx, tx, spec, verified); err != nil {
		return result, err
	}
	inventory, err := recoverySnapshotInventory(ctx, tx, s.runtimeSchema, true)
	if err != nil {
		return result, err
	}
	baseInventory, err := recoverySourceInventory(inventory, original.InventorySHA256)
	if err != nil {
		return result, err
	}
	if err = recoveryStageCheckDigests(ctx, tx, s.runtimeSchema, baseInventory, original, limits); err != nil {
		return result, err
	}
	laterInventory, err := recoverySourceInventory(inventory, later.InventorySHA256)
	if err != nil {
		return result, err
	}
	staged, err := s.stageRecoveryTypedDataTx(ctx, tx, late, later, limits, held, catalog, laterInventory)
	if err != nil {
		return result, err
	}
	// Recompute comparisons from the verified bytes; a stored report/count is
	// never a substitute for checking financial rows across both snapshots.
	if err = s.projectEvidenceChanges(ctx, tx, staged); err != nil {
		return result, err
	}
	l := verified.Link()
	result = RecoveryReconciliationPlan{Version: 1, Algorithm: "held-financial-review-v1", SourceID: held.SourceID, TargetID: held.InstallationID, RestoreID: held.RestoreID, BatchID: l.OperationID, SelectedReceiptSHA256: l.SelectedReceiptSHA256, EvidenceReceiptSHA256: l.EvidenceReceiptSHA256, SourceSchemaSHA256: original.Metadata.SchemaSHA256, EvidenceSchemaSHA256: later.Metadata.SchemaSHA256, SelectedSnapshotSHA256: original.SHA256, EvidenceSnapshotSHA256: later.SHA256, CaptureLinkSHA256: verified.SHA256(), TargetCatalogSHA256: recoverySemanticCatalogSHA256, SelectedCutUTC: original.Metadata.CutUTC, EvidenceCutUTC: later.Metadata.CutUTC, Organizations: []RecoveryFinancialOrganization{}, Reservations: []RecoveryFinancialReservation{}, NewFacts: []RecoveryFinancialFact{}, Blockers: []RecoveryFinancialBlocker{{Code: "external_window_coverage_unproven"}}, ReconciliationState: "pending", ExecutionHeld: true}
	if err = s.recoveryFinancialPlan(ctx, tx, staged.schema, &result); err != nil {
		return RecoveryReconciliationPlan{}, err
	}
	return result, nil
}

func (s *Store) recoveryFinancialPlan(ctx context.Context, tx pgx.Tx, lateSchema string, p *RecoveryReconciliationPlan) error {
	orgs := map[string]*RecoveryFinancialOrganization{}
	positions := map[recoveryCostKey]*RecoveryFinancialReservation{}
	histories := [2]map[recoveryCostKey]*recoveryCostHistory{{}, {}}
	block := func(code, org, reservation string) {
		p.Blockers = append(p.Blockers, RecoveryFinancialBlocker{Code: code, OrganizationID: org, ReservationID: reservation})
	}
	for index, schema := range []string{s.runtimeSchema, lateSchema} {
		// Independently recompute numeric projections in both snapshots. SQL
		// numeric sums cannot overflow before comparison to the int64 accounts.
		for _, check := range recoveryInvariantQueries(schema) {
			if check.name != "reservation_projection" && check.name != "organization_projection" && check.name != "task_projection" {
				continue
			}
			var valid bool
			if tx.QueryRow(ctx, check.sql).Scan(&valid) != nil || !valid {
				return ErrRecoveryReconciliation
			}
		}
		q := func(table string) string { return pgx.Identifier{schema, table}.Sanitize() }
		rows, err := tx.Query(ctx, "SELECT id,budget_microusd,reserved_microusd,spent_microusd,financial_hold FROM "+q("organizations")+" ORDER BY id COLLATE \"C\" LIMIT 10001")
		if err != nil {
			return ErrRecoveryReconciliation
		}
		for rows.Next() {
			var id string
			var a RecoveryFinancialAccount
			if rows.Scan(&id, &a.Budget, &a.Reserved, &a.Booked, &a.FinancialHold) != nil || len(id) > 128 {
				rows.Close()
				return ErrRecoveryReconciliation
			}
			if orgs[id] == nil {
				orgs[id] = &RecoveryFinancialOrganization{OrganizationID: id}
			}
			if len(orgs) > recoveryPlanMaxItems {
				rows.Close()
				return ErrRecoveryReconciliation
			}
			if index == 0 {
				orgs[id].Before = &a
			} else {
				orgs[id].After = &a
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return ErrRecoveryReconciliation
		}
		hs, err := recoveryCostRead(ctx, tx, schema)
		if err != nil {
			return err
		}
		work := &recoveryCostWork{}
		for _, h := range hs {
			legacy, opaque, _, ok := recoveryCostCheck(ctx, h, work)
			if !ok {
				return ErrRecoveryReconciliation
			}
			if legacy != 0 || opaque != 0 {
				block("opaque_financial_history", h.org, h.id)
			}
			histories[index][recoveryCostKey{h.org, h.id}] = h
		}
		rows, err = tx.Query(ctx, "SELECT organization_id,id,encode(sha256(convert_to((to_jsonb(r)-ARRAY['status','actual_microusd','revision','pending_quarantine_id'])::text,'UTF8')),'hex') FROM "+q("reservations")+" r ORDER BY organization_id COLLATE \"C\",id COLLATE \"C\" LIMIT 10001")
		if err != nil {
			return ErrRecoveryReconciliation
		}
		for rows.Next() {
			var org, id, digest string
			if rows.Scan(&org, &id, &digest) != nil {
				rows.Close()
				return ErrRecoveryReconciliation
			}
			key := recoveryCostKey{org, id}
			h := histories[index][key]
			if h == nil || orgs[org] == nil {
				rows.Close()
				return ErrRecoveryReconciliation
			}
			v := &RecoveryFinancialPosition{Category: h.category, Provider: h.provider, Status: h.status, Amount: h.amount, Actual: h.actual, Revision: h.revision, PendingQuarantineID: h.pending, TaskID: h.task, RootID: h.root, OperationID: h.operation, ImmutableSHA256: digest}
			if positions[key] == nil {
				positions[key] = &RecoveryFinancialReservation{OrganizationID: org, ReservationID: id}
			}
			if len(positions) > recoveryPlanMaxItems {
				rows.Close()
				return ErrRecoveryReconciliation
			}
			a := orgs[org].Before
			if index == 0 {
				positions[key].Before = v
			} else {
				positions[key].After = v
				a = orgs[org].After
			}
			if h.status == "unknown" {
				a.UnknownCount++
			}
			if h.pending != "" {
				a.PendingQuarantineCount++
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return ErrRecoveryReconciliation
		}
	}
	for id, o := range orgs {
		if o.Before == nil {
			block("late_only_organization", id, "")
		}
		if o.After == nil {
			block("organization_missing_later", id, "")
		}
		if o.Before != nil && o.After != nil && o.Before.FinancialHold && !o.After.FinancialHold {
			block("financial_hold_disappeared", id, "")
		}
		p.Organizations = append(p.Organizations, *o)
	}
	continuationWork := &recoveryCostWork{}
	for key, r := range positions {
		if r.Before == nil {
			block("late_only_reservation", key.org, key.id)
		}
		if r.After == nil {
			block("reservation_missing_later", key.org, key.id)
		}
		if r.Before != nil && r.After != nil {
			if r.Before.ImmutableSHA256 != r.After.ImmutableSHA256 {
				block("reservation_identity_changed", key.org, key.id)
			}
			continuous, err := recoveryCostContinuationChecked(ctx, histories[0][key], histories[1][key], continuationWork)
			if err != nil {
				return err
			}
			if !continuous {
				block("financial_history_not_contiguous", key.org, key.id)
			}
		}
		p.Reservations = append(p.Reservations, *r)
	}
	if err := recoveryFinancialFacts(ctx, tx, lateSchema, p); err != nil {
		return err
	}
	if len(p.Blockers) > recoveryPlanMaxItems {
		return ErrRecoveryReconciliation
	}
	sort.Slice(p.Organizations, func(i, j int) bool { return p.Organizations[i].OrganizationID < p.Organizations[j].OrganizationID })
	sort.Slice(p.Reservations, func(i, j int) bool {
		a, b := p.Reservations[i], p.Reservations[j]
		if a.OrganizationID != b.OrganizationID {
			return a.OrganizationID < b.OrganizationID
		}
		return a.ReservationID < b.ReservationID
	})
	sort.Slice(p.Blockers, func(i, j int) bool {
		a, _ := json.Marshal(p.Blockers[i])
		b, _ := json.Marshal(p.Blockers[j])
		return string(a) < string(b)
	})
	return nil
}

func recoveryFinancialFacts(ctx context.Context, tx pgx.Tx, lateSchema string, p *RecoveryReconciliationPlan) error {
	count := 0
	// The exact PK expression for each immutable financial relation is fixed
	// here, independently of record contents. Full row diffs were recomputed.
	for _, table := range []string{"cost_evidence", "cost_quarantine_resolutions", "cost_quarantines", "financial_incidents", "ledger_entries"} {
		key := "jsonb_build_array(r.organization_id,r.id)"
		if table == "cost_evidence" {
			key = "jsonb_build_array(r.organization_id,r.source,r.reference)"
		}
		rows, err := tx.Query(ctx, `SELECT d.organization_id,d.key_sha256,d.classification,COALESCE(d.after_sha256,''),
 COALESCE(to_jsonb(r)->>'id',''),COALESCE(to_jsonb(r)->>'reservation_id',''),
 COALESCE(to_jsonb(r)->>'evidence_id',''),COALESCE(to_jsonb(r)->>'kind',to_jsonb(r)->>'requested_kind',''),
 (to_jsonb(r)->>'actual_microusd')::bigint,(to_jsonb(r)->>'synthetic')::boolean,
 (to_jsonb(r)->>'delta_reserved_microusd')::bigint,(to_jsonb(r)->>'delta_spent_microusd')::bigint
 FROM pg_temp.rvg_evidence_diff d LEFT JOIN `+pgx.Identifier{lateSchema, table}.Sanitize()+` r ON d.key_parts=`+key+`
 WHERE d.table_name=$1 ORDER BY d.key_sha256 LIMIT 10001`, table)
		if err != nil {
			return ErrRecoveryReconciliation
		}
		for rows.Next() {
			f := RecoveryFinancialFact{Table: table}
			var classification string
			if rows.Scan(&f.OrganizationID, &f.KeySHA256, &classification, &f.RowSHA256, &f.RecordID, &f.ReservationID, &f.EvidenceID, &f.Kind, &f.Actual, &f.Synthetic, &f.ReservedDelta, &f.BookedDelta) != nil {
				rows.Close()
				return ErrRecoveryReconciliation
			}
			count++
			if count > recoveryPlanMaxItems {
				rows.Close()
				return ErrRecoveryReconciliation
			}
			if classification == "added" {
				p.NewFacts = append(p.NewFacts, f)
			} else {
				p.Blockers = append(p.Blockers, RecoveryFinancialBlocker{Code: "immutable_financial_fact_" + classification, OrganizationID: f.OrganizationID, Table: table, KeySHA256: f.KeySHA256})
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return ErrRecoveryReconciliation
		}
	}
	return nil
}
