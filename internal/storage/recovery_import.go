// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
)

var ErrRecoveryImport = errors.New("recovery import failed; target remains held")
var ErrRecoveryImportOutcomeUnknown = errors.New("recovery import commit outcome unknown; inspect exact target before another operation")
var ErrRecoveryImportCleanup = errors.New("recovery import rollback not confirmed; inspect exact target")

type RecoveryImportReceipt struct {
	Profile              string                         `json:"profile"`
	TargetInstallationID string                         `json:"target_installation_id"`
	RestoreID            string                         `json:"restore_id"`
	BackupID             string                         `json:"backup_id"`
	SourceID             string                         `json:"source_id"`
	CutUTC               string                         `json:"cut_utc"`
	SnapshotSHA256       string                         `json:"snapshot_sha256"`
	SchemaSHA256         string                         `json:"schema_sha256"`
	InventorySHA256      string                         `json:"inventory_sha256"`
	CatalogSHA256        string                         `json:"catalog_sha256"`
	Tables               []RecoverySnapshotTableSummary `json:"tables"`
	Sequences            []RecoverySnapshotSequence     `json:"sequences"`
	CostHistory          RecoveryCostHistoryReport      `json:"cost_history"`
	HistoryCoverage      RecoveryHistoryCoverageReport  `json:"history_coverage"`
	ImportedAt           time.Time                      `json:"imported_at"`
}

type RecoveryImportInspection struct {
	State   string                 `json:"state"`
	Receipt *RecoveryImportReceipt `json:"receipt,omitempty"`
}

// ImportRecoverySnapshot is a trusted-host path, unmounted from tenant APIs.
// It accepts matching-schema authenticated archives only, into the exact empty
// held target. It neither clears that hold nor issues authority, replays work,
// contacts providers or retries a lost commit acknowledgment. The caller owns
// the archive and must close it. Metadata remains private operator information.
func (s *Store) ImportRecoverySnapshot(ctx context.Context, archive *recoveryarchive.VerifiedArchive, spec RecoveryTargetSpec, limits RecoverySnapshotLimits) (result RecoveryImportReceipt, resultErr error) {
	if archive == nil || !limits.valid() {
		return result, ErrRecoverySnapshotInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	snapshot, err := recoveryBoundArchive(ctx, archive, spec, limits)
	if err != nil {
		return result, err
	}
	connection, err := s.Pool.Acquire(ctx)
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	defer connection.Release()
	tx, err := connection.Begin(ctx)
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	commitStarted := false
	defer func() {
		if commitStarted {
			return
		}
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := tx.Rollback(cleanup); err != nil {
			_ = connection.Conn().Close(cleanup)
			result, resultErr = RecoveryImportReceipt{}, ErrRecoveryImportCleanup
		}
	}()
	staged, err := s.stageRecoverySnapshotTx(ctx, tx, archive, spec, snapshot, limits)
	if err != nil {
		return result, err
	}
	if err = s.importStagedRecoveryTx(ctx, tx, staged, limits); err != nil {
		return result, err
	}
	result, err = readRecoveryImportReceipt(ctx, tx, s.runtimeSchema)
	if err != nil {
		return RecoveryImportReceipt{}, recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	commitStarted = true
	if err = tx.Commit(ctx); err != nil {
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = connection.Conn().Close(closeCtx)
		return RecoveryImportReceipt{}, ErrRecoveryImportOutcomeUnknown
	}
	return result, nil
}

func recoveryBoundArchive(ctx context.Context, archive *recoveryarchive.VerifiedArchive, spec RecoveryTargetSpec, limits RecoverySnapshotLimits) (RecoverySnapshotReport, error) {
	if _, err := archive.Seek(0, 0); err != nil {
		return RecoverySnapshotReport{}, recoverySnapshotError(ctx, ErrRecoverySnapshotIO)
	}
	snapshot, err := ValidateRecoverySnapshot(ctx, archive, limits)
	if err != nil {
		return snapshot, err
	}
	m := snapshot.Metadata
	manifest := archive.Manifest()
	if manifest.Metadata != (recoveryarchive.Metadata{BackupID: m.BackupID, SourceID: m.SourceID, CutUTC: m.CutUTC, SchemaSHA256: m.SchemaSHA256}) || manifest.PlaintextSHA256 != snapshot.SHA256 || manifest.PlaintextBytes != snapshot.WireBytes || spec.BackupID != m.BackupID || spec.SourceID != m.SourceID || spec.CutUTC != m.CutUTC || spec.SchemaSHA256 != m.SchemaSHA256 || !validRecoveryID(spec.TargetInstallationID, "inst") || !validRecoveryID(spec.RestoreID, "restore") || spec.TargetInstallationID == m.SourceID {
		return RecoverySnapshotReport{}, ErrRecoverySnapshotInvalid
	}
	return snapshot, nil
}

func (s *Store) importStagedRecoveryTx(ctx context.Context, tx pgx.Tx, staged recoveryStagedData, limits RecoverySnapshotLimits) error {
	q := func(table string) string { return pgx.Identifier{s.runtimeSchema, table}.Sanitize() }
	// Hold ACCESS EXCLUSIVE locks across every target relation before suspending
	// any trusted user trigger. Recheck the full catalog after lock acquisition.
	for _, table := range staged.inventory.Tables {
		if _, err := tx.Exec(ctx, "LOCK TABLE "+q(table.Name)+" IN ACCESS EXCLUSIVE MODE"); err != nil {
			return recoverySnapshotError(ctx, ErrRecoveryImport)
		}
	}
	if _, err := validateRecoverySemanticCatalog(ctx, tx, s.runtimeSchema); err != nil {
		return err
	}
	for _, table := range staged.inventory.Tables {
		if table.Exported {
			if _, err := tx.Exec(ctx, "ALTER TABLE "+q(table.Name)+" DISABLE TRIGGER USER"); err != nil {
				return recoverySnapshotError(ctx, ErrRecoveryImport)
			}
		}
	}
	exported := map[string]bool{}
	for _, table := range staged.inventory.Tables {
		exported[table.Name] = table.Exported
	}
	for _, fk := range staged.catalog.Constraints {
		if fk.Type == "f" && exported[fk.Table] && !fk.Deferrable {
			if _, err := tx.Exec(ctx, "ALTER TABLE "+q(fk.Table)+" ALTER CONSTRAINT "+pgx.Identifier{fk.Name}.Sanitize()+" DEFERRABLE INITIALLY DEFERRED"); err != nil {
				return recoverySnapshotError(ctx, ErrRecoveryImport)
			}
		}
	}
	if _, err := tx.Exec(ctx, "SET CONSTRAINTS ALL DEFERRED"); err != nil {
		return recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	r := staged.report
	m := r.Snapshot.Metadata
	cutAt, parseErr := time.Parse(time.RFC3339Nano, m.CutUTC)
	if parseErr != nil {
		return ErrRecoverySnapshotInvalid
	}
	tables, _ := json.Marshal(r.Snapshot.Tables)
	sequences, _ := json.Marshal(r.Snapshot.Sequences)
	cost, _ := json.Marshal(r.CostHistory)
	coverage, _ := json.Marshal(r.HistoryCoverage)
	if _, err := tx.Exec(ctx, `INSERT INTO `+q("recovery_import_receipts")+`(profile,target_installation_id,restore_id,backup_id,source_id,cut_at,snapshot_sha256,schema_sha256,inventory_sha256,catalog_sha256,tables,sequences,cost_history,history_coverage)
 VALUES('pg18-held-import-v1',$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, r.TargetInstallationID, r.RestoreID, m.BackupID, m.SourceID, cutAt, r.Snapshot.SHA256, m.SchemaSHA256, r.Snapshot.InventorySHA256, r.SemanticCatalogSHA256, tables, sequences, cost, coverage); err != nil {
		return recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	counts := map[string]int64{}
	for _, table := range r.Snapshot.Tables {
		counts[table.Name] = table.Rows
	}
	for _, table := range staged.inventory.Tables {
		if table.Exported {
			var columns, keys []string
			for _, column := range table.Columns {
				if column.Generated == "" {
					columns = append(columns, pgx.Identifier{column.Name}.Sanitize())
				}
			}
			for _, key := range table.PrimaryKey {
				keys = append(keys, "source."+pgx.Identifier{key}.Sanitize())
			}
			cols := strings.Join(columns, ",")
			tag, err := tx.Exec(ctx, "INSERT INTO "+q(table.Name)+"("+cols+") OVERRIDING SYSTEM VALUE SELECT "+cols+" FROM "+pgx.Identifier{staged.schema, table.Name}.Sanitize())
			if err != nil || tag.RowsAffected() != counts[table.Name] {
				return recoverySnapshotError(ctx, ErrRecoveryImport)
			}
			tag, err = tx.Exec(ctx, "INSERT INTO "+q("recovery_historical_rows")+"(restore_id,table_name,pk_sha256) SELECT $1,$2,"+pgx.Identifier{s.runtimeSchema, "recovery_row_key"}.Sanitize()+"($2,pg_catalog.jsonb_build_array("+strings.Join(keys, ",")+")) FROM "+q(table.Name)+" source", r.RestoreID, table.Name)
			if err != nil || tag.RowsAffected() != counts[table.Name] {
				return recoverySnapshotError(ctx, ErrRecoveryImport)
			}
		}
	}
	if _, err := tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
		return recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	if err := recoveryStageCheckDigests(ctx, tx, s.runtimeSchema, staged.inventory, r.Snapshot, limits); err != nil {
		return err
	}
	// No setval: RESTART is transactional and takes the sequence lock until this
	// transaction ends. The signed high-water bound includes all retained cursors.
	for _, sequence := range r.Snapshot.Sequences {
		if sequence.NextMinimum < 1 {
			return ErrRecoveryImport
		}
		if _, err := tx.Exec(ctx, "ALTER SEQUENCE "+q(sequence.Name)+" RESTART WITH "+strconv.FormatInt(sequence.NextMinimum, 10)); err != nil {
			return recoverySnapshotError(ctx, ErrRecoveryImport)
		}
		var last int64
		var called bool
		if err := tx.QueryRow(ctx, "SELECT last_value,is_called FROM "+q(sequence.Name)).Scan(&last, &called); err != nil || last != sequence.NextMinimum || called {
			return recoverySnapshotError(ctx, ErrRecoveryImport)
		}
	}
	for _, fk := range staged.catalog.Constraints {
		if fk.Type == "f" && exported[fk.Table] && !fk.Deferrable {
			if _, err := tx.Exec(ctx, "ALTER TABLE "+q(fk.Table)+" ALTER CONSTRAINT "+pgx.Identifier{fk.Name}.Sanitize()+" NOT DEFERRABLE INITIALLY IMMEDIATE"); err != nil {
				return recoverySnapshotError(ctx, ErrRecoveryImport)
			}
		}
	}
	for _, trigger := range staged.catalog.Triggers {
		if exported[trigger.Table] {
			mode := map[string]string{"O": "ENABLE TRIGGER ", "A": "ENABLE ALWAYS TRIGGER ", "R": "ENABLE REPLICA TRIGGER ", "D": "DISABLE TRIGGER "}[trigger.Enabled]
			if mode == "" {
				return ErrRecoveryImport
			}
			if _, err := tx.Exec(ctx, "ALTER TABLE "+q(trigger.Table)+" "+mode+pgx.Identifier{trigger.Name}.Sanitize()); err != nil {
				return recoverySnapshotError(ctx, ErrRecoveryImport)
			}
		}
	}
	if _, err := validateRecoverySemanticCatalog(ctx, tx, s.runtimeSchema); err != nil {
		return err
	}
	return recoveryCheckMarkerCounts(ctx, tx, s.runtimeSchema, r.Snapshot.Tables)
}

func recoveryCheckMarkerCounts(ctx context.Context, tx pgx.Tx, schema string, tables []RecoverySnapshotTableSummary) error {
	var total int64
	for _, table := range tables {
		var count int64
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, "recovery_historical_rows"}.Sanitize()+" WHERE table_name=$1", table.Name).Scan(&count); err != nil || count != table.Rows {
			return recoverySnapshotError(ctx, ErrRecoveryImport)
		}
		total += table.Rows
	}
	var actual int64
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, "recovery_historical_rows"}.Sanitize()).Scan(&actual); err != nil || actual != total {
		return recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	return nil
}

func readRecoveryImportReceipt(ctx context.Context, tx pgx.Tx, schema string) (RecoveryImportReceipt, error) {
	var r RecoveryImportReceipt
	var cut time.Time
	var tables, sequences, cost, coverage []byte
	err := tx.QueryRow(ctx, `SELECT profile,target_installation_id,restore_id,backup_id,source_id,cut_at,snapshot_sha256,schema_sha256,inventory_sha256,catalog_sha256,tables,sequences,cost_history,history_coverage,imported_at FROM `+pgx.Identifier{schema, "recovery_import_receipts"}.Sanitize()+` WHERE singleton`).Scan(&r.Profile, &r.TargetInstallationID, &r.RestoreID, &r.BackupID, &r.SourceID, &cut, &r.SnapshotSHA256, &r.SchemaSHA256, &r.InventorySHA256, &r.CatalogSHA256, &tables, &sequences, &cost, &coverage, &r.ImportedAt)
	if err != nil {
		return r, err
	}
	r.CutUTC = cut.UTC().Format(time.RFC3339Nano)
	if json.Unmarshal(tables, &r.Tables) != nil || json.Unmarshal(sequences, &r.Sequences) != nil || json.Unmarshal(cost, &r.CostHistory) != nil || json.Unmarshal(coverage, &r.HistoryCoverage) != nil {
		return RecoveryImportReceipt{}, ErrRecoveryImport
	}
	return r, nil
}

// InspectRecoveryImport uses fresh database observations, not a cached mutation
// reply. A lock wait or missing/corrupt evidence is an error, never permission
// to drop, repeat, clear a hold or start an ordinary worker.
func (s *Store) InspectRecoveryImport(ctx context.Context, spec RecoveryTargetSpec, snapshotSHA256 string) (result RecoveryImportInspection, resultErr error) {
	if !validDigest(snapshotSHA256) || !validRecoveryID(spec.TargetInstallationID, "inst") || !validRecoveryID(spec.RestoreID, "restore") {
		return RecoveryImportInspection{}, ErrRecoveryStageTarget
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	connection, err := s.Pool.Acquire(ctx)
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	defer connection.Release()
	tx, err := connection.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := tx.Rollback(cleanup); err != nil {
			_ = connection.Conn().Close(cleanup)
			result, resultErr = RecoveryImportInspection{}, ErrRecoveryImportCleanup
		}
	}()
	if _, err = tx.Exec(ctx, "SET LOCAL lock_timeout='5000'"); err != nil {
		return RecoveryImportInspection{}, ErrRecoveryImport
	}
	if _, err = tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock_shared(841932079)"); err != nil {
		return RecoveryImportInspection{}, recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	if _, err = tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock(841932080,pg_catalog.hashtext($1))", s.runtimeSchema); err != nil {
		return RecoveryImportInspection{}, recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	if _, err = validateRecoverySemanticCatalog(ctx, tx, s.runtimeSchema); err != nil {
		return RecoveryImportInspection{}, err
	}
	held, err := readRecoveryState(ctx, s.runtimeSchema, tx)
	if err != nil || held.State != "held" || held.InstallationID != spec.TargetInstallationID || held.RestoreID != spec.RestoreID || held.BackupID != spec.BackupID || held.SourceID != spec.SourceID || held.CutUTC != spec.CutUTC || held.SchemaSHA256 != spec.SchemaSHA256 {
		return RecoveryImportInspection{}, recoverySnapshotError(ctx, ErrRecoveryStageTarget)
	}
	r, err := readRecoveryImportReceipt(ctx, tx, s.runtimeSchema)
	if errors.Is(err, pgx.ErrNoRows) {
		inventory, e := recoverySnapshotInventory(ctx, tx, s.runtimeSchema, true)
		if e != nil {
			return RecoveryImportInspection{}, e
		}
		for _, table := range inventory.Tables {
			if table.Exported || table.Name == "recovery_historical_rows" {
				var populated bool
				if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+pgx.Identifier{s.runtimeSchema, table.Name}.Sanitize()+" LIMIT 1)").Scan(&populated); e != nil || populated {
					return RecoveryImportInspection{}, ErrRecoveryImport
				}
			}
		}
		return RecoveryImportInspection{State: "empty_held"}, nil
	}
	if err != nil {
		return RecoveryImportInspection{}, recoverySnapshotError(ctx, ErrRecoveryImport)
	}
	if r.Profile != "pg18-held-import-v1" || r.TargetInstallationID != spec.TargetInstallationID || r.RestoreID != spec.RestoreID || r.BackupID != spec.BackupID || r.SourceID != spec.SourceID || r.CutUTC != spec.CutUTC || r.SchemaSHA256 != spec.SchemaSHA256 || r.SnapshotSHA256 != snapshotSHA256 || r.CatalogSHA256 != recoverySemanticCatalogSHA256 || r.InventorySHA256 != recoverySnapshotInventorySHA256 {
		return RecoveryImportInspection{}, ErrRecoveryImport
	}
	if err = recoveryCheckMarkerCounts(ctx, tx, s.runtimeSchema, r.Tables); err != nil {
		return RecoveryImportInspection{}, err
	}
	return RecoveryImportInspection{State: "committed_held", Receipt: &r}, nil
}
