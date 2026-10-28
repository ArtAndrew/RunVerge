// SPDX-License-Identifier: Elastic-2.0
package capabilities

import (
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"agentplatform.local/platform/internal/domain"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func text(s string, max int, empty bool) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0) && len(s) <= max && (empty || strings.TrimSpace(s) != "")
}
func texts(a []string, lo, hi, size int) bool {
	if a == nil || len(a) < lo || len(a) > hi {
		return false
	}
	for _, s := range a {
		if !text(s, size, false) {
			return false
		}
	}
	return true
}
func docs(a []string) bool {
	if !texts(a, 1, 8, 256) {
		return false
	}
	for _, s := range a {
		if (!strings.HasPrefix(s, "docs/") && !strings.HasPrefix(s, "contracts/")) || strings.Contains(s, "..") || strings.ContainsAny(s, "\\:#?\r\n") {
			return false
		}
	}
	return true
}
func commands(a [][]string) bool {
	if a == nil || len(a) > 4 {
		return false
	}
	for _, argv := range a {
		if !texts(argv, 1, 20, 512) || argv[0] != "platform" {
			return false
		}
	}
	return true
}
func one(s string, values ...string) bool { return slices.Contains(values, s) }
func effect(s string) bool {
	return one(s, "read_only", "platform_write", "queues_external_work", "external_dispatch", "records_observation")
}

// Valid checks a bounded public observation, never authorizes its operations.
func Valid(v domain.ProductCapabilities) bool {
	if v.CatalogVersion != "1" || v.APIContract != domain.ContractVersion || v.Assessment != "support_and_role_observation" || !v.ReadOnly || v.ObservedAt.IsZero() {
		return false
	}
	p := domain.Principal{ID: v.Identity.PrincipalID, OrganizationID: v.Identity.OrganizationID, Kind: v.Identity.PrincipalKind, Role: v.Identity.Role}
	if !identifier.MatchString(p.ID) || !identifier.MatchString(p.OrganizationID) || !one(p.Kind, "human", "service") || !one(p.Role, "viewer", "member", "admin", "owner") || p.Kind == "service" && !one(p.Role, "viewer", "member") || !reflect.DeepEqual(v.Identity.Permissions, domain.EffectivePermissions(p)) {
		return false
	}
	if !texts(v.AuthorizationRules, 1, 32, 4096) || !texts(v.RemediationCommand, 1, 20, 512) || v.RemediationCommand[0] != "platform" || len(v.Operations) < 1 || len(v.Operations) > 128 || len(v.Profiles) < 1 || len(v.Profiles) > 32 || len(v.Roles) != 6 || len(v.Onboarding) != 6 {
		return false
	}
	ops := map[string]bool{}
	routes := map[string]bool{}
	for _, o := range v.Operations {
		if !identifier.MatchString(o.ID) || ops[o.ID] || !identifier.MatchString(o.CapabilityID) || !one(o.Method, "GET", "POST") || !strings.HasPrefix(o.Path, "/api/v1/") || !text(o.Path, 512, false) || strings.ContainsAny(o.Path, "?#\\\r\n") || !text(o.InputSchema, 128, true) || !text(o.OutputSchema, 128, false) || !one(o.ResponseKind, "object", "list", "sse") || !one(o.AccessModel, "public", "current_membership", "role_and_resource", "scoped_configuration", "human_owner") || !effect(o.Effect) || !one(o.CostSemantics, "none", "may_reserve_and_queue", "reserves_upper_bound", "possible_charge_existing_reservation", "observation_no_settlement") || !one(o.RetryPolicy, "read_with_current_authorization", "explicit_same_key_after_inspection", "scoped_stream_resume") || !oneInt(o.SuccessStatus, 200, 201) || !texts(o.RuntimeGuards, 1, 32, 128) || !docs(o.Documentation) || o.PermissionAlternatives == nil || len(o.PermissionAlternatives) > 4 || len(o.Bindings) != 6 {
			return false
		}
		key := o.Method + " " + o.Path
		if routes[key] {
			return false
		}
		routes[key] = true
		ops[o.ID] = true
		for _, group := range o.PermissionAlternatives {
			if !texts(group, 1, 4, 128) {
				return false
			}
			for _, permission := range group {
				found := false
				for _, list := range domain.RolePermissions {
					found = found || slices.Contains(list, permission)
				}
				if !found {
					return false
				}
			}
		}
		seen := map[string]bool{}
		for _, b := range o.Bindings {
			if !one(b.Surface, "http", "cli", "python", "typescript", "mcp", "workspace") || seen[b.Surface] || !one(b.Status, "available", "operator_opt_in", "not_exposed") || !text(b.Name, 512, b.Status == "not_exposed") || b.Status == "not_exposed" && b.Name != "" {
				return false
			}
			seen[b.Surface] = true
		}
	}
	seen := map[string]bool{}
	for _, p := range v.Profiles {
		if !identifier.MatchString(p.ID) || seen[p.ID] || !text(p.Name, 4096, false) || !text(p.Summary, 4096, false) || !one(p.Status, "implemented", "bounded", "not_implemented") || !texts(p.Constraints, 1, 32, 4096) || !docs(p.Documentation) {
			return false
		}
		seen[p.ID] = true
	}
	seen = map[string]bool{}
	for _, r := range v.Roles {
		key := r.PrincipalKind + ":" + r.Role
		if seen[key] || !one(r.PrincipalKind, "human", "service") || !one(r.Role, "viewer", "member", "admin", "owner") || r.PrincipalKind == "service" && !one(r.Role, "viewer", "member") || !reflect.DeepEqual(r.Permissions, domain.EffectivePermissions(domain.Principal{Kind: r.PrincipalKind, Role: r.Role})) {
			return false
		}
		seen[key] = true
	}
	seen = map[string]bool{}
	for _, o := range v.Onboarding {
		if !identifier.MatchString(o.ID) || seen[o.ID] || !text(o.Persona, 4096, false) || !text(o.Summary, 4096, false) || !one(o.Status, "available", "partial", "not_implemented") || !texts(o.Prerequisites, 1, 32, 4096) || o.Steps == nil || len(o.Steps) > 12 || !docs(o.Documentation) {
			return false
		}
		seen[o.ID] = true
		steps := map[string]bool{}
		for _, s := range o.Steps {
			if !identifier.MatchString(s.ID) || steps[s.ID] || !text(s.Instruction, 4096, false) || !text(s.Actor, 4096, false) || !texts(s.OperationIDs, 0, 16, 128) || !commands(s.Commands) || !effect(s.Effect) {
				return false
			}
			steps[s.ID] = true
			for _, id := range s.OperationIDs {
				if !ops[id] {
					return false
				}
			}
		}
	}
	return true
}
func oneInt(n int, options ...int) bool { return slices.Contains(options, n) }
