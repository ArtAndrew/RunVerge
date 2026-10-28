// SPDX-License-Identifier: Elastic-2.0
package mcpbridge

import (
	"agentplatform.local/platform/internal/domain"
	"net/http"
)

type memberArgs struct {
	PrincipalID string `json:"principal_id"`
}
type membershipAuditArgs struct {
	PrincipalID string `json:"principal_id,omitempty"`
	Cursor      string `json:"cursor,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}
type createMemberArgs struct {
	IdempotencyKey string                          `json:"idempotency_key"`
	Input          domain.CreateOrganizationMember `json:"input"`
}
type updateMemberArgs struct {
	PrincipalID    string                          `json:"principal_id"`
	IdempotencyKey string                          `json:"idempotency_key"`
	Input          domain.UpdateOrganizationMember `json:"input"`
}
type revokeMemberArgs struct {
	PrincipalID    string                         `json:"principal_id"`
	IdempotencyKey string                         `json:"idempotency_key"`
	Input          domain.RevokeMemberCredentials `json:"input"`
}

func membershipCatalog() []toolSpec {
	return []toolSpec{
		{Name: "platform_members_capabilities", Description: "Read current membership-management capabilities. These flags do not delegate authority.", Method: http.MethodGet, Path: "/api/v1/members/capabilities", Args: struct{}{}},
		{Name: "platform_members_list", Description: "List members of the currently authorized organization, including inactive memberships. Requires identity.manage.", Method: http.MethodGet, Path: "/api/v1/members", Args: listArgs{}},
		{Name: "platform_members_get", Description: "Read one membership by exact principal ID in the current organization.", Method: http.MethodGet, Path: "/api/v1/members/{principal_id}", Args: memberArgs{}},
		{Name: "platform_membership_audit_list", Description: "Read append-only organization membership audit; optionally filter by target principal ID. This does not expose credentials.", Method: http.MethodGet, Path: "/api/v1/membership-audit", Args: membershipAuditArgs{}},
		{Name: "platform_members_create", Description: "Provision a NEW principal and one organization membership. No invitation is delivered and no login token or external identity is created. Requires current management authority and a reason.", Method: http.MethodPost, Path: "/api/v1/members", Args: createMemberArgs{}, Mutation: true},
		{Name: "platform_members_update", Description: "Update exact membership version, local name, role and explicit active status. Self changes are rejected. Role/status changes invalidate old execution authority; disabling and re-enabling never restore old credentials.", Method: http.MethodPost, Path: "/api/v1/members/{principal_id}/update", Args: updateMemberArgs{}, Mutation: true},
		{Name: "platform_members_revoke_credentials", Description: "Invalidate existing login credentials for this organization member only. This does NOT revoke durable task grants or change role/status. Disable membership or revoke the exact task grant to stop durable authorization. Requires exact version and reason; self changes are rejected.", Method: http.MethodPost, Path: "/api/v1/members/{principal_id}/revoke-credentials", Args: revokeMemberArgs{}, Mutation: true},
	}
}
