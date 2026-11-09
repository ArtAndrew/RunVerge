// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/storage"
	"github.com/jackc/pgx/v5"
)

type recoveryDatabaseFenceFile struct {
	AdminDatabaseURL string                              `json:"admin_database_url"`
	Intent           storage.RecoveryDatabaseFenceIntent `json:"intent"`
}

func readRecoveryFenceJSON(path string, out any) error {
	raw, err := recoveryarchive.ReadHostPrivateFile(path)
	if err != nil {
		return err
	}
	defer clear(raw)
	return decodePrivateJSON(raw, out)
}

func runRecoveryDatabaseFenceHost(ctx context.Context, args []string, out io.Writer) error {
	usage := errors.New("usage: platform recovery database-fence-plan|database-fence-inspect|database-fence-apply|database-fence-drain [explicit private file options]")
	if len(args) == 0 {
		return usage
	}
	f := flag.NewFlagSet("database-fence", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var sourceFile, targetFile, adminFile, bundle, output, intentFile, expectedName string
	var confirmed bool
	switch args[0] {
	case "database-fence-plan":
		f.StringVar(&sourceFile, "source-config", "", "private explicit source database JSON")
		f.StringVar(&targetFile, "target-config", "", "private held target intent")
		f.StringVar(&adminFile, "admin-config", "", "private explicit maintenance database JSON")
		f.StringVar(&bundle, "backup-dir", "", "transported backup directory, excluded from intent output")
		f.StringVar(&output, "out", "", "new private fence intent outside the backup")
	case "database-fence-inspect", "database-fence-apply", "database-fence-drain":
		f.StringVar(&intentFile, "intent-file", "", "private saved database fence intent")
		if args[0] != "database-fence-inspect" {
			f.StringVar(&expectedName, "expected-source-database", "", "exact reviewed source database name")
			f.BoolVar(&confirmed, "confirm-disconnect-source", false, "authorize disabling source DB connections and terminating every session in that database")
		}
	default:
		return usage
	}
	if f.Parse(args[1:]) != nil || f.NArg() != 0 {
		return usage
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	closeAdmin := func(c *pgx.Conn) {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = c.Close(cleanup)
	}
	emit := func(v any) error { return json.NewEncoder(out).Encode(v) }
	if args[0] == "database-fence-plan" {
		if sourceFile == "" || targetFile == "" || adminFile == "" || bundle == "" || output == "" {
			return usage
		}
		var sourceConfig, adminConfig recoveryDatabaseFile
		var targetConfig recoveryHostTarget
		for _, path := range []string{sourceFile, adminFile, targetFile} {
			if err := recoveryarchive.RequireHostPathOutside(bundle, path); err != nil {
				return err
			}
		}
		if readRecoveryFenceJSON(sourceFile, &sourceConfig) != nil || readRecoveryFenceJSON(adminFile, &adminConfig) != nil || readRecoveryFenceJSON(targetFile, &targetConfig) != nil {
			return errors.New("invalid private database fence configuration")
		}
		sourceDSN, err := explicitRecoveryDSN(sourceConfig.DatabaseURL)
		if err != nil {
			return err
		}
		targetDSN, err := explicitRecoveryDSN(targetConfig.DatabaseURL)
		if err != nil {
			return err
		}
		adminDSN, err := explicitRecoveryDSN(adminConfig.DatabaseURL)
		if err != nil {
			return err
		}
		admin, err := pgx.Connect(ctx, adminDSN)
		if err != nil {
			return errors.New("cannot open explicit maintenance connection")
		}
		defer closeAdmin(admin)
		source, err := storage.Open(ctx, sourceDSN)
		if err != nil {
			return errors.New("cannot open explicit source")
		}
		defer source.Close()
		target, err := storage.Open(ctx, targetDSN)
		if err != nil {
			return errors.New("cannot open explicit held target")
		}
		defer target.Close()
		intent, err := storage.CaptureRecoveryDatabaseFence(ctx, admin, source, target, targetConfig.spec())
		if err != nil {
			return err
		}
		// No database admission mutation has occurred. Persist before a later
		// separately confirmed apply; never put the maintenance password in stdout.
		if err = recoveryarchive.WriteHostJSONOutside(bundle, output, recoveryDatabaseFenceFile{AdminDatabaseURL: adminConfig.DatabaseURL, Intent: intent}); err != nil {
			return err
		}
		return emit(struct {
			Scope  string                              `json:"scope"`
			Intent storage.RecoveryDatabaseFenceIntent `json:"intent"`
		}{"database_admission_only", intent})
	}
	if intentFile == "" {
		return usage
	}
	var saved recoveryDatabaseFenceFile
	if readRecoveryFenceJSON(intentFile, &saved) != nil {
		return errors.New("invalid private database fence intent")
	}
	if args[0] != "database-fence-inspect" && (!confirmed || expectedName == "" || expectedName != saved.Intent.SourceDatabase) {
		return usage
	}
	dsn, err := explicitRecoveryDSN(saved.AdminDatabaseURL)
	if err != nil {
		return err
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return errors.New("cannot open explicit maintenance connection")
	}
	defer closeAdmin(admin)
	var observation storage.RecoveryDatabaseFenceObservation
	switch args[0] {
	case "database-fence-inspect":
		observation, err = storage.InspectRecoveryDatabaseFence(ctx, admin, saved.Intent)
	case "database-fence-apply":
		observation, err = storage.ApplyRecoveryDatabaseFence(ctx, admin, saved.Intent)
	case "database-fence-drain":
		observation, err = storage.DrainRecoveryDatabaseFence(ctx, admin, saved.Intent)
	}
	if err != nil {
		return err
	}
	return emit(observation)
}
