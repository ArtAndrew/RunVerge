// SPDX-License-Identifier: Elastic-2.0
package storage

const recoveryAccountingMutationV1 = "sql025-exact-mutation-v1"
const recoveryAccountingMutationV2 = "sql026-exact-mutation-v1"

// Seal from the reviewed SQL026 reference and retain literally forever.
// Do not alias the moving current-catalog constant for historical replay.
const recoveryAccountingSourceV2CatalogSHA256 = "873463718e42a33e7df8ba9eff916260ca46370d49affafc609423338674a9b8"

func recoveryAccountingCommitmentProfile(profile, kind string) (string, string, bool) {
	if kind != "fact" && kind != "resolution" {
		return "", "", false
	}
	switch profile {
	case recoveryAccountingMutationV1:
		return "source-financial-" + kind + "-v1", recoveryAccountingSourceV1CatalogSHA256, true
	case recoveryAccountingMutationV2:
		if !validDigest(recoveryAccountingSourceV2CatalogSHA256) {
			return "", "", false
		}
		return "source-financial-" + kind + "-v2", recoveryAccountingSourceV2CatalogSHA256, true
	default:
		return "", "", false
	}
}

// SQL027 changes only retained-lineage metadata. It hosts the unchanged V2
// accounting contract; V2 commitment hashes keep their original SQL026 anchor.
// This whitelist never substitutes for validation of the full actual catalog.
func recoveryAccountingV2HostCatalog(catalog string) bool {
	return catalog == recoverySQL026Catalog || catalog == recoverySQL027Catalog
}

// Historical observation hashes may be reconstructed on the same host profile
// or the explicitly compatible forward upgrade, never on a downgraded host.
func recoveryUsageHistoricalCatalog(stored, current string) bool {
	return recoveryAccountingV2HostCatalog(stored) && recoveryAccountingV2HostCatalog(current) &&
		(stored == current || stored == recoverySQL026Catalog && current == recoverySQL027Catalog)
}
