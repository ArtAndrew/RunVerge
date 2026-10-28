// SPDX-License-Identifier: Elastic-2.0
package domain

import "time"

const PrincipalHuman = "human"
const PrincipalService = "service"

// LoginPolicyRuntime is trusted server configuration, never request JSON. An
// issuer is supplied only by a successfully initialized browser LoginService.
type LoginPolicyRuntime struct{ TrustedIssuer string }

type OrganizationLoginPolicy struct {
	OrganizationID          string    `json:"organization_id"`
	Mode                    string    `json:"mode"`
	OIDCIssuer              string    `json:"oidc_issuer"`
	ServiceAccessEnabled    bool      `json:"service_access_enabled"`
	MaxServiceKeyTTLSeconds int       `json:"max_service_key_ttl_seconds"`
	Version                 int       `json:"version"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
	AvailableCommands       []string  `json:"available_commands"`
	ConfiguredOIDCIssuer    string    `json:"configured_oidc_issuer,omitempty"`
	CanRequireOIDC          bool      `json:"can_require_oidc"`
	ReadinessReasons        []string  `json:"readiness_reasons"`
}

type UpdateOrganizationLoginPolicy struct {
	ExpectedVersion         int    `json:"expected_version"`
	Mode                    string `json:"mode"`
	OIDCIssuer              string `json:"oidc_issuer"`
	ServiceAccessEnabled    *bool  `json:"service_access_enabled"`
	MaxServiceKeyTTLSeconds int    `json:"max_service_key_ttl_seconds"`
	Reason                  string `json:"reason"`
}

// Service accounts are organization-local memberships with immutable kind and
// home organization. They can never become human owners or browser identities.
type ServiceAccount struct {
	OrganizationID       string     `json:"organization_id"`
	PrincipalID          string     `json:"principal_id"`
	PrincipalKind        string     `json:"principal_kind"`
	DisplayName          string     `json:"display_name"`
	Role                 string     `json:"role"`
	Active               bool       `json:"active"`
	PrincipalActive      bool       `json:"principal_active"`
	Version              int        `json:"version"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	CredentialsRevokedAt *time.Time `json:"credentials_revoked_at,omitempty"`
	AvailableCommands    []string   `json:"available_commands"`
	AssignableRoles      []string   `json:"assignable_roles"`
}

type CreateServiceAccount struct {
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Reason      string `json:"reason"`
}

type UpdateServiceAccount struct {
	ExpectedVersion int    `json:"expected_version"`
	DisplayName     string `json:"display_name"`
	Role            string `json:"role"`
	Active          *bool  `json:"active"`
	Reason          string `json:"reason"`
}

// No secret, hash, token prefix or secret reference is part of public metadata.
// Revocation/expiry prevents new admissions; accepted work is durable. Disable
// the account or organization service access to revoke that execution consent.
type ServiceKey struct {
	ID                string    `json:"id"`
	OrganizationID    string    `json:"organization_id"`
	PrincipalID       string    `json:"principal_id"`
	RoleCeiling       string    `json:"role_ceiling"`
	Version           int       `json:"version"`
	Revoked           bool      `json:"revoked"`
	CreatedAt         time.Time `json:"created_at"`
	ExpiresAt         time.Time `json:"expires_at"`
	Status            string    `json:"status"`
	AvailableCommands []string  `json:"available_commands"`
}

type RevokeServiceKey struct {
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason"`
}

// Audit states deliberately exclude caller-specific capabilities and secrets.
type OrganizationSecurityAuditState struct {
	Version                 int        `json:"version"`
	Mode                    string     `json:"mode,omitempty"`
	OIDCIssuer              string     `json:"oidc_issuer,omitempty"`
	ServiceAccessEnabled    *bool      `json:"service_access_enabled,omitempty"`
	MaxServiceKeyTTLSeconds int        `json:"max_service_key_ttl_seconds,omitempty"`
	DisplayName             string     `json:"display_name,omitempty"`
	Role                    string     `json:"role,omitempty"`
	Active                  *bool      `json:"active,omitempty"`
	RoleCeiling             string     `json:"role_ceiling,omitempty"`
	ExpiresAt               *time.Time `json:"expires_at,omitempty"`
	Revoked                 *bool      `json:"revoked,omitempty"`
}

type OrganizationSecurityAuditEntry struct {
	ID                string                          `json:"id"`
	OrganizationID    string                          `json:"organization_id"`
	ActorType         string                          `json:"actor_type"`
	ActorID           string                          `json:"actor_id,omitempty"`
	ObjectType        string                          `json:"object_type"`
	ObjectID          string                          `json:"object_id"`
	TargetPrincipalID string                          `json:"target_principal_id,omitempty"`
	Action            string                          `json:"action"`
	Reason            string                          `json:"reason"`
	Before            *OrganizationSecurityAuditState `json:"before,omitempty"`
	After             OrganizationSecurityAuditState  `json:"after"`
	CreatedAt         time.Time                       `json:"created_at"`
}

// EffectivePermissions is shared by HTTP, MCP, commands and reconstructed
// background principals. Unknown identity kinds fail closed. A service key's
// immutable role ceiling is already intersected with the live membership role
// by authentication; services have no administrative or human-review authority.
func EffectivePermissions(p Principal) []string {
	out := []string{}
	if p.Kind != PrincipalHuman && p.Kind != PrincipalService {
		return out
	}
	if p.Kind == PrincipalService && p.Role != "member" && p.Role != "viewer" {
		return out
	}
	for _, permission := range RolePermissions[p.Role] {
		if p.Kind == PrincipalService && !servicePermissionAllowed(permission) {
			continue
		}
		out = append(out, permission)
	}
	return out
}

// An explicit allowlist prevents future additions to a human role from
// silently broadening the service profile. Unknown permissions remain denied.
func servicePermissionAllowed(permission string) bool {
	switch permission {
	case "tasks.read", "tasks.create", "tasks.retry_own", "tasks.continue_own", "tasks.cancel_own", "actions.prepare_own", "actions.dispatch_own", "materials.write_own", "materials.publish_own", "agents.read", "agents.write", "costs.read":
		return true
	}
	return false
}

func PrincipalHasPermission(p Principal, permission string) bool {
	for _, allowed := range EffectivePermissions(p) {
		if allowed == permission {
			return true
		}
	}
	return false
}
