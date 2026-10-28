// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"strconv"

	"agentplatform.local/platform/internal/domain"
)

func webhookDestinationCursorID(v domain.WebhookDestination) string {
	return Hash([]byte(v.ID + "\x1f" + strconv.Itoa(v.Version)))
}

func (s *Store) WebhookDestinations(ctx context.Context, p domain.Principal, raw string, limit int) ([]domain.WebhookDestination, string, error) {
	if limit < 1 || limit > 100 {
		return nil, "", domain.Err("invalid_request", "limit должен быть от 1 до 100", 400)
	}
	c, err := decodeCursor(raw)
	if err != nil {
		return nil, "", err
	}
	tx, p, err := beginMemberRead(ctx, s, p, false)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	if err = webhookManager(p); err != nil {
		return nil, "", err
	}
	// Read the public descriptor columns directly. No endpoint, environment
	// reference, private-network approval or trust root enters the public value.
	rows, err := tx.Query(ctx, `SELECT id,organization_id,version,profile->'destination'->>'name',profile->'destination'->>'signing_key_id',active,created_at
 FROM webhook_destinations WHERE organization_id=$1 AND (created_at,encode(sha256(convert_to(id||chr(31)||version::text,'UTF8')),'hex'))<($2,$3)
 ORDER BY created_at DESC,encode(sha256(convert_to(id||chr(31)||version::text,'UTF8')),'hex') DESC LIMIT $4`, p.OrganizationID, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	values := []domain.WebhookDestination{}
	for rows.Next() {
		var v domain.WebhookDestination
		if err = rows.Scan(&v.ID, &v.OrganizationID, &v.Version, &v.Name, &v.SigningKeyID, &v.Active, &v.CreatedAt); err != nil {
			rows.Close()
			return nil, "", err
		}
		v.UnmeteredApproved = true // immutable installation/schema invariant
		values = append(values, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(values) > limit {
		values = values[:limit]
		last := values[limit-1]
		next = encodeCursor(last.CreatedAt, webhookDestinationCursorID(last))
	}
	if err = currentActorTx(ctx, tx, p); err != nil {
		return nil, "", err
	}
	return values, next, tx.Commit(ctx)
}

func (s *Store) WebhookSubscriptions(ctx context.Context, p domain.Principal, task, raw string, limit int) ([]domain.WebhookSubscription, string, error) {
	if !configurationIdentifier.MatchString(task) {
		return nil, "", domain.NotFound
	}
	if limit < 1 || limit > 100 {
		return nil, "", domain.Err("invalid_request", "limit должен быть от 1 до 100", 400)
	}
	c, err := decodeCursor(raw)
	if err != nil {
		return nil, "", err
	}
	tx, p, err := beginMemberRead(ctx, s, p, false)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback(ctx)
	if err = webhookCommandGuard(task)(ctx, tx, p); err != nil {
		return nil, "", err
	}
	rows, err := tx.Query(ctx, "SELECT "+webhookSubscriptionColumns+" FROM webhook_subscriptions WHERE organization_id=$1 AND task_id=$2 AND (created_at,id)<($3,$4) ORDER BY created_at DESC,id DESC LIMIT $5", p.OrganizationID, task, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	records := []webhookSubscriptionRecord{}
	for rows.Next() {
		v, e := scanWebhookSubscription(rows)
		if e != nil {
			rows.Close()
			return nil, "", e
		}
		records = append(records, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(records) > limit {
		records = records[:limit]
		last := records[limit-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	values := []domain.WebhookSubscription{}
	for _, record := range records {
		v, e := webhookSubscriptionViewTx(ctx, tx, p, record)
		if e != nil {
			return nil, "", e
		}
		values = append(values, v)
	}
	if err = currentActorTx(ctx, tx, p); err != nil {
		return nil, "", err
	}
	return values, next, tx.Commit(ctx)
}
