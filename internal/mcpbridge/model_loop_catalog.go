// SPDX-License-Identifier: Elastic-2.0
package mcpbridge

import (
	"agentplatform.local/platform/internal/domain"
	"net/http"
)

type proposalArgs struct {
	TaskID     string `json:"task_id"`
	ProposalID string `json:"proposal_id"`
}
type continueModelArgs struct {
	TaskID         string                      `json:"task_id"`
	IdempotencyKey string                      `json:"idempotency_key"`
	Input          domain.ContinueModelRequest `json:"input"`
}

func modelLoopCatalog() []toolSpec {
	return []toolSpec{
		{Name: "platform_model_proposals_list", Description: "List metadata of untrusted native model tool requests. They are not final task answers or authority to act.", Method: http.MethodGet, Path: "/api/v1/tasks/{task_id}/model-tool-proposals", Args: taskPageArgs{}},
		{Name: "platform_model_proposals_get", Description: "Read an exact model tool request before separately preparing, approving and dispatching its bound action.", Method: http.MethodGet, Path: "/api/v1/tasks/{task_id}/model-tool-proposals/{proposal_id}", Args: proposalArgs{}},
		{Name: "platform_model_continue", Description: "Explicitly queue the next model turn in the same Run after all exact tool requests have terminal observations. Requires current versions and cost acknowledgement. Unknown effects cannot continue; no prior bill is settled.", Method: http.MethodPost, Path: "/api/v1/tasks/{task_id}/continue-model", Args: continueModelArgs{}, Mutation: true},
	}
}
