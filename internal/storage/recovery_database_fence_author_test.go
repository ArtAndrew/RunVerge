// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"net/url"
	"os"
	"reflect"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type recoveryFenceAuthorFixture struct {
	admin          *pgx.Conn
	source, target *Store
	sourceDSN      string
	spec           RecoveryTargetSpec
	intent         RecoveryDatabaseFenceIntent
	names          []string
}

func recoveryFenceAuthorSetup(t *testing.T) *recoveryFenceAuthorFixture {
	t.Helper()
	recoveryInvariantOwned(t)
	if os.Getenv("RUNVERGE_RECOVERY_INDEPENDENT_OWNED_PG") != "1" {
		t.Skip("requires explicit disposable cluster for whole database tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("owned maintenance connection unavailable")
	}
	f := &recoveryFenceAuthorFixture{admin: admin}
	t.Cleanup(func() {
		if f.source != nil {
			f.source.Close()
		}
		if f.target != nil {
			f.target.Close()
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, name := range f.names {
			if _, e := admin.Exec(cleanup, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); e != nil {
				t.Error("owned database cleanup failed", e)
			}
		}
		_ = admin.Close(cleanup)
	})
	create := func() (*Store, string) {
		name := domain.NewID("fence_test")
		if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
			t.Fatal("fresh database creation failed", err)
		}
		f.names = append(f.names, name)
		u, e := url.Parse(os.Getenv("TEST_DATABASE_URL"))
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + name
		q := u.Query()
		q.Del("search_path")
		u.RawQuery = q.Encode()
		s, e := Open(ctx, u.String())
		if e != nil {
			t.Fatal("fresh database unavailable")
		}
		if e = s.Migrate(ctx); e != nil {
			s.Close()
			t.Fatal(e)
		}
		return s, u.String()
	}
	f.source, f.sourceDSN = create()
	f.target, _ = create()
	sourceState, err := f.source.InspectRecoveryTarget(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.spec = recoveryTargetAuthorSpec(t, f.target)
	f.spec.SourceID = sourceState.InstallationID
	if _, err = f.target.PrepareRecoveryTarget(ctx, f.spec); err != nil {
		t.Fatal(err)
	}
	f.intent, err = CaptureRecoveryDatabaseFence(ctx, admin, f.source, f.target, f.spec)
	if err != nil {
		t.Fatal("capture separate database identities", err)
	}
	return f
}

func TestRecoveryDatabaseFenceAuthorStopsRawClients(t *testing.T) {
	f := recoveryFenceAuthorSetup(t)
	ctx := context.Background()
	old, err := pgx.Connect(ctx, f.sourceDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close(ctx)
	bad := f.intent
	bad.SourceOID = f.intent.TargetOID
	if _, err = ApplyRecoveryDatabaseFence(ctx, f.admin, bad); err == nil {
		t.Fatal("target/source equality accepted")
	}
	bad = f.intent
	bad.ServerStartedAt = bad.ServerStartedAt.Add(-time.Second)
	if _, err = ApplyRecoveryDatabaseFence(ctx, f.admin, bad); err == nil {
		t.Fatal("stale primary identity accepted")
	}
	badSpec := f.spec
	badSpec.SourceID = domain.NewID("inst")
	if _, err = CaptureRecoveryDatabaseFence(ctx, f.admin, f.source, f.target, badSpec); err == nil {
		t.Fatal("wrong source installation accepted")
	}
	if _, err = CaptureRecoveryDatabaseFence(ctx, old, f.source, f.target, f.spec); err == nil {
		t.Fatal("source used as maintenance database")
	}
	ob, err := ApplyRecoveryDatabaseFence(ctx, f.admin, f.intent)
	if err != nil || ob.State != "database_admission_fenced" || ob.ExternalExecutionFenced || ob.Sessions != 0 || ob.ConnectionsAllowed || ob.ObservedAt.IsZero() {
		t.Fatal("database admission fence failed", err)
	}
	if _, err = old.Exec(ctx, "SELECT 1"); err == nil {
		t.Fatal("raw old client retained access without application guards")
	}
	fresh, err := pgx.Connect(ctx, f.sourceDSN)
	if err == nil {
		fresh.Close(ctx)
		t.Fatal("new raw connection admitted")
	}
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "55000" {
		t.Fatal("new connection did not fail at database admission")
	}
	if _, err = ApplyRecoveryDatabaseFence(ctx, f.admin, f.intent); err == nil {
		t.Fatal("repeat apply silently adopted disabled DB")
	}
	if _, err = f.target.Pool.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal("target disconnected", err)
	}
	if !errors.Is(f.target.CheckRuntimeAdmission(ctx), RecoveryHeld) {
		t.Fatal("target hold changed")
	}
	if _, err = InspectRecoveryDatabaseFence(ctx, f.admin, f.intent); err != nil {
		t.Fatal("fresh inspection unavailable", err)
	}
}

func TestRecoveryDatabaseFenceAuthorPreparedAndExplicitDrain(t *testing.T) {
	f := recoveryFenceAuthorSetup(t)
	ctx := context.Background()
	old, err := pgx.Connect(ctx, f.sourceDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close(ctx)
	const gid = "runverge_synthetic_fence_prepared"
	if _, err = old.Exec(ctx, "BEGIN; SELECT 1; PREPARE TRANSACTION '"+gid+"'"); err != nil {
		t.Fatal("owned CI requires prepared transaction support", err)
	}
	if _, err = ApplyRecoveryDatabaseFence(ctx, f.admin, f.intent); err == nil {
		t.Fatal("prepared transaction ignored before mutation")
	}
	var allowed bool
	if err = f.admin.QueryRow(ctx, "SELECT datallowconn FROM pg_catalog.pg_database WHERE oid=$1", f.intent.SourceOID).Scan(&allowed); err != nil || !allowed {
		t.Fatal("rejected prepared history disabled source", err)
	}
	// Simulate a previously committed disable with its acknowledgment lost.
	// This fixture alone applies the DDL; Drain must not settle its prepared work.
	if _, err = f.admin.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{f.intent.SourceDatabase}.Sanitize()+" ALLOW_CONNECTIONS false"); err != nil {
		t.Fatal(err)
	}
	if _, err = DrainRecoveryDatabaseFence(ctx, f.admin, f.intent); err == nil {
		t.Fatal("prepared transaction certified as drained")
	}
	ob, err := InspectRecoveryDatabaseFence(ctx, f.admin, f.intent)
	if err != nil || ob.PreparedTransactions != 1 || ob.State != "connections_disabled_unresolved" || ob.ExternalExecutionFenced {
		t.Fatal("prepared uncertainty lost", err)
	}
	// Disposable fixture cleanup only: reopen this newly created source to roll
	// back the synthetic prepared transaction. Production exposes no reopen API.
	if _, err = f.admin.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{f.intent.SourceDatabase}.Sanitize()+" ALLOW_CONNECTIONS true"); err != nil {
		t.Fatal(err)
	}
	cleanup, err := pgx.Connect(ctx, f.sourceDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cleanup.Exec(ctx, "ROLLBACK PREPARED '"+gid+"'"); err != nil {
		t.Fatal(err)
	}
	cleanup.Close(ctx)
	if _, err = f.admin.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{f.intent.SourceDatabase}.Sanitize()+" ALLOW_CONNECTIONS false"); err != nil {
		t.Fatal(err)
	}
	if ob, err = DrainRecoveryDatabaseFence(ctx, f.admin, f.intent); err != nil || ob.State != "database_admission_fenced" {
		t.Fatal("explicit drain after exact inspection failed", err)
	}
}

func TestRecoveryDatabaseFenceAuthorRealCommitLoss(t *testing.T) {
	for _, mode := range []string{"before_commit", "after_commit"} {
		t.Run(mode, func(t *testing.T) {
			f := recoveryFenceAuthorSetup(t)
			ctx := context.Background()
			old, err := pgx.Connect(ctx, f.sourceDSN)
			if err != nil {
				t.Fatal(err)
			}
			defer old.Close(ctx)
			_, maintenanceDSN := independentRecoveryStore(t)
			victim, proxy := independentImportCommitProxy(t, maintenanceDSN)
			c, err := victim.Pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Release()
			cut := proxy.arm(t, "alter database "+pgx.Identifier{f.intent.SourceDatabase}.Sanitize(), mode)
			bounded, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			ob, err := ApplyRecoveryDatabaseFence(bounded, c.Conn(), f.intent)
			if !errors.Is(err, ErrRecoveryDatabaseFenceUnknown) || !reflect.DeepEqual(ob, RecoveryDatabaseFenceObservation{}) {
				t.Fatal("lost COMMIT not classified", err)
			}
			proxy.wait(t, cut)
			if _, err = old.Exec(ctx, "SELECT 1"); err != nil {
				t.Fatal("sessions terminated on unknown commit", err)
			}
			ob, err = InspectRecoveryDatabaseFence(ctx, f.admin, f.intent)
			if err != nil || ob.ConnectionsAllowed != (mode == "before_commit") || ob.Sessions == 0 || ob.ExternalExecutionFenced {
				t.Fatal("exact fresh inspection disagrees with commit cut", err)
			}
			if mode == "after_commit" {
				if _, err = DrainRecoveryDatabaseFence(ctx, f.admin, f.intent); err != nil {
					t.Fatal("explicit post-inspection drain", err)
				}
			}
		})
	}
}

func TestRecoveryDatabaseFenceAuthorSameDatabaseRefused(t *testing.T) {
	recoveryInvariantOwned(t)
	source, _ := independentRecoveryStore(t)
	target, _ := independentRecoveryStore(t)
	ctx := context.Background()
	state, err := source.InspectRecoveryTarget(ctx)
	if err != nil {
		t.Fatal(err)
	}
	spec := recoveryTargetAuthorSpec(t, target)
	spec.SourceID = state.InstallationID
	if _, err = target.PrepareRecoveryTarget(ctx, spec); err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err = CaptureRecoveryDatabaseFence(ctx, admin, source, target, spec); err == nil {
		t.Fatal("different schemas accepted as isolated databases")
	}
	if _, err = source.Pool.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal("source mutated by refused capture", err)
	}
}

func TestRecoveryDatabaseFenceAuthorRenameDuringApply(t *testing.T) {
	f := recoveryFenceAuthorSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	f.source.Close()
	replacement, retired := domain.NewID("fence_test"), domain.NewID("fence_test")
	if _, err := f.admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{replacement}.Sanitize()+" TEMPLATE template0"); err != nil {
		t.Fatal(err)
	}
	f.names = append(f.names, replacement, retired)
	locker, err := f.admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(ctx)
	if _, err = locker.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{f.intent.SourceDatabase}.Sanitize()+" CONNECTION LIMIT 9"); err != nil {
		t.Fatal(err)
	}
	applying, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer applying.Close(ctx)
	done := make(chan error, 1)
	go func() { _, e := ApplyRecoveryDatabaseFence(ctx, applying, f.intent); done <- e }()
	for {
		var waiting bool
		err = locker.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_locks WHERE pid=$1 AND NOT granted)", applying.PgConn().PID()).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case e := <-done:
			t.Fatal("apply did not wait for database object lock", e)
		case <-ctx.Done():
			t.Fatal("lock wait not observed")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err = locker.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{f.intent.SourceDatabase}.Sanitize()+" RENAME TO "+pgx.Identifier{retired}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err = locker.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{replacement}.Sanitize()+" RENAME TO "+pgx.Identifier{f.intent.SourceDatabase}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if err = locker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil {
		t.Fatal("renamed/replaced DB accepted")
	}
	var enabled int
	if err = f.admin.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_database WHERE datname IN($1,$2) AND datallowconn", f.intent.SourceDatabase, retired).Scan(&enabled); err != nil || enabled != 2 {
		t.Fatal("wrong database durably disabled", err)
	}
}
