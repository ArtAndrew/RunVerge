// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"encoding/json"
	"math"
	"sort"
	"strings"

	"agentplatform.local/platform/internal/domain"
)

func eventFieldSchema(f domain.EventFieldDefinition) schema {
	v := schema{}
	switch f.Kind {
	case "string":
		v["type"] = "string"
		if !f.AllowEmpty {
			v["minLength"] = 1
			v["pattern"] = nonblankPattern
		}
		if f.MaxBytes > 0 {
			v["maxLength"] = f.MaxBytes
			v["x-max-utf8-bytes"] = f.MaxBytes
		}
	case "digest":
		v = schema{"type": "string", "pattern": "^[0-9a-f]{64}(?![\\s\\S])"}
	case "timestamp":
		v = schema{"type": "string", "format": "date-time"}
	case "boolean":
		v["type"] = "boolean"
	case "integer":
		v = schema{"type": "integer", "minimum": f.Minimum, "maximum": f.Maximum}
	case "decimal":
		max := int64(math.MaxInt64)
		if f.MaximumDecimal == "1000000000000" {
			max = domain.MaxMoney
		}
		v = decimalSchema(max, false)
		v["description"] = "Canonical nonnegative decimal integer; full declared range remains exact."
		if f.Minimum > 0 {
			v["not"] = schema{"const": "0"}
		}
	}
	if f.Const != nil {
		v["const"] = f.Const
	}
	if len(f.Enum) > 0 {
		v["enum"] = f.Enum
	}
	return v
}
func eventPayloadSchema(def domain.EventPayloadDefinition) schema {
	props := schema{}
	required := []string{}
	for name, field := range def.Fields {
		props[name] = eventFieldSchema(field)
		if field.Required {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	v := schema{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	if len(def.Variants) > 0 {
		conditions := []any{}
		for _, variant := range def.Variants {
			then := schema{}
			if len(variant.Required) > 0 {
				then["required"] = variant.Required
			}
			if len(variant.Forbidden) > 0 {
				blocked := []any{}
				for _, name := range variant.Forbidden {
					blocked = append(blocked, schema{"required": []string{name}})
				}
				then["not"] = schema{"anyOf": blocked}
			}
			conditions = append(conditions, schema{"if": schema{"properties": schema{variant.Field: schema{"const": variant.Value}}, "required": []string{variant.Field}}, "then": then})
		}
		v["allOf"] = conditions
	}
	return v
}
func publicEventSchema() schema {
	seq := decimalSchema(math.MaxInt64, false)
	seq["not"] = schema{"const": "0"}
	seq["description"] = "Positive PostgreSQL bigint event position as an exact decimal string. Gaps are normal."
	common := schema{"id": schema{"type": "string", "minLength": 1}, "organization_id": schema{"type": "string", "minLength": 1}, "task_id": schema{"type": "string", "minLength": 1}, "sequence": seq, "created_at": schema{"type": "string", "format": "date-time"}}
	baseRequired := []string{"id", "organization_id", "task_id", "sequence", "created_at", "type", "payload_version"}
	variant := func(kind string, version int, payload any) schema {
		props := schema{}
		for k, v := range common {
			props[k] = v
		}
		props["payload_version"] = schema{"const": version}
		props["type"] = schema{"type": "string", "minLength": 1}
		required := append([]string{}, baseRequired...)
		if version == 0 {
			props["legacy_data_json"] = schema{"type": "string", "minLength": 1, "description": "Exact stored JSONB text, opaque and read only. Never coerce its historical numeric values."}
			required = append(required, "legacy_data_json")
		} else {
			props["type"] = schema{"const": kind}
			props["data"] = payload
			required = append(required, "data")
		}
		return schema{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	choices := []any{variant("", 0, nil)}
	registry := domain.PublicEventPayloadRegistry()
	names := []string{}
	for name := range registry.Types {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		choices = append(choices, variant(name, 1, eventPayloadSchema(registry.Types[name])))
	}
	return schema{"oneOf": choices, "x-payload-registry": "event-payloads.json", "description": "Versioned task event: typed version one, or unchanged opaque legacy version zero."}
}

// This shared protocol corpus is also usable by Apache-2.0 client SDKs. It
// covers every registered kind plus historical and deliberately invalid wires.
func eventConformance() schema {
	cases := []any{}
	add := func(name string, valid bool, event map[string]any) {
		cases = append(cases, schema{"name": name, "valid": valid, "event": event})
	}
	event := func(kind string, data map[string]any) map[string]any {
		return map[string]any{"id": "evt_contract", "organization_id": "org_contract", "task_id": "task_contract", "sequence": "9007199254740993", "type": kind, "payload_version": 1, "data": data, "created_at": "2026-10-24T12:00:00Z"}
	}
	registry := domain.PublicEventPayloadRegistry()
	names := []string{}
	for name := range registry.Types {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		def := registry.Types[name]
		data := map[string]any{}
		for field, f := range def.Fields {
			if !f.Required {
				continue
			}
			var v any
			switch f.Kind {
			case "string":
				v = "contract_value"
			case "digest":
				v = strings.Repeat("a", 64)
			case "timestamp":
				v = "2026-10-24T12:00:00Z"
			case "boolean":
				v = false
			case "integer":
				v = f.Minimum
			case "decimal":
				v = "9223372036854775807"
				if f.MaximumDecimal != "9223372036854775807" {
					v = f.MaximumDecimal
				}
			}
			if len(f.Enum) > 0 {
				v = f.Enum[0]
			}
			if f.Const != nil {
				v = f.Const
			}
			data[field] = v
		}
		if name == "operation.claimed" {
			data["synthetic_cost"] = true
		}
		add(name, true, event(name, data))
	}
	add("operation.claimed.model", true, event("operation.claimed", map[string]any{"operation_id": "op_contract", "fencing_token": "9223372036854775807", "attempt": 1, "adapter": "model", "model_connection_id": "model_contract", "model_connection_version": 1}))
	legacy := map[string]any{"id": "evt_legacy", "organization_id": "org_contract", "task_id": "task_contract", "sequence": "9223372036854775807", "type": "historical.unknown", "payload_version": 0, "legacy_data_json": "{\"fence\":9223372036854775807,\"amount\":9007199254740993}", "created_at": "2026-10-24T12:00:00Z"}
	add("legacy.opaque_wide_integer", true, legacy)
	clone := func(v map[string]any) map[string]any {
		b, _ := json.Marshal(v)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return out
	}
	for _, bad := range []struct {
		name     string
		sequence any
	}{{"sequence.numeric", 9007199254740992}, {"sequence.zero", "0"}, {"sequence.leading_zero", "01"}, {"sequence.overflow", "9223372036854775808"}} {
		v := clone(legacy)
		v["sequence"] = bad.sequence
		add(bad.name, false, v)
	}
	v := clone(legacy)
	v["data"] = map[string]any{"fence": 1}
	add("legacy.not_typed", false, v)
	v = event("task.blocked", map[string]any{"reason": "contract"})
	v["payload_version"] = 2
	add("version.unknown", false, v)
	add("type.unknown", false, event("future.event", map[string]any{"reason": "contract"}))
	add("field.unknown", false, event("task.blocked", map[string]any{"reason": "contract", "extra": true}))
	add("field.missing", false, event("task.blocked", map[string]any{}))
	add("field.null", false, event("task.blocked", map[string]any{"reason": nil}))
	add("field.blank", false, event("task.blocked", map[string]any{"reason": " \t"}))
	add("decimal.numeric", false, event("cost.settled", map[string]any{"reservation_id": "res_contract", "actual_microusd": 1, "synthetic_cost": true}))
	add("decimal.noncanonical", false, event("cost.settled", map[string]any{"reservation_id": "res_contract", "actual_microusd": "01", "synthetic_cost": true}))
	add("constant.false_truth", false, event("cost.settled", map[string]any{"reservation_id": "res_contract", "actual_microusd": "1", "synthetic_cost": false}))
	add("variant.missing_model_pin", false, event("operation.claimed", map[string]any{"operation_id": "op_contract", "fencing_token": "1", "attempt": 1, "adapter": "model"}))
	return schema{"contract_version": domain.ContractVersion, "license": "Apache-2.0", "cases": cases}
}

func applyEventSchemas(defs map[string]any) {
	for _, name := range []string{"TaskEventSnapshot", "TaskEventBatch"} {
		def := defs[name].(schema)
		props := def["properties"].(map[string]any)
		props["cursor"] = schema{"type": "string", "maxLength": 1024}
		if name == "TaskEventSnapshot" {
			props["cursor"].(schema)["minLength"] = 1
			props["replay_cursor"] = schema{"type": "string", "minLength": 1, "maxLength": 1024}
		}
		props["events"].(schema)["maxItems"] = 100
		props["window"] = defs["EventReplayWindow"]
		if name == "TaskEventSnapshot" {
			props["task"] = defs["Task"]
			props["events"].(schema)["maxItems"] = domain.EventSnapshotLimit
		}
		if name == "TaskEventBatch" {
			def["if"] = schema{"properties": schema{"reset_required": schema{"const": true}}, "required": []string{"reset_required"}}
			def["then"] = schema{"properties": schema{"cursor": schema{"const": ""}, "events": schema{"maxItems": 0}, "has_more": schema{"const": false}, "reset_reason": schema{"const": "history_expired"}}, "required": []string{"reset_reason"}}
			def["else"] = schema{"properties": schema{"cursor": schema{"minLength": 1}}, "not": schema{"required": []string{"reset_reason"}}}
		}
	}
	frame := defs["TaskEventStreamFrame"].(schema)
	frameProps := frame["properties"].(map[string]any)
	frameProps["batch"] = defs["TaskEventBatch"]
	frameProps["error"] = defs["EventStreamError"]
	frameProps["frame_version"].(schema)["const"] = 1
	choices := []any{}
	for _, kind := range []string{"batch", "reset_required", "error"} {
		required, forbidden := "batch", "error"
		if kind == "error" {
			required, forbidden = "error", "batch"
		}
		props := schema{"kind": schema{"const": kind}}
		if kind != "error" {
			props["batch"] = schema{"properties": schema{"reset_required": schema{"const": kind == "reset_required"}}}
		}
		choices = append(choices, schema{"properties": props, "required": []string{required}, "not": schema{"required": []string{forbidden}}})
	}
	frame["oneOf"] = choices
	errorProps := defs["EventStreamError"].(schema)["properties"].(map[string]any)
	codes := []string{}
	for code := range domain.ErrorCatalog {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	errorProps["code"].(schema)["enum"] = codes
	compat := defs["APICompatibility"].(schema)["properties"].(map[string]any)
	compat["api_version"].(schema)["const"] = domain.ContractVersion
	compat["header_required"].(schema)["const"] = true
	compat["version_header"].(schema)["const"] = domain.APIContractHeader
}
