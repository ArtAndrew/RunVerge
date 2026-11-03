// SPDX-License-Identifier: Elastic-2.0
package storage

import "encoding/hex"

// readRecoveryLineageGeneration validates one generation's exact layout and
// identity against an independently selected expected identity. It consumes
// shared budgets without buffering the payload. This is not row semantics or
// authorization: a later staging importer must validate keys, references,
// receipt contents and accounting before persisting or admitting any work.
func readRecoveryLineageGeneration(r *recoverySnapshotReader, inventory RecoverySnapshotInventory, expected recoveryLineageGenerationStart, limits RecoverySnapshotLimits, report *RecoverySnapshotReport) error {
	if r == nil || report == nil || !limits.valid() {
		return ErrRecoverySnapshotLimit
	}
	projected, err := recoveryLineageSourceInventory(inventory)
	if err != nil {
		return err
	}
	if err = validateRecoveryLineageGenerationIdentity(expected); err != nil {
		return err
	}
	line, err := r.line()
	if err != nil {
		return err
	}
	var start recoveryLineageGenerationStart
	if recoverySnapshotDecode(line, &start) != nil || start != expected {
		return ErrRecoverySnapshotInvalid
	}
	r.accept(line)
	return readRecoveryLineageGenerationTables(r, projected, limits, report)
}

func readRecoveryLineageGenerationTables(r *recoverySnapshotReader, projected RecoverySnapshotInventory, limits RecoverySnapshotLimits, report *RecoverySnapshotReport) error {
	rows, bytes, tables := report.Rows, report.Bytes, len(report.Tables)
	metadataLimits := limits
	metadataLimits.MaxRowBytes = recoveryLineageMetadataRowBytes
	if err := readRecoverySnapshotTables(r, projected, metadataLimits, report); err != nil {
		return err
	}
	line, err := r.line()
	if err != nil {
		return err
	}
	var end recoveryLineageGenerationEnd
	if recoverySnapshotDecode(line, &end) != nil || end.Kind != "generation_end" || end.Tables != len(report.Tables)-tables || end.Rows != report.Rows-rows || end.Bytes != report.Bytes-bytes {
		return ErrRecoverySnapshotInvalid
	}
	r.accept(line)
	return nil
}

func validateRecoveryLineageGenerationIdentity(expected recoveryLineageGenerationStart) error {
	digest, err := hex.DecodeString(expected.ImportedSnapshotSHA256)
	if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != expected.ImportedSnapshotSHA256 ||
		expected.Kind != "generation" || expected.Profile != "sql026-current-generation-v1" ||
		expected.InventorySHA256 != recoveryLineageSQL026Inventory || expected.CatalogSHA256 != recoveryLineageSQL026Catalog ||
		!validRecoveryID(expected.InstallationID, "inst") || !validRecoveryID(expected.ImportedSourceID, "inst") ||
		expected.InstallationID == expected.ImportedSourceID || !validRecoveryID(expected.RestoreID, "restore") {
		return ErrRecoverySnapshotInvalid
	}
	return nil
}
