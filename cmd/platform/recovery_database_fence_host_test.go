// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/storage"
	"github.com/jackc/pgx/v5"
)

func TestRecoveryHostAuthorDatabaseFenceConsentRequired(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	file := recoveryHostPrivateJSON(t, filepath.Join(parent, "intent.json"), recoveryDatabaseFenceFile{AdminDatabaseURL: "unreachable-private-test-value", Intent: storage.RecoveryDatabaseFenceIntent{SourceDatabase: "synthetic"}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, command := range []string{"database-fence-apply", "database-fence-drain"} {
		for _, extra := range [][]string{nil, {"--expected-source-database", "synthetic"}, {"--confirm-disconnect-source", "--expected-source-database", "different"}} {
			args := append([]string{command, "--intent-file", file}, extra...)
			out, err := recoveryHostCommand(t, ctx, args...)
			if err == nil || len(out) != 0 {
				t.Fatal("missing consent or wrong source selection accepted")
			}
		}
	}
	bundle := filepath.Join(parent, "backup")
	if err := os.Mkdir(bundle, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(bundle, alias); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"--source-config", "--admin-config", "--target-config"} {
		for _, inside := range []string{bundle, alias} {
			args := []string{"database-fence-plan", "--source-config", file, "--admin-config", file, "--target-config", file, "--backup-dir", bundle, "--out", filepath.Join(parent, "output.json")}
			for i, arg := range args {
				if arg == input {
					args[i+1] = filepath.Join(inside, "config.json")
				}
			}
			var out bytes.Buffer
			if err := runRecoveryHost(ctx, args, &out); !errors.Is(err, recoveryarchive.ErrOptions) || out.Len() != 0 {
				t.Fatal("transported configuration not rejected before parsing/connection", err)
			}
		}
	}
	if recoveryarchive.RequireHostPathOutside(file, file) == nil {
		t.Fatal("regular file accepted as backup root")
	}
	linked := filepath.Join(parent, "linked.json")
	if err := os.Link(file, linked); err != nil {
		t.Fatal(err)
	}
	var value recoveryDatabaseFenceFile
	if readRecoveryFenceJSON(file, &value) == nil {
		t.Fatal("hardlinked privileged intent trusted")
	}

}

func TestRecoveryHostAuthorDatabaseFenceCommands(t *testing.T) {
	if os.Getenv("RUNVERGE_RECOVERY_OWNED_PG") != "1" || os.Getenv("RUNVERGE_RECOVERY_INDEPENDENT_OWNED_PG") != "1" {
		t.Skip("requires explicitly owned disposable whole databases")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("owned maintenance connection unavailable")
	}
	defer admin.Close(context.Background())
	create := func() (*storage.Store, string, string) {
		name := domain.NewID("fence_cli_test")
		if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'"); err != nil {
			t.Fatal(err)
		}
		u, e := url.Parse(os.Getenv("TEST_DATABASE_URL"))
		if e != nil {
			t.Fatal(e)
		}
		u.Path = "/" + name
		q := u.Query()
		q.Del("search_path")
		u.RawQuery = q.Encode()
		s, e := storage.Open(ctx, u.String())
		if e != nil {
			t.Fatal("new owned database unavailable")
		}
		t.Cleanup(func() {
			s.Close()
			cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
			defer stop()
			c, e := pgx.Connect(cleanup, os.Getenv("TEST_DATABASE_URL"))
			if e != nil {
				t.Error("owned cleanup connection unavailable")
				return
			}
			defer c.Close(cleanup)
			if _, e = c.Exec(cleanup, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); e != nil {
				t.Error("owned cleanup failed", e)
			}
		})
		if e = s.Migrate(ctx); e != nil {
			t.Fatal(e)
		}
		return s, u.String(), name
	}
	source, sourceDSN, sourceName := create()
	target, targetDSN, _ := create()
	ss, err := source.InspectRecoveryTarget(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := target.InspectRecoveryTarget(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := recoveryHostTarget{DatabaseURL: targetDSN, TargetInstallationID: ts.InstallationID, SourceID: ss.InstallationID, RestoreID: domain.NewID("restore"), BackupID: domain.NewID("bak"), CutUTC: time.Now().UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano), SchemaSHA256: storage.EmbeddedSchemaSHA256()}
	if _, err = target.PrepareRecoveryTarget(ctx, c.spec()); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	if err = os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(parent, "transport")
	if err = os.Mkdir(bundle, 0700); err != nil {
		t.Fatal(err)
	}
	sourceFile := recoveryHostPrivateJSON(t, filepath.Join(parent, "source.json"), recoveryDatabaseFile{sourceDSN})
	adminFile := recoveryHostPrivateJSON(t, filepath.Join(parent, "admin.json"), recoveryDatabaseFile{os.Getenv("TEST_DATABASE_URL")})
	targetFile := recoveryHostPrivateJSON(t, filepath.Join(parent, "target.json"), c)
	intentFile := filepath.Join(parent, "fence.json")
	planArgs := []string{"database-fence-plan", "--source-config", sourceFile, "--admin-config", adminFile, "--target-config", targetFile, "--backup-dir", bundle, "--out", intentFile}
	out, err := recoveryHostCommand(t, ctx, planArgs...)
	if err != nil || bytes.Contains(out, []byte(sourceDSN)) || bytes.Contains(out, []byte(os.Getenv("TEST_DATABASE_URL"))) {
		t.Fatal("private plan failed or leaked credentials", err)
	}
	var saved recoveryDatabaseFenceFile
	if readPrivateJSON(intentFile, &saved) != nil || saved.Intent.SourceDatabase != sourceName || saved.Intent.SourceOID == saved.Intent.TargetOID {
		t.Fatal("durable plan missing exact selection")
	}
	before, err := os.ReadFile(intentFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = recoveryHostCommand(t, ctx, planArgs...); err == nil {
		t.Fatal("existing fence intent overwritten")
	}
	after, err := os.ReadFile(intentFile)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("existing plan changed")
	}
	bad := append([]string(nil), planArgs...)
	bad[len(bad)-1] = filepath.Join(bundle, "fence.json")
	if _, err = recoveryHostCommand(t, ctx, bad...); err == nil {
		t.Fatal("maintenance credentials placed in transported directory")
	}
	if _, err = os.Lstat(bad[len(bad)-1]); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected private output created")
	}
	inspect := func(want string) {
		t.Helper()
		out, e := recoveryHostCommand(t, ctx, "database-fence-inspect", "--intent-file", intentFile)
		var result storage.RecoveryDatabaseFenceObservation
		if e != nil || json.Unmarshal(out, &result) != nil || result.State != want || result.ExternalExecutionFenced {
			t.Fatal("fence observation mismatch", want, e)
		}
	}
	inspect("connections_allowed")
	apply := []string{"database-fence-apply", "--intent-file", intentFile, "--expected-source-database", sourceName, "--confirm-disconnect-source"}
	// Real operation, failed result delivery, then observation by a new process.
	if err = runRecoveryHost(ctx, apply, recoveryHostFailedOutput{}); !errors.Is(err, errRecoveryHostOutput) {
		t.Fatal("post-fence output failure not reached", err)
	}
	inspect("database_admission_fenced")
	if _, err = recoveryHostCommand(t, ctx, apply...); err == nil {
		t.Fatal("uncertain operation blindly repeated")
	}
	if !errors.Is(target.CheckRuntimeAdmission(ctx), storage.RecoveryHeld) {
		t.Fatal("target execution enabled")
	}
}
