// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestRecoveryLineageVersionedLayouts(t *testing.T) {
	if EmbeddedSchemaSHA256() != recoverySQL031Schema {
		t.Fatal("embedded SQL031 identity differs from reviewed reference")
	}

	read := func(path string) RecoverySnapshotInventory {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var inventory RecoverySnapshotInventory
		if err = json.Unmarshal(raw, &inventory); err != nil {
			t.Fatal(err)
		}
		return inventory
	}
	old, current := read("testdata/recovery-sql026-inventory.json"), read("testdata/recovery-sql027-inventory.json")
	original, err := recoveryLineageInventoryProfile(old, "sql026-current-generation-v1")
	if err != nil {
		t.Fatal(err)
	}
	historical, err := recoveryLineageInventoryProfile(current, "sql026-current-generation-v1")
	if err != nil || !reflect.DeepEqual(original, historical) {
		t.Fatal("old layout changed on new target", err)
	}
	modern, err := recoveryLineageInventoryProfile(current, "sql027-current-generation-v1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, table := range modern.Tables {
		for _, column := range table.Columns {
			if table.Name == "recovery_import_receipts" && column.Name == "creating_transaction" {
				found = true
				if column.TypeOID != 5069 || column.NotNull {
					t.Fatal("new receipt field changed")
				}
			}
		}
	}
	if !found || reflect.DeepEqual(modern, historical) {
		t.Fatal("new receipt field lost")
	}
	if _, err = recoveryLineageInventoryProfile(old, "sql027-current-generation-v1"); err == nil {
		t.Fatal("old target fabricated new generation")
	}
	if _, err = recoveryLineageInventoryProfile(current, "unknown"); err == nil {
		t.Fatal("unreviewed generation profile accepted")
	}
	upgraded := read("testdata/recovery-sql028-inventory.json")
	for _, layout := range []struct {
		name string
		want RecoverySnapshotInventory
	}{{"sql026-current-generation-v1", original}, {"sql027-current-generation-v1", modern}, {"sql028-current-generation-v1", modern}} {
		got, err := recoveryLineageInventoryProfile(upgraded, layout.name)
		if err != nil || !reflect.DeepEqual(got, layout.want) {
			t.Fatal("new local admission metadata changed the lineage layout", layout.name, err)
		}
	}
}

// A generation may retain a receipt from an earlier target, but cannot claim
// that the receipt was produced by a target newer than the generation itself.
func TestRecoveryLineageReceiptGenerationBoundary(t *testing.T) {
	raw, err := os.ReadFile("testdata/recovery-sql031-inventory.json")
	if err != nil {
		t.Fatal(err)
	}
	var inventory RecoverySnapshotInventory
	if err = json.Unmarshal(raw, &inventory); err != nil {
		t.Fatal(err)
	}
	for _, sourceProfile := range []struct {
		schema, inventory string
		version           int
	}{
		{recoverySQL026Schema, recoverySQL026Inventory, 26},
		{recoverySQL027Schema, recoverySQL027Inventory, 27},
		{recoverySQL028Schema, recoverySQL028Inventory, 28},
		{recoverySQL029Schema, recoverySQL028Inventory, 29},
		{recoverySQL030Schema, recoverySQL030Inventory, 30},
		{recoverySQL031Schema, recoverySQL031Inventory, 31},
	} {
		source, err := recoverySourceInventoryProfile(inventory, sourceProfile.inventory, recoverySQL031Inventory)
		if err != nil {
			t.Fatal(err)
		}
		receipt := RecoveryImportReceipt{SchemaSHA256: sourceProfile.schema, InventorySHA256: sourceProfile.inventory}
		for _, table := range source.Tables {
			if table.Exported {
				receipt.Tables = append(receipt.Tables, RecoverySnapshotTableSummary{Name: table.Name, Bytes: 21, SHA256: Hash(independentRecoveryCopy())})
			}
		}
		for _, seq := range source.Sequences {
			receipt.Sequences = append(receipt.Sequences, RecoverySnapshotSequence{Name: seq.Name, ObservedLast: 1, NextMinimum: 2})
		}
		for _, target := range []struct {
			catalog string
			version int
		}{{recoverySQL026Catalog, 26}, {recoverySQL027Catalog, 27}, {recoverySQL028Catalog, 28}, {recoverySQL029Catalog, 29}, {recoverySQL030Catalog, 30}, {recoverySQL031Catalog, 31}} {
			receipt.CatalogSHA256 = target.catalog
			for _, generation := range []struct {
				name    string
				version int
			}{{"sql026-current-generation-v1", 26}, {"sql027-current-generation-v1", 27}, {"sql028-current-generation-v1", 28}, {"sql029-current-generation-v1", 29}, {"sql030-current-generation-v1", 30}, {"sql031-current-generation-v1", 31}, {recoveryLineageAdmittedProfile, 31}, {"unknown", 0}} {
				accepted := validateRecoveryLineageReceiptLayout(receipt, inventory, generation.name, DefaultRecoverySnapshotLimits()) == nil
				want := target.version >= sourceProfile.version && target.version <= generation.version
				if accepted != want {
					t.Fatalf("source %d target %d generation %s: accepted=%v want=%v", sourceProfile.version, target.version, generation.name, accepted, want)
				}
			}
		}
	}
}

func TestRecoveryImportReceiptTimezoneCommitment(t *testing.T) {
	utc := time.Date(2026, 11, 9, 11, 2, 3, 123456000, time.UTC)
	stored := RecoveryImportReceipt{ImportedAt: utc.In(time.FixedZone("old operator", 5*3600+45*60))}
	current := stored
	current.ImportedAt = utc.In(time.FixedZone("new operator", -6*3600))
	if recoveryImportReceiptCommitment(stored) != recoveryImportReceiptCommitment(current) {
		t.Fatal("new receipt commitment differs by timezone")
	}
	stamp, ok := recoveryImportReceiptProofTime(current, digestValue(stored))
	if !ok || !stamp.Equal(utc) || stamp.Format(time.RFC3339Nano) != stored.ImportedAt.Format(time.RFC3339Nano) {
		t.Fatal("old commitment could not be verified on a different host")
	}
	current.TargetInstallationID = "different"
	if recoveryImportReceiptMatches(current, digestValue(stored)) {
		t.Fatal("timezone compatibility accepted a changed receipt")
	}
}

func TestRecoveryLineageAccountingCatalogCompatibility(t *testing.T) {
	for _, stored := range []string{recoverySQL026Catalog, recoverySQL027Catalog, "unknown"} {
		for _, current := range []string{recoverySQL026Catalog, recoverySQL027Catalog, "unknown"} {
			want := stored == recoverySQL026Catalog && (current == recoverySQL026Catalog || current == recoverySQL027Catalog) || stored == recoverySQL027Catalog && current == recoverySQL027Catalog
			if recoveryUsageHistoricalCatalog(stored, current) != want {
				t.Fatal("unexpected historical catalog compatibility", stored, current)
			}
		}
	}
	for _, kind := range []string{"fact", "resolution"} {
		algorithm, catalog, ok := recoveryAccountingCommitmentProfile(recoveryAccountingMutationV2, kind)
		if !ok || catalog != recoverySQL026Catalog || algorithm != "source-financial-"+kind+"-v2" {
			t.Fatal("V2 historical commitment changed")
		}
	}
}
