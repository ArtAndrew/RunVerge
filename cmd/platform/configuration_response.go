// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"agentplatform.local/platform/internal/capabilities"
	"agentplatform.local/platform/internal/domain"
)

var configurationCLIHexDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

// The CLI prints a success only after checking the public wire contract and
// binding the result to the requested organization/object. This does not grant
// authority, accept a business result, or establish that a provider bill settled.
func configurationCLIValidResponse(raw []byte, envelopeType reflect.Type, cmd configurationCLICommand, organization, id, planFilter, action, token string, limit int, request []byte) bool {
	if !configurationCLIValidJSON(raw, envelopeType) || !configurationCLIResponseValue(raw, envelopeType, organization, token, 0) {
		return false
	}
	var envelope struct {
		Data       json.RawMessage `json:"data"`
		NextCursor *string         `json:"next_cursor"`
	}
	if json.Unmarshal(raw, &envelope) != nil || (envelope.NextCursor != nil && (!cmd.list || len(*envelope.NextCursor) > 4096)) {
		return false
	}
	items := []json.RawMessage{envelope.Data}
	if cmd.list && (json.Unmarshal(envelope.Data, &items) != nil || len(items) > limit) {
		return false
	}
	seen := map[string]bool{}
	for _, item := range items {
		target := reflect.New(cmd.output)
		if json.Unmarshal(item, target.Interface()) != nil || !configurationCLIRecord(target.Elem().Interface()) {
			return false
		}
		var fields struct {
			ID     string `json:"id"`
			PlanID string `json:"plan_id"`
		}
		if json.Unmarshal(item, &fields) != nil || (planFilter != "" && fields.PlanID != planFilter) {
			return false
		}
		if cmd.list && fields.ID != "" {
			// Resource IDs have independently versioned model/MCP slots.
			identity := fields.ID
			if resource, ok := target.Elem().Interface().(domain.ConfigurationResource); ok {
				identity = resource.Kind + ":" + identity + ":" + strconv.Itoa(resource.Version)
			}
			if destination, ok := target.Elem().Interface().(domain.ConfigurationDestination); ok {
				identity += ":" + strconv.Itoa(destination.Version)
			}
			if seen[identity] {
				return false
			}
			seen[identity] = true
		}
		if cmd.exact {
			returnedID := fields.ID
			if action == "validate" || action == "approve" || action == "verify" {
				returnedID = fields.PlanID
			}
			if returnedID != id {
				return false
			}
		}
		if len(request) > 0 && !configurationCLIResponseMatchesRequest(target.Elem().Interface(), action, request) {
			return false
		}
	}
	return true
}

// Check required fields before decoding: Go otherwise silently turns an absent
// boolean, revision or entire object into its zero value. The existing strict
// decoder separately rejects duplicate/unknown fields and invalid Unicode.
func configurationCLIResponseValue(raw json.RawMessage, typ reflect.Type, organization, token string, depth int) bool {
	if depth > 32 || len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeFor[time.Time]() {
		var timestamp time.Time
		return json.Unmarshal(raw, &timestamp) == nil && !timestamp.IsZero()
	}
	switch typ.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil {
			return false
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			tag := strings.Split(field.Tag.Get("json"), ",")
			value, present := object[tag[0]]
			if !present {
				if !slices.Contains(tag[1:], "omitempty") {
					return false
				}
				continue
			}
			if slices.Contains(tag[1:], "string") {
				var number string
				if json.Unmarshal(value, &number) != nil || number == "" || len(number) > 19 || len(number) > 1 && number[0] == '0' {
					return false
				}
				parsed, err := strconv.ParseInt(number, 10, 64)
				if err != nil || parsed < 0 || strconv.FormatInt(parsed, 10) != number || strings.HasSuffix(tag[0], "microusd") && parsed > domain.MaxMoney || tag[0] != "applied_revision" && !strings.HasSuffix(tag[0], "microusd") && parsed == 0 {
					return false
				}
			} else if !configurationCLIResponseValue(value, field.Type, organization, token, depth+1) {
				return false
			}
			if !configurationCLIField(typ.Name(), tag[0], value, organization) {
				return false
			}
		}
	case reflect.Slice:
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return false
		}
		for _, item := range items {
			if !configurationCLIResponseValue(item, typ.Elem(), organization, token, depth+1) {
				return false
			}
		}
	case reflect.String:
		var value string
		if json.Unmarshal(raw, &value) != nil || strings.ContainsRune(value, 0) || token != "" && strings.Contains(value, token) {
			return false
		}
	}
	return true
}

func configurationCLIField(parent, name string, raw json.RawMessage, organization string) bool {
	var text string
	_ = json.Unmarshal(raw, &text)
	if parent == "ProductCapabilities" && name == "catalog_version" {
		return text == "1"
	}
	switch {
	case name == "id" || strings.HasSuffix(name, "_id"):
		return configurationCLIIdentifier.MatchString(text) && (name != "organization_id" || text == organization)
	case name == "digest" || strings.HasSuffix(name, "_digest"):
		return configurationCLIHexDigest.MatchString(text)
	case name == "protocol_version":
		return strings.TrimSpace(text) != "" && len(text) <= 128
	case name == "version" || strings.HasSuffix(name, "_version") || name == "new_version":
		var value int64
		if json.Unmarshal(raw, &value) != nil {
			return false
		}
		if name == "expected_resource_version" || name == "expected_version" && parent == "ConfigurationDiff" {
			return value >= 0 && value <= 2147483646
		}
		return value >= 1 && value <= 2147483647
	case name == "audience":
		return text == domain.ConfigurationAudience
	case name == "kind":
		return text == "model" || text == "mcp"
	case name == "reason":
		return domain.ValidateConfigurationReason(text) == nil
	case name == "status":
		switch parent {
		case "CapabilityEnvelope":
			return slices.Contains([]string{"active", "revoked", "expired", "invalidated"}, text)
		case "ConfigurationPlan":
			return slices.Contains([]string{"planned", "applied", "expired"}, text)
		case "ConfigurationVerification":
			return slices.Contains([]string{"queued", "sent", "response_received", "unknown", "failed", "cancelled"}, text)
		}
	case name == "cost_status":
		return slices.Contains([]string{"not_reserved", "reserved", "unknown", "settled", "released", "unmetered"}, text)
	case name == "outcome" && parent == "PolicyDecision":
		return slices.Contains([]string{"allow", "requires_approval", "deny"}, text)
	case name == "depth" || name == "max_grant_depth" || name == "max_plans" || name == "max_verifications" || name == "used_plans" || name == "used_verifications" || name == "tool_count" || name == "max_operations":
		var value int
		if json.Unmarshal(raw, &value) != nil {
			return false
		}
		max, min := 100, 0
		switch name {
		case "depth", "max_grant_depth":
			max = domain.MaxConfigurationGrantDepth
		case "tool_count":
			max = 128
		case "max_operations":
			max, min = domain.MaxConfigurationOperations, 1
		case "max_plans", "max_verifications":
			min = 1
		}
		return value >= min && value <= max
	case name == "actions" || name == "available_commands" || name == "reason_codes" || name == "obligations":
		var values []string
		if json.Unmarshal(raw, &values) != nil {
			return false
		}
		max := 6
		if name == "reason_codes" || name == "obligations" {
			max = 16
		}
		if len(values) > max {
			return false
		}
		seen := map[string]bool{}
		for _, value := range values {
			if strings.TrimSpace(value) == "" || len(value) > 128 || seen[value] && (name == "actions" || name == "available_commands") || name == "actions" && !domain.ConfigurationActionAllowed(value) {
				return false
			}
			seen[value] = true
		}
	case name == "scopes" || name == "operations" || name == "diff":
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return false
		}
		max := domain.MaxConfigurationOperations
		if parent == "ProductCapabilities" && name == "operations" {
			max = 128
		}
		if name == "scopes" {
			max = domain.MaxConfigurationScopes
		}
		return len(values) >= 1 && len(values) <= max
	case name == "retired_versions":
		var versions []int
		if json.Unmarshal(raw, &versions) != nil {
			return false
		}
		seen := map[int]bool{}
		for _, version := range versions {
			if version < 1 || version > 2147483647 || seen[version] {
				return false
			}
			seen[version] = true
		}
	}
	return true
}

func configurationCLIRecord(value any) bool {
	switch v := value.(type) {
	case domain.ProductCapabilities:
		return capabilities.Valid(v)
	case domain.ConfigurationCatalog:
		return v.MaxOperations == domain.MaxConfigurationOperations && v.MaxGrantDepth == domain.MaxConfigurationGrantDepth
	case domain.CapabilityEnvelope:
		if len(v.Actions) == 0 || v.UsedPlans > v.MaxPlans || v.UsedVerifications > v.MaxVerifications || v.AllocatedChargeMicroUSD > v.MaxAllocatedChargeMicroUSD || !v.ExpiresAt.After(v.CreatedAt) || v.Revoked != (v.Status == "revoked") {
			return false
		}
		seen := map[string]bool{}
		for _, scope := range v.Scopes {
			if seen[scope.DestinationID] {
				return false
			}
			seen[scope.DestinationID] = true
		}
	case domain.ConfigurationResource:
		return v.Kind == v.Scope.Kind && v.ID == v.Scope.TargetID
	case domain.ConfigurationPlan:
		if len(v.Operations) != len(v.Diff) || !v.ExpiresAt.After(v.CreatedAt) || v.Status == "applied" && (v.Version != 2 || v.AppliedAt == nil || v.AppliedRevision == 0 || v.ReceiptID == "") || v.Status != "applied" && (v.Version != 1 || v.AppliedAt != nil || v.AppliedRevision != 0 || v.ReceiptID != "") {
			return false
		}
		seen, targets := map[string]bool{}, map[string]bool{}
		for i, op := range v.Operations {
			diff := v.Diff[i]
			target := op.Scope.Kind + ":" + op.Scope.TargetID
			if seen[op.ID] || targets[target] || diff.OperationID != op.ID || diff.Kind != op.Scope.Kind || diff.TargetID != op.Scope.TargetID || diff.ExpectedVersion != op.ExpectedResourceVersion || diff.NewVersion != op.ExpectedResourceVersion+1 || diff.Active != op.Active {
				return false
			}
			seen[op.ID], targets[target] = true, true
		}
	case domain.ConfigurationValidation:
		return v.Decision.TargetID == v.PlanID && v.Decision.Action == "apply" && v.Decision.ConfigurationRevision == v.CurrentRevision && v.Valid == (v.Decision.Outcome != "deny")
	case domain.ConfigurationVerification:
		return v.Kind == v.Scope.Kind && (v.Kind != "mcp" || v.MaxChargeMicroUSD == 0 && v.CostStatus == "unmetered")
	}
	return true
}

func configurationCLIResponseMatchesRequest(value any, action string, raw []byte) bool {
	var input struct {
		ExpectedVersion     int    `json:"expected_version"`
		ExpectedPlanVersion int    `json:"expected_plan_version"`
		PlanDigest          string `json:"plan_digest"`
		ValidationID        string `json:"validation_id"`
		OperationID         string `json:"operation_id"`
	}
	if json.Unmarshal(raw, &input) != nil {
		return false
	}
	switch v := value.(type) {
	case domain.ConfigurationValidation:
		return v.PlanVersion == input.ExpectedVersion && v.PlanDigest == input.PlanDigest
	case domain.ConfigurationApproval:
		return v.PlanDigest == input.PlanDigest && v.ValidationID == input.ValidationID
	case domain.ConfigurationVerification:
		var in domain.StartConfigurationVerification
		return json.Unmarshal(raw, &in) == nil && v.OperationID == in.OperationID && v.MaxChargeMicroUSD == in.MaxChargeMicroUSD
	case domain.ConfigurationPlan:
		if action == "apply" {
			return v.Digest == input.PlanDigest && v.Version == input.ExpectedVersion+1 && v.Status == "applied"
		}
		var in domain.CreateConfigurationPlan
		if json.Unmarshal(raw, &in) != nil || v.BaseRevision != in.BaseRevision || v.GrantID != in.GrantID || v.Reason != in.Reason || len(v.Operations) != len(in.Operations) {
			return false
		}
		for i, op := range v.Operations {
			requested := in.Operations[i]
			if requested.Active == nil || op.ID != requested.ID || op.Scope.DestinationID != requested.DestinationID || op.Scope.DestinationVersion != requested.DestinationVersion || op.ExpectedResourceVersion != requested.ExpectedResourceVersion || op.Active != *requested.Active {
				return false
			}
		}
	case domain.CapabilityEnvelope:
		if action == "revoke" {
			return v.Version == input.ExpectedVersion+1 && v.Revoked && v.Status == "revoked"
		}
		var in domain.CreateConfigurationGrant
		if json.Unmarshal(raw, &in) != nil || v.SubjectID != in.SubjectID || v.ParentGrantID != in.ParentGrantID || !slices.Equal(v.Actions, in.Actions) || v.MaxPlans != in.MaxPlans || v.MaxVerifications != in.MaxVerifications || v.MaxAllocatedChargeMicroUSD != in.MaxAllocatedChargeMicroUSD || len(v.Scopes) != len(in.Destinations) {
			return false
		}
		for i, scope := range v.Scopes {
			if scope.DestinationID != in.Destinations[i].ID || scope.DestinationVersion != in.Destinations[i].Version {
				return false
			}
		}
	}
	return true
}
