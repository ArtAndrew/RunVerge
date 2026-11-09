// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
)

// Check the historical receipt's declared table inventory against its original
// source profile and the preserved marker counts. These checks cannot establish
// the original business bytes or financial projections without that snapshot.
func validateRecoveryLineageReceipt(ctx context.Context, tx pgx.Tx, schema string, inventory RecoverySnapshotInventory, generationProfile string, limits RecoverySnapshotLimits) error {
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
	if err = validateRecoveryLineageReceiptLayout(receipt, inventory, generationProfile, limits); err != nil {
		return err
	}
	return recoveryCheckMarkerCounts(ctx, tx, schema, receipt.Tables)
}

func validateRecoveryLineageReceiptLayout(receipt RecoveryImportReceipt, inventory RecoverySnapshotInventory, generationProfile string, limits RecoverySnapshotLimits) error {
	if !limits.valid() {
		return ErrRecoverySnapshotLimit
	}
	generation, err := recoveryLineageProfileByName(generationProfile)
	if err != nil {
		return ErrRecoveryStageData
	}
	generationVersion := 26
	switch generation.CatalogSHA256 {
	case recoverySQL027Catalog:
		generationVersion = 27
	case recoverySQL028Catalog:
		generationVersion = 28
	case recoverySQL029Catalog:
		generationVersion = 29
	case recoverySQL030Catalog:
		generationVersion = 30
	case recoverySQL031Catalog:
		generationVersion = 31
	}
	var expectedInventory string
	var sourceVersion int
	switch receipt.SchemaSHA256 {
	case recoverySQL031Schema:
		expectedInventory, sourceVersion = recoverySQL031Inventory, 31
	case recoverySQL030Schema:
		expectedInventory, sourceVersion = recoverySQL030Inventory, 30
	case recoverySQL029Schema:
		expectedInventory, sourceVersion = recoverySQL028Inventory, 29
	case recoverySQL028Schema:
		expectedInventory, sourceVersion = recoverySQL028Inventory, 28
	case recoverySQL027Schema:
		expectedInventory, sourceVersion = recoverySQL027Inventory, 27
	case recoverySQL026Schema:
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
	case recoverySQL031Catalog:
		targetVersion = 31
	case recoverySQL030Catalog:
		targetVersion = 30
	case recoverySQL029Catalog:
		targetVersion = 29
	case recoverySQL028Catalog:
		targetVersion = 28
	case recoverySQL027Catalog:
		targetVersion = 27
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
	if targetVersion < sourceVersion || targetVersion > generationVersion {
		return ErrRecoveryStageData
	}
	raw, err := json.Marshal(inventory)
	if err != nil {
		return ErrRecoveryStageData
	}
	source, err := recoverySourceInventoryProfile(inventory, expectedInventory, Hash(raw))
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
