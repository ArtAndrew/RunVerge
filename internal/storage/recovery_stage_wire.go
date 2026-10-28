// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"hash"
	"io"
	"reflect"

	"github.com/jackc/pgx/v5"
)

// The archive's second pass is checked as it is copied, including exact table
// summaries and the final whole-stream digest. It never decodes archive DDL.
func recoveryStageCopy(ctx context.Context, tx pgx.Tx, in io.Reader, schema string, inventory RecoverySnapshotInventory, snapshot RecoverySnapshotReport, limits RecoverySnapshotLimits) error {
	r := recoveryStageReader(ctx, in, limits.MaxWireBytes)
	line, err := r.line()
	var header recoverySnapshotHeader
	if err != nil || recoverySnapshotDecode(line, &header) != nil || recoverySnapshotValidateHeader(header) != nil || !reflect.DeepEqual(header.Inventory, inventory) || header.Metadata != snapshot.Metadata {
		return recoverySnapshotError(ctx, ErrRecoverySnapshotInvalid)
	}
	r.accept(line)
	index := 0
	for _, table := range inventory.Tables {
		if !table.Exported {
			continue
		}
		if index >= len(snapshot.Tables) || snapshot.Tables[index].Name != table.Name {
			return ErrRecoverySnapshotInvalid
		}
		line, err = r.line()
		var start recoverySnapshotTableStart
		if err != nil || recoverySnapshotDecode(line, &start) != nil || start.Kind != "table" || start.Name != table.Name {
			return recoverySnapshotError(ctx, ErrRecoverySnapshotInvalid)
		}
		r.accept(line)
		chunks := &recoveryStageChunks{r: r, expected: snapshot.Tables[index], hash: sha256.New()}
		copyContext, cancelCopy := context.WithCancel(ctx)
		typed := newRecoveryStageTypedReader(copyContext, chunks, table, limits)
		sql := "COPY " + pgx.Identifier{schema, table.Name}.Sanitize() + "(" + recoverySnapshotColumns(table) + ") FROM STDIN (FORMAT binary)"
		tag, copyErr := tx.Conn().PgConn().CopyFrom(copyContext, typed, sql)
		cancelCopy()
		typedErr := typed.Stop()
		if copyErr != nil {
			if typedErr != nil && !errors.Is(typedErr, context.Canceled) {
				return recoverySnapshotError(ctx, typedErr)
			}
			if chunks.failure != nil {
				return recoverySnapshotError(ctx, chunks.failure)
			}
			return recoverySnapshotError(ctx, ErrRecoveryStageData)
		}
		if typedErr != nil || !chunks.done || tag.RowsAffected() != chunks.expected.Rows {
			return ErrRecoveryStageData
		}
		index++
	}
	line, err = r.line()
	var state recoverySnapshotState
	if err != nil || recoverySnapshotDecode(line, &state) != nil || recoverySnapshotValidateState(state, inventory, limits) != nil ||
		!reflect.DeepEqual(state.Sequences, snapshot.Sequences) || !reflect.DeepEqual(state.KeyReferences, snapshot.KeyReferences) || !reflect.DeepEqual(state.Organizations, snapshot.Organizations) {
		return recoverySnapshotError(ctx, ErrRecoverySnapshotInvalid)
	}
	r.accept(line)
	line, err = r.line()
	var complete recoverySnapshotComplete
	if err != nil || recoverySnapshotDecode(line, &complete) != nil || complete.Kind != "complete" || complete.Tables != index || complete.Rows != snapshot.Rows || complete.Bytes != snapshot.Bytes || complete.PrecedingSHA256 != recoverySnapshotHash(r.hash) || complete.PrecedingWireBytes != r.bytes {
		return recoverySnapshotError(ctx, ErrRecoverySnapshotInvalid)
	}
	r.accept(line)
	if r.bytes != snapshot.WireBytes || recoverySnapshotHash(r.hash) != snapshot.SHA256 {
		return ErrRecoverySnapshotInvalid
	}
	if _, err := r.reader.Peek(1); err != io.EOF {
		return recoverySnapshotError(ctx, ErrRecoverySnapshotInvalid)
	}
	return nil
}

type recoveryStageChunks struct {
	r        *recoverySnapshotReader
	expected RecoverySnapshotTableSummary
	hash     hash.Hash
	bytes    int64
	buffer   []byte
	done     bool
	failure  error
}

func (c *recoveryStageChunks) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.failure != nil {
		return 0, c.failure
	}
	if len(c.buffer) != 0 {
		n := copy(p, c.buffer)
		c.buffer = c.buffer[n:]
		return n, nil
	}
	if c.done {
		return 0, io.EOF
	}
	fail := func(err error) (int, error) { c.failure = err; return 0, err }
	line, err := c.r.line()
	if err != nil {
		return fail(err)
	}
	kind, err := recoverySnapshotKind(line)
	if err != nil {
		return fail(err)
	}
	if kind == "table_end" {
		var end recoverySnapshotTableEnd
		if recoverySnapshotDecode(line, &end) != nil || end.Summary != c.expected || c.bytes != c.expected.Bytes || recoverySnapshotHash(c.hash) != c.expected.SHA256 {
			return fail(ErrRecoverySnapshotInvalid)
		}
		c.r.accept(line)
		c.done = true
		return 0, io.EOF
	}
	var chunk recoverySnapshotChunk
	if kind != "chunk" || recoverySnapshotDecode(line, &chunk) != nil || chunk.Data == "" || len(chunk.Data) > base64.StdEncoding.EncodedLen(recoverySnapshotChunkSize) {
		return fail(ErrRecoverySnapshotInvalid)
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(chunk.Data)
	if err != nil || len(raw) == 0 || len(raw) > recoverySnapshotChunkSize || base64.StdEncoding.EncodeToString(raw) != chunk.Data || int64(len(raw)) > c.expected.Bytes-c.bytes {
		return fail(ErrRecoverySnapshotInvalid)
	}
	c.bytes += int64(len(raw))
	_, _ = c.hash.Write(raw)
	c.r.accept(line)
	n := copy(p, raw)
	c.buffer = raw[n:]
	return n, nil
}

type recoveryStageDigest struct {
	ctx    context.Context
	parser *recoverySnapshotCopyParser
	hash   hash.Hash
	bytes  int64
	limit  int64
}

func (w *recoveryStageDigest) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.limit-w.bytes {
		return 0, ErrRecoverySnapshotLimit
	}
	if err := w.parser.feed(p); err != nil {
		return 0, err
	}
	w.bytes += int64(len(p))
	_, _ = w.hash.Write(p)
	return len(p), nil
}

func recoveryStageCheckDigests(ctx context.Context, tx pgx.Tx, schema string, inventory RecoverySnapshotInventory, snapshot RecoverySnapshotReport, limits RecoverySnapshotLimits) error {
	counts, err := recoverySnapshotPreflight(ctx, tx, schema, inventory, limits)
	if err != nil {
		return recoverySnapshotError(ctx, ErrRecoveryStageData)
	}
	index := 0
	for _, table := range inventory.Tables {
		if !table.Exported {
			continue
		}
		if index >= len(snapshot.Tables) || snapshot.Tables[index].Name != table.Name || counts[table.Name] != snapshot.Tables[index].Rows {
			return ErrRecoveryStageData
		}
		expected := snapshot.Tables[index]
		w := &recoveryStageDigest{ctx: ctx, parser: newRecoverySnapshotCopyParser(len(table.Columns), limits.MaxRowsPerTable, limits.MaxRowBytes), hash: sha256.New(), limit: limits.MaxTableBytes}
		tag, err := tx.Conn().PgConn().CopyTo(ctx, w, recoverySnapshotCopySQL(schema, table))
		if err != nil || w.parser.finish() != nil || tag.RowsAffected() != expected.Rows || w.parser.rows != expected.Rows || w.bytes != expected.Bytes || recoverySnapshotHash(w.hash) != expected.SHA256 {
			return recoverySnapshotError(ctx, ErrRecoveryStageData)
		}
		index++
	}
	return nil
}
