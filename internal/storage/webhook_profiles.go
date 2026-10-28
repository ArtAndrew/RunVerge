// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/webhookruntime"
	"github.com/jackc/pgx/v5"
)

const webhookProfileColumns = "profile,profile_digest,active,created_at"

func canonicalWebhookProfile(in domain.WebhookDestinationProfile) (domain.WebhookDestinationProfile, []byte, error) {
	if err := webhookruntime.ValidateProfile(in); err != nil {
		return in, nil, domain.Err("invalid_request", "Некорректный доверенный профиль уведомлений", 400)
	}
	in.Destination.Active = false
	in.Destination.CreatedAt = time.Time{}
	raw, err := json.Marshal(in)
	if err != nil {
		return in, nil, err
	}
	return in, raw, nil
}

func scanWebhookProfile(row scanner) (p domain.WebhookDestinationProfile, digest string, err error) {
	var raw []byte
	var active bool
	var at time.Time
	if err = row.Scan(&raw, &digest, &active, &at); err != nil {
		return p, "", rowExists(err)
	}
	if len(raw) > 131072 || json.Unmarshal(raw, &p) != nil {
		return p, "", domain.Err("conflict", "Профиль уведомлений не согласован", 409)
	}
	canonical, encoded, e := canonicalWebhookProfile(p)
	if e != nil || Hash(encoded) != digest {
		return p, "", domain.Err("conflict", "Профиль уведомлений не согласован", 409)
	}
	p = canonical
	p.Destination.Active = active
	p.Destination.CreatedAt = at.UTC()
	return p, digest, nil
}

// InstallWebhookDestination is trusted host-only administration. It performs
// no resolution, key access or network call and cannot reactivate old versions.
func (s *Store) InstallWebhookDestination(ctx context.Context, in domain.WebhookDestinationProfile) (domain.WebhookDestination, error) {
	var empty domain.WebhookDestination
	canonical, raw, err := canonicalWebhookProfile(in)
	if err != nil {
		return empty, err
	}
	d := canonical.Destination
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	if err = organizationIAMGate(ctx, tx, d.OrganizationID); err != nil {
		return empty, err
	}
	if err = lockOrganizationPolicyTx(ctx, tx, d.OrganizationID); err != nil {
		return empty, err
	}
	if err = lockConfigurationOrganizationOnlyTx(ctx, tx, d.OrganizationID); err != nil {
		return empty, err
	}
	old, digest, err := scanWebhookProfile(tx.QueryRow(ctx, "SELECT "+webhookProfileColumns+" FROM webhook_destinations WHERE organization_id=$1 AND id=$2 AND version=$3 FOR UPDATE", d.OrganizationID, d.ID, d.Version))
	if err == nil {
		if digest != Hash(raw) {
			return empty, domain.Err("conflict", "Версия назначения уже содержит другой профиль", 409)
		}
		return old.Destination, tx.Commit(ctx)
	}
	if !errors.Is(err, domain.NotFound) {
		return empty, err
	}
	var latest int
	if err = tx.QueryRow(ctx, "SELECT COALESCE(max(version),0) FROM webhook_destinations WHERE organization_id=$1 AND id=$2", d.OrganizationID, d.ID).Scan(&latest); err != nil {
		return empty, err
	}
	if d.Version != latest+1 {
		return empty, domain.Err("conflict", "Нужна последовательная версия назначения", 409)
	}
	if in.Destination.Active {
		if _, err = tx.Exec(ctx, "UPDATE webhook_destinations SET active=false WHERE organization_id=$1 AND id=$2 AND active", d.OrganizationID, d.ID); err != nil {
			return empty, err
		}
	}
	created, _, err := scanWebhookProfile(tx.QueryRow(ctx, `INSERT INTO webhook_destinations(organization_id,id,version,profile,profile_digest,active)
 VALUES($1,$2,$3,$4,$5,$6) RETURNING `+webhookProfileColumns, d.OrganizationID, d.ID, d.Version, raw, Hash(raw), in.Destination.Active))
	if err != nil {
		return empty, err
	}
	if err = appendConfigurationAuditTx(ctx, tx, d.OrganizationID, "trusted_host", "", "webhook_destination", d.ID, "webhook_destination.installed", Hash(raw), "Trusted host installed exact webhook destination version "+strconv.Itoa(d.Version)); err != nil {
		return empty, err
	}
	return created.Destination, tx.Commit(ctx)
}

// RetireWebhookDestination is trusted-host-only and irreversible. No key is
// read and no network check occurs. The same organization gate linearizes it
// with final send admission; a short already-issued permit may be in flight.
func (s *Store) RetireWebhookDestination(ctx context.Context, org, id string, version int) (domain.WebhookDestination, error) {
	var empty domain.WebhookDestination
	if !configurationIdentifier.MatchString(org) || !configurationIdentifier.MatchString(id) || version < 1 || version > 2147483647 {
		return empty, domain.Err("invalid_request", "Нужно точное назначение уведомлений", 400)
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	if err = organizationIAMGate(ctx, tx, org); err != nil {
		return empty, err
	}
	if err = lockOrganizationPolicyTx(ctx, tx, org); err != nil {
		return empty, err
	}
	if err = lockWebhookFactOrganizationTx(ctx, tx, org); err != nil {
		return empty, err
	}
	p, digest, err := loadWebhookProfileTx(ctx, tx, org, id, version)
	if err != nil {
		return empty, err
	}
	if !p.Destination.Active {
		return p.Destination, tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, "UPDATE webhook_destinations SET active=false WHERE organization_id=$1 AND id=$2 AND version=$3", org, id, version); err != nil {
		return empty, err
	}
	if err = appendConfigurationAuditTx(ctx, tx, org, "trusted_host", "", "webhook_destination", id, "webhook_destination.retired", digest, "Trusted host retired exact webhook destination version "+strconv.Itoa(version)); err != nil {
		return empty, err
	}
	p.Destination.Active = false
	return p.Destination, tx.Commit(ctx)
}

func loadWebhookProfileTx(ctx context.Context, tx pgx.Tx, org, id string, version int) (domain.WebhookDestinationProfile, string, error) {
	return scanWebhookProfile(tx.QueryRow(ctx, "SELECT "+webhookProfileColumns+" FROM webhook_destinations WHERE organization_id=$1 AND id=$2 AND version=$3", org, id, version))
}
