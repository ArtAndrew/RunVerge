// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/domain"
	"strings"
	"testing"
)

func TestRecoveryLineageChainIdentity(t *testing.T) {
	first := recoveryLineageGenerationStart{Kind: "generation", Profile: "sql026-current-generation-v1", InstallationID: domain.NewID("inst"), RestoreID: domain.NewID("restore"), ImportedSourceID: domain.NewID("inst"), ImportedSnapshotSHA256: strings.Repeat("a", 64), InventorySHA256: recoveryLineageSQL026Inventory, CatalogSHA256: recoveryLineageSQL026Catalog}
	second := first
	second.ImportedSourceID = first.InstallationID
	second.InstallationID = domain.NewID("inst")
	second.RestoreID = domain.NewID("restore")
	chain := []recoveryLineageGenerationStart{first, second}
	if validateRecoveryLineageChain(chain, second.InstallationID) != nil || validateRecoveryLineageChain(chain[:1], first.InstallationID) != nil {
		t.Fatal("valid ancestry rejected")
	}
	for name, mutate := range map[string]func([]recoveryLineageGenerationStart){
		"missing_parent":         func(g []recoveryLineageGenerationStart) { g[1].ImportedSourceID = domain.NewID("inst") },
		"cycle_to_origin":        func(g []recoveryLineageGenerationStart) { g[1].InstallationID = g[0].ImportedSourceID },
		"duplicate_restore":      func(g []recoveryLineageGenerationStart) { g[1].RestoreID = g[0].RestoreID },
		"duplicate_installation": func(g []recoveryLineageGenerationStart) { g[1].InstallationID = g[0].InstallationID },
		"unknown_profile":        func(g []recoveryLineageGenerationStart) { g[1].Profile = "unknown" },
		"reverse_order":          func(g []recoveryLineageGenerationStart) { g[0], g[1] = g[1], g[0] },
	} {
		t.Run(name, func(t *testing.T) {
			bad := append([]recoveryLineageGenerationStart{}, chain...)
			mutate(bad)
			if validateRecoveryLineageChain(bad, second.InstallationID) == nil {
				t.Fatal("invalid ancestry accepted")
			}
		})
	}
	if validateRecoveryLineageChain(chain, first.InstallationID) == nil || validateRecoveryLineageChain(nil, first.InstallationID) == nil || validateRecoveryLineageChain(make([]recoveryLineageGenerationStart, 65), first.InstallationID) == nil {
		t.Fatal("missing or oversized lineage accepted")
	}
}
