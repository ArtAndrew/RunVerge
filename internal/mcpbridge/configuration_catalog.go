// SPDX-License-Identifier: Elastic-2.0
package mcpbridge

import (
	"agentplatform.local/platform/internal/domain"
	"net/http"
)

type configurationGrantArgs struct {
	GrantID string `json:"grant_id"`
}
type configurationPlanArgs struct {
	PlanID string `json:"plan_id"`
}
type configurationVerificationArgs struct {
	VerificationID string `json:"verification_id"`
}
type configurationVerificationPageArgs struct {
	PlanID string `json:"plan_id,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}
type configurationCreateArgs[T any] struct {
	IdempotencyKey string `json:"idempotency_key"`
	Input          T      `json:"input"`
}
type configurationPlanCommandArgs[T any] struct {
	PlanID         string `json:"plan_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Input          T      `json:"input"`
}
type configurationRevokeArgs struct {
	GrantID        string                          `json:"grant_id"`
	IdempotencyKey string                          `json:"idempotency_key"`
	Input          domain.RevokeConfigurationGrant `json:"input"`
}

func configurationCatalog() []toolSpec {
	const base = "/api/v1/configuration"
	specs := []toolSpec{
		{Name: "platform_configuration_catalog", Description: "Read current configuration revision and caller capabilities. This confers no delegated authority or private connection access.", Method: http.MethodGet, Path: base + "/catalog", Args: struct{}{}},
		{Name: "platform_configuration_grants_get", Description: "Read one caller-visible capability envelope. It is a durable record, not a bearer credential.", Method: http.MethodGet, Path: base + "/grants/{grant_id}", Args: configurationGrantArgs{}},
		{Name: "platform_configuration_plans_get", Description: "Read one immutable plan and redacted diff. Applied settings do not prove connection health.", Method: http.MethodGet, Path: base + "/plans/{plan_id}", Args: configurationPlanArgs{}},
		{Name: "platform_configuration_verifications_get", Description: "Read one structural connection check and separate cost status. Unknown outcomes never authorize resend; no peer text or keys are returned.", Method: http.MethodGet, Path: base + "/verifications/{verification_id}", Args: configurationVerificationArgs{}},
		{Name: "platform_configuration_grants_create", Description: "Issue explicit root consent as a human owner, or attenuate a live delegate grant. Children share every ancestor cap; no key is issued.", Method: http.MethodPost, Path: base + "/grants", Args: configurationCreateArgs[domain.CreateConfigurationGrant]{}, Mutation: true},
		{Name: "platform_configuration_grants_revoke", Description: "Revoke exact grant authority. Stops new stages and unsent checks; preserves applied settings, late facts and financial holds.", Method: http.MethodPost, Path: base + "/grants/{grant_id}/revoke", Args: configurationRevokeArgs{}, Mutation: true},
		{Name: "platform_configuration_plans_create", Description: "Persist exact approved destination pins, expected resource versions and immutable intent. This performs no network I/O.", Method: http.MethodPost, Path: base + "/plans", Args: configurationCreateArgs[domain.CreateConfigurationPlan]{}, Mutation: true},
		{Name: "platform_configuration_plans_validate", Description: "Persist static validation of one exact plan. Policy explanations are not reusable dispatch permissions; no network I/O occurs.", Method: http.MethodPost, Path: base + "/plans/{plan_id}/validate", Args: configurationPlanCommandArgs[domain.ValidateConfigurationPlan]{}, Mutation: true},
		{Name: "platform_configuration_plans_approve", Description: "Record current human-owner approval bound to exact plan digest and validation ID. Services cannot approve.", Method: http.MethodPost, Path: base + "/plans/{plan_id}/approve", Args: configurationPlanCommandArgs[domain.ApproveConfigurationPlan]{}, Mutation: true},
		{Name: "platform_configuration_plans_apply", Description: "Apply validated settings once with current scope, approval and revision checks. Retirement can stop pinned tasks; no hidden check or rollback runs.", Method: http.MethodPost, Path: base + "/plans/{plan_id}/apply", Args: configurationPlanCommandArgs[domain.ApplyConfigurationPlan]{}, Mutation: true},
		{Name: "platform_configuration_plans_verify", Description: "Request one separately acknowledged check per applied operation. Model checks reserve the explicit ceiling; MCP discovery must be owner-approved unmetered. Lost replies do not authorize replacement attempts.", Method: http.MethodPost, Path: base + "/plans/{plan_id}/verify", Args: configurationPlanCommandArgs[domain.StartConfigurationVerification]{}, Mutation: true},
	}
	for _, kind := range []string{"destinations", "resources", "grants", "plans", "audit", "verifications"} {
		var args any = listArgs{}
		if kind == "verifications" {
			args = configurationVerificationPageArgs{}
		}
		specs = append(specs, toolSpec{Name: "platform_configuration_" + kind + "_list", Description: "List paginated caller-visible configuration " + kind + " through current authorization. Descriptors and receipts contain no endpoint, environment reference, credential or raw peer response; they grant no tool execution authority.", Method: http.MethodGet, Path: base + "/" + kind, Args: args})
	}
	return specs
}
