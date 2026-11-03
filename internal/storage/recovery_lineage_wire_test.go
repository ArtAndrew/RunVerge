// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"
)

func TestRecoveryLineageEnvelopeFraming(t *testing.T) {
	fixture := func() independentRecoveryFixture {
		f := independentRecoveryFixtureNew(t)
		f.header.Format = recoverySnapshotLineageFormat
		f.lines[0] = independentRecoveryJSON(t, f.header)
		f.lines = append(f.lines, []byte("{\"kind\":\"lineage\",\"payload_bytes\":\"3\",\"records\":\"1\",\"generations\":1,\"chunks_sha256\":\"6ae408386a61f69681602a20e7ea6f4916caffd320353b0477e11293fdc7438c\"}\n"), []byte("{\"kind\":\"lineage_chunk\",\"ordinal\":0,\"data\":\"YWJj\"}\n"), []byte("{\"kind\":\"lineage_end\",\"chunks_sha256\":\"6ae408386a61f69681602a20e7ea6f4916caffd320353b0477e11293fdc7438c\"}\n"))
		f.dataBytes += 3
		f.rows++
		return f
	}
	ctx := context.Background()
	limits := DefaultRecoverySnapshotLimits()
	f := fixture()
	report, err := validateRecoverySnapshotEnvelope(ctx, bytes.NewReader(f.bytes(t)), limits, true)
	if err != nil || report.Lineage == nil || report.Lineage.PayloadBytes != 3 || report.Lineage.Records != 1 {
		t.Fatal("bounded v2 framing rejected", err)
	}
	if _, err = ValidateRecoverySnapshot(ctx, bytes.NewReader(f.bytes(t)), limits); err == nil {
		t.Fatal("unfinished v2 import publicly enabled")
	}
	cases := map[string]func(*independentRecoveryFixture){
		"missing_section": func(f *independentRecoveryFixture) { f.lines = f.lines[:len(f.lines)-3] },
		"missing_chunk":   func(f *independentRecoveryFixture) { n := len(f.lines); f.lines = append(f.lines[:n-2], f.lines[n-1]) },
		"changed_payload": func(f *independentRecoveryFixture) {
			n := len(f.lines) - 2
			f.lines[n] = bytes.ReplaceAll(f.lines[n], []byte("YWJj"), []byte("YWJk"))
		},
		"wrong_ordinal": func(f *independentRecoveryFixture) {
			n := len(f.lines) - 2
			f.lines[n] = bytes.ReplaceAll(f.lines[n], []byte("\"ordinal\":0"), []byte("\"ordinal\":1"))
		},
		"extra_chunk": func(f *independentRecoveryFixture) {
			n := len(f.lines)
			f.lines = append(f.lines[:n-1], f.lines[n-2], f.lines[n-1])
		},
		"missing_end": func(f *independentRecoveryFixture) { f.lines = f.lines[:len(f.lines)-1] },
		"unknown_field": func(f *independentRecoveryFixture) {
			n := len(f.lines) - 3
			f.lines[n] = bytes.Replace(f.lines[n], []byte("{\"kind\""), []byte("{\"authority\":true,\"kind\""), 1)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := fixture()
			mutate(&f)
			if _, err := validateRecoverySnapshotEnvelope(ctx, bytes.NewReader(f.bytes(t)), limits, true); err == nil {
				t.Fatal("invalid lineage accepted")
			}
		})
	}
	raw := f.bytes(t)
	for _, suffix := range [][]byte{[]byte("\n"), []byte("{}\n")} {
		if _, err := validateRecoverySnapshotEnvelope(ctx, bytes.NewReader(append(append([]byte{}, raw...), suffix...)), limits, true); err == nil {
			t.Fatal("trailing data accepted")
		}
	}
	limited := limits
	limited.MaxRows = limited.MaxRowsPerTable
	f = fixture()
	n := len(f.lines) - 3
	f.lines[n] = bytes.Replace(f.lines[n], []byte("\"records\":\"1\""), []byte("\"records\":\"500001\""), 1)
	if _, err := validateRecoverySnapshotEnvelope(ctx, bytes.NewReader(f.bytes(t)), limited, true); err == nil {
		t.Fatal("lineage bypassed shared row budget")
	}
}

func TestRecoveryLineageEnvelopeWriterMultiChunk(t *testing.T) {
	ctx := context.Background()
	limits := DefaultRecoverySnapshotLimits()
	payload := append(make([]byte, 1<<20), 'z')
	declared := RecoveryLineageFramingReport{PayloadBytes: int64(len(payload)), Records: 1, Generations: 1, ChunksSHA256: "e7b4407cd39ab26b77d5811c12f59130ac73fd92d4a58bdff4f68ed1164f01f3"}
	f := independentRecoveryFixtureNew(t)
	f.header.Format = recoverySnapshotLineageFormat
	f.lines[0] = independentRecoveryJSON(t, f.header)
	var section bytes.Buffer
	w := &recoverySnapshotWriter{ctx: ctx, out: &section, hash: sha256.New(), limit: limits.MaxWireBytes}
	report := RecoverySnapshotReport{Bytes: f.dataBytes, Rows: f.rows}
	if err := writeRecoverySnapshotLineage(w, bytes.NewReader(payload), declared, limits, &report); err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(section.Bytes(), []byte{'\n'})
	f.lines = append(f.lines, lines[:len(lines)-1]...)
	f.dataBytes = report.Bytes
	f.rows = report.Rows
	got, err := validateRecoverySnapshotEnvelope(ctx, bytes.NewReader(f.bytes(t)), limits, true)
	if err != nil || got.Lineage == nil || got.Lineage.ChunksSHA256 != declared.ChunksSHA256 || got.Bytes != report.Bytes {
		t.Fatal("multi-chunk envelope rejected", err)
	}
	for _, tc := range []struct {
		name   string
		body   []byte
		digest string
	}{
		{"truncated", payload[:len(payload)-1], declared.ChunksSHA256},
		{"extra", append(append([]byte{}, payload...), 0), declared.ChunksSHA256},
		{"wrong_commitment", payload, strings.Repeat("0", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			w := &recoverySnapshotWriter{ctx: ctx, out: &out, hash: sha256.New(), limit: limits.MaxWireBytes}
			report := RecoverySnapshotReport{}
			d := declared
			d.ChunksSHA256 = tc.digest
			if err := writeRecoverySnapshotLineage(w, bytes.NewReader(tc.body), d, limits, &report); err == nil {
				t.Fatal("invalid source accepted")
			}
			if report.Lineage != nil || report.Bytes != 0 || bytes.Contains(out.Bytes(), []byte("lineage_end")) {
				t.Fatal("failed section finalized")
			}
		})
	}
}
