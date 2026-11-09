// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"errors"
	"net/url"
	"os"
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
	Retirement         *recoveryHandoffRetirement  `json:"retirement,omitempty"`
}

type recoveryHandoffRetirement struct {
	Destination       storage.RecoveryRetirementDestination `json:"destination"`
	IntentSHA256      string                                `json:"intent_sha256"`
	LastReceiptSHA256 string                                `json:"last_receipt_sha256"`
	CutProofSHA256    string                                `json:"cut_proof_sha256,omitempty"`
	SealSHA256        string                                `json:"seal_sha256,omitempty"`
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
	if h.Retirement != nil {
		d := h.Retirement.Destination
		if p.OperationID != d.RestoreID || p.SystemID != d.SystemID || p.WALTimeline != d.WALTimeline || p.MaintenanceOID != d.MaintenanceOID || p.AdminOID != d.AdminOID {
			return false
		}
	} else if p.WALTimeline != "" {
		return false
	}
	return h.Version == 1 && h.Profile == recoveryHandoffProfile && l.Version == 2 && l.ExportAlgorithm == recoveryarchive.DrainedCaptureAlgorithm && captureHash(h) == p.ProvenanceSHA256 && saved.BackupDirectory == h.BaselineDirectory && saved.ReceiptSHA256 == l.EvidenceReceiptSHA256 && saved.CutUTC == l.EvidenceCutUTC && p.BackupID == l.EvidenceBackupID && p.SourceID == l.SourceID && p.SnapshotSHA256 == l.EvidenceSnapshotSHA256 && p.SchemaSHA256 == l.EvidenceSchemaSHA256
}

type recoveryHandoffSource struct {
	provenance        recoveryProvisionHandoff
	selectedDirectory string
	socket            string
	link              *recoveryarchive.VerifiedCaptureLink
	stop              *recoveryarchive.VerifiedSourceStop
}

func loadRecoveryHandoffSource(sourceFile, stopFile, bundle, keys, stage, receipt, planFile string, extraPaths ...string) (*recoveryHandoffSource, error) {
	var source recoveryProcessFile
	if sourceFile == "" || stopFile == "" || readRecoveryFenceJSON(sourceFile, &source) != nil || source.Binding == nil || source.Capture == nil {
		return nil, errors.New("handoff requires a private bound source capture and final stop")
	}
	capture := source.Capture
	if bundle != capture.Directory || keys != capture.Keys || stage != capture.Stage {
		return nil, errors.New("handoff must explicitly select the captured late archive")
	}
	paths := append([]string{sourceFile, sourceFile + ".apply", sourceFile + ".capture", sourceFile + ".capture-state", sourceFile + ".retirement-intent", sourceFile + ".retirement", sourceFile + sourceRetirementSealSuffix, stopFile, keys, stage, planFile, planFile + ".apply", planFile + ".database.json", planFile + ".target.json"}, extraPaths...)
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
	retirement, seal, err := loadHandoffRetirement(source, sourceFile, link)
	if err != nil {
		return nil, err
	}
	stop, err := recoveryarchive.VerifyHostSourceStop(stopFile, keys, link, seal)
	if err != nil {
		return nil, err
	}
	return &recoveryHandoffSource{
		provenance:        recoveryProvisionHandoff{Version: 1, Profile: recoveryHandoffProfile, SourceIntentFile: sourceFile, SourceIntentSHA256: captureHash(source), SourceStopFile: stopFile, SourceStopSHA256: stop.SHA256(), CaptureLinkSHA256: link.SHA256(), Capture: l, BaselineDirectory: bundle, KeysFile: keys, StagingParent: stage, Retirement: retirement},
		selectedDirectory: capture.SelectedDirectory, socket: source.Socket, link: link, stop: stop,
	}, nil
}

// This carries a previous observation into held provisioning. It is not a
// live database check and supplies no ordinary execution authority.
func loadHandoffRetirement(source recoveryProcessFile, path string, link *recoveryarchive.VerifiedCaptureLink) (*recoveryHandoffRetirement, *recoveryarchive.VerifiedSourceRetirementSeal, error) {
	if source.RetireTo == nil {
		for _, suffix := range []string{".retirement-intent", ".retirement", sourceRetirementSealSuffix} {
			if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
				return nil, nil, errors.New("retirement selection is missing from the source plan")
			}
		}
		return nil, nil, nil
	}
	p, o, seal, err := readSavedSourceRetirement(source, path, link)
	if err != nil {
		return nil, nil, err
	}
	return &recoveryHandoffRetirement{
		Destination: p.Destination, IntentSHA256: captureHash(p), LastReceiptSHA256: o.ReceiptSHA256, CutProofSHA256: o.CutProofSHA256, SealSHA256: seal.SHA256(),
	}, seal, nil
}

func (s *recoveryHandoffSource) openOriginal(ctx context.Context) (*recoveryarchive.VerifiedArchive, error) {
	h, l := s.provenance, s.provenance.Capture
	original, err := recoveryarchive.VerifyHostBackup(ctx, s.selectedDirectory, h.KeysFile, l.SelectedReceiptSHA256, h.StagingParent)
	if err != nil {
		return nil, err
	}
	m := original.Manifest()
	if m.PlaintextSHA256 != l.SelectedSnapshotSHA256 || m.Metadata.SourceID != l.SourceID || m.Metadata.CutUTC != l.SelectedCutUTC {
		_ = original.Close()
		return nil, errors.New("handoff original archive provenance differs")
	}
	return original, nil
}

func (s *recoveryHandoffSource) openBaseline(ctx context.Context) (*recoveryarchive.VerifiedArchive, error) {
	h, l := s.provenance, s.provenance.Capture
	late, err := recoveryarchive.VerifyHostBackup(ctx, h.BaselineDirectory, h.KeysFile, l.EvidenceReceiptSHA256, h.StagingParent)
	if err != nil {
		return nil, err
	}
	if !s.link.MatchesEvidenceManifest(late.Manifest()) || !storage.SupportedRecoverySourceSchema(late.Manifest().Metadata.SchemaSHA256) {
		_ = late.Close()
		return nil, errors.New("handoff captured archive not verified")
	}
	return late, nil
}

// Only live source observation requires access to the source Docker socket.
// Commands that use retained evidence can run on a separate maintenance host.
func (s *recoveryHandoffSource) openSourceObserver() (*recoveryprocess.Client, error) {
	if recoveryarchive.RequireHostPathOutside(s.selectedDirectory, s.socket) != nil ||
		recoveryarchive.RequireHostPathOutside(s.provenance.BaselineDirectory, s.socket) != nil {
		return nil, errors.New("handoff observer must remain outside both archives")
	}
	return recoveryprocess.New(s.socket)
}

func (s *recoveryHandoffSource) observeStop(ctx context.Context) (time.Time, error) {
	client, err := s.openSourceObserver()
	if err != nil {
		return time.Time{}, err
	}
	defer client.Close()
	stop, err := recoveryarchive.RecheckHostSourceStop(ctx, client, s.stop, s.link)
	if err != nil {
		return time.Time{}, errors.New("handoff source no longer matches its stopped generation")
	}
	return stop.Record().Observation.ObservedAt, nil
}

// Read both archives and exact retained source provenance, then recheck the
// original stopped generation. This never stops processes or creates a target.
func inspectProvisionHandoff(ctx context.Context, sourceFile, stopFile, bundle, keys, stage, receipt, planFile string, extraPaths ...string) (*recoveryProvisionHandoff, error) {
	s, err := loadRecoveryHandoffSource(sourceFile, stopFile, bundle, keys, stage, receipt, planFile, extraPaths...)
	if err != nil {
		return nil, err
	}
	original, err := s.openOriginal(ctx)
	if err != nil {
		return nil, err
	}
	if err = original.Close(); err != nil {
		return nil, err
	}
	late, err := s.openBaseline(ctx)
	if err != nil {
		return nil, err
	}
	if err = late.Close(); err != nil {
		return nil, err
	}
	if _, err = s.observeStop(ctx); err != nil {
		return nil, err
	}
	return &s.provenance, nil
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
	saved, err := loadProvisionedHandoffTarget(config)
	if err != nil {
		return nil, nil, err
	}
	h := saved.Handoff
	if bundle != h.BaselineDirectory || keys != h.KeysFile || stage != h.StagingParent {
		return nil, nil, errors.New("handoff target baseline differs from provisioned selection")
	}
	if err := recheckProvisionHandoff(ctx, saved, config.HandoffPlanFile, targetFile); err != nil {
		return nil, nil, err
	}
	return openBoundRecoveryHandoffTarget(ctx, config, saved)
}

func loadProvisionedHandoffTarget(config recoveryHostTarget) (recoveryProvisionFile, error) {
	var saved recoveryProvisionFile
	if config.HandoffPlanFile == "" || readRecoveryFenceJSON(config.HandoffPlanFile, &saved) != nil || saved.Handoff == nil || !provisionHandoffBindingValid(saved) || saved.Intent.ProvenanceSHA256 != config.ProvenanceSHA256 {
		return recoveryProvisionFile{}, errors.New("handoff target lacks its exact provisioning provenance")
	}
	p := saved.Intent
	if config.RestoreID != p.OperationID || config.BackupID != p.BackupID || config.SourceID != p.SourceID || config.CutUTC != saved.CutUTC || config.SchemaSHA256 != p.SchemaSHA256 || config.SnapshotSHA256 != p.SnapshotSHA256 || config.ReceiptSHA256 != saved.ReceiptSHA256 {
		return recoveryProvisionFile{}, errors.New("handoff target baseline differs from provisioned selection")
	}
	return saved, nil
}

func openBoundRecoveryHandoffTarget(ctx context.Context, config recoveryHostTarget, saved recoveryProvisionFile) (*storage.Store, func(), error) {
	return openBoundRecoveryHandoffTargetMode(ctx, config, saved, config.DatabaseOID, false)
}

func openBoundRecoveryAccountingTarget(ctx context.Context, config recoveryHostTarget) (*storage.Store, func(), error) {
	if config.HandoffPlanFile == "" && config.ProvenanceSHA256 == "" {
		dsn, err := explicitRecoveryDSN(config.DatabaseURL)
		if err != nil {
			return nil, nil, err
		}
		s, err := storage.Open(ctx, dsn)
		if err != nil {
			return nil, nil, errors.New("explicit recovery target unavailable")
		}
		return s, s.Close, nil
	}
	saved, err := loadProvisionedHandoffTarget(config)
	if err != nil {
		return nil, nil, err
	}
	s, closeTarget, err := openBoundRecoveryHandoffTarget(ctx, config, saved)
	if errors.Is(err, storage.ErrRecoveryProvision) && config.DatabaseOID != 0 {
		return openBoundRecoveryHandoffTargetMode(ctx, config, saved, config.DatabaseOID, true)
	}
	return s, closeTarget, err
}

func openBoundRecoveryHandoffTargetMode(ctx context.Context, config recoveryHostTarget, saved recoveryProvisionFile, expectedOID uint32, admitted bool) (*storage.Store, func(), error) {
	p := saved.Intent
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
	dsn, err := explicitRecoveryDSN(config.DatabaseURL)
	if err != nil {
		closeAdmin()
		return nil, nil, err
	}
	var s *storage.Store
	if admitted {
		s, err = storage.OpenAdmittedRecoveryProvisionTarget(ctx, admin, dsn, p, expectedOID)
	} else {
		ob, e := storage.InspectRecoveryProvision(ctx, admin, p)
		if e != nil || ob.State != "database_restricted" || (expectedOID != 0 && expectedOID != ob.DatabaseOID) {
			closeAdmin()
			return nil, nil, storage.ErrRecoveryProvision
		}
		s, err = storage.OpenRecoveryProvisionTarget(ctx, admin, dsn, p, ob.DatabaseOID)
	}
	if err != nil {
		closeAdmin()
		return nil, nil, err
	}
	return s, func() { s.Close(); closeAdmin() }, nil
}
