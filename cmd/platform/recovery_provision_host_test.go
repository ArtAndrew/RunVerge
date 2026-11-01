// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentplatform.local/platform/internal/storage"
	"github.com/jackc/pgx/v5"
)

func TestRecoveryHostAuthorProvisionedResultAcrossProcesses(t *testing.T) {
	recoveryHostArchiveRoundTrip(t, true, true)
}

// Extend the archive/read command test with a physically separate database.
// Inspection and subsequent commands run in separate test helper processes.
func recoveryHostProvisionedTarget(t *testing.T, ctx context.Context, parent string, selection []string) (*storage.Store, storage.RecoveryTargetState, string) {
	t.Helper()
	admin, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("owned maintenance unavailable")
	}
	path := filepath.Join(parent, "provision.json")
	adminFile := recoveryHostPrivateJSON(t, filepath.Join(parent, "admin.json"), recoveryDatabaseFile{os.Getenv("TEST_DATABASE_URL")})
	planArgs := append([]string{"provision-plan", "--admin-config", adminFile, "--out", path}, selection...)
	out, err := recoveryHostCommand(t, ctx, planArgs...)
	var plan storage.RecoveryProvisionIntent
	if err != nil || json.Unmarshal(out, &plan) != nil || plan.Database == "" {
		admin.Close(ctx)
		t.Fatal("target creation plan failed", err)
	}
	var target *storage.Store
	t.Cleanup(func() {
		if target != nil {
			target.Close()
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, e := admin.Exec(cleanup, "DROP DATABASE IF EXISTS "+pgx.Identifier{plan.Database}.Sanitize()+" WITH (FORCE)"); e != nil {
			t.Error("owned target cleanup failed")
		}
		if _, e := admin.Exec(cleanup, "DROP ROLE IF EXISTS "+pgx.Identifier{plan.Owner}.Sanitize()); e != nil {
			t.Error("owned owner cleanup failed")
		}
		admin.Close(cleanup)
	})
	args := []string{"provision-apply", "--intent-file", path, "--expected-database", plan.Database, "--confirm-create-target"}
	bad := append([]string(nil), args...)
	bad[4] = "wrong-target"
	if _, err = recoveryHostCommand(t, ctx, bad...); err == nil {
		t.Fatal("wrong target confirmation accepted")
	}
	if _, err = os.Stat(path + ".apply"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("denial consumed attempt")
	}
	if err = runRecoveryHost(ctx, args, recoveryHostFailedOutput{}); !errors.Is(err, errRecoveryHostOutput) {
		t.Fatal("creation output loss not observed", err)
	}
	out, err = recoveryHostCommand(t, ctx, "provision-inspect", "--intent-file", path)
	var observed storage.RecoveryProvisionObservation
	if err != nil || json.Unmarshal(out, &observed) != nil || observed.State != "database_restricted" || !observed.OrdinaryConnectionsDenied || observed.DatabaseOID == 0 {
		t.Fatal("new process did not inspect isolated target", err)
	}
	if _, err = recoveryHostCommand(t, ctx, args...); err == nil {
		t.Fatal("one-use creation repeated")
	}
	configFile := path + ".target.json"
	var config recoveryHostTarget
	if readRecoveryFenceJSON(configFile, &config) != nil {
		t.Fatal("durable target selector missing")
	}
	target, err = storage.Open(ctx, config.DatabaseURL)
	if err != nil {
		t.Fatal("owned target inspection unavailable")
	}
	state, err := target.InspectRecoveryTarget(ctx)
	if err != nil || state.State != "held" || state.RestoreID != plan.OperationID {
		t.Fatal("new target not held", err)
	}
	if !errors.Is(target.CheckRuntimeAdmission(ctx), storage.RecoveryHeld) {
		t.Fatal("ordinary runtime accepted during provisioning")
	}
	return target, state, configFile
}
