// SPDX-License-Identifier: Elastic-2.0
package domain

import "time"

const MaxAutomationTTLSeconds = 86400

type AutomationToolPin struct {
	ToolID            string `json:"tool_id"`
	ToolVersion       int    `json:"tool_version"`
	ToolProfileDigest string `json:"tool_profile_digest"`
}

// A task-scoped, explicit delegation. It contains no login token or credential.
// Renewing it does not reset the task's cumulative automation usage.
type TaskAutomationGrant struct {
	ID                     string              `json:"id"`
	OrganizationID         string              `json:"organization_id"`
	TaskID                 string              `json:"task_id"`
	PrincipalID            string              `json:"principal_id"`
	OwnerID                string              `json:"owner_id"`
	AgentID                string              `json:"agent_id"`
	AgentVersion           int                 `json:"agent_version"`
	ModelConnectionID      string              `json:"model_connection_id"`
	ModelConnectionVersion int                 `json:"model_connection_version"`
	Version                int                 `json:"version"`
	Status                 string              `json:"status"`
	Mode                   string              `json:"mode"`
	ToolPins               []AutomationToolPin `json:"tool_pins"`
	MaxModelTurns          int                 `json:"max_model_turns"`
	MaxToolExecutions      int                 `json:"max_tool_executions"`
	MaxChargeMicroUSD      int64               `json:"max_charge_microusd,string"`
	Reason                 string              `json:"reason"`
	StopReason             string              `json:"stop_reason,omitempty"`
	CreatedAt              time.Time           `json:"created_at"`
	ExpiresAt              time.Time           `json:"expires_at"`
}

type CreateTaskAutomation struct {
	ExpectedTaskVersion int                 `json:"expected_task_version"`
	Mode                string              `json:"mode"`
	ToolPins            []AutomationToolPin `json:"tool_pins"`
	MaxModelTurns       int                 `json:"max_model_turns"`
	MaxToolExecutions   int                 `json:"max_tool_executions"`
	MaxChargeMicroUSD   int64               `json:"max_charge_microusd,string"`
	ExpiresInSeconds    int                 `json:"expires_in_seconds"`
	AcknowledgeCost     bool                `json:"acknowledge_cost"`
	Reason              string              `json:"reason"`
}

type RevokeTaskAutomation struct {
	GrantID         string `json:"grant_id"`
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason"`
}

type AutomationUsage struct {
	ModelTurns        int   `json:"model_turns"`
	ToolExecutions    int   `json:"tool_executions"`
	AllocatedMicroUSD int64 `json:"allocated_microusd,string"`
}

// Signals explain a conservative automation stop; they do not prove a lie.
type ProgressSignal struct {
	ID              string    `json:"id"`
	OrganizationID  string    `json:"organization_id"`
	TaskID          string    `json:"task_id"`
	GrantID         string    `json:"grant_id"`
	Code            string    `json:"code"`
	DetectorVersion string    `json:"detector_version"`
	Fingerprint     string    `json:"fingerprint,omitempty"`
	EvidenceIDs     []string  `json:"evidence_ids"`
	SnapshotDigest  string    `json:"snapshot_digest"`
	CreatedAt       time.Time `json:"created_at"`
}

type TaskAutomationState struct {
	Grant             *TaskAutomationGrant `json:"grant,omitempty"`
	Usage             AutomationUsage      `json:"usage"`
	Signals           []ProgressSignal     `json:"signals"`
	AvailableCommands []string             `json:"available_commands"`
}
