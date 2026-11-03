// SPDX-License-Identifier: Elastic-2.0
package storage

import "encoding/json"

type recoveryLineageProfile struct{ Name, InventorySHA256, CatalogSHA256 string }

func recoveryLineageProfileByName(name string) (recoveryLineageProfile, error) {
	switch name {
	case "sql026-current-generation-v1":
		return recoveryLineageProfile{name, recoverySQL026Inventory, recoverySQL026Catalog}, nil
	case "sql027-current-generation-v1":
		return recoveryLineageProfile{name, recoverySQL027Inventory, recoverySQL027Catalog}, nil
	default:
		return recoveryLineageProfile{}, ErrRecoverySnapshotSchema
	}
}
func recoveryLineageCurrentProfile() (recoveryLineageProfile, error) {
	switch recoverySemanticCatalogSHA256 {
	case recoverySQL026Catalog:
		return recoveryLineageProfileByName("sql026-current-generation-v1")
	case recoverySQL027Catalog:
		return recoveryLineageProfileByName("sql027-current-generation-v1")
	default:
		return recoveryLineageProfile{}, ErrRecoverySnapshotSchema
	}
}

// Each generation retains its own exact layout. Projection to an older profile
// is for reading old bytes, never for exporting a new generation as an old one.
func recoveryLineageInventoryProfile(inventory RecoverySnapshotInventory, name string) (RecoverySnapshotInventory, error) {
	profile, err := recoveryLineageProfileByName(name)
	if err != nil {
		return RecoverySnapshotInventory{}, err
	}
	raw, err := json.Marshal(inventory)
	if err != nil {
		return RecoverySnapshotInventory{}, err
	}
	historical, err := recoverySourceInventoryProfile(inventory, profile.InventorySHA256, Hash(raw))
	if err != nil {
		return RecoverySnapshotInventory{}, err
	}
	out := RecoverySnapshotInventory{Tables: []RecoverySnapshotTable{}, Sequences: []RecoverySnapshotSequenceDefinition{}}
	for _, table := range historical.Tables {
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
