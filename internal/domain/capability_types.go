// SPDX-License-Identifier: Elastic-2.0
package domain

import "time"

// ProductCapabilities documents support and the current role observation.
// It is never a reusable authorization decision or an execution permit.
type ProductCapabilities struct {
	CatalogVersion     string                 `json:"catalog_version"`
	APIContract        string                 `json:"api_contract"`
	Assessment         string                 `json:"assessment"`
	ReadOnly           bool                   `json:"read_only"`
	ObservedAt         time.Time              `json:"observed_at"`
	Identity           CurrentIdentity        `json:"identity"`
	AuthorizationRules []string               `json:"authorization_rules"`
	Operations         []CapabilityOperation  `json:"operations"`
	Profiles           []CapabilityProfile    `json:"profiles"`
	Roles              []CapabilityRole       `json:"roles"`
	Onboarding         []CapabilityOnboarding `json:"onboarding"`
	RemediationCommand []string               `json:"remediation_command"`
}

type CapabilityOperation struct {
	ID                     string              `json:"id"`
	CapabilityID           string              `json:"capability_id"`
	Method                 string              `json:"method"`
	Path                   string              `json:"path"`
	InputSchema            string              `json:"input_schema"`
	OutputSchema           string              `json:"output_schema"`
	ResponseKind           string              `json:"response_kind"`
	SuccessStatus          int                 `json:"success_status"`
	AccessModel            string              `json:"access_model"`
	PermissionAlternatives [][]string          `json:"permission_alternatives"`
	RuntimeGuards          []string            `json:"runtime_guards"`
	Effect                 string              `json:"effect"`
	CostSemantics          string              `json:"cost_semantics"`
	RetryPolicy            string              `json:"retry_policy"`
	Bindings               []CapabilityBinding `json:"bindings"`
	Documentation          []string            `json:"documentation"`
}

type CapabilityBinding struct {
	Surface string `json:"surface"`
	Status  string `json:"status"`
	Name    string `json:"name"`
}

type CapabilityProfile struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Status        string   `json:"status"`
	Summary       string   `json:"summary"`
	Constraints   []string `json:"constraints"`
	Documentation []string `json:"documentation"`
}

type CapabilityRole struct {
	PrincipalKind string   `json:"principal_kind"`
	Role          string   `json:"role"`
	Permissions   []string `json:"permissions"`
}

type CapabilityOnboarding struct {
	ID            string           `json:"id"`
	Persona       string           `json:"persona"`
	Status        string           `json:"status"`
	Summary       string           `json:"summary"`
	Prerequisites []string         `json:"prerequisites"`
	Steps         []CapabilityStep `json:"steps"`
	Documentation []string         `json:"documentation"`
}

type CapabilityStep struct {
	ID                  string     `json:"id"`
	Instruction         string     `json:"instruction"`
	Actor               string     `json:"actor"`
	OperationIDs        []string   `json:"operation_ids"`
	Commands            [][]string `json:"commands"`
	Effect              string     `json:"effect"`
	CostAcknowledgement bool       `json:"cost_acknowledgement"`
}
