// SPDX-License-Identifier: Elastic-2.0
package mcpbridge

import (
	"agentplatform.local/platform/internal/domain"
	"net/http"
)

type updateLoginPolicyArgs struct {
	IdempotencyKey string                               `json:"idempotency_key"`
	Input          domain.UpdateOrganizationLoginPolicy `json:"input"`
}
type createServiceAccountArgs struct {
	IdempotencyKey string                      `json:"idempotency_key"`
	Input          domain.CreateServiceAccount `json:"input"`
}
type updateServiceAccountArgs struct {
	PrincipalID    string                      `json:"principal_id"`
	IdempotencyKey string                      `json:"idempotency_key"`
	Input          domain.UpdateServiceAccount `json:"input"`
}
type serviceKeysArgs struct {
	PrincipalID string `json:"principal_id"`
	Cursor      string `json:"cursor,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}
type serviceKeyArgs struct {
	PrincipalID string `json:"principal_id"`
	KeyID       string `json:"key_id"`
}
type revokeServiceKeyArgs struct {
	PrincipalID    string                  `json:"principal_id"`
	KeyID          string                  `json:"key_id"`
	IdempotencyKey string                  `json:"idempotency_key"`
	Input          domain.RevokeServiceKey `json:"input"`
}

func securityCatalog() []toolSpec {
	return []toolSpec{
		{Name: "platform_login_policy_get", Description: "Read organization login policy and current trusted-runtime readiness. Requires human identity management permission. No secrets are returned.", Method: http.MethodGet, Path: "/api/v1/login-policy", Args: struct{}{}},
		{Name: "platform_login_policy_update", Description: "Update the complete policy with exact version and audit reason. Bearer transport can change service access and new-key lifetime in mixed mode. SSO mode/issuer changes require a current human owner browser session and cannot be performed by this bearer adapter. Disabling service access revokes old service execution consent; re-enabling never revives old keys or jobs.", Method: http.MethodPost, Path: "/api/v1/login-policy/update", Args: updateLoginPolicyArgs{}, Mutation: true},
		{Name: "platform_service_accounts_list", Description: "List organization-local service accounts. Requires human identity management permission; this grants no service credentials.", Method: http.MethodGet, Path: "/api/v1/service-accounts", Args: listArgs{}},
		{Name: "platform_service_accounts_get", Description: "Read a service account and its current management capabilities.", Method: http.MethodGet, Path: "/api/v1/service-accounts/{principal_id}", Args: memberArgs{}},
		{Name: "platform_service_accounts_create", Description: "Create an organization-bound service account with member or viewer role and audit reason. This never returns or delivers a key.", Method: http.MethodPost, Path: "/api/v1/service-accounts", Args: createServiceAccountArgs{}, Mutation: true},
		{Name: "platform_service_accounts_update", Description: "Update the complete service account snapshot with exact version and audit reason. Disabling revokes current credentials and execution consent; re-enabling never revives old authority.", Method: http.MethodPost, Path: "/api/v1/service-accounts/{principal_id}/update", Args: updateServiceAccountArgs{}, Mutation: true},
		{Name: "platform_service_keys_list", Description: "List safe service-key metadata, immutable role ceilings and expiry; never a secret or hash. Key issuance is a trusted-host command.", Method: http.MethodGet, Path: "/api/v1/service-accounts/{principal_id}/keys", Args: serviceKeysArgs{}},
		{Name: "platform_service_keys_get", Description: "Read exact service-key metadata in its home organization without any credential material.", Method: http.MethodGet, Path: "/api/v1/service-accounts/{principal_id}/keys/{key_id}", Args: serviceKeyArgs{}},
		{Name: "platform_service_keys_revoke", Description: "Revoke one exact key version with audit reason. Revocation blocks new requests; accepted work remains durable. Disable the service account or organization service access to revoke execution consent.", Method: http.MethodPost, Path: "/api/v1/service-accounts/{principal_id}/keys/{key_id}/revoke", Args: revokeServiceKeyArgs{}, Mutation: true},
		{Name: "platform_security_audit_list", Description: "Read append-only safe organization security audit, optionally filtered by target principal. No tokens, hashes, secret references or identity-provider subjects.", Method: http.MethodGet, Path: "/api/v1/security-audit", Args: membershipAuditArgs{}},
	}
}
