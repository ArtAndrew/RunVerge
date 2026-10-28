// SPDX-License-Identifier: Elastic-2.0
package mcpbridge

import (
	"agentplatform.local/platform/internal/domain"
	"net/http"
)

type createAutomationArgs struct {
	TaskID         string                      `json:"task_id"`
	IdempotencyKey string                      `json:"idempotency_key"`
	Input          domain.CreateTaskAutomation `json:"input"`
}
type revokeAutomationArgs struct {
	TaskID         string                      `json:"task_id"`
	IdempotencyKey string                      `json:"idempotency_key"`
	Input          domain.RevokeTaskAutomation `json:"input"`
}

func automationCatalog() []toolSpec {
	return []toolSpec{
		{Name: "platform_automation_get", Description: "Inspect the exact task automation grant, cumulative usage and recent stop signals. Reading does not grant authority or start work.", Method: http.MethodGet, Path: "/api/v1/tasks/{task_id}/automation", Args: taskArgs{}},
		{Name: "platform_progress_signals_list", Description: "Read durable bounded detector signals explaining automatic stops. These are not proof of truth or financial settlement.", Method: http.MethodGet, Path: "/api/v1/tasks/{task_id}/progress-signals", Args: taskPageArgs{}},
		{Name: "platform_automation_create", Description: "Explicitly delegate bounded task automation with pinned tools, cumulative cost ceilings and expiry. This delegation persists after logout until revoked or expired; it cannot bypass current organization permissions. Read-only automation requires trusted installed read tools; prepare-and-continue still requires human approval and explicit dispatch for tools. Final output is not automatically accepted.", Method: http.MethodPost, Path: "/api/v1/tasks/{task_id}/automation", Args: createAutomationArgs{}, Mutation: true},
		{Name: "platform_automation_revoke", Description: "Revoke an exact task automation grant by version. Stops future calls under that delegation; preserves past effects, observations and financial liabilities.", Method: http.MethodPost, Path: "/api/v1/tasks/{task_id}/automation/revoke", Args: revokeAutomationArgs{}, Mutation: true},
	}
}
