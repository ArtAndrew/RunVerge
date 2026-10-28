// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
)

func TestIndependentConfigurationCLIResponseTypedWireControls(t *testing.T) {
	t.Run("mcp_received_protocol_is_string", func(t *testing.T) {
		value := independentConfigurationResponseVerification()
		value.Status = "response_received"
		value.ProtocolVersion = mcpbridge.ProtocolVersion
		value.ToolCount = 2
		value.CatalogDigest = strings.Repeat("c", 64)
		independentConfigurationResponseRun(t, []string{"verifications", "get", "--id", value.ID}, map[string]any{"data": value}, false)
	})
	t.Run("numeric_protocol_is_rejected", func(t *testing.T) {
		value := independentConfigurationResponseVerification()
		value.Status = "response_received"
		value.ProtocolVersion = mcpbridge.ProtocolVersion
		value.CatalogDigest = strings.Repeat("c", 64)
		object := independentConfigurationResponseObject(t, value)
		object["protocol_version"] = 20250326
		independentConfigurationResponseRun(t, []string{"verifications", "get", "--id", value.ID}, map[string]any{"data": object}, true)
	})
	t.Run("model_received_has_no_mcp_protocol", func(t *testing.T) {
		value := independentConfigurationResponseVerification()
		value.Kind, value.Scope.Kind = "model", "model"
		value.Status, value.CostStatus = "response_received", "unknown"
		value.TaskID, value.MaxChargeMicroUSD = "task_one", 17
		value.ResponseDigest = strings.Repeat("b", 64)
		independentConfigurationResponseRun(t, []string{"verifications", "get", "--id", value.ID}, map[string]any{"data": value}, false)
	})
	t.Run("applied_plan_receipt", func(t *testing.T) {
		value := independentConfigurationResponsePlan()
		value.Status, value.Version = "applied", 2
		value.AppliedAt, value.AppliedRevision, value.ReceiptID = &value.CreatedAt, 2, "receipt_one"
		value.AvailableCommands = []string{}
		independentConfigurationResponseRun(t, []string{"plans", "get", "--id", value.ID}, map[string]any{"data": value}, false)
	})
}

func TestIndependentConfigurationCLIResponseBindsCreatedIntentAndVerificationCeiling(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		name := "matching"
		if mismatch {
			name = "mismatched"
		}
		t.Run("create_plan_"+name, func(t *testing.T) {
			value := independentConfigurationResponsePlan()
			active := true
			input := domain.CreateConfigurationPlan{BaseRevision: value.BaseRevision, TTLSeconds: 3600, Reason: value.Reason, Operations: []domain.ConfigurationOperationInput{{ID: "op_one", DestinationID: "dest_one", DestinationVersion: 1, Active: &active}}}
			if mismatch {
				value.Operations[0].Scope.DestinationID = "dest_other"
			}
			file := independentConfigurationResponseWireRequest(t, input)
			independentConfigurationResponseRun(t, []string{"plans", "create", "--file", file, "--key", "create_plan_one"}, map[string]any{"data": value}, mismatch)
		})
		t.Run("create_grant_"+name, func(t *testing.T) {
			value := independentConfigurationResponseGrant()
			input := domain.CreateConfigurationGrant{SubjectID: value.SubjectID, Actions: value.Actions, Destinations: []domain.ConfigurationDestinationRef{{ID: "dest_one", Version: 1}}, TTLSeconds: 3600, MaxPlans: value.MaxPlans, MaxVerifications: value.MaxVerifications, Reason: "Grant exact bounded access"}
			if mismatch {
				value.SubjectID = "service_other"
			}
			file := independentConfigurationResponseWireRequest(t, input)
			independentConfigurationResponseRun(t, []string{"grants", "create", "--file", file, "--key", "create_grant_one"}, map[string]any{"data": value}, mismatch)
		})
		t.Run("model_verify_ceiling_"+name, func(t *testing.T) {
			value := independentConfigurationResponseVerification()
			value.Kind, value.Scope.Kind = "model", "model"
			value.TaskID, value.MaxChargeMicroUSD, value.CostStatus = "task_one", 17, "not_reserved"
			ack := true
			input := domain.StartConfigurationVerification{ExpectedPlanVersion: 2, PlanDigest: strings.Repeat("a", 64), OperationID: value.OperationID, MaxChargeMicroUSD: 17, AcknowledgeCost: &ack, Reason: "One explicitly bounded connection check"}
			if mismatch {
				value.MaxChargeMicroUSD = 18
			}
			file := independentConfigurationResponseWireRequest(t, input)
			independentConfigurationResponseRun(t, []string{"plans", "verify", "--id", "plan_one", "--file", file, "--key", "verify_one"}, map[string]any{"data": value}, mismatch)
		})
	}
}

func independentConfigurationResponseWireRequest(t *testing.T, input any) string {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "request.json")
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return file
}
