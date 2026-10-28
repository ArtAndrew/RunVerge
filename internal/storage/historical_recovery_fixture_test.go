// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

var historicalRecoveryFixtureSchema = regexp.MustCompile(`^(test(?:_[a-z0-9]+)*|independent_upgrade)_[a-f0-9]{32}$`)

// This is a test fixture boundary, not an old binary, runtime fallback or import
// privilege. Constructors from the current binary can populate a deliberately
// staged immutable old schema only during the callback. Never use it to test
// missing-control admission, after022, during upgrade, or in production code.
func historicalFixtureWithoutRecoveryAdmission(t *testing.T, s *Store, populate func()) {
	t.Helper()
	ctx := context.Background()
	prefix, err := historicalRecoveryFixturePrefix(ctx, s)
	if err != nil {
		t.Fatal("historical constructor fixture refused", err)
	}
	name := pgx.Identifier{s.runtimeSchema, "runtime_recovery_admit"}.Sanitize()
	if _, err := s.Pool.Exec(ctx, "CREATE FUNCTION "+name+"(boolean) RETURNS void LANGUAGE plpgsql AS $$BEGIN RETURN; END$$"); err != nil {
		t.Fatal("create temporary historical constructor fixture", err)
	}
	removed := false
	remove := func() error {
		if removed {
			return nil
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := s.Pool.Exec(cleanup, "DROP FUNCTION "+name+"(boolean)"); err != nil {
			return err
		}
		removed = true
		return nil
	}
	// Defer covers panic, t.Fatal/t.Skip and Goexit as well as ordinary returns.
	// A t.Cleanup-only shim would remain present during snapshots/Migrate.
	defer func() {
		if err := remove(); err != nil {
			t.Error("remove historical constructor fixture", err)
		}
	}()
	populate()
	if err := remove(); err != nil {
		t.Fatal("remove historical constructor fixture before snapshots/upgrade", err)
	}
	after, err := historicalRecoveryFixturePrefix(ctx, s)
	if err != nil || after != prefix {
		t.Fatal("historical callback changed migration history or retained a gate", err)
	}
}

func historicalRecoveryFixturePrefix(ctx context.Context, s *Store) (string, error) {
	if os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" || !historicalRecoveryFixtureSchema.MatchString(s.runtimeSchema) {
		return "", fmt.Errorf("requires an explicitly owned cluster and isolated historical schema")
	}
	var clean bool
	err := s.Pool.QueryRow(ctx, `SELECT pg_catalog.current_schema()=$1
 AND pg_catalog.to_regclass($2) IS NULL
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname=$1 AND p.proname LIKE 'runtime_recovery_%')`, s.runtimeSchema,
		pgx.Identifier{s.runtimeSchema, "runtime_recovery_control"}.Sanitize()).Scan(&clean)
	if err != nil {
		return "", err
	}
	if !clean {
		return "", fmt.Errorf("historical schema already has recovery authority or a different lookup")
	}
	rows, err := s.Pool.Query(ctx, "SELECT name,sha256 FROM "+pgx.Identifier{s.runtimeSchema, "schema_migrations"}.Sanitize()+" ORDER BY name")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	expected := EmbeddedSchemaIdentity()
	var prefix []string
	for rows.Next() {
		var name, digest string
		if err := rows.Scan(&name, &digest); err != nil {
			return "", err
		}
		i := len(prefix)
		if i >= len(expected) || i >= 21 || name >= "migrations/022_" || expected[i].Name != name || expected[i].SHA256 != digest {
			return "", fmt.Errorf("requires an exact immutable prefix before022")
		}
		prefix = append(prefix, name+":"+digest)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(prefix) == 0 {
		return "", fmt.Errorf("historical schema has no immutable migration prefix")
	}
	return Hash([]byte(strings.Join(prefix, "\n"))), nil
}

func TestHistoricalRecoveryFixtureIsTemporaryAndRejectsCurrentSchema(t *testing.T) {
	if os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" {
		t.Skip("explicit owned temporary cluster required for historical constructors")
	}
	ctx := context.Background()
	old := independentStoreAt003(t)
	if err := old.CheckRuntimeAdmission(ctx); !errors.Is(err, RecoveryHeld) {
		t.Fatal("current runtime did not reject original missing recovery control", err)
	}
	historicalFixtureWithoutRecoveryAdmission(t, old, func() {
		if err := old.CheckRuntimeAdmission(ctx); err != nil {
			t.Fatal("explicit temporary historical constructor did not work", err)
		}
	})
	if err := old.CheckRuntimeAdmission(ctx); !errors.Is(err, RecoveryHeld) {
		t.Fatal("historical constructor left a runtime bypass", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		historicalFixtureWithoutRecoveryAdmission(t, old, func() { runtime.Goexit() })
	}()
	select {
	case <-done:
	case <-time.After(12 * time.Second):
		t.Fatal("Goexit did not finish historical fixture cleanup")
	}
	if _, err := historicalRecoveryFixturePrefix(ctx, old); err != nil {
		t.Fatal("Goexit retained historical constructor authority", err)
	}
	current, _ := testStore(t)
	if _, err := historicalRecoveryFixturePrefix(ctx, current); err == nil {
		t.Fatal("historical constructor accepted the current schema/control")
	}
}
