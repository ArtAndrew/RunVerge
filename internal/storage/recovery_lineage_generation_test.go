// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

func TestRecoveryLineageGenerationLayout(t *testing.T) {
	inventory := independentRecoveryFixtureNew(t).header.Inventory
	projected, err := recoveryLineageSourceInventory(inventory)
	if err != nil {
		t.Fatal(err)
	}
	expected := recoveryLineageGenerationStart{Kind: "generation", Profile: "sql026-current-generation-v1", InstallationID: domain.NewID("inst"), RestoreID: domain.NewID("restore"), ImportedSourceID: domain.NewID("inst"), ImportedSnapshotSHA256: strings.Repeat("a", 64), InventorySHA256: recoveryLineageSQL026Inventory, CatalogSHA256: recoveryLineageSQL026Catalog}
	// Independent COPY grammar, no production exporter. Empty tables are valid
	// layout only; the future semantic importer must require an actual receipt.
	lines := [][]byte{independentRecoveryJSON(t, expected)}
	raw := independentRecoveryCopy()
	for _, table := range projected.Tables {
		lines = append(lines, independentRecoveryJSON(t, recoverySnapshotTableStart{Kind: "table", Name: table.Name}), independentRecoveryJSON(t, recoverySnapshotChunk{Kind: "chunk", Data: base64.StdEncoding.EncodeToString(raw)}), independentRecoveryJSON(t, recoverySnapshotTableEnd{Kind: "table_end", Summary: RecoverySnapshotTableSummary{Name: table.Name, Bytes: int64(len(raw)), SHA256: Hash(raw)}}))
	}
	lines = append(lines, independentRecoveryJSON(t, recoveryLineageGenerationEnd{Kind: "generation_end", Tables: 9, Bytes: 9 * int64(len(raw))}))
	input := bytes.Join(lines, nil)
	check := func(input []byte, want recoveryLineageGenerationStart, report *RecoverySnapshotReport) error {
		limits := DefaultRecoverySnapshotLimits()
		r := &recoverySnapshotReader{ctx: context.Background(), reader: bufio.NewReader(bytes.NewReader(input)), hash: sha256.New(), limit: limits.MaxWireBytes}
		if err := readRecoveryLineageGeneration(r, inventory, want, limits, report); err != nil {
			return err
		}
		if _, err := r.reader.Peek(1); err != io.EOF {
			t.Fatal("generation consumed incorrect boundary", err)
		}
		return nil
	}
	report := RecoverySnapshotReport{Rows: 7, Bytes: 31}
	if err := check(input, expected, &report); err != nil || report.Rows != 7 || report.Bytes != 31+9*int64(len(raw)) || len(report.Tables) != 9 {
		t.Fatal("layout or cumulative budget mismatch", err)
	}
	for name, bad := range map[string][]byte{
		"missing_end":   bytes.Join(lines[:len(lines)-1], nil),
		"missing_table": bytes.Join(append(append([][]byte{}, lines[:1]...), lines[4:]...), nil),
		"wrong_table":   bytes.Replace(input, []byte(projected.Tables[0].Name), []byte("access_tokens"), 1),
		"wrong_count":   bytes.Replace(input, []byte(`"tables":9`), []byte(`"tables":8`), 1),
		"unknown_field": bytes.Replace(input, []byte(`"kind":"generation"`), []byte(`"kind":"generation","authority":true`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if check(bad, expected, &RecoverySnapshotReport{}) == nil {
				t.Fatal("invalid layout accepted")
			}
		})
	}
	changed := expected
	changed.InstallationID = domain.NewID("inst")
	if check(input, changed, &RecoverySnapshotReport{}) == nil {
		t.Fatal("different generation accepted")
	}
	limits := DefaultRecoverySnapshotLimits()
	if check(input, expected, &RecoverySnapshotReport{Bytes: limits.MaxTotalBytes}) == nil {
		t.Fatal("cumulative budget bypass")
	}
}
