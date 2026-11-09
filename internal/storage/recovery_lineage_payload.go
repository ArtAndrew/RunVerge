// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"io"
)

type recoveryLineageSection struct {
	Identity       recoveryLineageGenerationStart
	Offset, Length int64
}

// indexRecoveryLineagePayload reads a flat decoded historical payload. The
// enclosing authenticated envelope must separately verify its chunk commitment.
// Returned spans identify original bytes for typed staging and exact copying;
// they are not execution authority or re-authenticated historical signatures.
func indexRecoveryLineagePayload(ctx context.Context, in io.Reader, sourceID string, declared RecoveryLineageFramingReport, inventory RecoverySnapshotInventory, limits RecoverySnapshotLimits) ([]recoveryLineageSection, error) {
	if declared.Records < 1 || declared.Records > limits.MaxRows {
		return nil, ErrRecoverySnapshotLimit
	}
	sections, rows, err := scanRecoveryLineagePayload(ctx, in, sourceID, declared.Generations, declared.PayloadBytes, inventory, limits)
	if err != nil {
		return nil, err
	}
	if rows != declared.Records {
		return nil, ErrRecoverySnapshotInvalid
	}
	return sections, nil
}

// Export derives the record count from stored bytes. Import additionally checks
// the authenticated wire declaration in indexRecoveryLineagePayload.
func scanRecoveryLineagePayload(ctx context.Context, in io.Reader, sourceID string, generations int, payloadBytes int64, inventory RecoverySnapshotInventory, limits RecoverySnapshotLimits) ([]recoveryLineageSection, int64, error) {
	if in == nil || !limits.valid() || generations < 1 || generations > 64 || generations > limits.MaxMetadataEntries || payloadBytes < 1 || payloadBytes > 384<<20 || payloadBytes > limits.MaxWireBytes || payloadBytes > limits.MaxTotalBytes {
		return nil, 0, ErrRecoverySnapshotLimit
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	reader := recoveryStageReader(ctx, in, payloadBytes)
	sections := make([]recoveryLineageSection, 0, generations)
	identities := make([]recoveryLineageGenerationStart, 0, generations)
	var report RecoverySnapshotReport
	for i := 0; i < generations; i++ {
		offset := reader.bytes
		line, err := reader.line()
		if err != nil {
			return nil, 0, err
		}
		var identity recoveryLineageGenerationStart
		if recoverySnapshotDecode(line, &identity) != nil || validateRecoveryLineageGenerationIdentity(identity) != nil {
			return nil, 0, ErrRecoverySnapshotInvalid
		}
		reader.accept(line)
		projected, err := recoveryLineageInventoryProfile(inventory, identity.Profile)
		if err != nil {
			return nil, 0, err
		}
		if err = readRecoveryLineageGenerationTables(reader, projected, limits, &report); err != nil {
			return nil, 0, err
		}
		sections = append(sections, recoveryLineageSection{Identity: identity, Offset: offset, Length: reader.bytes - offset})
		identities = append(identities, identity)
	}
	if reader.bytes != payloadBytes || validateRecoveryLineageChain(identities, sourceID) != nil {
		return nil, 0, ErrRecoverySnapshotInvalid
	}
	if _, err := reader.reader.Peek(1); err != io.EOF {
		return nil, 0, ErrRecoverySnapshotInvalid
	}
	return sections, report.Rows, nil
}
