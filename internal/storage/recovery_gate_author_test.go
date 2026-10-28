// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5/pgconn"
)

func recoveryTargetAuthorSpec(t *testing.T, s *Store) RecoveryTargetSpec {
	t.Helper()
	state, err := s.InspectRecoveryTarget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return RecoveryTargetSpec{TargetInstallationID: state.InstallationID, RestoreID: domain.NewID("restore"), BackupID: domain.NewID("bak"), SourceID: domain.NewID("inst"), CutUTC: time.Now().UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano), SchemaSHA256: EmbeddedSchemaSHA256()}
}

func TestRecoveryGateAuthorFreshTarget(t *testing.T) {
	if os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" {
		t.Skip("requires newly owned isolated PostgreSQL cluster")
	}
	s, dsn := testStore(t)
	ctx := context.Background()
	if err := s.CheckRuntimeAdmission(ctx); err != nil {
		t.Fatal("fresh normal admission", err)
	}
	spec := recoveryTargetAuthorSpec(t, s)
	held, err := s.PrepareRecoveryTarget(ctx, spec)
	if err != nil {
		t.Fatal("prepare fresh held target", err)
	}
	if held.State != "held" || held.InstallationID != spec.TargetInstallationID || held.RestoreID != spec.RestoreID || held.BackupID != spec.BackupID || held.CutUTC != spec.CutUTC || held.HeldAt.IsZero() {
		t.Fatal("exact target receipt mismatch")
	}
	if err = s.CheckRuntimeAdmission(ctx); !errors.Is(err, RecoveryHeld) {
		t.Fatal("held runtime admitted", err)
	}
	if _, err = s.Bootstrap(ctx, "must not create", 1000); !errors.Is(err, RecoveryHeld) {
		t.Fatal("held bootstrap admitted", err)
	}
	if _, err = s.Authenticate(ctx, "historical-token", domain.NewID("org")); !errors.Is(err, RecoveryHeld) {
		t.Fatal("held auth admitted", err)
	}
	if _, err = s.ClaimFixture(ctx, "author", time.Minute); !errors.Is(err, RecoveryHeld) {
		t.Fatal("held fixture admitted", err)
	}
	if _, _, err = s.ClaimModel(ctx, "author", time.Minute); !errors.Is(err, RecoveryHeld) {
		t.Fatal("held model admitted", err)
	}
	if _, _, err = s.ClaimToolExecution(ctx, "author", time.Minute); !errors.Is(err, RecoveryHeld) {
		t.Fatal("held tool admitted", err)
	}
	for name, reconcile := range map[string]func(context.Context) (bool, error){"model": s.ReconcileModelUnsent, "tool": s.ReconcileToolUnsent, "team": s.ReconcileTeam, "automation": s.ReconcileTaskAutomation, "webhook": s.ReconcileWebhookSent} {
		t.Run(name, func(t *testing.T) {
			if changed, e := reconcile(ctx); changed || !errors.Is(e, RecoveryHeld) {
				t.Fatal("held cleanup admitted", e)
			}
		})
	}
	for _, statement := range []string{
		"INSERT INTO organizations(id,name,budget_microusd) VALUES('author-denied','denied',1)",
		"UPDATE organizations SET name=name WHERE false",
		"DELETE FROM access_tokens WHERE false",
		"TRUNCATE access_tokens",
		"UPDATE runtime_recovery_control SET state='normal',restore_id=NULL,backup_id=NULL,source_id=NULL,cut_at=NULL,schema_sha256=NULL,held_at=NULL",
		"DELETE FROM runtime_recovery_control",
		"TRUNCATE runtime_recovery_control",
	} {
		_, e := s.Pool.Exec(ctx, statement)
		var pgerr *pgconn.PgError
		if !errors.As(e, &pgerr) || pgerr.Code != "55000" {
			t.Fatalf("held SQL did not fail closed: %s: %v", statement, e)
		}
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("migration replay", err)
	}
	if _, err = s.PrepareRecoveryTarget(ctx, spec); err == nil {
		t.Fatal("held target was silently reused")
	}
	reopened, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	observed, err := reopened.InspectRecoveryTarget(ctx)
	if err != nil || observed != held {
		t.Fatal("hold changed on reopen/migration replay", err)
	}
	if err = reopened.CheckRuntimeAdmission(ctx); !errors.Is(err, RecoveryHeld) {
		t.Fatal("reopened runtime admitted", err)
	}
}

func TestRecoveryGateAuthorRejectsPopulatedOrWrongTarget(t *testing.T) {
	if os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" {
		t.Skip("requires newly owned isolated PostgreSQL cluster")
	}
	s, _ := testStore(t)
	ctx := context.Background()
	spec := recoveryTargetAuthorSpec(t, s)
	wrong := spec
	wrong.TargetInstallationID = domain.NewID("inst")
	if _, err := s.PrepareRecoveryTarget(ctx, wrong); err == nil {
		t.Fatal("wrong target accepted")
	}
	alias := spec
	alias.SourceID = spec.TargetInstallationID
	if _, err := s.PrepareRecoveryTarget(ctx, alias); err == nil {
		t.Fatal("source alias accepted")
	}
	bad := spec
	bad.SchemaSHA256 = Hash([]byte("wrong schema"))
	if _, err := s.PrepareRecoveryTarget(ctx, bad); err == nil {
		t.Fatal("wrong schema accepted")
	}
	p, b := identity(t, s, 1000)
	if _, err := s.PrepareRecoveryTarget(ctx, spec); err == nil {
		t.Fatal("populated target accepted")
	}
	state, err := s.InspectRecoveryTarget(ctx)
	if err != nil || state.State != "normal" {
		t.Fatal("failed preparation changed control", err)
	}
	actor, err := s.Authenticate(ctx, b.Token, b.OrganizationID)
	if err != nil || actor.ID != p.ID {
		t.Fatal("failed preparation changed authority", err)
	}
}
