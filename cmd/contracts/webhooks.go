// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"reflect"
	"sort"
	"strings"

	"agentplatform.local/platform/internal/domain"
)

func webhookDTOs() map[string]reflect.Type {
	return map[string]reflect.Type{
		"WebhookDestination":        reflect.TypeFor[domain.WebhookDestination](),
		"CreateWebhookSubscription": reflect.TypeFor[domain.CreateWebhookSubscription](),
		"RevokeWebhookSubscription": reflect.TypeFor[domain.RevokeWebhookSubscription](),
		"WebhookSubscription":       reflect.TypeFor[domain.WebhookSubscription](),
		"WebhookDelivery":           reflect.TypeFor[domain.WebhookDelivery](),
		"WebhookAttempt":            reflect.TypeFor[domain.WebhookAttempt](),
		"RetryWebhookDelivery":      reflect.TypeFor[domain.RetryWebhookDelivery](),
		"WebhookEnvelope":           reflect.TypeFor[domain.WebhookEnvelope](),
		"WebhookEventMetadata":      reflect.TypeFor[domain.WebhookEventMetadata](),
	}
}

// Exact public fields only: profile/lease/permit/observation are deliberately
// absent. They must never enter schema/SDK/MCP input generation recursively.
func refineWebhookSchema(name string, result schema) schema {
	if _, ok := webhookDTOs()[name]; !ok {
		return result
	}
	props := result["properties"].(map[string]any)
	result["additionalProperties"] = false
	for field, raw := range props {
		v := raw.(schema)
		switch {
		case field == "id" || strings.HasSuffix(field, "_id"):
			v["minLength"], v["maxLength"], v["pattern"] = 1, 128, `^[A-Za-z0-9_-]{1,128}$`
		case strings.HasSuffix(field, "_digest"):
			v["minLength"], v["maxLength"], v["pattern"] = 64, 64, `^[a-f0-9]{64}$`
		case field == "version" || field == "expected_version" || field == "destination_version":
			v["minimum"], v["maximum"] = 1, 2147483647
		case field == "envelope_version":
			v["const"] = 1
		case field == "payload_version":
			v["enum"] = []int{0, 1}
		case field == "ttl_seconds":
			v["minimum"], v["maximum"] = 1, domain.MaxWebhookTTLSeconds
		case field == "max_notifications":
			v["minimum"], v["maximum"] = 1, domain.MaxWebhookNotifications
		case field == "admitted_notifications":
			v["minimum"], v["maximum"] = 0, domain.MaxWebhookNotifications
		case field == "attempt_count" || field == "max_attempts" || field == "number":
			v["minimum"], v["maximum"] = 1, domain.MaxWebhookAttempts
		case field == "unknown_attempts" || field == "ever_unknown_attempts" || field == "acknowledged_attempts":
			v["minimum"], v["maximum"] = 0, domain.MaxWebhookAttempts
		case field == "start_after_sequence" || field == "last_admitted_sequence" || field == "event_sequence" || field == "sequence":
			props[field] = decimalSchema(9223372036854775807, false)
			props[field].(schema)["description"] = "Canonical exact PostgreSQL bigint event position, never a floating-point number."
		case field == "reason":
			v["minLength"], v["maxLength"], v["x-max-utf8-bytes"], v["pattern"] = 1, 4096, 4096, nonblankPattern
		case field == "name":
			v["minLength"], v["maxLength"], v["x-max-utf8-bytes"] = 1, 120, 120
		case field == "acknowledge_metadata_export" || field == "acknowledge_possible_duplicate" || field == "unmetered_approved":
			v["const"] = true
		case field == "event_types":
			kinds := []string{}
			for kind := range domain.PublicEventPayloadRegistry().Types {
				kinds = append(kinds, kind)
			}
			// Deterministic order is required for source-stable generation.
			v["minItems"], v["maxItems"], v["uniqueItems"] = 1, domain.MaxWebhookEventTypes, true
			sort.Strings(kinds)
			v["items"] = schema{"type": "string", "enum": kinds}
		case field == "available_commands":
			v["maxItems"], v["uniqueItems"] = 1, true
			v["items"] = schema{"type": "string", "enum": []string{"revoke", "retry"}}
		case field == "http_status":
			v["anyOf"] = []schema{{"const": 0}, {"minimum": 100, "maximum": 599}}
		case field == "reason_code":
			v["minLength"], v["maxLength"], v["pattern"] = 1, 64, `^[a-z0-9_]{1,64}$`
		case field == "type":
			v["minLength"], v["maxLength"] = 1, 128
		}
	}
	if v, ok := props["status"]; ok {
		states := []string{"queued", "leased", "sent", "unknown", "acknowledged", "not_acknowledged", "stopped", "expired"}
		if name == "WebhookSubscription" {
			states = []string{"active", "exhausted", "revoked", "expired", "invalidated"}
		}
		v.(schema)["enum"] = states
	}
	if name == "WebhookSubscription" {
		props["available_commands"].(schema)["items"] = schema{"type": "string", "const": "revoke"}
	}
	if name == "WebhookDelivery" {
		props["available_commands"].(schema)["items"] = schema{"type": "string", "const": "retry"}
		props["max_attempts"].(schema)["const"] = domain.MaxWebhookAttempts
	}
	return result
}
