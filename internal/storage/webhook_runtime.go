// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Every webhook writer, including factual recording after revocation, holds
// this exclusive organization gate before subscription/delivery/attempt locks.
// Facts do not require an active organization or current execution consent.
func lockWebhookFactOrganizationTx(ctx context.Context, tx pgx.Tx, org string) error {
	var id string
	return rowExists(tx.QueryRow(ctx, "SELECT id FROM organizations WHERE id=$1 FOR UPDATE", org).Scan(&id))
}
func webhookNowTx(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now)
	return now, err
}
func webhookLeaseMatches(d webhookDeliveryRecord, a webhookAttemptRecord, l *domain.WebhookLease) bool {
	return l != nil && l.Token > 0 && l.OrganizationID == d.OrganizationID && l.TaskID == d.TaskID && l.SubscriptionID == d.SubscriptionID && l.DeliveryID == d.ID && l.AttemptID == a.ID && a.ID == d.CurrentAttemptID && a.DeliveryID == d.ID && l.WorkerID == a.WorkerID && l.Token == a.Token && l.ProfileDigest == d.ProfileDigest && l.EnvelopeDigest == d.EnvelopeDigest && l.DestinationID == d.DestinationID && l.DestinationVersion == d.DestinationVersion && a.LeaseUntil != nil && l.LeaseUntil.Equal(*a.LeaseUntil) && Hash(l.Envelope) == d.EnvelopeDigest && string(l.Envelope) == d.Envelope
}
func webhookStaleLease() error {
	return domain.Err("stale_lease", "Доставка потеряла исходную аренду", 409)
}

// The aggregate follows the current attempt unless ANY immutable physical
// attempt has an ACK. It never discards old sent/unknown/definitive facts.
func syncWebhookDeliveryTx(ctx context.Context, tx pgx.Tx, org, task, id string) (webhookDeliveryRecord, error) {
	_, err := tx.Exec(ctx, `UPDATE webhook_deliveries d SET status=CASE WHEN EXISTS(SELECT 1 FROM webhook_attempts a WHERE a.organization_id=d.organization_id AND a.delivery_id=d.id AND a.status='acknowledged') THEN 'acknowledged' ELSE (SELECT a.status FROM webhook_attempts a WHERE a.organization_id=d.organization_id AND a.id=d.current_attempt_id) END,version=version+1 WHERE d.organization_id=$1 AND d.task_id=$2 AND d.id=$3`, org, task, id)
	if err != nil {
		return webhookDeliveryRecord{}, err
	}
	return loadWebhookDeliveryTx(ctx, tx, org, task, id, false)
}
func stopWebhookAttemptTx(ctx context.Context, tx pgx.Tx, d webhookDeliveryRecord, a webhookAttemptRecord, status, code string) error {
	if a.SentAt != nil || (a.Status != "queued" && a.Status != "leased") {
		return webhookStaleLease()
	}
	if _, err := tx.Exec(ctx, `UPDATE webhook_attempts SET status=$3,reason_code=$4,worker_id=NULL,lease_until=NULL WHERE organization_id=$1 AND id=$2`, d.OrganizationID, a.ID, status, code); err != nil {
		return err
	}
	if _, err := syncWebhookDeliveryTx(ctx, tx, d.OrganizationID, d.TaskID, d.ID); err != nil {
		return err
	}
	return appendWebhookAuditTx(ctx, tx, d.OrganizationID, d.TaskID, "", a.ID, "attempt.stopped", code, "")
}

func (s *Store) ClaimWebhook(ctx context.Context, worker string, duration time.Duration) (*domain.WebhookLease, domain.WebhookDestinationProfile, error) {
	var empty domain.WebhookDestinationProfile
	if !configurationIdentifier.MatchString(worker) || duration < time.Second || duration > time.Minute {
		return nil, empty, domain.Err("invalid_request", "Некорректная аренда доставки", 400)
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return nil, empty, err
	}
	defer tx.Rollback(ctx)
	var org, task, id, subID string
	err = tx.QueryRow(ctx, `SELECT d.organization_id,d.task_id,d.id,d.subscription_id FROM webhook_deliveries d JOIN webhook_attempts a ON a.organization_id=d.organization_id AND a.id=d.current_attempt_id
 WHERE NOT recovery_historical_task(d.organization_id,d.task_id) AND d.status IN('queued','leased') AND a.sent_at IS NULL AND (a.status='queued' OR (a.status='leased' AND a.lease_until<=clock_timestamp())) ORDER BY d.created_at,d.id LIMIT 1`).Scan(&org, &task, &id, &subID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, empty, nil
	}
	if err != nil {
		return nil, empty, err
	}
	sub, err := loadWebhookSubscriptionTx(ctx, tx, org, task, subID, false)
	if err != nil {
		return nil, empty, err
	}
	if err = lockAuthorityPrincipalsTx(ctx, tx, org, []string{sub.CreatorID, sub.OwnerID}); err != nil {
		return nil, empty, err
	}
	// Active/consent is evaluated AFTER the organization lock; an inactive
	// organization still permits stopping proven-unsent work durably.
	if err = lockWebhookFactOrganizationTx(ctx, tx, org); err != nil {
		return nil, empty, err
	}
	sub, err = loadWebhookSubscriptionTx(ctx, tx, org, task, subID, true)
	if err != nil {
		return nil, empty, err
	}
	d, err := loadWebhookDeliveryTx(ctx, tx, org, task, id, true)
	if err != nil {
		return nil, empty, err
	}
	a, err := loadWebhookAttemptTx(ctx, tx, org, id, d.CurrentAttemptID, true)
	if err != nil {
		return nil, empty, err
	}
	now, err := webhookNowTx(ctx, tx)
	if err != nil {
		return nil, empty, err
	}
	if d.Status != "queued" && d.Status != "leased" || a.SentAt != nil || a.Status != "queued" && (a.Status != "leased" || a.LeaseUntil == nil || now.Before(*a.LeaseUntil)) {
		return nil, empty, nil
	}
	stopStatus, stopCode := "", ""
	if !now.Before(d.ExpiresAt) {
		stopStatus, stopCode = "expired", "delivery_expired"
	} else if err = checkWebhookConsentTx(ctx, tx, sub); err != nil {
		if !configurationAuthorityDenied(err) {
			return nil, empty, err
		}
		stopStatus, stopCode = "stopped", "authorization_revoked"
	} else if a.Status == "leased" && a.Recoveries >= 5 {
		stopStatus, stopCode = "stopped", "lease_recovery_exhausted"
	}
	if stopStatus != "" {
		if err = stopWebhookAttemptTx(ctx, tx, d, a, stopStatus, stopCode); err != nil {
			return nil, empty, err
		}
		return nil, empty, tx.Commit(ctx)
	}
	profile, digest, err := loadWebhookProfileTx(ctx, tx, org, d.DestinationID, d.DestinationVersion)
	if err != nil {
		return nil, empty, err
	}
	if digest != d.ProfileDigest || digest != sub.ProfileDigest || d.SubscriptionID != sub.ID || d.DestinationID != sub.DestinationID || d.DestinationVersion != sub.DestinationVersion || !d.ExpiresAt.Equal(sub.ExpiresAt) || Hash([]byte(d.Envelope)) != d.EnvelopeDigest {
		return nil, empty, domain.Forbidden
	}
	now, err = webhookNowTx(ctx, tx)
	if err != nil {
		return nil, empty, err
	}
	until := now.Add(duration)
	if d.ExpiresAt.Before(until) {
		until = d.ExpiresAt
	}
	if !now.Before(until) {
		return nil, empty, domain.Forbidden
	}
	a, err = scanWebhookAttempt(tx.QueryRow(ctx, `UPDATE webhook_attempts SET status='leased',worker_id=$3,lease_until=$4,fencing_token=fencing_token+1,recoveries=recoveries+CASE WHEN status='leased' THEN 1 ELSE 0 END,reason_code='' WHERE organization_id=$1 AND id=$2 RETURNING `+webhookAttemptColumns, org, a.ID, worker, until))
	if err != nil {
		return nil, empty, err
	}
	d, err = syncWebhookDeliveryTx(ctx, tx, org, task, id)
	if err != nil {
		return nil, empty, err
	}
	if err = appendWebhookAuditTx(ctx, tx, org, task, "", a.ID, "attempt.claimed", "proven_unsent", ""); err != nil {
		return nil, empty, err
	}
	lease := &domain.WebhookLease{OrganizationID: org, TaskID: task, SubscriptionID: subID, DeliveryID: id, AttemptID: a.ID, DestinationID: d.DestinationID, DestinationVersion: d.DestinationVersion, ProfileDigest: d.ProfileDigest, EnvelopeDigest: d.EnvelopeDigest, WorkerID: worker, Token: a.Token, LeaseUntil: *a.LeaseUntil, Envelope: []byte(d.Envelope)}
	return lease, profile, tx.Commit(ctx)
}

func (s *Store) PrepareWebhookDispatch(ctx context.Context, l *domain.WebhookLease) (domain.WebhookDispatchPermit, error) {
	var empty domain.WebhookDispatchPermit
	if l == nil {
		return empty, domain.Forbidden
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	sub, err := loadWebhookSubscriptionTx(ctx, tx, l.OrganizationID, l.TaskID, l.SubscriptionID, false)
	if err != nil {
		return empty, err
	}
	if err = lockAuthorityPrincipalsTx(ctx, tx, l.OrganizationID, []string{sub.CreatorID, sub.OwnerID}); err != nil {
		return empty, err
	}
	if err = lockWebhookFactOrganizationTx(ctx, tx, l.OrganizationID); err != nil {
		return empty, err
	}
	sub, err = loadWebhookSubscriptionTx(ctx, tx, l.OrganizationID, l.TaskID, l.SubscriptionID, true)
	if err != nil {
		return empty, err
	}
	d, err := loadWebhookDeliveryTx(ctx, tx, l.OrganizationID, l.TaskID, l.DeliveryID, true)
	if err != nil {
		return empty, err
	}
	a, err := loadWebhookAttemptTx(ctx, tx, l.OrganizationID, l.DeliveryID, l.AttemptID, true)
	if err != nil {
		return empty, err
	}
	if !webhookLeaseMatches(d, a, l) || d.Status != "leased" || a.Status != "leased" || a.SentAt != nil || d.AcknowledgedAttempts > 0 {
		return empty, webhookStaleLease()
	}
	if err = checkWebhookConsentTx(ctx, tx, sub); err != nil {
		return empty, err
	}
	now, err := webhookNowTx(ctx, tx)
	if err != nil {
		return empty, err
	}
	until := now.Add(2 * time.Second)
	for _, bound := range []time.Time{d.ExpiresAt, sub.ExpiresAt, *a.LeaseUntil} {
		if bound.Before(until) {
			until = bound
		}
	}
	if !now.Before(until) || now.Unix() < 0 {
		return empty, webhookStaleLease()
	}
	if _, err = tx.Exec(ctx, `UPDATE webhook_attempts SET status='sent',sent_at=$3,signature_timestamp=$4 WHERE organization_id=$1 AND id=$2`, d.OrganizationID, a.ID, now, now.Unix()); err != nil {
		return empty, err
	}
	if _, err = syncWebhookDeliveryTx(ctx, tx, d.OrganizationID, d.TaskID, d.ID); err != nil {
		return empty, err
	}
	if err = appendWebhookAuditTx(ctx, tx, d.OrganizationID, d.TaskID, "", a.ID, "attempt.sent", "short_dispatch_permit", ""); err != nil {
		return empty, err
	}
	permit := domain.WebhookDispatchPermit{OrganizationID: d.OrganizationID, TaskID: d.TaskID, SubscriptionID: d.SubscriptionID, DeliveryID: d.ID, AttemptID: a.ID, WorkerID: a.WorkerID, Token: a.Token, ProfileDigest: d.ProfileDigest, EnvelopeDigest: d.EnvelopeDigest, IssuedAt: now, NotAfter: until, ValidFor: until.Sub(now), SignatureTimestamp: now.Unix()}
	return permit, tx.Commit(ctx)
}

func (s *Store) StopWebhookUnsent(ctx context.Context, l *domain.WebhookLease, code string) error {
	if l == nil || (code != "credentials_unavailable" && code != "invalid_profile" && code != "execution_cancelled" && code != "execution_not_admitted") {
		return domain.Forbidden
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockWebhookFactOrganizationTx(ctx, tx, l.OrganizationID); err != nil {
		return err
	}
	d, err := loadWebhookDeliveryTx(ctx, tx, l.OrganizationID, l.TaskID, l.DeliveryID, true)
	if err != nil {
		return err
	}
	a, err := loadWebhookAttemptTx(ctx, tx, l.OrganizationID, l.DeliveryID, l.AttemptID, true)
	if err != nil {
		return err
	}
	if !webhookLeaseMatches(d, a, l) || a.Status != "leased" || a.SentAt != nil {
		return webhookStaleLease()
	}
	if err = stopWebhookAttemptTx(ctx, tx, d, a, "stopped", code); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) RecordWebhookObservation(ctx context.Context, in domain.WebhookObservation) (domain.WebhookDelivery, error) {
	var empty domain.WebhookDelivery
	if !configurationIdentifier.MatchString(in.OrganizationID) || !configurationIdentifier.MatchString(in.TaskID) || !configurationIdentifier.MatchString(in.DeliveryID) || !configurationIdentifier.MatchString(in.AttemptID) || !configurationIdentifier.MatchString(in.WorkerID) || !configurationDigest.MatchString(in.EnvelopeDigest) || in.Token < 1 || in.HTTPStatus != 0 && (in.HTTPStatus < 100 || in.HTTPStatus > 599) {
		return empty, domain.Forbidden
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	if err := admitRecoveryRowTx(ctx, tx, "webhook_deliveries", in.OrganizationID, in.DeliveryID); err != nil {
		return empty, err
	}
	if err = lockWebhookFactOrganizationTx(ctx, tx, in.OrganizationID); err != nil {
		return empty, err
	}
	d, err := loadWebhookDeliveryTx(ctx, tx, in.OrganizationID, in.TaskID, in.DeliveryID, true)
	if err != nil {
		return empty, err
	}
	a, err := loadWebhookAttemptTx(ctx, tx, in.OrganizationID, in.DeliveryID, in.AttemptID, true)
	if err != nil {
		return empty, err
	}
	if d.EnvelopeDigest != in.EnvelopeDigest || a.SentAt == nil || a.Token != in.Token || a.WorkerID != in.WorkerID {
		return empty, webhookStaleLease()
	}
	if a.ObservedAt != nil {
		if in.HTTPStatus != 0 && in.HTTPStatus != a.HTTPStatus {
			return empty, domain.Err("conflict", "Первая HTTP-наблюдаемая запись неизменяема", 409)
		}
		return d.WebhookDelivery, tx.Commit(ctx)
	}
	if in.HTTPStatus == 0 && a.Status == "unknown" {
		return d.WebhookDelivery, tx.Commit(ctx)
	}
	status, code := "unknown", "transport_unknown"
	if in.HTTPStatus >= 200 && in.HTTPStatus <= 299 {
		status, code = "acknowledged", "http_acknowledged"
	} else if in.HTTPStatus != 0 {
		status, code = "not_acknowledged", "http_not_acknowledged"
	}
	if _, err = tx.Exec(ctx, `UPDATE webhook_attempts SET status=$3,http_status=$4,reason_code=$5,observed_at=CASE WHEN $4::integer=0 THEN NULL ELSE clock_timestamp() END,unknown_at=CASE WHEN $4::integer=0 THEN COALESCE(unknown_at,clock_timestamp()) ELSE unknown_at END WHERE organization_id=$1 AND id=$2`, in.OrganizationID, a.ID, status, in.HTTPStatus, code); err != nil {
		return empty, err
	}
	if status == "acknowledged" && a.ID != d.CurrentAttemptID {
		current, e := loadWebhookAttemptTx(ctx, tx, in.OrganizationID, d.ID, d.CurrentAttemptID, true)
		if e != nil {
			return empty, e
		}
		if current.SentAt == nil && (current.Status == "queued" || current.Status == "leased") {
			if _, err = tx.Exec(ctx, `UPDATE webhook_attempts SET status='stopped',reason_code='already_acknowledged',worker_id=NULL,lease_until=NULL WHERE organization_id=$1 AND id=$2`, in.OrganizationID, current.ID); err != nil {
				return empty, err
			}
		}
	}
	d, err = syncWebhookDeliveryTx(ctx, tx, in.OrganizationID, in.TaskID, in.DeliveryID)
	if err != nil {
		return empty, err
	}
	if err = appendWebhookAuditTx(ctx, tx, in.OrganizationID, in.TaskID, "", a.ID, "attempt.observed", code, ""); err != nil {
		return empty, err
	}
	return d.WebhookDelivery, tx.Commit(ctx)
}

// Recovery records uncertainty, never another external call. This intentionally
// scans ALL sent attempts, even if an older attempt already acknowledged the
// aggregate while this physical replacement was in flight.
func (s *Store) ReconcileWebhookSent(ctx context.Context) (bool, error) {
	if err := s.CheckRuntimeAdmission(ctx); err != nil {
		return false, err
	}
	var in domain.WebhookObservation
	err := s.Pool.QueryRow(ctx, `SELECT d.organization_id,d.task_id,d.id,a.id,d.envelope_digest,a.worker_id,a.fencing_token FROM webhook_attempts a JOIN webhook_deliveries d ON d.organization_id=a.organization_id AND d.id=a.delivery_id WHERE NOT recovery_historical_task(d.organization_id,d.task_id) AND a.status='sent' AND a.lease_until<=clock_timestamp() ORDER BY a.sent_at,a.id LIMIT 1`).Scan(&in.OrganizationID, &in.TaskID, &in.DeliveryID, &in.AttemptID, &in.EnvelopeDigest, &in.WorkerID, &in.Token)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = s.RecordWebhookObservation(ctx, in)
	return err == nil, err
}
