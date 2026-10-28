// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"sync"
	"unicode/utf8"
)

// ErrRecoveryBinaryValue intentionally contains no field value, table payload,
// parser detail or underlying transport error.
var ErrRecoveryBinaryValue = errors.New("invalid or unsupported recovery binary value")

// recoveryStageTypedReader is a finite, single-consumer COPY preflight. It may
// release the benign header before inspecting rows, but releases no part of a
// row until every field in that bounded row has passed. It is not a relational
// validator: NULL, NOT NULL, uniqueness, FK and business facts remain stage work.
type recoveryStageTypedReader struct {
	mu           sync.Mutex
	ctx          context.Context
	raw          io.Reader
	columns      []RecoverySnapshotColumn
	maxBytes     int64
	maxRows      int64
	maxRow       int64
	bytes, rows  int64
	pending      []byte
	err          error
	header, done bool
	sourceEOF    bool
	stopped      bool
}

// Limits here concern decoded COPY bytes, not NDJSON/base64 MaxWireBytes.
// The caller separately enforces global remaining budgets across all tables.
// Generic blocked I/O still requires caller-owned cancellation/deadlines.
func newRecoveryStageTypedReader(ctx context.Context, raw io.Reader, table RecoverySnapshotTable, limits RecoverySnapshotLimits) *recoveryStageTypedReader {
	r := &recoveryStageTypedReader{ctx: ctx, raw: raw, columns: slices.Clone(table.Columns),
		maxBytes: min(limits.MaxTableBytes, limits.MaxTotalBytes), maxRows: min(limits.MaxRowsPerTable, limits.MaxRows), maxRow: limits.MaxRowBytes}
	if ctx == nil || raw == nil || len(r.columns) == 0 || len(r.columns) > 256 ||
		limits.MaxRowBytes < 32 || limits.MaxRowBytes > 16<<20 ||
		limits.MaxTableBytes < 21 || limits.MaxTableBytes > 4<<30 || limits.MaxTotalBytes < 21 || limits.MaxTotalBytes > 16<<30 ||
		limits.MaxRowsPerTable < 0 || limits.MaxRowsPerTable > 10_000_000 || limits.MaxRows < 0 || limits.MaxRows > 50_000_000 {
		r.err = ErrRecoveryBinaryValue
	}
	for _, column := range r.columns {
		switch column.TypeOID {
		case 16, 17, 20, 23, 25, 1009, 1184, 3802:
		default:
			r.err = ErrRecoveryBinaryValue
		}
	}
	return r
}

func (r *recoveryStageTypedReader) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Stop waits for the current local-file Read, discards pending output and
// detaches the borrowed input. Future Read calls never touch raw. The owner
// cancels its child context before Stop after COPY returns: pgconn can return
// early on a fatal connection error while its reader goroutine is still live.
// Stop is not an interrupt for an arbitrary blocked transport. A clean EOF or
// deliberate stop does not invent a value error; COPY/result checks stay owned
// by the caller.
func (r *recoveryStageTypedReader) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	r.pending = nil
	return r.err
}

func (r *recoveryStageTypedReader) fail(err error) error {
	if r.err == nil {
		r.err = err
	}
	r.pending = nil
	return r.err
}

func (r *recoveryStageTypedReader) readExact(dst []byte) error {
	if int64(len(dst)) > r.maxBytes-r.bytes || r.sourceEOF {
		return ErrRecoveryBinaryValue
	}
	empty := 0
	for len(dst) > 0 {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		n, err := r.raw.Read(dst)
		if e := r.ctx.Err(); e != nil {
			return e
		}
		if n < 0 || n > len(dst) {
			return ErrRecoveryBinaryValue
		}
		r.bytes += int64(n)
		dst = dst[n:]
		if err == io.EOF {
			r.sourceEOF = true
			if len(dst) == 0 {
				return nil
			}
			return ErrRecoveryBinaryValue
		}
		if err != nil {
			return ErrRecoveryBinaryValue
		}
		if n == 0 {
			empty++
			if empty >= 100 {
				return ErrRecoveryBinaryValue
			}
		} else {
			empty = 0
		}
	}
	return nil
}

func (r *recoveryStageTypedReader) requireEOF() error {
	if r.sourceEOF {
		return r.ctx.Err()
	}
	var one [1]byte
	for empty := 0; empty < 100; empty++ {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		n, err := r.raw.Read(one[:])
		if e := r.ctx.Err(); e != nil {
			return e
		}
		if n != 0 {
			return ErrRecoveryBinaryValue
		}
		if err == io.EOF {
			r.sourceEOF = true
			return nil
		}
		if err != nil {
			return ErrRecoveryBinaryValue
		}
	}
	return ErrRecoveryBinaryValue
}

func (r *recoveryStageTypedReader) next() error {
	if !r.header {
		header := make([]byte, 19)
		if err := r.readExact(header); err != nil {
			return err
		}
		if !bytes.Equal(header, []byte("PGCOPY\n\xff\r\n\x00\x00\x00\x00\x00\x00\x00\x00\x00")) {
			return ErrRecoveryBinaryValue
		}
		r.header, r.pending = true, header
		return nil
	}
	var prefix [4]byte
	if err := r.readExact(prefix[:2]); err != nil {
		return err
	}
	columns := int16(binary.BigEndian.Uint16(prefix[:2]))
	if columns == -1 {
		if err := r.requireEOF(); err != nil {
			return err
		}
		r.done, r.pending = true, []byte{255, 255}
		return nil
	}
	if int(columns) != len(r.columns) || r.rows >= r.maxRows {
		return ErrRecoveryBinaryValue
	}
	row := make([]byte, 2, int(min(r.maxRow, 4096)))
	copy(row, prefix[:2])
	for _, column := range r.columns {
		if int64(len(row))+4 > r.maxRow {
			return ErrRecoveryBinaryValue
		}
		if err := r.readExact(prefix[:]); err != nil {
			return err
		}
		length := int32(binary.BigEndian.Uint32(prefix[:]))
		if length < -1 || (length >= 0 && int64(length) > r.maxRow-int64(len(row))-4) {
			return ErrRecoveryBinaryValue
		}
		row = append(row, prefix[:]...)
		if length == -1 { // A well-formed NULL; PG enforces NOT NULL separately.
			continue
		}
		start := len(row)
		row = slices.Grow(row, int(length))
		row = row[:start+int(length)]
		if err := r.readExact(row[start:]); err != nil {
			return err
		}
		if !recoveryBinaryField(column.TypeOID, row[start:]) {
			return ErrRecoveryBinaryValue
		}
	}
	r.rows++
	r.pending = row
	return nil
}

func (r *recoveryStageTypedReader) Read(dst []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return 0, r.err
	}
	if r.stopped {
		return 0, io.EOF
	}
	if len(dst) == 0 {
		return 0, nil
	}
	if err := r.ctx.Err(); err != nil {
		return 0, r.fail(err)
	}
	if len(r.pending) == 0 {
		if r.done {
			return 0, io.EOF
		}
		if err := r.next(); err != nil {
			return 0, r.fail(err)
		}
	}
	n := copy(dst, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func recoveryBinaryText(raw []byte) bool {
	return utf8.Valid(raw) && bytes.IndexByte(raw, 0) < 0
}

func recoveryBinaryField(oid uint32, raw []byte) bool {
	switch oid {
	case 16:
		return len(raw) == 1 && raw[0] <= 1
	case 17:
		return true
	case 20, 1184:
		return len(raw) == 8
	case 23:
		return len(raw) == 4
	case 25:
		return recoveryBinaryText(raw)
	case 1009:
		return recoveryBinaryTextArray(raw)
	case 3802:
		return len(raw) > 1 && raw[0] == 1 && recoveryBinaryJSON(raw[1:])
	}
	return false
}

func recoveryBinaryTextArray(raw []byte) bool {
	if len(raw) < 12 {
		return false
	}
	ndim := int32(binary.BigEndian.Uint32(raw))
	flags := int32(binary.BigEndian.Uint32(raw[4:]))
	if ndim < 0 || ndim > 6 || (flags != 0 && flags != 1) || binary.BigEndian.Uint32(raw[8:]) != 25 || len(raw) < 12+int(ndim)*8 {
		return false
	}
	position := 12 + int(ndim)*8
	maxElements := int64((len(raw) - position) / 4)
	items := int64(0)
	if ndim > 0 {
		items = 1
	}
	for i := 0; i < int(ndim); i++ {
		dim := int64(int32(binary.BigEndian.Uint32(raw[12+i*8:])))
		lower := int64(int32(binary.BigEndian.Uint32(raw[16+i*8:])))
		// PG18 requires the exclusive upper bound dim+lower to fit int32.
		if dim < 0 || dim+lower > math.MaxInt32 || dim+lower < math.MinInt32 || (dim > 0 && items > math.MaxInt32/dim) {
			return false
		}
		items *= dim
	}
	// PG array_recv allocates from nitems BEFORE checking its element bytes.
	// Every element needs at least a four-byte length, even when NULL. Check
	// this necessary bound before looping or allowing these bytes to reach PG.
	if items > maxElements {
		return false
	}
	for i := int64(0); i < items; i++ {
		if len(raw)-position < 4 {
			return false
		}
		length := int32(binary.BigEndian.Uint32(raw[position:]))
		position += 4
		if length == -1 {
			continue
		}
		if length < 0 || int64(length) > int64(len(raw)-position) {
			return false
		}
		if !recoveryBinaryText(raw[position : position+int(length)]) {
			return false
		}
		position += int(length)
	}
	return position == len(raw)
}

// PG18 jsonb_send uses numeric_out's decimal notation. Exponents are excluded
// from this canonical snapshot profile, preventing tiny numbers such as1e100000
// from expanding substantially in PG before CHECKs. String content is untouched.
// This is not a general JSONB import decoder. Depth is bounded before json.Valid
// allocates its scanner stack; Unicode escapes follow PG's UTF8/NUL requirements.
func recoveryBinaryJSON(raw []byte) bool {
	if !recoveryBinaryText(raw) {
		return false
	}
	depth := 0
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '"':
			closed := false
			for i++; i < len(raw); i++ {
				if raw[i] == '"' {
					closed = true
					break
				}
				if raw[i] != '\\' {
					continue
				}
				i++
				if i >= len(raw) {
					return false
				}
				if raw[i] == 'u' {
					code, ok := recoveryBinaryHex4(raw, i+1)
					if !ok || code == 0 || (code >= 0xdc00 && code <= 0xdfff) {
						return false
					}
					i += 4
					if code >= 0xd800 && code <= 0xdbff {
						if i+2 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
							return false
						}
						low, ok := recoveryBinaryHex4(raw, i+3)
						if !ok || low < 0xdc00 || low > 0xdfff {
							return false
						}
						i += 6
					}
				}
			}
			if !closed {
				return false
			}
		case '{', '[':
			depth++
			if depth > 256 {
				return false
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return false
			}
		default:
			if raw[i] == '-' || (raw[i] >= '0' && raw[i] <= '9') {
				for i++; i < len(raw); i++ {
					c := raw[i]
					if c == 'e' || c == 'E' {
						return false
					}
					if (c < '0' || c > '9') && c != '.' && c != '-' && c != '+' {
						i--
						break
					}
				}
			}
		}
	}
	return depth == 0 && json.Valid(raw)
}

func recoveryBinaryHex4(raw []byte, start int) (uint16, bool) {
	if start < 0 || len(raw)-start < 4 {
		return 0, false
	}
	var value uint16
	for _, c := range raw[start : start+4] {
		value <<= 4
		switch {
		case c >= '0' && c <= '9':
			value |= uint16(c - '0')
		case c >= 'a' && c <= 'f':
			value |= uint16(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			value |= uint16(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return value, true
}
