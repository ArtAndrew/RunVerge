// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"strings"

	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
)

type recoveryAccountingChainRow struct {
	batch, previous, org, reservation  string
	batchOrdinal, revision, batchBytes int64
	count, ordinal                     int
	table, operation                   string
	key, before, after                 json.RawMessage
}

// The caller already holds the exact target recovery gate and has authenticated
// the original archive/evidence pair. Verification changes temporary rows only.
// A temporary expected-row map explains deviations; ordinary budget readers
// still use the real financial tables, never this map or a parallel ledger.
func (s *Store) verifyRecoveryAccountingBaselineTx(ctx context.Context, tx pgx.Tx, archive *recoveryarchive.VerifiedArchive, snapshot RecoverySnapshotReport, limits RecoverySnapshotLimits, held RecoveryTargetState, catalog RecoverySemanticCatalog, inventory RecoverySnapshotInventory) (recoveryAccountingReceipt, error) {
	var head recoveryAccountingReceipt
	q := func(table string) string { return pgx.Identifier{s.runtimeSchema, table}.Sanitize() }
	rows, err := tx.Query(ctx, `SELECT b.id,b.ordinal,COALESCE(b.previous_id,''),b.organization_id,b.reservation_id,b.expected_revision,b.mutation_count,b.mutation_bytes,
 m.ordinal,m.table_name,m.operation,m.key_parts,m.before_row,m.after_row,
 b.restore_id=$1 AND b.target_installation_id=$2 AND e.restore_id=b.restore_id AND e.target_installation_id=b.target_installation_id AND e.selected_snapshot_sha256=$3,
 a.after_sha256=encode(sha256(convert_to(m.after_row::text,'UTF8')),'hex') AND a.before_sha256 IS NOT DISTINCT FROM
 (CASE WHEN m.before_row IS NULL THEN NULL ELSE encode(sha256(convert_to(m.before_row::text,'UTF8')),'hex') END)
 FROM `+q("recovery_accounting_batches")+` b
 LEFT JOIN `+q("recovery_evidence_batches")+` e ON e.id=b.evidence_batch_id
 LEFT JOIN `+q("recovery_accounting_mutations")+` m ON m.batch_id=b.id
 LEFT JOIN `+q("recovery_accounting_applied")+` a ON a.batch_id=m.batch_id AND a.ordinal=m.ordinal
 ORDER BY b.ordinal,m.ordinal LIMIT 200001`, held.RestoreID, held.InstallationID, snapshot.SHA256)
	if err != nil {
		return head, ErrRecoveryAccounting
	}
	var chain []recoveryAccountingChainRow
	var total int64
	for rows.Next() {
		var row recoveryAccountingChainRow
		var bound, observed bool
		if rows.Scan(&row.batch, &row.batchOrdinal, &row.previous, &row.org, &row.reservation, &row.revision, &row.count, &row.batchBytes, &row.ordinal, &row.table, &row.operation, &row.key, &row.before, &row.after, &bound, &observed) != nil || !bound || !observed {
			rows.Close()
			return head, ErrRecoveryAccounting
		}
		total += int64(len(row.key) + len(row.before) + len(row.after))
		if len(chain) >= 200000 || total > 64<<20 {
			rows.Close()
			return head, ErrRecoveryAccounting
		}
		chain = append(chain, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return head, ErrRecoveryAccounting
	}
	if len(chain) == 0 {
		return head, recoveryStageCheckDigests(ctx, tx, s.runtimeSchema, inventory, snapshot, limits)
	}
	staged, err := s.stageRecoveryTypedDataTx(ctx, tx, archive, snapshot, limits, held, catalog, inventory)
	if err != nil {
		return head, err
	}
	if _, err = tx.Exec(ctx, `CREATE TEMP TABLE rvg_accounting_expected(table_name text NOT NULL,key_parts jsonb NOT NULL,after_row jsonb NOT NULL,PRIMARY KEY(table_name,key_parts)) ON COMMIT DROP`); err != nil {
		return head, ErrRecoveryAccounting
	}
	var batchBytes int64
	var batchCount int
	var scope []string
	var current recoveryAccountingChainRow
	for _, row := range chain {
		if row.batch != current.batch {
			if current.batch != "" && (batchCount != current.count || batchBytes != current.batchBytes) {
				return head, ErrRecoveryAccounting
			}
			if row.batchOrdinal != head.Ordinal+1 || row.previous != head.ID || row.batchOrdinal > 10000 || row.ordinal != 0 {
				return head, ErrRecoveryAccounting
			}
			var revision int64
			if tx.QueryRow(ctx, `SELECT (COALESCE(e.after_row,to_jsonb(r))->>'revision')::bigint,r.scope_task_ids
 FROM `+pgx.Identifier{staged.schema, "reservations"}.Sanitize()+` r LEFT JOIN pg_temp.rvg_accounting_expected e
 ON e.table_name='reservations' AND e.key_parts=jsonb_build_array(r.organization_id,r.id)
 WHERE r.organization_id=$1 AND r.id=$2`, row.org, row.reservation).Scan(&revision, &scope) != nil || revision != row.revision {
				return head, ErrRecoveryAccounting
			}
			current = row
			batchCount, batchBytes = 0, 0
			head = recoveryAccountingReceipt{ID: row.batch, Ordinal: row.batchOrdinal, PreviousID: row.previous, MutationCount: row.count}
		}
		if row.ordinal != batchCount || row.batchOrdinal != current.batchOrdinal || row.previous != current.previous || row.org != current.org || row.reservation != current.reservation || row.revision != current.revision || row.count != current.count || row.batchBytes != current.batchBytes {
			return head, ErrRecoveryAccounting
		}
		columns, allowed := recoveryAccountingColumns(row.table)
		if !allowed || ((len(columns) != 0) != (row.before != nil)) || (len(columns) == 0 && row.operation != "INSERT") || (len(columns) != 0 && row.operation != "UPDATE") {
			return head, ErrRecoveryAccounting
		}
		keyExpression := "jsonb_build_array(($1::jsonb)->'organization_id',($1::jsonb)->'id')"
		predicate := "r.organization_id=($1::jsonb->>0) AND r.id=($1::jsonb->>1)"
		orgExpression := "($1::jsonb)->>'organization_id'"
		if row.table == "organizations" {
			keyExpression = "jsonb_build_array(($1::jsonb)->'id')"
			predicate = "r.id=($1::jsonb->>0)"
			orgExpression = "($1::jsonb)->>'id'"
		}
		if row.table == "cost_evidence" {
			keyExpression = "jsonb_build_array(($1::jsonb)->'organization_id',($1::jsonb)->'source',($1::jsonb)->'reference')"
			predicate = "r.organization_id=($1::jsonb->>0) AND r.source=($1::jsonb->>1) AND r.reference=($1::jsonb->>2)"
		}
		var before any
		if row.before != nil {
			before = string(row.before)
		}
		var valid bool
		var size int64
		// Recheck the fixed mutation policy instead of assuming that a saved
		// prescription itself proves a permitted change.
		policy := "true"
		if len(columns) != 0 {
			policy = "($3::jsonb-$4::text[]) IS NOT DISTINCT FROM ($1::jsonb-$4::text[])"
			if row.table == "reservations" {
				policy += " AND ($1::jsonb->>'id')=$5 AND ($3::jsonb->>'revision')::bigint=$6 AND ($1::jsonb->>'revision')::bigint=$6+1"
			} else {
				policy += " AND NOT (COALESCE(($3::jsonb->>'financial_hold')::boolean,false) AND NOT COALESCE(($1::jsonb->>'financial_hold')::boolean,false))"
				if row.table == "tasks" {
					policy += " AND ($1::jsonb->>'id')=ANY($7::text[])"
				}
			}
		} else if row.table == "cost_quarantine_resolutions" {
			// The final target has a real FK, but also bind the referenced
			// quarantine to this exact reservation, never another tenant's fact.
			policy = "EXISTS(SELECT 1 FROM " + q("cost_quarantines") + " z WHERE z.organization_id=$8 AND z.id=($1::jsonb->>'quarantine_id') AND z.reservation_id=$5)"
		} else {
			policy = "($1::jsonb->>'reservation_id')=$5"
		}
		query := "SELECT " + keyExpression + "=$2::jsonb AND " + orgExpression + "=$8 AND (" + policy + "),octet_length(COALESCE(($3::jsonb)::text,''))+octet_length(($1::jsonb)::text)"
		// Cast even unused parameters so PostgreSQL has a stable parameter
		// contract for every relation-specific expression.
		query += " FROM (SELECT $4::text[],$5::text,$6::bigint,$7::text[]) typed"
		if tx.QueryRow(ctx, query, string(row.after), string(row.key), before, columns, row.reservation, row.revision, scope, row.org).Scan(&valid, &size) != nil || !valid {
			return head, ErrRecoveryAccounting
		}
		var expected json.RawMessage
		err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT after_row FROM pg_temp.rvg_accounting_expected WHERE table_name=$2 AND key_parts=$1::jsonb),
 (SELECT to_jsonb(r) FROM `+pgx.Identifier{staged.schema, row.table}.Sanitize()+` r WHERE `+predicate+`))`, string(row.key), row.table).Scan(&expected)
		if err != nil {
			return head, ErrRecoveryAccounting
		}
		var expectedValue any
		if expected != nil {
			expectedValue = string(expected)
		}
		if tx.QueryRow(ctx, "SELECT $1::jsonb IS NOT DISTINCT FROM $2::jsonb", expectedValue, before).Scan(&valid) != nil || !valid {
			return head, ErrRecoveryAccounting
		}
		if _, err = tx.Exec(ctx, `INSERT INTO pg_temp.rvg_accounting_expected VALUES($1,$2::jsonb,$3::jsonb)
 ON CONFLICT(table_name,key_parts) DO UPDATE SET after_row=excluded.after_row`, row.table, string(row.key), string(row.after)); err != nil {
			return head, ErrRecoveryAccounting
		}
		batchBytes += size
		batchCount++
	}
	if batchCount != current.count || batchBytes != current.batchBytes {
		return head, ErrRecoveryAccounting
	}
	// Every target business row must equal its authenticated original or the
	// last exact permitted after-row. This detects unexplained extra/deleted
	// rows as well as mutations outside financial fields.
	for _, table := range inventory.Tables {
		if !table.Exported {
			continue
		}
		keys := make([]string, len(table.PrimaryKey))
		for i, key := range table.PrimaryKey {
			keys[i] = "r." + pgx.Identifier{key}.Sanitize()
		}
		projection := func(schema string) string {
			return "SELECT jsonb_build_array(" + strings.Join(keys, ",") + ") AS k,to_jsonb(r) AS v FROM " + pgx.Identifier{schema, table.Name}.Sanitize() + " r"
		}
		query := `WITH original AS (` + projection(staged.schema) + `), actual AS (` + projection(s.runtimeSchema) + `), expected AS (
 SELECT COALESCE(o.k,e.key_parts) AS k,COALESCE(e.after_row,o.v) AS v FROM original o
 FULL JOIN (SELECT key_parts,after_row FROM pg_temp.rvg_accounting_expected WHERE table_name=$1) e ON e.key_parts=o.k)
 SELECT NOT EXISTS(SELECT 1 FROM expected e FULL JOIN actual a ON a.k=e.k WHERE a.v IS DISTINCT FROM e.v)`
		var equal bool
		if tx.QueryRow(ctx, query, table.Name).Scan(&equal) != nil || !equal {
			return head, ErrRecoveryAccounting
		}
	}
	if err = validateRecoveryInvariants(ctx, tx, s.runtimeSchema); err != nil {
		return head, err
	}
	if _, err = checkRecoveryCostHistory(ctx, tx, s.runtimeSchema); err != nil {
		return head, err
	}
	// Leave no TEMP relations behind: the caller may now stage the late source
	// independently. Errors retain the caller's rollback-only cleanup contract.
	drop := []string{pgx.Identifier{staged.schema, "rvg_accounting_expected"}.Sanitize()}
	for _, table := range inventory.Tables {
		if table.Exported {
			drop = append(drop, pgx.Identifier{staged.schema, table.Name}.Sanitize())
		}
	}
	// Staging represents sequence observations as ordinary TEMP tables too.
	// They must leave with the snapshot before another stage on this backend.
	for _, seq := range snapshot.Sequences {
		drop = append(drop, pgx.Identifier{staged.schema, seq.Name}.Sanitize())
	}
	if _, err = tx.Exec(ctx, "DROP TABLE "+strings.Join(drop, ",")); err != nil {
		return head, ErrRecoveryAccounting
	}
	return head, nil
}
