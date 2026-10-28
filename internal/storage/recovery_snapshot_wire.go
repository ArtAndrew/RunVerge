// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash"
	"io"
	"reflect"
	"strconv"
	"strings"

	"agentplatform.local/platform/internal/jsonwire"
)

type recoverySnapshotWriter struct {
	ctx          context.Context
	out          io.Writer
	hash         hash.Hash
	bytes, limit int64
}

func (w *recoverySnapshotWriter) record(value any) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return ErrRecoverySnapshotInvalid
	}
	raw = append(raw, '\n')
	if len(raw) > recoverySnapshotMaxRecordBytes || int64(len(raw)) > w.limit-w.bytes {
		return ErrRecoverySnapshotLimit
	}
	n, err := w.out.Write(raw)
	if err != nil || n != len(raw) {
		return recoverySnapshotError(w.ctx, ErrRecoverySnapshotIO)
	}
	w.bytes += int64(n)
	_, _ = w.hash.Write(raw)
	return nil
}

// The binary parser never retains a field value. It validates the exact COPY
// header, field counts, signed lengths, tuple/stream limits and final marker.
type recoverySnapshotCopyParser struct {
	columns              int
	maxRows, maxRowBytes int64
	rows, rowBytes       int64
	state                uint8
	scratch              [19]byte
	have, want           int
	field                int
	remaining            int64
}

func newRecoverySnapshotCopyParser(columns int, maxRows, maxRowBytes int64) *recoverySnapshotCopyParser {
	return &recoverySnapshotCopyParser{columns: columns, maxRows: maxRows, maxRowBytes: maxRowBytes, want: 19}
}
func (p *recoverySnapshotCopyParser) feed(raw []byte) error {
	for len(raw) > 0 {
		if p.state == 4 {
			return ErrRecoverySnapshotInvalid
		}
		if p.state == 3 {
			n := min(int64(len(raw)), p.remaining)
			raw = raw[n:]
			p.remaining -= n
			if p.remaining == 0 {
				p.field++
				if err := p.nextField(); err != nil {
					return err
				}
			}
			continue
		}
		n := min(len(raw), p.want-p.have)
		copy(p.scratch[p.have:], raw[:n])
		p.have += n
		raw = raw[n:]
		if p.have < p.want {
			continue
		}
		p.have = 0
		switch p.state {
		case 0:
			if !bytes.Equal(p.scratch[:11], []byte("PGCOPY\n\xff\r\n\x00")) || binary.BigEndian.Uint32(p.scratch[11:15]) != 0 || binary.BigEndian.Uint32(p.scratch[15:19]) != 0 {
				return ErrRecoverySnapshotInvalid
			}
			p.state, p.want = 1, 2
		case 1:
			columns := int16(binary.BigEndian.Uint16(p.scratch[:2]))
			if columns == -1 {
				p.state = 4
				continue
			}
			if int(columns) != p.columns || columns < 1 {
				return ErrRecoverySnapshotInvalid
			}
			if p.rows >= p.maxRows {
				return ErrRecoverySnapshotLimit
			}
			p.field, p.rowBytes = 0, 2
			p.state, p.want = 2, 4
		case 2:
			length := int32(binary.BigEndian.Uint32(p.scratch[:4]))
			if length < -1 {
				return ErrRecoverySnapshotInvalid
			}
			p.rowBytes += 4
			if length > 0 {
				p.rowBytes += int64(length)
			}
			if p.rowBytes > p.maxRowBytes {
				return ErrRecoverySnapshotLimit
			}
			if length > 0 {
				p.remaining = int64(length)
				p.state = 3
			} else {
				p.field++
				if err := p.nextField(); err != nil {
					return err
				}
			}
		default:
			return ErrRecoverySnapshotInvalid
		}
	}
	return nil
}
func (p *recoverySnapshotCopyParser) nextField() error {
	if p.field == p.columns {
		p.rows++
		p.state, p.want = 1, 2
		return nil
	}
	if p.field > p.columns {
		return ErrRecoverySnapshotInvalid
	}
	p.state, p.want = 2, 4
	return nil
}
func (p *recoverySnapshotCopyParser) finish() error {
	if p.state != 4 || p.have != 0 || p.remaining != 0 {
		return ErrRecoverySnapshotInvalid
	}
	return nil
}

type recoverySnapshotCopyWriter struct {
	w               *recoverySnapshotWriter
	parser          *recoverySnapshotCopyParser
	hash            hash.Hash
	buffer          []byte
	bytes, maxBytes int64
	failure         error
}

func newRecoverySnapshotCopyWriter(w *recoverySnapshotWriter, t RecoverySnapshotTable, l RecoverySnapshotLimits, remainingBytes, remainingRows int64) *recoverySnapshotCopyWriter {
	return &recoverySnapshotCopyWriter{w: w, parser: newRecoverySnapshotCopyParser(len(t.Columns), min(l.MaxRowsPerTable, remainingRows), l.MaxRowBytes), hash: sha256.New(), buffer: make([]byte, 0, recoverySnapshotChunkSize), maxBytes: min(l.MaxTableBytes, remainingBytes)}
}
func (c *recoverySnapshotCopyWriter) failureOr(fallback error) error {
	if c.failure != nil {
		return c.failure
	}
	return fallback
}
func (c *recoverySnapshotCopyWriter) Write(p []byte) (int, error) {
	fail := func(err error) (int, error) { c.failure = err; return 0, err }
	if c.failure != nil {
		return 0, c.failure
	}
	if err := c.w.ctx.Err(); err != nil {
		return fail(err)
	}
	if int64(len(p)) > c.maxBytes-c.bytes {
		return fail(ErrRecoverySnapshotLimit)
	}
	if err := c.parser.feed(p); err != nil {
		return fail(err)
	}
	c.bytes += int64(len(p))
	_, _ = c.hash.Write(p)
	original := len(p)
	for len(p) > 0 {
		n := min(len(p), recoverySnapshotChunkSize-len(c.buffer))
		c.buffer = append(c.buffer, p[:n]...)
		p = p[n:]
		if len(c.buffer) == recoverySnapshotChunkSize {
			if err := c.flush(); err != nil {
				return fail(err)
			}
		}
	}
	return original, nil
}
func (c *recoverySnapshotCopyWriter) flush() error {
	if len(c.buffer) == 0 {
		return nil
	}
	err := c.w.record(recoverySnapshotChunk{Kind: "chunk", Data: base64.StdEncoding.EncodeToString(c.buffer)})
	if err == nil {
		c.buffer = c.buffer[:0]
	}
	return err
}
func (c *recoverySnapshotCopyWriter) finish() error {
	if c.failure != nil {
		return c.failure
	}
	if err := c.parser.finish(); err != nil {
		return err
	}
	return c.flush()
}

type recoverySnapshotReader struct {
	ctx          context.Context
	reader       *bufio.Reader
	bytes, limit int64
	hash         hash.Hash
}

func (r *recoverySnapshotReader) line() ([]byte, error) {
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	var line []byte
	for {
		chunk, err := r.reader.ReadSlice('\n')
		if len(line)+len(chunk) > recoverySnapshotMaxRecordBytes || int64(len(line)+len(chunk)) > r.limit-r.bytes {
			return nil, ErrRecoverySnapshotLimit
		}
		line = append(line, chunk...)
		if err == nil {
			return line, nil
		}
		if err != bufio.ErrBufferFull {
			if err == io.EOF {
				return nil, ErrRecoverySnapshotInvalid
			}
			return nil, recoverySnapshotError(r.ctx, ErrRecoverySnapshotIO)
		}
		if err = r.ctx.Err(); err != nil {
			return nil, err
		}
	}
}
func (r *recoverySnapshotReader) accept(line []byte) {
	r.bytes += int64(len(line))
	_, _ = r.hash.Write(line)
}
func recoverySnapshotDecode(raw []byte, out any) error {
	if len(raw) == 0 || raw[len(raw)-1] != '\n' || !jsonwire.ValidUnicode(raw) {
		return ErrRecoverySnapshotInvalid
	}
	// encoding/json accepts duplicate properties; reject them before decoding.
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if recoverySnapshotJSONValue(d, 0) != nil {
		return ErrRecoverySnapshotInvalid
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrRecoverySnapshotInvalid
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return ErrRecoverySnapshotInvalid
	}
	return recoverySnapshotRequiredShape(raw, reflect.TypeOf(out).Elem(), 0)
}
func recoverySnapshotJSONValue(d *json.Decoder, depth int) error {
	if depth > 32 {
		return ErrRecoverySnapshotInvalid
	}
	token, err := d.Token()
	if err != nil || token == nil {
		return ErrRecoverySnapshotInvalid
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] {
				return ErrRecoverySnapshotInvalid
			}
			seen[name] = true
			if recoverySnapshotJSONValue(d, depth+1) != nil {
				return ErrRecoverySnapshotInvalid
			}
		}
		token, err = d.Token()
		if err != nil || token != json.Delim('}') {
			return ErrRecoverySnapshotInvalid
		}
	case '[':
		for d.More() {
			if recoverySnapshotJSONValue(d, depth+1) != nil {
				return ErrRecoverySnapshotInvalid
			}
		}
		token, err = d.Token()
		if err != nil || token != json.Delim(']') {
			return ErrRecoverySnapshotInvalid
		}
	default:
		return ErrRecoverySnapshotInvalid
	}
	return nil
}
func recoverySnapshotRequiredShape(raw []byte, t reflect.Type, depth int) error {
	if depth > 32 {
		return ErrRecoverySnapshotInvalid
	}
	if t.Kind() == reflect.Pointer {
		return recoverySnapshotRequiredShape(raw, t.Elem(), depth+1)
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return ErrRecoverySnapshotInvalid
		}
		if len(fields) != t.NumField() {
			return ErrRecoverySnapshotInvalid
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			value, ok := fields[name]
			if !ok {
				return ErrRecoverySnapshotInvalid
			}
			if strings.HasSuffix(f.Tag.Get("json"), ",string") {
				var decimal string
				if json.Unmarshal(value, &decimal) != nil {
					return ErrRecoverySnapshotInvalid
				}
				integer, err := strconv.ParseInt(decimal, 10, 64)
				if err != nil || strconv.FormatInt(integer, 10) != decimal {
					return ErrRecoverySnapshotInvalid
				}
			}
			if recoverySnapshotRequiredShape(value, f.Type, depth+1) != nil {
				return ErrRecoverySnapshotInvalid
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil || values == nil {
			return ErrRecoverySnapshotInvalid
		}
		for _, value := range values {
			if recoverySnapshotRequiredShape(value, t.Elem(), depth+1) != nil {
				return ErrRecoverySnapshotInvalid
			}
		}
	}
	return nil
}
func recoverySnapshotKind(line []byte) (string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(line, &fields) != nil {
		return "", ErrRecoverySnapshotInvalid
	}
	var kind string
	if json.Unmarshal(fields["kind"], &kind) != nil {
		return "", ErrRecoverySnapshotInvalid
	}
	return kind, nil
}

// ValidateRecoverySnapshot authenticates no publisher by itself. Call it on
// completely authenticated/decrypted private input, then compare Metadata with
// the independently pinned encrypted-envelope metadata before any import.
// It validates framing, the compiled schema/type inventory, counts/digests and
// bounds. It does not decode field types or enforce NOT NULL/primary keys.
// A future trusted importer must additionally validate those row constraints,
// cross-table references, accounting projections, generated columns and sequence values
// against actual imported data; a valid stream is not permission to resume.
func ValidateRecoverySnapshot(ctx context.Context, in io.Reader, limits RecoverySnapshotLimits) (RecoverySnapshotReport, error) {
	var report RecoverySnapshotReport
	if in == nil || !limits.valid() {
		return report, ErrRecoverySnapshotLimit
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	r := &recoverySnapshotReader{ctx: ctx, reader: bufio.NewReaderSize(io.LimitReader(in, limits.MaxWireBytes+1), 64<<10), hash: sha256.New(), limit: limits.MaxWireBytes}
	line, err := r.line()
	if err != nil {
		return report, err
	}
	var h recoverySnapshotHeader
	if recoverySnapshotDecode(line, &h) != nil {
		return report, ErrRecoverySnapshotInvalid
	}
	if err = recoverySnapshotValidateHeader(h); err != nil {
		return report, err
	}
	r.accept(line)
	report = RecoverySnapshotReport{Metadata: h.Metadata, Format: h.Format, InventorySHA256: h.InventorySHA256, Tables: []RecoverySnapshotTableSummary{}}
	for _, table := range h.Inventory.Tables {
		if !table.Exported {
			continue
		}
		line, err = r.line()
		if err != nil {
			return RecoverySnapshotReport{}, err
		}
		var start recoverySnapshotTableStart
		if recoverySnapshotDecode(line, &start) != nil || start.Kind != "table" || start.Name != table.Name {
			return RecoverySnapshotReport{}, ErrRecoverySnapshotInvalid
		}
		r.accept(line)
		parser := newRecoverySnapshotCopyParser(len(table.Columns), min(limits.MaxRowsPerTable, limits.MaxRows-report.Rows), limits.MaxRowBytes)
		digest := sha256.New()
		var size int64
		short := false
		for {
			line, err = r.line()
			if err != nil {
				return RecoverySnapshotReport{}, err
			}
			kind, kindErr := recoverySnapshotKind(line)
			if kindErr != nil {
				return RecoverySnapshotReport{}, kindErr
			}
			if kind == "table_end" {
				var end recoverySnapshotTableEnd
				if recoverySnapshotDecode(line, &end) != nil || parser.finish() != nil || end.Summary.Name != table.Name || end.Summary.Rows != parser.rows || end.Summary.Bytes != size || end.Summary.SHA256 != recoverySnapshotHash(digest) {
					return RecoverySnapshotReport{}, ErrRecoverySnapshotInvalid
				}
				r.accept(line)
				report.Tables = append(report.Tables, end.Summary)
				report.Rows += parser.rows
				report.Bytes += size
				break
			}
			if kind != "chunk" || short {
				return RecoverySnapshotReport{}, ErrRecoverySnapshotInvalid
			}
			var chunk recoverySnapshotChunk
			if recoverySnapshotDecode(line, &chunk) != nil || chunk.Data == "" || len(chunk.Data) > base64.StdEncoding.EncodedLen(recoverySnapshotChunkSize) {
				return RecoverySnapshotReport{}, ErrRecoverySnapshotInvalid
			}
			raw, decodeErr := base64.StdEncoding.Strict().DecodeString(chunk.Data)
			if decodeErr != nil || len(raw) == 0 || len(raw) > recoverySnapshotChunkSize || base64.StdEncoding.EncodeToString(raw) != chunk.Data {
				return RecoverySnapshotReport{}, ErrRecoverySnapshotInvalid
			}
			if int64(len(raw)) > limits.MaxTableBytes-size || int64(len(raw)) > limits.MaxTotalBytes-report.Bytes-size {
				return RecoverySnapshotReport{}, ErrRecoverySnapshotLimit
			}
			if err = parser.feed(raw); err != nil {
				return RecoverySnapshotReport{}, err
			}
			size += int64(len(raw))
			_, _ = digest.Write(raw)
			short = len(raw) < recoverySnapshotChunkSize
			r.accept(line)
		}
	}
	line, err = r.line()
	if err != nil {
		return RecoverySnapshotReport{}, err
	}
	var state recoverySnapshotState
	if recoverySnapshotDecode(line, &state) != nil {
		return RecoverySnapshotReport{}, ErrRecoverySnapshotInvalid
	}
	if err = recoverySnapshotValidateState(state, h.Inventory, limits); err != nil {
		return RecoverySnapshotReport{}, err
	}
	r.accept(line)
	line, err = r.line()
	if err != nil {
		return RecoverySnapshotReport{}, err
	}
	var complete recoverySnapshotComplete
	if recoverySnapshotDecode(line, &complete) != nil || complete.Kind != "complete" || complete.Tables != len(report.Tables) || complete.Rows != report.Rows || complete.Bytes != report.Bytes || complete.PrecedingWireBytes != r.bytes || complete.PrecedingSHA256 != recoverySnapshotHash(r.hash) {
		return RecoverySnapshotReport{}, ErrRecoverySnapshotInvalid
	}
	r.accept(line)
	if err = ctx.Err(); err != nil {
		return RecoverySnapshotReport{}, err
	}
	if _, err = r.reader.Peek(1); err != io.EOF {
		if err != nil {
			return RecoverySnapshotReport{}, recoverySnapshotError(ctx, ErrRecoverySnapshotIO)
		}
		return RecoverySnapshotReport{}, ErrRecoverySnapshotInvalid
	}
	report.Sequences, report.KeyReferences, report.Organizations = state.Sequences, state.KeyReferences, state.Organizations
	report.WireBytes, report.SHA256 = r.bytes, recoverySnapshotHash(r.hash)
	return report, nil
}
