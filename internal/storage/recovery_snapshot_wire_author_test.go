// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

func recoveryAuthorCopy(rows [][][]byte) []byte {
	var b bytes.Buffer
	b.WriteString("PGCOPY\n\xff\r\n\x00")
	_ = binary.Write(&b, binary.BigEndian, int32(0))
	_ = binary.Write(&b, binary.BigEndian, int32(0))
	for _, row := range rows {
		_ = binary.Write(&b, binary.BigEndian, int16(len(row)))
		for _, field := range row {
			size := int32(len(field))
			if field == nil {
				size = -1
			}
			_ = binary.Write(&b, binary.BigEndian, size)
			if field != nil {
				b.Write(field)
			}
		}
	}
	_ = binary.Write(&b, binary.BigEndian, int16(-1))
	return b.Bytes()
}
func TestRecoverySnapshotCopyBoundariesAndLimits(t *testing.T) {
	raw := recoveryAuthorCopy([][][]byte{{[]byte("review \xe2\x9c\x93\n"), {}}, {nil, {0, 255, 1}}})
	for _, step := range []int{1, 2, 3, 7, 19, 32, len(raw)} {
		t.Run(string(rune('A'+step%26)), func(t *testing.T) {
			p := newRecoverySnapshotCopyParser(2, 2, 128)
			for i := 0; i < len(raw); i += step {
				if err := p.feed(raw[i:min(len(raw), i+step)]); err != nil {
					t.Fatal("valid fragmented COPY rejected", err)
				}
			}
			if p.finish() != nil || p.rows != 2 {
				t.Fatal("row count or trailer lost")
			}
		})
	}
	cases := map[string]func([]byte) []byte{
		"signature":             func(v []byte) []byte { v[0] = 'X'; return v },
		"flags":                 func(v []byte) []byte { v[14] = 1; return v },
		"header_extension":      func(v []byte) []byte { v[18] = 1; return v },
		"column_count":          func(v []byte) []byte { v[20] = 3; return v },
		"negative_field_length": func(v []byte) []byte { binary.BigEndian.PutUint32(v[21:25], 0xfffffffe); return v },
		"missing_trailer":       func(v []byte) []byte { return v[:len(v)-2] },
		"truncated_field":       func(v []byte) []byte { return v[:28] },
		"trailing_binary":       func(v []byte) []byte { return append(v, 0) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			p := newRecoverySnapshotCopyParser(2, 2, 128)
			err := p.feed(change(append([]byte{}, raw...)))
			if err == nil {
				err = p.finish()
			}
			if !errors.Is(err, ErrRecoverySnapshotInvalid) {
				t.Fatal("malformed COPY accepted", err)
			}
		})
	}
	t.Run("row_limit", func(t *testing.T) {
		p := newRecoverySnapshotCopyParser(2, 1, 128)
		if !errors.Is(p.feed(raw), ErrRecoverySnapshotLimit) {
			t.Fatal("row bound ignored")
		}
	})
	t.Run("field_allocation_bound", func(t *testing.T) {
		p := newRecoverySnapshotCopyParser(2, 2, 32)
		head := append([]byte{}, raw[:25]...)
		binary.BigEndian.PutUint32(head[21:25], 1<<30)
		if !errors.Is(p.feed(head), ErrRecoverySnapshotLimit) {
			t.Fatal("huge declared field accepted before body")
		}
	})
}
func TestRecoverySnapshotStrictWireObjects(t *testing.T) {
	good := recoverySnapshotTableEnd{Kind: "table_end", Summary: RecoverySnapshotTableSummary{Name: "artifacts", Rows: 0, Bytes: 21, SHA256: Hash(nil)}}
	body, _ := json.Marshal(good)
	body = append(body, '\n')
	var got recoverySnapshotTableEnd
	if recoverySnapshotDecode(body, &got) != nil {
		t.Fatal("valid record rejected")
	}
	cases := map[string][]byte{
		"duplicate":                 bytes.Replace(body, []byte(`"kind":"table_end"`), []byte(`"kind":"table_end","kind":"chunk"`), 1),
		"escaped_duplicate":         bytes.Replace(body, []byte(`"kind":"table_end"`), []byte(`"kind":"table_end","\u006bind":"table_end"`), 1),
		"unknown":                   bytes.Replace(body, []byte(`"name":"artifacts"`), []byte(`"name":"artifacts","sql":"DROP DATABASE ignored"`), 1),
		"missing_nested_zero_field": bytes.Replace(body, []byte(`"rows":"0",`), nil, 1),
		"null":                      bytes.Replace(body, []byte(`"name":"artifacts"`), []byte(`"name":null`), 1),
		"leading_zero_number":       bytes.Replace(body, []byte(`"rows":"0"`), []byte(`"rows":"00"`), 1),
		"negative_zero_number":      bytes.Replace(body, []byte(`"rows":"0"`), []byte(`"rows":"-0"`), 1),
		"positive_sign_number":      bytes.Replace(body, []byte(`"rows":"0"`), []byte(`"rows":"+0"`), 1),
		"wrong_number_wire":         bytes.Replace(body, []byte(`"rows":"0"`), []byte(`"rows":0`), 1),
		"unpaired_surrogate":        bytes.Replace(body, []byte(`"name":"artifacts"`), []byte(`"name":"\ud800"`), 1),
		"extra_json":                append(append([]byte{}, body...), []byte("{}\n")...),
		"missing_newline":           body[:len(body)-1],
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var result recoverySnapshotTableEnd
			if recoverySnapshotDecode(raw, &result) == nil {
				t.Fatal("malformed wire object accepted")
			}
		})
	}
}

type recoveryAuthorBrokenWriter struct{ short bool }

func (w recoveryAuthorBrokenWriter) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	return 0, errors.New("private sink failure details")
}
func TestRecoverySnapshotOutputFailureAndBounds(t *testing.T) {
	t.Run("short_write", func(t *testing.T) {
		w := recoverySnapshotWriter{ctx: context.Background(), out: recoveryAuthorBrokenWriter{short: true}, hash: sha256.New(), limit: 1000}
		if !errors.Is(w.record(recoverySnapshotChunk{Kind: "chunk", Data: "x"}), ErrRecoverySnapshotIO) {
			t.Fatal("short write accepted")
		}
	})
	t.Run("sink_details_not_exposed", func(t *testing.T) {
		w := recoverySnapshotWriter{ctx: context.Background(), out: recoveryAuthorBrokenWriter{}, hash: sha256.New(), limit: 1000}
		if err := w.record(recoverySnapshotChunk{Kind: "chunk", Data: "x"}); err != ErrRecoverySnapshotIO {
			t.Fatal("sink error escaped fixed boundary")
		}
	})
	t.Run("wire_bound_before_write", func(t *testing.T) {
		var b bytes.Buffer
		w := recoverySnapshotWriter{ctx: context.Background(), out: &b, hash: sha256.New(), limit: 5}
		if !errors.Is(w.record(recoverySnapshotChunk{Kind: "chunk", Data: "x"}), ErrRecoverySnapshotLimit) || b.Len() != 0 {
			t.Fatal("oversize record emitted")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		w := recoverySnapshotWriter{ctx: ctx, out: io.Discard, hash: sha256.New(), limit: 1000}
		if !errors.Is(w.record(recoverySnapshotChunk{}), context.Canceled) {
			t.Fatal("cancel ignored")
		}
	})
	t.Run("truncated_record", func(t *testing.T) {
		if _, err := ValidateRecoverySnapshot(context.Background(), bytes.NewReader([]byte("{")), DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoverySnapshotInvalid) {
			t.Fatal("truncation accepted", err)
		}
	})
	t.Run("max_record", func(t *testing.T) {
		if _, err := ValidateRecoverySnapshot(context.Background(), bytes.NewReader(bytes.Repeat([]byte("x"), recoverySnapshotMaxRecordBytes+1)), DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoverySnapshotLimit) {
			t.Fatal("record bound ignored", err)
		}
	})
}
