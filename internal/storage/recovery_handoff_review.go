// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
)

var ErrRecoveryHandoffReview = errors.New("held handoff target or its history could not be verified")

type RecoveryHandoffTargetReview struct {
	Target                RecoveryTargetState          `json:"target"`
	DatabaseOID           uint32                       `json:"database_oid"`
	ProvenanceSHA256      string                       `json:"provenance_sha256"`
	ImportReceiptSHA256   string                       `json:"import_receipt_sha256"`
	AccountingHeadID      string                       `json:"accounting_head_id"`
	AccountingHeadOrdinal int64                        `json:"accounting_head_ordinal,string"`
	Organizations         []RecoveryHandoffAccount     `json:"organizations"`
	OpenReservations      []RecoveryHandoffReservation `json:"open_reservations"`
	ExecutionWindow       RecoveryExecutionWindow      `json:"execution_window"`
	PeerWindow            *RecoveryPeerWindowReview    `json:"peer_window,omitempty"`
}

type RecoveryHandoffAccount struct {
	OrganizationID string `json:"organization_id"`
	RecoveryFinancialAccount
}

type RecoveryHandoffReservation struct {
	OrganizationID string                    `json:"organization_id"`
	ReservationID  string                    `json:"reservation_id"`
	Position       RecoveryFinancialPosition `json:"position"`
}

// ReviewRecoveryHandoffTarget checks the late-baseline import, historical keys,
// sequence bounds and accounting state under the exclusive recovery gate. The
// host must separately authenticate and reobserve the source. The result is a
// snapshot of the held target, never permission to run an agent or send a tool.
func (s *Store) ReviewRecoveryHandoffTarget(ctx context.Context, selected, late *recoveryarchive.VerifiedArchive, link *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, peer *recoveryarchive.VerifiedRecoveryPeerJournal) (result RecoveryHandoffTargetReview, resultErr error) {
	if !limits.valid() {
		return result, ErrRecoveryHandoffReview
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	original, baseline, err := s.bindRecoveryHandoffReview(ctx, selected, late, link, spec, limits, peer)
	if err != nil {
		return result, err
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return result, ErrRecoveryHandoffReview
	}
	defer conn.Release()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return result, ErrRecoveryHandoffReview
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if tx.Rollback(cleanup) != nil {
			_ = conn.Conn().Close(cleanup)
			result, resultErr = RecoveryHandoffTargetReview{}, ErrRecoveryStageCleanup
		}
	}()
	return s.reviewRecoveryHandoffTargetTx(ctx, tx, selected, late, spec, limits, peer, original, baseline)
}

func (s *Store) bindRecoveryHandoffReview(ctx context.Context, selected, late *recoveryarchive.VerifiedArchive, link *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, peer *recoveryarchive.VerifiedRecoveryPeerJournal) (RecoverySnapshotReport, RecoverySnapshotReport, error) {
	var empty RecoverySnapshotReport
	if s.recoveryProvision == nil || selected == nil || late == nil || link == nil || link.Link().Version != 2 || !limits.valid() {
		return empty, empty, ErrRecoveryHandoffReview
	}
	l := link.Link()
	if s.recoveryProvision.intent.ProvenanceSHA256 == "" || spec.SourceID != l.SourceID || spec.BackupID != l.EvidenceBackupID || spec.CutUTC != l.EvidenceCutUTC || spec.SchemaSHA256 != l.EvidenceSchemaSHA256 {
		return empty, empty, ErrRecoveryHandoffReview
	}
	if peer != nil {
		scope := peer.Journal().Scope
		if !validDigest(peer.SHA256()) || scope.TargetID != spec.TargetInstallationID || scope.SourceID != spec.SourceID || scope.RestoreID != spec.RestoreID || scope.CaptureLinkSHA256 != link.SHA256() || scope.OriginalCutUTC != l.SelectedCutUTC {
			return empty, empty, ErrRecoveryHandoffReview
		}
	}
	originSpec := spec
	metadata := selected.Manifest().Metadata
	originSpec.BackupID, originSpec.CutUTC, originSpec.SchemaSHA256 = metadata.BackupID, metadata.CutUTC, metadata.SchemaSHA256
	return bindRecoveryEvidence(ctx, selected, late, link, originSpec, limits)
}

// The exclusive gate acquired here remains with the caller. Admission can
// recompute the exact review and append its grant in this same transaction.
func (s *Store) reviewRecoveryHandoffTargetTx(ctx context.Context, tx pgx.Tx, selected, late *recoveryarchive.VerifiedArchive, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, peer *recoveryarchive.VerifiedRecoveryPeerJournal, original, baseline RecoverySnapshotReport) (result RecoveryHandoffTargetReview, resultErr error) {
	p := s.recoveryProvision
	held, catalog, err := s.evidenceTargetTx(ctx, tx, spec, baseline.SHA256)
	if err != nil {
		return result, err
	}
	if err = s.checkRecoveryProvisionImportTx(ctx, tx, spec, baseline.SHA256); err != nil {
		return result, err
	}
	var admitted bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+pgx.Identifier{s.runtimeSchema, "recovery_work_admissions"}.Sanitize()+")").Scan(&admitted); err != nil || admitted {
		return result, ErrRecoveryHandoffReview
	}
	receipt, err := readRecoveryImportReceipt(ctx, tx, s.runtimeSchema)
	if err != nil || receipt.InventorySHA256 != baseline.InventorySHA256 || !slices.Equal(receipt.Tables, baseline.Tables) || !slices.Equal(receipt.Sequences, baseline.Sequences) {
		return result, ErrRecoveryHandoffReview
	}
	inventory, err := recoverySnapshotInventory(ctx, tx, s.runtimeSchema, true)
	if err != nil {
		return result, err
	}
	base, err := recoverySourceInventory(inventory, baseline.InventorySHA256)
	if err != nil {
		return result, err
	}
	head, err := s.verifyRecoveryAccountingBaselineTx(ctx, tx, late, baseline, limits, held, catalog, base)
	if err != nil {
		return result, err
	}
	work := &recoveryReviewWork{}
	var windows [2]recoveryWindowSnapshot
	for i, snapshot := range []RecoverySnapshotReport{original, baseline} {
		archive := selected
		if i == 1 {
			archive = late
		}
		inv, e := recoverySourceInventory(inventory, snapshot.InventorySHA256)
		if e != nil {
			return result, e
		}
		stage, e := s.stageRecoveryTypedDataTx(ctx, tx, archive, snapshot, limits, held, catalog, inv)
		if e != nil {
			return result, e
		}
		windows[i], e = readRecoveryExecutionWindow(ctx, tx, stage.schema, work)
		if e != nil {
			return result, e
		}
		if i == 1 {
			if e = s.checkRecoveryHandoffHistoryTx(ctx, tx, stage, baseline, held.RestoreID); e != nil {
				return result, e
			}
		}
		if e = dropRecoveryAccountingStageTx(ctx, tx, stage, snapshot); e != nil {
			return result, e
		}
	}
	view, err := readRecoveryReviewSnapshot(ctx, tx, s.runtimeSchema, work)
	if err != nil {
		return result, err
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	result = RecoveryHandoffTargetReview{
		Target: held, DatabaseOID: p.databaseOID, ProvenanceSHA256: p.intent.ProvenanceSHA256, ImportReceiptSHA256: recoveryImportReceiptCommitment(receipt),
		AccountingHeadID: head.ID, AccountingHeadOrdinal: head.Ordinal, ExecutionWindow: compareRecoveryExecutionWindow(windows[0], windows[1]),
		Organizations: []RecoveryHandoffAccount{}, OpenReservations: []RecoveryHandoffReservation{},
	}
	for org, account := range view.organizations {
		result.Organizations = append(result.Organizations, RecoveryHandoffAccount{org, *account})
	}
	sort.Slice(result.Organizations, func(i, j int) bool {
		return result.Organizations[i].OrganizationID < result.Organizations[j].OrganizationID
	})
	for key, history := range view.histories {
		position := history.position
		if position.Status == "reserved" || position.Status == "unknown" || position.PendingQuarantineID != "" {
			result.OpenReservations = append(result.OpenReservations, RecoveryHandoffReservation{key.org, key.id, position})
		}
	}
	sort.Slice(result.OpenReservations, func(i, j int) bool {
		a, b := result.OpenReservations[i], result.OpenReservations[j]
		if a.OrganizationID != b.OrganizationID {
			return a.OrganizationID < b.OrganizationID
		}
		return a.ReservationID < b.ReservationID
	})
	if peer != nil {
		assessment := reviewRecoveryPeerWindow(peer, result.ExecutionWindow, view)
		result.PeerWindow = &assessment
		result.ExecutionWindow.CoverageProven = assessment.CoverageProven
	}
	return result, nil
}

// Counting historical markers is insufficient before admitting new authority:
// replacing a key with another key can leave the count unchanged.
func (s *Store) checkRecoveryHandoffHistoryTx(ctx context.Context, tx pgx.Tx, stage recoveryStagedData, baseline RecoverySnapshotReport, restoreID string) error {
	q := func(name string) string { return pgx.Identifier{s.runtimeSchema, name}.Sanitize() }
	for _, table := range stage.inventory.Tables {
		if !table.Exported {
			continue
		}
		var keys []string
		for _, key := range table.PrimaryKey {
			keys = append(keys, "source."+pgx.Identifier{key}.Sanitize())
		}
		var covered bool
		query := `SELECT NOT EXISTS(SELECT 1 FROM ` + pgx.Identifier{stage.schema, table.Name}.Sanitize() + ` source
			LEFT JOIN ` + q("recovery_historical_rows") + ` h ON h.table_name=$1 AND h.restore_id=$2 AND h.pk_sha256=` + q("recovery_row_key") + `($1,pg_catalog.jsonb_build_array(` + strings.Join(keys, ",") + `)) WHERE h.pk_sha256 IS NULL)`
		if tx.QueryRow(ctx, query, table.Name, restoreID).Scan(&covered) != nil || !covered {
			return ErrRecoveryHandoffReview
		}
	}
	for _, sequence := range baseline.Sequences {
		var last int64
		if tx.QueryRow(ctx, "SELECT last_value FROM "+q(sequence.Name)).Scan(&last) != nil || last < sequence.NextMinimum {
			return ErrRecoveryHandoffReview
		}
	}
	return nil
}
