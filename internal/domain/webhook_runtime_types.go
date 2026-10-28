// SPDX-License-Identifier: Elastic-2.0
package domain

import "time"

// Trusted internal worker protocol; never generated into public API schemas.
type WebhookLease struct {
	OrganizationID, TaskID, SubscriptionID, DeliveryID, AttemptID string
	DestinationID, ProfileDigest, EnvelopeDigest, WorkerID        string
	DestinationVersion                                            int
	Token                                                         int64
	LeaseUntil                                                    time.Time
	Envelope                                                      []byte
}

type WebhookDispatchPermit struct {
	OrganizationID, TaskID, SubscriptionID, DeliveryID, AttemptID string
	ProfileDigest, EnvelopeDigest, WorkerID                       string
	Token                                                         int64
	IssuedAt, NotAfter                                            time.Time
	ValidFor                                                      time.Duration
	SignatureTimestamp                                            int64
}

// An observation is a trusted worker's HTTP fact, never receiver prose or a
// financial/business-result settlement. HTTPStatus zero means uncertain.
type WebhookObservation struct {
	OrganizationID, TaskID, DeliveryID, AttemptID string
	EnvelopeDigest, WorkerID                      string
	Token                                         int64
	HTTPStatus                                    int
}
