// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// Immutable SQL026 layout identities; a later schema needs a new profile.
const recoveryLineageSQL026Inventory = "2c9d16d3cdead2c3dc0c1baa34b0f7ea646a87bada95202d5fa1999d861aea24"
const recoveryLineageSQL026Catalog = "873463718e42a33e7df8ba9eff916260ca46370d49affafc609423338674a9b8"
const recoveryLineageMetadataRowBytes int64 = 2 << 20

type recoveryLineageGenerationStart struct {
	Kind                   string `json:"kind"`
	Profile                string `json:"profile"`
	InstallationID         string `json:"installation_id"`
	RestoreID              string `json:"restore_id"`
	ImportedSourceID       string `json:"imported_source_id"`
	ImportedSnapshotSHA256 string `json:"imported_snapshot_sha256"`
	InventorySHA256        string `json:"inventory_sha256"`
	CatalogSHA256          string `json:"catalog_sha256"`
}
type recoveryLineageGenerationEnd struct {
	Kind   string `json:"kind"`
	Tables int    `json:"tables"`
	Rows   int64  `json:"rows,string"`
	Bytes  int64  `json:"bytes,string"`
}

// Derive SQL identifiers only from the exact trusted inventory. The projection
// includes empty tables and excludes control, credentials and business tables.
func recoveryLineageSourceInventory(inventory RecoverySnapshotInventory) (RecoverySnapshotInventory, error) {
	raw, err := json.Marshal(inventory)
	if err != nil || Hash(raw) != recoveryLineageSQL026Inventory {
		return RecoverySnapshotInventory{}, ErrRecoverySnapshotSchema
	}
	out := RecoverySnapshotInventory{Tables: []RecoverySnapshotTable{}, Sequences: []RecoverySnapshotSequenceDefinition{}}
	for _, table := range inventory.Tables {
		switch table.Name {
		case "recovery_import_receipts", "recovery_historical_rows", "recovery_evidence_batches", "recovery_evidence_chunks", "recovery_evidence_changes", "recovery_accounting_batches", "recovery_accounting_mutations", "recovery_accounting_applied", "recovery_usage_observations":
			table.Exported = true
			out.Tables = append(out.Tables, table)
		}
	}
	if len(out.Tables) != 9 {
		return RecoverySnapshotInventory{}, ErrRecoverySnapshotSchema
	}
	return out, nil
}

// writeRecoveryLineageGenerationTx exports existing operator metadata as inert
// history, using the caller's RR snapshot, wire writer and cumulative budgets.
// It neither commits nor accepts a resumed source: that requires the future
// admission profile. It is a byte-preserving export, not semantic verification
// of generations or independent re-verification of past archive signatures.
func writeRecoveryLineageGenerationTx(ctx context.Context, tx pgx.Tx, schema string, w *recoverySnapshotWriter, limits RecoverySnapshotLimits, report *RecoverySnapshotReport) error {
	if tx == nil || w == nil || report == nil || !limits.valid() || report.Rows < 0 || report.Bytes < 0 || report.Rows > limits.MaxRows || report.Bytes > limits.MaxTotalBytes {
		return ErrRecoverySnapshotLimit
	}
	if recoverySemanticCatalogSHA256 != recoveryLineageSQL026Catalog {
		return ErrRecoverySnapshotSchema
	}
	var boundedSnapshot bool
	if err := tx.QueryRow(ctx, "SELECT current_setting('transaction_isolation')='repeatable read' AND current_setting('transaction_read_only')='on'").Scan(&boundedSnapshot); err != nil || !boundedSnapshot {
		return ErrRecoverySnapshotSource
	}
	if _, err := tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock_shared(841932079)"); err != nil {
		return ErrRecoverySnapshotSource
	}
	if _, err := tx.Exec(ctx, "SET LOCAL row_security=off"); err != nil {
		return ErrRecoverySnapshotSource
	}
	if _, err := validateRecoverySemanticCatalog(ctx, tx, schema); err != nil {
		return err
	}
	inventory, err := recoverySnapshotInventory(ctx, tx, schema, true)
	if err != nil {
		return err
	}
	projected, err := recoveryLineageSourceInventory(inventory)
	if err != nil {
		return err
	}
	held, err := readRecoveryState(ctx, schema, tx)
	if err != nil || held.State != "held" {
		return ErrRecoverySnapshotSource
	}
	receipt, err := readRecoveryImportReceipt(ctx, tx, schema)
	if err != nil || receipt.TargetInstallationID != held.InstallationID || receipt.RestoreID != held.RestoreID || receipt.SourceID != held.SourceID || receipt.BackupID != held.BackupID || receipt.CutUTC != held.CutUTC || receipt.SchemaSHA256 != held.SchemaSHA256 {
		return ErrRecoverySnapshotSource
	}
	// A full 1MiB evidence payload has additional row fields. This fixed,
	// bounded metadata profile does not change the business-row limit or the
	// shared total byte/row/wire budgets.
	metadataLimits := limits
	metadataLimits.MaxRowBytes = recoveryLineageMetadataRowBytes
	remaining := metadataLimits
	remaining.MaxRows -= report.Rows
	remaining.MaxTotalBytes -= report.Bytes
	counts, err := recoverySnapshotPreflight(ctx, tx, schema, projected, remaining)
	if err != nil {
		return err
	}
	start := recoveryLineageGenerationStart{Kind: "generation", Profile: "sql026-current-generation-v1", InstallationID: held.InstallationID, RestoreID: held.RestoreID, ImportedSourceID: receipt.SourceID, ImportedSnapshotSHA256: receipt.SnapshotSHA256, InventorySHA256: recoveryLineageSQL026Inventory, CatalogSHA256: recoveryLineageSQL026Catalog}
	if err = w.record(start); err != nil {
		return err
	}
	rows, bytes, tables := report.Rows, report.Bytes, len(report.Tables)
	if err = writeRecoverySnapshotTables(ctx, tx, schema, projected, counts, w, metadataLimits, report); err != nil {
		return err
	}
	return w.record(recoveryLineageGenerationEnd{Kind: "generation_end", Tables: len(report.Tables) - tables, Rows: report.Rows - rows, Bytes: report.Bytes - bytes})
}
