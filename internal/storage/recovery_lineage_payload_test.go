// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/domain"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestRecoveryLineagePayloadFlatIndex(t *testing.T) {
	rawInventory, err := os.ReadFile("testdata/recovery-sql027-inventory.json")
	if err != nil {
		t.Fatal(err)
	}
	var inventory RecoverySnapshotInventory
	if err = json.Unmarshal(rawInventory, &inventory); err != nil {
		t.Fatal(err)
	}
	first := recoveryLineageGenerationStart{Kind: "generation", Profile: "sql026-current-generation-v1", InstallationID: domain.NewID("inst"), RestoreID: domain.NewID("restore"), ImportedSourceID: domain.NewID("inst"), ImportedSnapshotSHA256: strings.Repeat("a", 64), InventorySHA256: recoveryLineageSQL026Inventory, CatalogSHA256: recoveryLineageSQL026Catalog}
	second := first
	second.ImportedSourceID = first.InstallationID
	second.InstallationID = domain.NewID("inst")
	second.RestoreID = domain.NewID("restore")
	second.Profile = "sql027-current-generation-v1"
	second.InventorySHA256 = recoverySQL027Inventory
	second.CatalogSHA256 = recoverySQL027Catalog
	makeGeneration := func(identity recoveryLineageGenerationStart) []byte {
		projected, err := recoveryLineageInventoryProfile(inventory, identity.Profile)
		if err != nil {
			t.Fatal(err)
		}
		var result bytes.Buffer
		result.Write(independentRecoveryJSON(t, identity))
		var size int64
		// Null fields intentionally exercise framing only. Typed staging must
		// separately refuse these rows; indexing never confers semantic validity.
		for _, table := range projected.Tables {
			raw := independentRecoveryCopy(make([][]byte, len(table.Columns)))
			size += int64(len(raw))
			result.Write(independentRecoveryJSON(t, recoverySnapshotTableStart{Kind: "table", Name: table.Name}))
			result.Write(independentRecoveryJSON(t, recoverySnapshotChunk{Kind: "chunk", Data: base64.StdEncoding.EncodeToString(raw)}))
			result.Write(independentRecoveryJSON(t, recoverySnapshotTableEnd{Kind: "table_end", Summary: RecoverySnapshotTableSummary{Name: table.Name, Rows: 1, Bytes: int64(len(raw)), SHA256: Hash(raw)}}))
		}
		result.Write(independentRecoveryJSON(t, recoveryLineageGenerationEnd{Kind: "generation_end", Tables: 9, Rows: 9, Bytes: size}))
		return result.Bytes()
	}
	a, b := makeGeneration(first), makeGeneration(second)
	payload := append(append([]byte{}, a...), b...)
	declared := RecoveryLineageFramingReport{Generations: 2, Records: 18, PayloadBytes: int64(len(payload))}
	limits := DefaultRecoverySnapshotLimits()
	spans, err := indexRecoveryLineagePayload(context.Background(), bytes.NewReader(payload), second.InstallationID, declared, inventory, limits)
	if err != nil || len(spans) != 2 || spans[0].Offset != 0 || spans[0].Length != int64(len(a)) || spans[1].Offset != int64(len(a)) || spans[1].Length != int64(len(b)) {
		t.Fatal("flat spans incorrect", err)
	}
	for name, change := range map[string]func(*RecoveryLineageFramingReport){"missing_generation": func(d *RecoveryLineageFramingReport) { d.Generations = 1 }, "extra_generation": func(d *RecoveryLineageFramingReport) { d.Generations = 3 }, "wrong_records": func(d *RecoveryLineageFramingReport) { d.Records++ }, "wrong_bytes": func(d *RecoveryLineageFramingReport) { d.PayloadBytes++ }} {
		t.Run(name, func(t *testing.T) {
			bad := declared
			change(&bad)
			if _, err := indexRecoveryLineagePayload(context.Background(), bytes.NewReader(payload), second.InstallationID, bad, inventory, limits); err == nil {
				t.Fatal("false declaration accepted")
			}
		})
	}
	limited := limits
	limited.MaxTotalBytes = declared.PayloadBytes - 1
	limited.MaxTableBytes = min(limited.MaxTableBytes, limited.MaxTotalBytes)
	limited.MaxRowBytes = min(limited.MaxRowBytes, limited.MaxTableBytes)
	if !limited.valid() {
		t.Fatal("invalid test limits")
	}
	if _, err := indexRecoveryLineagePayload(context.Background(), bytes.NewReader(payload), second.InstallationID, declared, inventory, limited); err == nil {
		t.Fatal("encoded payload exceeded total limit")
	}
	if _, err := indexRecoveryLineagePayload(context.Background(), bytes.NewReader(append(payload, '\n')), second.InstallationID, declared, inventory, limits); err == nil {
		t.Fatal("trailing bytes accepted")
	}
	if _, err := indexRecoveryLineagePayload(context.Background(), bytes.NewReader(payload), first.InstallationID, declared, inventory, limits); err == nil {
		t.Fatal("wrong outer source accepted")
	}
}
