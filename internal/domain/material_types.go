// SPDX-License-Identifier: Elastic-2.0
package domain

import "time"

const MaxMaterialsPerTask = 32
const MaxMaterialRevisions = 128
const MaxMaterialPublications = 64
const MaxMaterialContentBytes = 262144
const MaxOrganizationMaterialBytes int64 = 268435456

// Material is the mutable head of a logical document. The revisions it points
// to are append-only. A draft is not the task's accepted or proposed result.
type Material struct {
	ID                string    `json:"id"`
	OrganizationID    string    `json:"organization_id"`
	TaskID            string    `json:"task_id"`
	Name              string    `json:"name"`
	MediaType         string    `json:"media_type"`
	HeadVersion       int       `json:"head_version"`
	HeadContentDigest string    `json:"head_content_digest"`
	BaseCandidateID   string    `json:"base_candidate_id,omitempty"`
	CreatedBy         string    `json:"created_by"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type MaterialRevision struct {
	ID               string    `json:"id"`
	OrganizationID   string    `json:"organization_id"`
	TaskID           string    `json:"task_id"`
	MaterialID       string    `json:"material_id"`
	Version          int       `json:"version"`
	BaseVersion      int       `json:"base_version"`
	MergeBaseVersion *int      `json:"merge_base_version,omitempty"`
	ContentDigest    string    `json:"content_digest"`
	PrincipalID      string    `json:"principal_id"`
	Reason           string    `json:"reason,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

type MaterialContent struct {
	Revision MaterialRevision `json:"revision"`
	Content  string           `json:"content"`
}

type CreateMaterial struct {
	Name            string `json:"name"`
	MediaType       string `json:"media_type"`
	Content         string `json:"content"`
	BaseCandidateID string `json:"base_candidate_id,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

type ReviseMaterial struct {
	ExpectedVersion  int    `json:"expected_version"`
	Content          string `json:"content"`
	Reason           string `json:"reason"`
	MergeBaseVersion *int   `json:"merge_base_version,omitempty"`
}

type PublishMaterial struct {
	ExpectedTaskVersion     int    `json:"expected_task_version"`
	ExpectedMaterialVersion int    `json:"expected_material_version"`
	ContentDigest           string `json:"content_digest"`
	ParentCandidateID       string `json:"parent_candidate_id"`
}
