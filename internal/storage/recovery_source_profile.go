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

func SupportedRecoverySourceSchema(digest string) bool {
	return digest == EmbeddedSchemaSHA256() || digest == recoverySQL023Schema
}
func recoverySourceHeaderProfile(h recoverySnapshotHeader) bool {
	var migrations []installation.Migration
	var inventory string
	switch h.Metadata.SchemaSHA256 {
	case EmbeddedSchemaSHA256():
		migrations = EmbeddedSchemaIdentity()
		inventory = recoverySnapshotInventorySHA256
	case recoverySQL023Schema:
		all := EmbeddedSchemaIdentity()
		if len(all) != 24 {
			return false
		}
		migrations = all[:23]
		inventory = recoverySQL023Inventory
	default:
		return false
	}
	encoded, _ := json.Marshal(migrations)
	raw, _ := json.Marshal(h.Inventory)
	return Hash(encoded) == h.Metadata.SchemaSHA256 && reflect.DeepEqual(h.Migrations, migrations) && h.InventorySHA256 == inventory && Hash(raw) == inventory
}

// The complete target catalog is checked before using this projection. SQL024
// adds only operator metadata; every business table and binary layout must
// still exactly match the pinned source inventory.
func recoverySourceInventory(target RecoverySnapshotInventory, source string) (RecoverySnapshotInventory, error) {
	raw, _ := json.Marshal(target)
	if Hash(raw) != recoverySnapshotInventorySHA256 {
		return RecoverySnapshotInventory{}, ErrRecoveryStageSchema
	}
	if source == recoverySnapshotInventorySHA256 {
		return target, nil
	}
	if source != recoverySQL023Inventory {
		return RecoverySnapshotInventory{}, ErrRecoveryStageSchema
	}
	projected := target
	projected.Tables = nil
	for _, t := range target.Tables {
		switch t.Name {
		case "recovery_evidence_batches", "recovery_evidence_chunks", "recovery_evidence_changes":
			continue
		}
		projected.Tables = append(projected.Tables, t)
	}
	raw, _ = json.Marshal(projected)
	if Hash(raw) != recoverySQL023Inventory {
		return RecoverySnapshotInventory{}, ErrRecoveryStageSchema
	}
	return projected, nil
}
func recoveryImportProfileSupported(r RecoveryImportReceipt) bool {
	if r.SchemaSHA256 == EmbeddedSchemaSHA256() {
		return r.InventorySHA256 == recoverySnapshotInventorySHA256 && r.CatalogSHA256 == recoverySemanticCatalogSHA256
	}
	return r.SchemaSHA256 == recoverySQL023Schema && r.InventorySHA256 == recoverySQL023Inventory && (r.CatalogSHA256 == recoverySQL023Catalog || r.CatalogSHA256 == recoverySemanticCatalogSHA256)
}
