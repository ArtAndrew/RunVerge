// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

// The independent fixtures below encode the documented PostgreSQL COPY binary
// grammar directly. They do not call ExportRecoverySnapshot, the snapshot
// writer, or the author's fixture builders.
func independentRecoveryCopy(rows ...[][]byte) []byte {
	raw := []byte{'P', 'G', 'C', 'O', 'P', 'Y', '\n', 255, '\r', '\n', 0, 0, 0, 0, 0, 0, 0, 0, 0}
	for _, row := range rows {
		raw = binary.BigEndian.AppendUint16(raw, uint16(len(row)))
		for _, field := range row {
			if field == nil {
				raw = binary.BigEndian.AppendUint32(raw, math.MaxUint32)
				continue
			}
			raw = binary.BigEndian.AppendUint32(raw, uint32(len(field)))
			raw = append(raw, field...)
		}
	}
	return binary.BigEndian.AppendUint16(raw, math.MaxUint16)
}

func independentRecoveryParse(raw []byte, columns int, rows, rowBytes int64) error {
	p := newRecoverySnapshotCopyParser(columns, rows, rowBytes)
	if err := p.feed(raw); err != nil {
		return err
	}
	return p.finish()
}

func TestRecoverySnapshotIndependentCOPYEveryPrefix(t *testing.T) {
	raw := independentRecoveryCopy([][]byte{[]byte("alpha"), nil}, [][]byte{{0, 1, 255}, {}})
	if err := independentRecoveryParse(raw, 2, 2, 64); err != nil {
		t.Fatal("independent valid fixture rejected", err)
	}
	for end := 0; end < len(raw); end++ {
		if err := independentRecoveryParse(raw[:end], 2, 2, 64); !errors.Is(err, ErrRecoverySnapshotInvalid) {
			t.Fatalf("truncated prefix %d/%d: expected invalid, got %v", end, len(raw), err)
		}
	}
	// Every possible two-piece split exercises transitions inside the COPY
	// header, signed lengths, opaque values and terminal marker.
	for split := 0; split <= len(raw); split++ {
		p := newRecoverySnapshotCopyParser(2, 2, 64)
		if err := p.feed(raw[:split]); err != nil {
			t.Fatalf("valid first fragment at %d: %v", split, err)
		}
		if err := p.feed(raw[split:]); err != nil || p.finish() != nil || p.rows != 2 {
			t.Fatalf("valid second fragment at %d failed: %v", split, err)
		}
	}
}

func TestRecoverySnapshotIndependentCOPYTerminalAndBounds(t *testing.T) {
	base := independentRecoveryCopy([][]byte{[]byte("value")})
	cases := []struct {
		name string
		raw  []byte
		rows int64
		max  int64
		want error
	}{
		{"second_stream", append(append([]byte{}, base...), independentRecoveryCopy()...), 1, 64, ErrRecoverySnapshotInvalid},
		{"trailing_marker", append(append([]byte{}, base...), 255, 255), 1, 64, ErrRecoverySnapshotInvalid},
		{"trailing_zero", append(append([]byte{}, base...), 0), 1, 64, ErrRecoverySnapshotInvalid},
		{"zero_row_cap", base, 0, 64, ErrRecoverySnapshotLimit},
		{"row_including_overhead_over_cap", base, 1, 10, ErrRecoverySnapshotLimit},
		{"row_including_overhead_at_cap", base, 1, 11, nil},
		{"empty_at_zero_rows", independentRecoveryCopy(), 0, 64, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := independentRecoveryParse(tc.raw, 1, tc.rows, tc.max); !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
		})
	}
	for _, size := range []uint32{0x7fffffff, 0x80000000, 0xfffffffe} {
		bad := append([]byte{}, base...)
		binary.BigEndian.PutUint32(bad[21:25], size)
		if err := independentRecoveryParse(bad, 1, 1, 64); err == nil {
			t.Fatalf("unbounded or negative field size %x accepted", size)
		}
	}
}

func TestRecoverySnapshotIndependentFramingIsNotSemanticValidation(t *testing.T) {
	// These are deliberately invalid for a hypothetical table with a NOT NULL
	// integer primary key. Framing has no field types or values and must never
	// be treated as proof of relational validity or permission to import/resume.
	for name, raw := range map[string][]byte{
		"null_not_null_key":  independentRecoveryCopy([][]byte{nil}),
		"invalid_int4_width": independentRecoveryCopy([][]byte{{1, 2, 3}}),
		"duplicate_primary_key": independentRecoveryCopy(
			[][]byte{{0, 0, 0, 7}}, [][]byte{{0, 0, 0, 7}}),
		"unsorted_primary_key": independentRecoveryCopy(
			[][]byte{{0, 0, 0, 8}}, [][]byte{{0, 0, 0, 7}}),
	} {
		t.Run(name, func(t *testing.T) {
			if err := independentRecoveryParse(raw, 1, 2, 64); err != nil {
				t.Fatalf("framing boundary changed; review semantic importer coverage: %v", err)
			}
		})
	}
}

type independentRecoveryReadError struct{}

func (independentRecoveryReadError) Read([]byte) (int, error) {
	return 0, errors.New("synthetic private connection details must not escape")
}

func TestRecoverySnapshotIndependentReadBoundaries(t *testing.T) {
	for name, tc := range map[string]struct {
		input io.Reader
		limit int64
		want  error
	}{
		"missing_newline":    {strings.NewReader(`{"kind":"table","name":"events"}`), 1024, ErrRecoverySnapshotInvalid},
		"empty":              {strings.NewReader(""), 1024, ErrRecoverySnapshotInvalid},
		"private_read_error": {independentRecoveryReadError{}, 1024, ErrRecoverySnapshotIO},
		"line_limit":         {strings.NewReader(strings.Repeat("x", 32) + "\n"), 32, ErrRecoverySnapshotLimit},
		"record_limit":       {strings.NewReader(strings.Repeat("x", (2<<20)) + "\n"), 4 << 20, ErrRecoverySnapshotLimit},
	} {
		t.Run(name, func(t *testing.T) {
			r := recoverySnapshotReader{ctx: context.Background(), reader: bufio.NewReaderSize(tc.input, 17), hash: sha256.New(), limit: tc.limit}
			if _, err := r.line(); !errors.Is(err, tc.want) {
				t.Fatalf("expected fixed %v, got %v", tc.want, err)
			}
		})
	}
	t.Run("cancelled_before_read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r := recoverySnapshotReader{ctx: ctx, reader: bufio.NewReader(independentRecoveryReadError{}), hash: sha256.New(), limit: 1024}
		if _, err := r.line(); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled input read unexpectedly: %v", err)
		}
	})
	t.Run("completed_line_then_error", func(t *testing.T) {
		r := recoverySnapshotReader{ctx: context.Background(), reader: bufio.NewReader(io.MultiReader(strings.NewReader("{}\n"), independentRecoveryReadError{})), hash: sha256.New(), limit: 1024}
		raw, err := r.line()
		if err != nil || !bytes.Equal(raw, []byte("{}\n")) {
			t.Fatal("read boundary fixture failed")
		}
		r.accept(raw)
		if _, err = r.line(); !errors.Is(err, ErrRecoverySnapshotIO) {
			t.Fatalf("post-line input error disappeared: %v", err)
		}
	})
}

func TestRecoverySnapshotIndependentStateIdentityAndMonotonicity(t *testing.T) {
	inventory := RecoverySnapshotInventory{Sequences: []RecoverySnapshotSequenceDefinition{{Name: "test_sequence"}}}
	valid := func() recoverySnapshotState {
		return recoverySnapshotState{
			Kind: "state", Sequences: []RecoverySnapshotSequence{{Name: "test_sequence", ObservedLast: 10, IsCalled: true, IncludedMaximum: 11, NextMinimum: 12}},
			KeyReferences: []RecoverySnapshotKeyReference{{Kind: "model", OrganizationID: "org_test", ID: "model_test", Version: 1, Environment: "PLATFORM_MODEL_KEY_TEST"}},
			Organizations: []RecoverySnapshotOrganization{{OrganizationID: "org_test", EventHead: 11, RetainedAfterMaximum: 4}},
		}
	}
	limits := DefaultRecoverySnapshotLimits()
	if err := recoverySnapshotValidateState(valid(), inventory, limits); err != nil {
		t.Fatal("valid independently constructed state rejected", err)
	}
	cases := map[string]func(*recoverySnapshotState){
		"wrong_kind":              func(s *recoverySnapshotState) { s.Kind = "complete" },
		"missing_sequence":        func(s *recoverySnapshotState) { s.Sequences = []RecoverySnapshotSequence{} },
		"duplicate_sequence":      func(s *recoverySnapshotState) { s.Sequences = append(s.Sequences, s.Sequences[0]) },
		"wrong_sequence_identity": func(s *recoverySnapshotState) { s.Sequences[0].Name = "other" },
		"below_included":          func(s *recoverySnapshotState) { s.Sequences[0].NextMinimum = 11 },
		"nonminimal_next":         func(s *recoverySnapshotState) { s.Sequences[0].NextMinimum = 13 },
		"overflow": func(s *recoverySnapshotState) {
			s.Sequences[0].ObservedLast = math.MaxInt64
			s.Sequences[0].NextMinimum = math.MinInt64
		},
		"negative_included":      func(s *recoverySnapshotState) { s.Sequences[0].IncludedMaximum = -1 },
		"null_references":        func(s *recoverySnapshotState) { s.KeyReferences = nil },
		"duplicate_reference":    func(s *recoverySnapshotState) { s.KeyReferences = append(s.KeyReferences, s.KeyReferences[0]) },
		"zero_reference_version": func(s *recoverySnapshotState) { s.KeyReferences[0].Version = 0 },
		"raw_key_in_reference":   func(s *recoverySnapshotState) { s.KeyReferences[0].Environment = "synthetic-value-never-a-real-key" },
		"path_reference":         func(s *recoverySnapshotState) { s.KeyReferences[0].Environment = "../private" },
		"duplicate_organization": func(s *recoverySnapshotState) { s.Organizations = append(s.Organizations, s.Organizations[0]) },
		"retention_ahead":        func(s *recoverySnapshotState) { s.Organizations[0].RetainedAfterMaximum = 12 },
		"negative_event_head":    func(s *recoverySnapshotState) { s.Organizations[0].EventHead = -1 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := valid()
			change(&s)
			if err := recoverySnapshotValidateState(s, inventory, limits); !errors.Is(err, ErrRecoverySnapshotInvalid) {
				t.Fatalf("malformed state accepted: %v", err)
			}
		})
	}
}

// These independent wire records deliberately do not use the implementation's
// private record types. Only public inventory descriptors and embedded migration
// identities are shared: they are the schema contract, not a serialized export.
type independentRecoveryHeader struct {
	Kind              string                    `json:"kind"`
	Format            string                    `json:"format"`
	Metadata          RecoverySnapshotMetadata  `json:"metadata"`
	APIVersion        string                    `json:"api_version"`
	PostgreSQLVersion int                       `json:"postgresql_version"`
	Encoding          string                    `json:"encoding"`
	MVCCSnapshot      string                    `json:"mvcc_snapshot"`
	Migrations        any                       `json:"migrations"`
	InventorySHA256   string                    `json:"inventory_sha256"`
	Inventory         RecoverySnapshotInventory `json:"inventory"`
}

type independentRecoveryFixture struct {
	header               independentRecoveryHeader
	lines                [][]byte
	starts, chunks, ends []int
	state                int
	rows, dataBytes      int64
}

func independentRecoveryJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal("independent fixture serialization failed", err)
	}
	return append(raw, '\n')
}

func independentRecoveryDigest(v []byte) string {
	h := sha256.Sum256(v)
	return hex.EncodeToString(h[:])
}

func independentRecoveryFixtureNew(t *testing.T) independentRecoveryFixture {
	t.Helper()
	if recoverySnapshotInventorySHA256 == "UNSEALED" {
		t.Fatal("complete snapshot validation requires a reviewed inventory pin")
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "evidence", "recovery-snapshot-export", "trusted-inventory.json"))
	if err != nil {
		t.Fatal("reviewed inventory fixture unavailable", err)
	}
	var inventory RecoverySnapshotInventory
	if json.Unmarshal(raw, &inventory) != nil {
		t.Fatal("reviewed inventory is malformed")
	}
	canonical, err := json.Marshal(inventory)
	if err != nil || independentRecoveryDigest(canonical) != recoverySnapshotInventorySHA256 {
		t.Fatal("reviewed inventory fixture does not match compiled pin")
	}
	f := independentRecoveryFixture{header: independentRecoveryHeader{
		Kind: "header", Format: "runverge-pg18-copy-ndjson-v1",
		Metadata:   RecoverySnapshotMetadata{SourceID: "inst_11111111111111111111111111111111", CutUTC: "2026-10-28T10:00:00Z", BackupID: "bak_22222222222222222222222222222222", SchemaSHA256: EmbeddedSchemaSHA256()},
		APIVersion: domain.ContractVersion, PostgreSQLVersion: 180000, Encoding: "UTF8", MVCCSnapshot: "10:10:",
		Migrations: EmbeddedSchemaIdentity(), InventorySHA256: independentRecoveryDigest(canonical), Inventory: inventory,
	}}
	f.lines = append(f.lines, independentRecoveryJSON(t, f.header))
	for _, table := range inventory.Tables {
		if !table.Exported {
			continue
		}
		f.starts = append(f.starts, len(f.lines))
		f.lines = append(f.lines, independentRecoveryJSON(t, struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		}{"table", table.Name}))
		copyBytes := independentRecoveryCopy()
		f.chunks = append(f.chunks, len(f.lines))
		f.lines = append(f.lines, independentRecoveryJSON(t, struct {
			Kind string `json:"kind"`
			Data string `json:"data"`
		}{"chunk", base64.StdEncoding.EncodeToString(copyBytes)}))
		f.ends = append(f.ends, len(f.lines))
		f.lines = append(f.lines, independentRecoveryJSON(t, struct {
			Kind    string                       `json:"kind"`
			Summary RecoverySnapshotTableSummary `json:"summary"`
		}{"table_end", RecoverySnapshotTableSummary{Name: table.Name, Rows: 0, Bytes: int64(len(copyBytes)), SHA256: independentRecoveryDigest(copyBytes)}}))
		f.dataBytes += int64(len(copyBytes))
	}
	sequences := []RecoverySnapshotSequence{}
	for _, s := range inventory.Sequences {
		sequences = append(sequences, RecoverySnapshotSequence{Name: s.Name, ObservedLast: 1, IsCalled: false, IncludedMaximum: 0, NextMinimum: 2})
	}
	f.state = len(f.lines)
	f.lines = append(f.lines, independentRecoveryJSON(t, struct {
		Kind          string                         `json:"kind"`
		Sequences     []RecoverySnapshotSequence     `json:"sequences"`
		KeyReferences []RecoverySnapshotKeyReference `json:"key_references"`
		Organizations []RecoverySnapshotOrganization `json:"organizations"`
	}{"state", sequences, []RecoverySnapshotKeyReference{}, []RecoverySnapshotOrganization{}}))
	return f
}

func (f independentRecoveryFixture) bytes(t *testing.T) []byte {
	t.Helper()
	body := bytes.Join(f.lines, nil)
	// Recompute the outer footer after mutations. This prevents a stale wire
	// checksum from masking a missing inner framing/inventory validation check.
	footer := independentRecoveryJSON(t, struct {
		Kind               string `json:"kind"`
		Tables             int    `json:"tables"`
		Rows               int64  `json:"rows,string"`
		Bytes              int64  `json:"bytes,string"`
		PrecedingWireBytes int64  `json:"preceding_wire_bytes,string"`
		PrecedingSHA256    string `json:"preceding_sha256"`
	}{"complete", len(f.starts), f.rows, f.dataBytes, int64(len(body)), independentRecoveryDigest(body)})
	return append(body, footer...)
}

func TestRecoverySnapshotIndependentCompleteStream(t *testing.T) {
	f := independentRecoveryFixtureNew(t)
	raw := f.bytes(t)
	report, err := ValidateRecoverySnapshot(context.Background(), bytes.NewReader(raw), DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal("independently assembled complete stream rejected", err)
	}
	if report.Rows != 0 || report.Bytes != f.dataBytes || report.WireBytes != int64(len(raw)) || report.SHA256 != independentRecoveryDigest(raw) || len(report.Tables) != len(f.starts) {
		t.Fatal("independent counts/hash/table inventory mismatch")
	}
	if report.Metadata != f.header.Metadata {
		t.Fatal("source/backup/cut/schema identity lost")
	}
}

func TestRecoverySnapshotIndependentFullStreamTamper(t *testing.T) {
	cases := map[string]func(*independentRecoveryFixture){
		"wrong_format": func(f *independentRecoveryFixture) {
			f.header.Format = "unknown"
			f.lines[0] = independentRecoveryJSON(t, f.header)
		},
		"wrong_api": func(f *independentRecoveryFixture) {
			f.header.APIVersion = "0.0.0"
			f.lines[0] = independentRecoveryJSON(t, f.header)
		},
		"wrong_pg": func(f *independentRecoveryFixture) {
			f.header.PostgreSQLVersion = 170000
			f.lines[0] = independentRecoveryJSON(t, f.header)
		},
		"wrong_encoding": func(f *independentRecoveryFixture) {
			f.header.Encoding = "LATIN1"
			f.lines[0] = independentRecoveryJSON(t, f.header)
		},
		"wrong_schema_identity": func(f *independentRecoveryFixture) {
			f.header.Metadata.SchemaSHA256 = strings.Repeat("0", 64)
			f.lines[0] = independentRecoveryJSON(t, f.header)
		},
		"bad_cut": func(f *independentRecoveryFixture) {
			f.header.Metadata.CutUTC = "2026-10-28T10:00:00+00:00"
			f.lines[0] = independentRecoveryJSON(t, f.header)
		},
		"missing_migration": func(f *independentRecoveryFixture) {
			f.header.Migrations = EmbeddedSchemaIdentity()[1:]
			f.lines[0] = independentRecoveryJSON(t, f.header)
		},
		"inventory_type_with_recomputed_hash": func(f *independentRecoveryFixture) {
			f.header.Inventory.Tables[0].Columns[0].TypeOID = 0
			v, _ := json.Marshal(f.header.Inventory)
			f.header.InventorySHA256 = independentRecoveryDigest(v)
			f.lines[0] = independentRecoveryJSON(t, f.header)
		},
		"missing_table": func(f *independentRecoveryFixture) { f.lines = append(f.lines[:f.starts[0]], f.lines[f.ends[0]+1:]...) },
		"duplicate_table": func(f *independentRecoveryFixture) {
			block := append([][]byte{}, f.lines[f.starts[0]:f.ends[0]+1]...)
			i := f.ends[0] + 1
			tail := append([][]byte{}, f.lines[i:]...)
			f.lines = append(append(f.lines[:i], block...), tail...)
		},
		"tables_reordered": func(f *independentRecoveryFixture) {
			a, b := f.starts[0], f.starts[1]
			for i := 0; i < 3; i++ {
				f.lines[a+i], f.lines[b+i] = f.lines[b+i], f.lines[a+i]
			}
		},
		"table_path_name": func(f *independentRecoveryFixture) {
			f.lines[f.starts[0]] = []byte("{\"kind\":\"table\",\"name\":\"../events\"}\n")
		},
		"unknown_chunk_field": func(f *independentRecoveryFixture) {
			i := f.chunks[0]
			f.lines[i] = bytes.Replace(f.lines[i], []byte(`"kind":"chunk"`), []byte(`"kind":"chunk","sql":"SELECT 1"`), 1)
		},
		"duplicate_chunk_kind": func(f *independentRecoveryFixture) {
			i := f.chunks[0]
			f.lines[i] = bytes.Replace(f.lines[i], []byte(`"kind":"chunk"`), []byte(`"kind":"chunk","kind":"chunk"`), 1)
		},
		"empty_chunk": func(f *independentRecoveryFixture) {
			f.lines[f.chunks[0]] = []byte("{\"kind\":\"chunk\",\"data\":\"\"}\n")
		},
		"base64_newline_alias": func(f *independentRecoveryFixture) {
			i := f.chunks[0]
			f.lines[i] = bytes.Replace(f.lines[i], []byte(`"data":"`), []byte(`"data":"\n`), 1)
		},
		"chunk_after_short": func(f *independentRecoveryFixture) {
			i := f.chunks[0] + 1
			tail := append([][]byte{}, f.lines[i:]...)
			f.lines = append(append(f.lines[:i], f.lines[i-1]), tail...)
		},
		"wrong_table_hash": func(f *independentRecoveryFixture) {
			i := f.ends[0]
			f.lines[i] = bytes.Replace(f.lines[i], []byte(independentRecoveryDigest(independentRecoveryCopy())), []byte(strings.Repeat("0", 64)), 1)
		},
		"wrong_table_count": func(f *independentRecoveryFixture) {
			i := f.ends[0]
			f.lines[i] = bytes.Replace(f.lines[i], []byte(`"rows":"0"`), []byte(`"rows":"1"`), 1)
		},
		"state_missing_array": func(f *independentRecoveryFixture) {
			f.lines[f.state] = bytes.Replace(f.lines[f.state], []byte(`,"key_references":[]`), nil, 1)
		},
		"duplicate_state": func(f *independentRecoveryFixture) { f.lines = append(f.lines, f.lines[f.state]) },
		"unknown_entry": func(f *independentRecoveryFixture) {
			f.lines = append(f.lines, []byte("{\"kind\":\"attachment\",\"path\":\"private\"}\n"))
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := independentRecoveryFixtureNew(t)
			change(&f)
			report, err := ValidateRecoverySnapshot(context.Background(), bytes.NewReader(f.bytes(t)), DefaultRecoverySnapshotLimits())
			if err == nil {
				t.Fatal("tampered independently rehashed stream accepted")
			}
			if report.SHA256 != "" {
				t.Fatal("invalid input returned completed report")
			}
			if !errors.Is(err, ErrRecoverySnapshotInvalid) && !errors.Is(err, ErrRecoverySnapshotSchema) {
				t.Fatalf("unexpected error class: %v", err)
			}
		})
	}
}

func TestRecoverySnapshotIndependentFullStreamEOF(t *testing.T) {
	f := independentRecoveryFixtureNew(t)
	raw := f.bytes(t)
	for name, bad := range map[string][]byte{
		"missing_final_newline": raw[:len(raw)-1],
		"no_completion":         bytes.Join(f.lines, nil),
		"cut_inside_header":     f.lines[0][:len(f.lines[0])/2],
		"cut_after_first_copy":  bytes.Join(f.lines[:f.chunks[0]+1], nil),
		"trailing_whitespace":   append(append([]byte{}, raw...), '\n'),
		"concatenated_stream":   append(append([]byte{}, raw...), raw...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateRecoverySnapshot(context.Background(), bytes.NewReader(bad), DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoverySnapshotInvalid) {
				t.Fatalf("expected invalid EOF/framing, got %v", err)
			}
		})
	}
	t.Run("error_after_complete", func(t *testing.T) {
		input := io.MultiReader(bytes.NewReader(raw), independentRecoveryReadError{})
		if _, err := ValidateRecoverySnapshot(context.Background(), input, DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoverySnapshotIO) {
			t.Fatalf("completion ignored trailing read error: %v", err)
		}
	})
	t.Run("whole_stream_hash", func(t *testing.T) {
		bad := append([]byte{}, raw...)
		i := bytes.LastIndex(bad, []byte(`"preceding_sha256":"`)) + len(`"preceding_sha256":"`)
		if i < 20 {
			t.Fatal("footer fixture missing")
		}
		bad[i] = 'g'
		if _, err := ValidateRecoverySnapshot(context.Background(), bytes.NewReader(bad), DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoverySnapshotInvalid) {
			t.Fatalf("completion digest ignored: %v", err)
		}
	})
}

func TestRecoverySnapshotIndependentLegacyReferenceNames(t *testing.T) {
	for i, id := range []string{"model.v1", "модель-тест", "model with space"} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			state := recoverySnapshotState{Kind: "state", Sequences: []RecoverySnapshotSequence{}, KeyReferences: []RecoverySnapshotKeyReference{{Kind: "model", OrganizationID: "org_test", ID: id, Version: 1, Environment: "PLATFORM_MODEL_KEY_TEST"}}, Organizations: []RecoverySnapshotOrganization{}}
			if !validText(id, 128) {
				t.Fatal("fixture is not a valid legacy ID")
			}
			if err := recoverySnapshotValidateState(state, RecoverySnapshotInventory{}, DefaultRecoverySnapshotLimits()); err != nil {
				t.Fatalf("valid legacy reference rejected: %v", err)
			}
		})
	}
}

func TestRecoverySnapshotIndependentDecimalWirePolicy(t *testing.T) {
	// Object property order/whitespace are allowed, while integer string
	// spellings have one form. The full wire checksum binds the exact bytes.
	valid := "{ \"summary\": { \"sha256\": \"" + strings.Repeat("0", 64) + "\", \"bytes\": \"21\", \"rows\": \"0\", \"name\": \"events\" }, \"kind\": \"table_end\" }\n"
	var target recoverySnapshotTableEnd
	if err := recoverySnapshotDecode([]byte(valid), &target); err != nil {
		t.Fatal("documented JSON order/whitespace rejected", err)
	}
	for _, spelling := range []string{"00", "01", "-0", "+1", "1e0", "1.0", " 1", "9223372036854775808"} {
		t.Run(spelling, func(t *testing.T) {
			bad := strings.Replace(valid, `"rows": "0"`, `"rows": "`+spelling+`"`, 1)
			if err := recoverySnapshotDecode([]byte(bad), &target); !errors.Is(err, ErrRecoverySnapshotInvalid) {
				t.Fatalf("noncanonical decimal accepted: %v", err)
			}
		})
	}
}

func independentRecoveryFirstPayload(t *testing.T, f *independentRecoveryFixture, rows int, firstField ...[]byte) {
	t.Helper()
	var table RecoverySnapshotTable
	for _, entry := range f.header.Inventory.Tables {
		if entry.Exported {
			table = entry
			break
		}
	}
	row := make([][]byte, len(table.Columns)) // NULL for every field: framing, not semantic validity.
	if len(firstField) > 0 {
		row[0] = firstField[0]
	}
	tuples := make([][][]byte, rows)
	for i := range tuples {
		tuples[i] = row
	}
	raw := independentRecoveryCopy(tuples...)
	f.lines[f.chunks[0]] = independentRecoveryJSON(t, struct {
		Kind string `json:"kind"`
		Data string `json:"data"`
	}{"chunk", base64.StdEncoding.EncodeToString(raw)})
	f.lines[f.ends[0]] = independentRecoveryJSON(t, struct {
		Kind    string                       `json:"kind"`
		Summary RecoverySnapshotTableSummary `json:"summary"`
	}{"table_end", RecoverySnapshotTableSummary{Name: table.Name, Rows: int64(rows), Bytes: int64(len(raw)), SHA256: independentRecoveryDigest(raw)}})
	f.rows = int64(rows)
	f.dataBytes += int64(len(raw) - len(independentRecoveryCopy()))
}

func TestRecoverySnapshotIndependentFullStreamBoundsAndSemanticBoundary(t *testing.T) {
	t.Run("duplicate_null_primary_keys_are_not_checked", func(t *testing.T) {
		f := independentRecoveryFixtureNew(t)
		independentRecoveryFirstPayload(t, &f, 2)
		report, err := ValidateRecoverySnapshot(context.Background(), bytes.NewReader(f.bytes(t)), DefaultRecoverySnapshotLimits())
		if err != nil || report.Rows != 2 {
			t.Fatalf("framing boundary changed; independently review importer constraints: %v", err)
		}
	})
	for _, kind := range []string{"wire", "total_data", "table_data", "rows", "row_bytes"} {
		t.Run(kind, func(t *testing.T) {
			f := independentRecoveryFixtureNew(t)
			if kind == "row_bytes" {
				independentRecoveryFirstPayload(t, &f, 2, bytes.Repeat([]byte("x"), 33))
			} else {
				independentRecoveryFirstPayload(t, &f, 2)
			}
			raw := f.bytes(t)
			limits := DefaultRecoverySnapshotLimits()
			switch kind {
			case "wire":
				limits.MaxRowBytes = 1024
				limits.MaxTableBytes = 1024
				limits.MaxTotalBytes = 2048
				limits.MaxWireBytes = int64(len(raw) - 1)
			case "total_data":
				limits.MaxRowBytes = 1024
				limits.MaxTableBytes = 1024
				limits.MaxTotalBytes = 1024
			case "table_data":
				limits.MaxRowBytes = 32
				limits.MaxTableBytes = 32
			case "rows":
				limits.MaxRowsPerTable = 1
				limits.MaxRows = 1
			case "row_bytes":
				limits.MaxRowBytes = 32
			}
			if !limits.valid() {
				t.Fatal("independent limit fixture is invalid")
			}
			if _, err := ValidateRecoverySnapshot(context.Background(), bytes.NewReader(raw), limits); !errors.Is(err, ErrRecoverySnapshotLimit) {
				t.Fatalf("configured %s bound ignored: %v", kind, err)
			}
		})
	}
}
