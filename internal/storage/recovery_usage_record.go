// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"agentplatform.local/platform/internal/modelgateway"
	"agentplatform.local/platform/internal/recoveryaccess"
	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
)

var ErrRecoveryUsageRecording = errors.New("recovery usage observation not recorded; execution remains held")
var ErrRecoveryUsageUnknown = errors.New("usage observation commit outcome unknown; inspect exact operation without repeating acquisition")

type RecoveryUsageAdmission struct {
	Version             int       `json:"version"`
	Algorithm           string    `json:"algorithm"`
	AdmissionSHA256     string    `json:"admission_sha256"`
	OperationID         string    `json:"operation_id"`
	SourcePlanSHA256    string    `json:"source_plan_sha256"`
	AcquisitionSHA256   string    `json:"acquisition_sha256"`
	ResponseSHA256      string    `json:"response_sha256"`
	TargetCatalogSHA256 string    `json:"target_catalog_sha256"`
	PolicySHA256        string    `json:"policy_sha256"`
	PolicyEpoch         uint64    `json:"policy_epoch"`
	PolicyOperationID   string    `json:"policy_operation_id"`
	AdmittedAt          time.Time `json:"admitted_at"`
	State               string    `json:"state"`
}

type RecoveryUsageRecording struct {
	RequestedOperationID string    `json:"requested_operation_id"`
	OperationID          string    `json:"operation_id,omitempty"`
	State                string    `json:"state"`
	ObservationState     string    `json:"observation_state,omitempty"`
	SourcePlanSHA256     string    `json:"source_plan_sha256,omitempty"`
	AcquisitionSHA256    string    `json:"acquisition_sha256,omitempty"`
	AdmissionSHA256      string    `json:"admission_sha256,omitempty"`
	ResponseSHA256       string    `json:"response_sha256,omitempty"`
	ResponseBytes        int       `json:"response_bytes,omitempty"`
	RecordedAt           time.Time `json:"recorded_at,omitempty"`
	ExecutionHeld        bool      `json:"execution_held"`
}

type recoveryUsageStored struct {
	id, restore, target, batch, org, reservation, state string
	planSHA, acquisitionSHA, responseSHA, admissionSHA  string
	plan, acquisition, response, admission              []byte
	recorded                                            time.Time
}

const recoveryUsageStoredColumns = "id,restore_id,target_installation_id,evidence_batch_id,organization_id,reservation_id,observation_state,source_plan_sha256,acquisition_sha256,response_sha256,admission_sha256,source_plan_commitment,acquisition_commitment,response,admission_commitment,recorded_at"

func scanRecoveryUsageStored(row pgx.Row) (r recoveryUsageStored, err error) {
	err = row.Scan(&r.id, &r.restore, &r.target, &r.batch, &r.org, &r.reservation, &r.state, &r.planSHA, &r.acquisitionSHA, &r.responseSHA, &r.admissionSHA, &r.plan, &r.acquisition, &r.response, &r.admission, &r.recorded)
	return r, err
}
func recoveryUsageOperationValid(id string) bool {
	return len(id) == 32 && strings.Trim(id, "0123456789abcdef") == ""
}
func recoveryUsageCanonical(raw []byte, value any, max int) bool {
	if len(raw) == 0 || len(raw) > max || decodeRecoveryHistoryJSON(raw, value, max) != nil {
		return false
	}
	canonical, err := json.Marshal(value)
	return err == nil && bytes.Equal(canonical, raw)
}
func recoveryUsageSealPlan(p RecoveryUsageReadbackPlan) RecoveryUsageReadbackPlan {
	// Reconstruct the acquired planner report for its digest; this is NOT the
	// outcome of the current write/inspection transaction.
	p.PlanSHA256 = ""
	p.RollbackConfirmed = true
	p.PlanSHA256 = digestValue(p)
	return p
}

func (s *Store) verifyRecoveryUsageStoredTx(ctx context.Context, tx pgx.Tx, current RecoveryUsageReadbackPlan, verified *recoveryarchive.VerifiedCaptureLink, r recoveryUsageStored) (RecoveryUsageRecording, error) {
	fail := func() (RecoveryUsageRecording, error) { return RecoveryUsageRecording{}, ErrRecoveryUsageRecording }
	if !recoveryUsageOperationValid(r.id) || r.restore != current.RestoreID || r.target != current.TargetID || r.batch != verified.Link().OperationID || r.org != current.Selection.OrganizationID || r.reservation != current.Selection.ReservationID || r.state != "observed_unverified" || r.recorded.IsZero() || len(r.response) == 0 || len(r.response) > 256<<10 || Hash(r.response) != r.responseSHA || Hash(r.plan) != r.planSHA || Hash(r.acquisition) != r.acquisitionSHA || Hash(r.admission) != r.admissionSHA {
		return fail()
	}
	var plan RecoveryUsageReadbackPlan
	var acquisition recoveryaccess.UsageAcquisitionReceipt
	var admission RecoveryUsageAdmission
	if !recoveryUsageCanonical(r.plan, &plan, 65536) || !recoveryUsageCanonical(r.acquisition, &acquisition, 65536) || !recoveryUsageCanonical(r.admission, &admission, 16384) || plan.PlanSHA256 != "" || acquisition.ReceiptSHA256 != "" || admission.AdmissionSHA256 != "" {
		return fail()
	}
	acquisition.ReceiptSHA256 = r.acquisitionSHA
	if !recoveryaccess.UsageReceiptConsistent(acquisition, r.response) {
		return fail()
	}
	// A recorded observation remains inspectable after later accounting work.
	// Its prior head must be an actual immutable prefix of this verified chain.
	if plan.HeadID != "" {
		var exists bool
		if tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+pgx.Identifier{s.runtimeSchema, "recovery_accounting_batches"}.Sanitize()+" WHERE id=$1 AND restore_id=$2)", plan.HeadID, r.restore).Scan(&exists) != nil || !exists {
			return fail()
		}
	}
	// Preserve the original observation commitment across a metadata-only
	// host upgrade. All other plan fields remain rederived from verified inputs.
	if !recoveryUsageHistoricalCatalog(plan.TargetCatalogSHA256, current.TargetCatalogSHA256) {
		return fail()
	}
	current.TargetCatalogSHA256 = plan.TargetCatalogSHA256
	current.HeadID = plan.HeadID
	current = recoveryUsageSealPlan(current)
	if current.State != "request_identity_available" || current.PlanSHA256 != r.planSHA || acquisition.Version != 1 || acquisition.AccountMappingMethod != "current_host_operator_attestation" || acquisition.Request != recoveryUsageRequestFromPlan(current) || acquisition.Observation.ResponseSHA256 != r.responseSHA || !modelgateway.UsageObservationMatchesBytes(r.response, acquisition.Observation) {
		return fail()
	}
	if admission.Version != 1 || admission.Algorithm != "recovery-usage-admission-v1" || admission.OperationID != r.id || admission.SourcePlanSHA256 != r.planSHA || admission.AcquisitionSHA256 != r.acquisitionSHA || admission.ResponseSHA256 != r.responseSHA || !recoveryAccountingV2HostCatalog(admission.TargetCatalogSHA256) || admission.TargetCatalogSHA256 != plan.TargetCatalogSHA256 || admission.PolicySHA256 != acquisition.PolicySHA256 || admission.PolicyEpoch != acquisition.PolicyEpoch || admission.PolicyOperationID != acquisition.PolicyOperationID || admission.State != r.state || admission.AdmittedAt.Before(acquisition.Observation.ObservedAt) || !admission.AdmittedAt.Before(acquisition.Policy.ExpiresAt) {
		return fail()
	}
	return RecoveryUsageRecording{OperationID: r.id, State: "recorded", ObservationState: r.state, SourcePlanSHA256: r.planSHA, AcquisitionSHA256: r.acquisitionSHA, AdmissionSHA256: r.admissionSHA, ResponseSHA256: r.responseSHA, ResponseBytes: len(r.response), RecordedAt: r.recorded, ExecutionHeld: true}, nil
}

// RecordRecoveryUsage never calls a provider. The opaque acquisition is only one
// input: archives, current target/baseline and current policy are revalidated.
// Current-policy lock precedes the database gate and spans commit.
func (s *Store) RecordRecoveryUsage(ctx context.Context, selected, late *recoveryarchive.VerifiedArchive, verified *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, selection RecoveryUsageSelection, authorityPath, operationID string, observation recoveryaccess.AcquiredUsageObservation) (RecoveryUsageRecording, error) {
	if ctx == nil || !limits.valid() || !recoveryUsageOperationValid(operationID) || !validText(selection.OrganizationID, 128) || !validText(selection.ReservationID, 128) {
		return RecoveryUsageRecording{}, ErrRecoveryUsageRecording
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	evidence, err := s.bindRecoveryUsageEvidence(ctx, selected, late, verified, spec, limits)
	if err != nil {
		return RecoveryUsageRecording{}, err
	}
	receipt := observation.Receipt()
	raw := observation.RawResponse()
	var result RecoveryUsageRecording
	committing := false
	err = recoveryaccess.WithUsageAdmission(ctx, authorityPath, observation, func(ctx context.Context, policy recoveryaccess.UsagePolicyAdmission) (callbackErr error) {
		conn, err := s.Pool.Acquire(ctx)
		if err != nil {
			return ErrRecoveryUsageRecording
		}
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		if err != nil {
			return ErrRecoveryUsageRecording
		}
		defer func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if !committing {
				if tx.Rollback(cleanup) != nil {
					_ = conn.Conn().Close(cleanup)
					result = RecoveryUsageRecording{}
					callbackErr = ErrRecoveryStageCleanup
				}
			}
		}()
		plan, err := s.planRecoveryUsageReadbackTx(ctx, tx, selected, late, verified, spec, limits, selection, evidence)
		if err != nil {
			return err
		}
		q := pgx.Identifier{s.runtimeSchema, "recovery_usage_observations"}.Sanitize()
		rows, err := tx.Query(ctx, "SELECT "+recoveryUsageStoredColumns+" FROM "+q+" WHERE id=$1 OR (restore_id=$2 AND acquisition_sha256=$3) LIMIT 2", operationID, spec.RestoreID, receipt.ReceiptSHA256)
		if err != nil {
			return ErrRecoveryUsageRecording
		}
		var existing *recoveryUsageStored
		for rows.Next() {
			r, e := scanRecoveryUsageStored(rows)
			if e != nil || existing != nil {
				rows.Close()
				return ErrRecoveryUsageRecording
			}
			existing = &r
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return ErrRecoveryUsageRecording
		}
		if existing != nil {
			if existing.acquisitionSHA != receipt.ReceiptSHA256 || existing.responseSHA != receipt.Observation.ResponseSHA256 {
				return ErrRecoveryUsageRecording
			}
			result, err = s.verifyRecoveryUsageStoredTx(ctx, tx, plan, verified, *existing)
			if err != nil {
				return err
			}
			result.State = "already_recorded"
			result.RequestedOperationID = operationID
			return nil
		}
		result, err = s.recordRecoveryUsageTx(ctx, tx, spec, verified, plan, policy, operationID, receipt, raw)
		if err != nil {
			return err
		}
		if ctx.Err() != nil || !time.Now().Before(policy.ExpiresAt) {
			return ErrRecoveryUsageRecording
		}
		committing = true
		if err = tx.Commit(ctx); err != nil {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = conn.Conn().Close(cleanup)
			return ErrRecoveryUsageUnknown
		}
		return nil
	})
	if err != nil {
		if committing {
			return RecoveryUsageRecording{}, ErrRecoveryUsageUnknown
		}
		return RecoveryUsageRecording{}, err
	}
	return result, nil
}

// Only the coordinator calls this executor after current policy admission
// and source-plan verification under the database gate. It does no provider I/O.
func (s *Store) recordRecoveryUsageTx(ctx context.Context, tx pgx.Tx, spec RecoveryTargetSpec, verified *recoveryarchive.VerifiedCaptureLink,
	plan RecoveryUsageReadbackPlan, policy recoveryaccess.UsagePolicyAdmission, operationID string, receipt recoveryaccess.UsageAcquisitionReceipt, raw []byte) (RecoveryUsageRecording, error) {
	var empty RecoveryUsageRecording
	plan = recoveryUsageSealPlan(plan)
	if plan.State != "request_identity_available" || recoveryUsageRequestFromPlan(plan) != receipt.Request ||
		!modelgateway.UsageObservationMatchesBytes(raw, receipt.Observation) || ctx.Err() != nil || !time.Now().Before(policy.ExpiresAt) {
		return empty, ErrRecoveryUsageRecording
	}
	planSHA, acquisitionSHA := plan.PlanSHA256, receipt.ReceiptSHA256
	plan.PlanSHA256, receipt.ReceiptSHA256 = "", ""
	planBytes, _ := json.Marshal(plan)
	acquisitionBytes, _ := json.Marshal(receipt)
	if Hash(planBytes) != planSHA || Hash(acquisitionBytes) != acquisitionSHA {
		return empty, ErrRecoveryUsageRecording
	}
	admission := RecoveryUsageAdmission{Version: 1, Algorithm: "recovery-usage-admission-v1", OperationID: operationID,
		SourcePlanSHA256: planSHA, AcquisitionSHA256: acquisitionSHA, ResponseSHA256: receipt.Observation.ResponseSHA256,
		TargetCatalogSHA256: recoverySemanticCatalogSHA256, PolicySHA256: policy.PolicySHA256, PolicyEpoch: policy.PolicyEpoch,
		PolicyOperationID: policy.PolicyOperationID, AdmittedAt: policy.AdmittedAt, State: "observed_unverified"}
	admissionBytes, _ := json.Marshal(admission)
	admissionSHA := Hash(admissionBytes)
	if plan.ColdActivationID != "" {
		if err := s.persistRecoveryColdUsageAnchorTx(ctx, tx, spec, verified, plan.ColdActivationID); err != nil {
			return empty, err
		}
	}
	q := pgx.Identifier{s.runtimeSchema, "recovery_usage_observations"}.Sanitize()
	var recorded time.Time
	err := tx.QueryRow(ctx, "INSERT INTO "+q+`(id,restore_id,target_installation_id,evidence_batch_id,organization_id,reservation_id,source_plan_sha256,source_plan_commitment,
 acquisition_sha256,acquisition_commitment,response_sha256,response,admission_sha256,admission_commitment)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING recorded_at`,
		operationID, spec.RestoreID, spec.TargetInstallationID, verified.Link().OperationID, plan.Selection.OrganizationID, plan.Selection.ReservationID,
		planSHA, planBytes, acquisitionSHA, acquisitionBytes, receipt.Observation.ResponseSHA256, raw, admissionSHA, admissionBytes).Scan(&recorded)
	if err != nil {
		return empty, ErrRecoveryUsageRecording
	}
	return RecoveryUsageRecording{RequestedOperationID: operationID, OperationID: operationID, State: "recorded", ObservationState: "observed_unverified",
		SourcePlanSHA256: planSHA, AcquisitionSHA256: acquisitionSHA, AdmissionSHA256: admissionSHA, ResponseSHA256: receipt.Observation.ResponseSHA256,
		ResponseBytes: len(raw), RecordedAt: recorded, ExecutionHeld: true}, nil
}

// Inspection is historical and needs no current provider key. It validates the
// exact archives and held target, and never repeats acquisition or insertion.
func (s *Store) InspectRecoveryUsage(ctx context.Context, selected, late *recoveryarchive.VerifiedArchive, verified *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, selection RecoveryUsageSelection, operationID, acquisitionSHA string) (result RecoveryUsageRecording, resultErr error) {
	if ctx == nil || !limits.valid() || !recoveryUsageOperationValid(operationID) || !validDigest(acquisitionSHA) || !validText(selection.OrganizationID, 128) || !validText(selection.ReservationID, 128) {
		return result, ErrRecoveryUsageRecording
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	evidence, err := s.bindRecoveryUsageEvidence(ctx, selected, late, verified, spec, limits)
	if err != nil {
		return result, err
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return result, ErrRecoveryUsageRecording
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return result, ErrRecoveryUsageRecording
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if tx.Rollback(cleanup) != nil {
			_ = conn.Conn().Close(cleanup)
			result, resultErr = RecoveryUsageRecording{}, ErrRecoveryStageCleanup
		}
		if resultErr != nil {
			result = RecoveryUsageRecording{}
		}
	}()
	plan, err := s.planRecoveryUsageReadbackTx(ctx, tx, selected, late, verified, spec, limits, selection, evidence)
	if err != nil {
		return result, err
	}
	r, err := scanRecoveryUsageStored(tx.QueryRow(ctx, "SELECT "+recoveryUsageStoredColumns+" FROM "+pgx.Identifier{s.runtimeSchema, "recovery_usage_observations"}.Sanitize()+" WHERE id=$1", operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryUsageRecording{RequestedOperationID: operationID, State: "not_recorded", ExecutionHeld: true}, nil
	}
	if err != nil || r.acquisitionSHA != acquisitionSHA {
		return result, ErrRecoveryUsageRecording
	}
	result, err = s.verifyRecoveryUsageStoredTx(ctx, tx, plan, verified, r)
	if err != nil {
		return RecoveryUsageRecording{}, err
	}
	result.RequestedOperationID = operationID
	return result, nil
}
