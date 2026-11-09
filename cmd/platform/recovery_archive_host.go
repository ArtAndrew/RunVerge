// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/storage"
)

var recoveryArchiveUsage = errors.New("usage: platform recovery backup-keygen|backup-create|backup-dedicated|archive-verify|target-prepare|target-inspect|archive-import|import-inspect [explicit private file options]")

type recoveryDatabaseFile struct {
	DatabaseURL string `json:"database_url"`
}

func runRecoveryArchiveHost(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return recoveryArchiveUsage
	}
	f := flag.NewFlagSet("recovery-archive", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var keys, output, bundle, stage, expectedReceipt, databaseFile, targetFile, expectedInstallation string
	var dedicatedOrganization string
	var confirmed bool
	addArchive := func() {
		f.StringVar(&bundle, "backup-dir", "", "selected private backup directory")
		f.StringVar(&keys, "keys-file", "", "independently trusted private archive keys")
		f.StringVar(&stage, "staging-parent", "", "existing private temporary staging parent")
	}
	addSelection := func() {
		addArchive()
		f.StringVar(&expectedReceipt, "expected-receipt-sha256", "", "digest recorded independently at backup creation")
	}
	addDatabase := func() { f.StringVar(&databaseFile, "database-config", "", "explicit private JSON with database_url") }
	addTarget := func() { f.StringVar(&targetFile, "target-config", "", "exact durable private target intent") }
	switch args[0] {
	case "backup-keygen":
		f.StringVar(&output, "out", "", "new private archive key file")
	case "backup-create", "backup-dedicated":
		if args[0] == "backup-dedicated" {
			f.StringVar(&dedicatedOrganization, "organization-id", "", "only organization permitted in the dedicated DATABASE_URL snapshot")
		} else {
			addDatabase()
			addTarget()
		}
		f.StringVar(&keys, "keys-file", "", "private archive keys")
		f.StringVar(&stage, "staging-parent", "", "private staging parent")
		f.StringVar(&output, "out-dir", "", "new backup directory")
	case "archive-verify":
		addSelection()
	case "target-prepare":
		addSelection()
		addDatabase()
		f.StringVar(&expectedInstallation, "expected-installation-id", "", "independently inspected fresh target installation ID")
		f.StringVar(&output, "intent-out", "", "new private intent file, written before target preparation")
		f.BoolVar(&confirmed, "confirm-new-target", false, "attest newly provisioned isolated target; never the source")
	case "target-inspect":
		addDatabase()
		addTarget()
	case "archive-import":
		addArchive()
		addTarget()
	case "import-inspect":
		addTarget()
		addArchive()
	default:
		return recoveryArchiveUsage
	}
	if f.Parse(args[1:]) != nil || f.NArg() != 0 {
		return recoveryArchiveUsage
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	emit := func(value any) error { return json.NewEncoder(out).Encode(value) }
	if args[0] == "backup-keygen" {
		if output == "" {
			return recoveryArchiveUsage
		}
		summary, err := recoveryarchive.GenerateHostKeys(output)
		if err != nil {
			return err
		}
		return emit(summary)
	}
	openDatabase := func(path string) (*storage.Store, string, error) {
		var config recoveryDatabaseFile
		if path == "" || readPrivateJSON(path, &config) != nil {
			return nil, "", errors.New("explicit private database configuration required")
		}
		dsn, err := explicitRecoveryDSN(config.DatabaseURL)
		if err != nil {
			return nil, "", err
		}
		s, err := storage.Open(ctx, dsn)
		if err != nil {
			return nil, "", errors.New("cannot open explicit recovery database")
		}
		return s, config.DatabaseURL, nil
	}
	if args[0] == "backup-create" || args[0] == "backup-dedicated" {
		dedicated := args[0] == "backup-dedicated"
		if output == "" || keys == "" || stage == "" {
			return recoveryArchiveUsage
		}
		if dedicated && dedicatedOrganization == "" || !dedicated && (databaseFile == "") == (targetFile == "") {
			return recoveryArchiveUsage
		}
		var config recoveryHostTarget
		var s *storage.Store
		var err error
		switch {
		case dedicated:
			if recoveryarchive.RequireHostPathOutsideNewDirectory(output, keys) != nil ||
				recoveryarchive.RequireHostPathOutsideNewDirectory(output, stage) != nil {
				return errors.New("choose a new private output directory separate from the keys and staging")
			}
			dsn, e := explicitRecoveryDSN(os.Getenv("DATABASE_URL"))
			if e != nil {
				return e
			}
			s, err = storage.Open(ctx, dsn)
		case targetFile != "":
			if readPrivateJSON(targetFile, &config) != nil {
				return errors.New("invalid private target intent")
			}
			dsn, e := explicitRecoveryDSN(config.DatabaseURL)
			if e != nil {
				return e
			}
			s, err = storage.Open(ctx, dsn)
		default:
			s, _, err = openDatabase(databaseFile)
		}
		if err != nil {
			return errors.New("cannot open explicit recovery database")
		}
		defer s.Close()
		receipt, err := recoveryarchive.CreateHostBackup(ctx, output, keys, stage, func(ctx context.Context, w io.Writer) (recoveryarchive.Metadata, error) {
			var report storage.RecoverySnapshotReport
			var e error
			limits := storage.DefaultRecoverySnapshotLimits()
			switch {
			case dedicated:
				report, e = s.ExportDedicatedRecoverySnapshot(ctx, w, limits, dedicatedOrganization)
			case targetFile != "":
				report, e = s.ExportHeldRecoveryLineageSnapshot(ctx, w, limits, config.spec(), config.SnapshotSHA256, stage)
			default:
				report, e = s.ExportRecoverySnapshot(ctx, w, limits)
			}
			if e != nil {
				return recoveryarchive.Metadata{}, e
			}
			m := report.Metadata
			return recoveryarchive.Metadata{BackupID: m.BackupID, SourceID: m.SourceID, CutUTC: m.CutUTC, SchemaSHA256: m.SchemaSHA256}, nil
		})
		if err != nil {
			return err
		}
		return emit(receipt)
	}
	var config recoveryHostTarget
	if targetFile != "" {
		if readPrivateJSON(targetFile, &config) != nil {
			return errors.New("invalid private target intent")
		}
	}
	if args[0] == "target-inspect" {
		if (databaseFile == "") == (targetFile == "") {
			return recoveryArchiveUsage
		}
		var s *storage.Store
		var err error
		if targetFile != "" {
			dsn, e := explicitRecoveryDSN(config.DatabaseURL)
			if e != nil {
				return e
			}
			s, err = storage.Open(ctx, dsn)
		} else {
			s, _, err = openDatabase(databaseFile)
		}
		if err != nil {
			return errors.New("cannot inspect explicit target")
		}
		defer s.Close()
		state, err := s.InspectRecoveryTarget(ctx)
		if err != nil {
			return errors.New("target inspection failed")
		}
		if targetFile != "" && (state.InstallationID != config.TargetInstallationID || (state.State == "held" && !recoveryStateMatchesIntent(state, config))) {
			return errors.New("target differs from saved intent")
		}
		return emit(state) // normal is not an emptiness/source-isolation certificate
	}
	if args[0] == "import-inspect" {
		if targetFile == "" {
			return recoveryArchiveUsage
		}
		dsn, err := explicitRecoveryDSN(config.DatabaseURL)
		if err != nil {
			return err
		}
		s, err := storage.Open(ctx, dsn)
		if err != nil {
			return errors.New("cannot inspect explicit target")
		}
		defer s.Close()
		inspection, err := inspectRecoveryHostImport(ctx, s, config, targetFile, bundle, keys, stage)
		if err != nil {
			return errors.New("exact import inspection failed; do not retry or clear the target")
		}
		return emit(recoveryImportStatus(inspection.State, config))
	}
	if args[0] == "archive-import" {
		if targetFile == "" || bundle == "" || recoveryarchive.RequireHostPathOutside(bundle, targetFile) != nil {
			return recoveryArchiveUsage
		}
		expectedReceipt = config.ReceiptSHA256
	}
	if bundle == "" || keys == "" || stage == "" || expectedReceipt == "" {
		return recoveryArchiveUsage
	}
	archive, err := recoveryarchive.VerifyHostBackup(ctx, bundle, keys, expectedReceipt, stage)
	if err != nil {
		return err
	}
	defer archive.Close()
	manifest := archive.Manifest()
	limits := storage.DefaultRecoverySnapshotLimits()
	report, err := storage.ValidateRecoveryHostSnapshot(ctx, archive, limits)
	if err != nil {
		return errors.New("authenticated archive has invalid snapshot framing")
	}
	m := report.Metadata
	if args[0] == "archive-verify" {
		if err = archive.Close(); err != nil {
			return err
		}
		return emit(struct {
			Scope                  string                   `json:"scope"`
			FullSemanticValidation bool                     `json:"full_semantic_validation"`
			ReceiptSHA256          string                   `json:"receipt_sha256"`
			Manifest               recoveryarchive.Manifest `json:"manifest"`
		}{"authenticated_envelope_and_snapshot_framing", false, expectedReceipt, manifest})
	}
	if args[0] == "target-prepare" {
		if !confirmed || output == "" || expectedInstallation == "" || !storage.SupportedRecoverySourceSchema(manifest.Metadata.SchemaSHA256) {
			return recoveryArchiveUsage
		}
		if err = archive.Close(); err != nil {
			return err
		}
		s, databaseURI, err := openDatabase(databaseFile)
		if err != nil {
			return err
		}
		defer s.Close()
		state, err := s.InspectRecoveryTarget(ctx)
		if err != nil || state.State != "normal" || state.InstallationID != expectedInstallation || state.InstallationID == manifest.Metadata.SourceID {
			return errors.New("selected target is not the expected fresh distinct installation")
		}
		config = recoveryHostTarget{DatabaseURL: databaseURI, TargetInstallationID: state.InstallationID, RestoreID: domain.NewID("restore"), BackupID: m.BackupID, SourceID: m.SourceID, CutUTC: m.CutUTC, SchemaSHA256: m.SchemaSHA256, SnapshotSHA256: report.SHA256, ReceiptSHA256: expectedReceipt}
		// A lost prepare acknowledgment must leave its exact durable selector.
		if err = recoveryarchive.WriteHostJSONOutside(bundle, output, config); err != nil {
			return err
		}
		if _, err = s.PrepareRecoveryTarget(ctx, config.spec()); err != nil {
			return errors.New("target preparation not confirmed; inspect the saved intent, do not automatically repeat")
		}
		return emit(recoveryImportStatus("empty_held", config))
	}
	if args[0] != "archive-import" {
		return recoveryArchiveUsage
	}
	if config.SnapshotSHA256 != report.SHA256 || config.SourceID != m.SourceID || config.BackupID != m.BackupID || config.CutUTC != m.CutUTC || config.SchemaSHA256 != m.SchemaSHA256 {
		return errors.New("selected archive differs from durable target intent")
	}
	s, closeTarget, err := openRecoveryImportTarget(ctx, config, targetFile, bundle, keys, stage)
	if err != nil {
		return errors.New("cannot open explicit import target")
	}
	defer closeTarget()
	if report.Lineage != nil {
		_, err = s.ImportRecoverySnapshotLineage(ctx, archive, config.spec(), limits, stage)
	} else {
		_, err = s.ImportRecoverySnapshot(ctx, archive, config.spec(), limits)
	}
	if err != nil {
		return err
	}
	if err = archive.Close(); err != nil {
		return errors.New("import returned but archive cleanup unconfirmed; inspect exact target")
	}
	return emit(recoveryImportStatus("committed_held", config))
}

func inspectRecoveryHostImport(ctx context.Context, s *storage.Store, config recoveryHostTarget, targetFile, bundle, keys, stage string) (result storage.RecoveryImportInspection, resultErr error) {
	if bundle == "" && keys == "" && stage == "" {
		return s.InspectRecoveryImport(ctx, config.spec(), config.SnapshotSHA256)
	}
	if bundle == "" || keys == "" || stage == "" || config.ReceiptSHA256 == "" || recoveryarchive.RequireHostPathOutside(bundle, targetFile) != nil {
		return result, recoveryArchiveUsage
	}
	archive, err := recoveryarchive.VerifyHostBackup(ctx, bundle, keys, config.ReceiptSHA256, stage)
	if err != nil {
		return result, err
	}
	defer func() {
		if archive.Close() != nil {
			result, resultErr = storage.RecoveryImportInspection{}, errors.New("archive cleanup unconfirmed")
		}
	}()
	if archive.Manifest().PlaintextSHA256 != config.SnapshotSHA256 {
		return result, errors.New("selected archive differs from durable target intent")
	}
	return s.InspectRecoverySnapshotLineage(ctx, archive, config.spec(), storage.DefaultRecoverySnapshotLimits())
}

func recoveryStateMatchesIntent(state storage.RecoveryTargetState, c recoveryHostTarget) bool {
	return state.RestoreID == c.RestoreID && state.BackupID == c.BackupID && state.SourceID == c.SourceID && state.CutUTC == c.CutUTC && state.SchemaSHA256 == c.SchemaSHA256
}
func recoveryImportStatus(state string, c recoveryHostTarget) any {
	return struct {
		State          string `json:"state"`
		TargetID       string `json:"target_id"`
		RestoreID      string `json:"restore_id"`
		SnapshotSHA256 string `json:"snapshot_sha256"`
	}{state, c.TargetInstallationID, c.RestoreID, c.SnapshotSHA256}
}
