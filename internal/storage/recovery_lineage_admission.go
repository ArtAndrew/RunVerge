// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Read only the evidence needed to check historical continuity. The local
// database and role OIDs in this record describe its old installation.
func readRecoveryLineageAdmission(ctx context.Context, tx pgx.Tx, schema string, receipt RecoveryImportReceipt) (recoveryAccountingAdmission, string, error) {
	var a recoveryAccountingAdmission
	var importSHA string
	var bound bool
	err := tx.QueryRow(ctx, strings.ReplaceAll(`SELECT a.accounting_head_id,a.accounting_head_ordinal,a.organizations,a.import_receipt_sha256,
 a.restore_id=$1 AND a.target_installation_id=$2 AND a.runtime_role=$3
 AND array_ndims(a.organizations)=1 AND array_lower(a.organizations,1)=1
 AND (a.accounting_head_ordinal=0 OR EXISTS(SELECT 1 FROM $S$.recovery_accounting_batches b
  WHERE b.id=a.accounting_head_id AND b.ordinal=a.accounting_head_ordinal
  AND b.restore_id=a.restore_id AND b.target_installation_id=a.target_installation_id))
 AND NOT EXISTS(SELECT 1 FROM $S$.recovery_evidence_batches e WHERE e.payload_bytes=0 AND
  (e.restore_id<>a.restore_id OR e.target_installation_id<>a.target_installation_id OR e.report IS DISTINCT FROM
   jsonb_build_object('profile',$4::text,'activation_id',a.activation_id,'import_receipt_sha256',a.import_receipt_sha256,
   'review_sha256',a.review_sha256,'source_stop_sha256',a.source_stop_sha256,'retirement_seal_sha256',a.retirement_seal_sha256)))
 FROM $S$.recovery_work_admissions a`, "$S$", pgx.Identifier{schema}.Sanitize()),
		receipt.RestoreID, receipt.TargetInstallationID, recoveryRuntimeRoleName(receipt.RestoreID), recoveryColdUsageProfile).
		Scan(&a.headID, &a.headOrdinal, &a.organizations, &importSHA, &bound)
	if errors.Is(err, pgx.ErrNoRows) {
		return recoveryAccountingAdmission{}, "", nil
	}
	if err != nil || !bound || len(a.organizations) < 1 || len(a.organizations) > 64 {
		return recoveryAccountingAdmission{}, "", ErrRecoveryStageData
	}
	var previous string
	for _, org := range a.organizations {
		if !validText(org, 128) || org <= previous {
			return recoveryAccountingAdmission{}, "", ErrRecoveryStageData
		}
		previous = org
	}
	return a, importSHA, nil
}

func recoveryLineageAdmissionTx(ctx context.Context, tx pgx.Tx, schema string, expected recoveryLineageGenerationStart) (recoveryAccountingAdmission, error) {
	receipt, err := readRecoveryImportReceipt(ctx, tx, schema)
	stamp, stampErr := time.Parse(time.RFC3339Nano, expected.AdmissionReceiptTime)
	if err != nil || stampErr != nil || !stamp.Equal(receipt.ImportedAt) {
		return recoveryAccountingAdmission{}, ErrRecoveryStageData
	}
	receipt.ImportedAt = stamp
	a, importSHA, err := readRecoveryLineageAdmission(ctx, tx, schema, receipt)
	if err != nil || len(a.organizations) == 0 || digestValue(receipt) != importSHA {
		return recoveryAccountingAdmission{}, ErrRecoveryStageData
	}
	return a, nil
}

// Historical row images are JSON rather than typed organization rows. A
// difference in shared aggregates must still look like a real database row.
func recoveryLineageOrganizationChange(before, after string) string {
	return "COALESCE((" + recoveryAccountingSharedRow("organizations", before, after) + `)
 AND jsonb_typeof(` + before + `->'financial_hold')='boolean' AND jsonb_typeof(` + after + `->'financial_hold')='boolean'
 AND NOT EXISTS(SELECT 1 FROM (VALUES (` + before + `),(` + after + `)) images(v)
 CROSS JOIN (VALUES ('reserved_microusd'),('spent_microusd')) counters(k)
 WHERE (jsonb_typeof(v->k)='number' AND (v->>k) ~ '^[0-9]{1,19}$' AND (v->k)<='9223372036854775807'::jsonb) IS DISTINCT FROM true),false)`
}
