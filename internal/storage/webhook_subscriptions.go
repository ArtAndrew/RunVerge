// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

type webhookSubscriptionRecord struct {
	domain.WebhookSubscription
	OwnerID                                   string
	CreatorAuthorityEpoch, CreatorPolicyEpoch int64
	OwnerAuthorityEpoch, OwnerPolicyEpoch     int64
	ProfileDigest                             string
}

const webhookSubscriptionColumns = "id,organization_id,task_id,creator_id,destination_id,destination_version,event_types,version,status,start_after_sequence,last_admitted_sequence,max_notifications,admitted_notifications,created_at,expires_at,owner_id,creator_authority_epoch,creator_policy_epoch,owner_authority_epoch,owner_policy_epoch,profile_digest"

func scanWebhookSubscription(row scanner) (v webhookSubscriptionRecord, err error) {
	err = row.Scan(&v.ID, &v.OrganizationID, &v.TaskID, &v.CreatorID, &v.DestinationID, &v.DestinationVersion, &v.EventTypes, &v.Version, &v.Status, &v.StartAfterSequence, &v.LastAdmittedSequence, &v.MaxNotifications, &v.AdmittedNotifications, &v.CreatedAt, &v.ExpiresAt, &v.OwnerID, &v.CreatorAuthorityEpoch, &v.CreatorPolicyEpoch, &v.OwnerAuthorityEpoch, &v.OwnerPolicyEpoch, &v.ProfileDigest)
	v.AvailableCommands = []string{}
	return v, rowExists(err)
}

// Export authority is a dedicated human-only permission. A service key's role
// ceiling or a connection-configuration grant cannot manufacture this consent.
func webhookManager(p domain.Principal) error {
	if p.Kind != domain.PrincipalHuman || !domain.PrincipalHasPermission(p, "webhooks.manage") {
		return domain.Forbidden
	}
	return nil
}

func webhookTaskOwnerTx(ctx context.Context, tx pgx.Tx, org, task string, lock bool) (string, error) {
	query := "SELECT owner_id FROM tasks WHERE organization_id=$1 AND id=$2"
	if lock {
		query += " FOR KEY SHARE"
	}
	var owner string
	err := tx.QueryRow(ctx, query, org, task).Scan(&owner)
	return owner, rowExists(err)
}

// Always before ordinary credential locks: serialize IAM writers, lock sorted
// source/actor authority. The callback takes the organization event-allocation
// gate before task/FK locks, following lockCostScopes and lockTreeTasksTx.
// Task owner is immutable.
// No source execution-status guard is borrowed:
// terminal/cancelled task events are legitimate notification subjects.
func webhookPreAuthorization(task string) func(context.Context, pgx.Tx, domain.Principal) error {
	return func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		if err := webhookManager(p); err != nil {
			return err
		}
		if err := organizationIAMGate(ctx, tx, p.OrganizationID); err != nil {
			return err
		}
		owner, err := webhookTaskOwnerTx(ctx, tx, p.OrganizationID, task, false)
		if err != nil {
			return err
		}
		if err = lockAuthorityPrincipalsTx(ctx, tx, p.OrganizationID, []string{p.ID, owner}); err != nil {
			return err
		}
		return nil
	}
}

func lockWebhookTaskOrganizationTx(ctx context.Context, tx pgx.Tx, org, task string) error {
	if err := lockConfigurationOrganizationOnlyTx(ctx, tx, org); err != nil {
		return err
	}
	if _, err := webhookTaskOwnerTx(ctx, tx, org, task, true); err != nil {
		return err
	}
	return nil
}

func webhookCommandGuard(task string) func(context.Context, pgx.Tx, domain.Principal) error {
	return func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		if err := webhookManager(p); err != nil {
			return err
		}
		_, err := webhookTaskOwnerTx(ctx, tx, p.OrganizationID, task, false)
		return err
	}
}

func loadWebhookSubscriptionTx(ctx context.Context, tx pgx.Tx, org, task, id string, lock bool) (webhookSubscriptionRecord, error) {
	q := "SELECT " + webhookSubscriptionColumns + " FROM webhook_subscriptions WHERE organization_id=$1 AND task_id=$2 AND id=$3"
	if lock {
		q += " FOR UPDATE"
	}
	return scanWebhookSubscription(tx.QueryRow(ctx, q, org, task, id))
}

// Callers hold the relevant authority locks before business locks when using
// this as an admission decision. Views call it only as a current observation.
func checkWebhookConsentTx(ctx context.Context, tx pgx.Tx, s webhookSubscriptionRecord) error {
	creator, ce, cp, err := currentConfigurationPrincipalTx(ctx, tx, s.OrganizationID, s.CreatorID)
	if err != nil {
		return err
	}
	if webhookManager(creator) != nil || ce != s.CreatorAuthorityEpoch || cp != s.CreatorPolicyEpoch {
		return domain.Forbidden
	}
	owner, oe, op, err := currentConfigurationPrincipalTx(ctx, tx, s.OrganizationID, s.OwnerID)
	if err != nil {
		return err
	}
	if owner.Role == "viewer" || oe != s.OwnerAuthorityEpoch || op != s.OwnerPolicyEpoch {
		return domain.Forbidden
	}
	actual, err := webhookTaskOwnerTx(ctx, tx, s.OrganizationID, s.TaskID, false)
	if err != nil {
		return err
	}
	if actual != s.OwnerID {
		return domain.Forbidden
	}
	p, digest, err := loadWebhookProfileTx(ctx, tx, s.OrganizationID, s.DestinationID, s.DestinationVersion)
	if err != nil {
		return err
	}
	if !p.Destination.Active || digest != s.ProfileDigest {
		return domain.Forbidden
	}
	var timely bool
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()<$1", s.ExpiresAt).Scan(&timely); err != nil {
		return err
	}
	if !timely || s.Status == "revoked" {
		return domain.Forbidden
	}
	return nil
}

func webhookSubscriptionViewTx(ctx context.Context, tx pgx.Tx, p domain.Principal, s webhookSubscriptionRecord) (domain.WebhookSubscription, error) {
	v := s.WebhookSubscription
	var timely bool
	if err := tx.QueryRow(ctx, "SELECT clock_timestamp()<$1", s.ExpiresAt).Scan(&timely); err != nil {
		return v, err
	}
	if s.Status != "revoked" {
		if !timely {
			v.Status = "expired"
		} else if err := checkWebhookConsentTx(ctx, tx, s); err != nil {
			if !configurationAuthorityDenied(err) {
				return v, err
			}
			v.Status = "invalidated"
		}
		if webhookManager(p) == nil {
			v.AvailableCommands = []string{"revoke"}
		}
	}
	return v, nil
}

func appendWebhookAuditTx(ctx context.Context, tx pgx.Tx, org, task, actor, object, action, code, reason string) error {
	digest := ""
	if reason != "" {
		digest = Hash([]byte(reason))
	}
	_, err := tx.Exec(ctx, `INSERT INTO webhook_audit(organization_id,id,task_id,actor_id,object_id,action,reason_code,reason_digest) VALUES($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8)`, org, domain.NewID("whaudit"), task, actor, object, action, code, digest)
	return err
}

func (s *Store) CreateWebhookSubscription(ctx context.Context, p domain.Principal, task, key string, in domain.CreateWebhookSubscription) (json.RawMessage, error) {
	if !configurationIdentifier.MatchString(task) {
		return nil, domain.NotFound
	}
	if err := domain.ValidateCreateWebhookSubscription(in); err != nil {
		return nil, err
	}
	return s.mutateWithPreAuthorization(ctx, p, "POST /tasks/"+task+"/webhook-subscriptions", key, in, webhookPreAuthorization(task), webhookCommandGuard(task), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		if err := lockWebhookTaskOrganizationTx(ctx, tx, p.OrganizationID, task); err != nil {
			return nil, err
		}
		ownerID, err := webhookTaskOwnerTx(ctx, tx, p.OrganizationID, task, false)
		if err != nil {
			return nil, err
		}
		_, ce, cp, err := currentConfigurationPrincipalTx(ctx, tx, p.OrganizationID, p.ID)
		if err != nil {
			return nil, err
		}
		owner, oe, op, err := currentConfigurationPrincipalTx(ctx, tx, p.OrganizationID, ownerID)
		if err != nil {
			return nil, err
		}
		if owner.Role == "viewer" {
			return nil, domain.Forbidden
		}
		profile, digest, err := loadWebhookProfileTx(ctx, tx, p.OrganizationID, in.DestinationID, in.DestinationVersion)
		if err != nil {
			return nil, err
		}
		if !profile.Destination.Active {
			return nil, domain.Forbidden
		}
		var now time.Time
		if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
			return nil, err
		}
		var orgCount, taskCount int
		if err = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE task_id=$2) FROM webhook_subscriptions WHERE organization_id=$1 AND status='active' AND expires_at>$3`, p.OrganizationID, task, now).Scan(&orgCount, &taskCount); err != nil {
			return nil, err
		}
		if orgCount >= 32 || taskCount >= 8 {
			return nil, domain.Err("invalid_transition", "Достигнут предел активных подписок", 409)
		}
		var after int64
		if err = tx.QueryRow(ctx, "SELECT COALESCE(max(sequence),0) FROM events WHERE organization_id=$1", p.OrganizationID).Scan(&after); err != nil {
			return nil, err
		}
		kinds := append([]string{}, in.EventTypes...)
		sort.Strings(kinds)
		v, err := scanWebhookSubscription(tx.QueryRow(ctx, `INSERT INTO webhook_subscriptions(organization_id,id,task_id,creator_id,owner_id,creator_authority_epoch,creator_policy_epoch,owner_authority_epoch,owner_policy_epoch,destination_id,destination_version,profile_digest,event_types,start_after_sequence,max_notifications,created_at,expires_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) RETURNING `+webhookSubscriptionColumns, p.OrganizationID, domain.NewID("whs"), task, p.ID, ownerID, ce, cp, oe, op, in.DestinationID, in.DestinationVersion, digest, kinds, after, in.MaxNotifications, now, now.Add(time.Duration(in.TTLSeconds)*time.Second)))
		if err != nil {
			return nil, err
		}
		if err = checkWebhookConsentTx(ctx, tx, v); err != nil {
			return nil, err
		}
		if err = appendWebhookAuditTx(ctx, tx, p.OrganizationID, task, p.ID, v.ID, "subscription.created", "metadata_export_acknowledged", in.Reason); err != nil {
			return nil, err
		}
		return webhookSubscriptionViewTx(ctx, tx, p, v)
	})
}

func (s *Store) RevokeWebhookSubscription(ctx context.Context, p domain.Principal, task, id, key string, in domain.RevokeWebhookSubscription) (json.RawMessage, error) {
	if !configurationIdentifier.MatchString(task) || !configurationIdentifier.MatchString(id) {
		return nil, domain.NotFound
	}
	if in.ExpectedVersion < 1 || in.ExpectedVersion > 2147483647 {
		return nil, domain.Err("invalid_request", "Нужна точная версия подписки", 400)
	}
	if err := domain.ValidateWebhookReason(in.Reason); err != nil {
		return nil, err
	}
	guard := func(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
		if err := webhookCommandGuard(task)(ctx, tx, p); err != nil {
			return err
		}
		_, err := loadWebhookSubscriptionTx(ctx, tx, p.OrganizationID, task, id, false)
		return err
	}
	return s.mutateWithPreAuthorization(ctx, p, "POST /tasks/"+task+"/webhook-subscriptions/"+id+"/revoke", key, in, webhookPreAuthorization(task), guard, func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		if err := lockWebhookTaskOrganizationTx(ctx, tx, p.OrganizationID, task); err != nil {
			return nil, err
		}
		v, err := loadWebhookSubscriptionTx(ctx, tx, p.OrganizationID, task, id, true)
		if err != nil {
			return nil, err
		}
		if v.Version != in.ExpectedVersion || v.Status == "revoked" {
			return nil, domain.Err("conflict", "Версия или состояние подписки изменились", 409)
		}
		v, err = scanWebhookSubscription(tx.QueryRow(ctx, "UPDATE webhook_subscriptions SET status='revoked',version=version+1 WHERE organization_id=$1 AND task_id=$2 AND id=$3 RETURNING "+webhookSubscriptionColumns, p.OrganizationID, task, id))
		if err != nil {
			return nil, err
		}
		// Sent and uncertain attempts are history, never recast as proven unsent.
		// Worker claim/final-marker paths use the same organization gate.
		if _, err = tx.Exec(ctx, `UPDATE webhook_attempts a SET status='stopped',reason_code='subscription_revoked',worker_id=NULL,lease_until=NULL
 FROM webhook_deliveries d WHERE a.organization_id=$1 AND d.organization_id=a.organization_id AND d.id=a.delivery_id AND d.subscription_id=$2 AND a.sent_at IS NULL AND a.status IN('queued','leased')`, p.OrganizationID, id); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(ctx, `UPDATE webhook_deliveries d SET status='stopped',version=version+1 WHERE organization_id=$1 AND subscription_id=$2 AND status IN('queued','leased')
 AND EXISTS(SELECT 1 FROM webhook_attempts a WHERE a.organization_id=d.organization_id AND a.id=d.current_attempt_id AND a.sent_at IS NULL AND a.status='stopped')`, p.OrganizationID, id); err != nil {
			return nil, err
		}
		if err = appendWebhookAuditTx(ctx, tx, p.OrganizationID, task, p.ID, id, "subscription.revoked", "explicit_revocation", in.Reason); err != nil {
			return nil, err
		}
		return webhookSubscriptionViewTx(ctx, tx, p, v)
	})
}

func (s *Store) WebhookSubscription(ctx context.Context, p domain.Principal, task, id string) (domain.WebhookSubscription, error) {
	var empty domain.WebhookSubscription
	if !configurationIdentifier.MatchString(task) || !configurationIdentifier.MatchString(id) {
		return empty, domain.NotFound
	}
	tx, p, err := beginMemberRead(ctx, s, p, false)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	if err = webhookCommandGuard(task)(ctx, tx, p); err != nil {
		return empty, err
	}
	v, err := loadWebhookSubscriptionTx(ctx, tx, p.OrganizationID, task, id, false)
	if err != nil {
		return empty, err
	}
	view, err := webhookSubscriptionViewTx(ctx, tx, p, v)
	if err != nil {
		return empty, err
	}
	if err = currentActorTx(ctx, tx, p); err != nil {
		return empty, err
	}
	return view, tx.Commit(ctx)
}
