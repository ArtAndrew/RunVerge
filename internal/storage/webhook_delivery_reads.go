// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"

	"agentplatform.local/platform/internal/domain"
)

func (s *Store) WebhookDelivery(ctx context.Context, p domain.Principal, task, id string) (domain.WebhookDelivery, error) {
	var empty domain.WebhookDelivery
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
	d, err := loadWebhookDeliveryTx(ctx, tx, p.OrganizationID, task, id, false)
	if err != nil {
		return empty, err
	}
	v, err := webhookDeliveryViewTx(ctx, tx, p, d)
	if err != nil {
		return empty, err
	}
	if err = currentActorTx(ctx, tx, p); err != nil {
		return empty, err
	}
	return v, tx.Commit(ctx)
}
func (s *Store) WebhookDeliveries(ctx context.Context, p domain.Principal, task, raw string, limit int) ([]domain.WebhookDelivery, string, error) {
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
	rows, err := tx.Query(ctx, "SELECT "+webhookDeliveryColumns+" FROM webhook_deliveries d WHERE d.organization_id=$1 AND d.task_id=$2 AND (d.created_at,d.id)<($3,$4) ORDER BY d.created_at DESC,d.id DESC LIMIT $5", p.OrganizationID, task, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	records := []webhookDeliveryRecord{}
	for rows.Next() {
		v, e := scanWebhookDelivery(rows)
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
	values := []domain.WebhookDelivery{}
	for _, record := range records {
		v, e := webhookDeliveryViewTx(ctx, tx, p, record)
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
func (s *Store) WebhookAttempts(ctx context.Context, p domain.Principal, task, delivery, raw string, limit int) ([]domain.WebhookAttempt, string, error) {
	if !configurationIdentifier.MatchString(task) || !configurationIdentifier.MatchString(delivery) {
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
	if _, err = loadWebhookDeliveryTx(ctx, tx, p.OrganizationID, task, delivery, false); err != nil {
		return nil, "", err
	}
	rows, err := tx.Query(ctx, "SELECT "+webhookAttemptColumns+" FROM webhook_attempts WHERE organization_id=$1 AND task_id=$2 AND delivery_id=$3 AND (created_at,id)<($4,$5) ORDER BY created_at DESC,id DESC LIMIT $6", p.OrganizationID, task, delivery, c.CreatedAt, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	values := []domain.WebhookAttempt{}
	for rows.Next() {
		v, e := scanWebhookAttempt(rows)
		if e != nil {
			rows.Close()
			return nil, "", e
		}
		values = append(values, v.WebhookAttempt)
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
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	if err = currentActorTx(ctx, tx, p); err != nil {
		return nil, "", err
	}
	return values, next, tx.Commit(ctx)
}
