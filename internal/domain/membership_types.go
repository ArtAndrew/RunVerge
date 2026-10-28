// SPDX-License-Identifier: Elastic-2.0
package domain

import "time"

// OrganizationMember is an organization-local view, never a global identity or
// credential directory. Capabilities are derived from the current caller.
type OrganizationMember struct {
	OrganizationID       string     `json:"organization_id"`
	PrincipalID          string     `json:"principal_id"`
	PrincipalKind        string     `json:"principal_kind,omitempty"`
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

type MembershipCapabilities struct {
	OrganizationID        string   `json:"organization_id"`
	CanCreateMembers      bool     `json:"can_create_members"`
	AssignableRoles       []string `json:"assignable_roles"`
	SelfManagementAllowed bool     `json:"self_management_allowed"`
}

type CreateOrganizationMember struct {
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Reason      string `json:"reason"`
}

type UpdateOrganizationMember struct {
	ExpectedVersion int    `json:"expected_version"`
	DisplayName     string `json:"display_name"`
	Role            string `json:"role"`
	Active          *bool  `json:"active"`
	Reason          string `json:"reason"`
}

type RevokeMemberCredentials struct {
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason"`
}

// MembershipAuditState has no caller-dependent capability flags or secrets.
type MembershipAuditState struct {
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Active      bool   `json:"active"`
	Version     int    `json:"version"`
}

type MembershipAuditEntry struct {
	ID                string                `json:"id"`
	OrganizationID    string                `json:"organization_id"`
	ActorID           string                `json:"actor_id"`
	TargetPrincipalID string                `json:"target_principal_id"`
	Action            string                `json:"action"`
	Reason            string                `json:"reason"`
	Before            *MembershipAuditState `json:"before,omitempty"`
	After             MembershipAuditState  `json:"after"`
	CreatedAt         time.Time             `json:"created_at"`
}
