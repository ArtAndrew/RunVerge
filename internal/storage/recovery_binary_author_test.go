// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

// Author tests of this leaf only: no PG, model, network or recovery acceptance.
func binaryAuthorTable(oids ...uint32) RecoverySnapshotTable {
	t := RecoverySnapshotTable{Name: "binary_author"}
	for i, oid := range oids {
		t.Columns = append(t.Columns, RecoverySnapshotColumn{Name: fmt.Sprintf("field%d", i), TypeOID: oid, NotNull: true})
	}
	return t
}

func binaryAuthorCopy(rows ...[][]byte) []byte {
	raw := []byte("PGCOPY\n\xff\r\n\x00\x00\x00\x00\x00\x00\x00\x00\x00")
	for _, row := range rows {
		raw = binary.BigEndian.AppendUint16(raw, uint16(len(row)))
		for _, value := range row {
			if value == nil {
				raw = binary.BigEndian.AppendUint32(raw, math.MaxUint32)
			} else {
				raw = binary.BigEndian.AppendUint32(raw, uint32(len(value)))
				raw = append(raw, value...)
			}
		}
	}
	return append(raw, 255, 255)
}

func binaryAuthorInt64(value int64) []byte { return binary.BigEndian.AppendUint64(nil, uint64(value)) }

func binaryAuthorArray(flags int32, dimensions [][2]int32, values ...[]byte) []byte {
	raw := binary.BigEndian.AppendUint32(nil, uint32(len(dimensions)))
	raw = binary.BigEndian.AppendUint32(raw, uint32(flags))
	raw = binary.BigEndian.AppendUint32(raw, 25)
	for _, d := range dimensions {
		raw = binary.BigEndian.AppendUint32(raw, uint32(d[0]))
		raw = binary.BigEndian.AppendUint32(raw, uint32(d[1]))
	}
	for _, value := range values {
		if value == nil {
			raw = binary.BigEndian.AppendUint32(raw, math.MaxUint32)
		} else {
			raw = binary.BigEndian.AppendUint32(raw, uint32(len(value)))
			raw = append(raw, value...)
		}
	}
	return raw
}

type binaryAuthorChunked struct {
	reader io.Reader
	step   int
}

func (r binaryAuthorChunked) Read(p []byte) (int, error) {
	return r.reader.Read(p[:min(len(p), r.step)])
}

func TestRecoveryBinaryAuthorExactEightTypesAndFragmentation(t *testing.T) {
	table := binaryAuthorTable(16, 17, 20, 23, 25, 1009, 1184, 3802)
	row := [][]byte{{1}, {0, 255, 17}, binaryAuthorInt64(math.MaxInt64), {128, 0, 0, 0}, []byte("exact UTF8 \u2713\n"),
		binaryAuthorArray(1, [][2]int32{{2, -3}}, []byte("one"), nil), binaryAuthorInt64(math.MinInt64),
		append([]byte{1}, []byte(`{"s":"1e999999","pair":"\uD83D\uDE00","n":1.25}`)...)}
	nulls := make([][]byte, len(table.Columns)) // PG, not this leaf, enforces NN.
	raw := binaryAuthorCopy(row, nulls)
	for _, step := range []int{1, 2, 3, 7, 19, 64, len(raw)} {
		t.Run(fmt.Sprint(step), func(t *testing.T) {
			r := newRecoveryStageTypedReader(context.Background(), binaryAuthorChunked{bytes.NewReader(raw), step}, table, DefaultRecoverySnapshotLimits())
			observed, err := io.ReadAll(r)
			if err != nil || r.Err() != nil || !bytes.Equal(observed, raw) {
				t.Fatal("valid exact bytes changed", err, r.Err())
			}
			if n, err := r.Read(make([]byte, 1)); n != 0 || err != io.EOF || r.Err() != nil {
				t.Fatal("EOF was not clean/sticky", n, err)
			}
			if err := r.Stop(); err != nil {
				t.Fatal("clean EOF stop invented an error", err)
			}
		})
	}
	for name, array := range map[string][]byte{
		"canonical_empty": binaryAuthorArray(0, nil),
		"legacy_lower_bounds": binaryAuthorArray(1, [][2]int32{{2, -7}, {2, 3}},
			[]byte("a"), nil, []byte(""), []byte("\u4e8c")),
	} {
		t.Run(name, func(t *testing.T) {
			raw := binaryAuthorCopy([][]byte{array})
			r := newRecoveryStageTypedReader(context.Background(), bytes.NewReader(raw), binaryAuthorTable(1009), DefaultRecoverySnapshotLimits())
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatal("valid source array changed", err)
			}
		})
	}
}

func TestRecoveryBinaryAuthorRejectsMalformedBuiltinFields(t *testing.T) {
	cases := []struct {
		name string
		oid  uint32
		raw  []byte
	}{
		{"bool_empty", 16, []byte{}}, {"bool_two", 16, []byte{2}}, {"bool_wide", 16, []byte{0, 0}},
		{"int8_short", 20, make([]byte, 7)}, {"int4_wide", 23, make([]byte, 5)}, {"timestamp_short", 1184, make([]byte, 7)},
		{"invalid_utf8", 25, []byte{255}}, {"text_nul", 25, []byte{'a', 0}}, {"unknown_oid", 99999, []byte("never dispatch")},
		{"jsonb_version", 3802, []byte{2, '{', '}'}}, {"jsonb_empty", 3802, []byte{1}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			r := newRecoveryStageTypedReader(context.Background(), bytes.NewReader(binaryAuthorCopy([][]byte{item.raw})), binaryAuthorTable(item.oid), DefaultRecoverySnapshotLimits())
			out, err := io.ReadAll(r)
			if err != ErrRecoveryBinaryValue || r.Err() != err || len(out) > 19 {
				t.Fatal("bad field escaped row boundary", len(out), err)
			}
		})
	}
}

func TestRecoveryBinaryAuthorJSONUnicodeDepthAndNoExpansion(t *testing.T) {
	valid := []string{`true`, `false`, `null`, `1.00`, `-0.25`, `{"e":"1e100000","escaped":"\\ud800"}`, `"\uD800\uDC00"`, `"\u0020"`, strings.Repeat("[", 256) + "0" + strings.Repeat("]", 256)}
	for i, value := range valid {
		t.Run(fmt.Sprintf("valid_%d", i), func(t *testing.T) {
			if !recoveryBinaryField(3802, append([]byte{1}, value...)) {
				t.Fatal("valid bounded canonical-profile JSON rejected")
			}
		})
	}
	invalid := []string{``, `{`, `{} true`, `1e100000`, `1E-100000`, `"\u0000"`, `"\ud800"`, `"\udc00"`, `"\ud800\u0020"`, `"\ud800x\udc00"`, `"\u00xz"`, `"bad\q"`, string([]byte{34, 255, 34}), strings.Repeat("[", 257) + "0" + strings.Repeat("]", 257)}
	for i, value := range invalid {
		t.Run(fmt.Sprintf("invalid_%d", i), func(t *testing.T) {
			if recoveryBinaryField(3802, append([]byte{1}, value...)) {
				t.Fatal("invalid/expanding JSON reached PG")
			}
		})
	}
}

func TestRecoveryBinaryAuthorArrayAllocationTrapAndMalformedArrays(t *testing.T) {
	trap := binaryAuthorArray(0, [][2]int32{{20_000_000, 1}}) // Exactly20 bytes, no elements.
	if len(trap) != 20 || recoveryBinaryTextArray(trap) {
		t.Fatal("declared huge array was not rejected without PG")
	}
	if allocations := testing.AllocsPerRun(100, func() {
		if recoveryBinaryTextArray(trap) {
			panic("huge array accepted")
		}
	}); allocations != 0 {
		t.Fatal("array header trap allocated from claimed element count", allocations)
	}
	base := binaryAuthorArray(0, [][2]int32{{1, 1}}, []byte("a"))
	badFlags, badOID := bytes.Clone(base), bytes.Clone(base)
	binary.BigEndian.PutUint32(badFlags[4:], 2)
	binary.BigEndian.PutUint32(badOID[8:], 23)
	for name, value := range map[string][]byte{
		"allocation_trap": trap, "short_header": base[:11], "flags": badFlags, "element_oid": badOID,
		"seven_dimensions":     binaryAuthorArray(0, make([][2]int32, 7)),
		"negative_dimension":   binaryAuthorArray(0, [][2]int32{{-1, 1}}),
		"product_overflow":     binaryAuthorArray(0, [][2]int32{{1 << 30, 0}, {2, 0}}),
		"upper_bound_overflow": binaryAuthorArray(0, [][2]int32{{1, math.MaxInt32}}, []byte("a")),
		"invalid_utf8":         binaryAuthorArray(0, [][2]int32{{1, 1}}, []byte{255}),
		"nul":                  binaryAuthorArray(0, [][2]int32{{1, 1}}, []byte{0}),
		"truncated_element":    base[:len(base)-1], "trailing": append(bytes.Clone(base), 0),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRecoveryStageTypedReader(context.Background(), bytes.NewReader(binaryAuthorCopy([][]byte{value})), binaryAuthorTable(1009), DefaultRecoverySnapshotLimits())
			out, err := io.ReadAll(r)
			if err != ErrRecoveryBinaryValue || len(out) != 19 {
				t.Fatal("bad array bytes reached PG", len(out), err)
			}
		})
	}
}

func TestRecoveryBinaryAuthorWholeRowHeldUntilLastFieldAndStickyError(t *testing.T) {
	secret := []byte("must-not-reach-pg-or-errors")
	r := newRecoveryStageTypedReader(context.Background(), bytes.NewReader(binaryAuthorCopy([][]byte{secret, {2}})), binaryAuthorTable(25, 16), DefaultRecoverySnapshotLimits())
	out, err := io.ReadAll(r)
	if err != ErrRecoveryBinaryValue || len(out) != 19 || bytes.Contains(out, secret) || strings.Contains(err.Error(), string(secret)) {
		t.Fatal("part of an invalid row escaped", len(out), err)
	}
	if n, next := r.Read(make([]byte, 100)); n != 0 || next != err || r.Err() != err {
		t.Fatal("failure was not sticky", n, next)
	}
}

func TestRecoveryBinaryAuthorFramingLimitsAndExactEOF(t *testing.T) {
	valid := binaryAuthorCopy([][]byte{[]byte("a")})
	for cut := 0; cut < len(valid); cut++ {
		t.Run(fmt.Sprintf("truncated_%d", cut), func(t *testing.T) {
			r := newRecoveryStageTypedReader(context.Background(), bytes.NewReader(valid[:cut]), binaryAuthorTable(25), DefaultRecoverySnapshotLimits())
			if _, err := io.ReadAll(r); err != ErrRecoveryBinaryValue {
				t.Fatal("truncated COPY accepted", err)
			}
		})
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"magic":                 func(b []byte) []byte { b[0] ^= 1; return b },
		"flags":                 func(b []byte) []byte { b[14] = 1; return b },
		"extension":             func(b []byte) []byte { b[18] = 1; return b },
		"columns":               func(b []byte) []byte { b[20] = 2; return b },
		"negative_field_length": func(b []byte) []byte { binary.BigEndian.PutUint32(b[21:], math.MaxUint32-1); return b },
		"trailing":              func(b []byte) []byte { return append(b, 0) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRecoveryStageTypedReader(context.Background(), bytes.NewReader(mutate(bytes.Clone(valid))), binaryAuthorTable(25), DefaultRecoverySnapshotLimits())
			if _, err := io.ReadAll(r); err != ErrRecoveryBinaryValue {
				t.Fatal("noncanonical frame accepted", err)
			}
		})
	}
	for _, kind := range []string{"row", "table", "total", "count"} {
		t.Run(kind, func(t *testing.T) {
			limits := DefaultRecoverySnapshotLimits()
			raw := valid
			switch kind {
			case "row":
				limits.MaxRowBytes = 32
				raw = binaryAuthorCopy([][]byte{bytes.Repeat([]byte("x"), 27)})
			case "table":
				limits.MaxTableBytes = int64(len(valid) - 1)
			case "total":
				limits.MaxTotalBytes = int64(len(valid) - 1)
			case "count":
				limits.MaxRows = 0
			}
			r := newRecoveryStageTypedReader(context.Background(), bytes.NewReader(raw), binaryAuthorTable(25), limits)
			if _, err := io.ReadAll(r); err != ErrRecoveryBinaryValue {
				t.Fatal("limit was ignored", err)
			}
		})
	}
	limits := DefaultRecoverySnapshotLimits()
	limits.MaxRows, limits.MaxRowsPerTable = 0, 0
	r := newRecoveryStageTypedReader(context.Background(), bytes.NewReader(binaryAuthorCopy()), binaryAuthorTable(25), limits)
	if _, err := io.ReadAll(r); err != nil || r.Err() != nil {
		t.Fatal("zero remaining rows should still permit an empty table", err)
	}
}

type binaryAuthorBrokenReader struct{ calls int }

func (r *binaryAuthorBrokenReader) Read([]byte) (int, error) { r.calls++; return 0, nil }

type binaryAuthorTailError struct{ io.Reader }

func (r binaryAuthorTailError) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		return n, errors.New("private-sentinel-reader-secret")
	}
	return n, err
}

type binaryAuthorCancelReader struct {
	io.Reader
	cancel context.CancelFunc
}

func (r binaryAuthorCancelReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.cancel()
	return n, err
}

func TestRecoveryBinaryAuthorNoProgressTransportAndCancellation(t *testing.T) {
	stalled := &binaryAuthorBrokenReader{}
	r := newRecoveryStageTypedReader(context.Background(), stalled, binaryAuthorTable(25), DefaultRecoverySnapshotLimits())
	if _, err := io.ReadAll(r); err != ErrRecoveryBinaryValue || stalled.calls != 100 {
		t.Fatal("no-progress reader did not stop finitely", stalled.calls, err)
	}
	valid := binaryAuthorCopy([][]byte{[]byte("a")})
	r = newRecoveryStageTypedReader(context.Background(), binaryAuthorTailError{bytes.NewReader(valid)}, binaryAuthorTable(25), DefaultRecoverySnapshotLimits())
	out, err := io.ReadAll(r)
	if err != ErrRecoveryBinaryValue || strings.Contains(err.Error(), "private-sentinel") || len(out) != len(valid)-2 {
		t.Fatal("non-EOF transport failure accepted or disclosed", len(out), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r = newRecoveryStageTypedReader(ctx, binaryAuthorCancelReader{bytes.NewReader(valid), cancel}, binaryAuthorTable(25), DefaultRecoverySnapshotLimits())
	if out, err := io.ReadAll(r); err != context.Canceled || len(out) != 0 || r.Err() != context.Canceled {
		t.Fatal("context cancellation did not fence bytes", len(out), err)
	}
}

type binaryAuthorGatedReader struct {
	io.Reader
	entered chan struct{}
	release chan struct{}
	calls   int
}

func (r *binaryAuthorGatedReader) Read(p []byte) (int, error) {
	r.calls++
	if r.calls == 2 { // The exact header has been emitted; row input is held.
		close(r.entered)
		<-r.release
	}
	return r.Reader.Read(p)
}

func TestRecoveryBinaryAuthorConcurrentStopDetachesBorrowedReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := &binaryAuthorGatedReader{Reader: bytes.NewReader(binaryAuthorCopy([][]byte{[]byte("borrowed archive")})), entered: make(chan struct{}), release: make(chan struct{})}
	r := newRecoveryStageTypedReader(ctx, input, binaryAuthorTable(25), DefaultRecoverySnapshotLimits())
	readDone := make(chan error, 1)
	go func() { _, err := io.ReadAll(r); readDone <- err }()
	select {
	case <-input.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture did not enter held row read")
	}
	stopDone, errDone := make(chan error, 1), make(chan error, 1)
	go func() { stopDone <- r.Stop() }()
	go func() { errDone <- r.Err() }()
	select {
	case <-stopDone:
		t.Fatal("Stop returned while the raw reader was still borrowed")
	default:
	}
	cancel()
	close(input.release)
	for name, done := range map[string]<-chan error{"Read": readDone, "Stop": stopDone, "Err": errDone} {
		select {
		case err := <-done:
			if err != context.Canceled {
				t.Fatal(name, "lost the final read error", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal(name, "did not finish")
		}
	}
	calls := input.calls
	for i := 0; i < 10; i++ {
		if _, err := r.Read(make([]byte, 1)); err != context.Canceled {
			t.Fatal("stopped failed reader lost error", err)
		}
	}
	if input.calls != calls {
		t.Fatal("a stopped reader touched the released borrowed input")
	}
	// Stop before any read is also a finite detach, not proof of completed COPY.
	unread := &binaryAuthorBrokenReader{}
	stopped := newRecoveryStageTypedReader(context.Background(), unread, binaryAuthorTable(25), DefaultRecoverySnapshotLimits())
	if stopped.Stop() != nil || stopped.Err() != nil {
		t.Fatal("deliberate stop invented a value error")
	}
	if n, err := stopped.Read(make([]byte, 1)); n != 0 || err != io.EOF || unread.calls != 0 {
		t.Fatal("stopped unused reader accessed input", n, err)
	}
}
