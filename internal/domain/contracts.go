// SPDX-License-Identifier: Elastic-2.0
package domain

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

var StateMachines = map[string]map[string][]string{
	"task":           {"queued": {"running", "cancelled", "blocked", "failed"}, "running": {"succeeded", "failed", "waiting_input", "waiting_approval", "blocked", "cancelled"}, "waiting_input": {"queued", "cancelled", "succeeded", "failed", "blocked", "waiting_approval"}, "waiting_approval": {"queued", "cancelled", "waiting_input", "blocked"}, "blocked": {"queued", "cancelled", "waiting_input"}, "succeeded": {}, "failed": {}, "cancelled": {}},
	"run":            {"queued": {"running", "cancelled", "blocked"}, "running": {"succeeded", "failed", "cancelled", "blocked", "waiting_input", "waiting_approval"}, "blocked": {"queued", "cancelled", "waiting_input"}, "waiting_input": {"queued", "cancelled", "succeeded", "failed", "waiting_approval"}, "waiting_approval": {"queued", "cancelled", "waiting_input", "blocked"}, "succeeded": {}, "failed": {}, "cancelled": {}},
	"operation":      {"queued": {"leased", "cancelled", "blocked"}, "leased": {"leased", "succeeded", "failed", "cancelled", "blocked"}, "blocked": {"queued", "cancelled", "succeeded", "failed"}, "succeeded": {}, "failed": {}, "cancelled": {}},
	"action":         {"pending": {"sent", "rejected", "confirmed"}, "sent": {"confirmed", "unknown", "rejected"}, "unknown": {"confirmed", "rejected"}, "rejected": {"confirmed"}, "confirmed": {}},
	"reservation":    {"reserved": {"unknown", "settled", "released"}, "unknown": {"settled", "released"}, "settled": {"settled"}, "released": {"settled"}},
	"tool_execution": {"awaiting_approval": {"approved", "denied", "expired", "cancelled"}, "approved": {"queued", "expired", "cancelled"}, "queued": {"leased", "expired", "cancelled", "rejected"}, "leased": {"leased", "sent", "expired", "cancelled", "rejected"}, "sent": {"unknown", "succeeded", "rejected"}, "unknown": {"succeeded", "rejected"}, "succeeded": {}, "rejected": {}, "denied": {}, "expired": {}, "cancelled": {}},
	"approval":       {"pending": {"approved", "denied", "expired"}, "approved": {}, "denied": {}, "expired": {}},
}

func ValidTransition(kind, from, to string) bool {
	for _, next := range StateMachines[kind][from] {
		if next == to {
			return true
		}
	}
	return false
}

var RolePermissions = map[string][]string{
	"viewer": {"tasks.read", "agents.read", "costs.read"},
	"member": {"tasks.read", "tasks.create", "tasks.review_own", "tasks.retry_own", "tasks.continue_own", "tasks.cancel_own", "actions.prepare_own", "actions.dispatch_own", "materials.write_own", "materials.publish_own", "agents.read", "agents.write", "costs.read"},
	"admin":  {"tasks.read", "tasks.create", "tasks.review_own", "tasks.retry_own", "tasks.continue_own", "tasks.cancel_own", "tasks.cancel_any", "tasks.review_any", "tasks.retry_any", "tasks.continue_any", "tasks.automate_any", "actions.prepare_own", "actions.dispatch_own", "materials.write_own", "materials.write_any", "materials.publish_own", "materials.publish_any", "agents.read", "agents.write", "costs.read", "actions.prepare_any", "actions.dispatch_any", "actions.approve_any", "actions.reconcile_any", "identity.manage", "connections.manage", "configuration.manage", "webhooks.manage"},
	"owner":  {"tasks.read", "tasks.create", "tasks.review_own", "tasks.retry_own", "tasks.continue_own", "tasks.cancel_own", "tasks.cancel_any", "tasks.review_any", "tasks.retry_any", "tasks.continue_any", "tasks.automate_any", "actions.prepare_own", "actions.dispatch_own", "materials.write_own", "materials.write_any", "materials.publish_own", "materials.publish_any", "agents.read", "agents.write", "costs.read", "actions.prepare_any", "actions.dispatch_any", "actions.approve_any", "actions.reconcile_any", "identity.manage", "connections.manage", "configuration.manage", "webhooks.manage", "organization.manage"},
}

func HasPermission(role, permission string) bool {
	for _, p := range RolePermissions[role] {
		if p == permission {
			return true
		}
	}
	return false
}

func ValidateTeam(team TeamDefinition) error {
	if len(team.Members) < 1 || len(team.Members) > 32 || team.MaxChildren < 1 || team.MaxChildren > 32 || team.MaxDepth < 1 || team.MaxDepth > MaxTaskDepth {
		return fmt.Errorf("team size/depth is out of bounds")
	}
	if len(team.Members) > team.MaxChildren || team.MaxParallel < 0 || team.MaxParallel > len(team.Members) || (team.Pattern != "parallel" && team.MaxParallel > 1) {
		return fmt.Errorf("team parallelism/children is out of bounds")
	}
	switch team.Pattern {
	case "sequence", "parallel", "delegate_verify":
	default:
		return fmt.Errorf("unsupported team pattern")
	}
	if team.JoinPolicy != "all" && team.JoinPolicy != "allow_partial" {
		return fmt.Errorf("unsupported join policy")
	}
	if team.Pattern == "delegate_verify" && len(team.Members) != 2 {
		return fmt.Errorf("delegate_verify requires worker and verifier")
	}
	seen := map[string]bool{}
	for _, m := range team.Members {
		if m.Task != "" && (strings.TrimSpace(m.Task) == "" || len(m.Task) > 4096 || !utf8.ValidString(m.Task) || strings.ContainsRune(m.Task, 0)) {
			return fmt.Errorf("invalid member task")
		}
		if strings.TrimSpace(m.ID) == "" || len(m.ID) > 128 || !utf8.ValidString(m.ID) || strings.ContainsRune(m.ID, '\x00') || seen[m.ID] || strings.TrimSpace(m.AgentID) == "" || len(m.AgentID) > 128 || !utf8.ValidString(m.AgentID) || strings.ContainsRune(m.AgentID, '\x00') || m.AgentVersion < 1 {
			return fmt.Errorf("invalid or duplicate team member")
		}
		seen[m.ID] = true
	}
	return nil
}
