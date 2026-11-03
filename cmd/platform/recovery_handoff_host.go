// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"errors"
	"net/url"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/recoveryprocess"
	"agentplatform.local/platform/internal/storage"
	"github.com/jackc/pgx/v5"
)

const recoveryHandoffProfile = "drained-compose-new-held-baseline-v1"

// Private host provenance. Its digest is also part of the immutable physical
// provisioning markers, not merely a claim in a target configuration file.
type recoveryProvisionHandoff struct {
	Version            int                         `json:"version"`
	Profile            string                      `json:"profile"`
	SourceIntentFile   string                      `json:"source_intent_file"`
	SourceIntentSHA256 string                      `json:"source_intent_sha256"`
	SourceStopFile     string                      `json:"source_stop_file"`
	SourceStopSHA256   string                      `json:"source_stop_sha256"`
	CaptureLinkSHA256  string                      `json:"capture_link_sha256"`
	Capture            recoveryarchive.CaptureLink `json:"capture"`
	BaselineDirectory  string                      `json:"baseline_directory"`
	KeysFile           string                      `json:"keys_file"`
	StagingParent      string                      `json:"staging_parent"`
}

func (h *recoveryProvisionHandoff) matchesManifest(m recoveryarchive.Manifest) bool {
	return h != nil && h.Version == 1 && h.Profile == recoveryHandoffProfile && h.Capture.Version == 2 && m.PlaintextSHA256 == h.Capture.EvidenceSnapshotSHA256 && m.PlaintextBytes == h.Capture.ExportWireBytes && m.Metadata == (recoveryarchive.Metadata{BackupID: h.Capture.EvidenceBackupID, SourceID: h.Capture.SourceID, CutUTC: h.Capture.EvidenceCutUTC, SchemaSHA256: h.Capture.EvidenceSchemaSHA256})
}

func provisionHandoffBindingValid(saved recoveryProvisionFile) bool {
	if saved.Handoff == nil {
		return saved.Intent.ProvenanceSHA256 == ""
	}
	h, p := saved.Handoff, saved.Intent
	l := h.Capture
	return h.Version == 1 && h.Profile == recoveryHandoffProfile && l.Version == 2 && l.ExportAlgorithm == recoveryarchive.DrainedCaptureAlgorithm && captureHash(h) == p.ProvenanceSHA256 && saved.BackupDirectory == h.BaselineDirectory && saved.ReceiptSHA256 == l.EvidenceReceiptSHA256 && saved.CutUTC == l.EvidenceCutUTC && p.BackupID == l.EvidenceBackupID && p.SourceID == l.SourceID && p.SnapshotSHA256 == l.EvidenceSnapshotSHA256 && p.SchemaSHA256 == l.EvidenceSchemaSHA256
}

// Read both archives and exact retained source provenance, then recheck the
// original stopped generation. This never stops processes or creates a target.
func inspectProvisionHandoff(ctx context.Context, sourceFile, stopFile, bundle, keys, stage, receipt, planFile string, extraPaths ...string) (*recoveryProvisionHandoff, error) {
	var source recoveryProcessFile
	if sourceFile == "" || stopFile == "" || readRecoveryFenceJSON(sourceFile, &source) != nil || source.Binding == nil || source.Capture == nil {
		return nil, errors.New("handoff requires a private bound source capture and final stop")
	}
	capture := source.Capture
	if bundle != capture.Directory || keys != capture.Keys || stage != capture.Stage {
		return nil, errors.New("handoff must explicitly select the captured late archive")
	}
	paths := append([]string{sourceFile, sourceFile + ".apply", sourceFile + ".capture", sourceFile + ".capture-state", stopFile, source.Socket, keys, stage, planFile, planFile + ".apply", planFile + ".database.json", planFile + ".target.json"}, extraPaths...)
	for _, archive := range []string{capture.SelectedDirectory, capture.Directory} {
		for _, path := range paths {
			if recoveryarchive.RequireHostPathOutside(archive, path) != nil {
				return nil, errors.New("handoff authority and outputs must remain outside both archives")
			}
		}
	}
	link, err := loadSourceCaptureLink(source, sourceFile)
	if err != nil {
		return nil, err
	}
	l := link.Link()
	if l.Version != 2 || l.EvidenceReceiptSHA256 != receipt {
		return nil, errors.New("handoff requires an authenticated drained capture V2")
	}
	original, err := recoveryarchive.VerifyHostBackup(ctx, capture.SelectedDirectory, keys, l.SelectedReceiptSHA256, stage)
	if err != nil {
		return nil, err
	}
	m := original.Manifest()
	closeErr := original.Close()
	if closeErr != nil || m.PlaintextSHA256 != l.SelectedSnapshotSHA256 || m.Metadata.SourceID != l.SourceID || m.Metadata.CutUTC != l.SelectedCutUTC {
		return nil, errors.New("handoff original archive provenance differs")
	}
	if ok, err := inspectSourceCapture(ctx, source, sourceFile); err != nil || !ok {
		return nil, errors.New("handoff captured archive not verified")
	}
	stop, err := recoveryarchive.VerifyHostSourceStop(stopFile, keys, link)
	if err != nil {
		return nil, err
	}
	client, err := recoveryprocess.New(source.Socket)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	if _, err = recoveryarchive.RecheckHostSourceStop(ctx, client, stop, link); err != nil {
		return nil, errors.New("handoff source no longer matches its stopped generation")
	}
	return &recoveryProvisionHandoff{Version: 1, Profile: recoveryHandoffProfile, SourceIntentFile: sourceFile, SourceIntentSHA256: captureHash(source), SourceStopFile: stopFile, SourceStopSHA256: stop.SHA256(), CaptureLinkSHA256: link.SHA256(), Capture: l, BaselineDirectory: bundle, KeysFile: keys, StagingParent: stage}, nil
}

func recheckProvisionHandoff(ctx context.Context, saved recoveryProvisionFile, path string, extraPaths ...string) error {
	if saved.Handoff == nil || !provisionHandoffBindingValid(saved) {
		return errors.New("handoff plan binding invalid")
	}
	h := saved.Handoff
	fresh, err := inspectProvisionHandoff(ctx, h.SourceIntentFile, h.SourceStopFile, saved.BackupDirectory, h.KeysFile, h.StagingParent, saved.ReceiptSHA256, path, extraPaths...)
	if err != nil {
		return err
	}
	if captureHash(fresh) != saved.Intent.ProvenanceSHA256 {
		return errors.New("handoff provenance changed since provisioning plan")
	}
	return nil
}

// Import uses the existing physical target witness on every connection. A
// generic config cannot promote itself to handoff by merely adding a digest.
func openRecoveryImportTarget(ctx context.Context, config recoveryHostTarget, targetFile, bundle, keys, stage string) (*storage.Store, func(), error) {
	if config.HandoffPlanFile == "" && config.ProvenanceSHA256 == "" {
		dsn, err := explicitRecoveryDSN(config.DatabaseURL)
		if err != nil {
			return nil, nil, err
		}
		s, err := storage.Open(ctx, dsn)
		if err != nil {
			return nil, nil, err
		}
		return s, s.Close, nil
	}
	var saved recoveryProvisionFile
	if config.HandoffPlanFile == "" || readRecoveryFenceJSON(config.HandoffPlanFile, &saved) != nil || saved.Handoff == nil || !provisionHandoffBindingValid(saved) || saved.Intent.ProvenanceSHA256 != config.ProvenanceSHA256 {
		return nil, nil, errors.New("handoff target lacks its exact provisioning provenance")
	}
	h, p := saved.Handoff, saved.Intent
	if bundle != h.BaselineDirectory || keys != h.KeysFile || stage != h.StagingParent || config.RestoreID != p.OperationID || config.BackupID != p.BackupID || config.SourceID != p.SourceID || config.CutUTC != saved.CutUTC || config.SchemaSHA256 != p.SchemaSHA256 || config.SnapshotSHA256 != p.SnapshotSHA256 || config.ReceiptSHA256 != saved.ReceiptSHA256 {
		return nil, nil, errors.New("handoff target baseline differs from provisioned selection")
	}
	for _, path := range []string{targetFile, config.HandoffPlanFile} {
		if recoveryarchive.RequireHostPathOutside(bundle, path) != nil {
			return nil, nil, errors.New("handoff target authority is inside archive")
		}
	}
	if err := recheckProvisionHandoff(ctx, saved, config.HandoffPlanFile, targetFile); err != nil {
		return nil, nil, err
	}
	u, err := url.Parse(saved.AdminDatabaseURL)
	if err != nil || u.Query().Get("search_path") != "" {
		return nil, nil, errors.New("invalid handoff maintenance endpoint")
	}
	u.Path = "/" + p.Database
	u.RawPath = ""
	if config.DatabaseURL != u.String() {
		return nil, nil, errors.New("handoff target endpoint differs from provisioned endpoint")
	}
	adminDSN, err := explicitRecoveryDSN(saved.AdminDatabaseURL)
	if err != nil {
		return nil, nil, err
	}
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return nil, nil, errors.New("handoff maintenance endpoint unavailable")
	}
	closeAdmin := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = admin.Close(cleanup)
	}
	ob, err := storage.InspectRecoveryProvision(ctx, admin, p)
	if err != nil || ob.State != "database_restricted" {
		closeAdmin()
		return nil, nil, errors.New("handoff physical target provenance differs")
	}
	dsn, err := explicitRecoveryDSN(config.DatabaseURL)
	if err != nil {
		closeAdmin()
		return nil, nil, err
	}
	s, err := storage.OpenRecoveryProvisionTarget(ctx, admin, dsn, p, ob.DatabaseOID)
	if err != nil {
		closeAdmin()
		return nil, nil, err
	}
	return s, func() { s.Close(); closeAdmin() }, nil
}
