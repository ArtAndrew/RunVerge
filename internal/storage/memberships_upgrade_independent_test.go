// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"io/fs"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The current runtime requires016. Restore its valid row image, restricted to
// original columns, into a second schema built from the exact001–015 SQL files.
// Restore temporarily disables target fixture triggers; every original trigger
// is re-enabled before exercising migration or checking behavior. No application
// or main database schema is attached, altered or restored by this fixture.
func TestIndependentMembershipUpgradePreservesRetiredAutomationApprovalHistory(t *testing.T) {
	f := independentAutomationSetup(t, nil)
	f.executeModel(t)
	g := f.grant(t, nil)
	f.reconcile(t)
	view := f.executeRead(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if _, err := f.s.RevokeAutomation(ctx, f.p, f.task.ID, domain.NewID("key"), domain.RevokeTaskAutomation{GrantID: g.Grant.ID, ExpectedVersion: g.Grant.Version, Reason: "Retire durable consent while preserving historical automatic approval"}); err != nil {
		t.Fatal(err)
	}
	session := independentSession(t, f.s, f.p, "independent-upgrade-session", time.Hour)
	var proof bool
	if err := f.s.Pool.QueryRow(ctx, `SELECT a.status='approved' AND a.decision_method='task_automation_grant' AND e.status='succeeded' AND g.status='revoked' FROM approvals a JOIN tool_executions e ON e.organization_id=a.organization_id AND e.id=a.execution_id JOIN task_automation_grants g ON g.organization_id=e.organization_id AND g.id=e.automation_grant_id WHERE e.organization_id=$1 AND e.id=$2`, f.p.OrganizationID, view.Execution.ID).Scan(&proof); err != nil || !proof {
		t.Fatal("fixture does not contain a real retired-grant automatic approval", err)
	}
	var sourceSchema string
	if err := f.s.Pool.QueryRow(ctx, "SELECT current_schema()").Scan(&sourceSchema); err != nil {
		t.Fatal(err)
	}
	targetSchema := domain.NewID("test_membership_upgrade")
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
		t.Fatal("open disposable old schema")
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
	oldMigrations := 0
	for _, name := range files {
		if name >= "migrations/016_" {
			continue
		}
		body, e := migrations.ReadFile(name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(ctx, string(body)); e != nil {
			t.Fatal("stage exact immutable old migration", name, e)
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
		oldMigrations++
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Pool.Query(ctx, "SELECT table_name,column_name,is_generated FROM information_schema.columns WHERE table_schema=$1 AND table_name<>'schema_migrations' ORDER BY table_name,ordinal_position", targetSchema)
	if err != nil {
		t.Fatal(err)
	}
	columns := map[string][]string{}
	writableColumns := map[string][]string{}
	for rows.Next() {
		var table, column, generated string
		if err = rows.Scan(&table, &column, &generated); err != nil {
			t.Fatal(err)
		}
		columns[table] = append(columns[table], column)
		if generated == "NEVER" {
			writableColumns[table] = append(writableColumns[table], column)
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
	for _, name := range tables {
		if _, err = restore.Exec(ctx, "ALTER TABLE "+pgx.Identifier{targetSchema, name}.Sanitize()+" DISABLE TRIGGER ALL"); err != nil {
			t.Fatal("prepare disposable fixture restore", err)
		}
	}
	columnSQL := func(name string) string {
		quoted := make([]string, 0, len(columns[name]))
		for _, c := range columns[name] {
			quoted = append(quoted, pgx.Identifier{c}.Sanitize())
		}
		return strings.Join(quoted, ",")
	}
	for _, name := range tables {
		quoted := make([]string, 0, len(writableColumns[name]))
		for _, column := range writableColumns[name] {
			quoted = append(quoted, pgx.Identifier{column}.Sanitize())
		}
		cols := strings.Join(quoted, ",")
		if _, err = restore.Exec(ctx, "INSERT INTO "+pgx.Identifier{targetSchema, name}.Sanitize()+" ("+cols+") OVERRIDING SYSTEM VALUE SELECT "+cols+" FROM "+pgx.Identifier{sourceSchema, name}.Sanitize()); err != nil {
			t.Fatal("restore valid original row image", name, err)
		}
	}
	for _, name := range tables {
		if _, err = restore.Exec(ctx, "ALTER TABLE "+pgx.Identifier{targetSchema, name}.Sanitize()+" ENABLE TRIGGER ALL"); err != nil {
			t.Fatal(err)
		}
	}
	if err = restore.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	projection := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, name := range tables {
			var raw string
			if e := s.Pool.QueryRow(ctx, "SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb)::text FROM (SELECT "+columnSQL(name)+" FROM "+pgx.Identifier{targetSchema, name}.Sanitize()+") x").Scan(&raw); e != nil {
				t.Fatal(e)
			}
			out[name] = Hash([]byte(raw))
		}
		return out
	}
	before := projection()
	// Reconstruct the identified defective backfill locally. Historical approval
	// shape must not be re-admitted against its now-revoked grant. Failure rolls
	// back all new DDL before the real current Migrate is exercised.
	body, err := migrations.ReadFile("migrations/016_organization_memberships.sql")
	if err != nil {
		t.Fatal(err)
	}
	baseline := strings.ReplaceAll(string(body), "ALTER TABLE approvals DISABLE TRIGGER tool_approval_shape_guard;", "")
	baseline = strings.ReplaceAll(baseline, "ALTER TABLE approvals ENABLE TRIGGER tool_approval_shape_guard;", "")
	if baseline == string(body) {
		t.Fatal("regression fixture did not remove exactly the identified backfill protection")
	}
	bad, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, baselineErr := bad.Exec(ctx, baseline)
	_ = bad.Rollback(context.Background())
	var pgError *pgconn.PgError
	if !errors.As(baselineErr, &pgError) || pgError.Code != "23514" || !strings.Contains(pgError.Message, "automatic approval requires current narrow read grant") {
		t.Fatal("defective baseline did not reproduce the historical approval admission failure", baselineErr)
	}
	afterFailed := projection()
	for name, digest := range before {
		if afterFailed[name] != digest {
			t.Fatal("failed migration changed old history", name)
		}
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("actual populated015 to016 upgrade rejected retired approval history", err)
	}
	after := projection()
	for name, digest := range before {
		if after[name] != digest {
			t.Fatal("upgrade changed original row image or immutable JSON", name)
		}
	}
	var count, disabled int
	var privateEpochs bool
	err = s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM schema_migrations),(SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND t.tgname IN('tool_execution_guard','tool_approval_guard','tool_approval_shape_guard') AND t.tgenabled<>'O'),(SELECT a.decider_authority_epoch=1 AND e.owner_authority_epoch=1 AND e.creator_authority_epoch=1 AND e.dispatcher_authority_epoch=1 FROM approvals a JOIN tool_executions e ON e.organization_id=a.organization_id AND e.id=a.execution_id WHERE e.organization_id=$1 AND e.id=$2)`, f.p.OrganizationID, view.Execution.ID).Scan(&count, &disabled, &privateEpochs)
	currentMigrations, globErr := fs.Glob(migrations, "migrations/*.sql")
	if err != nil || globErr != nil || count != len(currentMigrations) || count <= oldMigrations || disabled != 0 || !privateEpochs {
		t.Fatal("upgrade did not restore old guards or exact private authority backfill", count, disabled, privateEpochs, err)
	}
	var originalGuards int
	if err = s.Pool.QueryRow(ctx, "SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND t.tgname IN('tool_execution_guard','tool_approval_guard','tool_approval_shape_guard')").Scan(&originalGuards); err != nil || originalGuards != 3 {
		t.Fatal("original historical guards missing after upgrade", originalGuards, err)
	}
	if _, err = s.AuthenticateBrowserSession(ctx, session.TokenHash, f.p.OrganizationID); err != nil {
		t.Fatal("legacy browser credential lost valid access at upgrade", err)
	}
	independentMemberAuthority(t, s, f.p, "owner", false)
	independentBalances(t, s, f.p.OrganizationID, []string{f.task.ID}, 420, 0)
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("replayed current migration changed history", err)
	}
	t.Logf("Preserved every old-column row projection across %d populated original tables; reproduced23514 baseline rolled back; all3 original guards restored", len(tables))
}
