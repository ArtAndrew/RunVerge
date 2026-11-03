// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/installation"
	"encoding/json"
	"reflect"
)

// Explicit source profiles. This is not an arbitrary migration-prefix policy.
const recoverySQL023Schema = "30a0619f08b777b814e1cb7146f7eb0f041139ddf893681a28fbe6a4209e10a9"
const recoverySQL023Inventory = "f6df86ff20c447e67132a5282b46a20aa2ccadecdd994767205ffb30c3362700"
const recoverySQL023Catalog = "35d765a3d47627666c1ac35997e149fa7ea45f712db6ba143b69b6075a94a4a3"
const recoverySQL024Schema = "19848269eace310288c8ed551091c11fb43c11c5a5600a56fc5c0efd3743c4b5"
const recoverySQL024Inventory = "f8627786eb0a42461640d880662df8b53c07769a4bd3aecf441b5c3c3cc1dd6e"
const recoverySQL024Catalog = "04ca663b80bdcbe366e214906cca288984fc78cf0cb0f00c9a1ac96e4233d015"
const recoverySQL025Schema = "8475c3b0d65447872fa81b30ea2739326467a5a428c224b8bba8855a528bf7e2"
const recoverySQL025Inventory = "a2544fad609b1d6ff15f97400a97a05b5c05e1b7e4944e799aa0d9135df45454"
const recoverySQL025Catalog = "70ab49a57a49bf1f8f40de0e65b063626fe734323829f9945c1d996b07cb5b98"

// Frozen SQL026 identities, retained when the current target advances.
const recoverySQL026Schema = "bfbc41facbe90a48c1feb3cfc1d35f10e866f52519c51bb1ff45a32b2d66fa9d"
const recoverySQL026Inventory = "2c9d16d3cdead2c3dc0c1baa34b0f7ea646a87bada95202d5fa1999d861aea24"
const recoverySQL026Catalog = "873463718e42a33e7df8ba9eff916260ca46370d49affafc609423338674a9b8"

// SQL027 identities captured from trusted DDL in a disposable DB.
// The embedded migration and current inventory/catalog pins select this profile.
const recoverySQL027Schema = "afe9fdcd108923f4013013f88aee9c59ee96aaf1422c61ca88c18a5428d048fc"
const recoverySQL027Inventory = "7d3e1b741ecf3451d737251ff8afd90bcb603ac098f633056d84a9536c58382a"
const recoverySQL027Catalog = "157f3653023ce2999a0f76d0c3c40d3299f8bf89845efe77c7b27896ff431af7"

func SupportedRecoverySourceSchema(digest string) bool {
	return digest == EmbeddedSchemaSHA256() || digest == recoverySQL023Schema || digest == recoverySQL024Schema || digest == recoverySQL025Schema || digest == recoverySQL026Schema
}
func recoverySourceHeaderProfile(h recoverySnapshotHeader) bool {
	var migrations []installation.Migration
	var inventory string
	switch h.Metadata.SchemaSHA256 {
	case EmbeddedSchemaSHA256():
		migrations = EmbeddedSchemaIdentity()
		inventory = recoverySnapshotInventorySHA256
	case recoverySQL023Schema, recoverySQL024Schema, recoverySQL025Schema, recoverySQL026Schema:
		all := EmbeddedSchemaIdentity()
		if len(all) != 26 && len(all) != 27 {
			return false
		}
		migrations = all[:23]
		inventory = recoverySQL023Inventory
		if h.Metadata.SchemaSHA256 == recoverySQL024Schema {
			migrations, inventory = all[:24], recoverySQL024Inventory
		}
		if h.Metadata.SchemaSHA256 == recoverySQL025Schema {
			migrations, inventory = all[:25], recoverySQL025Inventory
		}
		if h.Metadata.SchemaSHA256 == recoverySQL026Schema {
			migrations, inventory = all[:26], recoverySQL026Inventory
		}
	default:
		return false
	}
	encoded, _ := json.Marshal(migrations)
	raw, _ := json.Marshal(h.Inventory)
	return Hash(encoded) == h.Metadata.SchemaSHA256 && reflect.DeepEqual(h.Migrations, migrations) && h.InventorySHA256 == inventory && Hash(raw) == inventory
}

// The complete target catalog is checked before projection. SQL024/025 add
// operator metadata; business binary layouts must match each exact source pin.
func recoverySourceInventory(target RecoverySnapshotInventory, source string) (RecoverySnapshotInventory, error) {
	return recoverySourceInventoryProfile(target, source, recoverySnapshotInventorySHA256)
}

// Only explicit reviewed target profiles may supply historical SQL layouts.
// Runtime callers still enter through the compiled-current wrapper above.
func recoverySourceInventoryProfile(target RecoverySnapshotInventory, source, targetProfile string) (RecoverySnapshotInventory, error) {
	if targetProfile != recoverySQL026Inventory && targetProfile != recoverySQL027Inventory {
		return RecoverySnapshotInventory{}, ErrRecoveryStageSchema
	}
	raw, _ := json.Marshal(target)
	if Hash(raw) != targetProfile {
		return RecoverySnapshotInventory{}, ErrRecoveryStageSchema
	}
	if source == targetProfile {
		return target, nil
	}
	if source != recoverySQL023Inventory && source != recoverySQL024Inventory && source != recoverySQL025Inventory && source != recoverySQL026Inventory {
		return RecoverySnapshotInventory{}, ErrRecoveryStageSchema
	}
	projected := target
	projected.Tables = nil
	for _, t := range target.Tables {
		switch t.Name {
		case "recovery_lineage_imports", "recovery_lineage_chunks":
			continue
		case "recovery_import_receipts":
			columns := make([]RecoverySnapshotColumn, 0, len(t.Columns))
			for _, c := range t.Columns {
				if c.Name != "creating_transaction" {
					columns = append(columns, c)
				}
			}
			t.Columns = columns
		case "recovery_usage_observations":
			if source != recoverySQL026Inventory {
				continue
			}
		case "recovery_accounting_batches", "recovery_accounting_mutations", "recovery_accounting_applied":
			if source != recoverySQL025Inventory && source != recoverySQL026Inventory {
				continue
			}
			if t.Name == "recovery_accounting_batches" && source != recoverySQL026Inventory {
				columns := make([]RecoverySnapshotColumn, 0, len(t.Columns))
				for _, c := range t.Columns {
					if c.Name != "mutation_profile" {
						columns = append(columns, c)
					}
				}
				t.Columns = columns
			}
		case "recovery_evidence_batches", "recovery_evidence_chunks", "recovery_evidence_changes":
			if source == recoverySQL023Inventory {
				continue
			}
		}
		projected.Tables = append(projected.Tables, t)
	}
	raw, _ = json.Marshal(projected)
	if Hash(raw) != source {
		return RecoverySnapshotInventory{}, ErrRecoveryStageSchema
	}
	return projected, nil
}
func recoveryImportProfileSupported(r RecoveryImportReceipt) bool {
	if r.SchemaSHA256 == EmbeddedSchemaSHA256() {
		return r.InventorySHA256 == recoverySnapshotInventorySHA256 && r.CatalogSHA256 == recoverySemanticCatalogSHA256
	}
	if r.SchemaSHA256 == recoverySQL026Schema {
		return r.InventorySHA256 == recoverySQL026Inventory && (r.CatalogSHA256 == recoverySQL026Catalog || r.CatalogSHA256 == recoverySemanticCatalogSHA256)
	}
	if r.SchemaSHA256 == recoverySQL024Schema {
		return r.InventorySHA256 == recoverySQL024Inventory && (r.CatalogSHA256 == recoverySQL024Catalog || r.CatalogSHA256 == recoverySQL025Catalog || r.CatalogSHA256 == recoverySQL026Catalog || r.CatalogSHA256 == recoverySemanticCatalogSHA256)
	}
	if r.SchemaSHA256 == recoverySQL025Schema {
		return r.InventorySHA256 == recoverySQL025Inventory && (r.CatalogSHA256 == recoverySQL025Catalog || r.CatalogSHA256 == recoverySQL026Catalog || r.CatalogSHA256 == recoverySemanticCatalogSHA256)
	}
	return r.SchemaSHA256 == recoverySQL023Schema && r.InventorySHA256 == recoverySQL023Inventory && (r.CatalogSHA256 == recoverySQL023Catalog || r.CatalogSHA256 == recoverySQL024Catalog || r.CatalogSHA256 == recoverySQL025Catalog || r.CatalogSHA256 == recoverySQL026Catalog || r.CatalogSHA256 == recoverySemanticCatalogSHA256)
}
