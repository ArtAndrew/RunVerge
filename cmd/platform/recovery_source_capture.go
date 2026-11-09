// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/recoveryprocess"
	"agentplatform.local/platform/internal/storage"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

type recoverySourceCapture struct {
	Directory         string `json:"directory"`
	Keys              string `json:"keys_file"`
	Stage             string `json:"staging_parent"`
	SelectedReceipt   string `json:"selected_receipt_sha256"`
	SelectedDirectory string `json:"selected_directory"`
	SelectedCut       string `json:"selected_cut_utc"`
}

func validateSourceCapturePaths(selected, path string, c recoverySourceCapture) error {
	for _, file := range []string{path, path + ".apply", path + ".capture", path + ".capture-state", path + ".source-stop", path + ".retirement-intent", path + ".retirement", path + sourceRetirementSealSuffix, c.Keys, c.Stage} {
		if recoveryarchive.RequireHostPathOutside(selected, file) != nil || recoveryarchive.RequireHostPathOutsideNewDirectory(c.Directory, file) != nil {
			return errors.New("evidence authority and staging must remain outside both archives")
		}
	}
	if recoveryarchive.RequireHostPathOutside(selected, c.Directory) != nil || recoveryarchive.RequireHostPathOutsideNewDirectory(c.Directory, selected) != nil {
		return errors.New("source evidence must use a separate new directory")
	}
	if _, err := os.Lstat(c.Directory); !errors.Is(err, os.ErrNotExist) {
		return errors.New("source evidence directory must not exist")
	}
	return nil
}
func sourceCaptureIdentity(saved recoveryProcessFile) storage.RecoverySourceIdentity {
	b := saved.Binding
	return storage.RecoverySourceIdentity{SourceID: b.SourceID, SystemID: b.SystemID, DatabaseOID: b.DatabaseOID, AfterCutUTC: saved.Capture.SelectedCut}
}
func prepareSourceCapture(ctx context.Context, c *recoveryprocess.Client, saved recoveryProcessFile, path string) (*storage.Store, error) {
	if err := validateSourceCapturePaths(saved.Capture.SelectedDirectory, path, *saved.Capture); err != nil {
		return nil, err
	}
	a, err := recoveryarchive.VerifyHostBackup(ctx, saved.Capture.SelectedDirectory, saved.Capture.Keys, saved.Capture.SelectedReceipt, saved.Capture.Stage)
	if err != nil {
		return nil, err
	}
	m := a.Manifest()
	if e := a.Close(); e != nil {
		return nil, e
	}
	if m.PlaintextSHA256 != saved.Binding.SnapshotSHA256 || m.Metadata.SourceID != saved.Binding.SourceID || m.Metadata.CutUTC != saved.Capture.SelectedCut {
		return nil, errors.New("selected source archive changed before capture")
	}
	dsn, err := c.PreflightEvidenceDatabaseURL(ctx, saved.Intent, *saved.Binding)
	if err != nil {
		return nil, err
	}
	dsn, err = explicitRecoveryDSN(dsn)
	if err != nil {
		return nil, err
	}
	s, err := storage.OpenRecoveryEvidenceSource(ctx, dsn, sourceCaptureIdentity(saved))
	if err != nil {
		return nil, errors.New("source evidence database preflight failed; no shutdown attempted")
	}
	return s, nil
}
func captureHash(v any) string { b, _ := json.Marshal(v); return storage.Hash(b) }

func applySourceCapture(ctx context.Context, c *recoveryprocess.Client, saved recoveryProcessFile, path string, s *storage.Store) (recoveryprocess.Observation, error) {
	var zero recoveryprocess.Observation
	_, err := c.StopExecutors(ctx, saved.Intent)
	if err != nil {
		return zero, err
	}
	observed, err := c.ObserveSourceCapture(ctx, saved.Intent, *saved.Binding)
	if err != nil {
		return zero, err
	}
	stopped := observed.Record().Observation
	if _, err = c.EvidenceDatabaseURL(ctx, saved.Intent, *saved.Binding); err != nil {
		return zero, err
	}
	identity := sourceCaptureIdentity(saved)
	identity.Observe = func(ctx context.Context, nonce string) error {
		return c.ObserveEvidenceBackend(ctx, saved.Intent, *saved.Binding, nonce)
	}
	var exported storage.RecoverySnapshotReport
	receipt, err := recoveryarchive.CreateHostBackup(ctx, saved.Capture.Directory, saved.Capture.Keys, saved.Capture.Stage, func(ctx context.Context, w io.Writer) (recoveryarchive.Metadata, error) {
		r, e := s.ExportRecoverySourceEvidence(ctx, w, storage.DefaultRecoverySnapshotLimits(), identity)
		if e != nil {
			return recoveryarchive.Metadata{}, e
		}
		// Recheck the exact executor and DB identities before sealing evidence.
		if _, e = c.EvidenceDatabaseURL(ctx, saved.Intent, *saved.Binding); e != nil {
			return recoveryarchive.Metadata{}, e
		}
		exported = r
		m := r.Metadata
		return recoveryarchive.Metadata{BackupID: m.BackupID, SourceID: m.SourceID, CutUTC: m.CutUTC, SchemaSHA256: m.SchemaSHA256}, nil
	})
	if err != nil {
		return zero, errors.New("source evidence capture incomplete; executors may be stopped, inspect without repeating apply")
	}
	link, err := completedSourceCaptureLink(saved, observed, exported, receipt)
	if err != nil {
		return zero, err
	}
	if err = recoveryarchive.WriteHostJSON(path+".capture-state", stopped); err != nil {
		return zero, errors.New("capture observation not confirmed; database was not stopped")
	}
	if err = recoveryarchive.WriteHostCaptureLink(path+".capture", saved.Capture.Keys, link); err != nil {
		return zero, errors.New("evidence link not confirmed; database was not stopped; inspect private output")
	}
	if saved.RetireTo != nil {
		return retireCapturedSource(ctx, c, saved, path, s)
	}
	// Receipt is durable before DB shutdown. It never claims the DB is stopped.
	s.Close()
	return c.StopDatabaseAfterEvidence(ctx, saved.Intent)
}

// Only the completed drained/nonce-witnessed export path calls this mint helper.
// The local signer attests that path, not provider settlement or permission to
// resume work. The receipt must describe exactly the stream returned by export.
func completedSourceCaptureLink(saved recoveryProcessFile, observed recoveryprocess.ObservedSourceCapture, report storage.RecoverySnapshotReport, receipt recoveryarchive.HostBackupReceipt) (recoveryarchive.CaptureLink, error) {
	var zero recoveryarchive.CaptureLink
	r := observed.Record()
	m := receipt.Manifest
	if saved.Binding == nil || saved.Capture == nil || !recoveryprocess.SourceCaptureRecordConsistent(r) || captureHash(r.Intent) != captureHash(saved.Intent) || captureHash(r.Binding) != captureHash(saved.Binding) || report.SHA256 == "" || report.SHA256 != m.PlaintextSHA256 || report.WireBytes <= 0 || report.WireBytes != m.PlaintextBytes || report.Metadata.SourceID != saved.Binding.SourceID || report.Metadata.BackupID != m.Metadata.BackupID || report.Metadata.SourceID != m.Metadata.SourceID || report.Metadata.CutUTC != m.Metadata.CutUTC || report.Metadata.SchemaSHA256 != m.Metadata.SchemaSHA256 {
		return zero, errors.New("drained export differs from completed archive")
	}
	return recoveryarchive.CaptureLink{Version: 2, OperationID: saved.Intent.OperationID, SourceID: saved.Binding.SourceID, SelectedReceiptSHA256: saved.Capture.SelectedReceipt, SelectedSnapshotSHA256: saved.Binding.SnapshotSHA256, SelectedCutUTC: saved.Capture.SelectedCut, BindingSHA256: captureHash(saved.Binding), StoppedExecutorsSHA256: captureHash(r.Observation), EvidenceReceiptSHA256: receipt.ReceiptSHA256, EvidenceSnapshotSHA256: m.PlaintextSHA256, EvidenceCutUTC: m.Metadata.CutUTC, CapturedAt: time.Now().UTC(), ExportAlgorithm: recoveryarchive.DrainedCaptureAlgorithm, SystemID: saved.Binding.SystemID, DatabaseOID: saved.Binding.DatabaseOID, ExportWireBytes: report.WireBytes, EvidenceSchemaSHA256: m.Metadata.SchemaSHA256, EvidenceBackupID: m.Metadata.BackupID, ExecutorsStoppedAtUTC: r.Observation.ObservedAt.UTC().Format(time.RFC3339Nano)}, nil
}
func inspectSourceCapture(ctx context.Context, saved recoveryProcessFile, path string) (bool, error) {
	if _, err := os.Lstat(path + ".capture"); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	verified, err := loadSourceCaptureLink(saved, path)
	if err != nil {
		return false, err
	}
	archive, err := openSourceCaptureArchive(ctx, saved, verified)
	if err != nil {
		return false, err
	}
	if err = archive.Close(); err != nil {
		return false, err
	}
	return true, nil
}

func openSourceCaptureArchive(ctx context.Context, saved recoveryProcessFile, verified *recoveryarchive.VerifiedCaptureLink) (*recoveryarchive.VerifiedArchive, error) {
	archive, err := recoveryarchive.VerifyHostBackup(ctx, saved.Capture.Directory, saved.Capture.Keys, verified.Link().EvidenceReceiptSHA256, saved.Capture.Stage)
	if err != nil {
		return nil, err
	}
	m := archive.Manifest()
	if !verified.MatchesEvidenceManifest(m) || !storage.SupportedRecoverySourceSchema(m.Metadata.SchemaSHA256) {
		_ = archive.Close()
		return nil, errors.New("source evidence differs from signed link")
	}
	return archive, nil
}

func loadSourceCaptureLink(saved recoveryProcessFile, path string) (*recoveryarchive.VerifiedCaptureLink, error) {
	if saved.Capture == nil || saved.Binding == nil {
		return nil, errors.New("source evidence plan required")
	}
	verified, err := recoveryarchive.VerifyHostCaptureLink(path+".capture", saved.Capture.Keys)
	if err != nil {
		return nil, err
	}
	link := verified.Link()
	if link.OperationID != saved.Intent.OperationID || link.SourceID != saved.Binding.SourceID || link.BindingSHA256 != captureHash(saved.Binding) || link.SelectedReceiptSHA256 != saved.Capture.SelectedReceipt || link.SelectedSnapshotSHA256 != saved.Binding.SnapshotSHA256 || link.SelectedCutUTC != saved.Capture.SelectedCut {
		return nil, errors.New("source evidence link not authenticated for this plan")
	}
	var stopped recoveryprocess.Observation
	if readRecoveryFenceJSON(path+".capture-state", &stopped) != nil || captureHash(stopped) != link.StoppedExecutorsSHA256 || stopped.OperationID != saved.Intent.OperationID {
		return nil, errors.New("capture observation differs from signed link")
	}
	if link.Version == 2 && !verified.MatchesDrainedSource(recoveryprocess.SourceStopRecord{Intent: saved.Intent, Binding: *saved.Binding, Observation: stopped}) {
		return nil, errors.New("drained source observation differs from authenticated capture")
	}
	return verified, nil
}
