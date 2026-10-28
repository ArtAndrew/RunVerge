// SPDX-License-Identifier: Elastic-2.0
package capabilities_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/capabilities"
	"agentplatform.local/platform/internal/domain"
)

func independentObservation(t *testing.T, kind, role string) domain.ProductCapabilities {
	t.Helper()
	v, err := capabilities.Observe(domain.Principal{ID: "independent_subject", OrganizationID: "independent_org", Kind: kind, Role: role}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func independentOperation(t *testing.T, v domain.ProductCapabilities, method, path string) domain.CapabilityOperation {
	t.Helper()
	for _, op := range v.Operations {
		if op.Method == method && op.Path == "/api/v1"+path {
			return op
		}
	}
	t.Fatalf("missing implemented operation %s %s", method, path)
	return domain.CapabilityOperation{}
}

func TestIndependentCapabilitiesCatalogueMatchesImplementedOpenAPIAndReferences(t *testing.T) {
	v := independentObservation(t, "human", "viewer")
	raw, err := os.ReadFile("../../contracts/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var api struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err = json.Unmarshal(raw, &api); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for path, methods := range api.Paths {
		for method, op := range methods {
			if op.OperationID != "" {
				want[strings.ToUpper(method)+" /api/v1"+path] = op.OperationID
			}
		}
	}
	if len(v.Operations) != len(want) {
		t.Fatalf("catalogue/OpenAPI count differs: %d/%d", len(v.Operations), len(want))
	}
	for _, op := range v.Operations {
		key := op.Method + " " + op.Path
		if want[key] != op.ID {
			t.Errorf("operation drift: %s = %s, OpenAPI %s", key, op.ID, want[key])
		}
		delete(want, key)
		if op.Path == "/api/v1/tasks/{id}/event-stream" {
			if op.ResponseKind != "sse" || op.RetryPolicy != "scoped_stream_resume" {
				t.Error("SSE lost separate transport semantics")
			}
			for _, b := range op.Bindings {
				if b.Surface == "mcp" && b.Status != "not_exposed" {
					t.Error("SSE falsely advertised as MCP JSON tool")
				}
			}
		}
	}
	if len(want) != 0 {
		t.Errorf("uncovered OpenAPI operations: %v", want)
	}
	raw, err = os.ReadFile("../../contracts/domain.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schemas struct {
		Definitions map[string]json.RawMessage `json:"$defs"`
	}
	if err = json.Unmarshal(raw, &schemas); err != nil {
		t.Fatal(err)
	}
	for _, op := range v.Operations {
		for _, name := range []string{op.InputSchema, op.OutputSchema} {
			if name != "" && schemas.Definitions[name] == nil {
				t.Errorf("%s references missing canonical schema %q", op.ID, name)
			}
		}
	}
	checkDocs := func(refs []string) {
		for _, ref := range refs {
			if _, err := os.Stat(filepath.Join("../..", strings.Split(ref, "#")[0])); err != nil {
				t.Errorf("broken catalogue documentation reference %q", ref)
			}
		}
	}
	for _, op := range v.Operations {
		checkDocs(op.Documentation)
	}
	for _, profile := range v.Profiles {
		checkDocs(profile.Documentation)
	}
	for _, persona := range v.Onboarding {
		checkDocs(persona.Documentation)
	}
}

func TestIndependentCapabilitiesDescriptionsRespectExistingPermissionBoundaries(t *testing.T) {
	v := independentObservation(t, "human", "owner")
	for _, item := range []struct{ method, path, permission string }{
		{"GET", "/login-policy", "identity.manage"},
		{"GET", "/service-accounts/{principal_id}", "identity.manage"},
		{"GET", "/service-accounts/{principal_id}/keys", "identity.manage"},
		{"GET", "/service-accounts/{principal_id}/keys/{key_id}", "identity.manage"},
		{"GET", "/tools", "tasks.read"},
		{"POST", "/tasks/{id}/automation", "tasks.automate_any"},
		{"POST", "/tasks/{id}/automation/revoke", "tasks.automate_any"},
	} {
		op := independentOperation(t, v, item.method, item.path)
		if !reflect.DeepEqual(op.PermissionAlternatives, [][]string{{item.permission}}) {
			t.Errorf("%s %s declares %v; existing guard requires %s", item.method, item.path, op.PermissionAlternatives, item.permission)
		}
		for _, guard := range op.RuntimeGuards {
			if strings.Contains(guard, "self_metadata") {
				t.Errorf("%s invents unsupported service self-metadata admission", item.path)
			}
		}
		if item.path == "/login-policy" && op.AccessModel == "current_membership" {
			t.Error("login policy falsely advertised as membership-only read")
		}
	}
	// These are historical/actor-visible observations. Reading them does not
	// require still-live delegated execution consent or new destination pins.
	for _, path := range []string{"/configuration/catalog", "/configuration/grants", "/configuration/grants/{grant_id}", "/configuration/plans/{plan_id}", "/configuration/verifications/{verification_id}"} {
		op := independentOperation(t, v, "GET", path)
		for _, guard := range op.RuntimeGuards {
			if guard == "human_manager_or_exact_live_grant" || guard == "current_grant_ancestry_and_authority_epochs" || guard == "exact_destination_purpose_and_resource_pins" {
				t.Errorf("%s claims execution guard %q for readable observation/history", path, guard)
			}
		}
	}
	// Deliberately do not infer configuration authority from role permissions.
	for _, path := range []string{"/configuration/plans", "/configuration/plans/{plan_id}/apply", "/configuration/plans/{plan_id}/verify"} {
		op := independentOperation(t, v, "POST", path)
		if op.AccessModel != "scoped_configuration" || len(op.PermissionAlternatives) != 0 {
			t.Errorf("%s replaces exact delegation with role authority", path)
		}
	}
	createAutomation := independentOperation(t, v, "POST", "/tasks/{id}/automation")
	revokeAutomation := independentOperation(t, v, "POST", "/tasks/{id}/automation/revoke")
	if createAutomation.Effect != "queues_external_work" || createAutomation.CostSemantics != "may_reserve_and_queue" {
		t.Error("automation grant creation hides future worker calls and bounded charges")
	}
	if revokeAutomation.Effect != "platform_write" || revokeAutomation.CostSemantics != "none" {
		t.Error("automation revocation incorrectly advertises new paid work")
	}
	for _, op := range v.Operations {
		if op.Method != "GET" {
			continue
		}
		for _, guard := range op.RuntimeGuards {
			if strings.Contains(op.Path, "/materials") && strings.Contains(guard, "task_owner_or_manager") {
				t.Errorf("%s incorrectly restricts org-scoped material reads to owner/manager", op.ID)
			}
			if strings.HasPrefix(op.Path, "/api/v1/members") && guard == "no_self_management_or_owner_escalation" {
				t.Errorf("%s applies self-mutation restriction to an allowed administrative self-read", op.ID)
			}
		}
	}
}

func TestIndependentCapabilitiesObservationCannotGrantAuthorityOrMutateSharedCatalogue(t *testing.T) {
	for _, kind := range []string{"human", "service"} {
		for _, role := range []string{"viewer", "member", "admin", "owner"} {
			if kind == "service" && (role == "admin" || role == "owner") {
				continue
			}
			v := independentObservation(t, kind, role)
			if !v.ReadOnly || v.Assessment != "support_and_role_observation" || !reflect.DeepEqual(v.Identity.Permissions, domain.EffectivePermissions(domain.Principal{Kind: kind, Role: role})) {
				t.Fatal("catalogue manufactured authority")
			}
			if kind == "service" {
				for _, denied := range []string{"tasks.review_own", "tasks.automate_any", "identity.manage", "actions.approve_any", "configuration.manage"} {
					if slices.Contains(v.Identity.Permissions, denied) {
						t.Errorf("service received %s", denied)
					}
				}
			}
		}
	}
	v := independentObservation(t, "human", "viewer")
	v.Operations[0].RuntimeGuards[0] = "mutated_by_caller"
	v.Roles[0].Permissions[0] = "mutated_by_caller"
	if raw, _ := json.Marshal(independentObservation(t, "human", "viewer")); strings.Contains(string(raw), "mutated_by_caller") {
		t.Fatal("one observation mutated future catalogue or shared role map")
	}
	for _, profile := range v.Profiles {
		if slices.Contains([]string{"mcp_extended", "sandbox", "provider"}, profile.ID) && profile.Status != "not_implemented" {
			t.Errorf("unsupported profile %s advertised as %s", profile.ID, profile.Status)
		}
	}
	for _, persona := range v.Onboarding {
		if persona.ID == "cloud_provider" && (persona.Status != "not_implemented" || len(persona.Steps) != 0) {
			t.Error("provider onboarding invented executable lifecycle")
		}
	}
	for _, mutate := range []func(*domain.ProductCapabilities){
		func(v *domain.ProductCapabilities) { v.ReadOnly = false },
		func(v *domain.ProductCapabilities) {
			v.Identity.Permissions = append(v.Identity.Permissions, "identity.manage")
		},
		func(v *domain.ProductCapabilities) { v.RemediationCommand = []string{"sh", "-c", "unexpected"} },
		func(v *domain.ProductCapabilities) {
			v.Onboarding[0].Steps[0].Commands = [][]string{{"node", "-e", "unexpected"}}
		},
		func(v *domain.ProductCapabilities) { v.Operations[0].RuntimeGuards = nil },
		func(v *domain.ProductCapabilities) { v.Operations = append(v.Operations, v.Operations[0]) },
	} {
		bad := independentObservation(t, "human", "viewer")
		mutate(&bad)
		if capabilities.Valid(bad) {
			t.Error("invalid/authority-bearing catalogue accepted")
		}
	}
}
