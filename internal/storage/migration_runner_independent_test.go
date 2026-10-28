// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// This independent test injects a DDL name conflict only in a disposable schema.
// It never edits an embedded/applied migration or corrupts production history.
func TestIndependentMigrationRunnerRollsBackPartialUpgradeAndSerializesReplay(t *testing.T) {
	s := independentStoreAt003(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	stage, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Rollback(ctx)
	for _, name := range files {
		if name <= "migrations/003_execution_ledger.sql" || name >= "migrations/014_" {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = stage.Exec(ctx, string(body)); err != nil {
			t.Fatal("stage immutable old schema", name, err)
		}
		if _, err = stage.Exec(ctx, "INSERT INTO schema_migrations(name,sha256) VALUES($1,$2)", name, Hash(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err = stage.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, "CREATE TABLE task_automation_grants(independent_fixture_marker integer NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, "INSERT INTO task_automation_grants(independent_fixture_marker) VALUES(17)"); err != nil {
		t.Fatal(err)
	}
	err = s.Migrate(ctx)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "42P07" || !strings.Contains(err.Error(), "015_task_automation.sql") {
		t.Fatal("intentional migration015 fixture conflict did not produce the expected failure", err)
	}
	var acknowledged, oldCount, marker int
	var modelTable, modelColumn bool
	err = s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM schema_migrations WHERE name>='migrations/014_'),(SELECT count(*) FROM schema_migrations),to_regclass('model_turns') IS NOT NULL,EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='agents' AND column_name='model_tools'),(SELECT independent_fixture_marker FROM task_automation_grants)`).Scan(&acknowledged, &oldCount, &modelTable, &modelColumn, &marker)
	if err != nil || acknowledged != 0 || oldCount != 13 || modelTable || modelColumn || marker != 17 {
		t.Fatal("failed015 committed partial014 DDL/checksum or altered pre-existing fixture", acknowledged, oldCount, modelTable, modelColumn, marker, err)
	}
	// Recover only the intentional test-owned blocker. The first upgrade must
	// have rolled back everything else, allowing normal replay without repair.
	if _, err = s.Pool.Exec(ctx, "DROP TABLE task_automation_grants"); err != nil {
		t.Fatal(err)
	}
	const callers = 2
	var wg sync.WaitGroup
	results := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.Migrate(ctx) }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal("concurrent migration replay was not serialized", err)
		}
	}
	var count int
	if err = s.Pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil || count != len(files) {
		t.Fatal("successful replay did not acknowledge each exact file once", count, len(files), err)
	}
	for _, name := range files {
		body, err := migrations.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		var checksum string
		if err = s.Pool.QueryRow(ctx, "SELECT sha256 FROM schema_migrations WHERE name=$1", name).Scan(&checksum); err != nil || checksum != Hash(body) {
			t.Fatal("migration replay changed or skipped exact historical checksum", name, err)
		}
	}
	var complete bool
	err = s.Pool.QueryRow(ctx, `SELECT to_regclass('model_turns') IS NOT NULL AND to_regclass('task_automation_grants') IS NOT NULL AND EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='agents' AND column_name='model_tools') AND NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='task_automation_grants' AND column_name='independent_fixture_marker')`).Scan(&complete)
	if err != nil || !complete {
		t.Fatal("atomic replay did not install the actual complete schema", err)
	}
}
