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
	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/storage"
)

type recoveryAccountingFile struct {
	Version          int                                 `json:"version"`
	SourceIntentFile string                              `json:"source_intent_file"`
	TargetConfig     string                              `json:"target_config"`
	Selection        storage.RecoveryAccountingSelection `json:"selection"`
	Plan             storage.RecoveryAccountingPlan      `json:"plan"`
}

func runRecoveryAccountingHost(ctx context.Context, args []string, out io.Writer) error {
	usage := errors.New("usage: platform recovery accounting-plan --source-intent-file PRIVATE_SOURCE --target-config PRIVATE_TARGET --organization-id ID --reservation-id ID --source-evidence-id ID --out PRIVATE_PLAN; accounting-apply|accounting-inspect --intent-file PRIVATE_PLAN [--confirm-record-financial-fact]")
	if len(args) == 0 {
		return usage
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var saved recoveryAccountingFile
	var path string
	var confirm bool
	switch args[0] {
	case "accounting-plan":
		f.StringVar(&saved.SourceIntentFile, "source-intent-file", "", "private bound source and captured evidence")
		f.StringVar(&saved.TargetConfig, "target-config", "", "explicit held target")
		f.StringVar(&saved.Selection.OrganizationID, "organization-id", "", "original organization")
		f.StringVar(&saved.Selection.ReservationID, "reservation-id", "", "original reservation")
		f.StringVar(&saved.Selection.SourceEvidenceID, "source-evidence-id", "", "exact late source cost_evidence ID")
		f.StringVar(&path, "out", "", "new private operation plan")
	case "accounting-apply", "accounting-inspect":
		f.StringVar(&path, "intent-file", "", "saved private operation plan")
		if args[0] == "accounting-apply" {
			f.BoolVar(&confirm, "confirm-record-financial-fact", false, "record the selected financial fact while execution stays held")
		}
	default:
		return usage
	}
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || path == "" || (args[0] == "accounting-apply" && !confirm) {
		return usage
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	var err error
	path, err = filepath.Abs(path)
	if err != nil {
		return usage
	}
	if args[0] == "accounting-plan" {
		if saved.SourceIntentFile == "" || saved.TargetConfig == "" || saved.Selection.OrganizationID == "" || saved.Selection.ReservationID == "" || saved.Selection.SourceEvidenceID == "" {
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
		saved.Version = 1
		saved.Selection.OperationID = strings.TrimPrefix(domain.NewID("accounting"), "accounting_")
	} else if readRecoveryFenceJSON(path, &saved) != nil || saved.Version != 1 || !filepath.IsAbs(saved.SourceIntentFile) || !filepath.IsAbs(saved.TargetConfig) || saved.Selection.OperationID != saved.Plan.RequestedOperationID || saved.Selection.OrganizationID != saved.Plan.OrganizationID || saved.Selection.ReservationID != saved.Plan.ReservationID || saved.Selection.SourceEvidenceID != saved.Plan.SourceEvidenceID {
		return errors.New("invalid private financial operation plan")
	}
	var source recoveryProcessFile
	var target recoveryHostTarget
	if readRecoveryFenceJSON(saved.SourceIntentFile, &source) != nil || source.Capture == nil || source.Binding == nil || readPrivateJSON(saved.TargetConfig, &target) != nil {
		return errors.New("explicit private source and target selectors required")
	}
	for _, directory := range []string{source.Capture.SelectedDirectory, source.Capture.Directory} {
		for _, selector := range []string{path, path + ".apply", saved.SourceIntentFile, saved.SourceIntentFile + ".capture", saved.SourceIntentFile + ".capture-state", saved.TargetConfig, source.Capture.Keys, source.Capture.Stage} {
			if recoveryarchive.RequireHostPathOutside(directory, selector) != nil {
				return errors.New("financial operation authority must remain outside the archives")
			}
		}
	}
	verified, err := loadSourceCaptureLink(source, saved.SourceIntentFile)
	if err != nil {
		return err
	}
	l := verified.Link()
	if target.SourceID != l.SourceID || target.CutUTC != l.SelectedCutUTC || target.SnapshotSHA256 != l.SelectedSnapshotSHA256 || target.ReceiptSHA256 != l.SelectedReceiptSHA256 {
		return errors.New("target does not select this evidence pair")
	}
	dsn, err := explicitRecoveryDSN(target.DatabaseURL)
	if err != nil {
		return err
	}
	s, err := storage.Open(ctx, dsn)
	if err != nil {
		return errors.New("explicit recovery target unavailable")
	}
	defer s.Close()
	selected, err := recoveryarchive.VerifyHostBackup(ctx, source.Capture.SelectedDirectory, source.Capture.Keys, l.SelectedReceiptSHA256, source.Capture.Stage)
	if err != nil {
		return err
	}
	defer selected.Close()
	late, err := recoveryarchive.VerifyHostBackup(ctx, source.Capture.Directory, source.Capture.Keys, l.EvidenceReceiptSHA256, source.Capture.Stage)
	if err != nil {
		return err
	}
	defer late.Close()
	var report any
	switch args[0] {
	case "accounting-plan":
		saved.Plan, err = s.PlanRecoveryAccounting(ctx, selected, late, verified, target.spec(), storage.DefaultRecoverySnapshotLimits(), saved.Selection)
		if err != nil {
			return err
		}
		if err = recoveryarchive.WriteHostJSON(path, saved); err != nil {
			return err
		}
		report = saved.Plan
	case "accounting-inspect":
		var inspection storage.RecoveryAccountingInspection
		inspection, err = s.InspectRecoveryAccounting(ctx, selected, verified, target.spec(), storage.DefaultRecoverySnapshotLimits(), saved.Plan.OperationID, saved.Plan.RequestSHA256)
		if err != nil {
			return err
		}
		if inspection.State == "recorded" && inspection.PlanSHA256 != saved.Plan.PlanSHA256 {
			return errors.New("recorded operation does not match the saved financial plan")
		}
		if inspection.State == "recorded" {
			// Saved display fields and hashes are editable. Reconstruct the
			// source mapping so an operation for B cannot be reported for A.
			bound, e := s.PlanRecoveryAccounting(ctx, selected, late, verified, target.spec(), storage.DefaultRecoverySnapshotLimits(), saved.Selection)
			if e != nil || bound.State != "already_recorded" || bound.OperationID != inspection.OperationID || bound.RequestSHA256 != inspection.RequestSHA256 || bound.PlanSHA256 != inspection.PlanSHA256 {
				return errors.New("recorded operation does not match the selected source fact")
			}
		}
		report = inspection
	case "accounting-apply":
		// A one-use durable marker comes before the database call. Even an
		// uncertain commit or failed output cannot silently become another apply.
		if err = recoveryarchive.WriteHostJSON(path+".apply", struct {
			OperationID string    `json:"operation_id"`
			PlanSHA256  string    `json:"plan_sha256"`
			StartedAt   time.Time `json:"started_at"`
		}{saved.Selection.OperationID, saved.Plan.PlanSHA256, time.Now().UTC()}); err != nil {
			return errors.New("financial attempt already recorded or not durable; inspect before preparing a new plan")
		}
		report, err = s.ApplyRecoveryAccounting(ctx, selected, late, verified, target.spec(), storage.DefaultRecoverySnapshotLimits(), saved.Selection, saved.Plan.PlanSHA256)
		if err != nil {
			return err
		}
	}
	if selected.Close() != nil || late.Close() != nil {
		return errors.New("archive cleanup not confirmed; inspect the exact financial operation")
	}
	return json.NewEncoder(out).Encode(report)
}
