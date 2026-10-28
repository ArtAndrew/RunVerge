// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

type webhookDeliveryRecord struct {
	domain.WebhookDelivery
	ProfileDigest, Envelope string
}
type webhookAttemptRecord struct {
	domain.WebhookAttempt
	WorkerID           string
	LeaseUntil         *time.Time
	Token              int64
	Recoveries         int
	SignatureTimestamp *int64
}

const webhookDeliveryColumns = `d.id,d.organization_id,d.task_id,d.subscription_id,d.destination_id,d.destination_version,d.event_id,d.event_sequence,d.envelope_digest,d.version,d.status,d.attempt_count,
 (SELECT count(*) FROM webhook_attempts a WHERE a.organization_id=d.organization_id AND a.delivery_id=d.id AND a.status='unknown'),
 (SELECT count(*) FROM webhook_attempts a WHERE a.organization_id=d.organization_id AND a.delivery_id=d.id AND a.unknown_at IS NOT NULL),
 (SELECT count(*) FROM webhook_attempts a WHERE a.organization_id=d.organization_id AND a.delivery_id=d.id AND a.status='acknowledged'),
 d.max_attempts,d.current_attempt_id,d.created_at,d.expires_at,d.profile_digest,d.envelope`
const webhookAttemptColumns = `id,organization_id,task_id,delivery_id,number,status,http_status,reason_code,created_at,sent_at,observed_at,unknown_at,COALESCE(worker_id,''),lease_until,fencing_token,recoveries,signature_timestamp`

func scanWebhookDelivery(row scanner) (v webhookDeliveryRecord, err error) {
	err = row.Scan(&v.ID, &v.OrganizationID, &v.TaskID, &v.SubscriptionID, &v.DestinationID, &v.DestinationVersion, &v.EventID, &v.EventSequence, &v.EnvelopeDigest, &v.Version, &v.Status, &v.AttemptCount, &v.UnknownAttempts, &v.EverUnknownAttempts, &v.AcknowledgedAttempts, &v.MaxAttempts, &v.CurrentAttemptID, &v.CreatedAt, &v.ExpiresAt, &v.ProfileDigest, &v.Envelope)
	v.AvailableCommands = []string{}
	return v, rowExists(err)
}
func scanWebhookAttempt(row scanner) (v webhookAttemptRecord, err error) {
	err = row.Scan(&v.ID, &v.OrganizationID, &v.TaskID, &v.DeliveryID, &v.Number, &v.Status, &v.HTTPStatus, &v.ReasonCode, &v.CreatedAt, &v.SentAt, &v.ObservedAt, &v.UnknownAt, &v.WorkerID, &v.LeaseUntil, &v.Token, &v.Recoveries, &v.SignatureTimestamp)
	return v, rowExists(err)
}
func loadWebhookDeliveryTx(ctx context.Context, tx pgx.Tx, org, task, id string, lock bool) (webhookDeliveryRecord, error) {
	q := "SELECT " + webhookDeliveryColumns + " FROM webhook_deliveries d WHERE d.organization_id=$1 AND d.task_id=$2 AND d.id=$3"
	if lock {
		q += " FOR UPDATE OF d"
	}
	return scanWebhookDelivery(tx.QueryRow(ctx, q, org, task, id))
}
func loadWebhookAttemptTx(ctx context.Context, tx pgx.Tx, org, delivery, id string, lock bool) (webhookAttemptRecord, error) {
	q := "SELECT " + webhookAttemptColumns + " FROM webhook_attempts WHERE organization_id=$1 AND delivery_id=$2 AND id=$3"
	if lock {
		q += " FOR UPDATE"
	}
	return scanWebhookAttempt(tx.QueryRow(ctx, q, org, delivery, id))
}
func webhookAttemptID(delivery string, n int) string {
	return "wha_" + Hash([]byte(delivery+"\x1f"+strconv.Itoa(n)))
}

func webhookRetryable(d webhookDeliveryRecord) bool {
	return d.AcknowledgedAttempts == 0 && d.AttemptCount < d.MaxAttempts && (d.Status == "unknown" || d.Status == "not_acknowledged" || d.Status == "stopped")
}
func webhookDeliveryViewTx(ctx context.Context, tx pgx.Tx, p domain.Principal, d webhookDeliveryRecord) (domain.WebhookDelivery, error) {
	v := d.WebhookDelivery
	if webhookManager(p) == nil && webhookRetryable(d) {
		s, err := loadWebhookSubscriptionTx(ctx, tx, p.OrganizationID, d.TaskID, d.SubscriptionID, false)
		if err != nil {
			return v, err
		}
		if err = checkWebhookConsentTx(ctx, tx, s); err == nil {
			var timely bool
			if err = tx.QueryRow(ctx, "SELECT clock_timestamp()<$1", d.ExpiresAt).Scan(&timely); err != nil {
				return v, err
			}
			if timely {
				v.AvailableCommands = []string{"retry"}
			}
		} else if !configurationAuthorityDenied(err) {
			return v, err
		}
	}
	return v, nil
}

// Lock both the subscription creator and source owner before ordinary caller
// authorization and business locks; a different admin may request redelivery.
func webhookDeliveryPreAuthorization(task, id string) func(context.Context, pgx.Tx, domain.Principal) error {
	return func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		if err := webhookManager(p); err != nil {
			return err
		}
		if err := organizationIAMGate(ctx, tx, p.OrganizationID); err != nil {
			return err
		}
		d, err := loadWebhookDeliveryTx(ctx, tx, p.OrganizationID, task, id, false)
		if err != nil {
			return err
		}
		s, err := loadWebhookSubscriptionTx(ctx, tx, p.OrganizationID, task, d.SubscriptionID, false)
		if err != nil {
			return err
		}
		return lockAuthorityPrincipalsTx(ctx, tx, p.OrganizationID, []string{p.ID, s.CreatorID, s.OwnerID})
	}
}

func (s *Store) RetryWebhookDelivery(ctx context.Context, p domain.Principal, task, id, key string, in domain.RetryWebhookDelivery) (json.RawMessage, error) {
	if !configurationIdentifier.MatchString(task) || !configurationIdentifier.MatchString(id) {
		return nil, domain.NotFound
	}
	if in.ExpectedVersion < 1 || in.ExpectedVersion > 2147483647 || !configurationIdentifier.MatchString(in.ExpectedAttemptID) || !configurationDigest.MatchString(in.EnvelopeDigest) || !in.AcknowledgePossibleDuplicate {
		return nil, domain.Err("invalid_request", "Нужны точная попытка, версия, digest и согласие на возможный дубликат", 400)
	}
	if err := domain.ValidateWebhookReason(in.Reason); err != nil {
		return nil, err
	}
	guard := func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		if err := webhookCommandGuard(task)(ctx, tx, p); err != nil {
			return err
		}
		_, err := loadWebhookDeliveryTx(ctx, tx, p.OrganizationID, task, id, false)
		return err
	}
	return s.mutateWithPreAuthorization(ctx, p, "POST /tasks/"+task+"/webhook-deliveries/"+id+"/retry", key, in, webhookDeliveryPreAuthorization(task, id), guard, func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		if err := lockWebhookTaskOrganizationTx(ctx, tx, p.OrganizationID, task); err != nil {
			return nil, err
		}
		peek, err := loadWebhookDeliveryTx(ctx, tx, p.OrganizationID, task, id, false)
		if err != nil {
			return nil, err
		}
		sub, err := loadWebhookSubscriptionTx(ctx, tx, p.OrganizationID, task, peek.SubscriptionID, true)
		if err != nil {
			return nil, err
		}
		d, err := loadWebhookDeliveryTx(ctx, tx, p.OrganizationID, task, id, true)
		if err != nil {
			return nil, err
		}
		a, err := loadWebhookAttemptTx(ctx, tx, p.OrganizationID, id, d.CurrentAttemptID, true)
		if err != nil {
			return nil, err
		}
		if d.Version != in.ExpectedVersion || d.CurrentAttemptID != in.ExpectedAttemptID || d.EnvelopeDigest != in.EnvelopeDigest || !webhookRetryable(d) || a.Status != d.Status || a.SentAt == nil && a.Status != "stopped" {
			return nil, domain.Err("conflict", "Состояние доставки изменилось или повтор недоступен", 409)
		}
		if err = checkWebhookConsentTx(ctx, tx, sub); err != nil {
			return nil, err
		}
		var now time.Time
		if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
			return nil, err
		}
		if !now.Before(d.ExpiresAt) {
			return nil, domain.Forbidden
		}
		next := d.AttemptCount + 1
		attempt := webhookAttemptID(d.ID, next)
		if _, err = tx.Exec(ctx, `UPDATE webhook_deliveries SET status='queued',attempt_count=$4,current_attempt_id=$3,version=version+1 WHERE organization_id=$1 AND id=$2`, p.OrganizationID, id, attempt, next); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO webhook_attempts(organization_id,id,task_id,delivery_id,number,created_at) VALUES($1,$2,$3,$4,$5,$6)`, p.OrganizationID, attempt, task, id, next, now); err != nil {
			return nil, err
		}
		if err = appendWebhookAuditTx(ctx, tx, p.OrganizationID, task, p.ID, id, "delivery.retry_requested", "possible_duplicate_acknowledged", in.Reason); err != nil {
			return nil, err
		}
		d, err = loadWebhookDeliveryTx(ctx, tx, p.OrganizationID, task, id, false)
		if err != nil {
			return nil, err
		}
		return webhookDeliveryViewTx(ctx, tx, p, d)
	})
}
