// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

var ErrRecoveryWebhookProfiles = errors.New("recovery historical webhook/profile validation failed")

// validateRecoveryWebhookProfiles validates historical profiles and webhook
// projections, never current permission, activation, wall-clock expiry or keys.
// Caller must already have pinned the schema, enforced declarative constraints
// and foreign keys, and provided exclusive staging access and a bounded context.
// This SELECT-only component neither authenticates provenance nor admits work.
func validateRecoveryWebhookProfiles(ctx context.Context, tx pgx.Tx, schema string) error {
	if ctx == nil || tx == nil || !recoverySnapshotName.MatchString(schema) {
		return ErrRecoveryWebhookProfiles
	}
	for _, table := range []string{"model_connections", "tool_connections", "credential_purpose_profiles", "configuration_destinations", "mcp_connections", "webhook_destinations"} {
		if err := recoveryValidateProfiles(ctx, tx, schema, table); err != nil {
			return err
		}
	}
	if err := recoveryValidateModelTools(ctx, tx, schema); err != nil {
		return err
	}
	if err := recoveryValidateResourceBindings(ctx, tx, schema); err != nil {
		return err
	}
	if err := recoveryValidateWebhookSubscriptions(ctx, tx, schema); err != nil {
		return err
	}
	if err := recoveryValidateWebhookEnvelopes(ctx, tx, schema); err != nil {
		return err
	}
	for _, check := range recoveryWebhookProfileQueries(schema) {
		var valid bool
		if err := tx.QueryRow(ctx, check.sql).Scan(&valid); err != nil || !valid {
			return recoveryWebhookError(ctx)
		}
	}
	return nil
}

func recoveryWebhookError(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrRecoveryWebhookProfiles
}

// Compare JSON values, not jsonb's reordered object bytes. Marshal is the actual
// historical serializer: omitempty legacy fields stay omitted, never defaulted.
func recoveryProfileEqual(raw []byte, value any) bool {
	canonical, err := json.Marshal(value)
	if err != nil {
		return false
	}
	var a, b any
	da, db := json.NewDecoder(bytes.NewReader(raw)), json.NewDecoder(bytes.NewReader(canonical))
	da.UseNumber()
	db.UseNumber()
	return da.Decode(&a) == nil && db.Decode(&b) == nil && reflect.DeepEqual(a, b)
}

type recoveryProfileRow struct {
	OrganizationID           string `json:"organization_id"`
	ID                       string `json:"id"`
	Version                  int    `json:"version"`
	Model                    string `json:"model"`
	ProviderAccount          string `json:"provider_account"`
	PriceVersion             string `json:"price_version"`
	CredentialEnvironment    string `json:"credential_environment"`
	Kind                     string `json:"kind"`
	EndpointDigest           string `json:"endpoint_digest"`
	TargetID                 string `json:"target_id"`
	CredentialPurposeID      string `json:"credential_purpose_id"`
	CredentialPurposeVersion int    `json:"credential_purpose_version"`
	ProfileDigest            string `json:"profile_digest"`
}

func recoveryValidateProfiles(ctx context.Context, tx pgx.Tx, schema, table string) error {
	// table is selected exclusively from the fixed list above.
	rows, err := tx.Query(ctx, "SELECT p.profile,pg_catalog.to_jsonb(p)-'profile' FROM "+pgx.Identifier{schema, table}.Sanitize()+" p ORDER BY organization_id,id,version")
	if err != nil {
		return recoveryWebhookError(ctx)
	}
	defer rows.Close()
	for rows.Next() {
		var raw, meta []byte
		var r recoveryProfileRow
		if rows.Scan(&raw, &meta) != nil || json.Unmarshal(meta, &r) != nil {
			return recoveryWebhookError(ctx)
		}
		valid := false
		switch table {
		case "model_connections":
			var p domain.ModelConnection
			if decodeRecoveryHistoryJSON(raw, &p, 1<<20) == nil && validateModelConnection(p) == nil {
				p.Active = false
				p.CreatedAt = time.Time{}
				valid = p.OrganizationID == r.OrganizationID && p.ID == r.ID && p.Version == r.Version && p.Model == r.Model && p.ProviderAccount == r.ProviderAccount && p.PriceVersion == r.PriceVersion && p.CredentialEnvironment == r.CredentialEnvironment && recoveryProfileEqual(raw, p)
			}
		case "tool_connections":
			var p domain.ToolConnection
			if decodeRecoveryHistoryJSON(raw, &p, 1<<20) == nil {
				n, e := normalizeInstalledToolConnection(p)
				n.Tool.Active = false
				n.Tool.CreatedAt = time.Time{}
				valid = e == nil && n.Tool.OrganizationID == r.OrganizationID && n.Tool.ID == r.ID && n.Tool.Version == r.Version && domain.ToolConnectionDigest(n) == r.ProfileDigest && recoveryProfileEqual(raw, n)
			}
		case "credential_purpose_profiles":
			var p domain.CredentialPurposeProfile
			if decodeRecoveryHistoryJSON(raw, &p, 4096) == nil {
				n, e := normalizeCredentialPurpose(p)
				valid = e == nil && n.OrganizationID == r.OrganizationID && n.ID == r.ID && n.Version == r.Version && n.Kind == r.Kind && n.EndpointDigest == r.EndpointDigest && recoveryProfileEqual(raw, n)
			}
		case "configuration_destinations":
			var p domain.ConfigurationDestinationProfile
			if decodeRecoveryHistoryJSON(raw, &p, 131072) == nil {
				n, _, e := normalizeConfigurationDestination(p)
				d := n.Destination
				valid = e == nil && d.OrganizationID == r.OrganizationID && d.ID == r.ID && d.Version == r.Version && d.Kind == r.Kind && d.TargetID == r.TargetID && d.CredentialPurposeID == r.CredentialPurposeID && d.CredentialPurposeVersion == r.CredentialPurposeVersion && recoveryProfileEqual(raw, n)
			}
		case "mcp_connections":
			var p domain.MCPConnectionProfile
			valid = decodeRecoveryHistoryJSON(raw, &p, 131072) == nil && validateMCPConnectionProfile(p) == nil && configurationIdentifier.MatchString(r.ID) && configurationIdentifier.MatchString(r.OrganizationID) && r.Version > 0 && recoveryProfileEqual(raw, p)
		case "webhook_destinations":
			var p domain.WebhookDestinationProfile
			if decodeRecoveryHistoryJSON(raw, &p, 131072) == nil {
				n, encoded, e := canonicalWebhookProfile(p)
				valid = e == nil && n.Destination.OrganizationID == r.OrganizationID && n.Destination.ID == r.ID && n.Destination.Version == r.Version && Hash(encoded) == r.ProfileDigest && recoveryProfileEqual(raw, n)
			}
		}
		if !valid {
			return recoveryWebhookError(ctx)
		}
	}
	if rows.Err() != nil {
		return recoveryWebhookError(ctx)
	}
	return nil
}

func recoveryValidateModelTools(ctx context.Context, tx pgx.Tx, schema string) error {
	q := `SELECT a.organization_id,a.model_tools,a.tools FROM ` + pgx.Identifier{schema, "agents"}.Sanitize() + ` a`
	rows, err := tx.Query(ctx, q)
	if err != nil {
		return recoveryWebhookError(ctx)
	}
	for rows.Next() {
		var org string
		var raw, rawNames []byte
		var tools []domain.ModelToolDefinition
		if rows.Scan(&org, &raw, &rawNames) != nil || decodeRecoveryHistoryJSON(raw, &tools, 2<<20) != nil || !recoveryProfileEqual(raw, tools) || len(tools) > 16 {
			rows.Close()
			return recoveryWebhookError(ctx)
		}
		// Legacy pre-014 metadata tools with no published model_tools pins
		// stay opaque. A nonempty pin list was published with exact ordered IDs.
		var ordered []string
		if len(tools) > 0 && (decodeRecoveryHistoryJSON(rawNames, &ordered, 32768) != nil || len(ordered) != len(tools)) {
			rows.Close()
			return recoveryWebhookError(ctx)
		}
		ids, names := map[string]bool{}, map[string]bool{}
		for i, p := range tools {
			if p.ToolID != ordered[i] || ids[p.ToolID] || names[p.Name] || p.Name != domain.ModelToolName(org, p.ToolID, p.ToolVersion) || p.ToolVersion < 1 || !configurationDigest.MatchString(p.ToolProfileDigest) {
				rows.Close()
				return recoveryWebhookError(ctx)
			}
			ids[p.ToolID] = true
			names[p.Name] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return recoveryWebhookError(ctx)
	}
	q = `SELECT x.value,t.profile,t.profile_digest FROM ` + pgx.Identifier{schema, "agents"}.Sanitize() + ` a CROSS JOIN LATERAL pg_catalog.jsonb_array_elements(a.model_tools) x(value) LEFT JOIN ` + pgx.Identifier{schema, "tool_connections"}.Sanitize() + ` t ON t.organization_id=a.organization_id AND t.id=x.value->>'tool_id' AND t.version=(x.value->>'tool_version')::integer`
	rows, err = tx.Query(ctx, q)
	if err != nil {
		return recoveryWebhookError(ctx)
	}
	defer rows.Close()
	for rows.Next() {
		var raw, profile []byte
		var digest *string
		var pin domain.ModelToolDefinition
		var p domain.ToolConnection
		if rows.Scan(&raw, &profile, &digest) != nil || digest == nil || decodeRecoveryHistoryJSON(raw, &pin, 2<<20) != nil || decodeRecoveryHistoryJSON(profile, &p, 1<<20) != nil || pin.ToolProfileDigest != *digest || pin.Description != p.Tool.Description || pin.InputSchemaJSON != p.Tool.InputSchemaJSON {
			return recoveryWebhookError(ctx)
		}
	}
	if rows.Err() != nil {
		return recoveryWebhookError(ctx)
	}
	return nil
}

func recoveryValidateResourceBindings(ctx context.Context, tx pgx.Tx, schema string) error {
	q := `SELECT b.kind,b.resource_version,d.profile,p.profile,CASE b.kind WHEN 'model' THEN m.profile ELSE c.profile END
 FROM ` + pgx.Identifier{schema, "configuration_resource_bindings"}.Sanitize() + ` b
 JOIN ` + pgx.Identifier{schema, "configuration_destinations"}.Sanitize() + ` d ON d.organization_id=b.organization_id AND d.id=b.destination_id AND d.version=b.destination_version
 JOIN ` + pgx.Identifier{schema, "credential_purpose_profiles"}.Sanitize() + ` p ON p.organization_id=b.organization_id AND p.id=b.credential_purpose_id AND p.version=b.credential_purpose_version
 LEFT JOIN ` + pgx.Identifier{schema, "model_connections"}.Sanitize() + ` m ON b.kind='model' AND m.organization_id=b.organization_id AND m.id=b.target_id AND m.version=b.resource_version
 LEFT JOIN ` + pgx.Identifier{schema, "mcp_connections"}.Sanitize() + ` c ON b.kind='mcp' AND c.organization_id=b.organization_id AND c.id=b.target_id AND c.version=b.resource_version`
	rows, err := tx.Query(ctx, q)
	if err != nil {
		return recoveryWebhookError(ctx)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var version int
		var dest, purpose, actual []byte
		var d domain.ConfigurationDestinationProfile
		var p domain.CredentialPurposeProfile
		if rows.Scan(&kind, &version, &dest, &purpose, &actual) != nil || decodeRecoveryHistoryJSON(dest, &d, 131072) != nil || decodeRecoveryHistoryJSON(purpose, &p, 4096) != nil {
			return recoveryWebhookError(ctx)
		}
		if kind == "model" && d.Model != nil {
			m := *d.Model
			m.Version = version
			m.CredentialEnvironment = p.CredentialEnvironment
			if !recoveryProfileEqual(actual, m) {
				return recoveryWebhookError(ctx)
			}
		} else if kind == "mcp" && d.MCP != nil {
			if !recoveryProfileEqual(actual, *d.MCP) {
				return recoveryWebhookError(ctx)
			}
		} else {
			return recoveryWebhookError(ctx)
		}
	}
	if rows.Err() != nil {
		return recoveryWebhookError(ctx)
	}
	return nil
}

func recoveryValidateWebhookSubscriptions(ctx context.Context, tx pgx.Tx, schema string) error {
	rows, err := tx.Query(ctx, "SELECT destination_id,destination_version,event_types,max_notifications FROM "+pgx.Identifier{schema, "webhook_subscriptions"}.Sanitize())
	if err != nil {
		return recoveryWebhookError(ctx)
	}
	defer rows.Close()
	for rows.Next() {
		var in domain.CreateWebhookSubscription
		if rows.Scan(&in.DestinationID, &in.DestinationVersion, &in.EventTypes, &in.MaxNotifications) != nil {
			return recoveryWebhookError(ctx)
		}
		in.TTLSeconds = 1
		in.AcknowledgeMetadataExport = true
		in.Reason = "Historical static metadata contract"
		if domain.ValidateCreateWebhookSubscription(in) != nil {
			return recoveryWebhookError(ctx)
		}
	}
	if rows.Err() != nil {
		return recoveryWebhookError(ctx)
	}
	return nil
}

func recoveryValidateWebhookEnvelopes(ctx context.Context, tx pgx.Tx, schema string) error {
	q := `SELECT d.id,d.organization_id,d.subscription_id,d.envelope,d.envelope_digest,e.id,e.task_id,e.sequence,e.type,e.payload_version,e.created_at FROM ` + pgx.Identifier{schema, "webhook_deliveries"}.Sanitize() + ` d JOIN ` + pgx.Identifier{schema, "events"}.Sanitize() + ` e ON e.organization_id=d.organization_id AND e.id=d.event_id`
	rows, err := tx.Query(ctx, q)
	if err != nil {
		return recoveryWebhookError(ctx)
	}
	defer rows.Close()
	for rows.Next() {
		var id, org, sub, body, digest string
		var event domain.WebhookEventMetadata
		var envelope domain.WebhookEnvelope
		if rows.Scan(&id, &org, &sub, &body, &digest, &event.ID, &event.TaskID, &event.Sequence, &event.Type, &event.PayloadVersion, &event.CreatedAt) != nil || decodeRecoveryHistoryJSON([]byte(body), &envelope, 4096) != nil || Hash([]byte(body)) != digest || envelope.EnvelopeVersion != 1 || envelope.DeliveryID != id || envelope.OrganizationID != org || envelope.SubscriptionID != sub {
			return recoveryWebhookError(ctx)
		}
		actual := envelope.Event
		actual.CreatedAt = event.CreatedAt
		if actual != event || !envelope.Event.CreatedAt.Equal(event.CreatedAt) {
			return recoveryWebhookError(ctx)
		}
	}
	if rows.Err() != nil {
		return recoveryWebhookError(ctx)
	}
	return nil
}

// No application SQL functions are called: all names below are fixed, qualified
// relations and pg_catalog operators/functions after the caller's catalogue pin.
func recoveryWebhookProfileQueries(schema string) []recoveryInvariantQuery {
	queries := []recoveryInvariantQuery{
		{"task_model_pin", `SELECT NOT EXISTS(SELECT 1 FROM {tasks} t JOIN {agents} a ON a.organization_id=t.organization_id AND a.id=t.agent_id AND a.version=t.agent_version LEFT JOIN {model_connections} m ON m.organization_id=t.organization_id AND m.id=t.model_connection_id AND m.version=t.model_connection_version WHERE (t.model_connection_id IS NOT NULL AND (m.id IS NULL OR m.model<>a.model)) OR (t.model_connection_id IS NULL AND a.model NOT IN('fixture:echo','team:coordinator')))`},
		{"destination_purpose", `SELECT NOT EXISTS(SELECT 1 FROM {configuration_destinations} d JOIN {credential_purpose_profiles} p ON p.organization_id=d.organization_id AND p.id=d.credential_purpose_id AND p.version=d.credential_purpose_version WHERE d.kind<>p.kind OR p.endpoint_digest<>pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(CASE d.kind WHEN 'model' THEN d.profile#>>'{model,endpoint}' ELSE d.profile#>>'{mcp,endpoint}' END,'UTF8')),'hex'))`},
		{"mcp_binding", `SELECT NOT EXISTS(SELECT 1 FROM {mcp_connections} c WHERE NOT EXISTS(SELECT 1 FROM {configuration_resource_bindings} b WHERE b.organization_id=c.organization_id AND b.kind='mcp' AND b.target_id=c.id AND b.resource_version=c.version))`},
		{"webhook_subscription", `SELECT NOT EXISTS(SELECT 1 FROM {webhook_subscriptions} s JOIN {tasks} t ON t.organization_id=s.organization_id AND t.id=s.task_id JOIN {principals} p ON p.id=s.creator_id JOIN {principals} op ON op.id=s.owner_id JOIN {memberships} cm ON cm.organization_id=s.organization_id AND cm.principal_id=s.creator_id JOIN {memberships} om ON om.organization_id=s.organization_id AND om.principal_id=s.owner_id JOIN {organization_login_policies} pol ON pol.organization_id=s.organization_id JOIN {webhook_destinations} dest ON dest.organization_id=s.organization_id AND dest.id=s.destination_id AND dest.version=s.destination_version LEFT JOIN LATERAL(SELECT pg_catalog.count(*) n,COALESCE(pg_catalog.max(event_sequence),0) seq FROM {webhook_deliveries} d WHERE d.organization_id=s.organization_id AND d.subscription_id=s.id) x ON true WHERE s.owner_id<>t.owner_id OR p.kind<>'human' OR s.profile_digest<>dest.profile_digest OR s.creator_authority_epoch>cm.security_epoch OR s.owner_authority_epoch>om.security_epoch OR s.creator_policy_epoch>pol.human_authority_epoch OR s.owner_policy_epoch>CASE op.kind WHEN 'human' THEN pol.human_authority_epoch ELSE pol.service_authority_epoch END OR s.admitted_notifications<>x.n OR s.last_admitted_sequence<>x.seq OR s.version::bigint<>1+x.n+CASE s.status WHEN 'revoked' THEN 1 ELSE 0 END OR (s.status='exhausted') IS DISTINCT FROM (s.admitted_notifications=s.max_notifications AND s.status<>'revoked') OR pg_catalog.cardinality(s.event_types)<>(SELECT pg_catalog.count(DISTINCT v) FROM pg_catalog.unnest(s.event_types) v))`},
		{"webhook_delivery", `SELECT NOT EXISTS(SELECT 1 FROM {webhook_deliveries} d JOIN {webhook_subscriptions} s ON s.organization_id=d.organization_id AND s.id=d.subscription_id JOIN {events} e ON e.organization_id=d.organization_id AND e.id=d.event_id WHERE d.destination_id<>s.destination_id OR d.destination_version<>s.destination_version OR d.profile_digest<>s.profile_digest OR d.expires_at<>s.expires_at OR d.created_at<s.created_at OR d.created_at>=d.expires_at OR d.event_sequence<=s.start_after_sequence OR NOT(e.type=ANY(s.event_types)) OR d.id<>'whd_'||pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(d.organization_id||pg_catalog.chr(31)||d.subscription_id||pg_catalog.chr(31)||d.event_id,'UTF8')),'hex') OR d.envelope<>pg_catalog.jsonb_build_object('envelope_version',1,'delivery_id',d.id,'organization_id',d.organization_id,'subscription_id',s.id,'event',pg_catalog.jsonb_build_object('id',e.id,'task_id',e.task_id,'sequence',e.sequence::text,'type',e.type,'payload_version',e.payload_version,'created_at',d.envelope::jsonb#>'{event,created_at}'))::text OR d.current_attempt_id<>'wha_'||pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(d.id||pg_catalog.chr(31)||d.attempt_count::text,'UTF8')),'hex'))`},
		{"webhook_attempts", `SELECT NOT EXISTS(SELECT 1 FROM {webhook_deliveries} d JOIN {webhook_attempts} current ON current.organization_id=d.organization_id AND current.id=d.current_attempt_id JOIN LATERAL(SELECT pg_catalog.count(*) n,pg_catalog.min(number) first,pg_catalog.max(number) last,pg_catalog.bool_or(status='acknowledged') ack FROM {webhook_attempts} a WHERE a.organization_id=d.organization_id AND a.delivery_id=d.id) x ON true WHERE x.n<>d.attempt_count OR x.first<>1 OR x.last<>d.attempt_count OR current.number<>d.attempt_count OR d.status<>CASE WHEN x.ack THEN 'acknowledged' ELSE current.status END OR (x.ack AND current.status IN('queued','leased') AND current.sent_at IS NULL)) AND NOT EXISTS(SELECT 1 FROM {webhook_attempts} a JOIN {webhook_deliveries} d ON d.organization_id=a.organization_id AND d.id=a.delivery_id WHERE a.id<>'wha_'||pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(d.id||pg_catalog.chr(31)||a.number::text,'UTF8')),'hex') OR a.created_at<d.created_at OR a.created_at>=d.expires_at OR (a.number=1 AND a.created_at<>d.created_at) OR (a.number<d.attempt_count AND a.status NOT IN('unknown','not_acknowledged','acknowledged','stopped')) OR (a.sent_at IS NOT NULL AND (a.worker_id IS NULL OR a.fencing_token<>a.recoveries+1 OR a.sent_at>=a.lease_until OR a.lease_until>d.expires_at OR a.signature_timestamp<>pg_catalog.floor(EXTRACT(epoch FROM a.sent_at)))) OR (a.status='leased' AND (a.fencing_token<>a.recoveries+1 OR a.lease_until>d.expires_at)) OR (a.status='queued' AND (a.worker_id IS NOT NULL OR a.fencing_token<>0 OR a.recoveries<>0)) OR (a.status IN('stopped','expired') AND (a.worker_id IS NOT NULL OR NOT((a.fencing_token=0 AND a.recoveries=0) OR a.fencing_token=a.recoveries+1))))`},
	}
	queries = append(queries, recoveryInvariantQuery{"webhook_attempt_reason", `SELECT NOT EXISTS(SELECT 1 FROM {webhook_attempts} x JOIN {webhook_deliveries} d ON d.organization_id=x.organization_id AND d.id=x.delivery_id JOIN {webhook_subscriptions} s ON s.organization_id=d.organization_id AND s.id=d.subscription_id WHERE CASE x.status
 WHEN 'queued' THEN x.reason_code<>'' WHEN 'leased' THEN x.reason_code<>'' WHEN 'sent' THEN x.reason_code<>''
 WHEN 'unknown' THEN x.reason_code<>'transport_unknown' WHEN 'acknowledged' THEN x.reason_code<>'http_acknowledged' WHEN 'not_acknowledged' THEN x.reason_code<>'http_not_acknowledged'
 WHEN 'expired' THEN x.reason_code<>'delivery_expired'
 WHEN 'stopped' THEN x.reason_code NOT IN('authorization_revoked','lease_recovery_exhausted','credentials_unavailable','invalid_profile','execution_cancelled','execution_not_admitted','subscription_revoked','already_acknowledged')
 OR (x.reason_code='subscription_revoked' AND s.status<>'revoked') OR (x.reason_code='already_acknowledged' AND NOT EXISTS(SELECT 1 FROM {webhook_attempts} ack WHERE ack.organization_id=x.organization_id AND ack.delivery_id=x.delivery_id AND ack.status='acknowledged'))
 ELSE true END)
 AND NOT EXISTS(SELECT 1 FROM {webhook_attempts} x WHERE x.status IN('stopped','expired') AND (SELECT pg_catalog.count(*) FROM {webhook_audit} a WHERE a.organization_id=x.organization_id AND a.object_id=x.id AND a.action='attempt.stopped')<>CASE WHEN x.reason_code IN('subscription_revoked','already_acknowledged') THEN 0 ELSE 1 END)`})
	queries = append(queries,
		recoveryInvariantQuery{"webhook_audit_objects", `SELECT NOT EXISTS(SELECT 1 FROM {webhook_audit} a LEFT JOIN {principals} p ON p.id=a.actor_id WHERE CASE a.action
 WHEN 'subscription.created' THEN NOT(a.actor_id IS NOT NULL AND p.kind='human' AND a.reason_code='metadata_export_acknowledged' AND a.reason_digest<>'' AND EXISTS(SELECT 1 FROM {webhook_subscriptions} s WHERE s.organization_id=a.organization_id AND s.task_id=a.task_id AND s.id=a.object_id AND s.creator_id=a.actor_id))
 WHEN 'subscription.revoked' THEN NOT(a.actor_id IS NOT NULL AND p.kind='human' AND a.reason_code='explicit_revocation' AND a.reason_digest<>'' AND EXISTS(SELECT 1 FROM {webhook_subscriptions} s WHERE s.organization_id=a.organization_id AND s.task_id=a.task_id AND s.id=a.object_id AND s.status='revoked'))
 WHEN 'delivery.retry_requested' THEN NOT(a.actor_id IS NOT NULL AND p.kind='human' AND a.reason_code='possible_duplicate_acknowledged' AND a.reason_digest<>'' AND EXISTS(SELECT 1 FROM {webhook_deliveries} d WHERE d.organization_id=a.organization_id AND d.task_id=a.task_id AND d.id=a.object_id))
 WHEN 'attempt.claimed' THEN NOT(a.actor_id IS NULL AND a.reason_digest='' AND a.reason_code='proven_unsent' AND EXISTS(SELECT 1 FROM {webhook_attempts} x WHERE x.organization_id=a.organization_id AND x.task_id=a.task_id AND x.id=a.object_id))
 WHEN 'attempt.sent' THEN NOT(a.actor_id IS NULL AND a.reason_digest='' AND a.reason_code='short_dispatch_permit' AND EXISTS(SELECT 1 FROM {webhook_attempts} x WHERE x.organization_id=a.organization_id AND x.task_id=a.task_id AND x.id=a.object_id AND x.sent_at IS NOT NULL))
 WHEN 'attempt.observed' THEN NOT(a.actor_id IS NULL AND a.reason_digest='' AND EXISTS(SELECT 1 FROM {webhook_attempts} x WHERE x.organization_id=a.organization_id AND x.task_id=a.task_id AND x.id=a.object_id AND ((a.reason_code='transport_unknown' AND x.unknown_at IS NOT NULL) OR (a.reason_code='http_acknowledged' AND x.status='acknowledged') OR (a.reason_code='http_not_acknowledged' AND x.status='not_acknowledged'))))
 WHEN 'attempt.stopped' THEN NOT(a.actor_id IS NULL AND a.reason_digest='' AND EXISTS(SELECT 1 FROM {webhook_attempts} x WHERE x.organization_id=a.organization_id AND x.task_id=a.task_id AND x.id=a.object_id AND x.status IN('stopped','expired') AND x.reason_code=a.reason_code))
 ELSE true END)`},
		recoveryInvariantQuery{"webhook_audit_projection", `SELECT NOT EXISTS(SELECT 1 FROM {webhook_subscriptions} s WHERE (SELECT pg_catalog.count(*) FROM {webhook_audit} a WHERE a.organization_id=s.organization_id AND a.object_id=s.id AND a.action='subscription.created')<>1 OR (SELECT pg_catalog.count(*) FROM {webhook_audit} a WHERE a.organization_id=s.organization_id AND a.object_id=s.id AND a.action='subscription.revoked')<>CASE s.status WHEN 'revoked' THEN 1 ELSE 0 END)
 AND NOT EXISTS(SELECT 1 FROM {webhook_deliveries} d WHERE (SELECT pg_catalog.count(*) FROM {webhook_audit} a WHERE a.organization_id=d.organization_id AND a.object_id=d.id AND a.action='delivery.retry_requested')<>d.attempt_count-1)
 AND NOT EXISTS(SELECT 1 FROM {webhook_attempts} x WHERE (SELECT pg_catalog.count(*) FROM {webhook_audit} a WHERE a.organization_id=x.organization_id AND a.object_id=x.id AND a.action='attempt.claimed')<>x.fencing_token OR (SELECT pg_catalog.count(*) FROM {webhook_audit} a WHERE a.organization_id=x.organization_id AND a.object_id=x.id AND a.action='attempt.sent')<>CASE WHEN x.sent_at IS NULL THEN 0 ELSE 1 END OR (SELECT pg_catalog.count(*) FROM {webhook_audit} a WHERE a.organization_id=x.organization_id AND a.object_id=x.id AND a.action='attempt.observed' AND a.reason_code='transport_unknown')<>CASE WHEN x.unknown_at IS NULL THEN 0 ELSE 1 END OR (SELECT pg_catalog.count(*) FROM {webhook_audit} a WHERE a.organization_id=x.organization_id AND a.object_id=x.id AND a.action='attempt.observed' AND a.reason_code='http_acknowledged')<>CASE WHEN x.status='acknowledged' THEN 1 ELSE 0 END OR (SELECT pg_catalog.count(*) FROM {webhook_audit} a WHERE a.organization_id=x.organization_id AND a.object_id=x.id AND a.action='attempt.observed' AND a.reason_code='http_not_acknowledged')<>CASE WHEN x.status='not_acknowledged' THEN 1 ELSE 0 END OR (SELECT pg_catalog.count(*) FROM {webhook_audit} a WHERE a.organization_id=x.organization_id AND a.object_id=x.id AND a.action='attempt.stopped')>1)`},
	)
	for i := range queries {
		for _, name := range []string{"tasks", "agents", "model_connections", "configuration_destinations", "credential_purpose_profiles", "configuration_resource_bindings", "mcp_connections", "principals", "memberships", "organization_login_policies", "webhook_destinations", "webhook_subscriptions", "webhook_deliveries", "webhook_attempts", "webhook_audit", "events"} {
			queries[i].sql = strings.ReplaceAll(queries[i].sql, "{"+name+"}", pgx.Identifier{schema, name}.Sanitize())
		}
	}
	return queries
}
