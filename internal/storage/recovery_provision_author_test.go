// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

func TestRecoveryProvisionAuthorIsolationAndInspection(t *testing.T) {
	recoveryInvariantOwned(t)
	if os.Getenv("RUNVERGE_RECOVERY_INDEPENDENT_OWNED_PG") != "1" {
		t.Skip("owned whole-cluster fixture required")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("owned maintenance unavailable")
	}
	p, err := PlanRecoveryProvision(ctx, admin, domain.NewID("bak"), domain.NewID("inst"), strings.Repeat("a", 64), EmbeddedSchemaSHA256())
	if err != nil {
		t.Fatal(err)
	}
	oldRole := domain.NewID("old_app")
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, e := admin.Exec(cleanup, "DROP DATABASE IF EXISTS "+pgx.Identifier{p.Database}.Sanitize()+" WITH (FORCE)"); e != nil {
			t.Error("target cleanup failed")
		}
		for _, role := range []string{p.Owner, oldRole} {
			if _, e := admin.Exec(cleanup, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize()); e != nil {
				t.Error("role cleanup failed")
			}
		}
		admin.Close(cleanup)
	}()
	ob, err := CreateRecoveryProvision(ctx, admin, p)
	if err != nil || ob.State != "database_restricted" {
		t.Fatal("create restricted target", err)
	}
	if _, err = CreateRecoveryProvision(ctx, admin, p); err == nil {
		t.Fatal("created target adopted")
	}
	var existing bool
	if admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_database WHERE oid=$1 AND datname=current_database())", p.MaintenanceOID).Scan(&existing) != nil || !existing {
		t.Fatal("maintenance database changed")
	}
	// A raw client with an old application's credentials never reaches the new
	// database, independent of whether its binary implements recovery guards.
	if _, err = admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{oldRole}.Sanitize()+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD 'synthetic-provision-only'"); err != nil {
		t.Fatal("old application fixture", err)
	}
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + p.Database
	u.RawPath = ""
	u.User = url.UserPassword(oldRole, "synthetic-provision-only")
	if c, e := pgx.Connect(ctx, u.String()); e == nil {
		c.Close(ctx)
		t.Fatal("old app connected to target")
	}
	u, err = url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(oldRole, "synthetic-provision-only")
	if c, e := pgx.Connect(ctx, u.String()); e != nil {
		t.Fatal("old credential control failed")
	} else {
		c.Close(ctx)
	}
	u.User = url.UserPassword(admin.Config().User, admin.Config().Password)
	if wrong, e := OpenRecoveryProvisionTarget(ctx, admin, u.String(), p, ob.DatabaseOID); e == nil {
		wrong.Close()
		t.Fatal("maintenance backend adopted as target")
	}
	u.Path = "/" + p.Database
	u.RawPath = ""
	target, err := OpenRecoveryProvisionTarget(ctx, admin, u.String(), p, ob.DatabaseOID)
	if err != nil {
		t.Fatal("witnessed target refused", err)
	}
	defer target.Close()
	if err = target.Migrate(ctx); err != nil {
		t.Fatal("trusted target migration failed", err)
	}
	if _, err = admin.Exec(ctx, "GRANT CONNECT ON DATABASE "+pgx.Identifier{p.Database}.Sanitize()+" TO "+pgx.Identifier{oldRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err = InspectRecoveryProvision(ctx, admin, p); err == nil {
		t.Fatal("unexpected database grant accepted")
	}
	// A reconnect must repeat the witness and isolation checks.
	target.Pool.Reset()
	if target.Pool.Ping(ctx) == nil {
		t.Fatal("reconnected without rechecking target isolation")
	}
	if _, err = admin.Exec(ctx, "REVOKE ALL ON DATABASE "+pgx.Identifier{p.Database}.Sanitize()+" FROM "+pgx.Identifier{oldRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if target.Pool.Ping(ctx) != nil {
		t.Fatal("restored isolation did not pass fresh witness")
	}
	spec := recoveryTargetAuthorSpec(t, target)
	if _, err = target.PrepareRecoveryTarget(ctx, spec); err != nil {
		t.Fatal("fresh target hold failed", err)
	}
	if target.CheckRuntimeAdmission(ctx) != RecoveryHeld {
		t.Fatal("held target admitted work")
	}

}

func TestRecoveryProvisionAuthorLostCreateAcknowledgment(t *testing.T) {
	for _, mode := range []string{"before_statement", "after_statement"} {
		t.Run(mode, func(t *testing.T) {
			recoveryInvariantOwned(t)
			direct, dsn := independentRecoveryStore(t)
			victim, proxy := independentImportCommitProxy(t, dsn)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c, err := victim.Pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Release()
			admin, err := direct.Pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Release()
			p, err := PlanRecoveryProvision(ctx, c.Conn(), domain.NewID("bak"), domain.NewID("inst"), strings.Repeat("b", 64), EmbeddedSchemaSHA256())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
				defer stop()
				if _, e := admin.Exec(cleanup, "DROP DATABASE IF EXISTS "+pgx.Identifier{p.Database}.Sanitize()+" WITH (FORCE)"); e != nil {
					t.Error("owned database cleanup failed")
				}
				if _, e := admin.Exec(cleanup, "DROP ROLE IF EXISTS "+pgx.Identifier{p.Owner}.Sanitize()); e != nil {
					t.Error("owned owner cleanup failed")
				}
			}()
			cut := proxy.arm(t, "create database "+pgx.Identifier{p.Database}.Sanitize(), mode)
			if _, err = CreateRecoveryProvision(ctx, c.Conn(), p); err == nil {
				t.Fatal("lost create acknowledged")
			}
			proxy.wait(t, cut)
			observed, err := InspectRecoveryProvision(ctx, admin.Conn(), p)
			want := "owner_created"
			if mode == "after_statement" {
				want = "database_closed"
			}
			if err != nil || observed.State != want {
				t.Fatal("fresh inspection disagrees with actual CREATE boundary", err, observed.State)
			}
			if _, err = CreateRecoveryProvision(ctx, admin.Conn(), p); err == nil {
				t.Fatal("partial target automatically adopted or recreated")
			}
		})
	}
}
