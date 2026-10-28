// SPDX-License-Identifier: Elastic-2.0
package domain

import "time"

const (
	ConfigurationAudience           = "platform.configuration.v1"
	MaxConfigurationOperations      = 8
	MaxConfigurationScopes          = 16
	MaxConfigurationGrantDepth      = 4
	MaxConfigurationPlans           = 100
	MaxConfigurationVerifications   = 100
	MaxConfigurationGrantTTLSeconds = 86400
	MaxConfigurationPlanTTLSeconds  = 3600
)

// Configuration actions are evaluated separately from role permissions. A
// service member requires an explicit live grant; this list grants no power.
var ConfigurationActions = []string{"inspect", "plan", "validate", "apply", "verify", "delegate"}

type ConfigurationDestinationRef struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

// Only this redacted descriptor crosses the API. The target slot and exact
// destination/purpose pairing are chosen by the trusted installation owner.
type ConfigurationDestination struct {
	ID                         string    `json:"id"`
	OrganizationID             string    `json:"organization_id"`
	Version                    int       `json:"version"`
	Kind                       string    `json:"kind"`
	Name                       string    `json:"name"`
	TargetID                   string    `json:"target_id"`
	CredentialPurposeID        string    `json:"credential_purpose_id"`
	CredentialPurposeVersion   int       `json:"credential_purpose_version"`
	RequiresApproval           bool      `json:"requires_approval"`
	MaxVerifyChargeMicroUSD    int64     `json:"max_verify_charge_microusd,string"`
	Model                      string    `json:"model,omitempty"`
	DiscoveryUnmeteredApproved bool      `json:"discovery_unmetered_approved"`
	Active                     bool      `json:"active"`
	CreatedAt                  time.Time `json:"created_at"`
}

// Private host configuration. No raw key is stored; Environment is resolved
// only by the worker. EndpointDigest binds the credential purpose to a URL.
type CredentialPurposeProfile struct {
	ID                    string    `json:"id"`
	OrganizationID        string    `json:"organization_id"`
	Version               int       `json:"version"`
	Kind                  string    `json:"kind"`
	EndpointDigest        string    `json:"endpoint_digest"`
	CredentialEnvironment string    `json:"credential_environment"`
	Active                bool      `json:"active"`
	CreatedAt             time.Time `json:"created_at"`
}

type MCPConnectionProfile struct {
	Endpoint            string `json:"endpoint"`
	TrustedCAPEM        string `json:"trusted_ca_pem,omitempty"`
	AllowPrivateNetwork bool   `json:"allow_private_network"`
	TimeoutSeconds      int    `json:"timeout_seconds"`
}

type ConfigurationDestinationProfile struct {
	Destination ConfigurationDestination `json:"destination"`
	Model       *ModelConnection         `json:"model,omitempty"`
	MCP         *MCPConnectionProfile    `json:"mcp,omitempty"`
}

type ConfigurationScope struct {
	DestinationID            string `json:"destination_id"`
	DestinationVersion       int    `json:"destination_version"`
	Kind                     string `json:"kind"`
	TargetID                 string `json:"target_id"`
	CredentialPurposeID      string `json:"credential_purpose_id"`
	CredentialPurposeVersion int    `json:"credential_purpose_version"`
}

// A server record's public representation, never a bearer credential. Parent
// counters apply to descendants; issuing children never resets a shared cap.
type CapabilityEnvelope struct {
	ID                         string               `json:"id"`
	OrganizationID             string               `json:"organization_id"`
	Audience                   string               `json:"audience"`
	IssuerID                   string               `json:"issuer_id"`
	SubjectID                  string               `json:"subject_id"`
	ParentGrantID              string               `json:"parent_grant_id,omitempty"`
	Depth                      int                  `json:"depth"`
	Version                    int                  `json:"version"`
	Actions                    []string             `json:"actions"`
	Scopes                     []ConfigurationScope `json:"scopes"`
	MaxPlans                   int                  `json:"max_plans"`
	UsedPlans                  int                  `json:"used_plans"`
	MaxVerifications           int                  `json:"max_verifications"`
	UsedVerifications          int                  `json:"used_verifications"`
	MaxAllocatedChargeMicroUSD int64                `json:"max_allocated_charge_microusd,string"`
	AllocatedChargeMicroUSD    int64                `json:"allocated_charge_microusd,string"`
	Revoked                    bool                 `json:"revoked"`
	Status                     string               `json:"status"`
	CreatedAt                  time.Time            `json:"created_at"`
	ExpiresAt                  time.Time            `json:"expires_at"`
	AvailableCommands          []string             `json:"available_commands"`
}

type CreateConfigurationGrant struct {
	SubjectID                  string                        `json:"subject_id"`
	ParentGrantID              string                        `json:"parent_grant_id,omitempty"`
	ExpectedParentVersion      int                           `json:"expected_parent_version,omitempty"`
	Actions                    []string                      `json:"actions"`
	Destinations               []ConfigurationDestinationRef `json:"destinations"`
	TTLSeconds                 int                           `json:"ttl_seconds"`
	MaxPlans                   int                           `json:"max_plans"`
	MaxVerifications           int                           `json:"max_verifications"`
	MaxAllocatedChargeMicroUSD int64                         `json:"max_allocated_charge_microusd,string"`
	Reason                     string                        `json:"reason"`
}

type RevokeConfigurationGrant struct {
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason"`
}

type ConfigurationCatalog struct {
	OrganizationID     string   `json:"organization_id"`
	Revision           int64    `json:"revision,string"`
	Audience           string   `json:"audience"`
	Actions            []string `json:"actions"`
	CanIssueRootGrants bool     `json:"can_issue_root_grants"`
	CanApprove         bool     `json:"can_approve"`
	MaxOperations      int      `json:"max_operations"`
	MaxGrantDepth      int      `json:"max_grant_depth"`
	AvailableCommands  []string `json:"available_commands"`
}

type ConfigurationOperationInput struct {
	ID                      string `json:"id"`
	DestinationID           string `json:"destination_id"`
	DestinationVersion      int    `json:"destination_version"`
	ExpectedResourceVersion int    `json:"expected_resource_version"`
	Active                  *bool  `json:"active"`
}

type ConfigurationOperation struct {
	ID                      string             `json:"id"`
	Scope                   ConfigurationScope `json:"scope"`
	ExpectedResourceVersion int                `json:"expected_resource_version"`
	Active                  bool               `json:"active"`
}

type ConfigurationDiff struct {
	OperationID        string `json:"operation_id"`
	Kind               string `json:"kind"`
	TargetID           string `json:"target_id"`
	ExpectedVersion    int    `json:"expected_version"`
	NewVersion         int    `json:"new_version"`
	Active             bool   `json:"active"`
	RetiredVersions    []int  `json:"retired_versions"`
	MayStopPinnedTasks bool   `json:"may_stop_pinned_tasks"`
}

type CreateConfigurationPlan struct {
	GrantID      string                        `json:"grant_id,omitempty"`
	BaseRevision int64                         `json:"base_revision,string"`
	TTLSeconds   int                           `json:"ttl_seconds"`
	Operations   []ConfigurationOperationInput `json:"operations"`
	Reason       string                        `json:"reason"`
}

// Intent, digest, actor, scope and expiry never change. Version/status describe
// the one durable application; validation/approval are separate immutable rows.
type ConfigurationPlan struct {
	ID                string                   `json:"id"`
	OrganizationID    string                   `json:"organization_id"`
	ActorID           string                   `json:"actor_id"`
	GrantID           string                   `json:"grant_id,omitempty"`
	Version           int                      `json:"version"`
	Status            string                   `json:"status"`
	BaseRevision      int64                    `json:"base_revision,string"`
	Digest            string                   `json:"digest"`
	Operations        []ConfigurationOperation `json:"operations"`
	Diff              []ConfigurationDiff      `json:"diff"`
	Reason            string                   `json:"reason"`
	CreatedAt         time.Time                `json:"created_at"`
	ExpiresAt         time.Time                `json:"expires_at"`
	AppliedAt         *time.Time               `json:"applied_at,omitempty"`
	AppliedRevision   int64                    `json:"applied_revision,string"`
	ReceiptID         string                   `json:"receipt_id,omitempty"`
	AvailableCommands []string                 `json:"available_commands"`
}

// Explanation at one instant, not a reusable dispatch permit. Admission errors
// keep the ordinary safe403/404; authorized static validation returns this DTO.
type PolicyDecision struct {
	ID                    string    `json:"id"`
	OrganizationID        string    `json:"organization_id"`
	Action                string    `json:"action"`
	TargetID              string    `json:"target_id"`
	Outcome               string    `json:"outcome"`
	ReasonCodes           []string  `json:"reason_codes"`
	GrantID               string    `json:"grant_id,omitempty"`
	ConfigurationRevision int64     `json:"configuration_revision,string"`
	Obligations           []string  `json:"obligations"`
	EvaluatedAt           time.Time `json:"evaluated_at"`
}

type ValidateConfigurationPlan struct {
	ExpectedVersion int    `json:"expected_version"`
	PlanDigest      string `json:"plan_digest"`
}

type ConfigurationValidation struct {
	ID              string              `json:"id"`
	OrganizationID  string              `json:"organization_id"`
	PlanID          string              `json:"plan_id"`
	PlanVersion     int                 `json:"plan_version"`
	PlanDigest      string              `json:"plan_digest"`
	BaseRevision    int64               `json:"base_revision,string"`
	CurrentRevision int64               `json:"current_revision,string"`
	Valid           bool                `json:"valid"`
	Decision        PolicyDecision      `json:"decision"`
	Diff            []ConfigurationDiff `json:"diff"`
	CreatedAt       time.Time           `json:"created_at"`
}

type ApproveConfigurationPlan struct {
	ExpectedVersion int    `json:"expected_version"`
	PlanDigest      string `json:"plan_digest"`
	ValidationID    string `json:"validation_id"`
	Reason          string `json:"reason"`
}

type ConfigurationApproval struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	PlanID         string    `json:"plan_id"`
	PlanDigest     string    `json:"plan_digest"`
	ValidationID   string    `json:"validation_id"`
	PrincipalID    string    `json:"principal_id"`
	Reason         string    `json:"reason"`
	CreatedAt      time.Time `json:"created_at"`
}

type ApplyConfigurationPlan struct {
	ExpectedVersion int    `json:"expected_version"`
	PlanDigest      string `json:"plan_digest"`
	ValidationID    string `json:"validation_id"`
	Reason          string `json:"reason"`
}

type StartConfigurationVerification struct {
	ExpectedPlanVersion int    `json:"expected_plan_version"`
	PlanDigest          string `json:"plan_digest"`
	OperationID         string `json:"operation_id"`
	MaxChargeMicroUSD   int64  `json:"max_charge_microusd,string"`
	AcknowledgeCost     *bool  `json:"acknowledge_cost"`
	Reason              string `json:"reason"`
}

type ConfigurationVerification struct {
	ID                string             `json:"id"`
	OrganizationID    string             `json:"organization_id"`
	PlanID            string             `json:"plan_id"`
	OperationID       string             `json:"operation_id"`
	ActorID           string             `json:"actor_id"`
	GrantID           string             `json:"grant_id,omitempty"`
	Scope             ConfigurationScope `json:"scope"`
	ResourceVersion   int                `json:"resource_version"`
	Kind              string             `json:"kind"`
	Status            string             `json:"status"`
	Version           int                `json:"version"`
	TaskID            string             `json:"task_id,omitempty"`
	MaxChargeMicroUSD int64              `json:"max_charge_microusd,string"`
	CostStatus        string             `json:"cost_status"`
	ProtocolVersion   string             `json:"protocol_version,omitempty"`
	ToolCount         int                `json:"tool_count"`
	CatalogDigest     string             `json:"catalog_digest,omitempty"`
	ResponseDigest    string             `json:"response_digest,omitempty"`
	ReasonCode        string             `json:"reason_code,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
}

type ConfigurationResource struct {
	OrganizationID string             `json:"organization_id"`
	Kind           string             `json:"kind"`
	ID             string             `json:"id"`
	Version        int                `json:"version"`
	Scope          ConfigurationScope `json:"scope"`
	Active         bool               `json:"active"`
	CreatedAt      time.Time          `json:"created_at"`
}

type ConfigurationAuditEntry struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	ActorType      string    `json:"actor_type"`
	ActorID        string    `json:"actor_id,omitempty"`
	ObjectType     string    `json:"object_type"`
	ObjectID       string    `json:"object_id"`
	Action         string    `json:"action"`
	Digest         string    `json:"digest"`
	Reason         string    `json:"reason"`
	CreatedAt      time.Time `json:"created_at"`
}

// Worker-only inputs; they are not registered as public JSON DTOs.
type ConfigurationVerificationLease struct {
	OrganizationID, ID, PlanID, OperationID, WorkerID string
	Scope                                             ConfigurationScope
	ResourceVersion                                   int
	ProfileDigest                                     string
	Token                                             int64
	LeaseUntil                                        time.Time
}

type ConfigurationProbeConfig struct {
	Destination ConfigurationDestinationProfile
	Purpose     CredentialPurposeProfile
}

type ConfigurationDispatchPermit struct {
	OrganizationID, VerificationID, WorkerID string
	Scope                                    ConfigurationScope
	ResourceVersion                          int
	ProfileDigest                            string
	FencingToken                             int64
	IssuedAt, NotAfter                       time.Time
	ValidFor                                 time.Duration
}

type ConfigurationProbeObservation struct {
	OrganizationID, VerificationID, WorkerID                string
	FencingToken                                            int64
	TaskID, OperationID, ReservationID, IntentDigest        string
	Outcome, ProtocolVersion, CatalogDigest, ResponseDigest string
	ToolCount                                               int
}
