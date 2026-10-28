// SPDX-License-Identifier: Elastic-2.0
// Generates wire schemas from the canonical Go DTOs. Do not edit generated JSON.
package main

import (
	"agentplatform.local/platform/internal/domain"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

type schema map[string]any

// Same Unicode White_Space set used by Go strings.TrimSpace. Literal scalar
// characters keep this JSON Schema pattern portable across regex engines.
const nonblankPattern = "[^\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]"

// Exact decimal limits are strings because JavaScript numbers cannot represent
// every signed int64 monetary fact. No floating point conversion is involved.
func decimalParts(limit string) []string {
	parts := []string{"0"}
	if len(limit) > 1 {
		parts = append(parts, "[1-9][0-9]{0,"+strconv.Itoa(len(limit)-2)+"}")
	}
	for i := range len(limit) {
		low := byte('0')
		if i == 0 {
			low = '1'
		}
		high := limit[i] - 1
		if limit[i] == '0' || high < low {
			continue
		}
		choice := string(low)
		if low != high {
			choice = "[" + string(low) + "-" + string(high) + "]"
		}
		rest := len(limit) - i - 1
		if rest > 0 {
			choice += "[0-9]{" + strconv.Itoa(rest) + "}"
		}
		parts = append(parts, limit[:i]+choice)
	}
	return append(parts, limit)
}
func decimalSchema(limit int64, signed bool) schema {
	positive := strings.Join(decimalParts(strconv.FormatInt(limit, 10)), "|")
	pattern := "^(?:" + positive + ")(?![\\s\\S])"
	if signed {
		negative := decimalParts(strconv.FormatUint(uint64(limit)+1, 10))
		pattern = "^(?:" + positive + "|-(?:" + strings.Join(negative[1:], "|") + "))(?![\\s\\S])"
	}
	return schema{"type": "string", "pattern": pattern, "maxLength": 20, "x-integer-encoding": "decimal-int64", "description": "Exact integer micro-USD encoded as a canonical decimal string."}
}

func describe(t reflect.Type) schema {
	if t == reflect.TypeFor[domain.Event]() {
		return publicEventSchema()
	}
	if t == reflect.TypeFor[time.Time]() {
		return schema{"type": "string", "format": "date-time"}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return describe(t.Elem())
	case reflect.String:
		return schema{"type": "string"}
	case reflect.Bool:
		return schema{"type": "boolean"}
	case reflect.Int, reflect.Int64:
		return schema{"type": "integer"}
	case reflect.Slice:
		return schema{"type": "array", "items": describe(t.Elem())}
	case reflect.Map:
		return schema{"type": "object", "additionalProperties": true}
	case reflect.Interface:
		return schema{}
	case reflect.Struct:
		properties := map[string]any{}
		required := []string{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")
			if tag[0] == "-" {
				continue
			}
			name := tag[0]
			if name == "" {
				name = f.Name
			}
			value := describe(f.Type)
			quoted, optional := false, false
			for _, option := range tag[1:] {
				quoted = quoted || option == "string"
				optional = optional || option == "omitempty"
			}
			if quoted {
				limit := int64(domain.MaxFactualMoney)
				if (t.Name() == "Reservation" && name == "amount_microusd") || ((t.Name() == "ToolInfo" || t.Name() == "ToolExecution" || t.Name() == "TaskAutomationGrant" || t.Name() == "CreateTaskAutomation") && name == "max_charge_microusd") || (t.Name() == "AutomationUsage" && name == "allocated_microusd") {
					limit = domain.MaxMoney
				}
				value = decimalSchema(limit, strings.HasPrefix(name, "delta_"))
				if t.Name() == "EventReplayWindow" || name == "unknown_reservation_count" {
					value = decimalSchema(9223372036854775807, false)
					if name == "unknown_reservation_count" {
						value["description"] = "Exact nonnegative bigint count across the full task subtree, including zero-amount unknown reservations; independent of pagination."
					} else {
						value["description"] = "Exact nonnegative PostgreSQL bigint event position."
					}
				}
			}
			properties[name] = value
			if !optional {
				required = append(required, name)
			}
		}
		result := schema{"type": "object", "properties": properties, "required": required}
		for name, raw := range properties {
			value := raw.(schema)
			if value["x-integer-encoding"] != "" && value["x-integer-encoding"] != nil {
				continue
			}
			switch name {
			case "budget_microusd":
				value["minimum"] = 1
				value["maximum"] = domain.MaxMoney
			case "max_charge_microusd":
				value["minimum"] = 1
				value["maximum"] = domain.MaxMoney
			case "max_input_bytes":
				value["minimum"] = 1
				value["maximum"] = 1048576
			case "max_output_tokens":
				value["minimum"] = 1
				value["maximum"] = 1000000
			case "bound_assumptions":
				value["minLength"] = 1
				value["maxLength"] = 4096
				value["x-max-utf8-bytes"] = 4096
			case "amount_microusd", "actual_microusd":
				value["minimum"] = 0
				value["maximum"] = domain.MaxMoney
			case "reserved_microusd", "spent_microusd", "unknown_microusd", "fencing_token", "attempts", "pending_quarantine_count":
				value["minimum"] = 0
			case "version", "agent_version", "model_connection_version", "artifact_version", "task_version_before", "task_version_after", "expected_version", "expected_revision", "base_revision", "revision", "max_attempts", "sequence":
				value["minimum"] = 1
			case "intent_hash", "payload_digest", "content_digest", "input_digest", "head_content_digest":
				value["pattern"] = "^[0-9a-f]{64}(?![\\s\\S])"
			case "depth":
				value["minimum"] = 0
				value["maximum"] = domain.MaxTaskDepth
			case "max_depth":
				value["minimum"] = 1
				value["maximum"] = domain.MaxTaskDepth
			case "max_children":
				value["minimum"] = 1
				value["maximum"] = 32
			case "max_parallel":
				value["minimum"] = 1
				value["maximum"] = 32
			case "max_runs", "attempt":
				value["minimum"] = 1
				value["maximum"] = 10
			case "attempt_count":
				value["minimum"] = 0
				value["maximum"] = 10
			case "execution_timeout_seconds":
				value["minimum"] = 1
				value["maximum"] = 2592000
			case "pattern":
				value["enum"] = []string{"sequence", "parallel", "delegate_verify"}
			case "join_policy":
				value["enum"] = []string{"all", "allow_partial"}
			case "decision":
				if value["type"] == "string" {
					value["enum"] = []string{"approved", "denied"}
				}
				if t.Name() == "ReviewTaskRequest" || t.Name() == "TaskDecision" {
					value["enum"] = []string{"accept", "reject", "request_changes"}
				}
			case "method":
				if t.Name() == "TaskDecision" {
					value["enum"] = []string{"authorized_user_decision", "inherited_authorized_user_decision"}
				}
			case "acknowledge_new_cost":
				value["const"] = true
			case "members":
				value["minItems"] = 1
				value["maxItems"] = 32
			case "charge_key":
				value["minLength"] = 1
				value["maxLength"] = 128
				value["x-max-utf8-bytes"] = 128
			case "provider", "source", "price_version":
				value["minLength"] = 1
				value["maxLength"] = 128
				value["x-max-utf8-bytes"] = 128
			case "reference":
				value["minLength"] = 1
				value["maxLength"] = 256
				value["x-max-utf8-bytes"] = 256
			case "reason":
				value["maxLength"] = 256
				value["x-max-utf8-bytes"] = 256
				if t.Name() == "ReviewTaskRequest" || t.Name() == "RetryTaskRequest" || t.Name() == "TaskDecision" || t.Name() == "CreateMaterial" || t.Name() == "ReviseMaterial" || t.Name() == "MaterialRevision" || t.Name() == "Approval" {
					value["maxLength"] = 4096
					value["x-max-utf8-bytes"] = 4096
				}
				if t.Name() == "RetryTaskRequest" || t.Name() == "ReviseMaterial" {
					value["minLength"] = 1
					value["pattern"] = nonblankPattern
				}
			case "feedback":
				value["maxLength"] = 8192
				value["x-max-utf8-bytes"] = 8192
			case "task":
				if t.Name() == "TeamMember" {
					value["minLength"] = 1
					value["maxLength"] = 4096
					value["x-max-utf8-bytes"] = 4096
				}
			case "final":
				value["const"] = true
			case "currency":
				value["const"] = "USD"
			case "category":
				categories := []string{"model", "tool", "compute", "verification", "recovery"}
				if t.Name() == "Reservation" {
					categories = append(categories, "fixture_legacy")
				}
				value["enum"] = categories
			case "name":
				if t.Name() == "Agent" || t.Name() == "CreateAgent" || t.Name() == "AgentDefinition" || t.Name() == "Material" || t.Name() == "CreateMaterial" {
					value["minLength"] = 1
					value["maxLength"] = 120
					value["x-max-utf8-bytes"] = 120
					if t.Name() == "Material" || t.Name() == "CreateMaterial" {
						value["pattern"] = nonblankPattern
					}
				}
			case "instructions":
				value["minLength"] = 1
				value["maxLength"] = 32768
				value["x-max-utf8-bytes"] = 32768
			case "goal":
				value["minLength"] = 1
				value["maxLength"] = 16384
				value["x-max-utf8-bytes"] = 16384
			case "agent_id", "candidate_id", "artifact_id", "material_id", "parent_candidate_id", "review_basis_candidate_id", "base_candidate_id":
				value["minLength"] = 1
				value["maxLength"] = 128
				value["x-max-utf8-bytes"] = 128
			case "content":
				value["maxLength"] = domain.MaxMaterialContentBytes
				value["x-max-utf8-bytes"] = domain.MaxMaterialContentBytes
			case "head_version", "material_version", "expected_material_version", "merge_base_version":
				value["minimum"] = 1
				value["maximum"] = domain.MaxMaterialRevisions
			case "expected_task_version":
				value["minimum"] = 1
			case "base_version":
				value["minimum"] = 0
				value["maximum"] = domain.MaxMaterialRevisions - 1
			case "media_type":
				if t.Name() == "Material" || t.Name() == "CreateMaterial" {
					value["enum"] = []string{"text/plain", "text/markdown", "application/json"}
				}
			}
		}
		if t.Name() == "MaterialRevision" || t.Name() == "ReviseMaterial" {
			field := "version"
			if t.Name() == "ReviseMaterial" {
				field = "expected_version"
			}
			properties[field].(schema)["maximum"] = domain.MaxMaterialRevisions
		}
		kinds := map[string]string{"Task": "task", "Run": "run", "Operation": "operation", "Action": "action", "Reservation": "reservation", "Approval": "approval", "ToolExecution": "tool_execution"}
		if kind := kinds[t.Name()]; kind != "" {
			states := []string{}
			for state := range domain.StateMachines[kind] {
				states = append(states, state)
			}
			sort.Strings(states)
			properties["status"].(schema)["enum"] = states
		}
		switch t.Name() {
		case "CreateTask", "CreateAgent", "ApprovalDecision", "ReserveCostRequest", "CostEvidence", "CostAdjustment", "AgentDefinition", "TeamDefinition", "TeamMember", "ReviewTaskRequest", "RetryTaskRequest", "CreateMaterial", "ReviseMaterial", "PublishMaterial", "PrepareToolExecution", "DecideToolExecution", "DispatchToolExecution", "ReconcileToolExecution", "ContinueModelRequest", "ModelToolResultBinding", "CreateTaskAutomation", "RevokeTaskAutomation", "AutomationToolPin":
			result["additionalProperties"] = false
		}
		if t.Name() == "AccountingOutcome" {
			properties["status"].(schema)["enum"] = []string{"settled", "quarantined"}
		}
		if t.Name() == "CostQuarantine" {
			properties["status"].(schema)["enum"] = []string{"pending", "resolved"}
			properties["reason"].(schema)["enum"] = []string{"projection_overflow"}
			properties["requested_kind"].(schema)["enum"] = []string{"settle", "adjustment"}
		}
		if t.Name() == "TaskCandidate" {
			properties["source"].(schema)["enum"] = []string{"model", "model_tools", "team", "fixture", "material"}
		}
		if t.Name() == "ReviewTaskRequest" {
			result["allOf"] = []any{schema{"if": schema{"properties": schema{"decision": schema{"enum": []string{"reject", "request_changes"}}}}, "then": schema{"required": []string{"reason"}, "properties": schema{"reason": schema{"minLength": 1}}}}}
		}

		if strings.Contains(t.Name(), "Tool") {
			for field, raw := range properties {
				if field == "id" || strings.HasSuffix(field, "_id") || field == "created_by" || field == "dispatched_by" || field == "provider" || field == "price_version" {
					v := raw.(schema)
					v["minLength"] = 1
					v["maxLength"] = 128
					v["x-max-utf8-bytes"] = 128
					v["pattern"] = nonblankPattern
				}
			}
			for _, field := range []string{"expected_task_version", "expected_execution_version", "expected_approval_version", "tool_version", "source_task_version"} {
				if value, ok := properties[field]; ok {
					value.(schema)["minimum"] = 1
				}
			}
			for _, field := range []string{"tool_profile_digest", "arguments_digest", "input_schema_digest", "evidence_digest"} {
				if value, ok := properties[field]; ok {
					value.(schema)["pattern"] = "^[0-9a-f]{64}(?![\\s\\S])"
				}
			}
			limits := map[string]int{"arguments_json": 32768, "input_schema_json": 8192, "reason": 4096, "effect_reference": 256, "description": 4096, "destination": 256, "result": 262144}
			for field, max := range limits {
				if value, ok := properties[field]; ok {
					value.(schema)["maxLength"] = max
					value.(schema)["x-max-utf8-bytes"] = max
				}
			}
			if v, ok := properties["occurrence_id"]; ok {
				v.(schema)["pattern"] = "^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}(?![\\s\\S])"
				v.(schema)["maxLength"] = 128
			}
		}
		if t.Name() == "ToolInfo" {
			properties["effect"].(schema)["enum"] = []string{"read", "write", "destructive"}
			properties["timeout_seconds"].(schema)["minimum"] = 1
			properties["timeout_seconds"].(schema)["maximum"] = 30
			properties["max_input_bytes"].(schema)["maximum"] = 32768
			properties["max_output_bytes"].(schema)["minimum"] = 1
			properties["max_output_bytes"].(schema)["maximum"] = 262144
		}
		if t.Name() == "PrepareToolExecution" {
			properties["expires_in_seconds"].(schema)["minimum"] = 1
			properties["expires_in_seconds"].(schema)["maximum"] = 86400
			properties["arguments_json"].(schema)["minLength"] = 2
			properties["arguments_json"].(schema)["x-content-json-object"] = true
		}
		if t.Name() == "DecideToolExecution" {
			properties["decision"].(schema)["enum"] = []string{"approve", "deny"}
		}
		if t.Name() == "DispatchToolExecution" {
			properties["acknowledge_cost"].(schema)["const"] = true
		}
		if t.Name() == "DecideToolExecution" || t.Name() == "ReconcileToolExecution" {
			properties["reason"].(schema)["minLength"] = 1
			properties["reason"].(schema)["pattern"] = nonblankPattern
		}
		if t.Name() == "ReconcileToolExecution" || t.Name() == "ToolOutcomeReceipt" {
			properties["outcome"].(schema)["enum"] = []string{"succeeded", "rejected"}
			properties["effect_reference"].(schema)["minLength"] = 1
			properties["effect_reference"].(schema)["pattern"] = nonblankPattern
		}
		if t.Name() == "ToolOutcomeReceipt" {
			properties["method"].(schema)["enum"] = []string{"registered_tool_response", "operator_assertion"}
		}
		if t.Name() == "ToolExecutionView" {
			properties["available_commands"].(schema)["items"] = schema{"type": "string", "enum": []string{"approve", "deny", "dispatch", "reconcile"}}
		}
		if t.Name() == "Agent" || t.Name() == "CreateAgent" || t.Name() == "AgentDefinition" {
			properties["tools"].(schema)["maxItems"] = domain.MaxModelTools
			properties["tools"].(schema)["uniqueItems"] = true
			if v, ok := properties["model_tools"]; ok {
				v.(schema)["maxItems"] = domain.MaxModelTools
			}
		}
		if t.Name() == "ModelToolCall" || t.Name() == "ModelToolDefinition" {
			properties["name"].(schema)["pattern"] = "^[A-Za-z0-9_-]{1,64}(?![\\s\\S])"
			if t.Name() == "ModelToolCall" {
				properties["id"].(schema)["pattern"] = "^[A-Za-z0-9_-]{1,64}(?![\\s\\S])"
			}
		}
		if t.Name() == "ModelToolResultBinding" {
			properties["execution_version"].(schema)["minimum"] = 1
			properties["status"].(schema)["enum"] = []string{"succeeded", "rejected", "denied", "expired", "cancelled"}
		}
		if t.Name() == "ContinueModelRequest" {
			properties["results"].(schema)["minItems"] = 1
			properties["results"].(schema)["maxItems"] = domain.MaxModelToolCalls
			properties["acknowledge_cost"].(schema)["const"] = true
			properties["reason"].(schema)["minLength"] = 1
			properties["reason"].(schema)["maxLength"] = 4096
			properties["reason"].(schema)["x-max-utf8-bytes"] = 4096
			properties["reason"].(schema)["pattern"] = nonblankPattern
		}
		if t.Name() == "CreateTaskAutomation" || t.Name() == "TaskAutomationGrant" {
			properties["mode"].(schema)["enum"] = []string{"read_only", "prepare_and_continue"}
			properties["tool_pins"].(schema)["minItems"] = 1
			properties["tool_pins"].(schema)["maxItems"] = domain.MaxModelTools
			properties["tool_pins"].(schema)["uniqueItems"] = true
			properties["max_model_turns"] = schema{"type": "integer", "minimum": 1, "maximum": domain.MaxModelTurnsPerTask - 1}
			properties["max_tool_executions"] = schema{"type": "integer", "minimum": 1, "maximum": domain.MaxToolExecutionsPerTask}
			properties["max_charge_microusd"].(schema)["not"] = schema{"const": "0"}
		}
		if t.Name() == "CreateTaskAutomation" {
			properties["expires_in_seconds"] = schema{"type": "integer", "minimum": 1, "maximum": domain.MaxAutomationTTLSeconds}
			properties["acknowledge_cost"].(schema)["const"] = true
		}
		if t.Name() == "CreateTaskAutomation" || t.Name() == "RevokeTaskAutomation" || t.Name() == "TaskAutomationGrant" {
			properties["reason"] = schema{"type": "string", "minLength": 1, "maxLength": 4096, "x-max-utf8-bytes": 4096, "pattern": nonblankPattern}
		}
		if t.Name() == "TaskAutomationGrant" {
			properties["status"].(schema)["enum"] = []string{"active", "revoked", "expired", "stopped", "completed"}
		}
		if t.Name() == "AutomationUsage" {
			properties["model_turns"] = schema{"type": "integer", "minimum": 0, "maximum": 31}
			properties["tool_executions"] = schema{"type": "integer", "minimum": 0, "maximum": 32}
		}
		if t.Name() == "TaskAutomationState" {
			properties["signals"].(schema)["maxItems"] = 20
			properties["available_commands"].(schema)["items"] = schema{"type": "string", "enum": []string{"create", "revoke"}}
		}
		if t.Name() == "ProgressSignal" {
			properties["evidence_ids"].(schema)["maxItems"] = 512
			properties["snapshot_digest"].(schema)["pattern"] = "^[0-9a-f]{64}(?![\\s\\S])"
		}
		if t.Name() == "AutomationToolPin" {
			properties["tool_id"] = schema{"type": "string", "minLength": 1, "maxLength": 128, "x-max-utf8-bytes": 128, "pattern": nonblankPattern}
			properties["tool_version"].(schema)["minimum"] = 1
			properties["tool_profile_digest"].(schema)["pattern"] = "^[0-9a-f]{64}(?![\\s\\S])"
		}
		if t.Name() == "RevokeTaskAutomation" {
			properties["grant_id"] = schema{"type": "string", "minLength": 1, "maxLength": 128, "x-max-utf8-bytes": 128, "pattern": nonblankPattern}
		}
		if t.Name() == "OrganizationMember" || t.Name() == "MembershipAuditState" || t.Name() == "CreateOrganizationMember" || t.Name() == "UpdateOrganizationMember" || t.Name() == "RevokeMemberCredentials" || t.Name() == "MembershipAuditEntry" {
			if v, ok := properties["display_name"]; ok {
				v.(schema)["minLength"] = 1
				v.(schema)["maxLength"] = 120
				v.(schema)["x-max-utf8-bytes"] = 120
				v.(schema)["pattern"] = nonblankPattern
			}
			if v, ok := properties["role"]; ok {
				v.(schema)["enum"] = []string{"owner", "admin", "member", "viewer"}
			}
			if v, ok := properties["reason"]; ok {
				v.(schema)["minLength"] = 1
				v.(schema)["maxLength"] = 4096
				v.(schema)["x-max-utf8-bytes"] = 4096
				v.(schema)["pattern"] = nonblankPattern
			}
			for _, name := range []string{"version", "expected_version"} {
				if v, ok := properties[name]; ok {
					v.(schema)["minimum"] = 1
					v.(schema)["maximum"] = 2147483647
				}
			}
		}
		if t.Name() == "CreateOrganizationMember" || t.Name() == "UpdateOrganizationMember" || t.Name() == "RevokeMemberCredentials" {
			result["additionalProperties"] = false
		}
		if t.Name() == "OrganizationMember" {
			properties["available_commands"].(schema)["items"] = schema{"type": "string", "enum": []string{"update", "revoke_credentials"}}
		}
		if t.Name() == "OrganizationMember" || t.Name() == "MembershipCapabilities" {
			properties["assignable_roles"].(schema)["items"] = schema{"type": "string", "enum": []string{"owner", "admin", "member", "viewer"}}
		}
		if t.Name() == "MembershipCapabilities" {
			properties["self_management_allowed"].(schema)["const"] = false
		}
		if t.Name() == "MembershipAuditEntry" {
			properties["action"].(schema)["enum"] = []string{"member.created", "member.updated", "member.credentials_revoked"}
		}
		if value, ok := properties["principal_kind"]; ok {
			value.(schema)["enum"] = []string{"human", "service"}
			if t.Name() == "ServiceAccount" {
				value.(schema)["const"] = "service"
			}
		}
		if securityDTO(t.Name()) {
			for _, name := range []string{"display_name", "reason"} {
				if value, ok := properties[name]; ok {
					bound := 4096
					if name == "display_name" {
						bound = 120
					}
					v := value.(schema)
					v["minLength"] = 1
					v["maxLength"] = bound
					v["x-max-utf8-bytes"] = bound
					v["pattern"] = nonblankPattern
				}
			}
			for _, name := range []string{"version", "expected_version"} {
				if value, ok := properties[name]; ok {
					value.(schema)["minimum"] = 1
					value.(schema)["maximum"] = 2147483647
				}
			}
			for _, name := range []string{"role", "role_ceiling"} {
				if value, ok := properties[name]; ok {
					value.(schema)["enum"] = []string{"member", "viewer"}
				}
			}
			if value, ok := properties["max_service_key_ttl_seconds"]; ok {
				value.(schema)["minimum"] = 1
				value.(schema)["maximum"] = 86400
			}
			if value, ok := properties["mode"]; ok {
				value.(schema)["enum"] = []string{"mixed", "oidc_required"}
			}
			for _, name := range []string{"oidc_issuer", "configured_oidc_issuer"} {
				if value, ok := properties[name]; ok {
					value.(schema)["maxLength"] = 2048
					value.(schema)["x-max-utf8-bytes"] = 2048
				}
			}
			if t.Name() == "OrganizationLoginPolicy" || t.Name() == "UpdateOrganizationLoginPolicy" {
				result["allOf"] = []any{schema{"if": schema{"properties": schema{"mode": schema{"const": "mixed"}}}, "then": schema{"properties": schema{"oidc_issuer": schema{"const": ""}}}}, schema{"if": schema{"properties": schema{"mode": schema{"const": "oidc_required"}}}, "then": schema{"properties": schema{"oidc_issuer": schema{"minLength": 1, "pattern": nonblankPattern}}}}}
			}
			if t.Name() == "OrganizationLoginPolicy" {
				properties["available_commands"].(schema)["items"] = schema{"type": "string", "enum": []string{"update"}}
				properties["readiness_reasons"].(schema)["items"] = schema{"type": "string", "enum": []string{"oidc_not_configured", "browser_owner_required", "issuer_mismatch"}}
			}
			if t.Name() == "ServiceAccount" {
				properties["available_commands"].(schema)["items"] = schema{"type": "string", "enum": []string{"update"}}
				properties["assignable_roles"].(schema)["items"] = schema{"type": "string", "enum": []string{"member", "viewer"}}
			}
			if t.Name() == "ServiceKey" {
				properties["status"].(schema)["enum"] = []string{"active", "expired", "revoked", "unavailable"}
				properties["available_commands"].(schema)["items"] = schema{"type": "string", "enum": []string{"revoke"}}
			}
			if t.Name() == "OrganizationSecurityAuditEntry" {
				properties["actor_type"].(schema)["enum"] = []string{"principal", "trusted_host"}
				properties["object_type"].(schema)["enum"] = []string{"policy", "service_account", "service_key"}
				properties["action"].(schema)["enum"] = []string{"policy.updated", "policy.recovered", "service_account.created", "service_account.updated", "service_key.issued", "service_key.revoked"}
			}
			switch t.Name() {
			case "UpdateOrganizationLoginPolicy", "CreateServiceAccount", "UpdateServiceAccount", "RevokeServiceKey":
				result["additionalProperties"] = false
			}
		}
		if t.Name() == "AvailableModel" || t.Name() == "ModelConnection" {
			modes := []string{domain.ModelAccountingMetered, domain.ModelAccountingLocalUnmetered}
			if t.Name() == "ModelConnection" {
				modes = append(modes, "")
			}
			properties["accounting_mode"].(schema)["enum"] = modes
			properties["max_charge_microusd"].(schema)["minimum"] = 0
			zero := map[string]any{"max_charge_microusd": schema{"const": 0}}
			if t.Name() == "ModelConnection" {
				for _, key := range []string{"input_microusd_per_million", "output_microusd_per_million", "fixed_microusd"} {
					zero[key] = schema{"const": 0}
				}
			}
			result["allOf"] = []any{schema{
				"if":   schema{"required": []string{"accounting_mode"}, "properties": schema{"accounting_mode": schema{"const": domain.ModelAccountingLocalUnmetered}}},
				"then": schema{"properties": zero}, "else": schema{"properties": schema{"max_charge_microusd": schema{"minimum": 1}}},
			}}
			for key, max := range map[string]int64{"max_input_tokens": 1_000_000_000, "max_output_bytes": 262144, "timeout_milliseconds": 600000} {
				properties[key].(schema)["minimum"] = 1
				properties[key].(schema)["maximum"] = max
			}
		}
		return refineCapabilitySchema(t.Name(), refineConfigurationSchema(t.Name(), result))
	}
	panic("unsupported schema type: " + t.String())
}

func securityDTO(name string) bool {
	switch name {
	case "OrganizationLoginPolicy", "UpdateOrganizationLoginPolicy", "ServiceAccount", "CreateServiceAccount", "UpdateServiceAccount", "ServiceKey", "RevokeServiceKey", "OrganizationSecurityAuditState", "OrganizationSecurityAuditEntry":
		return true
	}
	return false
}
func write(path string, value any) {
	if e := os.MkdirAll(filepath.Dir(path), 0755); e != nil {
		panic(e)
	}
	b, e := json.MarshalIndent(value, "", "  ")
	if e != nil {
		panic(e)
	}
	if e = os.WriteFile(path, append(b, '\n'), 0644); e != nil {
		panic(e)
	}
}
func main() {
	write("contracts/event-payloads.json", domain.PublicEventPayloadRegistry())
	write("contracts/event-conformance.json", eventConformance())
	defs := map[string]any{}
	for name, typ := range capabilityDTOs() {
		defs[name] = describe(typ)
	}
	for name, typ := range configurationDTOs() {
		defs[name] = describe(typ)
	}
	for name, t := range map[string]reflect.Type{"Agent": reflect.TypeFor[domain.Agent](), "CreateAgent": reflect.TypeFor[domain.CreateAgent](), "Task": reflect.TypeFor[domain.Task](), "CreateTask": reflect.TypeFor[domain.CreateTask](), "Event": reflect.TypeFor[domain.Event](), "Artifact": reflect.TypeFor[domain.Artifact](), "Run": reflect.TypeFor[domain.Run](), "Operation": reflect.TypeFor[domain.Operation](), "Action": reflect.TypeFor[domain.Action](), "Approval": reflect.TypeFor[domain.Approval](), "ApprovalDecision": reflect.TypeFor[domain.ApprovalDecision](), "Reservation": reflect.TypeFor[domain.Reservation](), "ReserveCostRequest": reflect.TypeFor[domain.ReserveCostRequest](), "CostEvidence": reflect.TypeFor[domain.CostEvidence](), "CostAdjustment": reflect.TypeFor[domain.CostAdjustment](), "LedgerEntry": reflect.TypeFor[domain.LedgerEntry](), "FinancialIncident": reflect.TypeFor[domain.FinancialIncident](), "CostReport": reflect.TypeFor[domain.CostReport](), "AgentDefinition": reflect.TypeFor[domain.AgentDefinition](), "TeamDefinition": reflect.TypeFor[domain.TeamDefinition]()} {
		defs[name] = describe(t)
	}
	defs["AccountingOutcome"] = describe(reflect.TypeFor[domain.AccountingOutcome]())
	defs["CostQuarantine"] = describe(reflect.TypeFor[domain.CostQuarantine]())
	defs["AvailableModel"] = describe(reflect.TypeFor[domain.AvailableModel]())
	defs["TaskCandidate"] = describe(reflect.TypeFor[domain.TaskCandidate]())
	defs["TaskDecision"] = describe(reflect.TypeFor[domain.TaskDecision]())
	defs["ReviewTaskRequest"] = describe(reflect.TypeFor[domain.ReviewTaskRequest]())
	defs["RetryTaskRequest"] = describe(reflect.TypeFor[domain.RetryTaskRequest]())
	defs["CurrentIdentity"] = describe(reflect.TypeFor[domain.CurrentIdentity]())
	for name, t := range map[string]reflect.Type{"TaskEventSnapshot": reflect.TypeFor[domain.TaskEventSnapshot](), "TaskEventBatch": reflect.TypeFor[domain.TaskEventBatch](), "EventReplayWindow": reflect.TypeFor[domain.EventReplayWindow](), "TaskEventStreamFrame": reflect.TypeFor[domain.TaskEventStreamFrame](), "EventStreamError": reflect.TypeFor[domain.EventStreamError](), "APICompatibility": reflect.TypeFor[domain.APICompatibility]()} {
		defs[name] = describe(t)
	}
	for name, t := range map[string]reflect.Type{"OrganizationLoginPolicy": reflect.TypeFor[domain.OrganizationLoginPolicy](), "UpdateOrganizationLoginPolicy": reflect.TypeFor[domain.UpdateOrganizationLoginPolicy](), "ServiceAccount": reflect.TypeFor[domain.ServiceAccount](), "CreateServiceAccount": reflect.TypeFor[domain.CreateServiceAccount](), "UpdateServiceAccount": reflect.TypeFor[domain.UpdateServiceAccount](), "ServiceKey": reflect.TypeFor[domain.ServiceKey](), "RevokeServiceKey": reflect.TypeFor[domain.RevokeServiceKey](), "OrganizationSecurityAuditState": reflect.TypeFor[domain.OrganizationSecurityAuditState](), "OrganizationSecurityAuditEntry": reflect.TypeFor[domain.OrganizationSecurityAuditEntry]()} {
		defs[name] = describe(t)
	}
	for name, t := range map[string]reflect.Type{"OrganizationMember": reflect.TypeFor[domain.OrganizationMember](), "MembershipCapabilities": reflect.TypeFor[domain.MembershipCapabilities](), "CreateOrganizationMember": reflect.TypeFor[domain.CreateOrganizationMember](), "UpdateOrganizationMember": reflect.TypeFor[domain.UpdateOrganizationMember](), "RevokeMemberCredentials": reflect.TypeFor[domain.RevokeMemberCredentials](), "MembershipAuditState": reflect.TypeFor[domain.MembershipAuditState](), "MembershipAuditEntry": reflect.TypeFor[domain.MembershipAuditEntry]()} {
		defs[name] = describe(t)
	}
	for name, t := range map[string]reflect.Type{"AutomationToolPin": reflect.TypeFor[domain.AutomationToolPin](), "TaskAutomationGrant": reflect.TypeFor[domain.TaskAutomationGrant](), "CreateTaskAutomation": reflect.TypeFor[domain.CreateTaskAutomation](), "RevokeTaskAutomation": reflect.TypeFor[domain.RevokeTaskAutomation](), "AutomationUsage": reflect.TypeFor[domain.AutomationUsage](), "ProgressSignal": reflect.TypeFor[domain.ProgressSignal](), "TaskAutomationState": reflect.TypeFor[domain.TaskAutomationState]()} {
		defs[name] = describe(t)
	}
	for name, t := range map[string]reflect.Type{"ModelToolDefinition": reflect.TypeFor[domain.ModelToolDefinition](), "ModelToolCall": reflect.TypeFor[domain.ModelToolCall](), "ModelMessage": reflect.TypeFor[domain.ModelMessage](), "ModelToolProposal": reflect.TypeFor[domain.ModelToolProposal](), "ModelToolResultBinding": reflect.TypeFor[domain.ModelToolResultBinding](), "ContinueModelRequest": reflect.TypeFor[domain.ContinueModelRequest]()} {
		defs[name] = describe(t)
	}
	for name, t := range map[string]reflect.Type{"ToolInfo": reflect.TypeFor[domain.ToolInfo](), "ToolExecution": reflect.TypeFor[domain.ToolExecution](), "ToolOutcomeReceipt": reflect.TypeFor[domain.ToolOutcomeReceipt](), "ToolExecutionView": reflect.TypeFor[domain.ToolExecutionView](), "PrepareToolExecution": reflect.TypeFor[domain.PrepareToolExecution](), "DecideToolExecution": reflect.TypeFor[domain.DecideToolExecution](), "DispatchToolExecution": reflect.TypeFor[domain.DispatchToolExecution](), "ReconcileToolExecution": reflect.TypeFor[domain.ReconcileToolExecution]()} {
		defs[name] = describe(t)
	}
	for name, t := range map[string]reflect.Type{"Material": reflect.TypeFor[domain.Material](), "MaterialRevision": reflect.TypeFor[domain.MaterialRevision](), "MaterialContent": reflect.TypeFor[domain.MaterialContent](), "CreateMaterial": reflect.TypeFor[domain.CreateMaterial](), "ReviseMaterial": reflect.TypeFor[domain.ReviseMaterial](), "PublishMaterial": reflect.TypeFor[domain.PublishMaterial]()} {
		defs[name] = describe(t)
	}
	for _, name := range []string{"CreateTask", "CreateAgent", "ApprovalDecision", "ReserveCostRequest", "CostEvidence", "CostAdjustment", "AgentDefinition", "TeamDefinition"} {
		defs[name].(schema)["additionalProperties"] = false
	}
	for _, name := range []string{"Task", "CreateTask", "Agent", "CreateAgent"} {
		props := defs[name].(schema)["properties"].(map[string]any)
		props["budget_microusd"].(schema)["minimum"] = 1
		props["budget_microusd"].(schema)["maximum"] = 1_000_000_000_000
	}
	for name, kind := range map[string]string{"Task": "task", "Run": "run", "Operation": "operation", "Action": "action", "Reservation": "reservation", "Approval": "approval", "ToolExecution": "tool_execution"} {
		states := []string{}
		for state := range domain.StateMachines[kind] {
			states = append(states, state)
		}
		sort.Strings(states)
		defs[name].(schema)["properties"].(map[string]any)["status"].(schema)["enum"] = states
	}
	defs["PublishAgent"] = schema{"type": "object", "additionalProperties": false, "required": []string{"expected_version"}, "properties": schema{"expected_version": schema{"type": "integer", "minimum": 1}}}
	defs["EmptyCommand"] = schema{"type": "object", "additionalProperties": false}
	errorCodes := []string{}
	for code := range domain.ErrorCatalog {
		errorCodes = append(errorCodes, code)
	}
	sort.Strings(errorCodes)
	defs["Error"] = schema{"type": "object", "required": []string{"code", "message", "request_id", "retryable"}, "properties": schema{"code": schema{"type": "string", "enum": errorCodes}, "message": schema{"type": "string"}, "request_id": schema{"type": "string"}, "retryable": schema{"type": "boolean"}}}
	applyEventSchemas(defs)
	write("contracts/domain.schema.json", schema{"$schema": "https://json-schema.org/draft/2020-12/schema", "$id": "urn:agent-platform:domain:" + domain.ContractVersion, "title": "Agent Platform domain and wire contracts", "$defs": defs})
	write("contracts/state-machines.json", schema{"contract_version": domain.ContractVersion, "transitions": domain.StateMachines, "guards": "See EXECUTION.md and COSTS.md. A listed transition never replaces authorization, fencing, evidence or optimistic concurrency."})
	write("contracts/permissions.json", schema{"contract_version": domain.ContractVersion, "roles": domain.RolePermissions, "service_roles": map[string][]string{"member": domain.EffectivePermissions(domain.Principal{Kind: domain.PrincipalService, Role: "member"}), "viewer": domain.EffectivePermissions(domain.Principal{Kind: domain.PrincipalService, Role: "viewer"})}, "service_credential_semantics": "Immutable role ceiling intersects live service membership. Key expiry/revocation blocks new admissions; accepted work is durable. Disable the account or organization service access to revoke execution consent.", "delegation": "Effective permissions are an intersection with all parent capabilities, never a union. No public role grants trusted external cost ingestion."})
	write("contracts/errors.json", schema{"contract_version": domain.ContractVersion, "errors": domain.ErrorCatalog, "retry": "Read retryable from the response. Never blindly repeat an unknown external effect. SDKs do not automatically retry mutations."})
	paths := map[string]any{}
	catalogOperations := []domain.CapabilityOperation{}
	type endpoint struct {
		path, method, input, output, status string
		list                                bool
	}
	endpoints := []endpoint{
		{"/capabilities", "get", "", "ProductCapabilities", "200", false},
		{"/configuration/catalog", "get", "", "ConfigurationCatalog", "200", false},
		{"/configuration/destinations", "get", "", "ConfigurationDestination", "200", true},
		{"/configuration/resources", "get", "", "ConfigurationResource", "200", true},
		{"/configuration/grants", "get", "", "CapabilityEnvelope", "200", true},
		{"/configuration/grants/{grant_id}", "get", "", "CapabilityEnvelope", "200", false},
		{"/configuration/grants", "post", "CreateConfigurationGrant", "CapabilityEnvelope", "201", false},
		{"/configuration/grants/{grant_id}/revoke", "post", "RevokeConfigurationGrant", "CapabilityEnvelope", "200", false},
		{"/configuration/plans", "get", "", "ConfigurationPlan", "200", true},
		{"/configuration/plans/{plan_id}", "get", "", "ConfigurationPlan", "200", false},
		{"/configuration/plans", "post", "CreateConfigurationPlan", "ConfigurationPlan", "201", false},
		{"/configuration/plans/{plan_id}/validate", "post", "ValidateConfigurationPlan", "ConfigurationValidation", "200", false},
		{"/configuration/plans/{plan_id}/approve", "post", "ApproveConfigurationPlan", "ConfigurationApproval", "201", false},
		{"/configuration/plans/{plan_id}/apply", "post", "ApplyConfigurationPlan", "ConfigurationPlan", "200", false},
		{"/configuration/plans/{plan_id}/verify", "post", "StartConfigurationVerification", "ConfigurationVerification", "201", false},
		{"/configuration/verifications", "get", "", "ConfigurationVerification", "200", true},
		{"/configuration/verifications/{verification_id}", "get", "", "ConfigurationVerification", "200", false},
		{"/configuration/audit", "get", "", "ConfigurationAuditEntry", "200", true},
		{"/compatibility", "get", "", "APICompatibility", "200", false},
		{"/tasks/{id}/event-snapshot", "get", "", "TaskEventSnapshot", "200", false},
		{"/tasks/{id}/event-feed", "get", "", "TaskEventBatch", "200", false},
		{"/tasks/{id}/event-stream", "get", "", "TaskEventStreamFrame", "200", false},
		{"/me", "get", "", "CurrentIdentity", "200", false},
		{"/login-policy", "get", "", "OrganizationLoginPolicy", "200", false},
		{"/login-policy/update", "post", "UpdateOrganizationLoginPolicy", "OrganizationLoginPolicy", "200", false},
		{"/service-accounts", "get", "", "ServiceAccount", "200", true},
		{"/service-accounts", "post", "CreateServiceAccount", "ServiceAccount", "201", false},
		{"/service-accounts/{principal_id}", "get", "", "ServiceAccount", "200", false},
		{"/service-accounts/{principal_id}/update", "post", "UpdateServiceAccount", "ServiceAccount", "200", false},
		{"/service-accounts/{principal_id}/keys", "get", "", "ServiceKey", "200", true},
		{"/service-accounts/{principal_id}/keys/{key_id}", "get", "", "ServiceKey", "200", false},
		{"/service-accounts/{principal_id}/keys/{key_id}/revoke", "post", "RevokeServiceKey", "ServiceKey", "200", false},
		{"/security-audit", "get", "", "OrganizationSecurityAuditEntry", "200", true},
		{"/members/capabilities", "get", "", "MembershipCapabilities", "200", false},
		{"/members", "get", "", "OrganizationMember", "200", true},
		{"/members/{principal_id}", "get", "", "OrganizationMember", "200", false},
		{"/members", "post", "CreateOrganizationMember", "OrganizationMember", "201", false},
		{"/members/{principal_id}/update", "post", "UpdateOrganizationMember", "OrganizationMember", "200", false},
		{"/members/{principal_id}/revoke-credentials", "post", "RevokeMemberCredentials", "OrganizationMember", "200", false},
		{"/membership-audit", "get", "", "MembershipAuditEntry", "200", true},
		{"/tools", "get", "", "ToolInfo", "200", true},
		{"/tasks/{id}/model-tool-proposals", "get", "", "ModelToolProposal", "200", true},
		{"/tasks/{id}/model-tool-proposals/{proposal_id}", "get", "", "ModelToolProposal", "200", false},
		{"/tasks/{id}/continue-model", "post", "ContinueModelRequest", "Task", "200", false},
		{"/tasks/{id}/automation", "get", "", "TaskAutomationState", "200", false},
		{"/tasks/{id}/automation", "post", "CreateTaskAutomation", "TaskAutomationState", "201", false},
		{"/tasks/{id}/automation/revoke", "post", "RevokeTaskAutomation", "TaskAutomationState", "200", false},
		{"/tasks/{id}/progress-signals", "get", "", "ProgressSignal", "200", true},
		{"/tasks/{id}/tool-executions", "get", "", "ToolExecution", "200", true},
		{"/tasks/{id}/tool-executions", "post", "PrepareToolExecution", "ToolExecutionView", "201", false},
		{"/tasks/{id}/tool-executions/{execution_id}", "get", "", "ToolExecutionView", "200", false},
		{"/tasks/{id}/tool-executions/{execution_id}/decide", "post", "DecideToolExecution", "ToolExecutionView", "200", false},
		{"/tasks/{id}/tool-executions/{execution_id}/dispatch", "post", "DispatchToolExecution", "ToolExecutionView", "200", false},
		{"/tasks/{id}/tool-executions/{execution_id}/reconcile", "post", "ReconcileToolExecution", "ToolExecutionView", "200", false},
		{"/models", "get", "", "AvailableModel", "200", true},
		{"/agents", "post", "CreateAgent", "Agent", "201", false}, {"/agents", "get", "", "Agent", "200", true},
		{"/agents/{id}/publish", "post", "PublishAgent", "Agent", "200", false},
		{"/tasks", "post", "CreateTask", "Task", "201", false}, {"/tasks", "get", "", "Task", "200", true},
		{"/tasks/{id}", "get", "", "Task", "200", false}, {"/tasks/{id}/cancel", "post", "EmptyCommand", "Task", "200", false},
		{"/tasks/{id}/events", "get", "", "Event", "200", true}, {"/tasks/{id}/artifacts", "get", "", "Artifact", "200", true},
		{"/tasks/{id}/runs", "get", "", "Run", "200", true}, {"/tasks/{id}/operations", "get", "", "Operation", "200", true},
		{"/tasks/{id}/actions", "get", "", "Action", "200", true}, {"/tasks/{id}/approvals", "get", "", "Approval", "200", true},
		{"/tasks/{id}/costs", "get", "", "CostReport", "200", false},
		{"/tasks/{id}/review", "post", "ReviewTaskRequest", "Task", "200", false},
		{"/tasks/{id}/retry", "post", "RetryTaskRequest", "Task", "200", false},
		{"/tasks/{id}/candidates", "get", "", "TaskCandidate", "200", true},
		{"/tasks/{id}/decisions", "get", "", "TaskDecision", "200", true},
		{"/tasks/{id}/children", "get", "", "Task", "200", true},
		{"/tasks/{id}/artifacts/{artifact_id}", "get", "", "Artifact", "200", false},
		{"/tasks/{id}/materials", "get", "", "Material", "200", true},
		{"/tasks/{id}/materials", "post", "CreateMaterial", "Material", "201", false},
		{"/tasks/{id}/materials/{material_id}", "get", "", "Material", "200", false},
		{"/tasks/{id}/materials/{material_id}/revisions", "get", "", "MaterialRevision", "200", true},
		{"/tasks/{id}/materials/{material_id}/revisions", "post", "ReviseMaterial", "Material", "201", false},
		{"/tasks/{id}/materials/{material_id}/revisions/{version}", "get", "", "MaterialContent", "200", false},
		{"/tasks/{id}/materials/{material_id}/publish", "post", "PublishMaterial", "Task", "200", false},
	}
	for _, ep := range endpoints {
		catalogOperations = append(catalogOperations, capabilityOperation(ep.path, ep.method, ep.input, ep.output, ep.status, ep.list))
		ref := schema{"$ref": "#/components/schemas/" + ep.output}
		var data any = ref
		if ep.list {
			data = schema{"type": "array", "items": ref}
		}
		params := []any{schema{"name": "X-Organization-ID", "in": "header", "required": true, "schema": schema{"type": "string"}}}
		if ep.path == "/compatibility" {
			params = []any{}
		}
		params = append(params, schema{"name": domain.APIContractHeader, "in": "header", "required": ep.path != "/compatibility", "schema": schema{"type": "string", "const": domain.ContractVersion}})
		if strings.Contains(ep.path, "{id}") {
			params = append(params, schema{"name": "id", "in": "path", "required": true, "schema": schema{"type": "string"}})
		}
		for _, name := range []string{"artifact_id", "material_id", "execution_id", "proposal_id", "principal_id", "key_id", "plan_id", "grant_id", "verification_id", "version"} {
			if strings.Contains(ep.path, "{"+name+"}") {
				value := schema{"type": "string", "minLength": 1, "maxLength": 128}
				if name == "version" {
					value = schema{"type": "integer", "minimum": 1, "maximum": domain.MaxMaterialRevisions}
				}
				params = append(params, schema{"name": name, "in": "path", "required": true, "schema": value})
			}
		}
		if strings.HasSuffix(ep.path, "/artifacts/{artifact_id}") {
			params = append(params, schema{"name": "version", "in": "query", "schema": schema{"type": "integer", "minimum": 1, "default": 1}})
		}
		if ep.method == "post" {
			params = append(params, schema{"name": "Idempotency-Key", "in": "header", "required": true, "schema": schema{"type": "string", "minLength": 1, "maxLength": 128}})
		}
		if ep.method == "get" && ep.list {
			defaultLimit := 50
			if strings.HasSuffix(ep.path, "/artifacts") || strings.HasSuffix(ep.path, "/tool-executions") || strings.HasSuffix(ep.path, "/model-tool-proposals") {
				defaultLimit = 20
			}
			params = append(params, schema{"name": "limit", "in": "query", "schema": schema{"type": "integer", "minimum": 1, "maximum": 100, "default": defaultLimit}})
			if strings.HasSuffix(ep.path, "/events") {
				params = append(params, schema{"name": "after", "in": "query", "schema": decimalSchema(9223372036854775807, false)})
			} else {
				params = append(params, schema{"name": "cursor", "in": "query", "schema": schema{"type": "string"}})
			}
		}
		if ep.path == "/membership-audit" || ep.path == "/security-audit" {
			params = append(params, schema{"name": "principal_id", "in": "query", "schema": schema{"type": "string", "minLength": 1, "maxLength": 128}})
		}
		if ep.path == "/configuration/verifications" {
			params = append(params, schema{"name": "plan_id", "in": "query", "schema": schema{"type": "string", "minLength": 1, "maxLength": 128}})
		}
		if strings.HasSuffix(ep.path, "/event-feed") || strings.HasSuffix(ep.path, "/event-stream") {
			params = append(params, schema{"name": "cursor", "in": "query", "schema": schema{"type": "string", "minLength": 1, "maxLength": 1024}})
			if strings.HasSuffix(ep.path, "/event-feed") {
				params = append(params, schema{"name": "limit", "in": "query", "schema": schema{"type": "integer", "minimum": 1, "maximum": 100, "default": 50}})
			} else {
				params = append(params, schema{"name": "Last-Event-ID", "in": "header", "schema": schema{"type": "string", "minLength": 1, "maxLength": 1024}, "description": "Optional reconnect coordinate; must equal query cursor when both are supplied."})
			}
		}
		if strings.HasSuffix(ep.path, "/costs") {
			params = append(params, schema{"name": "limit", "in": "query", "schema": schema{"type": "integer", "minimum": 1, "maximum": 100, "default": 50}})
			for _, name := range []string{"reservation_cursor", "entry_cursor", "incident_cursor", "quarantine_cursor"} {
				params = append(params, schema{"name": name, "in": "query", "schema": schema{"type": "string"}})
			}
		}
		responses := schema{ep.status: schema{"description": "Success", "content": schema{"application/json": schema{"schema": schema{"type": "object", "required": []string{"data"}, "properties": schema{"data": data, "next_cursor": schema{"type": "string"}}}}}}}
		responses[ep.status].(schema)["headers"] = schema{domain.APIContractHeader: schema{"schema": schema{"type": "string", "const": domain.ContractVersion}}}
		if strings.HasSuffix(ep.path, "/event-stream") {
			responses[ep.status].(schema)["description"] = "Bounded 20-second SSE connection. Each data frame is one TaskEventStreamFrame; only successful batches carry an id. Heartbeats are comments. No automatic mutation retry."
			responses[ep.status].(schema)["content"] = schema{"text/event-stream": schema{"schema": ref}}
		}
		for _, status := range []string{"400", "401", "403", "404", "409", "500", "503"} {
			responses[status] = schema{"$ref": "#/components/responses/Error"}
		}
		operation := schema{"operationId": ep.method + strings.NewReplacer("/", "_", "{", "", "}", "").Replace(ep.path), "parameters": params, "responses": responses}
		if ep.path == "/compatibility" {
			operation["security"] = []any{}
		}
		if ep.input != "" {
			operation["requestBody"] = schema{"required": true, "content": schema{"application/json": schema{"schema": schema{"$ref": "#/components/schemas/" + ep.input}}}}
		}
		if paths[ep.path] == nil {
			paths[ep.path] = schema{}
		}
		paths[ep.path].(schema)[ep.method] = operation
	}
	writeCapabilities(catalogOperations)
	write("contracts/openapi.json", schema{"openapi": "3.1.0", "info": schema{"title": "Agent Platform local development API", "version": domain.ContractVersion, "description": "Implemented foundation API only. Full product features are tracked separately; fixture:echo is not real model inference."}, "servers": []any{schema{"url": "/api/v1"}}, "security": []any{schema{"bearerAuth": []string{}}}, "paths": paths, "components": schema{"schemas": defs, "securitySchemes": schema{"bearerAuth": schema{"type": "http", "scheme": "bearer"}}, "responses": schema{"Error": schema{"description": "Application error", "content": schema{"application/json": schema{"schema": schema{"type": "object", "required": []string{"error"}, "properties": schema{"error": schema{"$ref": "#/components/schemas/Error"}}}}}}}}})
}
