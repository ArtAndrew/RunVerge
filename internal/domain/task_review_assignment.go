// SPDX-License-Identifier: Elastic-2.0
package domain

// Assignment is for a saved candidate, not permission over the task tree or
// a future candidate. TaskVersion is the current optimistic version.
type TaskReviewAssignment struct {
	CandidateID         string         `json:"candidate_id,omitempty"`
	TaskVersion         int            `json:"task_version"`
	ReviewerPrincipalID string         `json:"reviewer_principal_id,omitempty"`
	ReviewerDisplayName string         `json:"reviewer_display_name,omitempty"`
	CanAssign           bool           `json:"can_assign"`
	CanReview           bool           `json:"can_review"`
	Reviewers           []TaskReviewer `json:"reviewers"`
	MoreReviewers       bool           `json:"more_reviewers"`
}

type TaskReviewer struct {
	PrincipalID string `json:"principal_id"`
	DisplayName string `json:"display_name"`
}

type AssignTaskReviewer struct {
	ExpectedVersion     int    `json:"expected_version"`
	CandidateID         string `json:"candidate_id"`
	ReviewerPrincipalID string `json:"reviewer_principal_id"`
}
