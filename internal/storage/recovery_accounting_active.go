// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
)

type recoveryAccountingAdmission struct {
	headID        string
	headOrdinal   int64
	organizations []string
}

func (a recoveryAccountingAdmission) includes(org string) bool {
	_, found := slices.BinarySearch(a.organizations, org)
	return found
}

// Only a permanent admission on the exact provisioned target can permit
// ordinary changes alongside the authenticated accounting baseline.
func (s *Store) recoveryAccountingAdmissionTx(ctx context.Context, tx pgx.Tx, held RecoveryTargetState, snapshot string) (recoveryAccountingAdmission, error) {
	var result recoveryAccountingAdmission
	var bound bool
	var importSHA string
	err := tx.QueryRow(ctx, `SELECT accounting_head_id,accounting_head_ordinal,organizations,import_receipt_sha256,
	 restore_id=$1 AND target_installation_id=$2 AND database_oid=(
	  SELECT oid FROM pg_catalog.pg_database WHERE datname=pg_catalog.current_database())
	 FROM `+pgx.Identifier{s.runtimeSchema, "recovery_work_admissions"}.Sanitize(), held.RestoreID, held.InstallationID).
		Scan(&result.headID, &result.headOrdinal, &result.organizations, &importSHA, &bound)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil || !bound || s.recoveryProvision == nil || s.recoveryProvision.intent.WALTimeline == "" ||
		len(result.organizations) == 0 || !slices.IsSorted(result.organizations) ||
		s.checkRecoveryProvisionImportTx(ctx, tx, RecoveryTargetSpec{TargetInstallationID: held.InstallationID, RestoreID: held.RestoreID,
			SourceID: held.SourceID, BackupID: held.BackupID, CutUTC: held.CutUTC, SchemaSHA256: held.SchemaSHA256}, snapshot) != nil {
		return recoveryAccountingAdmission{}, ErrRecoveryAccounting
	}
	receipt, err := readRecoveryImportReceipt(ctx, tx, s.runtimeSchema)
	if err != nil || !recoveryImportReceiptMatches(receipt, importSHA) {
		return recoveryAccountingAdmission{}, ErrRecoveryAccounting
	}
	return result, nil
}

// The organization totals are independently recomputed from the entire ledger
// and all reservations, including fresh ones. A verified historical hold
// can only stay set. Other aggregates can advance under their own row guards.
func recoveryAccountingSharedRow(table, before, after string) string {
	switch table {
	case "organizations":
		const columns = "ARRAY['reserved_microusd','spent_microusd','financial_hold']"
		return "(" + before + "-" + columns + ")=(" + after + "-" + columns + ") AND (" +
			before + "->>'financial_hold'<>'true' OR " + after + "->>'financial_hold'='true')"
	case "material_storage":
		const columns = "ARRAY['used_bytes','used_documents']"
		return "(" + before + "-" + columns + ")=(" + after + "-" + columns + ")" +
			" AND (" + before + "->>'used_bytes')::bigint<=(" + after + "->>'used_bytes')::bigint" +
			" AND (" + before + "->>'used_documents')::bigint<=(" + after + "->>'used_documents')::bigint"
	case "configuration_revisions":
		const columns = "ARRAY['revision','updated_at']"
		return "(" + before + "-" + columns + ")=(" + after + "-" + columns + ")" +
			" AND (" + before + "->>'revision')::bigint<(" + after + "->>'revision')::bigint"
	default:
		return ""
	}
}

func (s *Store) recoveryAccountingActiveDifference(table RecoverySnapshotTable) string {
	q := func(name string) string { return pgx.Identifier{s.runtimeSchema, name}.Sanitize() }
	scope := "a.v->>'organization_id'=ANY($2::text[])"
	allowed := scope
	switch table.Name {
	case "organizations":
		scope = "a.v->>'id'=ANY($2::text[])"
		allowed = "false"
	case "principals", "access_tokens", "browser_sessions", "identity_mappings":
		id := "principal_id"
		if table.Name == "principals" {
			id = "id"
		}
		allowed = "EXISTS(SELECT 1 FROM " + q("memberships") +
			" m WHERE m.principal_id=a.v->>'" + id + "' AND m.organization_id=ANY($2::text[]))"
	case "login_transactions":
		// A browser login starts before its organization is known. It can
		// only yield a session after the existing identity and scope checks.
		allowed = "cardinality($2::text[])>0"
	case "cost_evidence", "ledger_entries", "cost_quarantines", "financial_incidents":
		allowed += " AND NOT " + q("recovery_is_historical") +
			"('reservations',jsonb_build_array(a.v->'organization_id',a.v->'reservation_id'))"
	case "cost_quarantine_resolutions":
		allowed += " AND EXISTS(SELECT 1 FROM " + q("cost_quarantines") +
			" z WHERE z.organization_id=a.v->>'organization_id' AND z.id=a.v->>'quarantine_id' AND NOT " +
			q("recovery_is_historical") + "('reservations',jsonb_build_array(z.organization_id,z.reservation_id)))"
	}
	same := "a.v IS NOT DISTINCT FROM e.v"
	if shared := recoveryAccountingSharedRow(table.Name, "e.v", "a.v"); shared != "" {
		same += " OR (" + scope + " AND " + shared + ")"
	}
	return "CASE WHEN e.k IS NULL THEN NOT COALESCE(" + allowed + ",false)" +
		" WHEN a.k IS NULL THEN true ELSE NOT COALESCE(" + same + ",false) END"
}
