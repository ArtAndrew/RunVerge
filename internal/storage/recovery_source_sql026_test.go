// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"encoding/json"
	"os"
	"testing"
)

func TestRecoverySQL026FrozenProfile(t *testing.T) {
	migrations := EmbeddedSchemaIdentity()
	if len(migrations) < 26 {
		t.Fatal("missing SQL026 migrations")
	}
	raw, err := json.Marshal(migrations[:26])
	if err != nil || Hash(raw) != recoverySQL026Schema {
		t.Fatal("frozen SQL026 schema changed", err)
	}
	frozen, err := os.ReadFile("testdata/recovery-sql026-inventory.json")
	if err != nil {
		t.Fatal(err)
	}
	var inventory RecoverySnapshotInventory
	if err = json.Unmarshal(frozen, &inventory); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(inventory)
	if err != nil || Hash(raw) != recoverySQL026Inventory {
		t.Fatal("frozen SQL026 inventory changed", err)
	}
	if recoveryLineageSQL026Inventory != recoverySQL026Inventory || recoveryLineageSQL026Catalog != recoverySQL026Catalog {
		t.Fatal("historical profiles disagree")
	}
	if !SupportedRecoverySourceSchema(recoverySQL026Schema) {
		t.Fatal("SQL026 source unsupported")
	}
	for _, name := range []string{"recovery_lineage_imports", "recovery_lineage_chunks"} {
		if recoveryBusinessTable(name) {
			t.Fatal("historical bytes entered business export", name)
		}
	}
	receipt := RecoveryImportReceipt{SchemaSHA256: recoverySQL026Schema, InventorySHA256: recoverySQL026Inventory, CatalogSHA256: recoverySQL026Catalog}
	if !recoveryImportProfileSupported(receipt) {
		t.Fatal("historical SQL026 receipt rejected")
	}
	receipt.CatalogSHA256 = recoverySQL025Catalog
	if recoveryImportProfileSupported(receipt) {
		t.Fatal("newer source attributed to older target")
	}
}
