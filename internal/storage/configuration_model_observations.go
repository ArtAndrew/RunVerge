// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

const configurationProbeSafeText = "Connection returned a structurally valid response. Response content was discarded."

// Called inside the ordinary model dispatch transaction, after its immutable
// Action marker and before commit/network. A failed marker rolls back both.
func markConfigurationModelProbeDispatchTx(ctx context.Context, tx pgx.Tx, l *domain.Lease, r domain.Reservation, permit domain.DispatchPermit) error {
	v, err := loadConfigurationVerificationForModelTx(ctx, tx, l.OrganizationID, l.TaskID)
	if errors.Is(err, domain.NotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if v.Status != "queued" || v.ActorID != l.Task.OwnerID || r.TaskID != l.TaskID || r.OperationID != l.OperationID || r.ID != permit.ReservationID || r.ActionID != permit.ActionID || permit.FencingToken != l.Token {
		return domain.Forbidden
	}
	result, err := tx.Exec(ctx, `UPDATE configuration_verifications SET status='sent',worker_id=$3,
 lease_until=(SELECT lease_until FROM operations WHERE organization_id=$1 AND id=$7),fencing_token=$4,
 dispatch_started_at=$5,dispatched_worker_id=$3,dispatched_fencing_token=$4
 WHERE organization_id=$1 AND id=$2 AND task_id=$6 AND kind='model' AND status='queued'`, l.OrganizationID, v.ID, l.OwnerID, l.Token, permit.IssuedAt, l.TaskID, l.OperationID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return domain.Forbidden
	}
	return nil
}

// This fact-ingestion path deliberately performs no current role/grant check:
// revocation stops new dispatch, not observations of already incurred exposure.
// Exact immutable Task/Operation/Reservation/Action/dispatch provenance remains
// mandatory, including after lease expiry, account disable or a worker crash.
func (s *Store) RecordConfigurationModelProbeObservation(ctx context.Context, l *domain.Lease, reservationID string, in domain.ConfigurationProbeObservation) (bool, error) {
	if l == nil || in.OrganizationID != l.OrganizationID || in.TaskID != l.TaskID || in.OperationID != l.OperationID || in.ReservationID != reservationID || in.WorkerID != l.OwnerID || in.FencingToken != l.Token ||
		in.Outcome != "response_received" || !validDigest(in.ResponseDigest) || !validDigest(in.IntentDigest) || in.ProtocolVersion != "" || in.CatalogDigest != "" || in.ToolCount != 0 {
		return false, domain.Err("invalid_request", "Некорректная структурная квитанция проверки", 400)
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	// An absent probe is common. Do not serialize ordinary model completions.
	v, err := loadConfigurationVerificationForModelTx(ctx, tx, l.OrganizationID, l.TaskID)
	if errors.Is(err, domain.NotFound) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	var ignored string
	if err = tx.QueryRow(ctx, "SELECT id FROM organizations WHERE id=$1 FOR UPDATE", l.OrganizationID).Scan(&ignored); err != nil {
		return true, rowExists(err)
	}
	v, err = loadConfigurationVerificationTx(ctx, tx, l.OrganizationID, v.ID, true)
	if err != nil {
		return true, err
	}
	if v.ID != in.VerificationID || v.Kind != "model" || v.TaskID != l.TaskID || v.DispatchStartedAt == nil || v.DispatchedWorkerID != in.WorkerID || v.DispatchedFencingToken != in.FencingToken {
		return true, domain.Forbidden
	}
	var actionStatus, intent, target string
	var resourceVersion int
	var issued time.Time
	var fence int64
	err = tx.QueryRow(ctx, `SELECT a.status,a.intent_hash,a.dispatch_issued_at,a.dispatch_fencing_token,t.model_connection_id,t.model_connection_version
 FROM reservations r JOIN actions a ON a.organization_id=r.organization_id AND a.id=r.action_id
 JOIN operations o ON o.organization_id=r.organization_id AND o.id=r.operation_id
 JOIN tasks t ON t.organization_id=r.organization_id AND t.id=r.task_id
 WHERE r.organization_id=$1 AND r.id=$2 AND r.task_id=$3 AND r.operation_id=$4 AND r.category='model'
 AND o.task_id=r.task_id AND a.task_id=r.task_id AND a.operation_id=r.operation_id
 AND a.dispatch_issued_at IS NOT NULL AND a.dispatch_fencing_token IS NOT NULL`, l.OrganizationID, reservationID, l.TaskID, l.OperationID).Scan(&actionStatus, &intent, &issued, &fence, &target, &resourceVersion)
	if err != nil {
		return true, rowExists(err)
	}
	if (actionStatus != "sent" && actionStatus != "unknown" && actionStatus != "confirmed" && actionStatus != "rejected") || intent != in.IntentDigest || !issued.Equal(*v.DispatchStartedAt) || fence != in.FencingToken || target != v.Scope.TargetID || resourceVersion != v.ResourceVersion {
		return true, domain.Forbidden
	}
	// No raw candidate, provider request ID, model name or peer body is stored.
	outcome := map[string]any{"outcome": "response_received", "response_digest": in.ResponseDigest}
	digest := digestValue(outcome)
	var old []byte
	var oldDigest *string
	if err = tx.QueryRow(ctx, "SELECT outcome,evidence_digest FROM configuration_verifications WHERE organization_id=$1 AND id=$2", l.OrganizationID, v.ID).Scan(&old, &oldDigest); err != nil {
		return true, err
	}
	if len(old) > 0 {
		if oldDigest == nil || *oldDigest != digest {
			return true, domain.Err("conflict", "Квитанция проверки уже содержит другой факт", 409)
		}
		return true, tx.Commit(ctx)
	}
	if v.Status != "sent" && v.Status != "unknown" {
		return true, domain.Err("conflict", "Проверка не имеет исходной отправки", 409)
	}
	raw, _ := json.Marshal(outcome)
	if _, err = tx.Exec(ctx, "UPDATE configuration_verifications SET status='response_received',outcome=$3,evidence_digest=$4,reason_code='' WHERE organization_id=$1 AND id=$2", l.OrganizationID, v.ID, raw, digest); err != nil {
		return true, err
	}
	if err = appendConfigurationAuditTx(ctx, tx, l.OrganizationID, "worker", l.OwnerID, "verification", v.ID, "response_received", digest, "A structurally valid model response was observed; content discarded; billing remains separate"); err != nil {
		return true, err
	}
	return true, tx.Commit(ctx)
}

func scrubConfigurationProbeCandidateTx(ctx context.Context, tx pgx.Tx, org, taskID string, c *domain.ModelCandidate) error {
	_, err := loadConfigurationVerificationForModelTx(ctx, tx, org, taskID)
	if errors.Is(err, domain.NotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	c.Text, c.ProviderRequestID, c.ToolCalls = configurationProbeSafeText, "", nil
	return nil
}
