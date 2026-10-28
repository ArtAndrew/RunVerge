// SPDX-License-Identifier: Elastic-2.0
package domain

import (
	"encoding/json"
	"time"
)

const MaxTreeTasks = 128
const MaxTreeAttempts = 384
const MaxRunInputBytes = 900000

func ExecutionInput(l Lease) string {
	if l.RunInput != "" {
		return l.RunInput
	}
	return l.Task.Goal
}

// TaskCandidate describes an immutable result snapshot. Neither production by
// an agent nor an authorized decision proves that its factual claims are true.
type TaskCandidate struct {
	ID                     string    `json:"id"`
	OrganizationID         string    `json:"organization_id"`
	TaskID                 string    `json:"task_id"`
	RunID                  string    `json:"run_id"`
	OperationID            string    `json:"operation_id"`
	ArtifactID             string    `json:"artifact_id"`
	ArtifactVersion        int       `json:"artifact_version"`
	ContentDigest          string    `json:"content_digest"`
	Source                 string    `json:"source"`
	CreatedAt              time.Time `json:"created_at"`
	ParentCandidateID      string    `json:"parent_candidate_id,omitempty"`
	ReviewBasisCandidateID string    `json:"review_basis_candidate_id,omitempty"`
	MaterialID             string    `json:"material_id,omitempty"`
	MaterialVersion        int       `json:"material_version,omitempty"`
	PublishedBy            string    `json:"published_by,omitempty"`
}

type ReviewTaskRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	CandidateID     string `json:"candidate_id"`
	ArtifactID      string `json:"artifact_id"`
	ArtifactVersion int    `json:"artifact_version"`
	ContentDigest   string `json:"content_digest"`
	Decision        string `json:"decision"`
	Reason          string `json:"reason,omitempty"`
}

type RetryTaskRequest struct {
	ExpectedVersion    int    `json:"expected_version"`
	Reason             string `json:"reason"`
	Feedback           string `json:"feedback,omitempty"`
	AcknowledgeNewCost bool   `json:"acknowledge_new_cost"`
}

type TaskDecision struct {
	ID                string    `json:"id"`
	OrganizationID    string    `json:"organization_id"`
	TaskID            string    `json:"task_id"`
	RunID             string    `json:"run_id"`
	CandidateID       string    `json:"candidate_id"`
	ArtifactID        string    `json:"artifact_id"`
	ArtifactVersion   int       `json:"artifact_version"`
	ContentDigest     string    `json:"content_digest"`
	Decision          string    `json:"decision"`
	Reason            string    `json:"reason,omitempty"`
	PrincipalID       string    `json:"principal_id"`
	Method            string    `json:"method"`
	TaskVersionBefore int       `json:"task_version_before"`
	TaskVersionAfter  int       `json:"task_version_after"`
	ParentDecisionID  string    `json:"parent_decision_id,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

// RunSource is quoted untrusted input. Its exact UTF-8 bytes and references are
// snapshotted once, never interpreted as authority or as an accepted result.
type RunSource struct {
	TaskID          string `json:"task_id"`
	CandidateID     string `json:"candidate_id"`
	ArtifactID      string `json:"artifact_id"`
	ArtifactVersion int    `json:"artifact_version"`
	ContentDigest   string `json:"content_digest"`
	Content         string `json:"content"`
}

type RunInput struct {
	OriginalGoal  string          `json:"original_goal"`
	LocalGoal     string          `json:"local_goal,omitempty"`
	Feedback      string          `json:"feedback,omitempty"`
	ParentContext json.RawMessage `json:"parent_context,omitempty"`
	Sources       []RunSource     `json:"untrusted_sources"`
}
