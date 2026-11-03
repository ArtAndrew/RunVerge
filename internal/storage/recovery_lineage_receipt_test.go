// SPDX-License-Identifier: Elastic-2.0
package storage

import "testing"

func TestRecoveryLineageReceiptProfiles(t *testing.T) {
	inventory := independentRecoveryFixtureNew(t).header.Inventory
	limits := DefaultRecoverySnapshotLimits()
	for _, profile := range []struct {
		schema, inventory string
		version           int
	}{
		{recoverySQL026Schema, recoveryLineageSQL026Inventory, 26},
		{recoverySQL023Schema, recoverySQL023Inventory, 23},
		{recoverySQL024Schema, recoverySQL024Inventory, 24},
		{recoverySQL025Schema, recoverySQL025Inventory, 25},
	} {
		t.Run(profile.schema[:8], func(t *testing.T) {
			source, err := recoverySourceInventory(inventory, profile.inventory)
			if err != nil {
				t.Fatal(err)
			}
			receipt := RecoveryImportReceipt{SchemaSHA256: profile.schema, InventorySHA256: profile.inventory, CatalogSHA256: recoveryLineageSQL026Catalog}
			for _, table := range source.Tables {
				if table.Exported {
					receipt.Tables = append(receipt.Tables, RecoverySnapshotTableSummary{Name: table.Name, Bytes: 21, SHA256: Hash(independentRecoveryCopy())})
				}
			}
			for _, seq := range source.Sequences {
				receipt.Sequences = append(receipt.Sequences, RecoverySnapshotSequence{Name: seq.Name, ObservedLast: 1, NextMinimum: 2})
			}
			if err = validateRecoveryLineageReceiptLayout(receipt, inventory, "sql026-current-generation-v1", limits); err != nil {
				t.Fatal("supported profile rejected", err)
			}
			for _, target := range []struct {
				catalog string
				version int
			}{{recoverySQL023Catalog, 23}, {recoverySQL024Catalog, 24}, {recoverySQL025Catalog, 25}, {recoveryLineageSQL026Catalog, 26}} {
				candidate := receipt
				candidate.CatalogSHA256 = target.catalog
				accepted := validateRecoveryLineageReceiptLayout(candidate, inventory, "sql026-current-generation-v1", limits) == nil
				if accepted != (target.version >= profile.version) {
					t.Fatalf("source%d target%d acceptance=%v", profile.version, target.version, accepted)
				}
			}

			for name, mutate := range map[string]func(*RecoveryImportReceipt){
				"missing_table":   func(r *RecoveryImportReceipt) { r.Tables = r.Tables[:len(r.Tables)-1] },
				"duplicate_table": func(r *RecoveryImportReceipt) { r.Tables[1] = r.Tables[0] },
				"negative_rows":   func(r *RecoveryImportReceipt) { r.Tables[0].Rows = -1 },
				"wrong_inventory": func(r *RecoveryImportReceipt) { r.InventorySHA256 = "bad" },
				"wrong_catalog":   func(r *RecoveryImportReceipt) { r.CatalogSHA256 = "bad" },
				"bad_sequence":    func(r *RecoveryImportReceipt) { r.Sequences[0].NextMinimum = 1 },
			} {
				t.Run(name, func(t *testing.T) {
					bad := receipt
					bad.Tables = append([]RecoverySnapshotTableSummary{}, receipt.Tables...)
					bad.Sequences = append([]RecoverySnapshotSequence{}, receipt.Sequences...)
					mutate(&bad)
					if validateRecoveryLineageReceiptLayout(bad, inventory, "sql026-current-generation-v1", limits) == nil {
						t.Fatal("bad receipt accepted")
					}
				})
			}
		})
	}
}
