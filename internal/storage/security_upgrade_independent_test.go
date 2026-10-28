// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// The target is built from immutable001–016, populated with only original
// writable columns from a valid current runtime image, and all original
// triggers are restored before Migrate. The old shared credential sequence is
// restored too. This is a real populated database upgrade, not an old binary
// compatibility or main database test.
func TestIndependentSecurityUpgradePreservesEveryOldColumnAndRetiredConsent(t *testing.T) {
	f := independentAutomationSetup(t, nil)
	f.executeModel(t)
	grant := f.grant(t, nil)
	f.reconcile(t)
	view := f.executeRead(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := f.s.RevokeAutomation(ctx, f.p, f.task.ID, domain.NewID("key"), domain.RevokeTaskAutomation{GrantID: grant.Grant.ID, ExpectedVersion: grant.Grant.Version, Reason: "Keep historical automatic read approval after retiring consent"}); err != nil {
		t.Fatal(err)
	}
	session := independentSession(t, f.s, f.p, "security-upgrade-existing-browser", time.Hour)
	retired := independentMemberCreate(t, f.s, f.p, "Existing revoked member credentials", "member")
	_, _ = independentMemberCredential(t, f.s, retired.OrganizationID, retired.PrincipalID)
	retired = independentMemberUpdate(t, f.s, f.p, retired, "member", false)
	_ = independentMemberUpdate(t, f.s, f.p, retired, "member", true)
	var sourceSchema string
	if err := f.s.Pool.QueryRow(ctx, "SELECT current_schema()").Scan(&sourceSchema); err != nil {
		t.Fatal(err)
	}
	targetSchema := domain.NewID("test_security_upgrade")
	if _, err := f.s.Pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{targetSchema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(f.dsn)
	if err != nil {
		t.Fatal("invalid isolated upgrade configuration")
	}
	q := u.Query()
	q.Set("search_path", targetSchema)
	u.RawQuery = q.Encode()
	s, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal("open disposable historical schema")
	}
	t.Cleanup(func() {
		s.Close()
		if _, e := f.s.Pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{targetSchema}.Sanitize()+" CASCADE"); e != nil {
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
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	staged := 0
	for _, name := range files {
		if name >= "migrations/017_" {
			continue
		}
		body, e := migrations.ReadFile(name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(ctx, string(body)); e != nil {
			t.Fatal("stage actual old migration", name, e)
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
	if staged != 16 {
		t.Fatal("fixture did not stage exact historical016", staged)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Pool.Query(ctx, "SELECT table_name,column_name,is_generated FROM information_schema.columns WHERE table_schema=$1 AND table_name<>'schema_migrations' ORDER BY table_name,ordinal_position", targetSchema)
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
	for table := range columns {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	quoteColumns := func(cols []string) string {
		out := make([]string, len(cols))
		for i, col := range cols {
			out[i] = pgx.Identifier{col}.Sanitize()
		}
		return strings.Join(out, ",")
	}
	restore, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer restore.Rollback(context.Background())
	for _, table := range tables {
		if _, err = restore.Exec(ctx, "ALTER TABLE "+pgx.Identifier{targetSchema, table}.Sanitize()+" DISABLE TRIGGER ALL"); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range tables {
		cols := quoteColumns(writable[table])
		if _, err = restore.Exec(ctx, "INSERT INTO "+pgx.Identifier{targetSchema, table}.Sanitize()+" ("+cols+") OVERRIDING SYSTEM VALUE SELECT "+cols+" FROM "+pgx.Identifier{sourceSchema, table}.Sanitize()); err != nil {
			t.Fatal("restore valid original columns", table, err)
		}
	}
	if _, err = restore.Exec(ctx, "SELECT setval('credential_origin_sequence',(SELECT last_value FROM "+pgx.Identifier{sourceSchema, "credential_origin_sequence"}.Sanitize()+"),true)"); err != nil {
		t.Fatal("restore credential origin sequence", err)
	}
	for _, table := range tables {
		if _, err = restore.Exec(ctx, "ALTER TABLE "+pgx.Identifier{targetSchema, table}.Sanitize()+" ENABLE TRIGGER ALL"); err != nil {
			t.Fatal(err)
		}
	}
	if err = restore.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	projection := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, table := range tables {
			var raw string
			if e := s.Pool.QueryRow(ctx, "SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb)::text FROM (SELECT "+quoteColumns(columns[table])+" FROM "+pgx.Identifier{targetSchema, table}.Sanitize()+") x").Scan(&raw); e != nil {
				t.Fatal(e)
			}
			out[table] = Hash([]byte(raw))
		}
		return out
	}
	before := projection()
	var oldTriggers []byte
	if err = s.Pool.QueryRow(ctx, "SELECT jsonb_agg(jsonb_build_array(c.relname,t.tgname,t.tgenabled) ORDER BY c.relname,t.tgname) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND NOT t.tgisinternal").Scan(&oldTriggers); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("actual populated016→017 upgrade rejected historical records", err)
	}
	if after := projection(); !reflect.DeepEqual(before, after) {
		for table, digest := range before {
			if after[table] != digest {
				t.Error("upgrade rewrote original row projection", table)
			}
		}
		t.FailNow()
	}
	var triggers [][]string
	if json.Unmarshal(oldTriggers, &triggers) != nil {
		t.Fatal("invalid original trigger inventory")
	}
	for _, trigger := range triggers {
		var enabled string
		if err = s.Pool.QueryRow(ctx, "SELECT t.tgenabled FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname=$1 AND t.tgname=$2", trigger[0], trigger[1]).Scan(&enabled); err != nil || enabled != trigger[2] {
			t.Fatal("original guard was lost or disabled", trigger[0], trigger[1], err)
		}
	}
	var valid bool
	if err = s.Pool.QueryRow(ctx, `SELECT (SELECT bool_and(kind='human' AND service_organization_id IS NULL) FROM principals)
 AND (SELECT bool_and(mode='mixed' AND human_authority_epoch=1 AND service_authority_epoch=1 AND human_credential_cutoff=0 AND service_credential_cutoff=0) FROM organization_login_policies)
 AND (SELECT a.status='approved' AND a.decision_method='task_automation_grant' AND e.status='succeeded' AND g.status='revoked' AND a.decider_policy_epoch=1 AND e.creator_policy_epoch=1 AND e.owner_policy_epoch=1 AND e.dispatcher_policy_epoch=1 FROM approvals a JOIN tool_executions e ON e.organization_id=a.organization_id AND e.id=a.execution_id JOIN task_automation_grants g ON g.organization_id=e.organization_id AND g.id=e.automation_grant_id WHERE e.organization_id=$1 AND e.id=$2)`, f.p.OrganizationID, view.Execution.ID).Scan(&valid); err != nil || !valid {
		t.Fatal("upgrade re-admitted retired consent or changed historical authority", err)
	}
	if _, err = s.AuthenticateBrowserSession(ctx, session.TokenHash, f.p.OrganizationID); err != nil {
		t.Fatal("upgrade invalidated a valid old browser origin", err)
	}
	independentMemberAuthority(t, s, f.p, "owner", false)
	independentBalances(t, s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("migration replay failed", err)
	}
	if !reflect.DeepEqual(before, projection()) {
		t.Fatal("migration replay changed original history")
	}
	t.Logf("Preserved every original column across %d historical tables and all %d original application triggers", len(tables), len(triggers))
}
