// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/recoveryarchive"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"io"
	"strings"
	"time"
)

var ErrRecoveryEvidence = errors.New("recovery evidence not accepted; target remains held")
var ErrRecoveryEvidenceUnknown = errors.New("evidence commit outcome unknown; inspect exact batch without replay")

type RecoveryEvidenceReport struct {
	BatchID                  string                        `json:"batch_id"`
	TargetID                 string                        `json:"target_id"`
	RestoreID                string                        `json:"restore_id"`
	SelectedSnapshotSHA256   string                        `json:"selected_snapshot_sha256"`
	EvidenceSnapshotSHA256   string                        `json:"evidence_snapshot_sha256"`
	CaptureLinkSHA256        string                        `json:"capture_link_sha256"`
	EvidenceCutUTC           string                        `json:"evidence_cut_utc"`
	PayloadBytes             int64                         `json:"payload_bytes,string"`
	ChangedRows              int64                         `json:"changed_rows,string"`
	Added                    int64                         `json:"added,string"`
	Changed                  int64                         `json:"changed,string"`
	MissingFromLaterSnapshot int64                         `json:"missing_from_later_snapshot,string"`
	UnmappedScope            int64                         `json:"unmapped_scope,string"`
	CostHistory              RecoveryCostHistoryReport     `json:"cost_history"`
	HistoryCoverage          RecoveryHistoryCoverageReport `json:"history_coverage"`
	ReconciliationState      string                        `json:"reconciliation_state"`
	ExecutionHeld            bool                          `json:"execution_held"`
}

func recoveryManifestReceipt(a *recoveryarchive.VerifiedArchive) string {
	raw, _ := json.Marshal(a.Manifest())
	return Hash(raw)
}

func bindRecoveryEvidence(ctx context.Context, selected, late *recoveryarchive.VerifiedArchive, verified *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits) (RecoverySnapshotReport, RecoverySnapshotReport, error) {
	if late == nil || !verified.MatchesEvidenceManifest(late.Manifest()) {
		return RecoverySnapshotReport{}, RecoverySnapshotReport{}, ErrRecoveryEvidence
	}
	var zero RecoverySnapshotReport
	if selected == nil || late == nil || verified == nil || !limits.valid() {
		return zero, zero, ErrRecoveryEvidence
	}
	l := verified.Link()
	original, err := recoveryBoundArchive(ctx, selected, spec, limits)
	if err != nil {
		return zero, zero, err
	}
	if l.SourceID != spec.SourceID || l.SelectedCutUTC != spec.CutUTC || l.SelectedSnapshotSHA256 != original.SHA256 || l.SelectedReceiptSHA256 != recoveryManifestReceipt(selected) || l.EvidenceReceiptSHA256 != recoveryManifestReceipt(late) {
		return zero, zero, ErrRecoveryEvidence
	}
	lateSpec := spec
	lateSpec.BackupID = late.Manifest().Metadata.BackupID
	lateSpec.CutUTC = l.EvidenceCutUTC
	lateSpec.SchemaSHA256 = late.Manifest().Metadata.SchemaSHA256
	later, err := recoveryBoundArchive(ctx, late, lateSpec, limits)
	if err != nil || later.SHA256 != l.EvidenceSnapshotSHA256 || !SupportedRecoverySourceSchema(later.Metadata.SchemaSHA256) {
		return zero, zero, ErrRecoveryEvidence
	}
	return original, later, nil
}

// evidenceTargetTx is separate from empty-target import admission. It verifies
// the exact committed held target; it never grants a populated-import bypass.
func (s *Store) evidenceTargetTx(ctx context.Context, tx pgx.Tx, spec RecoveryTargetSpec, selectedSHA string) (RecoveryTargetState, RecoverySemanticCatalog, error) {
	var held RecoveryTargetState
	var catalog RecoverySemanticCatalog
	if _, err := tx.Exec(ctx, "SET LOCAL search_path=pg_catalog,pg_temp; SET LOCAL timezone='UTC'; SET LOCAL lock_timeout='5000'; SET LOCAL statement_timeout='60000'"); err != nil {
		return held, catalog, ErrRecoveryEvidence
	}
	if _, err := tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock_shared(841932079)"); err != nil {
		return held, catalog, ErrRecoveryEvidence
	}
	if _, err := tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock(841932080,pg_catalog.hashtext($1))", s.runtimeSchema); err != nil {
		return held, catalog, ErrRecoveryEvidence
	}
	var err error
	catalog, err = validateRecoverySemanticCatalog(ctx, tx, s.runtimeSchema)
	if err != nil {
		return held, catalog, err
	}
	held, err = readRecoveryState(ctx, s.runtimeSchema, tx)
	if err != nil || held.State != "held" || held.InstallationID != spec.TargetInstallationID || held.RestoreID != spec.RestoreID || held.BackupID != spec.BackupID || held.SourceID != spec.SourceID || held.CutUTC != spec.CutUTC || held.SchemaSHA256 != spec.SchemaSHA256 {
		return held, catalog, ErrRecoveryEvidence
	}
	r, err := readRecoveryImportReceipt(ctx, tx, s.runtimeSchema)
	if err != nil || r.Profile != "pg18-held-import-v1" || r.TargetInstallationID != held.InstallationID || r.RestoreID != held.RestoreID || r.SourceID != held.SourceID || r.BackupID != held.BackupID || r.CutUTC != held.CutUTC || r.SchemaSHA256 != held.SchemaSHA256 || r.SnapshotSHA256 != selectedSHA || !recoveryImportProfileSupported(r) {
		return held, catalog, ErrRecoveryEvidence
	}
	if err = recoveryCheckMarkerCounts(ctx, tx, s.runtimeSchema, r.Tables); err != nil {
		return held, catalog, err
	}
	return held, catalog, nil
}

// IngestRecoveryEvidence preserves the complete authenticated late snapshot and
// differences inside the exact restored target. All normal business rows remain
// unchanged. This is factual evidence acceptance, never financial settlement,
// restored authority or execution permission. An unknown COMMIT is not retried.
func (s *Store) IngestRecoveryEvidence(ctx context.Context, selected, late *recoveryarchive.VerifiedArchive, verified *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits) (result RecoveryEvidenceReport, resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	original, later, err := bindRecoveryEvidence(ctx, selected, late, verified, spec, limits)
	if err != nil {
		return result, err
	}
	connection, err := s.Pool.Acquire(ctx)
	if err != nil {
		return result, ErrRecoveryEvidence
	}
	defer connection.Release()
	tx, err := connection.Begin(ctx)
	if err != nil {
		return result, ErrRecoveryEvidence
	}
	committing := false
	defer func() {
		if !committing {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if tx.Rollback(cleanup) != nil {
				_ = connection.Conn().Close(cleanup)
				result, resultErr = RecoveryEvidenceReport{}, ErrRecoveryEvidenceUnknown
			}
		}
	}()
	held, catalog, err := s.evidenceTargetTx(ctx, tx, spec, original.SHA256)
	if err != nil {
		return result, err
	}
	q := func(name string) string { return pgx.Identifier{s.runtimeSchema, name}.Sanitize() }
	var exists bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+q("recovery_evidence_batches")+")").Scan(&exists); err != nil || exists {
		return result, ErrRecoveryEvidence
	}
	inventory, err := recoverySnapshotInventory(ctx, tx, s.runtimeSchema, true)
	if err != nil {
		return result, err
	}
	baseInventory, err := recoverySourceInventory(inventory, original.InventorySHA256)
	if err != nil {
		return result, err
	}
	// A receipt and marker count do not prove the old rows are unchanged.
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
	if err = s.projectEvidenceChanges(ctx, tx, staged); err != nil {
		return result, err
	}
	l := verified.Link()
	result = RecoveryEvidenceReport{BatchID: l.OperationID, TargetID: held.InstallationID, RestoreID: held.RestoreID, SelectedSnapshotSHA256: original.SHA256, EvidenceSnapshotSHA256: later.SHA256, CaptureLinkSHA256: verified.SHA256(), EvidenceCutUTC: later.Metadata.CutUTC, PayloadBytes: later.WireBytes, CostHistory: staged.report.CostHistory, HistoryCoverage: staged.report.HistoryCoverage, ReconciliationState: "pending", ExecutionHeld: true}
	err = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE classification='added'),count(*) FILTER(WHERE classification='changed'),count(*) FILTER(WHERE classification='missing_from_later_snapshot'),count(*) FILTER(WHERE organization_id IS NULL) FROM pg_temp.rvg_evidence_diff`).Scan(&result.ChangedRows, &result.Added, &result.Changed, &result.MissingFromLaterSnapshot, &result.UnmappedScope)
	if err != nil || result.ChangedRows > 2000000 {
		return RecoveryEvidenceReport{}, ErrRecoveryEvidence
	}
	report, _ := json.Marshal(result)
	cut, _ := time.Parse(time.RFC3339Nano, result.EvidenceCutUTC)
	if _, err = tx.Exec(ctx, "SET CONSTRAINTS "+pgx.Identifier{s.runtimeSchema, "recovery_evidence_complete"}.Sanitize()+" DEFERRED"); err != nil {
		return RecoveryEvidenceReport{}, ErrRecoveryEvidence
	}
	_, err = tx.Exec(ctx, `INSERT INTO `+q("recovery_evidence_batches")+`(id,restore_id,target_installation_id,source_id,selected_snapshot_sha256,selected_receipt_sha256,evidence_receipt_sha256,evidence_snapshot_sha256,evidence_schema_sha256,evidence_cut_at,capture_link,capture_link_sha256,target_catalog_sha256,payload_bytes,chunk_count,changed_rows,report) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, result.BatchID, result.RestoreID, result.TargetID, l.SourceID, original.SHA256, l.SelectedReceiptSHA256, l.EvidenceReceiptSHA256, later.SHA256, later.Metadata.SchemaSHA256, cut, verified.Bytes(), verified.SHA256(), recoverySemanticCatalogSHA256, later.WireBytes, (later.WireBytes+(1<<20)-1)/(1<<20), result.ChangedRows, report)
	if err != nil {
		return RecoveryEvidenceReport{}, ErrRecoveryEvidence
	}
	if _, err = late.Seek(0, io.SeekStart); err != nil {
		return RecoveryEvidenceReport{}, ErrRecoveryEvidence
	}
	buffer := make([]byte, 1<<20)
	defer clear(buffer)
	hash := sha256.New()
	var total int64
	for ordinal := 0; ; ordinal++ {
		n, e := io.ReadFull(late, buffer)
		if e == io.EOF {
			break
		}
		if e != nil && e != io.ErrUnexpectedEOF {
			return RecoveryEvidenceReport{}, ErrRecoveryEvidence
		}
		total += int64(n)
		if total > later.WireBytes || ordinal >= 384 {
			return RecoveryEvidenceReport{}, ErrRecoveryEvidence
		}
		hash.Write(buffer[:n])
		if _, err = tx.Exec(ctx, "INSERT INTO "+q("recovery_evidence_chunks")+"(batch_id,ordinal,payload,payload_sha256) VALUES($1,$2,$3,$4)", result.BatchID, ordinal, buffer[:n], Hash(buffer[:n])); err != nil {
			return RecoveryEvidenceReport{}, ErrRecoveryEvidence
		}
		if e == io.ErrUnexpectedEOF {
			break
		}
	}
	if total != later.WireBytes || recoverySnapshotHash(hash) != later.SHA256 {
		return RecoveryEvidenceReport{}, ErrRecoveryEvidence
	}
	_, err = tx.Exec(ctx, "INSERT INTO "+q("recovery_evidence_changes")+"(batch_id,ordinal,table_name,key_parts,key_sha256,organization_id,classification,before_sha256,after_sha256) SELECT $1,row_number() OVER(ORDER BY table_name,key_sha256)-1,table_name,key_parts,key_sha256,organization_id,classification,before_sha256,after_sha256 FROM pg_temp.rvg_evidence_diff", result.BatchID)
	if err != nil {
		return RecoveryEvidenceReport{}, ErrRecoveryEvidence
	}
	if _, err = tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
		return RecoveryEvidenceReport{}, ErrRecoveryEvidence
	}
	committing = true
	if err = tx.Commit(ctx); err != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = connection.Conn().Close(cleanup)
		return RecoveryEvidenceReport{}, ErrRecoveryEvidenceUnknown
	}
	return result, nil
}

func (s *Store) projectEvidenceChanges(ctx context.Context, tx pgx.Tx, staged recoveryStagedData) error {
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE rvg_evidence_diff(table_name text NOT NULL,key_parts jsonb NOT NULL,key_sha256 text NOT NULL,organization_id text,classification text NOT NULL,before_sha256 text,after_sha256 text,PRIMARY KEY(table_name,key_sha256)) ON COMMIT DROP`); err != nil {
		return ErrRecoveryEvidence
	}
	for _, table := range staged.inventory.Tables {
		if !table.Exported {
			continue
		}
		keyParts := make([]string, len(table.PrimaryKey))
		for i, k := range table.PrimaryKey {
			keyParts[i] = pgx.Identifier{k}.Sanitize()
		}
		key := "pg_catalog.jsonb_build_array(" + strings.Join(keyParts, ",") + ")"
		org := "NULL::text"
		for _, column := range table.Columns {
			if column.Name == "organization_id" {
				org = "organization_id"
			}
		}
		if table.Name == "organizations" {
			org = "id"
		}
		projection := func(schema string) string {
			return "SELECT " + key + " AS key," + org + " AS org,pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(pg_catalog.to_jsonb(r)::text,'UTF8')),'hex') AS digest FROM " + pgx.Identifier{schema, table.Name}.Sanitize() + " r"
		}
		// PK values, table names and schema names come only from the pinned
		// inventory. Values are never treated as SQL or permission assertions.
		query := `INSERT INTO pg_temp.rvg_evidence_diff SELECT $1,COALESCE(a.key,b.key),pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(COALESCE(a.key,b.key)::text,'UTF8')),'hex'),CASE WHEN octet_length(COALESCE(a.org,b.org)) BETWEEN 1 AND 128 THEN COALESCE(a.org,b.org) END,CASE WHEN b.digest IS NULL THEN 'added' WHEN a.digest IS NULL THEN 'missing_from_later_snapshot' ELSE 'changed' END,b.digest,a.digest FROM (` + projection(s.runtimeSchema) + `) b FULL JOIN (` + projection(staged.schema) + `) a ON a.key=b.key WHERE a.digest IS DISTINCT FROM b.digest`
		if _, err := tx.Exec(ctx, query, table.Name); err != nil {
			return ErrRecoveryEvidence
		}
	}
	return nil
}

// InspectRecoveryEvidence reads the exact committed batch, verifies all stored
// chunks, their ordering and the signed-link digest. It does not retry ingestion.
func (s *Store) InspectRecoveryEvidence(ctx context.Context, spec RecoveryTargetSpec, verified *recoveryarchive.VerifiedCaptureLink) (result RecoveryEvidenceReport, err error) {
	if verified == nil {
		return result, ErrRecoveryEvidence
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, ErrRecoveryEvidence
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if tx.Rollback(cleanup) != nil {
			err = ErrRecoveryEvidence
		}
	}()
	l := verified.Link()
	if _, _, err = s.evidenceTargetTx(ctx, tx, spec, l.SelectedSnapshotSHA256); err != nil {
		return result, err
	}
	return s.inspectRecoveryEvidenceTx(ctx, tx, spec, verified)
}

// Caller already holds the exact recovery target gate and validates its catalog.
func (s *Store) inspectRecoveryEvidenceTx(ctx context.Context, tx pgx.Tx, spec RecoveryTargetSpec, verified *recoveryarchive.VerifiedCaptureLink) (result RecoveryEvidenceReport, err error) {
	l := verified.Link()
	q := func(name string) string { return pgx.Identifier{s.runtimeSchema, name}.Sanitize() }
	var raw, link []byte
	var payload int64
	var chunks int
	var changed int64
	var selected, evidence, linkSHA string
	err = tx.QueryRow(ctx, "SELECT report,capture_link,payload_bytes,chunk_count,changed_rows,selected_snapshot_sha256,evidence_snapshot_sha256,capture_link_sha256 FROM "+q("recovery_evidence_batches")+" WHERE id=$1 AND restore_id=$2", l.OperationID, spec.RestoreID).Scan(&raw, &link, &payload, &chunks, &changed, &selected, &evidence, &linkSHA)
	if err != nil || json.Unmarshal(raw, &result) != nil || Hash(link) != verified.SHA256() || linkSHA != verified.SHA256() || selected != l.SelectedSnapshotSHA256 || evidence != l.EvidenceSnapshotSHA256 || result.BatchID != l.OperationID || result.TargetID != spec.TargetInstallationID || result.RestoreID != spec.RestoreID || result.ReconciliationState != "pending" || !result.ExecutionHeld || result.PayloadBytes != payload || result.ChangedRows != changed {
		return RecoveryEvidenceReport{}, ErrRecoveryEvidence
	}
	rows, e := tx.Query(ctx, "SELECT ordinal,payload,payload_sha256 FROM "+q("recovery_evidence_chunks")+" WHERE batch_id=$1 ORDER BY ordinal", l.OperationID)
	if e != nil {
		return RecoveryEvidenceReport{}, ErrRecoveryEvidence
	}
	h := sha256.New()
	var total int64
	count := 0
	for rows.Next() {
		var ordinal int
		var data []byte
		var digest string
		if rows.Scan(&ordinal, &data, &digest) != nil || ordinal != count || len(data) > 1<<20 || Hash(data) != digest {
			rows.Close()
			return RecoveryEvidenceReport{}, ErrRecoveryEvidence
		}
		h.Write(data)
		total += int64(len(data))
		count++
	}
	e = rows.Err()
	rows.Close()
	if e != nil || total != payload || count != chunks || recoverySnapshotHash(h) != evidence {
		return RecoveryEvidenceReport{}, ErrRecoveryEvidence
	}
	var actual int64
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM "+q("recovery_evidence_changes")+" WHERE batch_id=$1", l.OperationID).Scan(&actual); err != nil || actual != changed {
		return RecoveryEvidenceReport{}, ErrRecoveryEvidence
	}
	return result, nil
}
