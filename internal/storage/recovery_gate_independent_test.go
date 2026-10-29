// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// An inherited developer DSN is not authorization to exercise recovery. These
// tests additionally require the integrator's explicit owned-cluster opt-in;
// testStore still creates/drops a separate schema for each top-level case.
func independentRecoveryStore(t *testing.T) (*Store, string) {
	t.Helper()
	if os.Getenv("RUNVERGE_RECOVERY_INDEPENDENT_OWNED_PG") != "1" {
		t.Skip("explicit owned temporary recovery cluster required")
	}
	return testStore(t)
}

func independentRecoverySpec(t *testing.T, s *Store) RecoveryTargetSpec {
	t.Helper()
	state, err := s.InspectRecoveryTarget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return RecoveryTargetSpec{
		TargetInstallationID: state.InstallationID,
		RestoreID:            "restore_" + strings.Repeat("a", 32),
		BackupID:             "bak_" + strings.Repeat("b", 32),
		SourceID:             "inst_" + strings.Repeat("c", 32),
		CutUTC:               "2026-10-27T12:34:56.123456Z",
		SchemaSHA256:         EmbeddedSchemaSHA256(),
	}
}

func independentRecoverySQLState(t *testing.T, err error, code string) {
	t.Helper()
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != code {
		t.Fatalf("wanted SQLSTATE %s, got %v", code, err)
	}
}

func independentRecoverySetHeld(ctx context.Context, tx pgx.Tx, schema string, spec RecoveryTargetSpec) error {
	cut, err := time.Parse(time.RFC3339Nano, spec.CutUTC)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE "+pgx.Identifier{schema, "runtime_recovery_control"}.Sanitize()+
		" SET state='held',restore_id=$1,backup_id=$2,source_id=$3,cut_at=$4,schema_sha256=$5 WHERE singleton",
		spec.RestoreID, spec.BackupID, spec.SourceID, cut, spec.SchemaSHA256)
	return err
}

func independentRecoveryWaitAdvisory(t *testing.T, s *Store, ctx context.Context, pid uint32) {
	t.Helper()
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting bool
		var err error
		if pid != 0 {
			err = s.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND NOT granted)", pid).Scan(&waiting)
		} else {
			err = s.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND classid=841932080 AND objid=hashtext($1)::oid AND NOT granted)", s.runtimeSchema).Scan(&waiting)
		}
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("context expired before an actual advisory wait")
		case <-deadline.C:
			t.Fatal("did not observe the required advisory wait")
		case <-tick.C:
		}
	}
}

func TestRecoveryGateIndependentAllRelationsAndRawStatements(t *testing.T) {
	s, _ := independentRecoveryStore(t)
	ctx := context.Background()
	rows, err := s.Pool.Query(ctx, `SELECT c.relname,a.attname,
 CASE WHEN c.relname IN('recovery_import_receipts','recovery_historical_rows') THEN
 EXISTS(SELECT 1 FROM pg_trigger t JOIN pg_proc f ON f.oid=t.tgfoid WHERE t.tgrelid=c.oid AND t.tgname IN('recovery_receipt_insert','recovery_mark_insert') AND t.tgenabled='O' AND t.tgtype=7 AND NOT t.tgisinternal AND f.pronamespace=c.relnamespace AND f.proname='recovery_import_metadata_guard')
 AND EXISTS(SELECT 1 FROM pg_trigger t JOIN pg_proc f ON f.oid=t.tgfoid WHERE t.tgrelid=c.oid AND t.tgname IN('recovery_receipt_permanent','recovery_mark_permanent') AND t.tgenabled='O' AND t.tgtype=58 AND NOT t.tgisinternal AND f.pronamespace=c.relnamespace AND f.proname='recovery_import_metadata_guard')
 ELSE EXISTS(SELECT 1 FROM pg_trigger t JOIN pg_proc f ON f.oid=t.tgfoid
 WHERE t.tgrelid=c.oid AND t.tgname='recovery_runtime_write' AND t.tgenabled='O'
 AND t.tgtype=62 AND NOT t.tgisinternal AND f.pronamespace=c.relnamespace AND f.proname='runtime_recovery_write_guard') END
 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 JOIN LATERAL (SELECT attname FROM pg_attribute WHERE attrelid=c.oid AND attnum>0 AND NOT attisdropped AND attidentity<>'a' AND attgenerated='' ORDER BY attnum LIMIT 1) a ON true
 WHERE n.nspname=$1 AND c.relkind IN('r','p') AND c.relname NOT IN('schema_migrations','runtime_recovery_control') ORDER BY c.relname`, s.runtimeSchema)
	if err != nil {
		t.Fatal(err)
	}
	type relation struct{ name, column string }
	var relations []relation
	for rows.Next() {
		var r relation
		var guarded bool
		if err := rows.Scan(&r.name, &r.column, &guarded); err != nil {
			t.Fatal(err)
		}
		if !guarded {
			t.Errorf("business relation %s lacks complete statement guard", r.name)
		}
		relations = append(relations, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(relations) < 60 {
		t.Fatal("unexpectedly narrow business inventory", len(relations))
	}
	if _, err := s.PrepareRecoveryTarget(ctx, independentRecoverySpec(t, s)); err != nil {
		t.Fatal(err)
	}
	for _, r := range relations {
		t.Run(r.name, func(t *testing.T) {
			name := pgx.Identifier{s.runtimeSchema, r.name}.Sanitize()
			column := pgx.Identifier{r.column}.Sanitize()
			for _, statement := range []string{
				"INSERT INTO " + name + " DEFAULT VALUES",
				"UPDATE " + name + " SET " + column + "=" + column + " WHERE false",
				"DELETE FROM " + name + " WHERE false",
				"TRUNCATE " + name + " CASCADE",
			} {
				_, err := s.Pool.Exec(ctx, statement)
				independentRecoverySQLState(t, err, "55000")
			}
		})
	}
	for _, statement := range []string{
		"DELETE FROM runtime_recovery_control",
		"TRUNCATE runtime_recovery_control",
		"INSERT INTO runtime_recovery_control(singleton) VALUES(true)",
		"UPDATE runtime_recovery_control SET state='normal',restore_id=NULL,backup_id=NULL,source_id=NULL,cut_at=NULL,schema_sha256=NULL,held_at=NULL",
		"UPDATE runtime_recovery_control SET restore_id='restore_" + strings.Repeat("d", 32) + "'",
	} {
		_, err := s.Pool.Exec(ctx, statement)
		independentRecoverySQLState(t, err, "55000")
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("migration replay unexpectedly failed", err)
	}
	if err := s.CheckRuntimeAdmission(ctx); !errors.Is(err, RecoveryHeld) {
		t.Fatal("migration replay cleared hold", err)
	}
	t.Logf("checked four raw statement classes on %d business relations", len(relations))
}

func TestRecoveryGateIndependentMissingControlIsNotReinitialized(t *testing.T) {
	s, _ := independentRecoveryStore(t)
	ctx := context.Background()
	// Deliberate isolated owner corruption, not an implemented import privilege.
	if _, err := s.Pool.Exec(ctx, "ALTER TABLE runtime_recovery_control DISABLE TRIGGER recovery_control_statement"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "DELETE FROM runtime_recovery_control"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "ALTER TABLE runtime_recovery_control ENABLE TRIGGER recovery_control_statement"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckRuntimeAdmission(ctx); !errors.Is(err, RecoveryHeld) {
		t.Fatal("missing singleton admitted", err)
	}
	if _, err := s.Bootstrap(ctx, "must stay absent", 1); !errors.Is(err, RecoveryHeld) {
		t.Fatal("missing singleton admitted host issuance", err)
	}
	_, err := s.Pool.Exec(ctx, "UPDATE organizations SET name=name WHERE false")
	independentRecoverySQLState(t, err, "55000")
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM runtime_recovery_control").Scan(&count); err != nil || count != 0 {
		t.Fatal("migration silently recreated missing authority", count, err)
	}
	if err := s.CheckRuntimeAdmission(ctx); !errors.Is(err, RecoveryHeld) {
		t.Fatal("migration repaired missing singleton to normal", err)
	}
}

func TestRecoveryGateIndependentCallerAndTemporaryLookalikes(t *testing.T) {
	s, _ := independentRecoveryStore(t)
	ctx := context.Background()
	if _, err := s.PrepareRecoveryTarget(ctx, independentRecoverySpec(t, s)); err != nil {
		t.Fatal(err)
	}
	fake := domain.NewID("lookalike")
	if _, err := s.Pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{fake}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := s.Pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{fake}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, statement := range []string{
		"CREATE TABLE " + pgx.Identifier{fake, "runtime_recovery_control"}.Sanitize() + "(singleton boolean,state text)",
		"INSERT INTO " + pgx.Identifier{fake, "runtime_recovery_control"}.Sanitize() + " VALUES(true,'normal')",
		"CREATE FUNCTION " + pgx.Identifier{fake, "runtime_recovery_admit"}.Sanitize() + "(boolean) RETURNS void LANGUAGE plpgsql AS $$BEGIN RETURN; END$$",
		"CREATE FUNCTION " + pgx.Identifier{fake, "format"}.Sanitize() + "(text,text) RETURNS text LANGUAGE sql AS $$SELECT 'SELECT 1'::text$$",
		"CREATE FUNCTION " + pgx.Identifier{fake, "format"}.Sanitize() + "(text,name) RETURNS text LANGUAGE sql AS $$SELECT 'SELECT 1'::text$$",
		"CREATE TEMP TABLE runtime_recovery_control(singleton boolean,state text)",
		"INSERT INTO pg_temp.runtime_recovery_control VALUES(true,'normal')",
		"SET search_path TO " + pgx.Identifier{fake}.Sanitize() + ",pg_temp,pg_catalog",
	} {
		if _, err := conn.Exec(ctx, statement); err != nil {
			t.Fatal("lookalike fixture setup", err)
		}
	}
	var fakeState string
	if err := conn.QueryRow(ctx, "SELECT state FROM runtime_recovery_control").Scan(&fakeState); err != nil || fakeState != "normal" {
		t.Fatal("lookalike fixture did not affect caller lookup", err)
	}
	for _, statement := range []string{
		"SELECT " + pgx.Identifier{s.runtimeSchema, "runtime_recovery_admit"}.Sanitize() + "(false)",
		"UPDATE " + pgx.Identifier{s.runtimeSchema, "organizations"}.Sanitize() + " SET name=name WHERE false",
		"DELETE FROM " + pgx.Identifier{s.runtimeSchema, "access_tokens"}.Sanitize() + " WHERE false",
	} {
		_, err := conn.Exec(ctx, statement)
		independentRecoverySQLState(t, err, "55000")
	}
	if _, err := conn.Exec(ctx, "RESET search_path"); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryGateIndependentOldRepeatableReadAfterActualWait(t *testing.T) {
	s, _ := independentRecoveryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	spec := independentRecoverySpec(t, s)
	old, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer old.Rollback(context.Background())
	var state string
	if err := old.QueryRow(ctx, "SELECT state FROM runtime_recovery_control").Scan(&state); err != nil || state != "normal" {
		t.Fatal("old snapshot setup", err)
	}
	admin, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Rollback(context.Background())
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_xact_lock(841932080,hashtext($1))", s.runtimeSchema); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := old.Exec(ctx, "UPDATE organizations SET name=name WHERE false")
		result <- err
	}()
	independentRecoveryWaitAdvisory(t, s, ctx, old.Conn().PgConn().PID())
	if err := independentRecoverySetHeld(ctx, admin, s.runtimeSchema, spec); err != nil {
		t.Fatal(err)
	}
	if err := admin.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		independentRecoverySQLState(t, err, "40001")
	case <-ctx.Done():
		t.Fatal("old writer did not finish after hold committed")
	}
	if err := old.Commit(ctx); err == nil {
		t.Fatal("obsolete writer committed")
	}
}

func TestRecoveryGateIndependentWriterPrecedesPreparation(t *testing.T) {
	s, _ := independentRecoveryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	spec := independentRecoverySpec(t, s)
	writer, err := s.beginRuntime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())
	if _, err := writer.Exec(ctx, "INSERT INTO organizations(id,name,budget_microusd) VALUES('independent-before','before',1)"); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := s.PrepareRecoveryTarget(ctx, spec); result <- err }()
	independentRecoveryWaitAdvisory(t, s, ctx, 0)
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("preparation ignored committed business data")
		}
	case <-ctx.Done():
		t.Fatal("preparation failed to finish after writer commit")
	}
	state, err := s.InspectRecoveryTarget(ctx)
	if err != nil || state.State != "normal" {
		t.Fatal("failed preparation modified normal target", err)
	}
	var count int
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM organizations WHERE id='independent-before'").Scan(&count); err != nil || count != 1 {
		t.Fatal("writer fact not preserved", err)
	}
}

func TestRecoveryGateIndependentPreparationPrecedesHostWriter(t *testing.T) {
	s, _ := independentRecoveryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	spec := independentRecoverySpec(t, s)
	admin, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Rollback(context.Background())
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_xact_lock(841932080,hashtext($1))", s.runtimeSchema); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := s.Bootstrap(ctx, "must not appear", 1); result <- err }()
	independentRecoveryWaitAdvisory(t, s, ctx, 0)
	if err := independentRecoverySetHeld(ctx, admin, s.runtimeSchema, spec); err != nil {
		t.Fatal(err)
	}
	if err := admin.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, RecoveryHeld) {
			t.Fatal("late host writer did not refuse held target", err)
		}
	case <-ctx.Done():
		t.Fatal("late host writer did not finish")
	}
	var count int
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM organizations").Scan(&count); err != nil || count != 0 {
		t.Fatal("late host writer left business rows", err)
	}
}

func TestRecoveryGateIndependentMissingBusinessGuardRefusesPreparation(t *testing.T) {
	s, _ := independentRecoveryStore(t)
	ctx := context.Background()
	spec := independentRecoverySpec(t, s)
	if _, err := s.Pool.Exec(ctx, "DROP TRIGGER recovery_runtime_write ON ledger_entries"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareRecoveryTarget(ctx, spec); !errors.Is(err, ErrRecoverySemanticCatalog) {
		t.Fatal("preparation did not reject incomplete trusted catalogue", err)
	}
	if _, err := s.InspectRecoveryTarget(ctx); !errors.Is(err, ErrRecoverySemanticCatalog) {
		t.Fatal("host inspection read data before rejecting unsupported catalogue", err)
	}
	// Explicit test-owner observation only: production Inspect must now reject
	// untrusted catalogue before reading even apparently harmless control data.
	var state string
	if err := s.Pool.QueryRow(ctx, "SELECT state FROM runtime_recovery_control WHERE singleton").Scan(&state); err != nil || state != "normal" {
		t.Fatal("incomplete guard refusal changed original control", err)
	}
}
