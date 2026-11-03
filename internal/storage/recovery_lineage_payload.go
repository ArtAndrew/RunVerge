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
	if in == nil || !limits.valid() || declared.Generations < 1 || declared.Generations > 64 || declared.Generations > limits.MaxMetadataEntries || declared.PayloadBytes < 1 || declared.PayloadBytes > 384<<20 || declared.PayloadBytes > limits.MaxWireBytes || declared.PayloadBytes > limits.MaxTotalBytes || declared.Records < 1 || declared.Records > limits.MaxRows {
		return nil, ErrRecoverySnapshotLimit
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	reader := recoveryStageReader(ctx, in, declared.PayloadBytes)
	sections := make([]recoveryLineageSection, 0, declared.Generations)
	identities := make([]recoveryLineageGenerationStart, 0, declared.Generations)
	var report RecoverySnapshotReport
	for i := 0; i < declared.Generations; i++ {
		offset := reader.bytes
		line, err := reader.line()
		if err != nil {
			return nil, err
		}
		var identity recoveryLineageGenerationStart
		if recoverySnapshotDecode(line, &identity) != nil || validateRecoveryLineageGenerationIdentity(identity) != nil {
			return nil, ErrRecoverySnapshotInvalid
		}
		reader.accept(line)
		projected, err := recoveryLineageInventoryProfile(inventory, identity.Profile)
		if err != nil {
			return nil, err
		}
		if err = readRecoveryLineageGenerationTables(reader, projected, limits, &report); err != nil {
			return nil, err
		}
		sections = append(sections, recoveryLineageSection{Identity: identity, Offset: offset, Length: reader.bytes - offset})
		identities = append(identities, identity)
	}
	if reader.bytes != declared.PayloadBytes || report.Rows != declared.Records || validateRecoveryLineageChain(identities, sourceID) != nil {
		return nil, ErrRecoverySnapshotInvalid
	}
	if _, err := reader.reader.Peek(1); err != io.EOF {
		return nil, ErrRecoverySnapshotInvalid
	}
	return sections, nil
}
