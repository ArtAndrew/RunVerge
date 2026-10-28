// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type configurationUpgradeTable struct {
	Rows   int64  `json:"rows"`
	SHA256 string `json:"sha256"`
}

type configurationUpgradeSequence struct {
	LastValue string `json:"last_value"`
	IsCalled  bool   `json:"is_called"`
}

type configurationUpgradeImage struct {
	Tables        map[string]configurationUpgradeTable    `json:"original_tables"`
	MigrationRows string                                  `json:"original_migration_rows_sha256"`
	Migrations    map[string]string                       `json:"original_migration_checksums"`
	Triggers      map[string]string                       `json:"original_trigger_definition_sha256"`
	Functions     map[string]string                       `json:"original_function_definition_sha256"`
	Sequences     map[string]configurationUpgradeSequence `json:"original_sequences"`
}

func configurationUpgradeQuotedColumns(cols []string) string {
	quoted := make([]string, len(cols))
	for i, name := range cols {
		quoted[i] = pgx.Identifier{name}.Sanitize()
	}
	return strings.Join(quoted, ",")
}

// This helper stages the exact embedded001–018, not a hand-written approximation.
// It never opens a schema selected by a caller or changes the main profile.
func configurationUpgradeTarget018(t *testing.T, ctx context.Context, source *independentAutomationFixture) (*Store, map[string][]string) {
	t.Helper()
	schema := domain.NewID("test_configuration_upgrade")
	if _, err := source.s.Pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(source.dsn)
	if err != nil {
		t.Fatal("invalid isolated fixture database configuration")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal("open isolated historical schema")
	}
	t.Cleanup(func() {
		s.Close()
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, e := source.s.Pool.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); e != nil {
			t.Error(e)
		}
	})
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "CREATE TABLE schema_migrations(name text PRIMARY KEY,sha256 text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		t.Fatal(err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	staged := 0
	for _, name := range names {
		if name >= "migrations/019_" {
			continue
		}
		body, e := migrations.ReadFile(name)
		if e != nil || len(body) == 0 || len(body) > 1<<20 {
			t.Fatal("invalid bounded historical migration", name, e)
		}
		if _, e = tx.Exec(ctx, string(body)); e != nil {
			t.Fatal("stage immutable historical migration", name, e)
		}
		if _, e = tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(ctx, "SET CONSTRAINTS ALL DEFERRED"); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(ctx, "INSERT INTO schema_migrations(name,sha256) VALUES($1,$2)", name, Hash(body)); e != nil {
			t.Fatal(e)
		}
		staged++
	}
	if staged != 18 {
		t.Fatal("fixture must begin at exact immutable018", staged)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Restore only original018 columns from a valid runtime-created image. The
	// temporary trigger suspension is fixture construction in one transaction;
	// every old trigger is enabled before the populated migration is attempted.
	var sourceSchema string
	if err = source.s.Pool.QueryRow(ctx, "SELECT current_schema()").Scan(&sourceSchema); err != nil || !strings.HasPrefix(sourceSchema, "test_") {
		t.Fatal("source must be a disposable test schema", err)
	}
	rows, err := s.Pool.Query(ctx, `SELECT table_name,column_name,is_generated FROM information_schema.columns
 WHERE table_schema=current_schema() AND table_name<>'schema_migrations' ORDER BY table_name,ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	columns, writable := map[string][]string{}, map[string][]string{}
	for rows.Next() {
		var table, col, generated string
		if err = rows.Scan(&table, &col, &generated); err != nil {
			t.Fatal(err)
		}
		columns[table] = append(columns[table], col)
		if generated == "NEVER" {
			writable[table] = append(writable[table], col)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	tables := make([]string, 0, len(columns))
	for name := range columns {
		tables = append(tables, name)
	}
	sort.Strings(tables)
	restore, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer restore.Rollback(context.Background())
	for _, table := range tables {
		if _, err = restore.Exec(ctx, "ALTER TABLE "+pgx.Identifier{table}.Sanitize()+" DISABLE TRIGGER ALL"); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range tables {
		cols := configurationUpgradeQuotedColumns(writable[table])
		if _, err = restore.Exec(ctx, "INSERT INTO "+pgx.Identifier{table}.Sanitize()+" ("+cols+") OVERRIDING SYSTEM VALUE SELECT "+cols+" FROM "+pgx.Identifier{sourceSchema, table}.Sanitize()); err != nil {
			t.Fatal("restore original018 writable columns", table, err)
		}
	}
	sequenceRows, err := restore.Query(ctx, "SELECT relname FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND relkind='S' ORDER BY relname")
	if err != nil {
		t.Fatal(err)
	}
	var sequences []string
	for sequenceRows.Next() {
		var name string
		if err = sequenceRows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		sequences = append(sequences, name)
	}
	if err = sequenceRows.Err(); err != nil {
		t.Fatal(err)
	}
	sequenceRows.Close()
	for _, name := range sequences {
		var value int64
		var called bool
		if err = restore.QueryRow(ctx, "SELECT last_value,is_called FROM "+pgx.Identifier{sourceSchema, name}.Sanitize()).Scan(&value, &called); err != nil {
			t.Fatal(err)
		}
		if _, err = restore.Exec(ctx, "SELECT setval($1::regclass,$2,$3)", pgx.Identifier{schema, name}.Sanitize(), value, called); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range tables {
		if _, err = restore.Exec(ctx, "ALTER TABLE "+pgx.Identifier{table}.Sanitize()+" ENABLE TRIGGER ALL"); err != nil {
			t.Fatal(err)
		}
	}
	if err = restore.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return s, columns
}

func configurationUpgradeSnapshot(t *testing.T, ctx context.Context, s *Store, columns map[string][]string) configurationUpgradeImage {
	t.Helper()
	image := configurationUpgradeImage{Tables: map[string]configurationUpgradeTable{}, Migrations: map[string]string{}, Triggers: map[string]string{}, Functions: map[string]string{}, Sequences: map[string]configurationUpgradeSequence{}}
	for table, cols := range columns {
		var raw string
		var count int64
		if err := s.Pool.QueryRow(ctx, "SELECT count(*),COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb)::text FROM (SELECT "+configurationUpgradeQuotedColumns(cols)+" FROM "+pgx.Identifier{table}.Sanitize()+") x").Scan(&count, &raw); err != nil {
			t.Fatal("original-column projection", table, err)
		}
		image.Tables[table] = configurationUpgradeTable{count, Hash([]byte(raw))}
	}
	var raw string
	if err := s.Pool.QueryRow(ctx, "SELECT jsonb_agg(to_jsonb(x) ORDER BY name)::text FROM schema_migrations x WHERE name<'migrations/019_'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	image.MigrationRows = Hash([]byte(raw))
	rows, err := s.Pool.Query(ctx, "SELECT name,sha256 FROM schema_migrations WHERE name<'migrations/019_' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, hash string
		if err = rows.Scan(&name, &hash); err != nil {
			t.Fatal(err)
		}
		image.Migrations[name] = hash
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	rows, err = s.Pool.Query(ctx, `SELECT c.relname||'.'||t.tgname,t.tgenabled,pg_get_triggerdef(t.oid)
 FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid WHERE c.relnamespace=current_schema()::regnamespace AND NOT t.tgisinternal ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, enabled, def string
		if err = rows.Scan(&name, &enabled, &def); err != nil {
			t.Fatal(err)
		}
		if enabled != "O" {
			t.Fatal("fixture or migration left an application guard disabled", name)
		}
		image.Triggers[name] = Hash([]byte(enabled + "\n" + def))
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	rows, err = s.Pool.Query(ctx, `SELECT p.proname||'('||pg_get_function_identity_arguments(p.oid)||')',pg_get_functiondef(p.oid)
 FROM pg_proc p WHERE p.pronamespace=current_schema()::regnamespace AND p.prokind IN('f','p') ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, def string
		if err = rows.Scan(&name, &def); err != nil {
			t.Fatal(err)
		}
		image.Functions[name] = Hash([]byte(def))
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	rows, err = s.Pool.Query(ctx, "SELECT relname FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND relkind='S' ORDER BY relname")
	if err != nil {
		t.Fatal(err)
	}
	var sequences []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		sequences = append(sequences, name)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, name := range sequences {
		var state configurationUpgradeSequence
		if err = s.Pool.QueryRow(ctx, "SELECT last_value::text,is_called FROM "+pgx.Identifier{name}.Sanitize()).Scan(&state.LastValue, &state.IsCalled); err != nil {
			t.Fatal(err)
		}
		image.Sequences[name] = state
	}
	return image
}

func configurationUpgradeAssertPreserved(t *testing.T, before, after configurationUpgradeImage) {
	t.Helper()
	if !reflect.DeepEqual(before.Tables, after.Tables) {
		for name, original := range before.Tables {
			if after.Tables[name] != original {
				t.Error("migration changed original business columns", name)
			}
		}
		t.FailNow()
	}
	if before.MigrationRows != after.MigrationRows || !reflect.DeepEqual(before.Migrations, after.Migrations) || !reflect.DeepEqual(before.Sequences, after.Sequences) {
		t.Fatal("migration changed original migration acknowledgments or sequence state")
	}
	for name, original := range before.Triggers {
		if after.Triggers[name] != original {
			t.Fatal("migration removed, disabled or rewrote an old trigger", name)
		}
	}
	for name, original := range before.Functions {
		if after.Functions[name] != original {
			t.Fatal("migration removed or rewrote an old SQL function", name)
		}
	}
}

// This is populated historical-schema migration evidence, not execution by an
// old server binary, main-profile evidence, or a backup/restore certification.
func TestConfigurationUpgradePreservesPopulated018AndRollsBackAtomically(t *testing.T) {
	f := independentAutomationSetup(t, nil)
	f.executeModel(t)
	grant := f.grant(t, nil)
	f.reconcile(t)
	tool := f.executeRead(t)
	f.reconcile(t)
	f.executeModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := f.s.RevokeAutomation(ctx, f.p, f.task.ID, domain.NewID("key"), domain.RevokeTaskAutomation{GrantID: grant.Grant.ID, ExpectedVersion: grant.Grant.Version, Reason: "Preserve retired consent and historical registered lookup"}); err != nil {
		t.Fatal(err)
	}
	originalTask := f.current(t)
	if originalTask.Status != "waiting_input" || originalTask.CandidateID == "" || originalTask.AcceptedDecisionID != "" || f.modelCalls.Load() != 2 || f.readCalls.Load() != 1 {
		t.Fatal("source image lacks a genuine unverified model candidate and registered read")
	}
	independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 720, 0)
	session := independentSession(t, f.s, f.p, "configuration-upgrade-existing-browser", time.Hour)
	other, _ := identity(t, f.s, 5000)
	otherAgent := agent(t, f.s, other)
	raw, err := f.s.CreateTask(ctx, other, domain.NewID("key"), domain.CreateTask{AgentID: otherAgent.ID, Goal: "Existing queued task in another organization", BudgetMicroUSD: 1000})
	var otherTask domain.Task
	if err != nil || json.Unmarshal(raw, &otherTask) != nil {
		t.Fatal("create old queued task", err)
	}
	var floor int64
	if err = f.s.Pool.QueryRow(ctx, "SELECT min(sequence) FROM events WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&floor); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.AdvanceEventReplayWindow(ctx, f.p.OrganizationID, f.task.ID, floor, "Existing018 logical replay window; retain every durable event"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Pool.Exec(ctx, "SELECT setval(pg_get_serial_sequence('events','sequence'),9007199254740992,true)"); err != nil {
		t.Fatal(err)
	}
	legacyID := domain.NewID("evt")
	var legacySequence int64
	if err = f.s.Pool.QueryRow(ctx, `INSERT INTO events(organization_id,id,task_id,type,data,payload_version)
 VALUES($1,$2,$3,'old.unregistered.provider-kind','{"wide":9007199254740993,"source":"old018fixture"}',0) RETURNING sequence`, f.p.OrganizationID, legacyID, f.task.ID).Scan(&legacySequence); err != nil || legacySequence != 9007199254740993 {
		t.Fatal("seed an exact wide opaque legacy record", err)
	}
	if _, err = f.s.Pool.Exec(ctx, "INSERT INTO outbox(sequence,organization_id,delivered_at) VALUES($1,$2,clock_timestamp())", legacySequence, f.p.OrganizationID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Pool.Exec(ctx, `INSERT INTO idempotency_keys(organization_id,principal_id,scope,key,request_hash,response)
 VALUES($1,$2,'POST /historical018-command','old018-command',$3,'{"version":"0.11.0","wide":9007199254740993,"accepted":false}')`, f.p.OrganizationID, f.p.ID, Hash([]byte("old018 exact command"))); err != nil {
		t.Fatal(err)
	}
	// The view includes its current Task. Capture it after the second model turn
	// and grant retirement, so the before/after comparison uses the same state.
	tool, err = f.s.GetToolExecution(ctx, f.p, f.task.ID, tool.Execution.ID)
	if err != nil {
		t.Fatal("capture exact pre-upgrade tool/task view", err)
	}
	s, columns := configurationUpgradeTarget018(t, ctx, f)
	before := configurationUpgradeSnapshot(t, ctx, s, columns)
	if len(before.Tables) != 52 || len(before.Migrations) != 18 || len(before.Triggers) < 85 || len(before.Functions) < 40 || len(before.Sequences) != 2 {
		t.Fatal("fixture lacks the expected complete018 business inventory", len(before.Tables), len(before.Migrations), len(before.Triggers), len(before.Functions), len(before.Sequences))
	}
	for _, table := range []string{"model_connections", "tool_connections", "tasks", "model_turns", "model_tool_proposals", "model_candidates", "tool_executions", "tool_outcome_receipts", "approvals", "artifacts", "reservations", "ledger_entries", "events", "outbox", "idempotency_keys", "task_event_windows", "event_replay_window_audit", "browser_sessions"} {
		if before.Tables[table].Rows < 1 {
			t.Fatal("representative old table is empty", table)
		}
	}
	checksums := map[string]string{
		"migrations/019_configuration_plane.sql":       "ca0618a160baa44213bab6d1bebb4c8d5299040cf753c42f16090e0f68659a5d",
		"migrations/020_configuration_attenuation.sql": "21d0883d7622d0072d5e228276395e97e03d17e282c85b0fffb6c6ff30dcffb3",
		"migrations/021_webhook_delivery.sql":          "0adca624129d4f62da56822d752605287eda0f02b3203ad0735b14f0ee3422f0",
	}
	for name, checksum := range checksums {
		body, e := migrations.ReadFile(name)
		if e != nil || len(body) == 0 || len(body) > 1<<20 || Hash(body) != checksum {
			t.Fatal("the previously applied migration is not immutable", name, e)
		}
	}

	t.Run("mid_migration_failure_is_atomic", func(t *testing.T) {
		// A late new-table collision rejects019 after earlier new DDL/backfill.
		// It is confined to this disposable target, with all old guards enabled.
		if _, e := s.Pool.Exec(ctx, "CREATE TABLE configuration_audit(fixture_fault integer)"); e != nil {
			t.Fatal(e)
		}
		err := s.Migrate(ctx)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42P07" {
			t.Fatal("expected deliberate new-table collision", err)
		}
		var rolledBack bool
		if e := s.Pool.QueryRow(ctx, `SELECT to_regclass('configuration_revisions') IS NULL
 AND to_regclass('configuration_destinations') IS NULL AND to_regclass('configuration_verifications') IS NULL
 AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE name>='migrations/019_')`).Scan(&rolledBack); e != nil || !rolledBack {
			t.Fatal("failed migration retained partial DDL, backfill or acknowledgment", e)
		}
		configurationUpgradeAssertPreserved(t, before, configurationUpgradeSnapshot(t, ctx, s, columns))
		if _, e := s.Pool.Exec(ctx, "DROP TABLE configuration_audit"); e != nil {
			t.Fatal(e)
		}
	})
	if t.Failed() {
		t.FailNow()
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("actual populated018→current embedded migration upgrade", err)
	}
	after := configurationUpgradeSnapshot(t, ctx, s, columns)
	configurationUpgradeAssertPreserved(t, before, after)
	currentMigrations, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	var initialized bool
	if err = s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM configuration_revisions)=(SELECT count(*) FROM organizations)
 AND (SELECT count(*) FROM configuration_revisions WHERE revision=1)=(SELECT count(*) FROM organizations)
 AND NOT EXISTS(SELECT 1 FROM configuration_resource_bindings) AND NOT EXISTS(SELECT 1 FROM configuration_audit)
 AND (SELECT count(*) FROM schema_migrations)=$1`, len(currentMigrations)).Scan(&initialized); err != nil || !initialized {
		t.Fatal("upgrade did not initialize each organization at revision1 without fabricated bindings or audit", err)
	}
	for name, checksum := range checksums {
		var stored string
		if err = s.Pool.QueryRow(ctx, "SELECT sha256 FROM schema_migrations WHERE name=$1", name).Scan(&stored); err != nil || stored != checksum {
			t.Fatal("new migration acknowledgment mismatch", name, err)
		}
	}
	if current, e := s.GetTask(ctx, f.p, f.task.ID); e != nil || !reflect.DeepEqual(originalTask, current) {
		t.Fatal("old task/candidate projection is not readable exactly", e)
	}
	if current, e := s.GetTask(ctx, other, otherTask.ID); e != nil || !reflect.DeepEqual(otherTask, current) {
		t.Fatal("other organization's old queued task is not readable exactly", e)
	}
	if _, err = s.AuthenticateBrowserSession(ctx, session.TokenHash, f.p.OrganizationID); err != nil {
		t.Fatal("upgrade invalidated an admitted old browser credential", err)
	}
	models, _, err := s.Models(ctx, f.p, "", 100)
	if err != nil || len(models) != 1 || models[0].ID != f.model.ID || models[0].Version != 1 {
		t.Fatal("old unbound model disappeared or was implicitly rebound", err)
	}
	tools, _, err := s.Tools(ctx, f.p, "", 100)
	if err != nil || len(tools) != 1 || tools[0].ID != f.tool.Tool.ID || tools[0].Version != 1 {
		t.Fatal("old pinned tool metadata is not readable", err)
	}
	view, err := s.GetToolExecution(ctx, f.p, f.task.ID, tool.Execution.ID)
	if err != nil || !reflect.DeepEqual(tool, view) || view.Approval.DecisionMethod != "task_automation_grant" || view.Receipt == nil || view.Receipt.Method != "registered_tool_response" {
		t.Fatal("retired exact automatic approval or registered receipt changed", err)
	}
	archive, _, err := s.Events(ctx, f.p, f.task.ID, legacySequence-1, 10)
	if err != nil || len(archive) != 1 || archive[0].ID != legacyID || archive[0].PayloadVersion != 0 || archive[0].Data != nil || !strings.Contains(archive[0].LegacyDataJSON, "9007199254740993") {
		t.Fatal("wide historical opaque event is not readable losslessly", err)
	}
	snapshot, err := s.TaskEventSnapshot(ctx, f.p, f.task.ID)
	if err != nil || snapshot.Window.AfterSequence != floor || snapshot.Window.ThroughSequence != legacySequence {
		t.Fatal("old018 task replay window changed", err)
	}
	independentBalances(t, s, f.p.OrganizationID, []string{f.task.ID}, 720, 0)
	if f.modelCalls.Load() != 2 || f.readCalls.Load() != 1 {
		t.Fatal("migration or old-data reads resent a physical request")
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("repeat migration", err)
	}
	repeated := configurationUpgradeSnapshot(t, ctx, s, columns)
	if !reflect.DeepEqual(after, repeated) {
		t.Fatal("repeat migration changed old data or any upgraded trigger/function/sequence")
	}
	if filename := os.Getenv("PLATFORM_CONFIGURATION_UPGRADE_REPORT"); filename != "" {
		if !filepath.IsAbs(filename) {
			t.Fatal("upgrade report must be an explicit absolute new file")
		}
		report, e := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal("create new sanitized upgrade evidence", e)
		}
		encodeErr := json.NewEncoder(report).Encode(map[string]any{
			"result": "pass", "scope": "isolated populated018→019+020; not main, old binary, or backup recovery",
			"fixture": "runtime-created rows restricted to original018 columns; trigger suspension only during fixture copy; enabled before migration",
			"checks":  []string{"all_original_columns", "18_original_migration_rows_and_hashes", "old_trigger_definitions_and_enabled_state", "all_old_function_definitions", "sequence_value_and_is_called", "mid019_transaction_rollback", "all_org_revision1_backfill", "old_authenticated_reads", "retired_auto_approval", "registered_receipt", "wide_opaque_v0_event", "018_replay_window", "unknown_bill_hold720", "repeat_no_change", "no_external_resend"},
			"before":  before, "after": after, "applied_checksums": checksums,
			"physical_requests": map[string]int64{"model": f.modelCalls.Load(), "tool": f.readCalls.Load()},
		})
		closeErr := report.Close()
		if encodeErr != nil || closeErr != nil {
			t.Fatal("write sanitized upgrade evidence", encodeErr, closeErr)
		}
	}
	t.Logf("Populated018→current embedded migrations preserved all original columns of%d tables,18 acknowledgments,%d application triggers,%d SQL functions,%d sequences,720held,2 model/1 tool physical requests; rollback and repeat verified", len(before.Tables), len(before.Triggers), len(before.Functions), len(before.Sequences))
}
