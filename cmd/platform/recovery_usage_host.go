// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"path/filepath"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryaccess"
	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/storage"
)

type recoveryUsageFile struct {
	Version          int                               `json:"version"`
	OperationID      string                            `json:"operation_id"`
	SourceIntentFile string                            `json:"source_intent_file"`
	TargetConfig     string                            `json:"target_config"`
	Selection        storage.RecoveryUsageSelection    `json:"selection"`
	Plan             storage.RecoveryUsageReadbackPlan `json:"plan"`
}

type recoveryUsageAttempt struct {
	Version     int       `json:"version"`
	OperationID string    `json:"operation_id"`
	PlanSHA256  string    `json:"plan_sha256"`
	StartedAt   time.Time `json:"started_at"`
}

// This file is evidence for inspection only. No command reconstructs an opaque
// acquisition from it or re-admits its bytes to the database.
type recoveryUsageAcquisitionFile struct {
	Version     int                                    `json:"version"`
	OperationID string                                 `json:"operation_id"`
	PlanSHA256  string                                 `json:"plan_sha256"`
	Receipt     recoveryaccess.UsageAcquisitionReceipt `json:"receipt"`
	Response    []byte                                 `json:"response"`
}

func recoveryUsagePlanDigest(p storage.RecoveryUsageReadbackPlan) string {
	p.PlanSHA256 = ""
	raw, _ := json.Marshal(p)
	return storage.Hash(raw)
}

func recoveryUsageSavedValid(s recoveryUsageFile) bool {
	return s.Version == 1 && len(s.OperationID) == 32 && strings.Trim(s.OperationID, "0123456789abcdef") == "" &&
		filepath.IsAbs(s.SourceIntentFile) && filepath.IsAbs(s.TargetConfig) && s.Selection == s.Plan.Selection &&
		s.Plan.Version == 1 && s.Plan.Algorithm == "archived-model-usage-binding-v1" && s.Plan.ExecutionHeld && s.Plan.RollbackConfirmed &&
		s.Plan.PlanSHA256 == recoveryUsagePlanDigest(s.Plan)
}

func recoveryUsageRequest(p storage.RecoveryUsageReadbackPlan) recoveryaccess.UsageRequest {
	return recoveryaccess.UsageRequest{Binding: recoveryaccess.Binding{TargetID: p.TargetID, RestoreID: p.RestoreID, SnapshotSHA256: p.SelectedSnapshotSHA256},
		SourcePlanSHA256: p.PlanSHA256, SourceID: p.SourceID, OrganizationID: p.Selection.OrganizationID,
		ModelConnectionID: p.ModelConnectionID, ModelConnectionVersion: p.ModelConnectionVersion,
		ConnectionIdentitySHA256: p.ConnectionIdentitySHA, ArchivedProviderAccount: p.ArchivedProviderAccount,
		GenerationID: p.GenerationID, Model: p.Model}
}

func recoveryUsageAttemptValid(a recoveryUsageAttempt, s recoveryUsageFile) bool {
	return a.Version == 1 && a.OperationID == s.OperationID && a.PlanSHA256 == s.Plan.PlanSHA256 && !a.StartedAt.IsZero()
}

func recoveryUsageAcquisitionValid(a recoveryUsageAcquisitionFile, s recoveryUsageFile) bool {
	return a.Version == 1 && a.OperationID == s.OperationID && a.PlanSHA256 == s.Plan.PlanSHA256 &&
		a.Receipt.Request == recoveryUsageRequest(s.Plan) && recoveryaccess.UsageReceiptConsistent(a.Receipt, a.Response)
}

// A session always authenticates both archives and the capture link. Saved JSON
// is a selector, never a replacement for archive or database verification.
type recoveryUsageSession struct {
	store          *storage.Store
	closeTarget    func()
	target         recoveryHostTarget
	selected, late *recoveryarchive.VerifiedArchive
	link           *recoveryarchive.VerifiedCaptureLink
}

func (s *recoveryUsageSession) close() error {
	var a, b error
	if s.selected != nil {
		a = s.selected.Close()
		s.selected = nil
	}
	if s.late != nil {
		b = s.late.Close()
		s.late = nil
	}
	if s.closeTarget != nil {
		s.closeTarget()
		s.closeTarget = nil
	}
	s.store = nil
	if a != nil || b != nil {
		return errors.New("usage archive cleanup not confirmed; inspect the saved operation")
	}
	return nil
}

func openRecoveryUsageSession(ctx context.Context, saved recoveryUsageFile, paths ...string) (_ *recoveryUsageSession, err error) {
	var source recoveryProcessFile
	s := &recoveryUsageSession{}
	defer func() {
		if err != nil {
			_ = s.close()
		}
	}()
	if readRecoveryFenceJSON(saved.SourceIntentFile, &source) != nil || source.Capture == nil || source.Binding == nil || readPrivateJSON(saved.TargetConfig, &s.target) != nil {
		return nil, errors.New("explicit private source and target selectors required")
	}
	if s.target.HandoffPlanFile != "" || s.target.ProvenanceSHA256 != "" {
		if err = s.openCold(ctx, saved, paths); err != nil {
			return nil, err
		}
		return s, nil
	}
	paths = append(paths, saved.SourceIntentFile, saved.SourceIntentFile+".capture", saved.SourceIntentFile+".capture-state", saved.TargetConfig, source.Capture.Keys, source.Capture.Stage)
	for _, directory := range []string{source.Capture.SelectedDirectory, source.Capture.Directory} {
		for _, path := range paths {
			if path != "" && recoveryarchive.RequireHostPathOutside(directory, path) != nil {
				return nil, errors.New("usage authority and operation files must remain outside the archives")
			}
		}
	}
	s.link, err = loadSourceCaptureLink(source, saved.SourceIntentFile)
	if err != nil {
		return nil, err
	}
	l := s.link.Link()
	if s.target.SourceID != l.SourceID || s.target.CutUTC != l.SelectedCutUTC || s.target.SnapshotSHA256 != l.SelectedSnapshotSHA256 || s.target.ReceiptSHA256 != l.SelectedReceiptSHA256 {
		return nil, errors.New("target does not select this usage evidence pair")
	}
	if saved.Plan.PlanSHA256 != "" && (saved.Plan.ColdActivationID != "" || saved.Plan.TargetID != s.target.TargetInstallationID || saved.Plan.RestoreID != s.target.RestoreID || saved.Plan.SourceID != l.SourceID ||
		saved.Plan.SelectedSnapshotSHA256 != l.SelectedSnapshotSHA256 || saved.Plan.CapturedSnapshotSHA256 != l.EvidenceSnapshotSHA256 ||
		saved.Plan.SelectedReceiptSHA256 != l.SelectedReceiptSHA256 || saved.Plan.CapturedReceiptSHA256 != l.EvidenceReceiptSHA256 || saved.Plan.CaptureLinkSHA256 != s.link.SHA256()) {
		return nil, errors.New("saved usage plan does not select this target and capture")
	}
	dsn, err := explicitRecoveryDSN(s.target.DatabaseURL)
	if err != nil {
		return nil, err
	}
	s.store, err = storage.Open(ctx, dsn)
	if err != nil {
		return nil, errors.New("explicit recovery target unavailable")
	}
	s.closeTarget = s.store.Close
	s.selected, err = recoveryarchive.VerifyHostBackup(ctx, source.Capture.SelectedDirectory, source.Capture.Keys, l.SelectedReceiptSHA256, source.Capture.Stage)
	if err != nil {
		return nil, err
	}
	s.late, err = recoveryarchive.VerifyHostBackup(ctx, source.Capture.Directory, source.Capture.Keys, l.EvidenceReceiptSHA256, source.Capture.Stage)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// A retired handoff already imported the captured archive. Its retained stop
// and physical target bind the readback, with no connection to the old source.
func (s *recoveryUsageSession) openCold(ctx context.Context, saved recoveryUsageFile, paths []string) error {
	provision, err := loadProvisionedHandoffTarget(s.target)
	if err != nil || provision.Handoff.Retirement == nil || s.target.DatabaseOID == 0 || saved.SourceIntentFile != provision.Handoff.SourceIntentFile {
		return storage.ErrRecoveryUsageBinding
	}
	h := provision.Handoff
	// Planning includes an optional authority path, which can be empty.
	var required []string
	for _, path := range append(paths, saved.TargetConfig) {
		if path != "" {
			required = append(required, path)
		}
	}
	source, err := loadRecoveryHandoffSource(h.SourceIntentFile, h.SourceStopFile, h.BaselineDirectory, h.KeysFile, h.StagingParent,
		provision.ReceiptSHA256, s.target.HandoffPlanFile, required...)
	if err != nil || captureHash(source.provenance) != provision.Intent.ProvenanceSHA256 {
		return storage.ErrRecoveryUsageBinding
	}
	s.link, err = source.link.WithRetiredSourceStop(source.stop)
	if err != nil {
		return err
	}
	l, p := s.link.Link(), saved.Plan
	if p.PlanSHA256 != "" && (p.ColdActivationID == "" || p.TargetID != s.target.TargetInstallationID || p.RestoreID != s.target.RestoreID || p.SourceID != l.SourceID ||
		p.SelectedSnapshotSHA256 != l.EvidenceSnapshotSHA256 || p.CapturedSnapshotSHA256 != l.EvidenceSnapshotSHA256 ||
		p.SelectedReceiptSHA256 != l.EvidenceReceiptSHA256 || p.CapturedReceiptSHA256 != l.EvidenceReceiptSHA256 ||
		p.OriginalSnapshotSHA256 != l.SelectedSnapshotSHA256 || p.OriginalReceiptSHA256 != l.SelectedReceiptSHA256 || p.CaptureLinkSHA256 != s.link.SHA256()) {
		return storage.ErrRecoveryUsageBinding
	}
	s.store, s.closeTarget, err = openBoundRecoveryHandoffTargetMode(ctx, s.target, provision, s.target.DatabaseOID, true)
	if err != nil {
		return err
	}
	if s.selected, err = source.openOriginal(ctx); err != nil {
		return err
	}
	s.late, err = source.openBaseline(ctx)
	return err
}

func runRecoveryUsageHost(ctx context.Context, args []string, out io.Writer) error {
	usage := errors.New("usage: platform recovery usage-plan --source-intent-file PRIVATE_SOURCE --target-config PRIVATE_TARGET --organization-id ID --reservation-id ID --out PRIVATE_PLAN; usage-acquire --intent-file PRIVATE_PLAN --authority-dir PRIVATE_AUTHORITY --confirm-read-provider-usage; usage-inspect --intent-file PRIVATE_PLAN")
	if len(args) == 0 {
		return usage
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var saved recoveryUsageFile
	var path, authority string
	var confirmed bool
	switch args[0] {
	case "usage-plan":
		f.StringVar(&saved.SourceIntentFile, "source-intent-file", "", "private source and capture plan")
		f.StringVar(&saved.TargetConfig, "target-config", "", "explicit held target")
		f.StringVar(&saved.Selection.OrganizationID, "organization-id", "", "original organization")
		f.StringVar(&saved.Selection.ReservationID, "reservation-id", "", "original model reservation")
		f.StringVar(&path, "out", "", "new private usage operation plan")
	case "usage-acquire", "usage-inspect":
		f.StringVar(&path, "intent-file", "", "saved private usage plan")
		if args[0] == "usage-acquire" {
			f.StringVar(&authority, "authority-dir", "", "current private usage authority")
			f.BoolVar(&confirmed, "confirm-read-provider-usage", false, "one provider usage read and unverified observation storage; no model execution or settlement")
		}
	default:
		return usage
	}
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || path == "" || (args[0] == "usage-acquire" && (!confirmed || authority == "")) {
		return usage
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	var err error
	path, err = filepath.Abs(path)
	if err != nil {
		return usage
	}
	if args[0] == "usage-plan" {
		if saved.SourceIntentFile == "" || saved.TargetConfig == "" || saved.Selection.OrganizationID == "" || saved.Selection.ReservationID == "" {
			return usage
		}
		saved.SourceIntentFile, err = filepath.Abs(saved.SourceIntentFile)
		if err != nil {
			return usage
		}
		saved.TargetConfig, err = filepath.Abs(saved.TargetConfig)
		if err != nil {
			return usage
		}
		saved.Version, saved.OperationID = 1, strings.TrimPrefix(domain.NewID("usage"), "usage_")
	} else if readRecoveryFenceJSON(path, &saved) != nil || !recoveryUsageSavedValid(saved) {
		return errors.New("invalid private usage operation plan")
	}
	if authority != "" {
		authority, err = filepath.Abs(authority)
		if err != nil || recoveryarchive.RequireHostPathOutside(authority, path) != nil {
			return usage
		}
	}
	s, err := openRecoveryUsageSession(ctx, saved, path, path+".attempt", path+".acquisition", authority)
	if err != nil {
		return err
	}
	defer s.close()
	limits := storage.DefaultRecoverySnapshotLimits()
	var report any
	switch args[0] {
	case "usage-plan":
		saved.Plan, err = s.store.PlanRecoveryUsageReadback(ctx, s.selected, s.late, s.link, s.target.spec(), limits, saved.Selection)
		if err != nil {
			return err
		}
		if !recoveryUsageSavedValid(saved) {
			return errors.New("usage plan could not be sealed")
		}
		if err = recoveryarchive.WriteHostJSON(path, saved); err != nil {
			return err
		}
		report = saved.Plan
	case "usage-acquire":
		if saved.Plan.State != "request_identity_available" {
			return errors.New("verified history has no supported usage request identity")
		}
		// O_EXCL + file/parent sync precedes all HTTP. A partial marker is also
		// a refusal: neither command retries it or removes it automatically.
		attempt := recoveryUsageAttempt{1, saved.OperationID, saved.Plan.PlanSHA256, time.Now().UTC()}
		if err = recoveryarchive.WriteHostJSON(path+".attempt", attempt); err != nil {
			return errors.New("usage attempt already exists or is not durable; inspect before preparing a new plan")
		}
		observation, e := s.store.AcquirePlannedRecoveryUsage(ctx, s.selected, s.late, s.link, s.target.spec(), limits, saved.Selection, authority, saved.Plan.PlanSHA256)
		if e != nil {
			return e
		}
		acquired := recoveryUsageAcquisitionFile{1, saved.OperationID, saved.Plan.PlanSHA256, observation.Receipt(), observation.RawResponse()}
		if !recoveryUsageAcquisitionValid(acquired, saved) {
			return errors.New("acquisition does not match the reviewed usage plan")
		}
		// Keep inspection identity before the database write, including when
		// COMMIT acknowledgment or stdout is lost. Never read it back to admit.
		if err = recoveryarchive.WriteHostJSON(path+".acquisition", acquired); err != nil {
			return errors.New("usage response could not be retained; no database admission attempted; do not repeat automatically")
		}
		report, err = s.store.RecordRecoveryUsage(ctx, s.selected, s.late, s.link, s.target.spec(), limits, saved.Selection, authority, saved.OperationID, observation)
		if err != nil {
			return err
		}
	case "usage-inspect":
		var attempt recoveryUsageAttempt
		var acquired recoveryUsageAcquisitionFile
		if readRecoveryFenceJSON(path+".attempt", &attempt) != nil || !recoveryUsageAttemptValid(attempt, saved) ||
			readRecoveryFenceJSON(path+".acquisition", &acquired) != nil || !recoveryUsageAcquisitionValid(acquired, saved) {
			return errors.New("usage attempt or response identity unavailable; outcome remains unknown; do not repeat automatically")
		}
		inspected, e := s.store.InspectRecoveryUsage(ctx, s.selected, s.late, s.link, s.target.spec(), limits, saved.Selection, saved.OperationID, acquired.Receipt.ReceiptSHA256)
		if e != nil {
			return e
		}
		if inspected.State == "recorded" && (inspected.SourcePlanSHA256 != saved.Plan.PlanSHA256 || inspected.ResponseSHA256 != acquired.Receipt.Observation.ResponseSHA256) {
			return errors.New("stored usage does not match the saved operation")
		}
		report = inspected
	}
	if err = s.close(); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(report)
}
