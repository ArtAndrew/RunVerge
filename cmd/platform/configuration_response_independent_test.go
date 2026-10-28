// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
)

const independentConfigurationResponseOrg = "org_cli_test"

func independentConfigurationResponseObject(t *testing.T, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func independentConfigurationResponseScope() domain.ConfigurationScope {
	return domain.ConfigurationScope{DestinationID: "dest_one", DestinationVersion: 1, Kind: "mcp", TargetID: "slot_one", CredentialPurposeID: "purpose_one", CredentialPurposeVersion: 1}
}

func independentConfigurationResponseCatalog() domain.ConfigurationCatalog {
	return domain.ConfigurationCatalog{OrganizationID: independentConfigurationResponseOrg, Revision: 1, Audience: domain.ConfigurationAudience, Actions: []string{"inspect"}, AvailableCommands: []string{}, MaxOperations: 8, MaxGrantDepth: 4}
}

func independentConfigurationResponseGrant() domain.CapabilityEnvelope {
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	return domain.CapabilityEnvelope{ID: "grant_one", OrganizationID: independentConfigurationResponseOrg, Audience: domain.ConfigurationAudience, IssuerID: "owner_one", SubjectID: "service_one", Version: 1, Actions: []string{"inspect", "plan"}, Scopes: []domain.ConfigurationScope{independentConfigurationResponseScope()}, MaxPlans: 2, MaxVerifications: 2, Status: "active", CreatedAt: now, ExpiresAt: now.Add(time.Hour), AvailableCommands: []string{"revoke"}}
}

func independentConfigurationResponsePlan() domain.ConfigurationPlan {
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	return domain.ConfigurationPlan{ID: "plan_one", OrganizationID: independentConfigurationResponseOrg, ActorID: "owner_one", Version: 1, Status: "planned", BaseRevision: 1, Digest: strings.Repeat("a", 64), Operations: []domain.ConfigurationOperation{{ID: "op_one", Scope: independentConfigurationResponseScope(), Active: true}}, Diff: []domain.ConfigurationDiff{{OperationID: "op_one", Kind: "mcp", TargetID: "slot_one", NewVersion: 1, Active: true, RetiredVersions: []int{}}}, Reason: "Approved bounded plan", CreatedAt: now, ExpiresAt: now.Add(time.Hour), AvailableCommands: []string{"validate"}}
}

func independentConfigurationResponseResource() domain.ConfigurationResource {
	return domain.ConfigurationResource{OrganizationID: independentConfigurationResponseOrg, Kind: "mcp", ID: "slot_one", Version: 1, Scope: independentConfigurationResponseScope(), Active: true, CreatedAt: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)}
}

func independentConfigurationResponseVerification() domain.ConfigurationVerification {
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	return domain.ConfigurationVerification{ID: "verification_one", OrganizationID: independentConfigurationResponseOrg, PlanID: "plan_one", OperationID: "op_one", ActorID: "owner_one", Scope: independentConfigurationResponseScope(), ResourceVersion: 1, Kind: "mcp", Status: "queued", Version: 1, CostStatus: "unmetered", CreatedAt: now, UpdatedAt: now}
}

func independentConfigurationResponseValidation() domain.ConfigurationValidation {
	plan := independentConfigurationResponsePlan()
	return domain.ConfigurationValidation{ID: "validation_one", OrganizationID: independentConfigurationResponseOrg, PlanID: plan.ID, PlanVersion: 1, PlanDigest: plan.Digest, BaseRevision: 1, CurrentRevision: 1, Valid: true, Decision: domain.PolicyDecision{ID: "decision_one", OrganizationID: independentConfigurationResponseOrg, Action: "apply", TargetID: plan.ID, Outcome: "allow", ReasonCodes: []string{}, Obligations: []string{}, ConfigurationRevision: 1, EvaluatedAt: plan.CreatedAt}, Diff: plan.Diff, CreatedAt: plan.CreatedAt}
}

// Each occurrence is a real controlled HTTP response. A rejected result must
// produce no stdout and must never trigger another request.
func independentConfigurationResponseRun(t *testing.T, args []string, envelope any, reject bool) {
	t.Helper()
	configurationCLITestIdentity(t)
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic-cli-test-token" || r.Header.Get("X-Organization-ID") != independentConfigurationResponseOrg {
			t.Error("ordinary scoped credential was not sent")
		}
		w.Header().Set(domain.APIContractHeader, domain.ContractVersion)
		_, _ = w.Write(raw)
	}))
	defer peer.Close()
	args = append(append([]string{}, args...), "--url", peer.URL, "--json")
	var output bytes.Buffer
	err = runConfigurationClient(context.Background(), args, &output)
	if calls.Load() != 1 {
		t.Fatalf("response caused %d requests, want one", calls.Load())
	}
	if reject {
		if err == nil || output.Len() != 0 {
			t.Errorf("invalid success response accepted or printed: err=%v output_bytes=%d", err, output.Len())
		}
	} else if err != nil || !bytes.Equal(bytes.TrimSpace(output.Bytes()), raw) {
		t.Errorf("valid public response rejected or altered: err=%v", err)
	}
}

func TestIndependentConfigurationCLIResponseValidPublicControls(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		data any
	}{
		{"catalog", []string{"catalog"}, independentConfigurationResponseCatalog()},
		{"grant", []string{"grants", "get", "--id", "grant_one"}, independentConfigurationResponseGrant()},
		{"plan", []string{"plans", "get", "--id", "plan_one"}, independentConfigurationResponsePlan()},
		{"resource_list", []string{"resources", "list"}, []domain.ConfigurationResource{independentConfigurationResponseResource()}},
		{"verification", []string{"verifications", "get", "--id", "verification_one"}, independentConfigurationResponseVerification()},
		{"empty_list", []string{"plans", "list"}, []domain.ConfigurationPlan{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			independentConfigurationResponseRun(t, tc.args, map[string]any{"data": tc.data}, false)
		})
	}
}

func TestIndependentConfigurationCLIResponseRequiresPublishedFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		data map[string]any
		omit string
	}{
		{"all_catalog_fields", []string{"catalog"}, map[string]any{}, ""},
		{"catalog_revision", []string{"catalog"}, independentConfigurationResponseObject(t, independentConfigurationResponseCatalog()), "revision"},
		{"grant_scopes", []string{"grants", "get", "--id", "grant_one"}, independentConfigurationResponseObject(t, independentConfigurationResponseGrant()), "scopes"},
		{"plan_digest", []string{"plans", "get", "--id", "plan_one"}, independentConfigurationResponseObject(t, independentConfigurationResponsePlan()), "digest"},
		{"verification_status", []string{"verifications", "get", "--id", "verification_one"}, independentConfigurationResponseObject(t, independentConfigurationResponseVerification()), "status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delete(tc.data, tc.omit)
			independentConfigurationResponseRun(t, tc.args, map[string]any{"data": tc.data}, true)
		})
	}
	t.Run("missing_data", func(t *testing.T) {
		independentConfigurationResponseRun(t, []string{"catalog"}, map[string]any{}, true)
	})
}

func TestIndependentConfigurationCLIResponseBindsOrganizationAndExactObject(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		data map[string]any
		key  string
	}{
		{"catalog_org", []string{"catalog"}, independentConfigurationResponseObject(t, independentConfigurationResponseCatalog()), "organization_id"},
		{"grant_org", []string{"grants", "get", "--id", "grant_one"}, independentConfigurationResponseObject(t, independentConfigurationResponseGrant()), "organization_id"},
		{"grant_id", []string{"grants", "get", "--id", "grant_one"}, independentConfigurationResponseObject(t, independentConfigurationResponseGrant()), "id"},
		{"plan_id", []string{"plans", "get", "--id", "plan_one"}, independentConfigurationResponseObject(t, independentConfigurationResponsePlan()), "id"},
		{"verification_id", []string{"verifications", "get", "--id", "verification_one"}, independentConfigurationResponseObject(t, independentConfigurationResponseVerification()), "id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.data[tc.key] = "another_valid_id"
			independentConfigurationResponseRun(t, tc.args, map[string]any{"data": tc.data}, true)
		})
	}
	t.Run("list_item_org", func(t *testing.T) {
		resource := independentConfigurationResponseResource()
		resource.OrganizationID = "org_other"
		independentConfigurationResponseRun(t, []string{"resources", "list"}, map[string]any{"data": []domain.ConfigurationResource{resource}}, true)
	})
}

func TestIndependentConfigurationCLIResponseRejectsMalformedPublicRecords(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		data map[string]any
		key  string
		bad  any
	}{
		{"grant_audience", []string{"grants", "get", "--id", "grant_one"}, independentConfigurationResponseObject(t, independentConfigurationResponseGrant()), "audience", "platform.unrelated.v1"},
		{"grant_actions", []string{"grants", "get", "--id", "grant_one"}, independentConfigurationResponseObject(t, independentConfigurationResponseGrant()), "actions", []string{"admin"}},
		{"grant_counter_above_cap", []string{"grants", "get", "--id", "grant_one"}, independentConfigurationResponseObject(t, independentConfigurationResponseGrant()), "used_plans", 3},
		{"plan_digest", []string{"plans", "get", "--id", "plan_one"}, independentConfigurationResponseObject(t, independentConfigurationResponsePlan()), "digest", "not-a-digest"},
		{"plan_status", []string{"plans", "get", "--id", "plan_one"}, independentConfigurationResponseObject(t, independentConfigurationResponsePlan()), "status", "succeeded"},
		{"plan_noncanonical_revision", []string{"plans", "get", "--id", "plan_one"}, independentConfigurationResponseObject(t, independentConfigurationResponsePlan()), "base_revision", "01"},
		{"plan_no_operations", []string{"plans", "get", "--id", "plan_one"}, independentConfigurationResponseObject(t, independentConfigurationResponsePlan()), "operations", []any{}},
		{"verification_count", []string{"verifications", "get", "--id", "verification_one"}, independentConfigurationResponseObject(t, independentConfigurationResponseVerification()), "tool_count", 129},
		{"verification_money_above_cap", []string{"verifications", "get", "--id", "verification_one"}, independentConfigurationResponseObject(t, independentConfigurationResponseVerification()), "max_charge_microusd", "1000000000001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.data[tc.key] = tc.bad
			independentConfigurationResponseRun(t, tc.args, map[string]any{"data": tc.data}, true)
		})
	}
	t.Run("resource_scope_target", func(t *testing.T) {
		resource := independentConfigurationResponseResource()
		resource.Scope.TargetID = "unrelated_slot"
		independentConfigurationResponseRun(t, []string{"resources", "list"}, map[string]any{"data": []domain.ConfigurationResource{resource}}, true)
	})
	t.Run("overlong_resource_id", func(t *testing.T) {
		resource := independentConfigurationResponseResource()
		resource.ID, resource.Scope.TargetID = strings.Repeat("a", 129), strings.Repeat("a", 129)
		independentConfigurationResponseRun(t, []string{"resources", "list"}, map[string]any{"data": []domain.ConfigurationResource{resource}}, true)
	})
	t.Run("zero_resource_version", func(t *testing.T) {
		resource := independentConfigurationResponseResource()
		resource.Version = 0
		independentConfigurationResponseRun(t, []string{"resources", "list"}, map[string]any{"data": []domain.ConfigurationResource{resource}}, true)
	})
	t.Run("null_required_scalar", func(t *testing.T) {
		value := independentConfigurationResponseObject(t, independentConfigurationResponseCatalog())
		value["max_operations"] = nil
		independentConfigurationResponseRun(t, []string{"catalog"}, map[string]any{"data": value}, true)
	})
}

func TestIndependentConfigurationCLIResponseBindsValidationToRequest(t *testing.T) {
	for _, variant := range []string{"plan_id", "plan_digest", "decision_org", "decision_target"} {
		t.Run(variant, func(t *testing.T) {
			value := independentConfigurationResponseValidation()
			switch variant {
			case "plan_id":
				value.PlanID = "plan_other"
			case "plan_digest":
				value.PlanDigest = strings.Repeat("b", 64)
			case "decision_org":
				value.Decision.OrganizationID = "org_other"
			case "decision_target":
				value.Decision.TargetID = "plan_other"
			}
			file := filepath.Join(t.TempDir(), "validation.json")
			if err := os.WriteFile(file, []byte(`{"expected_version":1,"plan_digest":"`+strings.Repeat("a", 64)+`"}`), 0600); err != nil {
				t.Fatal(err)
			}
			independentConfigurationResponseRun(t, []string{"plans", "validate", "--id", "plan_one", "--file", file, "--key", "validation_one"}, map[string]any{"data": value}, true)
		})
	}
}

func TestIndependentConfigurationCLIResponseLimitsListMetadata(t *testing.T) {
	t.Run("oversized_cursor", func(t *testing.T) {
		independentConfigurationResponseRun(t, []string{"plans", "list"}, map[string]any{"data": []domain.ConfigurationPlan{}, "next_cursor": strings.Repeat("a", 4097)}, true)
	})
	t.Run("cursor_on_exact_reply", func(t *testing.T) {
		independentConfigurationResponseRun(t, []string{"plans", "get", "--id", "plan_one"}, map[string]any{"data": independentConfigurationResponsePlan(), "next_cursor": "cursor"}, true)
	})
	t.Run("more_than_requested_limit", func(t *testing.T) {
		other := independentConfigurationResponsePlan()
		other.ID = "plan_other"
		independentConfigurationResponseRun(t, []string{"plans", "list", "--limit", "1"}, map[string]any{"data": []domain.ConfigurationPlan{independentConfigurationResponsePlan(), other}}, true)
	})
	t.Run("null_list", func(t *testing.T) {
		independentConfigurationResponseRun(t, []string{"plans", "list"}, map[string]any{"data": nil}, true)
	})
}

func TestIndependentConfigurationCLIResponseNeverPrintsKnownCredentialEcho(t *testing.T) {
	t.Run("private_property", func(t *testing.T) {
		value := independentConfigurationResponseObject(t, independentConfigurationResponseCatalog())
		value["credential_environment"] = "PRIVATE_PROVIDER_KEY"
		independentConfigurationResponseRun(t, []string{"catalog"}, map[string]any{"data": value}, true)
	})
	t.Run("current_platform_token_in_allowed_reason", func(t *testing.T) {
		value := independentConfigurationResponsePlan()
		value.Reason = "synthetic-cli-test-token"
		independentConfigurationResponseRun(t, []string{"plans", "get", "--id", "plan_one"}, map[string]any{"data": value}, true)
	})
}
