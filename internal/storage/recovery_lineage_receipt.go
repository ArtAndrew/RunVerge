// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"github.com/jackc/pgx/v5"
)

// Check the historical receipt's declared table inventory against its original
// source profile and the preserved marker counts. These checks cannot establish
// the original business bytes or financial projections without that snapshot.
func validateRecoveryLineageReceipt(ctx context.Context, tx pgx.Tx, schema string, inventory RecoverySnapshotInventory, limits RecoverySnapshotLimits) error {
	if tx == nil || !limits.valid() {
		return ErrRecoveryStageData
	}
	var owned bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_namespace WHERE oid=pg_catalog.pg_my_temp_schema() AND nspname=$1)", schema).Scan(&owned); err != nil || !owned {
		return ErrRecoveryStageTarget
	}
	receipt, err := readRecoveryImportReceipt(ctx, tx, schema)
	if err != nil {
		return ErrRecoveryStageData
	}
	if err = validateRecoveryLineageReceiptLayout(receipt, inventory, limits); err != nil {
		return err
	}
	return recoveryCheckMarkerCounts(ctx, tx, schema, receipt.Tables)
}

func validateRecoveryLineageReceiptLayout(receipt RecoveryImportReceipt, inventory RecoverySnapshotInventory, limits RecoverySnapshotLimits) error {
	if !limits.valid() {
		return ErrRecoverySnapshotLimit
	}
	var expectedInventory string
	var sourceVersion int
	switch receipt.SchemaSHA256 {
	case EmbeddedSchemaSHA256():
		expectedInventory, sourceVersion = recoveryLineageSQL026Inventory, 26
	case recoverySQL023Schema:
		expectedInventory, sourceVersion = recoverySQL023Inventory, 23
	case recoverySQL024Schema:
		expectedInventory, sourceVersion = recoverySQL024Inventory, 24
	case recoverySQL025Schema:
		expectedInventory, sourceVersion = recoverySQL025Inventory, 25
	default:
		return ErrRecoveryStageData
	}
	if receipt.InventorySHA256 != expectedInventory {
		return ErrRecoveryStageData
	}
	targetVersion := 0
	switch receipt.CatalogSHA256 {
	case recoverySQL023Catalog:
		targetVersion = 23
	case recoverySQL024Catalog:
		targetVersion = 24
	case recoverySQL025Catalog:
		targetVersion = 25
	case recoveryLineageSQL026Catalog:
		targetVersion = 26
	default:
		return ErrRecoveryStageData
	}
	if targetVersion < sourceVersion {
		return ErrRecoveryStageData
	}
	source, err := recoverySourceInventory(inventory, expectedInventory)
	if err != nil {
		return err
	}
	var rows, size int64
	index := 0
	for _, table := range source.Tables {
		if !table.Exported {
			continue
		}
		if index >= len(receipt.Tables) {
			return ErrRecoveryStageData
		}
		summary := receipt.Tables[index]
		if summary.Name != table.Name || summary.Rows < 0 || summary.Rows > limits.MaxRowsPerTable || summary.Rows > limits.MaxRows-rows || summary.Bytes < 21 || summary.Bytes > limits.MaxTableBytes || summary.Bytes > limits.MaxTotalBytes-size || !validDigest(summary.SHA256) {
			return ErrRecoveryStageData
		}
		rows += summary.Rows
		size += summary.Bytes
		index++
	}
	if index != len(receipt.Tables) || len(receipt.Sequences) != len(source.Sequences) {
		return ErrRecoveryStageData
	}
	for i, seq := range receipt.Sequences {
		if seq.Name != source.Sequences[i].Name || seq.ObservedLast < 1 || seq.IncludedMaximum < 0 || seq.NextMinimum <= seq.ObservedLast || seq.NextMinimum <= seq.IncludedMaximum || seq.NextMinimum != max(seq.ObservedLast, seq.IncludedMaximum)+1 {
			return ErrRecoveryStageData
		}
	}
	return nil
}
