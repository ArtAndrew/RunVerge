// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func independentEventStoreAt017(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for isolated populated017 upgrade")
	}
	base, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal("historical test database unavailable")
	}
	schema := domain.NewID("test_events_upgrade")
	if _, err = base.Pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		base.Close()
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("invalid isolated upgrade configuration")
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	s, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal("cannot open historical schema")
	}
	t.Cleanup(func() {
		s.Close()
		_, err := base.Pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		base.Close()
		if err != nil {
			t.Error(err)
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
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	staged := 0
	for _, name := range files {
		if name >= "migrations/018_" {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, string(body)); err != nil {
			t.Fatal("stage actual immutable migration", name, err)
		}
		if _, err = tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "SET CONSTRAINTS ALL DEFERRED"); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(name,sha256) VALUES($1,$2)", name, Hash(body)); err != nil {
			t.Fatal(err)
		}
		staged++
	}
	if staged != 17 {
		t.Fatal("fixture did not construct exact017 schema", staged)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIndependentEventMigrationPreservesPopulated017HistoryAndGuards(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := independentEventStoreAt017(t, ctx)
	var p domain.Principal
	var a domain.Agent
	// Populate exact017 using current constructors only during this test-only
	// emulation of the old gate's absence; remove it before original snapshots.
	historicalFixtureWithoutRecoveryAdmission(t, s, func() {
		p, _ = identity(t, s, 100000)
		a = agent(t, s, p) // These commands do not require the new event column.
	})
	unknown := independentLease(t, s, p, a, "", 1000)
	quiet := independentLease(t, s, p, a, "", 1000)
	const held, spent int64 = 17, 19
	type occurrence struct {
		id, action, status string
		maximum            int64
		actual             any
	}
	unknownReservation := domain.NewID("res")
	for _, item := range []occurrence{
		{unknownReservation, domain.NewID("act"), "unknown", held, nil},
		{domain.NewID("res"), domain.NewID("act"), "settled", 25, spent},
	} {
		actionStatus := "unknown"
		if item.status == "settled" {
			actionStatus = "confirmed"
		}
		if _, err := s.Pool.Exec(ctx, `INSERT INTO actions(organization_id,id,task_id,operation_id,intent_hash,idempotency_key,status,
 dispatch_issued_at,dispatch_not_after,dispatch_fencing_token)
 VALUES($1,$2,$3,$4,$5,$2,$6,clock_timestamp(),clock_timestamp()+interval '1 second',1)`, p.OrganizationID, item.action, unknown.TaskID, unknown.OperationID, Hash([]byte("historical exact request "+item.id)), actionStatus); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(ctx, `INSERT INTO reservations(organization_id,id,task_id,operation_id,action_id,category,provider,price_version,amount_microusd,actual_microusd,status)
 VALUES($1,$2,$3,$4,$5,'model','independent-historical-account','historical-price-1',$6,$7,$8)`, p.OrganizationID, item.id, unknown.TaskID, unknown.OperationID, item.action, item.maximum, item.actual, item.status); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Pool.Exec(ctx, `INSERT INTO ledger_entries(organization_id,id,reservation_id,task_id,root_id,event_key,event_hash,kind,delta_reserved_microusd,delta_spent_microusd)
 VALUES($1,$2,$3,$4,$4,'reserve','historical-reserve','reserve',$5,0)`, p.OrganizationID, domain.NewID("led"), item.id, unknown.TaskID, item.maximum); err != nil {
			t.Fatal(err)
		}
		if item.status == "settled" {
			if _, err := s.Pool.Exec(ctx, `INSERT INTO ledger_entries(organization_id,id,reservation_id,task_id,root_id,event_key,event_hash,kind,delta_reserved_microusd,delta_spent_microusd)
 VALUES($1,$2,$3,$4,$4,'settle','historical-settlement','settle',-25,$5)`, p.OrganizationID, domain.NewID("led"), item.id, unknown.TaskID, spent); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Pool.Exec(ctx, `INSERT INTO cost_evidence(organization_id,source,reference,reservation_id,payload_digest,actual_microusd,reason,id,request_hash)
 VALUES($1,'independent-historical-account','historical-statement',$2,$3,$4,'Historical local fixture, not external provider proof',$5,$6)`, p.OrganizationID, item.id, Hash([]byte("historical statement")), spent, domain.NewID("evi"), Hash([]byte("historical evidence request"))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE organizations SET reserved_microusd=$2,spent_microusd=$3 WHERE id=$1", p.OrganizationID, held, spent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE tasks SET reserved_microusd=$2,spent_microusd=$3,status='blocked',waiting_reason='historical_unknown_cost' WHERE organization_id=$1 AND id=$4", p.OrganizationID, held, spent, unknown.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE operations SET status='blocked',lease_owner=NULL,lease_until=NULL WHERE organization_id=$1 AND id=$2", p.OrganizationID, unknown.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE runs SET status='blocked' WHERE organization_id=$1 AND id=$2", p.OrganizationID, unknown.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO idempotency_keys(organization_id,principal_id,scope,key,request_hash,response)
 VALUES($1,$2,'POST /historical-receipt','old-command',$3,'{"id":"old-reply","actual_microusd":19,"wide":9007199254740993}')`, p.OrganizationID, p.ID, Hash([]byte("historical command"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "SELECT setval(pg_get_serial_sequence('events','sequence'),9007199254740992,true)"); err != nil {
		t.Fatal(err)
	}
	legacyJSON := `{"wide":9007199254740993,"historical_extra":true}`
	eventIDs := []string{domain.NewID("evt"), domain.NewID("evt")}
	for i, id := range eventIDs {
		kind := "task.created"
		if i == 1 {
			kind = "old.provider-specific-kind"
		}
		if _, err := s.Pool.Exec(ctx, `WITH event AS (INSERT INTO events(organization_id,id,task_id,type,data)
 VALUES($1,$2,$3,$4,$5) RETURNING sequence,organization_id)
 INSERT INTO outbox(sequence,organization_id,delivered_at)
 SELECT sequence,organization_id,CASE WHEN $6::boolean THEN clock_timestamp() ELSE NULL END FROM event`, p.OrganizationID, id, unknown.TaskID, kind, legacyJSON, i == 0); err != nil {
			t.Fatal(err)
		}
	}
	// Capture the complete original column sets before018 exists. No old guard
	// was disabled to construct this populated fixture.
	columns := map[string][]string{}
	rows, err := s.Pool.Query(ctx, `SELECT table_name,column_name FROM information_schema.columns
 WHERE table_schema=current_schema() AND table_name<>'schema_migrations' ORDER BY table_name,ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var table, col string
		if err = rows.Scan(&table, &col); err != nil {
			t.Fatal(err)
		}
		columns[table] = append(columns[table], col)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	project := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for table, cols := range columns {
			quoted := make([]string, len(cols))
			for i, col := range cols {
				quoted[i] = pgx.Identifier{col}.Sanitize()
			}
			var body string
			if err := s.Pool.QueryRow(ctx, "SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb)::text FROM (SELECT "+strings.Join(quoted, ",")+" FROM "+pgx.Identifier{table}.Sanitize()+") x").Scan(&body); err != nil {
				t.Fatal(err)
			}
			out[table] = Hash([]byte(body))
		}
		var acknowledgments string
		if err := s.Pool.QueryRow(ctx, "SELECT jsonb_agg(to_jsonb(x) ORDER BY name)::text FROM schema_migrations x WHERE name<'migrations/018_'").Scan(&acknowledgments); err != nil {
			t.Fatal(err)
		}
		out["schema_migrations_original_rows"] = Hash([]byte(acknowledgments))
		return out
	}
	guards := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		r, err := s.Pool.Query(ctx, `SELECT c.relname,t.tgname,t.tgenabled,pg_get_triggerdef(t.oid),pg_get_functiondef(t.tgfoid)
 FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=current_schema() AND NOT t.tgisinternal ORDER BY c.relname,t.tgname`)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		for r.Next() {
			var table, name, enabled, definition, function string
			if err = r.Scan(&table, &name, &enabled, &definition, &function); err != nil {
				t.Fatal(err)
			}
			out[table+"."+name] = Hash([]byte(enabled + "\n" + definition + "\n" + function))
		}
		if err = r.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	sequences := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		r, err := s.Pool.Query(ctx, "SELECT relname FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND relkind='S' ORDER BY relname")
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for r.Next() {
			var name string
			if err = r.Scan(&name); err != nil {
				t.Fatal(err)
			}
			names = append(names, name)
		}
		if err = r.Err(); err != nil {
			t.Fatal(err)
		}
		r.Close()
		for _, name := range names {
			var state string
			if err := s.Pool.QueryRow(ctx, "SELECT last_value::text||':'||is_called::text FROM "+pgx.Identifier{name}.Sanitize()).Scan(&state); err != nil {
				t.Fatal(err)
			}
			out[name] = state
		}
		return out
	}
	before, oldGuards, oldSequences := project(), guards(), sequences()
	if len(columns) != 50 || len(oldGuards) < 80 || len(oldSequences) != 2 {
		t.Fatal("historical fixture did not cover the expected017 inventory", len(columns), len(oldGuards), len(oldSequences))
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("actual populated017→018 migration failed", err)
	}
	if !reflect.DeepEqual(before, project()) || !reflect.DeepEqual(oldSequences, sequences()) {
		t.Fatal("upgrade rewrote original row projections or sequence states")
	}
	afterGuards := guards()
	for key, digest := range oldGuards {
		if afterGuards[key] != digest {
			t.Fatal("upgrade replaced, removed or disabled an original guard/function", key)
		}
	}
	var windows, tasks, zeroWindows, versionZero, historical, audits int
	if err = s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM task_event_windows),(SELECT count(*) FROM tasks),
 (SELECT count(*) FROM task_event_windows WHERE after_sequence=0 AND version=1),
 (SELECT count(*) FROM events WHERE payload_version=0),(SELECT count(*) FROM events),
 (SELECT count(*) FROM event_replay_window_audit)`).Scan(&windows, &tasks, &zeroWindows, &versionZero, &historical, &audits); err != nil || windows != tasks || zeroWindows != tasks || historical != 2 || versionZero != historical || audits != 0 {
		t.Fatal("migration relabeled old events or skipped a task window", err)
	}
	archive, _, err := s.Events(ctx, p, unknown.TaskID, 0, 100)
	if err != nil || len(archive) != 2 {
		t.Fatal("upgraded historical event read failed", err)
	}
	for _, event := range archive {
		var raw string
		if err = s.Pool.QueryRow(ctx, "SELECT data::text FROM events WHERE organization_id=$1 AND id=$2", p.OrganizationID, event.ID).Scan(&raw); err != nil || event.PayloadVersion != 0 || event.Data != nil || event.LegacyDataJSON != raw || !strings.Contains(raw, "9007199254740993") {
			t.Fatal("historical payload was inferred, rounded or misrepresented as typed v1", err)
		}
	}
	if err = s.Migrate(ctx); err != nil || !reflect.DeepEqual(before, project()) || !reflect.DeepEqual(oldSequences, sequences()) {
		t.Fatal("migration replay changed original history", err)
	}
	if _, err = s.CancelTask(ctx, p, quiet.TaskID, domain.NewID("key")); err != nil {
		t.Fatal("new task window did not support an ordinary post-upgrade command", err)
	}
	var newVersion int
	var newSequence int64
	if err = s.Pool.QueryRow(ctx, "SELECT payload_version,sequence FROM events WHERE organization_id=$1 AND task_id=$2", p.OrganizationID, quiet.TaskID).Scan(&newVersion, &newSequence); err != nil || newVersion != 1 || newSequence != 9007199254740995 {
		t.Fatal("post-upgrade event did not distinguish typed v1 or preserve identity sequence", err)
	}
	beforeAdvance := project()
	floor := archive[0].Sequence
	if _, err = s.AdvanceEventReplayWindow(ctx, p.OrganizationID, unknown.TaskID, floor, "Independent historical replay retention"); err != nil || !reflect.DeepEqual(beforeAdvance, project()) {
		t.Fatal("logical retention rewrote old tables or financial/dedup obligations", err)
	}
	for name, statement := range map[string]string{
		"rewind":          "UPDATE task_event_windows SET after_sequence=0 WHERE task_id=$1",
		"future":          "UPDATE task_event_windows SET after_sequence=9223372036854775807 WHERE task_id=$1",
		"timestamp":       "UPDATE task_event_windows SET after_sequence=after_sequence+1,updated_at=clock_timestamp() WHERE task_id=$1",
		"delete_window":   "DELETE FROM task_event_windows WHERE task_id=$1",
		"delete_history":  "DELETE FROM events WHERE task_id=$1",
		"relabel_history": "UPDATE events SET payload_version=1 WHERE task_id=$1",
		"delete_audit":    "DELETE FROM event_replay_window_audit WHERE task_id=$1",
		"change_audit":    "UPDATE event_replay_window_audit SET reason='rewrite' WHERE task_id=$1",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.Pool.Exec(ctx, statement, unknown.TaskID)
			var rejection *pgconn.PgError
			if !errors.As(err, &rejection) || rejection.Code != "23514" {
				t.Fatal("historical/window mutation did not fail at its durable guard", err)
			}
		})
	}
	if !reflect.DeepEqual(beforeAdvance, project()) {
		t.Fatal("rejected guard probes modified original facts")
	}
	independentBalances(t, s, p.OrganizationID, []string{unknown.TaskID}, held, spent)
	var unresolved bool
	if err = s.Pool.QueryRow(ctx, "SELECT status='unknown' AND amount_microusd=17 AND actual_microusd IS NULL FROM reservations WHERE organization_id=$1 AND id=$2", p.OrganizationID, unknownReservation).Scan(&unresolved); err != nil || !unresolved {
		t.Fatal("historical unknown obligation was silently settled/released", err)
	}
	t.Logf("Actual017→018 preserved every original column across %d tables, %d original trigger/function/enabled definitions, %d identity/origin sequences, wide historical v0 JSON, pending outbox, dedup replies and17held/19spent", len(columns), len(oldGuards), len(oldSequences))
}
