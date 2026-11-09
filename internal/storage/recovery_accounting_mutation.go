// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

var ErrRecoveryAccounting = errors.New("recovery accounting change refused; execution remains held")

// Private executor input, constructed from verified facts and locked target
// rows. Never decode this shape from a tenant request or an operator JSON plan.
// The caller owns the recovery gate, verifies the complete baseline/receipt
// chain, and validates financial projections before committing its transaction.
type recoveryAccountingBatch struct {
	id, evidenceBatchID, organizationID, reservationID string
	requestSHA256, planSHA256                          string
	expectedRevision                                   int64
}

type recoveryAccountingMutation struct {
	table         string
	before, after json.RawMessage
}

type recoveryAccountingReceipt struct {
	ID            string `json:"id"`
	Ordinal       int64  `json:"ordinal,string"`
	PreviousID    string `json:"previous_id"`
	MutationCount int    `json:"mutation_count"`
}

func recoveryAccountingColumns(table string) ([]string, bool) {
	switch table {
	case "organizations", "tasks":
		return []string{"reserved_microusd", "spent_microusd", "financial_hold"}, true
	case "reservations":
		return []string{"status", "actual_microusd", "revision", "pending_quarantine_id"}, true
	case "cost_evidence", "ledger_entries", "cost_quarantines", "cost_quarantine_resolutions", "financial_incidents":
		return nil, true
	default:
		return nil, false
	}
}

// recordRecoveryAccountingMutationsTx prescribes and executes exact changes.
// It cannot commit, lift a hold, mutate an action, or emit ordinary events.
// Every statement is also checked independently by SQL row/statement guards.
func (s *Store) recordRecoveryAccountingMutationsTx(ctx context.Context, tx pgx.Tx, held RecoveryTargetState, b recoveryAccountingBatch, changes []recoveryAccountingMutation) (recoveryAccountingReceipt, error) {
	var zero recoveryAccountingReceipt
	if held.State != "held" || !validDigest(b.requestSHA256) || !validDigest(b.planSHA256) || b.expectedRevision < 1 || len(changes) == 0 || len(changes) > 10000 {
		return zero, ErrRecoveryAccounting
	}
	// Normalize through PostgreSQL's exact jsonb form. Money never passes through
	// float64; byte bounds match the SQL completeness check, including whitespace.
	normalized := make([]recoveryAccountingMutation, len(changes))
	var total int64
	for i, change := range changes {
		columns, allowed := recoveryAccountingColumns(change.table)
		if !allowed || !json.Valid(change.after) || len(change.before)+len(change.after) > 1<<20 || ((len(columns) != 0) != (change.before != nil)) {
			return zero, ErrRecoveryAccounting
		}
		var before any
		if change.before != nil {
			if !json.Valid(change.before) {
				return zero, ErrRecoveryAccounting
			}
			before = string(change.before)
		}
		var size int64
		var shape bool
		n := recoveryAccountingMutation{table: change.table}
		if err := tx.QueryRow(ctx, `SELECT $1::jsonb,$2::jsonb,
 octet_length(COALESCE(($1::jsonb)::text,''))+octet_length(($2::jsonb)::text),
 ($1::jsonb IS NULL OR jsonb_typeof($1::jsonb)='object') AND jsonb_typeof($2::jsonb)='object'`, before, string(change.after)).Scan(&n.before, &n.after, &size, &shape); err != nil || !shape || size > 1<<20 {
			return zero, ErrRecoveryAccounting
		}
		total += size
		if total > 16<<20 {
			return zero, ErrRecoveryAccounting
		}
		normalized[i] = n
	}
	q := func(table string) string { return pgx.Identifier{s.runtimeSchema, table}.Sanitize() }
	// Typed archive staging flushes all constraints before this phase. Restore
	// this one batch constraint's deferred mode before inserting its header.
	if _, err := tx.Exec(ctx, "SET CONSTRAINTS "+pgx.Identifier{s.runtimeSchema, "recovery_accounting_complete"}.Sanitize()+" DEFERRED"); err != nil {
		return zero, ErrRecoveryAccounting
	}
	var previous string
	var ordinal int64
	if err := tx.QueryRow(ctx, "SELECT COALESCE(max(ordinal),0)+1,COALESCE((SELECT id FROM "+q("recovery_accounting_batches")+" WHERE restore_id=$1 ORDER BY ordinal DESC LIMIT 1),'') FROM "+q("recovery_accounting_batches")+" WHERE restore_id=$1", held.RestoreID).Scan(&ordinal, &previous); err != nil {
		return zero, ErrRecoveryAccounting
	}
	if _, err := tx.Exec(ctx, "INSERT INTO "+q("recovery_accounting_batches")+`(id,restore_id,target_installation_id,evidence_batch_id,organization_id,reservation_id,expected_revision,ordinal,previous_id,request_sha256,plan_sha256,mutation_count,mutation_bytes)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11,$12,$13)`, b.id, held.RestoreID, held.InstallationID, b.evidenceBatchID, b.organizationID, b.reservationID, b.expectedRevision, ordinal, previous, b.requestSHA256, b.planSHA256, len(normalized), total); err != nil {
		return zero, ErrRecoveryAccounting
	}
	// Prescribe the whole batch before any mutable financial row changes. The
	// metadata guard validates scope against the pre-apply reservation revision.
	for i, n := range normalized {
		operation := "INSERT"
		var before any
		if n.before != nil {
			operation, before = "UPDATE", string(n.before)
		}
		key := "jsonb_build_array(($6::jsonb)->'organization_id',($6::jsonb)->'id')"
		if n.table == "organizations" {
			key = "jsonb_build_array(($6::jsonb)->'id')"
		} else if n.table == "cost_evidence" {
			key = "jsonb_build_array(($6::jsonb)->'organization_id',($6::jsonb)->'source',($6::jsonb)->'reference')"
		}
		if _, err := tx.Exec(ctx, "INSERT INTO "+q("recovery_accounting_mutations")+"(batch_id,ordinal,table_name,operation,key_parts,before_row,after_row) VALUES($1,$2,$3,$4,"+key+",$5::jsonb,$6::jsonb)", b.id, i, n.table, operation, before, string(n.after)); err != nil {
			return zero, ErrRecoveryAccounting
		}
	}
	for _, n := range normalized {
		columns, _ := recoveryAccountingColumns(n.table)
		statement := "INSERT INTO " + q(n.table) + " SELECT r.* FROM pg_catalog.jsonb_populate_record(NULL::" + q(n.table) + ",$1::jsonb) r"
		args := []any{string(n.after)}
		if len(columns) != 0 {
			assignments := make([]string, len(columns))
			for i, column := range columns {
				c := pgx.Identifier{column}.Sanitize()
				assignments[i] = c + "=r." + c
			}
			key := "t.organization_id=r.organization_id AND t.id=r.id"
			if n.table == "organizations" {
				key = "t.id=r.id"
			}
			statement = "UPDATE " + q(n.table) + " t SET " + strings.Join(assignments, ",") + " FROM pg_catalog.jsonb_populate_record(NULL::" + q(n.table) + ",$1::jsonb) r WHERE " + key + " AND pg_catalog.to_jsonb(t)=$2::jsonb"
			args = append(args, string(n.before))
		}
		result, err := tx.Exec(ctx, statement, args...)
		if err != nil || result.RowsAffected() != 1 {
			return zero, ErrRecoveryAccounting
		}
	}
	// Checking now also exercises completeness when the caller later commits.
	// The SQL finite slots forbid any additional prescribed or consumed mutation.
	if _, err := tx.Exec(ctx, "SET CONSTRAINTS "+pgx.Identifier{s.runtimeSchema, "recovery_accounting_complete"}.Sanitize()+" IMMEDIATE"); err != nil {
		return zero, ErrRecoveryAccounting
	}
	return recoveryAccountingReceipt{ID: b.id, Ordinal: ordinal, PreviousID: previous, MutationCount: len(normalized)}, nil
}
