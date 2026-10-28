// SPDX-License-Identifier: Elastic-2.0
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

const MaxToolExecutionsPerTask = 32
const MaxToolExecutionsPerRoot = 128
const MaxToolArgumentsBytes = 32768

// ToolInfo is safe discovery metadata, never a network or secret configuration.
type ToolInfo struct {
	AutomationAllowed       bool      `json:"automation_allowed,omitempty"`
	ID                      string    `json:"id"`
	OrganizationID          string    `json:"organization_id"`
	Version                 int       `json:"version"`
	Name                    string    `json:"name"`
	Description             string    `json:"description"`
	Effect                  string    `json:"effect"`
	Destination             string    `json:"destination"`
	Provider                string    `json:"provider"`
	PriceVersion            string    `json:"price_version"`
	MaxChargeMicroUSD       int64     `json:"max_charge_microusd,string"`
	InputSchemaJSON         string    `json:"input_schema_json"`
	InputSchemaDigest       string    `json:"input_schema_digest"`
	RequireDistinctApprover bool      `json:"require_distinct_approver"`
	TimeoutSeconds          int       `json:"timeout_seconds"`
	MaxInputBytes           int       `json:"max_input_bytes"`
	MaxOutputBytes          int       `json:"max_output_bytes"`
	Active                  bool      `json:"active"`
	CreatedAt               time.Time `json:"created_at"`
}

// ToolConnection is trusted-host configuration. Only Tool is public.
type ToolConnection struct {
	TrustedCAPEM          string   `json:"trusted_ca_pem,omitempty"`
	Tool                  ToolInfo `json:"tool"`
	Transport             string   `json:"transport"`
	ResultContract        string   `json:"result_contract"`
	Endpoint              string   `json:"endpoint"`
	RemoteTool            string   `json:"remote_tool,omitempty"`
	CredentialEnvironment string   `json:"credential_environment,omitempty"`
	AllowPrivateNetwork   bool     `json:"allow_private_network"`
}

type ToolExecution struct {
	AutomationGrantID string    `json:"automation_grant_id,omitempty"`
	ModelProposalID   string    `json:"model_proposal_id,omitempty"`
	DispatchedBy      string    `json:"dispatched_by,omitempty"`
	OccurrenceID      string    `json:"occurrence_id"`
	ID                string    `json:"id"`
	OrganizationID    string    `json:"organization_id"`
	TaskID            string    `json:"task_id"`
	RunID             string    `json:"run_id"`
	OperationID       string    `json:"operation_id"`
	ActionID          string    `json:"action_id"`
	ReservationID     string    `json:"reservation_id"`
	ApprovalID        string    `json:"approval_id"`
	ToolID            string    `json:"tool_id"`
	ToolVersion       int       `json:"tool_version"`
	ToolProfileDigest string    `json:"tool_profile_digest"`
	CandidateID       string    `json:"candidate_id"`
	ArtifactID        string    `json:"artifact_id"`
	ArtifactVersion   int       `json:"artifact_version"`
	ContentDigest     string    `json:"content_digest"`
	SourceTaskVersion int       `json:"source_task_version"`
	ArgumentsJSON     string    `json:"arguments_json,omitempty"`
	ArgumentsDigest   string    `json:"arguments_digest"`
	IntentHash        string    `json:"intent_hash"`
	MaxChargeMicroUSD int64     `json:"max_charge_microusd,string"`
	Provider          string    `json:"provider"`
	PriceVersion      string    `json:"price_version"`
	CreatedBy         string    `json:"created_by"`
	Status            string    `json:"status"`
	Version           int       `json:"version"`
	Reason            string    `json:"reason,omitempty"`
	ResultReceiptID   string    `json:"result_receipt_id,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	ExpiresAt         time.Time `json:"expires_at"`
}

type ToolOutcomeReceipt struct {
	ID              string    `json:"id"`
	OrganizationID  string    `json:"organization_id"`
	ExecutionID     string    `json:"execution_id"`
	IntentHash      string    `json:"intent_hash"`
	Outcome         string    `json:"outcome"`
	Method          string    `json:"method"`
	EffectReference string    `json:"effect_reference"`
	ArtifactID      string    `json:"artifact_id"`
	ContentDigest   string    `json:"content_digest"`
	EvidenceDigest  string    `json:"evidence_digest"`
	PrincipalID     string    `json:"principal_id,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

type ToolExecutionView struct {
	Execution         ToolExecution       `json:"execution"`
	Tool              ToolInfo            `json:"tool"`
	Approval          Approval            `json:"approval"`
	Task              Task                `json:"task"`
	AvailableCommands []string            `json:"available_commands"`
	Receipt           *ToolOutcomeReceipt `json:"receipt,omitempty"`
}

type PrepareToolExecution struct {
	ModelProposalID     string `json:"model_proposal_id,omitempty"`
	OccurrenceID        string `json:"occurrence_id"`
	ExpectedTaskVersion int    `json:"expected_task_version"`
	CandidateID         string `json:"candidate_id"`
	ArtifactID          string `json:"artifact_id"`
	ArtifactVersion     int    `json:"artifact_version"`
	ContentDigest       string `json:"content_digest"`
	ToolID              string `json:"tool_id"`
	ToolVersion         int    `json:"tool_version"`
	ArgumentsJSON       string `json:"arguments_json"`
	ExpiresInSeconds    int    `json:"expires_in_seconds"`
}
type DecideToolExecution struct {
	ExpectedTaskVersion      int    `json:"expected_task_version"`
	ExpectedExecutionVersion int    `json:"expected_execution_version"`
	ExpectedApprovalVersion  int    `json:"expected_approval_version"`
	IntentHash               string `json:"intent_hash"`
	Decision                 string `json:"decision"`
	Reason                   string `json:"reason"`
}
type DispatchToolExecution struct {
	ExpectedTaskVersion      int    `json:"expected_task_version"`
	ExpectedExecutionVersion int    `json:"expected_execution_version"`
	ExpectedApprovalVersion  int    `json:"expected_approval_version"`
	IntentHash               string `json:"intent_hash"`
	AcknowledgeCost          bool   `json:"acknowledge_cost"`
}
type ReconcileToolExecution struct {
	ExpectedTaskVersion      int    `json:"expected_task_version"`
	ExpectedExecutionVersion int    `json:"expected_execution_version"`
	IntentHash               string `json:"intent_hash"`
	Outcome                  string `json:"outcome"`
	EffectReference          string `json:"effect_reference"`
	Result                   string `json:"result"`
	EvidenceDigest           string `json:"evidence_digest"`
	Reason                   string `json:"reason"`
}

// ToolLease fences the separate tool job, not the blocked source Operation.
type ToolLease struct {
	Execution    ToolExecution
	LeaseOwner   string
	FencingToken int64
	LeaseUntil   time.Time
}
type ToolDispatchPermit struct {
	ExecutionID   string
	ActionID      string
	ReservationID string
	IntentHash    string
	FencingToken  int64
	IssuedAt      time.Time
	NotAfter      time.Time
	MaxAge        time.Duration
}

// This trusted runtime input never supplies a financial settlement.
type ToolOutcomeEvidence struct {
	OrganizationID  string
	ExecutionID     string
	IntentHash      string
	FencingToken    int64
	Outcome         string
	Result          string
	EffectReference string
	EvidenceDigest  string
}

func ToolConnectionDigest(c ToolConnection) string {
	c.Tool.Active = false
	c.Tool.CreatedAt = time.Time{}
	body, _ := json.Marshal(c)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// ToolIntentDigest excludes only mutable observation/presentation fields.
func ToolIntentDigest(e ToolExecution) string {
	e.DispatchedBy = ""
	e.IntentHash = ""
	e.Status = ""
	e.Version = 0
	e.Reason = ""
	e.ResultReceiptID = ""
	e.CreatedAt = time.Time{}
	body, _ := json.Marshal(e)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
