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

func SupportedRecoverySourceSchema(digest string) bool {
	return digest == EmbeddedSchemaSHA256() || digest == recoverySQL023Schema || digest == recoverySQL024Schema || digest == recoverySQL025Schema
}
func recoverySourceHeaderProfile(h recoverySnapshotHeader) bool {
	var migrations []installation.Migration
	var inventory string
	switch h.Metadata.SchemaSHA256 {
	case EmbeddedSchemaSHA256():
		migrations = EmbeddedSchemaIdentity()
		inventory = recoverySnapshotInventorySHA256
	case recoverySQL023Schema, recoverySQL024Schema, recoverySQL025Schema:
		all := EmbeddedSchemaIdentity()
		if len(all) != 26 {
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
	raw, _ := json.Marshal(target)
	if Hash(raw) != recoverySnapshotInventorySHA256 {
		return RecoverySnapshotInventory{}, ErrRecoveryStageSchema
	}
	if source == recoverySnapshotInventorySHA256 {
		return target, nil
	}
	if source != recoverySQL023Inventory && source != recoverySQL024Inventory && source != recoverySQL025Inventory {
		return RecoverySnapshotInventory{}, ErrRecoveryStageSchema
	}
	projected := target
	projected.Tables = nil
	for _, t := range target.Tables {
		switch t.Name {
		case "recovery_usage_observations":
			continue
		case "recovery_accounting_batches", "recovery_accounting_mutations", "recovery_accounting_applied":
			if source != recoverySQL025Inventory {
				continue
			}
			if t.Name == "recovery_accounting_batches" {
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
	if r.SchemaSHA256 == recoverySQL024Schema {
		return r.InventorySHA256 == recoverySQL024Inventory && (r.CatalogSHA256 == recoverySQL024Catalog || r.CatalogSHA256 == recoverySQL025Catalog || r.CatalogSHA256 == recoverySemanticCatalogSHA256)
	}
	if r.SchemaSHA256 == recoverySQL025Schema {
		return r.InventorySHA256 == recoverySQL025Inventory && (r.CatalogSHA256 == recoverySQL025Catalog || r.CatalogSHA256 == recoverySemanticCatalogSHA256)
	}
	return r.SchemaSHA256 == recoverySQL023Schema && r.InventorySHA256 == recoverySQL023Inventory && (r.CatalogSHA256 == recoverySQL023Catalog || r.CatalogSHA256 == recoverySQL024Catalog || r.CatalogSHA256 == recoverySQL025Catalog || r.CatalogSHA256 == recoverySemanticCatalogSHA256)
}
