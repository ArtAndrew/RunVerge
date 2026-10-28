// SPDX-License-Identifier: Elastic-2.0
package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agentplatform.local/platform/internal/jsonwire"
)

// These declarations describe the public wire format. The generated declaration
// artifact is Apache-2.0 so SDKs can consume it without copying server code.
type EventFieldDefinition struct {
	Kind           string   `json:"kind"`
	Required       bool     `json:"required"`
	MaxBytes       int      `json:"max_bytes,omitempty"`
	AllowEmpty     bool     `json:"allow_empty,omitempty"`
	Minimum        int64    `json:"minimum,omitempty"`
	Maximum        int64    `json:"maximum,omitempty"`
	MaximumDecimal string   `json:"maximum_decimal,omitempty"`
	Const          any      `json:"const,omitempty"`
	Enum           []string `json:"enum,omitempty"`
}
type EventPayloadVariant struct {
	Field     string   `json:"field"`
	Value     string   `json:"value"`
	Required  []string `json:"required,omitempty"`
	Forbidden []string `json:"forbidden,omitempty"`
}
type EventPayloadDefinition struct {
	Fields   map[string]EventFieldDefinition `json:"fields"`
	Variants []EventPayloadVariant           `json:"variants,omitempty"`
}
type EventPayloadRegistry struct {
	APIVersion      string                            `json:"api_version"`
	RegistryVersion int                               `json:"registry_version"`
	License         string                            `json:"license"`
	Types           map[string]EventPayloadDefinition `json:"types"`
}

func payloadFields(spec string) map[string]EventFieldDefinition {
	out := map[string]EventFieldDefinition{}
	for _, part := range strings.Fields(spec) {
		name, kind, _ := strings.Cut(part, ":")
		required := !strings.HasSuffix(name, "?")
		name = strings.TrimSuffix(name, "?")
		f := EventFieldDefinition{Kind: kind, Required: required}
		switch kind {
		case "id":
			f.Kind = "string"
			f.MaxBytes = 128
		case "text":
			f.Kind = "string"
			f.MaxBytes = 4096
		case "emptyid":
			f.Kind = "string"
			f.MaxBytes = 128
			f.AllowEmpty = true
		case "integer":
			f.Maximum = math.MaxInt32
		case "positive":
			f.Kind = "integer"
			f.Minimum = 1
			f.Maximum = math.MaxInt32
		case "money":
			f.Kind = "decimal"
			f.MaximumDecimal = strconv.FormatInt(math.MaxInt64, 10)
		case "budget":
			f.Kind = "decimal"
			f.Minimum = 1
			f.MaximumDecimal = strconv.FormatInt(MaxMoney, 10)
		case "fence":
			f.Kind = "decimal"
			f.Minimum = 1
			f.MaximumDecimal = strconv.FormatInt(math.MaxInt64, 10)
		case "true":
			f.Kind = "boolean"
			f.Const = true
		case "false":
			f.Kind = "boolean"
			f.Const = false
		}
		out[name] = f
	}
	return out
}

// Every production emitter is registered. Unknown kinds or additional fields
// are errors before commit; there is no arbitrary-map escape hatch for v1.
func eventPayloadDefinitions() map[string]EventPayloadDefinition {
	d := map[string]EventPayloadDefinition{}
	add := func(kinds, fields string) {
		for _, k := range strings.Fields(kinds) {
			d[k] = EventPayloadDefinition{Fields: payloadFields(fields)}
		}
	}
	add("cost.quarantined", "reservation_id:id evidence_id:id quarantine_id:id actual_microusd:money reason:text")
	add("cost.settle cost.adjustment cost.quarantine_resolution", "reservation_id:id actual_microusd:money incident:boolean")
	add("automation.granted", "grant_id:id mode:text principal_id:id expires_at:timestamp max_model_turns:positive max_tool_executions:integer max_charge_microusd:budget independent_of_login_session:true acceptance_automated:false")
	add("automation.revoked", "grant_id:id principal_id:id reason:text past_effects_preserved:true")
	add("automation.expired automation.stopped automation.completed", "grant_id:id reason:text snapshot_digest:digest signal_count:integer task_acceptance_changed:false external_call:false")
	add("task.result_decided", "decision_id:id candidate_id:id run_id:id decision:text method:text principal_id:id content_digest:digest costs_unchanged:true")
	add("task.new_attempt", "run_id:id operation_id:id attempt:positive input_digest:digest reason:text acknowledged_new_cost:true prior_liabilities_preserved:true")
	add("task.cancelled", "reason:text liabilities_preserved:true")
	add("operation.claimed", "operation_id:id fencing_token:fence attempt:positive adapter:text synthetic_cost?:true model_connection_id?:id model_connection_version?:positive")
	add("task.blocked", "reason:text")
	add("cost.settled", "reservation_id:id actual_microusd:money synthetic_cost:true")
	add("task.succeeded", "verification:text adapter:text")
	add("cost.fixture_reconciled", "reservation_id:id actual_microusd:money synthetic_cost:true basis:text")
	add("cost.reserved", "reservation_id:id amount_microusd:money category:text price_version:text")
	add("action.dispatched", "action_id:id reservation_id:id dispatch_not_after:timestamp fencing_token:fence")
	add("cost.unknown", "reservation_id:id reason:text")
	add("cost.released", "reservation_id:id basis:text")
	add("material.published", "material_id:id material_version:positive candidate_id:id parent_candidate_id:emptyid review_basis_candidate_id:emptyid artifact_id:id artifact_version:positive content_digest:digest published_by:id new_run_created:false costs_unchanged:true")
	add("material.created", "material_id:id version:positive content_digest:digest principal_id:id draft_only:true costs_unchanged:true")
	add("material.revised", "material_id:id version:positive base_version:positive merge_base_version?:positive content_digest:digest principal_id:id draft_only:true costs_unchanged:true")
	add("model.continuation_queued", "operation_id:id source_operation_id:id source_candidate_id:emptyid run_id:id model_turn:positive messages_digest:digest acknowledged_new_cost:true prior_liabilities_preserved:true reason:text")
	add("model.candidate_created", "artifact_id:id content_digest:digest operation_id:id reservation_id:id verified:false cost_status:text source:text proposal_count:integer")
	add("task.created", "run_id:id agent_version:positive")
	add("team.member_candidate", "run_id:id member_id:id child_task_id:id candidate_id:id content_digest:digest acceptance:text")
	add("task.delegated", "parent_id:id root_id:id member_id:id agent_version:positive state:text")
	add("team.initialized", "run_id:id pattern:text join_policy:text plan_digest:digest all_fixture:boolean")
	add("team.member_started", "parent_id:id parent_run_id:id member_id:id run_id:id operation_id:id input_digest:digest source_candidate_id:emptyid")
	add("team.candidate_created", "candidate_id:id artifact_id:id content_digest:digest run_id:id verified:false complete:boolean all_fixture:boolean synthetic_cost:boolean")
	add("team.blocked", "run_id:id reason:text")
	add("task.decision_inherited", "decision_id:id parent_decision_id:id decision:text candidate_id:id method:text")
	add("tool.prepared", "execution_id:id action_id:id approval_id:id intent_hash:digest reservation_id:id external_call:false")
	add("tool.approval_decided", "execution_id:id approval_id:id decision:text principal_id:id method:text external_call:false")
	add("tool.dispatch_queued", "execution_id:id intent_hash:digest principal_id:id cost_acknowledged:true external_call:false")
	add("tool.operator_reconciled", "execution_id:id receipt_id:id outcome:text principal_id:id method:text financial_settlement:false external_call:false")
	add("tool.cost_released", "execution_id:id reservation_id:id proof:text")
	add("tool.outcome_conflict_overflow", "execution_id:id receipt_id:id retained_conflicts:integer additional_payload_retained:false requires_operator_review:true")
	add("tool.outcome_conflicted", "execution_id:id conflict_id:id receipt_id:id observed_outcome:text effect_reference:text artifact_id:id artifact_version:positive content_digest:digest evidence_digest:digest existing_receipt_unchanged:true financial_settlement:false")
	add("tool.cancelled", "execution_id:id outcome:text liabilities_preserved:boolean")
	add("tool.claimed", "execution_id:id fencing_token:fence attempt:positive")
	add("tool.dispatched", "execution_id:id action_id:id intent_hash:digest dispatch_not_after:timestamp fencing_token:fence")
	add("tool.stopped_unsent", "execution_id:id reason:text external_call:false")
	add("tool.outcome_unknown", "execution_id:id resend_allowed:false reason?:text financial_settlement?:false")
	add("tool.effect_observed", "execution_id:id receipt_id:id outcome:text method:text financial_settlement:false")
	set := func(k, f string, mutate func(*EventFieldDefinition)) {
		def := d[k]
		field := def.Fields[f]
		mutate(&field)
		def.Fields[f] = field
		d[k] = def
	}
	constant := func(k, f string, v any) { set(k, f, func(d *EventFieldDefinition) { d.Const = v }) }
	enum := func(k, f string, v ...string) { set(k, f, func(d *EventFieldDefinition) { d.Enum = v }) }
	constant("cost.quarantined", "reason", "projection_overflow")
	// Ordinary cancellation historically has no explanatory waiting reason.
	set("task.cancelled", "reason", func(f *EventFieldDefinition) { f.AllowEmpty = true })
	constant("task.succeeded", "verification", "exact_goal_echo")
	constant("task.succeeded", "adapter", "fixture:echo")
	constant("cost.fixture_reconciled", "basis", "fixture_per_logical_operation")
	constant("cost.released", "basis", "not_dispatched")
	constant("team.member_candidate", "acceptance", "not_implied")
	constant("task.delegated", "state", "dependency_pending")
	constant("tool.operator_reconciled", "method", "operator_assertion")
	constant("tool.effect_observed", "method", "registered_tool_response")
	constant("tool.cost_released", "proof", "pending_action_never_dispatched")
	constant("tool.outcome_conflict_overflow", "retained_conflicts", 8)
	for _, k := range []string{"material.created", "material.published", "tool.outcome_conflicted"} {
		f := "artifact_version"
		if k == "material.created" {
			f = "version"
		}
		constant(k, f, 1)
	}
	enum("operation.claimed", "adapter", "fixture", "model")
	enum("team.initialized", "pattern", "sequence", "parallel", "delegate_verify")
	enum("team.initialized", "join_policy", "all", "allow_partial")
	for _, k := range []string{"task.result_decided", "task.decision_inherited"} {
		enum(k, "decision", "accept", "reject", "request_changes")
	}
	enum("task.result_decided", "method", "authorized_user_decision")
	enum("task.decision_inherited", "method", "inherited_authorized_user_decision")
	claim := d["operation.claimed"]
	claim.Variants = []EventPayloadVariant{
		{Field: "adapter", Value: "fixture", Required: []string{"synthetic_cost"}, Forbidden: []string{"model_connection_id", "model_connection_version"}},
		{Field: "adapter", Value: "model", Required: []string{"model_connection_id", "model_connection_version"}, Forbidden: []string{"synthetic_cost"}},
	}
	d["operation.claimed"] = claim
	return d
}

var registeredEventPayloads = eventPayloadDefinitions()
var eventDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func PublicEventPayloadRegistry() EventPayloadRegistry {
	// A fresh copy prevents callers from changing the runtime's immutable rules.
	return EventPayloadRegistry{APIVersion: ContractVersion, RegistryVersion: 1, License: "Apache-2.0", Types: eventPayloadDefinitions()}
}

func NormalizeEventPayload(kind string, data any) ([]byte, error) {
	def, ok := registeredEventPayloads[kind]
	if !ok {
		return nil, fmt.Errorf("unregistered event kind %q", kind)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxEventPayloadBytes || !jsonwire.ValidUnicode(raw) {
		return nil, fmt.Errorf("invalid event payload size or Unicode")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("event payload must be an object")
	}
	values := map[string]any{}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, fmt.Errorf("invalid event field")
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate event field")
		}
		seen[name] = true
		field, known := def.Fields[name]
		if !known {
			return nil, fmt.Errorf("unknown event field %s.%s", kind, name)
		}
		var value any
		if err = d.Decode(&value); err != nil {
			return nil, err
		}
		// The old producer explicitly used nil for an absent merge base. Only
		// this optional v1 field is normalized; historical JSON is never rewritten.
		if kind == "material.revised" && name == "merge_base_version" && value == nil {
			continue
		}
		normalized, err := normalizeEventField(field, value)
		if err != nil {
			return nil, fmt.Errorf("event %s.%s: %w", kind, name, err)
		}
		values[name] = normalized
	}
	if _, err = d.Token(); err != nil {
		return nil, err
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("trailing event payload")
	}
	for name, field := range def.Fields {
		if _, present := values[name]; field.Required && !present {
			return nil, fmt.Errorf("missing event field %s.%s", kind, name)
		}
	}
	for _, variant := range def.Variants {
		if values[variant.Field] == variant.Value {
			for _, name := range variant.Required {
				if _, ok := values[name]; !ok {
					return nil, fmt.Errorf("missing event variant field %s", name)
				}
			}
			for _, name := range variant.Forbidden {
				if _, ok := values[name]; ok {
					return nil, fmt.Errorf("forbidden event variant field %s", name)
				}
			}
		}
	}
	out, err := json.Marshal(values)
	if err == nil && len(out) > MaxEventPayloadBytes {
		return nil, fmt.Errorf("normalized event payload too large")
	}
	return out, err
}

// ValidateEventWire validates an already serialized public event. Producer
// normalization accepts integer inputs for known decimal fields; the public
// version-one wire format requires strings and never coerces unsafe numbers.
func ValidateEventWire(event Event) error {
	if event.Sequence <= 0 || event.ID == "" || event.OrganizationID == "" || event.TaskID == "" || event.Type == "" || event.CreatedAt.IsZero() {
		return fmt.Errorf("incomplete event envelope")
	}
	if event.PayloadVersion == 0 {
		if event.LegacyDataJSON == "" || event.Data != nil {
			return fmt.Errorf("legacy event requires opaque JSON string only")
		}
		return nil
	}
	if event.PayloadVersion != 1 || event.Data == nil || event.LegacyDataJSON != "" {
		return fmt.Errorf("unsupported event payload version")
	}
	def, ok := registeredEventPayloads[event.Type]
	if !ok {
		return fmt.Errorf("unsupported event type")
	}
	for name, field := range def.Fields {
		if value, present := event.Data[name]; present && field.Kind == "decimal" {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("event decimal must be an exact string")
			}
		}
		if value, present := event.Data[name]; present && value == nil {
			return fmt.Errorf("wire event field cannot be null")
		}
	}
	_, err := NormalizeEventPayload(event.Type, event.Data)
	return err
}

func DecodeEventWire(raw []byte) (Event, error) {
	var event Event
	if !jsonwire.ValidUnicode(raw) {
		return event, fmt.Errorf("invalid event Unicode")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return event, fmt.Errorf("event must be an object")
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return event, err
		}
		name := key.(string)
		if _, ok := fields[name]; ok {
			return event, fmt.Errorf("duplicate event field")
		}
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return event, err
		}
		fields[name] = value
	}
	if _, err = d.Token(); err != nil {
		return event, err
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return event, fmt.Errorf("trailing event")
	}
	var sequence string
	if json.Unmarshal(fields["sequence"], &sequence) != nil {
		return event, fmt.Errorf("event sequence must be a string")
	}
	n, err := strconv.ParseInt(sequence, 10, 64)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != sequence {
		return event, fmt.Errorf("event sequence must be a canonical positive int64")
	}
	if string(fields["payload_version"]) != "0" && string(fields["payload_version"]) != "1" {
		return event, fmt.Errorf("unsupported event version")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err = decoder.Decode(&event); err != nil {
		return event, err
	}
	if event.PayloadVersion == 0 {
		if _, exists := fields["data"]; exists {
			return event, fmt.Errorf("legacy event cannot contain typed data")
		}
	} else {
		if _, exists := fields["legacy_data_json"]; exists {
			return event, fmt.Errorf("typed event cannot contain legacy data")
		}
		// Validate the original raw payload too, before a duplicate property can
		// disappear in a map decoder. Wire decimals must already be strings.
		if _, err = NormalizeEventPayload(event.Type, json.RawMessage(fields["data"])); err != nil {
			return event, err
		}
	}
	return event, ValidateEventWire(event)
}

func normalizeEventField(field EventFieldDefinition, value any) (any, error) {
	invalid := fmt.Errorf("invalid %s value", field.Kind)
	switch field.Kind {
	case "string", "digest", "timestamp":
		s, ok := value.(string)
		if !ok || strings.ContainsRune(s, '\x00') {
			return nil, invalid
		}
		if !field.AllowEmpty && strings.TrimSpace(s) == "" {
			return nil, invalid
		}
		if field.MaxBytes > 0 && len(s) > field.MaxBytes {
			return nil, invalid
		}
		if field.Kind == "digest" && !eventDigest.MatchString(s) {
			return nil, invalid
		}
		if field.Kind == "timestamp" {
			if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
				return nil, invalid
			}
		}
		if len(field.Enum) > 0 {
			found := false
			for _, allowed := range field.Enum {
				found = found || s == allowed
			}
			if !found {
				return nil, invalid
			}
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return nil, invalid
		}
	case "integer":
		n, ok := value.(json.Number)
		if !ok {
			return nil, invalid
		}
		i, err := strconv.ParseInt(string(n), 10, 64)
		if err != nil || strconv.FormatInt(i, 10) != string(n) || i < field.Minimum || i > field.Maximum {
			return nil, invalid
		}
	case "decimal":
		var s string
		switch v := value.(type) {
		case string:
			s = v
		case json.Number:
			s = string(v)
		default:
			return nil, invalid
		}
		i, err := strconv.ParseInt(s, 10, 64)
		max, me := strconv.ParseInt(field.MaximumDecimal, 10, 64)
		if err != nil || me != nil || strconv.FormatInt(i, 10) != s || i < field.Minimum || i > max {
			return nil, invalid
		}
		value = s
	default:
		return nil, invalid
	}
	if field.Const != nil {
		a, _ := json.Marshal(value)
		b, _ := json.Marshal(field.Const)
		if !bytes.Equal(a, b) {
			return nil, fmt.Errorf("unexpected constant value")
		}
	}
	return value, nil
}
