// SPDX-License-Identifier: Elastic-2.0
package domain

// CurrentIdentity is the active membership of this authenticated request. It
// contains no credential, session handle, identity-provider token or secret.
type CurrentIdentity struct {
	PrincipalID    string   `json:"principal_id"`
	OrganizationID string   `json:"organization_id"`
	Role           string   `json:"role"`
	PrincipalKind  string   `json:"principal_kind,omitempty"`
	Permissions    []string `json:"permissions"`
}
