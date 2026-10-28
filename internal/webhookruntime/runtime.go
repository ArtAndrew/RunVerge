// SPDX-License-Identifier: Elastic-2.0
package webhookruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"agentplatform.local/platform/internal/domain"
)

type Store interface {
	PrepareWebhookDispatch(context.Context, *domain.WebhookLease) (domain.WebhookDispatchPermit, error)
	RecordWebhookObservation(context.Context, domain.WebhookObservation) (domain.WebhookDelivery, error)
}
type Config struct {
	Profile            domain.WebhookDestinationProfile
	EncodedSigningKey  string
	PersistenceTimeout time.Duration
}

func (Config) String() string   { return "webhookruntime.Config{redacted}" }
func (Config) GoString() string { return "webhookruntime.Config{redacted}" }

type Runtime struct {
	store              Store
	profile            domain.WebhookDestinationProfile
	key                []byte
	persistenceTimeout time.Duration
}

func (*Runtime) String() string   { return "webhookruntime.Runtime{redacted}" }
func (*Runtime) GoString() string { return "webhookruntime.Runtime{redacted}" }

func failed(code string) error { return errors.New("webhook: " + code) }
func ProfileDigest(p domain.WebhookDestinationProfile) string {
	p.Destination.Active = false
	p.Destination.CreatedAt = time.Time{}
	raw, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return digest(raw)
}
func digest(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }

func New(store Store, c Config) (*Runtime, error) {
	if store == nil || ValidateProfile(c.Profile) != nil || !c.Profile.Destination.Active || c.PersistenceTimeout < time.Millisecond || c.PersistenceTimeout > 30*time.Second {
		return nil, failed("invalid_configuration")
	}
	key, err := DecodeSigningKey(c.EncodedSigningKey)
	if err != nil {
		return nil, failed("credentials_unavailable")
	}
	return &Runtime{store: store, profile: c.Profile, key: key, persistenceTimeout: c.PersistenceTimeout}, nil
}

// Execute accepts one already claimed immutable attempt. It never allocates a
// retry and never treats an ambiguous marker commit as permission to send.
func (r *Runtime) Execute(ctx context.Context, l *domain.WebhookLease) (domain.WebhookDelivery, error) {
	var empty domain.WebhookDelivery
	if ctx == nil || l == nil || ctx.Err() != nil {
		return empty, failed("invalid_execution")
	}
	v := *l
	v.Envelope = bytes.Clone(l.Envelope)
	p := r.profile.Destination
	if !safeID.MatchString(v.OrganizationID) || !safeID.MatchString(v.TaskID) || !safeID.MatchString(v.SubscriptionID) || !safeID.MatchString(v.DeliveryID) || !safeID.MatchString(v.AttemptID) || !safeID.MatchString(v.WorkerID) || v.Token < 1 || v.OrganizationID != p.OrganizationID || v.DestinationID != p.ID || v.DestinationVersion != p.Version || v.ProfileDigest != ProfileDigest(r.profile) || len(v.Envelope) == 0 || len(v.Envelope) > domain.MaxWebhookEnvelopeBytes || digest(v.Envelope) != v.EnvelopeDigest {
		return empty, failed("intent_mismatch")
	}
	started := time.Now()
	permit, err := r.store.PrepareWebhookDispatch(ctx, &v)
	if err != nil {
		r.observe(v, 0)
		return empty, failed("dispatch_not_admitted")
	}
	deadline := started.Add(permit.ValidFor)
	if permit.OrganizationID != v.OrganizationID || permit.TaskID != v.TaskID || permit.SubscriptionID != v.SubscriptionID || permit.DeliveryID != v.DeliveryID || permit.AttemptID != v.AttemptID || permit.WorkerID != v.WorkerID || permit.Token != v.Token || permit.ProfileDigest != v.ProfileDigest || permit.EnvelopeDigest != v.EnvelopeDigest || permit.IssuedAt.IsZero() || permit.ValidFor <= 0 || permit.ValidFor > 2*time.Second || permit.NotAfter.Sub(permit.IssuedAt) != permit.ValidFor || permit.NotAfter.After(v.LeaseUntil) || permit.SignatureTimestamp != permit.IssuedAt.Unix() || permit.SignatureTimestamp < 0 || !time.Now().Before(deadline) {
		r.observe(v, 0)
		return empty, failed("dispatch_permit_rejected")
	}
	headers, err := Sign(r.key, p.SigningKeyID, v.DeliveryID, v.AttemptID, v.Envelope, time.Unix(permit.SignatureTimestamp, 0))
	if err != nil {
		r.observe(v, 0)
		return empty, failed("signing_failed")
	}
	status := r.send(ctx, v.Envelope, headers, deadline)
	delivery, err := r.observe(v, status)
	if err != nil {
		return empty, failed("observation_not_committed")
	}
	if status == 0 {
		return delivery, failed("transport_outcome_unknown")
	}
	return delivery, nil
}
func (r *Runtime) observe(v domain.WebhookLease, status int) (domain.WebhookDelivery, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.persistenceTimeout)
	defer cancel()
	return r.store.RecordWebhookObservation(ctx, domain.WebhookObservation{OrganizationID: v.OrganizationID, TaskID: v.TaskID, DeliveryID: v.DeliveryID, AttemptID: v.AttemptID, WorkerID: v.WorkerID, Token: v.Token, EnvelopeDigest: v.EnvelopeDigest, HTTPStatus: status})
}
