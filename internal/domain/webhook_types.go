// SPDX-License-Identifier: Elastic-2.0
package domain

import "time"

const (
	WebhookEnvelopeVersion  = 1
	MaxWebhookEventTypes    = 16
	MaxWebhookNotifications = 1024
	MaxWebhookAttempts      = 8
	MaxWebhookTTLSeconds    = 86400
	MaxWebhookEnvelopeBytes = 4096
)

// WebhookDestination is the public descriptor of a trusted-host installed
// receiver. Its key identity is public; key bytes, env ref, URL and CA are not.
type WebhookDestination struct {
	ID                string    `json:"id"`
	OrganizationID    string    `json:"organization_id"`
	Version           int       `json:"version"`
	Name              string    `json:"name"`
	SigningKeyID      string    `json:"signing_key_id"`
	UnmeteredApproved bool      `json:"unmetered_approved"`
	Active            bool      `json:"active"`
	CreatedAt         time.Time `json:"created_at"`
}

// Private host profile. A dedicated encoded secret is resolved only by workers.
type WebhookDestinationProfile struct {
	Destination           WebhookDestination `json:"destination"`
	Endpoint              string             `json:"endpoint"`
	SigningKeyEnvironment string             `json:"signing_key_environment"`
	TrustedCAPEM          string             `json:"trusted_ca_pem,omitempty"`
	AllowPrivateNetwork   bool               `json:"allow_private_network"`
	TimeoutSeconds        int                `json:"timeout_seconds"`
}

type CreateWebhookSubscription struct {
	DestinationID             string   `json:"destination_id"`
	DestinationVersion        int      `json:"destination_version"`
	EventTypes                []string `json:"event_types"`
	TTLSeconds                int      `json:"ttl_seconds"`
	MaxNotifications          int      `json:"max_notifications"`
	AcknowledgeMetadataExport bool     `json:"acknowledge_metadata_export"`
	Reason                    string   `json:"reason"`
}

type RevokeWebhookSubscription struct {
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason"`
}

type WebhookSubscription struct {
	ID                    string    `json:"id"`
	OrganizationID        string    `json:"organization_id"`
	TaskID                string    `json:"task_id"`
	CreatorID             string    `json:"creator_id"`
	DestinationID         string    `json:"destination_id"`
	DestinationVersion    int       `json:"destination_version"`
	EventTypes            []string  `json:"event_types"`
	Version               int       `json:"version"`
	Status                string    `json:"status"`
	StartAfterSequence    int64     `json:"start_after_sequence,string"`
	LastAdmittedSequence  int64     `json:"last_admitted_sequence,string"`
	MaxNotifications      int       `json:"max_notifications"`
	AdmittedNotifications int       `json:"admitted_notifications"`
	CreatedAt             time.Time `json:"created_at"`
	ExpiresAt             time.Time `json:"expires_at"`
	AvailableCommands     []string  `json:"available_commands"`
}

// WebhookEventMetadata never contains Event.Data or legacy JSONB text.
type WebhookEventMetadata struct {
	ID             string    `json:"id"`
	TaskID         string    `json:"task_id"`
	Sequence       int64     `json:"sequence,string"`
	Type           string    `json:"type"`
	PayloadVersion int       `json:"payload_version"`
	CreatedAt      time.Time `json:"created_at"`
}

type WebhookEnvelope struct {
	EnvelopeVersion int                  `json:"envelope_version"`
	DeliveryID      string               `json:"delivery_id"`
	OrganizationID  string               `json:"organization_id"`
	SubscriptionID  string               `json:"subscription_id"`
	Event           WebhookEventMetadata `json:"event"`
}

type WebhookDelivery struct {
	ID                   string    `json:"id"`
	OrganizationID       string    `json:"organization_id"`
	TaskID               string    `json:"task_id"`
	SubscriptionID       string    `json:"subscription_id"`
	DestinationID        string    `json:"destination_id"`
	DestinationVersion   int       `json:"destination_version"`
	EventID              string    `json:"event_id"`
	EventSequence        int64     `json:"event_sequence,string"`
	EnvelopeDigest       string    `json:"envelope_digest"`
	Version              int       `json:"version"`
	Status               string    `json:"status"`
	AttemptCount         int       `json:"attempt_count"`
	UnknownAttempts      int       `json:"unknown_attempts"`
	EverUnknownAttempts  int       `json:"ever_unknown_attempts"`
	AcknowledgedAttempts int       `json:"acknowledged_attempts"`
	MaxAttempts          int       `json:"max_attempts"`
	CurrentAttemptID     string    `json:"current_attempt_id"`
	CreatedAt            time.Time `json:"created_at"`
	ExpiresAt            time.Time `json:"expires_at"`
	AvailableCommands    []string  `json:"available_commands"`
}

type WebhookAttempt struct {
	ID             string     `json:"id"`
	OrganizationID string     `json:"organization_id"`
	TaskID         string     `json:"task_id"`
	DeliveryID     string     `json:"delivery_id"`
	Number         int        `json:"number"`
	Status         string     `json:"status"`
	HTTPStatus     int        `json:"http_status"`
	ReasonCode     string     `json:"reason_code,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	SentAt         *time.Time `json:"sent_at,omitempty"`
	ObservedAt     *time.Time `json:"observed_at,omitempty"`
	UnknownAt      *time.Time `json:"unknown_at,omitempty"`
}

type RetryWebhookDelivery struct {
	ExpectedVersion              int    `json:"expected_version"`
	ExpectedAttemptID            string `json:"expected_attempt_id"`
	EnvelopeDigest               string `json:"envelope_digest"`
	AcknowledgePossibleDuplicate bool   `json:"acknowledge_possible_duplicate"`
	Reason                       string `json:"reason"`
}
