// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/storage"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"time"
)

func runRecoveryEvidenceHost(ctx context.Context, args []string, out io.Writer) error {
	usage := errors.New("usage: platform recovery evidence-ingest|evidence-inspect|reconciliation-plan --source-intent-file PRIVATE_PLAN --target-config PRIVATE_TARGET [--confirm-ingest-evidence]")
	if len(args) == 0 || (args[0] != "evidence-ingest" && args[0] != "evidence-inspect" && args[0] != "reconciliation-plan") {
		return usage
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var sourceFile, targetFile string
	var confirm bool
	f.StringVar(&sourceFile, "source-intent-file", "", "private bound source plan and capture evidence")
	f.StringVar(&targetFile, "target-config", "", "explicit exact held target")
	if args[0] == "evidence-ingest" {
		f.BoolVar(&confirm, "confirm-ingest-evidence", false, "store evidence without enabling execution or settling charges")
	}
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || sourceFile == "" || targetFile == "" || (args[0] == "evidence-ingest" && !confirm) {
		return usage
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	var saved recoveryProcessFile
	var target recoveryHostTarget
	if readRecoveryFenceJSON(sourceFile, &saved) != nil || saved.Capture == nil || saved.Binding == nil || readPrivateJSON(targetFile, &target) != nil {
		return errors.New("explicit private source and target selectors required")
	}
	for _, directory := range []string{saved.Capture.SelectedDirectory, saved.Capture.Directory} {
		for _, path := range []string{sourceFile, sourceFile + ".capture", sourceFile + ".capture-state", targetFile, saved.Capture.Keys, saved.Capture.Stage} {
			if recoveryarchive.RequireHostPathOutside(directory, path) != nil {
				return errors.New("recovery authority must remain outside the archives")
			}
		}
	}
	verified, err := loadSourceCaptureLink(saved, sourceFile)
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
	if args[0] == "evidence-inspect" {
		report, e := s.InspectRecoveryEvidence(ctx, target.spec(), verified)
		if e != nil {
			return e
		}
		return json.NewEncoder(out).Encode(report)
	}
	selected, err := recoveryarchive.VerifyHostBackup(ctx, saved.Capture.SelectedDirectory, saved.Capture.Keys, l.SelectedReceiptSHA256, saved.Capture.Stage)
	if err != nil {
		return err
	}
	defer selected.Close()
	late, err := recoveryarchive.VerifyHostBackup(ctx, saved.Capture.Directory, saved.Capture.Keys, l.EvidenceReceiptSHA256, saved.Capture.Stage)
	if err != nil {
		return err
	}
	defer late.Close()
	if args[0] == "reconciliation-plan" {
		plan, err := s.PlanRecoveryReconciliation(ctx, selected, late, verified, target.spec(), storage.DefaultRecoverySnapshotLimits())
		if err != nil {
			return err
		}
		if selected.Close() != nil || late.Close() != nil {
			return errors.New("reconciliation archive cleanup not confirmed")
		}
		return json.NewEncoder(out).Encode(plan)
	}
	report, err := s.IngestRecoveryEvidence(ctx, selected, late, verified, target.spec(), storage.DefaultRecoverySnapshotLimits())
	if err != nil {
		return err
	}
	if selected.Close() != nil || late.Close() != nil {
		return errors.New("evidence returned but archive cleanup not confirmed; inspect exact target")
	}
	return json.NewEncoder(out).Encode(report)
}
