// SPDX-License-Identifier: Elastic-2.0
package storage

import "encoding/json"

type recoveryLineageProfile struct{ Name, InventorySHA256, CatalogSHA256 string }

const recoveryLineageAdmittedProfile = "sql031-admitted-generation-v1"

func recoveryLineageProfileByName(name string) (recoveryLineageProfile, error) {
	switch name {
	case "sql026-current-generation-v1":
		return recoveryLineageProfile{name, recoverySQL026Inventory, recoverySQL026Catalog}, nil
	case "sql027-current-generation-v1":
		return recoveryLineageProfile{name, recoverySQL027Inventory, recoverySQL027Catalog}, nil
	case "sql028-current-generation-v1":
		return recoveryLineageProfile{name, recoverySQL028Inventory, recoverySQL028Catalog}, nil
	case "sql029-current-generation-v1":
		return recoveryLineageProfile{name, recoverySQL028Inventory, recoverySQL029Catalog}, nil
	case "sql030-current-generation-v1":
		return recoveryLineageProfile{name, recoverySQL030Inventory, recoverySQL030Catalog}, nil
	case "sql031-current-generation-v1", recoveryLineageAdmittedProfile:
		return recoveryLineageProfile{name, recoverySQL031Inventory, recoverySQL031Catalog}, nil
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
	case recoverySQL028Catalog:
		return recoveryLineageProfileByName("sql028-current-generation-v1")
	case recoverySQL029Catalog:
		return recoveryLineageProfileByName("sql029-current-generation-v1")
	case recoverySQL030Catalog:
		return recoveryLineageProfileByName("sql030-current-generation-v1")
	case recoverySQL031Catalog:
		return recoveryLineageProfileByName("sql031-current-generation-v1")
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
		case "recovery_import_receipts", "recovery_historical_rows", "recovery_evidence_batches", "recovery_evidence_chunks", "recovery_evidence_changes", "recovery_accounting_batches", "recovery_accounting_mutations", "recovery_accounting_applied", "recovery_usage_observations", "recovery_finance_statements":
			table.Exported = true
			out.Tables = append(out.Tables, table)
		case "recovery_work_admissions":
			if name == recoveryLineageAdmittedProfile {
				table.Exported = true
				out.Tables = append(out.Tables, table)
			}
		}
	}
	expected := 9
	switch name {
	case recoveryLineageAdmittedProfile:
		expected += 2
	case "sql030-current-generation-v1", "sql031-current-generation-v1":
		expected++
	}
	if len(out.Tables) != expected {
		return RecoverySnapshotInventory{}, ErrRecoverySnapshotSchema
	}
	return out, nil
}
