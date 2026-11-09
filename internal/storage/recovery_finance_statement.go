// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
)

type RecoveryFinanceResult struct {
	State           string                    `json:"state"`
	OperationID     string                    `json:"operation_id"`
	StatementSHA256 string                    `json:"statement_sha256"`
	Outcome         *domain.AccountingOutcome `json:"outcome,omitempty"`
}

type RecoveryFinanceResolution struct {
	QuarantineID     string
	ExpectedRevision int
}

// SettleRecoveryFinanceStatement records a separately attested final USD line.
// The stored usage response identifies the operation; its reported amount is
// never used as a bill. A replay or inspection performs no additional posting.
func (s *Store) SettleRecoveryFinanceStatement(ctx context.Context, selected, captured *recoveryarchive.VerifiedArchive,
	link *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, selection RecoveryUsageSelection,
	statement recoveryarchive.VerifiedFinanceStatement, post bool) (RecoveryFinanceResult, error) {
	return s.recoveryFinanceStatement(ctx, selected, captured, link, spec, limits, selection, statement, nil, post)
}

func (s *Store) ResolveRecoveryFinanceStatement(ctx context.Context, selected, captured *recoveryarchive.VerifiedArchive,
	link *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, selection RecoveryUsageSelection,
	statement recoveryarchive.VerifiedFinanceStatement, resolution RecoveryFinanceResolution, apply bool) (RecoveryFinanceResult, error) {
	if !validText(resolution.QuarantineID, 128) || resolution.ExpectedRevision < 1 {
		return RecoveryFinanceResult{}, ErrRecoveryAccounting
	}
	return s.recoveryFinanceStatement(ctx, selected, captured, link, spec, limits, selection, statement, &resolution, apply)
}

func (s *Store) recoveryFinanceStatement(ctx context.Context, selected, captured *recoveryarchive.VerifiedArchive,
	link *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, selection RecoveryUsageSelection,
	statement recoveryarchive.VerifiedFinanceStatement, resolution *RecoveryFinanceResolution, post bool) (RecoveryFinanceResult, error) {
	var empty RecoveryFinanceResult
	st := statement.Statement()
	if !limits.valid() || !validDigest(statement.SHA256()) || !validDigest(statement.SignerSHA256()) ||
		st.Scope.TargetID != spec.TargetInstallationID || st.Scope.RestoreID != spec.RestoreID || st.Scope.OrganizationID != selection.OrganizationID {
		return empty, ErrRecoveryAccounting
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	evidence, err := s.bindRecoveryUsageEvidence(ctx, selected, captured, link, spec, limits)
	if err != nil || !evidence.cold {
		return empty, ErrRecoveryAccounting
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return empty, ErrRecoveryAccounting
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return empty, ErrRecoveryAccounting
	}
	defer recoveryFenceRollback(tx, conn.Conn())
	plan, err := s.planRecoveryUsageReadbackTx(ctx, tx, selected, captured, link, spec, limits, selection, evidence)
	if err != nil || plan.ColdActivationID != st.Scope.ActivationID || plan.ArchivedProviderAccount != st.Scope.Account ||
		statement.SignerSHA256() == link.RetirementSignerSHA256() {
		return empty, ErrRecoveryAccounting
	}
	q := func(n string) string { return pgx.Identifier{s.runtimeSchema, n}.Sanitize() }
	var peer string
	if tx.QueryRow(ctx, "SELECT peer_signer_sha256 FROM "+q("recovery_work_admissions")+" WHERE activation_id=$1", plan.ColdActivationID).Scan(&peer) != nil ||
		!validDigest(peer) || statement.SignerSHA256() == peer {
		return empty, ErrRecoveryAccounting
	}
	observation, err := scanRecoveryUsageStored(tx.QueryRow(ctx, "SELECT "+recoveryUsageStoredColumns+" FROM "+q("recovery_usage_observations")+" WHERE id=$1", st.ObservationID))
	if err != nil || observation.acquisitionSHA != st.AcquisitionSHA {
		return empty, ErrRecoveryAccounting
	}
	if _, err = s.verifyRecoveryUsageStoredTx(ctx, tx, plan, link, observation); err != nil {
		return empty, ErrRecoveryAccounting
	}
	// An invoice line is unique within the provider account, including across
	// organizations on this restored database. A changed statement for the
	// same line conflicts. A correction has its own provider line and revision.
	identity, fact, correction := recoveryFinanceEvidence(st, statement.SHA256())
	result := RecoveryFinanceResult{State: "not_recorded", OperationID: identity[:32], StatementSHA256: statement.SHA256()}
	requestSHA := evidenceRequestHash(fact, correction)
	commitment := recoveryFinanceCommitment(statement.SHA256(), statement.SignerSHA256(), observation.planSHA, observation.acquisitionSHA)
	var matches bool
	held := RecoveryTargetState{State: "held", RestoreID: plan.RestoreID, InstallationID: plan.TargetID}
	err = tx.QueryRow(ctx, `SELECT evidence_batch_id=$2 AND organization_id=$3 AND reservation_id=$4 AND
 request_sha256=$5 AND plan_sha256=$6 AND expected_revision=$7 FROM `+q("recovery_accounting_batches")+" WHERE id=$1",
		result.OperationID, link.Link().OperationID, selection.OrganizationID, selection.ReservationID, requestSHA, commitment, st.ExpectedRevision).Scan(&matches)
	if err == nil && matches {
		envelope, publicKey := statement.RetainedProof()
		if tx.QueryRow(ctx, "SELECT usage_id=$2 AND statement_sha256=$3 AND envelope=$4 AND public_key=$5 FROM "+q("recovery_finance_statements")+" WHERE batch_id=$1",
			result.OperationID, st.ObservationID, statement.SHA256(), envelope, publicKey).Scan(&matches) != nil || !matches {
			return empty, ErrRecoveryAccounting
		}
		if _, err = tx.Exec(ctx, "SET LOCAL search_path="+pgx.Identifier{s.runtimeSchema}.Sanitize()+",pg_catalog,pg_temp"); err != nil {
			return empty, ErrRecoveryAccounting
		}
		reservation, e := scanReservation(tx.QueryRow(ctx, "SELECT "+reservationColumns+" FROM "+q("reservations")+" WHERE organization_id=$1 AND id=$2",
			selection.OrganizationID, selection.ReservationID))
		var evidenceID string
		if e != nil || tx.QueryRow(ctx, "SELECT id FROM "+q("cost_evidence")+" WHERE organization_id=$1 AND reservation_id=$2 AND source=$3 AND reference=$4 AND request_hash=$5",
			selection.OrganizationID, selection.ReservationID, fact.Source, fact.Reference, requestSHA).Scan(&evidenceID) != nil {
			return empty, ErrRecoveryAccounting
		}
		outcome, err := accountingOutcomeTx(ctx, tx, reservation, evidenceID)
		if err != nil {
			return empty, err
		}
		if resolution != nil {
			b := recoveryAccountingBatch{evidenceBatchID: link.Link().OperationID, organizationID: selection.OrganizationID, reservationID: selection.ReservationID}
			result, write, err := s.resolveRecoveryFinanceStatementTx(ctx, tx, held, plan.HeadID, b, result, commitment, outcome, *resolution, post)
			if err != nil {
				return empty, err
			}
			if !write {
				return result, tx.Rollback(ctx)
			}
			if err = tx.Commit(ctx); err != nil {
				recoveryFenceClose(conn.Conn())
				return empty, ErrRecoveryAccountingUnknown
			}
			return result, nil
		}
		result.State, result.Outcome = "already_recorded", &outcome
		return result, tx.Rollback(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) || resolution != nil {
		return empty, ErrRecoveryAccounting
	}
	if !post {
		return result, tx.Rollback(ctx)
	}
	b := recoveryAccountingBatch{id: result.OperationID, evidenceBatchID: link.Link().OperationID, organizationID: selection.OrganizationID,
		reservationID: selection.ReservationID, requestSHA256: requestSHA, planSHA256: commitment, expectedRevision: int64(st.ExpectedRevision)}
	outcome, receipt, err := s.recordRecoveryFinancialFactTx(ctx, tx, held, b, fact, correction)
	if err != nil || receipt.ID != b.id || receipt.PreviousID != plan.HeadID {
		return empty, ErrRecoveryAccounting
	}
	envelope, publicKey := statement.RetainedProof()
	if _, err = tx.Exec(ctx, "INSERT INTO "+q("recovery_finance_statements")+" (batch_id,usage_id,statement_sha256,envelope,public_key) VALUES($1,$2,$3,$4,$5)",
		b.id, st.ObservationID, statement.SHA256(), envelope, publicKey); err != nil {
		return empty, ErrRecoveryAccounting
	}
	if err = tx.Commit(ctx); err != nil {
		recoveryFenceClose(conn.Conn())
		return empty, ErrRecoveryAccountingUnknown
	}
	result.State, result.Outcome = "recorded", &outcome
	return result, nil
}

func recoveryFinanceEvidence(st recoveryarchive.FinanceStatement, statementSHA string) (string, domain.CostEvidence, *int) {
	identity := digestValue([]string{"cold-finance-line-v1", st.Scope.Account, st.InvoiceLine})
	fact := domain.CostEvidence{Source: st.Scope.Account, Reference: "finance-line:" + identity, PayloadDigest: statementSHA,
		ActualMicroUSD: st.AmountMicroUSD, Reason: st.CorrectionReason, Final: true, Currency: "USD"}
	var correction *int
	if st.CorrectionReason != "" {
		correction = &st.ExpectedRevision
	}
	return identity, fact, correction
}

func recoveryFinanceCommitment(statement, signer, plan, acquisition string) string {
	return digestValue([]string{"cold-finance-posting-v1", statement, signer, plan, acquisition})
}
