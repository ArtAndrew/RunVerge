// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"agentplatform.local/platform/internal/domain"
	"reflect"
	"strings"
)

// Only public DTOs belong here. Trusted profiles and worker inputs are never
// reachable from the public registry, including via recursive description.
func configurationDTOs() map[string]reflect.Type {
	return map[string]reflect.Type{
		"ConfigurationDestinationRef":    reflect.TypeFor[domain.ConfigurationDestinationRef](),
		"ConfigurationDestination":       reflect.TypeFor[domain.ConfigurationDestination](),
		"ConfigurationScope":             reflect.TypeFor[domain.ConfigurationScope](),
		"CapabilityEnvelope":             reflect.TypeFor[domain.CapabilityEnvelope](),
		"CreateConfigurationGrant":       reflect.TypeFor[domain.CreateConfigurationGrant](),
		"RevokeConfigurationGrant":       reflect.TypeFor[domain.RevokeConfigurationGrant](),
		"ConfigurationCatalog":           reflect.TypeFor[domain.ConfigurationCatalog](),
		"ConfigurationOperationInput":    reflect.TypeFor[domain.ConfigurationOperationInput](),
		"ConfigurationOperation":         reflect.TypeFor[domain.ConfigurationOperation](),
		"ConfigurationDiff":              reflect.TypeFor[domain.ConfigurationDiff](),
		"CreateConfigurationPlan":        reflect.TypeFor[domain.CreateConfigurationPlan](),
		"ConfigurationPlan":              reflect.TypeFor[domain.ConfigurationPlan](),
		"PolicyDecision":                 reflect.TypeFor[domain.PolicyDecision](),
		"ValidateConfigurationPlan":      reflect.TypeFor[domain.ValidateConfigurationPlan](),
		"ConfigurationValidation":        reflect.TypeFor[domain.ConfigurationValidation](),
		"ApproveConfigurationPlan":       reflect.TypeFor[domain.ApproveConfigurationPlan](),
		"ConfigurationApproval":          reflect.TypeFor[domain.ConfigurationApproval](),
		"ApplyConfigurationPlan":         reflect.TypeFor[domain.ApplyConfigurationPlan](),
		"StartConfigurationVerification": reflect.TypeFor[domain.StartConfigurationVerification](),
		"ConfigurationVerification":      reflect.TypeFor[domain.ConfigurationVerification](),
		"ConfigurationResource":          reflect.TypeFor[domain.ConfigurationResource](),
		"ConfigurationAuditEntry":        reflect.TypeFor[domain.ConfigurationAuditEntry](),
	}
}

func refineConfigurationSchema(name string, result schema) schema {
	if _, public := configurationDTOs()[name]; !public {
		return result
	}
	properties := result["properties"].(map[string]any)
	result["additionalProperties"] = false
	for field, raw := range properties {
		value := raw.(schema)
		switch {
		case field == "revision" || field == "base_revision" || field == "current_revision" || field == "applied_revision" || field == "configuration_revision":
			properties[field] = decimalSchema(9223372036854775807, false)
			properties[field].(schema)["description"] = "Exact configuration revision encoded as a canonical decimal string; use as an opaque concurrency token."
		case strings.HasSuffix(field, "microusd"):
			properties[field] = decimalSchema(domain.MaxMoney, false)
		case field == "id" || strings.HasSuffix(field, "_id"):
			value["minLength"], value["maxLength"], value["pattern"] = 1, 128, `^[A-Za-z0-9_-]{1,128}$`
		case field == "version" || strings.HasSuffix(field, "_version") || field == "expected_version" || field == "new_version":
			value["minimum"], value["maximum"] = 1, 2147483647
			if field == "expected_resource_version" || field == "expected_version" && name == "ConfigurationDiff" {
				value["minimum"], value["maximum"] = 0, 2147483646
			}
			if field == "expected_parent_version" {
				value["minimum"] = 0
			}
		case field == "digest" || strings.HasSuffix(field, "_digest"):
			value["minLength"], value["maxLength"], value["pattern"] = 64, 64, `^[a-f0-9]{64}$`
		case field == "reason":
			value["minLength"], value["maxLength"], value["x-max-utf8-bytes"], value["pattern"] = 1, 4096, 4096, nonblankPattern
		case field == "ttl_seconds":
			value["minimum"], value["maximum"] = 1, domain.MaxConfigurationPlanTTLSeconds
			if name == "CreateConfigurationGrant" {
				value["maximum"] = domain.MaxConfigurationGrantTTLSeconds
			}
		case field == "depth" || field == "max_grant_depth":
			value["minimum"], value["maximum"] = 0, domain.MaxConfigurationGrantDepth
		case field == "max_plans" || field == "max_verifications":
			value["minimum"], value["maximum"] = 1, 100
		case field == "used_plans" || field == "used_verifications":
			value["minimum"], value["maximum"] = 0, 100
		case field == "tool_count":
			value["minimum"], value["maximum"] = 0, 128
		case field == "operations":
			value["minItems"], value["maxItems"] = 1, domain.MaxConfigurationOperations
		case field == "destinations" || field == "scopes":
			value["minItems"], value["maxItems"], value["uniqueItems"] = 1, domain.MaxConfigurationScopes, true
		case field == "actions":
			value["maxItems"], value["uniqueItems"], value["items"] = 6, true, schema{"type": "string", "enum": domain.ConfigurationActions}
		case field == "available_commands":
			value["maxItems"], value["uniqueItems"] = 6, true
		case field == "reason_codes" || field == "obligations":
			value["maxItems"], value["items"] = 16, schema{"type": "string", "minLength": 1, "maxLength": 128}
		case field == "kind":
			value["enum"] = []string{"model", "mcp"}
		case field == "audience":
			value["const"] = domain.ConfigurationAudience
		case field == "cost_status":
			value["enum"] = []string{"not_reserved", "reserved", "unknown", "settled", "released", "unmetered"}
		}
	}
	if status, ok := properties["status"]; ok {
		switch name {
		case "CapabilityEnvelope":
			status.(schema)["enum"] = []string{"active", "revoked", "expired", "invalidated"}
		case "ConfigurationPlan":
			status.(schema)["enum"] = []string{"planned", "applied", "expired"}
		case "ConfigurationVerification":
			status.(schema)["enum"] = []string{"queued", "sent", "response_received", "unknown", "failed", "cancelled"}
		}
	}
	if name == "PolicyDecision" {
		properties["outcome"].(schema)["enum"] = []string{"allow", "requires_approval", "deny"}
	}
	return result
}
