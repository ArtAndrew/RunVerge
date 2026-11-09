// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
)

const recoveryColdUsageProfile = "retired-handoff-usage-anchor-v1"

type recoveryUsageEvidence struct {
	original, captured RecoverySnapshotReport
	cold               bool
}

func (s *Store) bindRecoveryUsageEvidence(ctx context.Context, selected, captured *recoveryarchive.VerifiedArchive, link *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits) (e recoveryUsageEvidence, err error) {
	if s.recoveryProvision != nil && s.recoveryProvision.intent.WALTimeline != "" && link != nil && spec.BackupID == link.Link().EvidenceBackupID {
		e.cold = true
		e.original, e.captured, err = s.bindRecoveryHandoffReview(ctx, selected, captured, link, spec, limits, nil)
	} else {
		e.original, e.captured, err = bindRecoveryEvidence(ctx, selected, captured, link, spec, limits)
	}
	return e, err
}

type recoveryColdUsageAnchor struct {
	Profile              string `json:"profile"`
	ActivationID         string `json:"activation_id"`
	ImportReceiptSHA256  string `json:"import_receipt_sha256"`
	ReviewSHA256         string `json:"review_sha256"`
	SourceStopSHA256     string `json:"source_stop_sha256"`
	RetirementSealSHA256 string `json:"retirement_seal_sha256"`
}

// Called under the exact target gate, after authenticating the two archives.
// Existing anchors must match the complete immutable proof. The first usage
// record can add its anchor atomically, including on earlier admitted targets.
func (s *Store) recoveryColdUsageAnchorTx(ctx context.Context, tx pgx.Tx, spec RecoveryTargetSpec, verified *recoveryarchive.VerifiedCaptureLink) (recoveryColdUsageAnchor, error) {
	var proof recoveryColdUsageAnchor
	if verified == nil || verified.Link().Version != 2 || s.recoveryProvision == nil || s.recoveryProvision.intent.WALTimeline == "" {
		return proof, ErrRecoveryUsageBinding
	}
	l := verified.Link()
	stopSHA, sealSHA := verified.RetiredSourceHashes()
	if !validDigest(stopSHA) || !validDigest(sealSHA) {
		return proof, ErrRecoveryUsageBinding
	}
	if err := s.checkRecoveryProvisionImportTx(ctx, tx, spec, l.EvidenceSnapshotSHA256); err != nil {
		return proof, err
	}
	q := func(n string) string { return pgx.Identifier{s.runtimeSchema, n}.Sanitize() }
	if err := tx.QueryRow(ctx, `SELECT activation_id,import_receipt_sha256,review_sha256,source_stop_sha256,retirement_seal_sha256
 FROM `+q("recovery_work_admissions")+` WHERE restore_id=$1 AND target_installation_id=$2 AND database_oid=$3`,
		spec.RestoreID, spec.TargetInstallationID, s.recoveryProvision.databaseOID).
		Scan(&proof.ActivationID, &proof.ImportReceiptSHA256, &proof.ReviewSHA256, &proof.SourceStopSHA256, &proof.RetirementSealSHA256); err != nil ||
		proof.SourceStopSHA256 != stopSHA || proof.RetirementSealSHA256 != sealSHA {
		return recoveryColdUsageAnchor{}, ErrRecoveryUsageBinding
	}
	receipt, err := readRecoveryImportReceipt(ctx, tx, s.runtimeSchema)
	if err != nil || !recoveryImportReceiptMatches(receipt, proof.ImportReceiptSHA256) {
		return recoveryColdUsageAnchor{}, ErrRecoveryUsageBinding
	}
	proof.Profile = recoveryColdUsageProfile
	raw, _ := json.Marshal(proof)
	var matches bool
	err = tx.QueryRow(ctx, `SELECT id=$1 AND target_installation_id=$3 AND source_id=$4 AND
 selected_snapshot_sha256=$5 AND selected_receipt_sha256=$6 AND evidence_snapshot_sha256=$5 AND evidence_receipt_sha256=$6
 AND evidence_schema_sha256=$7 AND evidence_cut_at=$8::text::timestamptz
 AND chunk_count=0 AND changed_rows=0 AND capture_link=$9 AND capture_link_sha256=$10 AND report=$11::jsonb
 FROM `+q("recovery_evidence_batches")+` WHERE (restore_id=$2 AND payload_bytes=0) OR id=$1`,
		l.OperationID, spec.RestoreID, spec.TargetInstallationID, spec.SourceID, l.EvidenceSnapshotSHA256, l.EvidenceReceiptSHA256,
		l.EvidenceSchemaSHA256, l.EvidenceCutUTC, verified.Bytes(), verified.SHA256(), string(raw)).Scan(&matches)
	if !errors.Is(err, pgx.ErrNoRows) && (err != nil || !matches) {
		return recoveryColdUsageAnchor{}, ErrRecoveryUsageBinding
	}
	return proof, nil
}

func (s *Store) persistRecoveryColdUsageAnchorTx(ctx context.Context, tx pgx.Tx, spec RecoveryTargetSpec, link *recoveryarchive.VerifiedCaptureLink, activation string) error {
	proof, err := s.recoveryColdUsageAnchorTx(ctx, tx, spec, link)
	if err != nil || proof.ActivationID != activation {
		return ErrRecoveryUsageRecording
	}
	l := link.Link()
	raw, _ := json.Marshal(proof)
	cut, _ := time.Parse(time.RFC3339Nano, l.EvidenceCutUTC)
	q := pgx.Identifier{s.runtimeSchema, "recovery_evidence_batches"}.Sanitize()
	_, err = tx.Exec(ctx, "INSERT INTO "+q+`(id,restore_id,target_installation_id,source_id,selected_snapshot_sha256,selected_receipt_sha256,
 evidence_receipt_sha256,evidence_snapshot_sha256,evidence_schema_sha256,evidence_cut_at,capture_link,capture_link_sha256,target_catalog_sha256,payload_bytes,chunk_count,changed_rows,report)
 VALUES($1,$2,$3,$4,$5,$6,$6,$5,$7,$8,$9,$10,$11,0,0,0,$12) ON CONFLICT(id) DO NOTHING`,
		l.OperationID, spec.RestoreID, spec.TargetInstallationID, spec.SourceID, l.EvidenceSnapshotSHA256, l.EvidenceReceiptSHA256, l.EvidenceSchemaSHA256, cut, link.Bytes(), link.SHA256(),
		recoverySemanticCatalogSHA256, raw)
	if err != nil {
		return ErrRecoveryUsageRecording
	}
	return nil
}
