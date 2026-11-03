// SPDX-License-Identifier: Elastic-2.0
package storage

// validateRecoveryLineageChain binds oldest-to-newest inert generations to the
// source of the enclosing authenticated backup. The initial ordinary source
// has no recovery generation of its own. No generation restores authority.
func validateRecoveryLineageChain(generations []recoveryLineageGenerationStart, sourceID string) error {
	if len(generations) < 1 || len(generations) > 64 || !validRecoveryID(sourceID, "inst") {
		return ErrRecoverySnapshotInvalid
	}
	installations := make(map[string]bool, len(generations)+1)
	restores := make(map[string]bool, len(generations))
	// The ordinary ancestor must never appear later as a restored installation.
	installations[generations[0].ImportedSourceID] = true
	previous := generations[0].ImportedSourceID
	for _, generation := range generations {
		if validateRecoveryLineageGenerationIdentity(generation) != nil || generation.ImportedSourceID != previous || installations[generation.InstallationID] || restores[generation.RestoreID] {
			return ErrRecoverySnapshotInvalid
		}
		installations[generation.InstallationID] = true
		restores[generation.RestoreID] = true
		previous = generation.InstallationID
	}
	if previous != sourceID {
		return ErrRecoverySnapshotInvalid
	}
	return nil
}
