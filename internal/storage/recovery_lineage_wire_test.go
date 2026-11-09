// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/recoveryarchive"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"filippo.io/age"
	"io"
	"os"
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
	var extracted bytes.Buffer
	extractedReport, extractErr := validateRecoverySnapshotEnvelopeTo(ctx, bytes.NewReader(f.bytes(t)), limits, true, &extracted)
	if extractErr != nil || extracted.String() != "abc" || extractedReport.SHA256 != report.SHA256 {
		t.Fatal("extracted payload differs", extractErr)
	}
	for name, sink := range map[string]io.Writer{"short": recoveryLineageTestSink{short: true}, "failure": recoveryLineageTestSink{}} {
		t.Run(name, func(t *testing.T) {
			got, err := validateRecoverySnapshotEnvelopeTo(ctx, bytes.NewReader(f.bytes(t)), limits, true, sink)
			if !errors.Is(err, ErrRecoverySnapshotIO) || got.Lineage != nil {
				t.Fatal("failed extraction returned a valid report", err)
			}
		})
	}
	var incomplete bytes.Buffer
	if got, err := validateRecoverySnapshotEnvelopeTo(ctx, bytes.NewReader(append(f.bytes(t), 'x')), limits, true, &incomplete); err == nil || got.Lineage != nil {
		t.Fatal("extracted bytes bypassed envelope completion")
	}
	t.Run("authenticated_binding", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.Chmod(parent, 0700); err != nil {
			t.Fatal(err)
		}
		id, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatal(err)
		}
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		m := f.header.Metadata
		metadata := recoveryarchive.Metadata{BackupID: m.BackupID, SourceID: m.SourceID, CutUTC: m.CutUTC, SchemaSHA256: m.SchemaSHA256}
		var sealed bytes.Buffer
		options := recoveryarchive.Limits{MaxPlaintextBytes: 8 << 20, MaxCiphertextBytes: 9 << 20}
		if _, err = recoveryarchive.Seal(ctx, &sealed, bytes.NewReader(f.bytes(t)), recoveryarchive.SealOptions{Recipient: id.Recipient(), SigningKey: priv, Metadata: metadata, Limits: options, StagingParent: parent}); err != nil {
			t.Fatal(err)
		}
		archive, err := recoveryarchive.Open(ctx, &sealed, recoveryarchive.OpenOptions{Identity: id, TrustedSigner: pub, Expected: metadata, Limits: options, StagingParent: parent})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := archive.Close(); err != nil {
				t.Error(err)
			}
		}()
		spec := RecoveryTargetSpec{TargetInstallationID: "inst_33333333333333333333333333333333", RestoreID: "restore_44444444444444444444444444444444", BackupID: m.BackupID, SourceID: m.SourceID, CutUTC: m.CutUTC, SchemaSHA256: m.SchemaSHA256}
		var payload bytes.Buffer
		got, err := recoveryBoundArchiveTo(ctx, archive, spec, limits, true, &payload)
		if err != nil || got.SHA256 != report.SHA256 || payload.String() != "abc" {
			t.Fatal("authenticated extraction failed", err)
		}
		// The authenticated framing fixture contains "abc", not generations.
		// Generation layout/chain indexing must refuse it and remove the derived staging directory.
		beforeEntries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := prepareRecoveryLineageArchive(ctx, archive, spec, f.header.Inventory, limits, parent)
		if err == nil || prepared != nil {
			t.Fatal("framing-only payload became prepared history")
		}
		afterEntries, err := os.ReadDir(parent)
		if err != nil || len(afterEntries) != len(beforeEntries) {
			t.Fatal("failed history preparation left staging", err)
		}
		wrong := spec
		wrong.SourceID = spec.TargetInstallationID
		payload.Reset()
		if got, err := recoveryBoundArchiveTo(ctx, archive, wrong, limits, true, &payload); err == nil || got.Lineage != nil {
			t.Fatal("wrong archive binding accepted")
		}
		if _, err := recoveryBoundArchive(ctx, archive, spec, limits); err == nil {
			t.Fatal("public importer admitted v2")
		}
	})
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

type recoveryLineageTestSink struct{ short bool }

func (w recoveryLineageTestSink) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	return 0, io.ErrClosedPipe
}

func recoveryLineageTestArchive(t *testing.T, raw []byte, m RecoverySnapshotMetadata, parent string) *recoveryarchive.VerifiedArchive {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	metadata := recoveryarchive.Metadata{BackupID: m.BackupID, SourceID: m.SourceID, CutUTC: m.CutUTC, SchemaSHA256: m.SchemaSHA256}
	limits := recoveryarchive.Limits{MaxPlaintextBytes: 8 << 20, MaxCiphertextBytes: 9 << 20}
	var sealed bytes.Buffer
	if _, err = recoveryarchive.Seal(context.Background(), &sealed, bytes.NewReader(raw), recoveryarchive.SealOptions{Recipient: identity.Recipient(), SigningKey: priv, Metadata: metadata, Limits: limits, StagingParent: parent}); err != nil {
		t.Fatal(err)
	}
	archive, err := recoveryarchive.Open(context.Background(), &sealed, recoveryarchive.OpenOptions{Identity: identity, TrustedSigner: pub, Expected: metadata, Limits: limits, StagingParent: parent})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := archive.Close(); err != nil {
			t.Error(err)
		}
	})
	return archive
}
