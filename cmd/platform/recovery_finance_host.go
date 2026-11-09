// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"path/filepath"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/storage"
)

func runRecoveryFinanceHost(ctx context.Context, args []string, out io.Writer) error {
	usage := errors.New("usage: platform recovery usage-invoice-post|usage-invoice-inspect|usage-invoice-resolve|usage-invoice-resolution-inspect --intent-file PRIVATE_USAGE_PLAN --statement-file PRIVATE_SIGNED_STATEMENT --finance-trust PRIVATE_PINNED_KEY --statement-document PRIVATE_FINAL_BILL [--confirm-final-usd-statement] [--quarantine-id ID --expected-revision N --confirm-quarantine-resolution]")
	post := args[0] == "usage-invoice-post"
	resolve := args[0] == "usage-invoice-resolve"
	resolution := resolve || args[0] == "usage-invoice-resolution-inspect"
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	f.SetOutput(io.Discard)
	planPath := f.String("intent-file", "", "saved cold usage plan")
	statementPath := f.String("statement-file", "", "final statement signed by the finance reviewer")
	trustPath := f.String("finance-trust", "", "privately pinned finance key")
	documentPath := f.String("statement-document", "", "private provider bill checked by the reviewer")
	confirm := f.Bool("confirm-final-usd-statement", false, "post a verified final statement in USD")
	quarantine := f.String("quarantine-id", "", "exact quarantine of this signed statement")
	revision := f.Int("expected-revision", 0, "revision recorded when the statement was quarantined")
	confirmResolution := f.Bool("confirm-quarantine-resolution", false, "post the exact previously quarantined statement")
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || post != *confirm || resolve != *confirmResolution ||
		(resolution && (*quarantine == "" || *revision < 1)) || (!resolution && (*quarantine != "" || *revision != 0)) {
		return usage
	}
	for _, path := range []string{*planPath, *statementPath, *trustPath, *documentPath} {
		if !filepath.IsAbs(path) {
			return usage
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	var saved recoveryUsageFile
	if readRecoveryFenceJSON(*planPath, &saved) != nil || !recoveryUsageSavedValid(saved) || saved.Plan.ColdActivationID == "" {
		return storage.ErrRecoveryUsageBinding
	}
	statement, err := recoveryarchive.VerifyHostFinanceStatement(*statementPath, *trustPath, *documentPath)
	if err != nil {
		return err
	}
	if statement.Statement().ObservationID != saved.OperationID {
		return storage.ErrRecoveryUsageBinding
	}
	s, err := openRecoveryUsageSession(ctx, saved, *planPath, *statementPath, *trustPath, *documentPath)
	if err != nil {
		return err
	}
	defer s.close()
	var result storage.RecoveryFinanceResult
	if resolution {
		request := storage.RecoveryFinanceResolution{QuarantineID: *quarantine, ExpectedRevision: *revision}
		result, err = s.store.ResolveRecoveryFinanceStatement(ctx, s.selected, s.late, s.link, s.target.spec(),
			storage.DefaultRecoverySnapshotLimits(), saved.Selection, statement, request, resolve)
	} else {
		result, err = s.store.SettleRecoveryFinanceStatement(ctx, s.selected, s.late, s.link, s.target.spec(),
			storage.DefaultRecoverySnapshotLimits(), saved.Selection, statement, post)
	}
	if err != nil {
		return err
	}
	if err = s.close(); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
