// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryarchive"
	"filippo.io/age"
)

type recoveryStageAuthorFixture struct {
	header recoverySnapshotHeader
	rows   map[string][][][]byte
	state  recoverySnapshotState
}

func recoveryStageAuthorRewrite(t *testing.T, original []byte, mutate func(*recoveryStageAuthorFixture)) []byte {
	t.Helper()
	rows, header := recoveryAuthorReadTables(t, original)
	f := recoveryStageAuthorFixture{header: header, rows: rows}
	for _, line := range bytes.Split(original, []byte{'\n'}) {
		var kind struct{ Kind string }
		if json.Unmarshal(line, &kind) == nil && kind.Kind == "state" {
			if json.Unmarshal(line, &f.state) != nil {
				t.Fatal("fixture state decode failed")
			}
		}
	}
	mutate(&f)
	var output bytes.Buffer
	w := &recoverySnapshotWriter{ctx: context.Background(), out: &output, hash: sha256.New(), limit: DefaultRecoverySnapshotLimits().MaxWireBytes}
	put := func(value any) {
		t.Helper()
		if err := w.record(value); err != nil {
			t.Fatal("fixture record failed")
		}
	}
	put(f.header)
	var count, size int64
	tables := 0
	for _, table := range f.header.Inventory.Tables {
		if !table.Exported {
			continue
		}
		put(recoverySnapshotTableStart{Kind: "table", Name: table.Name})
		raw := independentRecoveryCopy(f.rows[table.Name]...)
		for chunk := raw; len(chunk) != 0; {
			n := min(len(chunk), recoverySnapshotChunkSize)
			put(recoverySnapshotChunk{Kind: "chunk", Data: base64.StdEncoding.EncodeToString(chunk[:n])})
			chunk = chunk[n:]
		}
		rows := int64(len(f.rows[table.Name]))
		put(recoverySnapshotTableEnd{Kind: "table_end", Summary: RecoverySnapshotTableSummary{Name: table.Name, Rows: rows, Bytes: int64(len(raw)), SHA256: Hash(raw)}})
		count += rows
		size += int64(len(raw))
		tables++
	}
	put(f.state)
	put(recoverySnapshotComplete{Kind: "complete", Tables: tables, Rows: count, Bytes: size, PrecedingWireBytes: w.bytes, PrecedingSHA256: recoverySnapshotHash(w.hash)})
	return output.Bytes()
}

func recoveryStageAuthorArchive(t *testing.T, raw []byte) *recoveryarchive.VerifiedArchive {
	t.Helper()
	ctx := context.Background()
	r, err := ValidateRecoverySnapshot(ctx, bytes.NewReader(raw), DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal("semantic fixture must pass framing first", err)
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal("fixture recipient failed")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("fixture signer failed")
	}
	parent := t.TempDir()
	if err = os.Chmod(parent, 0700); err != nil {
		t.Fatal("private fixture directory failed")
	}
	metadata := recoveryarchive.Metadata{BackupID: r.Metadata.BackupID, SourceID: r.Metadata.SourceID, CutUTC: r.Metadata.CutUTC, SchemaSHA256: r.Metadata.SchemaSHA256}
	limits := recoveryarchive.Limits{MaxPlaintextBytes: 8 << 20, MaxCiphertextBytes: 9 << 20}
	var sealed bytes.Buffer
	if _, err = recoveryarchive.Seal(ctx, &sealed, bytes.NewReader(raw), recoveryarchive.SealOptions{Recipient: id.Recipient(), SigningKey: priv, Metadata: metadata, Limits: limits, StagingParent: parent}); err != nil {
		t.Fatal("fixture signing failed")
	}
	verified, err := recoveryarchive.Open(ctx, &sealed, recoveryarchive.OpenOptions{Identity: id, TrustedSigner: pub, Expected: metadata, Limits: limits, StagingParent: parent})
	if err != nil {
		t.Fatal("fixture authentication failed")
	}
	t.Cleanup(func() {
		if err := verified.Close(); err != nil {
			t.Error("fixture private staging cleanup failed")
		}
	})
	return verified
}

func recoveryStageAuthorTarget(t *testing.T, raw []byte) (*Store, RecoveryTargetSpec, RecoveryTargetState) {
	t.Helper()
	s, _ := recoveryAuthorStore(t)
	r, err := ValidateRecoverySnapshot(context.Background(), bytes.NewReader(raw), DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal("fixture framing failed")
	}
	spec := recoveryTargetAuthorSpec(t, s)
	spec.BackupID, spec.SourceID, spec.CutUTC, spec.SchemaSHA256 = r.Metadata.BackupID, r.Metadata.SourceID, r.Metadata.CutUTC, r.Metadata.SchemaSHA256
	held, err := s.PrepareRecoveryTarget(context.Background(), spec)
	if err != nil {
		t.Fatal("fixture target hold failed", err)
	}
	return s, spec, held
}

func recoveryStageAuthorAssertUntouched(t *testing.T, s *Store, before RecoveryTargetState) {
	t.Helper()
	after, err := s.InspectRecoveryTarget(context.Background())
	if err != nil || after != before {
		t.Fatal("validation changed target identity or hold")
	}
	var count int
	if err = s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM organizations").Scan(&count); err != nil || count != 0 {
		t.Fatal("validation persisted business rows")
	}
	if err = s.CheckRuntimeAdmission(context.Background()); !errors.Is(err, RecoveryHeld) {
		t.Fatal("validation enabled ordinary runtime")
	}
}

func TestRecoveryStageAuthorPopulatedRoundTrip(t *testing.T) {
	source, _ := recoveryAuthorStore(t)
	recoveryAuthorPopulate(t, source)
	var out bytes.Buffer
	if _, err := source.ExportRecoverySnapshot(context.Background(), &out, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal("source export failed")
	}
	target, spec, held := recoveryStageAuthorTarget(t, out.Bytes())
	verified := recoveryStageAuthorArchive(t, out.Bytes())
	report, err := target.CheckRecoverySnapshotData(context.Background(), verified, spec, DefaultRecoverySnapshotLimits())
	if err != nil || report.Scope != "rollback_only_typed_stage" || !report.RollbackConfirmed || report.FullSemanticValidation || report.ForeignKeysChecked == 0 || report.GeneratedChecked != 2 || report.SemanticCatalogSHA256 != recoverySemanticCatalogSHA256 {
		t.Fatal("populated stage validation failed", err, report.ForeignKeysChecked, report.GeneratedChecked)
	}
	t.Log("validated tables", len(report.Snapshot.Tables), "rows", report.Snapshot.Rows, "foreign keys", report.ForeignKeysChecked, "generated values", report.GeneratedChecked, "rollback confirmed", report.RollbackConfirmed)
	recoveryStageAuthorAssertUntouched(t, target, held)
	// A new preflight against the same still-empty held target is allowed;
	// neither run claims to have imported or resolved an import outcome.
	if again, err := target.CheckRecoverySnapshotData(context.Background(), verified, spec, DefaultRecoverySnapshotLimits()); err != nil || !again.RollbackConfirmed || again.Snapshot.SHA256 != report.Snapshot.SHA256 {
		t.Fatal("read-only repeated validation failed", err)
	}
	recoveryStageAuthorAssertUntouched(t, target, held)
}

func TestRecoveryStageAuthorFramingValidCorruptions(t *testing.T) {
	source, _ := recoveryAuthorStore(t)
	recoveryAuthorPopulate(t, source)
	var original bytes.Buffer
	if _, err := source.ExportRecoverySnapshot(context.Background(), &original, DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal("source export failed")
	}
	column := func(f *recoveryStageAuthorFixture, table, name string) int {
		return recoveryAuthorColumn(t, f.header, table, name)
	}
	cases := []struct {
		name string
		edit func(*recoveryStageAuthorFixture)
		want error
	}{
		{"null_primary_key", func(f *recoveryStageAuthorFixture) {
			f.rows["organizations"][0][column(f, "organizations", "id")] = nil
		}, ErrRecoveryStageData},
		{"duplicate_primary_key", func(f *recoveryStageAuthorFixture) {
			f.rows["organizations"] = append(f.rows["organizations"], f.rows["organizations"][0])
		}, ErrRecoveryStageData},
		{"negative_checked_budget", func(f *recoveryStageAuthorFixture) {
			f.rows["organizations"][0][column(f, "organizations", "budget_microusd")] = binary.BigEndian.AppendUint64(nil, ^uint64(0))
		}, ErrRecoveryStageData},
		{"malformed_integer", func(f *recoveryStageAuthorFixture) {
			f.rows["organizations"][0][column(f, "organizations", "budget_microusd")] = []byte{1}
		}, ErrRecoveryBinaryValue},
		{"invalid_utf8", func(f *recoveryStageAuthorFixture) {
			f.rows["organizations"][0][column(f, "organizations", "name")] = []byte{255}
		}, ErrRecoveryBinaryValue},
		{"orphan_artifact", func(f *recoveryStageAuthorFixture) {
			f.rows["artifacts"][0][column(f, "artifacts", "task_id")] = []byte(domain.NewID("task"))
		}, ErrRecoveryStageReferences},
		{"wrong_financial_projection", func(f *recoveryStageAuthorFixture) {
			f.rows["organizations"][0][column(f, "organizations", "spent_microusd")] = binary.BigEndian.AppendUint64(nil, 1)
		}, ErrRecoveryInvariant},
		{"wrong_sequence_projection", func(f *recoveryStageAuthorFixture) {
			f.state.Sequences[0].IncludedMaximum++
			f.state.Sequences[0].NextMinimum = max(f.state.Sequences[0].ObservedLast, f.state.Sequences[0].IncludedMaximum) + 1
		}, ErrRecoveryStageState},
		{"omitted_key_reference", func(f *recoveryStageAuthorFixture) { f.state.KeyReferences = []RecoverySnapshotKeyReference{} }, ErrRecoveryStageState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := recoveryStageAuthorRewrite(t, original.Bytes(), tc.edit)
			target, spec, held := recoveryStageAuthorTarget(t, raw)
			verified := recoveryStageAuthorArchive(t, raw)
			if result, err := target.CheckRecoverySnapshotData(context.Background(), verified, spec, DefaultRecoverySnapshotLimits()); !errors.Is(err, tc.want) || result.RollbackConfirmed {
				t.Fatal("framing-valid corruption accepted or wrong boundary", err)
			}
			recoveryStageAuthorAssertUntouched(t, target, held)
		})
	}
}

func TestRecoveryStageAuthorTargetAndCancellation(t *testing.T) {
	source, _ := recoveryAuthorStore(t)
	recoveryAuthorPopulate(t, source)
	var original bytes.Buffer
	before, err := source.ExportRecoverySnapshot(context.Background(), &original, DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal("source export failed")
	}
	target, spec, held := recoveryStageAuthorTarget(t, original.Bytes())
	verified := recoveryStageAuthorArchive(t, original.Bytes())
	t.Run("different_target_generation", func(t *testing.T) {
		wrong := spec
		wrong.RestoreID = domain.NewID("restore")
		if _, err := target.CheckRecoverySnapshotData(context.Background(), verified, wrong, DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoveryStageTarget) {
			t.Fatal("different recovery generation admitted", err)
		}
		recoveryStageAuthorAssertUntouched(t, target, held)
	})
	t.Run("normal_populated_source", func(t *testing.T) {
		if _, err := source.CheckRecoverySnapshotData(context.Background(), verified, spec, DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoveryStageTarget) {
			t.Fatal("normal source admitted as destination", err)
		}
		var out bytes.Buffer
		after, err := source.ExportRecoverySnapshot(context.Background(), &out, DefaultRecoverySnapshotLimits())
		if err != nil || !bytes.Equal(mustStageTableJSON(t, before.Tables), mustStageTableJSON(t, after.Tables)) {
			t.Fatal("rejected target attempt changed source data")
		}
	})
	t.Run("cancelled_before_input", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := target.CheckRecoverySnapshotData(ctx, verified, spec, DefaultRecoverySnapshotLimits()); !errors.Is(err, context.Canceled) {
			t.Fatal("cancelled validation continued", err)
		}
		recoveryStageAuthorAssertUntouched(t, target, held)
	})
}

func mustStageTableJSON(t *testing.T, tables []RecoverySnapshotTableSummary) []byte {
	t.Helper()
	raw, err := json.Marshal(tables)
	if err != nil {
		t.Fatal("table summary encoding failed")
	}
	return raw
}
