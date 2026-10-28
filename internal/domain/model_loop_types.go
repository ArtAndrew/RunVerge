// SPDX-License-Identifier: Elastic-2.0
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

const MaxModelToolCalls = 8
const MaxModelTools = 16
const MaxModelTurnsPerTask = 32
const MaxModelTurnsPerRoot = 128

// A legacy 900KB run input becomes a quoted JSON string inside the transcript.
// Preserve it on upgrade; actual model wire requests remain separately capped.
const MaxModelTranscriptBytes = 2 << 20

// ModelToolDefinition is the immutable public part of an installed tool. It
// deliberately contains no endpoint, secret reference or certificate.
type ModelToolDefinition struct {
	ToolID            string `json:"tool_id"`
	ToolVersion       int    `json:"tool_version"`
	ToolProfileDigest string `json:"tool_profile_digest"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	InputSchemaJSON   string `json:"input_schema_json"`
}

func ModelToolName(org, id string, version int) string {
	sum := sha256.Sum256([]byte(org + "/" + id + "/" + strconv.Itoa(version)))
	return "t_" + hex.EncodeToString(sum[:16])
}

type ModelToolCall struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ArgumentsJSON string `json:"arguments_json"`
}

// Tool results are quoted as role=tool, never promoted to system instructions.
type ModelMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []ModelToolCall `json:"tool_calls,omitempty"`
}

// List responses omit ArgumentsJSON; exact reads expose the original bytes.
type ModelToolProposal struct {
	ID                string    `json:"id"`
	OrganizationID    string    `json:"organization_id"`
	TaskID            string    `json:"task_id"`
	RunID             string    `json:"run_id"`
	OperationID       string    `json:"operation_id"`
	CandidateID       string    `json:"candidate_id"`
	ArtifactID        string    `json:"artifact_id"`
	ArtifactVersion   int       `json:"artifact_version"`
	ContentDigest     string    `json:"content_digest"`
	CallID            string    `json:"call_id"`
	ToolID            string    `json:"tool_id"`
	ToolVersion       int       `json:"tool_version"`
	ToolProfileDigest string    `json:"tool_profile_digest"`
	ToolName          string    `json:"tool_name"`
	ArgumentsJSON     string    `json:"arguments_json,omitempty"`
	ArgumentsDigest   string    `json:"arguments_digest"`
	ExecutionID       string    `json:"execution_id,omitempty"`
	ExecutionStatus   string    `json:"execution_status,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

type ModelToolResultBinding struct {
	ProposalID       string `json:"proposal_id"`
	ExecutionID      string `json:"execution_id"`
	ExecutionVersion int    `json:"execution_version"`
	Status           string `json:"status"`
	ReceiptID        string `json:"receipt_id,omitempty"`
	ContentDigest    string `json:"content_digest,omitempty"`
}

type ContinueModelRequest struct {
	ExpectedTaskVersion int                      `json:"expected_task_version"`
	CandidateID         string                   `json:"candidate_id"`
	ArtifactID          string                   `json:"artifact_id"`
	ArtifactVersion     int                      `json:"artifact_version"`
	ContentDigest       string                   `json:"content_digest"`
	Results             []ModelToolResultBinding `json:"results"`
	AcknowledgeCost     bool                     `json:"acknowledge_cost"`
	Reason              string                   `json:"reason"`
}
