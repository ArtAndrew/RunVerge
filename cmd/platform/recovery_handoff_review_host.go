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
)

type recoveryHandoffReview struct {
	Version                    int                                 `json:"version"`
	Profile                    string                              `json:"profile"`
	State                      string                              `json:"state"`
	SourceStopSHA256           string                              `json:"source_stop_sha256"`
	CaptureLinkSHA256          string                              `json:"capture_link_sha256"`
	OriginalReceiptSHA256      string                              `json:"original_receipt_sha256"`
	BaselineReceiptSHA256      string                              `json:"baseline_receipt_sha256"`
	OriginalSnapshotSHA256     string                              `json:"original_snapshot_sha256"`
	BaselineSnapshotSHA256     string                              `json:"baseline_snapshot_sha256"`
	LastSourceCutSHA256        string                              `json:"last_source_cut_proof_sha256,omitempty"`
	SourceRetirementSealSHA256 string                              `json:"source_retirement_seal_sha256,omitempty"`
	PeerScope                  *recoveryarchive.RecoveryPeerScope  `json:"peer_scope,omitempty"`
	PeerMinimumThroughUTC      string                              `json:"peer_minimum_through_utc,omitempty"`
	OriginalCutUTC             string                              `json:"original_cut_utc"`
	BaselineCutUTC             string                              `json:"baseline_cut_utc"`
	TargetReview               storage.RecoveryHandoffTargetReview `json:"target_review"`
	ExecutionHeld              bool                                `json:"execution_held"`
	Blockers                   []string                            `json:"blockers"`
}

// Compact, create-only commitment to the evidence and the exact held review.
// Future admission must still reverify the signed journal and current target.
type recoveryHandoffPeerReceipt struct {
	Version                int                               `json:"version"`
	Scope                  recoveryarchive.RecoveryPeerScope `json:"scope"`
	JournalSHA256          string                            `json:"journal_sha256"`
	SignerSHA256           string                            `json:"signer_sha256"`
	PeerReviewSHA256       string                            `json:"peer_review_sha256"`
	HandoffReviewSHA256    string                            `json:"handoff_review_sha256"`
	TargetReviewSHA256     string                            `json:"target_review_sha256,omitempty"`
	TargetProvenanceSHA256 string                            `json:"target_provenance_sha256"`
	ImportReceiptSHA256    string                            `json:"import_receipt_sha256"`
	AccountingHeadID       string                            `json:"accounting_head_id"`
	AccountingHeadOrdinal  int64                             `json:"accounting_head_ordinal,string"`
	CoverageProven         bool                              `json:"coverage_proven"`
	UnmatchedRecords       int                               `json:"unmatched_records"`
	ExecutionHeld          bool                              `json:"execution_held"`
}

func runRecoveryHandoffReviewHost(ctx context.Context, args []string, out io.Writer) error {
	usage := errors.New("usage: platform recovery handoff-review --target-config PRIVATE_HANDOFF_TARGET [--peer-journal PRIVATE_SIGNED_JOURNAL --peer-trust PRIVATE_PINNED_KEY --peer-receipt-out NEW_PRIVATE_RECEIPT]")
	f := flag.NewFlagSet("handoff-review", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	targetFile := f.String("target-config", "", "private target intent from handoff provisioning")
	journalFile := f.String("peer-journal", "", "independently signed closed egress journal")
	trustFile := f.String("peer-trust", "", "independently pinned trust for this exact source and target")
	receiptFile := f.String("peer-receipt-out", "", "new private peer reconciliation receipt")
	if f.Parse(args) != nil || f.NArg() != 0 || *targetFile == "" {
		return usage
	}
	withPeer := *journalFile != ""
	if (*trustFile != "") != withPeer || (*receiptFile != "") != withPeer {
		return usage
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	var target recoveryHostTarget
	if readRecoveryFenceJSON(*targetFile, &target) != nil {
		return errors.New("private handoff target configuration required")
	}
	saved, err := loadProvisionedHandoffTarget(target)
	if err != nil {
		return err
	}
	h := saved.Handoff
	paths := []string{*targetFile}
	if withPeer {
		paths = append(paths, *journalFile, *trustFile, *receiptFile)
	}
	source, err := loadRecoveryHandoffSource(h.SourceIntentFile, h.SourceStopFile, h.BaselineDirectory, h.KeysFile, h.StagingParent, saved.ReceiptSHA256, target.HandoffPlanFile, paths...)
	if err != nil {
		return err
	}
	if captureHash(source.provenance) != saved.Intent.ProvenanceSHA256 {
		return errors.New("handoff provenance changed since provisioning plan")
	}
	var peer *recoveryarchive.VerifiedRecoveryPeerJournal
	if withPeer {
		peer, err = recoveryarchive.VerifyHostRecoveryPeerJournal(*journalFile, *trustFile, target.TargetInstallationID, source.stop)
		if err != nil || peer.Journal().Scope.RestoreID != target.RestoreID {
			return recoveryarchive.ErrRecoveryPeerJournal
		}
	}
	if _, err = source.observeStop(ctx); err != nil {
		return err
	}
	original, err := source.openOriginal(ctx)
	if err != nil {
		return err
	}
	defer original.Close()
	late, err := source.openBaseline(ctx)
	if err != nil {
		return err
	}
	defer late.Close()
	s, closeTarget, err := openBoundRecoveryHandoffTarget(ctx, target, saved)
	if err != nil {
		return err
	}
	defer closeTarget()
	review, err := s.ReviewRecoveryHandoffTarget(ctx, original, late, source.link, target.spec(), storage.DefaultRecoverySnapshotLimits(), peer)
	if err != nil {
		return err
	}
	if err = errors.Join(original.Close(), late.Close()); err != nil {
		return err
	}
	checked, err := source.observeStop(ctx)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	targetReviewSHA := review.SHA256()
	review.ExecutionWindow.SourceStopRechecked = true
	l := h.Capture
	result := recoveryHandoffReview{
		Version: 1, Profile: "drained-compose-held-review-v1", State: "committed_held",
		SourceStopSHA256: h.SourceStopSHA256, CaptureLinkSHA256: h.CaptureLinkSHA256,
		OriginalReceiptSHA256: l.SelectedReceiptSHA256, BaselineReceiptSHA256: l.EvidenceReceiptSHA256,
		OriginalSnapshotSHA256: l.SelectedSnapshotSHA256, BaselineSnapshotSHA256: l.EvidenceSnapshotSHA256,
		OriginalCutUTC: l.SelectedCutUTC, BaselineCutUTC: l.EvidenceCutUTC, TargetReview: review, ExecutionHeld: true,
		Blockers: []string{},
	}
	if h.Retirement != nil {
		result.LastSourceCutSHA256 = h.Retirement.CutProofSHA256
		result.SourceRetirementSealSHA256 = h.Retirement.SealSHA256
	}
	if result.SourceRetirementSealSHA256 == "" {
		result.Blockers = append(result.Blockers, "durable_source_executor_fence_required")
	} else {
		scope, e := recoveryarchive.RecoveryPeerScopeFor(target.TargetInstallationID, source.stop)
		if e != nil || scope.RestoreID != target.RestoreID {
			return recoveryarchive.ErrRecoveryPeerJournal
		}
		result.PeerScope = &scope
		result.PeerMinimumThroughUTC = source.stop.Receipt().Source.Observation.ObservedAt.UTC().Format(time.RFC3339Nano)
	}
	if !review.ExecutionWindow.CoverageProven {
		result.Blockers = append(result.Blockers, "external_effect_coverage_unproven")
	}
	result.Blockers = append(result.Blockers, "current_execution_authority_required")
	stable, err := json.Marshal(result)
	if err != nil || len(stable) > (8<<20)-1024 {
		return errors.New("handoff review exceeds output limits")
	}
	reviewSHA := storage.Hash(stable)
	if w := review.PeerWindow; w != nil {
		receipt := recoveryHandoffPeerReceipt{
			Version: 1, Scope: w.Scope, JournalSHA256: w.JournalSHA256, SignerSHA256: w.SignerSHA256,
			PeerReviewSHA256: captureHash(w), HandoffReviewSHA256: reviewSHA, TargetReviewSHA256: targetReviewSHA, TargetProvenanceSHA256: review.ProvenanceSHA256,
			ImportReceiptSHA256: review.ImportReceiptSHA256, AccountingHeadID: review.AccountingHeadID, AccountingHeadOrdinal: review.AccountingHeadOrdinal,
			CoverageProven: w.CoverageProven, UnmatchedRecords: len(w.Unmatched), ExecutionHeld: true,
		}
		if err = recoveryarchive.WriteHostJSONOutside(h.BaselineDirectory, *receiptFile, receipt); err != nil {
			return err
		}
	}
	return json.NewEncoder(out).Encode(struct {
		recoveryHandoffReview
		ReviewSHA256          string    `json:"review_sha256"`
		SourceStopRecheckedAt time.Time `json:"source_stop_rechecked_at"`
	}{result, reviewSHA, checked})
}
