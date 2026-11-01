// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/url"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/storage"
	"github.com/jackc/pgx/v5"
)

type recoveryProvisionFile struct {
	AdminDatabaseURL string                          `json:"admin_database_url"`
	BackupDirectory  string                          `json:"backup_directory"`
	ReceiptSHA256    string                          `json:"receipt_sha256"`
	CutUTC           string                          `json:"cut_utc"`
	Intent           storage.RecoveryProvisionIntent `json:"intent"`
}

func runRecoveryProvisionHost(ctx context.Context, args []string, out io.Writer) error {
	usage := errors.New("usage: platform recovery provision-plan|provision-apply|provision-inspect [explicit private file options]")
	if len(args) == 0 {
		return usage
	}
	f := flag.NewFlagSet("recovery-provision", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var adminFile, bundle, keys, stage, receipt, path, expected string
	var confirm bool
	switch args[0] {
	case "provision-plan":
		f.StringVar(&adminFile, "admin-config", "", "private operator-only maintenance connection")
		f.StringVar(&bundle, "backup-dir", "", "selected backup directory")
		f.StringVar(&keys, "keys-file", "", "independently trusted archive keys")
		f.StringVar(&stage, "staging-parent", "", "private temporary staging directory")
		f.StringVar(&receipt, "expected-receipt-sha256", "", "independently recorded receipt digest")
		f.StringVar(&path, "out", "", "new private provisioning plan outside backup")
	case "provision-apply", "provision-inspect":
		f.StringVar(&path, "intent-file", "", "saved private provisioning plan")
		if args[0] == "provision-apply" {
			f.StringVar(&expected, "expected-database", "", "exact database name from reviewed plan")
			f.BoolVar(&confirm, "confirm-create-target", false, "create a new restricted database; never replace the source")
		}
	default:
		return usage
	}
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || path == "" {
		return usage
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var saved recoveryProvisionFile
	if args[0] == "provision-plan" {
		if adminFile == "" || bundle == "" || keys == "" || stage == "" || receipt == "" {
			return usage
		}
		if recoveryarchive.RequireHostPathOutside(bundle, adminFile) != nil || recoveryarchive.RequireHostPathOutside(bundle, path) != nil {
			return errors.New("provisioning authority must be outside backup")
		}
		var config recoveryDatabaseFile
		if readRecoveryFenceJSON(adminFile, &config) != nil {
			return errors.New("private operator database configuration required")
		}
		saved.AdminDatabaseURL = config.DatabaseURL
		archive, err := recoveryarchive.VerifyHostBackup(ctx, bundle, keys, receipt, stage)
		if err != nil {
			return err
		}
		defer archive.Close()
		report, err := storage.ValidateRecoverySnapshot(ctx, archive, storage.DefaultRecoverySnapshotLimits())
		if err != nil {
			return errors.New("selected snapshot framing invalid")
		}
		m := report.Metadata
		manifest := archive.Manifest()
		if manifest.Metadata != (recoveryarchive.Metadata{BackupID: m.BackupID, SourceID: m.SourceID, CutUTC: m.CutUTC, SchemaSHA256: m.SchemaSHA256}) || manifest.PlaintextSHA256 != report.SHA256 || manifest.PlaintextBytes != report.WireBytes || m.SchemaSHA256 != storage.EmbeddedSchemaSHA256() {
			return errors.New("provisioning requires a matching authenticated snapshot")
		}
		if err = archive.Close(); err != nil {
			return err
		}
		saved.BackupDirectory = bundle
		saved.ReceiptSHA256 = receipt
		saved.CutUTC = m.CutUTC
		saved.Intent.BackupID = m.BackupID
		saved.Intent.SourceID = m.SourceID
		saved.Intent.SnapshotSHA256 = report.SHA256
		saved.Intent.SchemaSHA256 = m.SchemaSHA256
	} else {
		if readRecoveryFenceJSON(path, &saved) != nil {
			return errors.New("invalid private provisioning plan")
		}
		if args[0] == "provision-apply" && (!confirm || expected == "" || expected != saved.Intent.Database) {
			return usage
		}
	}
	dsn, err := explicitRecoveryDSN(saved.AdminDatabaseURL)
	if err != nil {
		return err
	}
	rawURL, err := url.Parse(saved.AdminDatabaseURL)
	if err != nil || rawURL.Query().Get("search_path") != "" {
		return errors.New("provisioning requires a maintenance connection without search_path")
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return errors.New("cannot connect to explicit maintenance database")
	}
	defer func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = admin.Close(closeCtx)
	}()
	emit := func(v any) error { return json.NewEncoder(out).Encode(v) }
	if args[0] == "provision-plan" {
		saved.Intent, err = storage.PlanRecoveryProvision(ctx, admin, saved.Intent.BackupID, saved.Intent.SourceID, saved.Intent.SnapshotSHA256, saved.Intent.SchemaSHA256)
		if err != nil {
			return err
		}
		if err = recoveryarchive.WriteHostJSONOutside(bundle, path, saved); err != nil {
			return err
		}
		return emit(saved.Intent)
	}
	ob, err := storage.InspectRecoveryProvision(ctx, admin, saved.Intent)
	if err != nil {
		return err
	}
	if args[0] == "provision-inspect" {
		return emit(ob)
	}
	if ob.State != "not_created" {
		return errors.New("target creation already started; inspect saved outputs instead of repeating")
	}
	if err = recoveryarchive.WriteHostJSONOutside(saved.BackupDirectory, path+".apply", struct {
		OperationID string    `json:"operation_id"`
		StartedAt   time.Time `json:"started_at"`
	}{saved.Intent.OperationID, time.Now().UTC()}); err != nil {
		return errors.New("provisioning attempt already recorded or not durable; inspect before any further action")
	}
	rawURL.Path = "/" + saved.Intent.Database
	rawURL.RawPath = ""
	targetURI := rawURL.String()
	// This is operator-only maintenance configuration, never application access.
	if err = recoveryarchive.WriteHostJSONOutside(saved.BackupDirectory, path+".database.json", recoveryDatabaseFile{DatabaseURL: targetURI}); err != nil {
		return err
	}
	ob, err = storage.CreateRecoveryProvision(ctx, admin, saved.Intent)
	if err != nil {
		return err
	}
	targetDSN, err := explicitRecoveryDSN(targetURI)
	if err != nil {
		return err
	}
	target, err := storage.OpenRecoveryProvisionTarget(ctx, admin, targetDSN, saved.Intent, ob.DatabaseOID)
	if err != nil {
		return errors.New("restricted target unavailable; inspect plan")
	}
	defer target.Close()
	if err = target.Migrate(ctx); err != nil {
		return errors.New("target schema initialization not confirmed; inspect without recreating")
	}
	state, err := target.InspectRecoveryTarget(ctx)
	if err != nil || state.State != "normal" || state.InstallationID == saved.Intent.SourceID {
		return errors.New("fresh target identity not confirmed")
	}
	config := recoveryHostTarget{DatabaseURL: targetURI, TargetInstallationID: state.InstallationID, RestoreID: saved.Intent.OperationID, BackupID: saved.Intent.BackupID, SourceID: saved.Intent.SourceID, CutUTC: saved.CutUTC, SchemaSHA256: saved.Intent.SchemaSHA256, SnapshotSHA256: saved.Intent.SnapshotSHA256, ReceiptSHA256: saved.ReceiptSHA256}
	if err = recoveryarchive.WriteHostJSONOutside(saved.BackupDirectory, path+".target.json", config); err != nil {
		return err
	}
	if _, err = target.PrepareRecoveryTarget(ctx, config.spec()); err != nil {
		return errors.New("target hold not confirmed; inspect saved target intent")
	}
	return emit(recoveryImportStatus("empty_held", config))
}
