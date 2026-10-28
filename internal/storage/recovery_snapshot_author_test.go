// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

var recoveryAuthorInventoryPath = flag.String("recovery-snapshot-inventory-output", "", "test-only new path for trusted fresh catalogue metadata")

func recoveryAuthorStore(t *testing.T) (*Store, string) {
	t.Helper()
	if os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" {
		t.Skip("new owned PostgreSQL test cluster required")
	}
	return testStore(t)
}
func TestRecoverySnapshotTrustedInventory(t *testing.T) {
	s, _ := recoveryAuthorStore(t)
	ctx := context.Background()
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal("fresh inventory transaction failed", err)
	}
	defer tx.Rollback(ctx)
	inventory, err := recoverySnapshotInventory(ctx, tx, s.runtimeSchema, true)
	if err != nil {
		t.Fatal("fresh typed inventory failed", err)
	}
	raw, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	digest := Hash(raw)
	if *recoveryAuthorInventoryPath != "" {
		f, err := os.OpenFile(*recoveryAuthorInventoryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			t.Fatal("catalogue output must be new", err)
		}
		_, err = f.Write(append(raw, '\n'))
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			t.Fatal("catalogue output failed")
		}
		t.Log("trusted fresh typed inventory", digest, "tables", len(inventory.Tables), "sequences", len(inventory.Sequences))
		if recoverySnapshotInventorySHA256 == "UNSEALED" {
			t.Skip("reference generation only; compiled inventory acceptance not yet sealed")
		}
	}
	if digest != recoverySnapshotInventorySHA256 {
		t.Fatal("fresh migration inventory differs from compiled pin")
	}
	if len(inventory.Sequences) != 2 {
		t.Fatal("expected explicit event and credential sequences")
	}
}

func recoveryAuthorPopulate(t *testing.T, s *Store) (domain.Principal, domain.Agent, string) {
	t.Helper()
	ctx := context.Background()
	p, _ := identity(t, s, 1_000_000)
	a := agent(t, s, p)
	_ = task(t, s, p, a)
	l, err := s.ClaimFixture(ctx, "snapshot-author-fixture", time.Minute)
	if err != nil || l == nil {
		t.Fatal("fixture lease failed", err)
	}
	content := "Тестовый результат\n" + domain.ExecutionInput(*l)
	if err = s.CompleteFixture(ctx, l, content); err != nil {
		t.Fatal("fixture completion failed", err)
	}
	_ = task(t, s, p, a)
	l, err = s.ClaimFixture(ctx, "snapshot-author-pending", time.Minute)
	if err != nil || l == nil {
		t.Fatal("pending lease failed", err)
	}
	for _, amount := range []int64{0, 37} {
		r, err := s.ReserveCost(ctx, l, domain.ReserveCostRequest{ChargeKey: domain.NewID("snapshot"), Category: "compute", Provider: "author-owned-test", PriceVersion: "author-v1", AmountMicroUSD: amount, IntentHash: Hash([]byte("bounded stored accounting fixture"))})
		if err != nil {
			t.Fatal("reserve fixture failed", err)
		}
		if _, err = s.MarkCostUnknown(ctx, p.OrganizationID, r.ID, domain.NewID("unknown"), "snapshot fixture: final cost unknown"); err != nil {
			t.Fatal("unknown fixture failed", err)
		}
	}
	model := domain.ModelConnection{ID: "model.v1-локальный", OrganizationID: p.OrganizationID, Version: 1, Model: "author-local-model", AccountingMode: domain.ModelAccountingLocalUnmetered, ProviderAccount: "author-owned-model", PriceVersion: "author-zero", Endpoint: "http://127.0.0.1:11434/v1/chat/completions", CredentialEnvironment: "PLATFORM_MODEL_KEY_SNAPSHOT_AUTHOR", MaxInputTokens: 1000, MaxInputBytes: 4096, MaxOutputTokens: 64, MaxOutputBytes: 4096, TimeoutMilliseconds: 5000, TokenLimitField: "max_tokens", AllowInsecureLoopback: true, BoundAssumptions: "Owned author fixture; no model request is performed.", Active: true}
	if _, err = s.InstallModelConnection(ctx, model); err != nil {
		t.Fatal("model reference fixture failed", err)
	}
	return p, a, content
}

// Independent field extraction for observations, not production wire parsing.
// No row content is printed on assertion failures.
func recoveryAuthorReadTables(t *testing.T, raw []byte) (map[string][][][]byte, recoverySnapshotHeader) {
	t.Helper()
	var header recoverySnapshotHeader
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	if json.Unmarshal(lines[0], &header) != nil {
		t.Fatal("header decode failed")
	}
	payloads := map[string][]byte{}
	table := ""
	for _, line := range lines[1:] {
		var row map[string]json.RawMessage
		if json.Unmarshal(line, &row) != nil {
			t.Fatal("record decode failed")
		}
		var kind string
		_ = json.Unmarshal(row["kind"], &kind)
		if kind == "table" {
			_ = json.Unmarshal(row["name"], &table)
			payloads[table] = nil
		}
		if kind == "chunk" {
			var encoded string
			_ = json.Unmarshal(row["data"], &encoded)
			chunk, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatal("base64 failed")
			}
			payloads[table] = append(payloads[table], chunk...)
		}
	}
	result := map[string][][][]byte{}
	for table, payload := range payloads {
		reader := bytes.NewReader(payload)
		signature := make([]byte, 19)
		if _, err := io.ReadFull(reader, signature); err != nil || !bytes.Equal(signature[:11], []byte("PGCOPY\n\xff\r\n\x00")) {
			t.Fatal("COPY header failed")
		}
		result[table] = [][][]byte{}
		for {
			var fields int16
			if binary.Read(reader, binary.BigEndian, &fields) != nil {
				t.Fatal("COPY tuple failed")
			}
			if fields == -1 {
				break
			}
			if fields < 0 || fields > 256 {
				t.Fatal("invalid fixture field count")
			}
			values := make([][]byte, int(fields))
			for i := range values {
				var size int32
				if binary.Read(reader, binary.BigEndian, &size) != nil {
					t.Fatal("COPY field size failed")
				}
				if size == -1 {
					continue
				}
				if size < 0 || size > 1<<20 {
					t.Fatal("unexpected fixture field size")
				}
				values[i] = make([]byte, size)
				if _, err := io.ReadFull(reader, values[i]); err != nil {
					t.Fatal("COPY field failed")
				}
			}
			result[table] = append(result[table], values)
		}
		if reader.Len() != 0 {
			t.Fatal("COPY trailing data")
		}
	}
	return result, header
}
func recoveryAuthorColumn(t *testing.T, h recoverySnapshotHeader, table, column string) int {
	t.Helper()
	for _, v := range h.Inventory.Tables {
		if v.Name == table {
			for i, c := range v.Columns {
				if c.Name == column {
					return i
				}
			}
		}
	}
	t.Fatal("expected trusted column missing")
	return -1
}
func TestRecoverySnapshotPopulatedAndStrictValidation(t *testing.T) {
	s, _ := recoveryAuthorStore(t)
	p, _, content := recoveryAuthorPopulate(t, s)
	var wire bytes.Buffer
	report, err := s.ExportRecoverySnapshot(context.Background(), &wire, DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal("populated export failed", err)
	}
	validated, err := ValidateRecoverySnapshot(context.Background(), bytes.NewReader(wire.Bytes()), DefaultRecoverySnapshotLimits())
	if err != nil || !reflect.DeepEqual(report, validated) {
		t.Fatal("actual export validation/report mismatch", err)
	}
	tables, h := recoveryAuthorReadTables(t, wire.Bytes())
	if _, ok := tables["runtime_recovery_control"]; ok {
		t.Fatal("target control was exported as active business state")
	}
	if _, ok := tables["schema_migrations"]; ok {
		t.Fatal("migration acknowledgements were exported for activation")
	}
	at := recoveryAuthorColumn(t, h, "artifacts", "content")
	if len(tables["artifacts"]) != 1 || string(tables["artifacts"][0][at]) != content {
		t.Fatal("exact artifact bytes lost")
	}
	status := recoveryAuthorColumn(t, h, "reservations", "status")
	amount := recoveryAuthorColumn(t, h, "reservations", "amount_microusd")
	unknownZero, unknownPaid := false, false
	for _, r := range tables["reservations"] {
		if string(r[status]) != "unknown" {
			continue
		}
		value := int64(binary.BigEndian.Uint64(r[amount]))
		unknownZero = unknownZero || value == 0
		unknownPaid = unknownPaid || value == 37
	}
	if !unknownZero || !unknownPaid {
		t.Fatal("unknown zero/paid liability omitted")
	}
	if len(report.KeyReferences) != 1 || report.KeyReferences[0].ID != "model.v1-локальный" || report.KeyReferences[0].Environment != "PLATFORM_MODEL_KEY_SNAPSHOT_AUTHOR" {
		t.Fatal("legacy model key reference lost")
	}
	if len(report.Organizations) != 1 || report.Organizations[0].OrganizationID != p.OrganizationID {
		t.Fatal("organization watermark missing")
	}
	var again bytes.Buffer
	second, err := s.ExportRecoverySnapshot(context.Background(), &again, DefaultRecoverySnapshotLimits())
	if err != nil || !reflect.DeepEqual(report.Tables, second.Tables) || !reflect.DeepEqual(report.Sequences, second.Sequences) {
		t.Fatal("read-only export changed source projections", err)
	}

	cases := map[string]func([]byte) []byte{
		"truncated_footer":    func(v []byte) []byte { return v[:len(v)-1] },
		"trailing_object":     func(v []byte) []byte { return append(v, []byte("{}\n")...) },
		"trailing_whitespace": func(v []byte) []byte { return append(v, ' ') },
		"wrong_table_name": func(v []byte) []byte {
			return bytes.Replace(v, []byte(`"kind":"table","name":"access_tokens"`), []byte(`"kind":"table","name":"organizations"`), 1)
		},
		"duplicate_header_key": func(v []byte) []byte {
			return bytes.Replace(v, []byte(`"kind":"header"`), []byte(`"kind":"header","kind":"header"`), 1)
		},
		"unknown_field": func(v []byte) []byte {
			return bytes.Replace(v, []byte(`"kind":"header"`), []byte(`"kind":"header","execute_sql":"SELECT 1"`), 1)
		},
		"wrong_digest": func(v []byte) []byte {
			return bytes.Replace(v, []byte(report.Tables[0].SHA256), []byte(strings.Repeat("0", 64)), 1)
		},
		"truncated_chunk": func(v []byte) []byte { offset := bytes.Index(v, []byte(`"kind":"chunk"`)); return v[:offset+24] },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateRecoverySnapshot(context.Background(), bytes.NewReader(change(append([]byte{}, wire.Bytes()...))), DefaultRecoverySnapshotLimits()); err == nil {
				t.Fatal("malformed snapshot accepted")
			}
		})
	}
}

type recoveryAuthorHookWriter struct {
	out    bytes.Buffer
	hook   func() error
	called bool
}

func (w *recoveryAuthorHookWriter) Write(p []byte) (int, error) {
	if !w.called {
		w.called = true
		if err := w.hook(); err != nil {
			return 0, err
		}
	}
	return w.out.Write(p)
}
func TestRecoverySnapshotOneCutAndSequenceHighWater(t *testing.T) {
	s, _ := recoveryAuthorStore(t)
	p, a, _ := recoveryAuthorPopulate(t, s)
	ctx := context.Background()
	var lateTask domain.Task
	var commitObserved time.Time
	writer := &recoveryAuthorHookWriter{hook: func() error {
		raw, err := s.CreateTask(ctx, p, domain.NewID("late"), domain.CreateTask{Goal: "Committed after snapshot cutoff", AgentID: a.ID, BudgetMicroUSD: 1000})
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &lateTask); err != nil {
			return err
		}
		_, err = s.Pool.Exec(ctx, "UPDATE memberships SET active=false WHERE organization_id=$1 AND principal_id=$2", p.OrganizationID, p.ID)
		if err != nil {
			return err
		}
		return s.Pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&commitObserved)
	}}
	report, err := s.ExportRecoverySnapshot(ctx, writer, DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal("concurrent snapshot failed", err)
	}
	cut, err := time.Parse(time.RFC3339Nano, report.Metadata.CutUTC)
	if err != nil || !cut.Before(commitObserved) {
		t.Fatal("cutoff does not precede observed late commit")
	}
	tables, h := recoveryAuthorReadTables(t, writer.out.Bytes())
	id := recoveryAuthorColumn(t, h, "tasks", "id")
	if len(tables["tasks"]) != 2 {
		t.Fatal("snapshot mixed before/after task rows")
	}
	for _, r := range tables["tasks"] {
		if string(r[id]) == lateTask.ID {
			t.Fatal("post-cut task entered old snapshot")
		}
	}
	var sourceCount int
	if err = s.Pool.QueryRow(ctx, "SELECT count(*) FROM tasks").Scan(&sourceCount); err != nil || sourceCount != 3 {
		t.Fatal("late source commit did not occur", err)
	}
	active := recoveryAuthorColumn(t, h, "memberships", "active")
	if len(tables["memberships"]) != 1 || !bytes.Equal(tables["memberships"][0][active], []byte{1}) {
		t.Fatal("post-cut revoke mixed into old membership snapshot")
	}
	for _, seq := range report.Sequences {
		if seq.NextMinimum <= seq.IncludedMaximum || seq.NextMinimum <= seq.ObservedLast {
			t.Fatal("sequence floor does not dominate captured values")
		}
		if seq.Name == "credential_origin_sequence" && seq.ObservedLast <= seq.IncludedMaximum {
			t.Fatal("non-MVCC revocation increment missing")
		}
	}
	if _, err = ValidateRecoverySnapshot(ctx, bytes.NewReader(writer.out.Bytes()), DefaultRecoverySnapshotLimits()); err != nil {
		t.Fatal("concurrent snapshot framing invalid", err)
	}
}

func TestRecoverySnapshotRefusesDriftHeldAndOversizeBeforeOutput(t *testing.T) {
	for _, scenario := range []string{"extra_table", "extra_column", "missing_migration", "held", "row_bound", "row_count", "saturated_sequence"} {
		t.Run(scenario, func(t *testing.T) {
			s, _ := recoveryAuthorStore(t)
			ctx := context.Background()
			l := DefaultRecoverySnapshotLimits()
			want := ErrRecoverySnapshotSchema
			switch scenario {
			case "extra_table":
				if _, err := s.Pool.Exec(ctx, "CREATE TABLE unexpected_snapshot_table(id integer PRIMARY KEY)"); err != nil {
					t.Fatal(err)
				}
			case "extra_column":
				if _, err := s.Pool.Exec(ctx, "ALTER TABLE artifacts ADD COLUMN unexpected_snapshot_column text"); err != nil {
					t.Fatal(err)
				}
			case "missing_migration":
				if _, err := s.Pool.Exec(ctx, "DELETE FROM schema_migrations WHERE name='migrations/001_foundation.sql'"); err != nil {
					t.Fatal(err)
				}
			case "held":
				state, err := s.InspectRecoveryTarget(ctx)
				if err != nil {
					t.Fatal(err)
				}
				_, err = s.PrepareRecoveryTarget(ctx, RecoveryTargetSpec{TargetInstallationID: state.InstallationID, RestoreID: domain.NewID("restore"), BackupID: domain.NewID("bak"), SourceID: domain.NewID("inst"), CutUTC: time.Now().UTC().Format(time.RFC3339Nano), SchemaSHA256: EmbeddedSchemaSHA256()})
				if err != nil {
					t.Fatal(err)
				}
				want = ErrRecoverySnapshotSource
			case "row_bound":
				recoveryAuthorPopulate(t, s)
				l.MaxRowBytes = 32
				want = ErrRecoverySnapshotLimit
			case "row_count":
				recoveryAuthorPopulate(t, s)
				l.MaxRowsPerTable = 1
				want = ErrRecoverySnapshotLimit
			case "saturated_sequence":
				if _, err := s.Pool.Exec(ctx, "SELECT setval('credential_origin_sequence',9223372036854775807,true)"); err != nil {
					t.Fatal(err)
				}
				want = ErrRecoverySnapshotLimit
			}
			var out bytes.Buffer
			_, err := s.ExportRecoverySnapshot(ctx, &out, l)
			if !errors.Is(err, want) {
				t.Fatal("expected export rejection", scenario, err)
			}
			if scenario != "saturated_sequence" && out.Len() != 0 {
				t.Fatal("preflight refusal emitted partial payload")
			}
			if bytes.Contains(out.Bytes(), []byte(`"kind":"complete"`)) {
				t.Fatal("failed export emitted complete marker")
			}
		})
	}
}

// Force the first RR statement to wait, then commit source work while it waits.
// A post-lock clock_timestamp would incorrectly claim a later cutoff than this
// excluded commit. transaction_timestamp remains a conservative lower bound.
func TestRecoverySnapshotCutBeforeMigrationGateWait(t *testing.T) {
	s, _ := recoveryAuthorStore(t)
	p, a, _ := recoveryAuthorPopulate(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gate, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal("gate transaction failed", err)
	}
	defer gate.Rollback(context.Background())
	if _, err = gate.Exec(ctx, "SELECT pg_advisory_xact_lock(841932079)"); err != nil {
		t.Fatal("gate acquisition failed", err)
	}
	type outcome struct {
		report RecoverySnapshotReport
		raw    []byte
		err    error
	}
	result := make(chan outcome, 1)
	go func() {
		var out bytes.Buffer
		r, err := s.ExportRecoverySnapshot(ctx, &out, DefaultRecoverySnapshotLimits())
		result <- outcome{r, out.Bytes(), err}
	}()
	for {
		var waiting int
		err = s.Pool.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND classid=0 AND objid=841932079 AND NOT granted").Scan(&waiting)
		if err != nil {
			t.Fatal("wait observation failed", err)
		}
		if waiting > 0 {
			break
		}
		select {
		case got := <-result:
			t.Fatal("export completed before migration gate release", got.err)
		case <-ctx.Done():
			t.Fatal("export did not wait for migration gate")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if _, err = s.CreateTask(ctx, p, domain.NewID("duringwait"), domain.CreateTask{Goal: "Committed while snapshot waits for migration gate", AgentID: a.ID, BudgetMicroUSD: 1000}); err != nil {
		t.Fatal("late fixture commit failed", err)
	}
	var lateCommit time.Time
	if err = s.Pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&lateCommit); err != nil {
		t.Fatal("commit observation failed", err)
	}
	if err = gate.Commit(ctx); err != nil {
		t.Fatal("gate release failed", err)
	}
	got := <-result
	if got.err != nil {
		t.Fatal("waited export failed", got.err)
	}
	cut, err := time.Parse(time.RFC3339Nano, got.report.Metadata.CutUTC)
	if err != nil || !cut.Before(lateCommit) {
		t.Fatal("cutoff excludes reconciliation gap during gate wait")
	}
	tables, _ := recoveryAuthorReadTables(t, got.raw)
	if len(tables["tasks"]) != 2 {
		t.Fatal("snapshot incorrectly included source commit during gate wait")
	}
}

func TestRecoverySnapshotExportFailureAndCancellation(t *testing.T) {
	s, _ := recoveryAuthorStore(t)
	recoveryAuthorPopulate(t, s)
	ctx := context.Background()
	var before, after bytes.Buffer
	prior, err := s.ExportRecoverySnapshot(ctx, &before, DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal("baseline export failed", err)
	}
	for _, short := range []bool{false, true} {
		r, err := s.ExportRecoverySnapshot(ctx, recoveryAuthorBrokenWriter{short: short}, DefaultRecoverySnapshotLimits())
		if err != ErrRecoverySnapshotIO || !reflect.DeepEqual(r, RecoverySnapshotReport{}) {
			t.Fatal("failed sink returned successful report")
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	var out bytes.Buffer
	if _, err = s.ExportRecoverySnapshot(cancelled, &out, DefaultRecoverySnapshotLimits()); !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatal("cancelled source emitted data", err)
	}
	final, err := s.ExportRecoverySnapshot(ctx, &after, DefaultRecoverySnapshotLimits())
	if err != nil || !reflect.DeepEqual(prior.Tables, final.Tables) || !reflect.DeepEqual(prior.Sequences, final.Sequences) {
		t.Fatal("failed export changed source data or sequence", err)
	}
}
