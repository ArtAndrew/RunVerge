// SPDX-License-Identifier: Elastic-2.0
package capabilities

import (
	"agentplatform.local/platform/internal/domain"
	"strings"
)

// Describe adds explanatory metadata to a canonical implemented HTTP operation.
// These prerequisites are documentation, not an alternate policy evaluator.
func Describe(o domain.CapabilityOperation) domain.CapabilityOperation {
	p := strings.TrimPrefix(o.Path, "/api/v1")
	o.AccessModel = "role_and_resource"
	o.PermissionAlternatives = [][]string{}
	o.RuntimeGuards = []string{"current_authorization", "organization_and_resource_scope"}
	o.Effect = "read_only"
	o.CostSemantics = "none"
	o.RetryPolicy = "read_with_current_authorization"
	o.CapabilityID = "tasks"
	o.Documentation = []string{"contracts/EXECUTION.md"}
	permission := func(values ...string) {
		for _, v := range values {
			o.PermissionAlternatives = append(o.PermissionAlternatives, []string{v})
		}
	}
	if o.Method == "POST" {
		o.Effect = "platform_write"
		o.RetryPolicy = "explicit_same_key_after_inspection"
		o.RuntimeGuards = append(o.RuntimeGuards, "scoped_idempotency_after_current_authorization", "exact_versions_and_current_state")
	}
	switch {
	case p == "/compatibility":
		o.CapabilityID = "discovery"
		o.AccessModel = "public"
		o.RuntimeGuards = []string{"explicit_wire_compatibility"}
		o.Documentation = []string{"contracts/EVENTS.md"}
	case p == "/me" || p == "/capabilities":
		o.CapabilityID = "discovery"
		o.AccessModel = "current_membership"
		o.Documentation = []string{"contracts/CAPABILITIES.md"}
	case strings.HasPrefix(p, "/configuration/"):
		o.CapabilityID = "configuration"
		o.AccessModel = "scoped_configuration"
		o.Documentation = []string{"contracts/CONFIGURATION.md", "docs/CONFIGURATION.md"}
		switch {
		case p == "/configuration/catalog":
			o.AccessModel = "current_membership"
			o.RuntimeGuards = append(o.RuntimeGuards, "observation_not_delegated_authority")
		case o.Method == "GET" && p != "/configuration/destinations" && p != "/configuration/resources":
			o.AccessModel = "role_and_resource"
			o.RuntimeGuards = append(o.RuntimeGuards, "human_manager_or_current_record_visibility", "historical_read_does_not_require_live_grant")
		case o.Method == "GET":
			o.RuntimeGuards = append(o.RuntimeGuards, "human_manager_or_exact_live_inspect_grant", "current_grant_ancestry_and_authority_epochs")
		case strings.HasPrefix(p, "/configuration/grants/") && strings.HasSuffix(p, "/revoke"):
			o.RuntimeGuards = append(o.RuntimeGuards, "human_manager_or_current_grant_issuer_or_subject", "exact_grant_version")
		case p == "/configuration/grants":
			o.RuntimeGuards = append(o.RuntimeGuards, "human_owner_root_or_live_parent_delegate", "attenuated_scope_expiry_and_cumulative_limits")
		default:
			o.RuntimeGuards = append(o.RuntimeGuards, "human_manager_or_exact_live_grant", "current_grant_ancestry_and_authority_epochs", "exact_destination_purpose_and_resource_pins")
		}
		if strings.HasSuffix(p, "/approve") {
			o.AccessModel = "human_owner"
			o.RuntimeGuards = append(o.RuntimeGuards, "human_owner_exact_plan_approval")
		}
		if strings.HasSuffix(p, "/apply") {
			o.RuntimeGuards = append(o.RuntimeGuards, "fresh_static_validation_and_required_owner_approval")
		}
		if strings.HasSuffix(p, "/verify") {
			o.Effect = "queues_external_work"
			o.CostSemantics = "may_reserve_and_queue"
			o.RuntimeGuards = append(o.RuntimeGuards, "explicit_cost_ceiling_and_acknowledgement", "one_verification_per_plan_operation", "unknown_outcome_no_redispatch")
		}
	case strings.HasPrefix(p, "/members") || p == "/membership-audit":
		o.CapabilityID = "membership"
		o.Documentation = []string{"contracts/MEMBERSHIPS.md"}
		if p == "/members/capabilities" {
			o.AccessModel = "current_membership"
		} else {
			permission("identity.manage")
			if o.Method == "POST" {
				o.RuntimeGuards = append(o.RuntimeGuards, "no_self_management_or_owner_escalation")
			}
		}
	case p == "/login-policy" || p == "/login-policy/update" || strings.HasPrefix(p, "/service-accounts") || p == "/security-audit":
		o.CapabilityID = "security"
		o.Documentation = []string{"contracts/SECURITY.md"}
		o.RuntimeGuards = append(o.RuntimeGuards, "human_administrator_required")
		if p == "/login-policy/update" {
			permission("organization.manage")
			o.AccessModel = "human_owner"
		} else {
			permission("identity.manage")
		}
	case p == "/agents" || strings.HasPrefix(p, "/agents/") || p == "/models" || p == "/tools":
		o.CapabilityID = "agents"
		o.Documentation = []string{"contracts/README.md"}
		if o.Method == "GET" {
			if p == "/tools" {
				permission("tasks.read")
			} else {
				permission("agents.read")
			}
		} else {
			permission("agents.write")
			o.RuntimeGuards = append(o.RuntimeGuards, "published_exact_model_tool_or_team_pins")
		}
	case strings.Contains(p, "/materials"):
		o.CapabilityID = "collaboration"
		o.Documentation = []string{"contracts/MATERIALS.md"}
		if o.Method == "GET" {
			permission("tasks.read")
		} else if strings.HasSuffix(p, "/publish") {
			permission("materials.publish_own", "materials.publish_any")
		} else {
			permission("materials.write_own", "materials.write_any")
		}
		if o.Method == "POST" {
			o.RuntimeGuards = append(o.RuntimeGuards, "exact_material_revision_and_task_owner_or_manager")
		}
	case strings.Contains(p, "/tool-executions"):
		o.CapabilityID = "external_actions"
		o.Documentation = []string{"contracts/TOOL-EXECUTIONS.md"}
		switch {
		case o.Method == "GET":
			permission("tasks.read")
		case strings.HasSuffix(p, "/decide"):
			permission("actions.approve_any")
			o.RuntimeGuards = append(o.RuntimeGuards, "exact_human_approval")
		case strings.HasSuffix(p, "/dispatch"):
			permission("actions.dispatch_own", "actions.dispatch_any")
			o.Effect = "queues_external_work"
			o.CostSemantics = "possible_charge_existing_reservation"
			o.RuntimeGuards = append(o.RuntimeGuards, "exact_approval_and_cost_acknowledgement", "short_send_permit", "unknown_outcome_no_redispatch")
		case strings.HasSuffix(p, "/reconcile"):
			permission("actions.reconcile_any")
			o.Effect = "records_observation"
			o.CostSemantics = "observation_no_settlement"
			o.RuntimeGuards = append(o.RuntimeGuards, "observation_is_not_invoice_or_redispatch")
		default:
			permission("actions.prepare_own", "actions.prepare_any")
			o.CostSemantics = "reserves_upper_bound"
			o.RuntimeGuards = append(o.RuntimeGuards, "immutable_occurrence_intent_and_reservation_before_io")
		}
	case strings.Contains(p, "/automation"):
		o.CapabilityID = "automation"
		o.Documentation = []string{"contracts/AUTOMATION.md"}
		if o.Method == "GET" {
			permission("tasks.read")
		} else {
			permission("tasks.automate_any")
			o.RuntimeGuards = append(o.RuntimeGuards, "human_grant_authority_and_exact_task_pins", "lifetime_cumulative_limits")
			if strings.HasSuffix(p, "/automation") {
				o.Effect = "queues_external_work"
				o.CostSemantics = "may_reserve_and_queue"
				o.RuntimeGuards = append(o.RuntimeGuards, "explicit_cost_acknowledgement", "automatic_continuation_within_cumulative_ceilings")
			}
		}
	case strings.HasSuffix(p, "/continue-model"):
		o.CapabilityID = "model_tools"
		o.Documentation = []string{"contracts/MODEL-TOOLS.md"}
		permission("tasks.continue_own", "tasks.continue_any")
		o.Effect = "queues_external_work"
		o.CostSemantics = "may_reserve_and_queue"
		o.RuntimeGuards = append(o.RuntimeGuards, "exact_terminal_proposal_outcomes", "explicit_cost_acknowledgement", "unknown_outcome_no_redispatch")
	case strings.HasSuffix(p, "/costs"):
		o.CapabilityID = "costs"
		o.Documentation = []string{"contracts/COSTS.md"}
		permission("costs.read")
	case strings.Contains(p, "/event-") || strings.HasSuffix(p, "/events"):
		o.CapabilityID = "events"
		o.Documentation = []string{"contracts/EVENTS.md"}
		permission("tasks.read")
		if o.ResponseKind == "sse" {
			o.RetryPolicy = "scoped_stream_resume"
			o.RuntimeGuards = append(o.RuntimeGuards, "bounded_stream_admission_and_scoped_replay_cursor")
		}
	case p == "/tasks" && o.Method == "POST":
		permission("tasks.create")
		o.Effect = "queues_external_work"
		o.CostSemantics = "may_reserve_and_queue"
		o.RuntimeGuards = append(o.RuntimeGuards, "published_agent_pins_and_budget_admission")
	case strings.HasSuffix(p, "/review"):
		permission("tasks.review_own", "tasks.review_any")
		o.RuntimeGuards = append(o.RuntimeGuards, "exact_candidate_artifact_digest", "human_review_authority")
	case strings.HasSuffix(p, "/retry"):
		permission("tasks.retry_own", "tasks.retry_any")
		o.Effect = "queues_external_work"
		o.CostSemantics = "may_reserve_and_queue"
		o.RuntimeGuards = append(o.RuntimeGuards, "explicit_new_attempt_and_cost_acknowledgement", "prior_liabilities_preserved")
	case strings.HasSuffix(p, "/cancel"):
		permission("tasks.cancel_own", "tasks.cancel_any")
		o.RuntimeGuards = append(o.RuntimeGuards, "cascade_current_state_and_prior_liabilities_preserved")
	default:
		permission("tasks.read")
	}
	return o
}
