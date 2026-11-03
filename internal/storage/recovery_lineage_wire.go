// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/recoverylineage"
	"encoding/base64"
	"io"
)

const recoverySnapshotLineageFormat = "runverge-pg18-copy-ndjson-v2"

// RecoveryLineageFramingReport describes bounded framing only. Counts are wire
// declarations until the generation semantic validator has verified payloads.
// It conveys no current authority or independently verified historical signature.
type RecoveryLineageFramingReport struct {
	PayloadBytes int64  `json:"payload_bytes,string"`
	Records      int64  `json:"records,string"`
	Generations  int    `json:"generations"`
	ChunksSHA256 string `json:"chunks_sha256"`
}
type recoveryLineageStart struct {
	Kind         string `json:"kind"`
	PayloadBytes int64  `json:"payload_bytes,string"`
	Records      int64  `json:"records,string"`
	Generations  int    `json:"generations"`
	ChunksSHA256 string `json:"chunks_sha256"`
}
type recoveryLineageChunk struct {
	Kind    string `json:"kind"`
	Ordinal int    `json:"ordinal"`
	Data    string `json:"data"`
}
type recoveryLineageEnd struct {
	Kind         string `json:"kind"`
	ChunksSHA256 string `json:"chunks_sha256"`
}

func readRecoverySnapshotLineage(r *recoverySnapshotReader, limits RecoverySnapshotLimits, report *RecoverySnapshotReport) error {
	return readRecoverySnapshotLineageTo(r, limits, report, nil)
}

// The sink is caller-owned staging only. Any error, including a later enclosing
// archive failure, invalidates all written bytes; callers must discard them.
func readRecoverySnapshotLineageTo(r *recoverySnapshotReader, limits RecoverySnapshotLimits, report *RecoverySnapshotReport, sink io.Writer) error {
	line, err := r.line()
	if err != nil {
		return err
	}
	var start recoveryLineageStart
	if recoverySnapshotDecode(line, &start) != nil || start.Kind != "lineage" || !recoverySnapshotDigest.MatchString(start.ChunksSHA256) {
		return ErrRecoverySnapshotInvalid
	}
	if start.PayloadBytes < 1 || start.PayloadBytes > limits.MaxTotalBytes-report.Bytes || start.Records < 1 || start.Records > limits.MaxRows-report.Rows || start.Generations < 1 || start.Generations > 64 || start.Generations > limits.MaxMetadataEntries {
		return ErrRecoverySnapshotLimit
	}
	commitment, err := recoverylineage.NewChunkCommitment(start.PayloadBytes)
	if err != nil {
		return ErrRecoverySnapshotLimit
	}
	r.accept(line)
	count := (start.PayloadBytes + recoverylineage.ChunkBytes - 1) / recoverylineage.ChunkBytes
	for ordinal := 0; int64(ordinal) < count; ordinal++ {
		line, err = r.line()
		if err != nil {
			return err
		}
		var chunk recoveryLineageChunk
		if recoverySnapshotDecode(line, &chunk) != nil || chunk.Kind != "lineage_chunk" || chunk.Ordinal != ordinal || chunk.Data == "" || len(chunk.Data) > base64.StdEncoding.EncodedLen(recoverylineage.ChunkBytes) {
			return ErrRecoverySnapshotInvalid
		}
		raw, e := base64.StdEncoding.Strict().DecodeString(chunk.Data)
		if e != nil || base64.StdEncoding.EncodeToString(raw) != chunk.Data || commitment.Add(ordinal, raw) != nil {
			return ErrRecoverySnapshotInvalid
		}
		if sink != nil {
			if err = r.ctx.Err(); err != nil {
				return err
			}
			n, writeErr := sink.Write(raw)
			if writeErr != nil || n != len(raw) {
				return recoverySnapshotError(r.ctx, ErrRecoverySnapshotIO)
			}
		}
		r.accept(line)
	}
	line, err = r.line()
	if err != nil {
		return err
	}
	var end recoveryLineageEnd
	digest, err := commitment.Digest()
	if err != nil || recoverySnapshotDecode(line, &end) != nil || end.Kind != "lineage_end" || end.ChunksSHA256 != digest || start.ChunksSHA256 != digest {
		return ErrRecoverySnapshotInvalid
	}
	r.accept(line)
	report.Lineage = &RecoveryLineageFramingReport{PayloadBytes: start.PayloadBytes, Records: start.Records, Generations: start.Generations, ChunksSHA256: start.ChunksSHA256}
	report.Bytes += start.PayloadBytes
	report.Rows += start.Records
	return nil
}

// writeRecoverySnapshotLineage writes into the enclosing snapshot's writer and
// budgets. The payload must be a bounded, cancellation-cooperating source from
// the same snapshot transaction. The caller must abandon the enclosing archive on any write error.
// Semantic validation and atomic storage remain separate required stages.
func writeRecoverySnapshotLineage(w *recoverySnapshotWriter, in io.Reader, declared RecoveryLineageFramingReport, limits RecoverySnapshotLimits, report *RecoverySnapshotReport) error {
	if w == nil || in == nil || report == nil || report.Lineage != nil || report.Bytes < 0 || report.Rows < 0 || declared.PayloadBytes < 1 || declared.PayloadBytes > limits.MaxTotalBytes-report.Bytes || declared.Records < 1 || declared.Records > limits.MaxRows-report.Rows || declared.Generations < 1 || declared.Generations > 64 || declared.Generations > limits.MaxMetadataEntries || !recoverySnapshotDigest.MatchString(declared.ChunksSHA256) {
		return ErrRecoverySnapshotLimit
	}
	commitment, err := recoverylineage.NewChunkCommitment(declared.PayloadBytes)
	if err != nil {
		return ErrRecoverySnapshotLimit
	}
	if err = w.record(recoveryLineageStart{Kind: "lineage", PayloadBytes: declared.PayloadBytes, Records: declared.Records, Generations: declared.Generations, ChunksSHA256: declared.ChunksSHA256}); err != nil {
		return err
	}
	buffer := make([]byte, min(declared.PayloadBytes, int64(recoverylineage.ChunkBytes)))
	remaining := declared.PayloadBytes
	for ordinal := 0; remaining > 0; ordinal++ {
		if err = w.ctx.Err(); err != nil {
			return err
		}
		chunk := buffer[:min(int64(len(buffer)), remaining)]
		if _, err = io.ReadFull(in, chunk); err != nil {
			return recoverySnapshotError(w.ctx, ErrRecoverySnapshotIO)
		}
		if err = commitment.Add(ordinal, chunk); err != nil {
			return ErrRecoverySnapshotInvalid
		}
		if err = w.record(recoveryLineageChunk{Kind: "lineage_chunk", Ordinal: ordinal, Data: base64.StdEncoding.EncodeToString(chunk)}); err != nil {
			return err
		}
		remaining -= int64(len(chunk))
	}
	var extra [1]byte
	n, err := in.Read(extra[:])
	if n != 0 || err != io.EOF {
		return recoverySnapshotError(w.ctx, ErrRecoverySnapshotInvalid)
	}
	digest, err := commitment.Digest()
	if err != nil || digest != declared.ChunksSHA256 {
		return ErrRecoverySnapshotInvalid
	}
	if err = w.record(recoveryLineageEnd{Kind: "lineage_end", ChunksSHA256: digest}); err != nil {
		return err
	}
	report.Lineage = &declared
	report.Bytes += declared.PayloadBytes
	report.Rows += declared.Records
	return nil
}
