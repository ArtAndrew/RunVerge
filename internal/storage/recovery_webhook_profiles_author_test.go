// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

type recoveryProfilesFixture struct {
	s       *Store
	p       domain.Principal
	model   domain.ModelConnection
	tool    domain.ToolConnection
	webhook webhookAuthorFixture
	sub     domain.WebhookSubscription
	agent   domain.Agent
}

// All endpoints are inert .invalid names. These tests do not resolve secrets or
// perform HTTP/model calls. Trusted host methods create real normal history;
// corruptions are applied only to rollback-only typed TEMP copies.
func recoveryProfilesSetup(t *testing.T) recoveryProfilesFixture {
	t.Helper()
	s, _ := recoveryAuthorStore(t)
	p, _ := identity(t, s, 1_000_000)
	ctx := context.Background()
	f := recoveryProfilesFixture{s: s, p: p}
	f.model = domain.ModelConnection{ID: "historical_model", OrganizationID: p.OrganizationID, Version: 1, Model: "historical-model", ProviderAccount: "historical-account", PriceVersion: "historical-price", Endpoint: "https://historical-model.invalid/chat", CredentialEnvironment: "PLATFORM_MODEL_KEY_HISTORICAL", MaxChargeMicroUSD: 150, MaxInputTokens: 100, MaxOutputTokens: 16, InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 2_000_000, FixedMicroUSD: 7, MaxInputBytes: 4096, MaxOutputBytes: 1024, TimeoutMilliseconds: 2000, BoundAssumptions: "Historical finite test price bound", TokenLimitField: "max_tokens", Active: true}
	if _, err := s.InstallModelConnection(ctx, f.model); err != nil {
		t.Fatal("model fixture install", err)
	}
	schema := `{"type":"object","properties":{},"additionalProperties":false}`
	f.tool = domain.ToolConnection{Tool: domain.ToolInfo{ID: "historical_tool", OrganizationID: p.OrganizationID, Version: 1, Name: "Historical reader", Description: "Immutable public description", Effect: "read", Destination: "Historical destination", Provider: "historical-tool-account", PriceVersion: "historical-tool-price", MaxChargeMicroUSD: 10, InputSchemaJSON: schema, InputSchemaDigest: Hash([]byte(schema)), TimeoutSeconds: 2, MaxInputBytes: 1024, MaxOutputBytes: 1024, Active: true}, Transport: "mcp_https", ResultContract: "mcp_read_text_v1", RemoteTool: "read_text", Endpoint: "https://historical-tool.invalid/mcp"}
	if _, err := s.InstallToolConnection(ctx, f.tool); err != nil {
		t.Fatal("tool fixture install", err)
	}
	raw, err := s.CreateAgent(ctx, p, domain.NewID("key"), domain.CreateAgent{Name: "Historical model agent", Instructions: "Do not run in this test", Model: f.model.Model, Tools: []string{f.tool.Tool.ID}})
	f.agent = configurationAuthorDecode[domain.Agent](t, raw, err)
	raw, err = s.PublishAgent(ctx, p, f.agent.ID, domain.NewID("key"), f.agent.Version)
	f.agent = configurationAuthorDecode[domain.Agent](t, raw, err)
	_ = task(t, s, p, f.agent)
	// Historical configured model and MCP profiles, with exact destination/purpose
	// binding. Direct trusted installation avoids running configuration probes.
	for _, kind := range []string{"model", "mcp"} {
		endpoint := "https://configured-" + kind + ".invalid/endpoint"
		prefix := "PLATFORM_MODEL_KEY_"
		if kind == "mcp" {
			prefix = "PLATFORM_MCP_KEY_"
		}
		purpose, err := s.InstallCredentialPurposeProfile(ctx, domain.CredentialPurposeProfile{ID: "purpose_" + kind, OrganizationID: p.OrganizationID, Version: 1, Kind: kind, EndpointDigest: Hash([]byte(endpoint)), CredentialEnvironment: prefix + "HISTORICAL", Active: true})
		if err != nil {
			t.Fatal("purpose fixture install", err)
		}
		dest := domain.ConfigurationDestinationProfile{Destination: domain.ConfigurationDestination{ID: "destination_" + kind, OrganizationID: p.OrganizationID, Version: 1, Kind: kind, Name: "Historical configuration", TargetID: "configured_" + kind, CredentialPurposeID: purpose.ID, CredentialPurposeVersion: 1, Active: true}}
		var configured domain.ModelConnection
		if kind == "model" {
			configured = f.model
			configured.ID = dest.Destination.TargetID
			configured.Model = "configured-model"
			configured.Endpoint = endpoint
			configured.CredentialEnvironment = ""
			dest.Model = &configured
			dest.Destination.Model = configured.Model
			dest.Destination.MaxVerifyChargeMicroUSD = 150
		} else {
			dest.MCP = &domain.MCPConnectionProfile{Endpoint: endpoint, TimeoutSeconds: 2}
			dest.Destination.DiscoveryUnmeteredApproved = true
		}
		if _, err = s.InstallConfigurationDestination(ctx, dest); err != nil {
			t.Fatal("destination fixture install", err)
		}
		tx, err := s.beginRuntime(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "model" {
			configured.CredentialEnvironment = purpose.CredentialEnvironment
			_, err = installModelConnectionTx(ctx, tx, configured)
		} else {
			err = installMCPConnectionTx(ctx, tx, p.OrganizationID, dest.Destination.TargetID, 1, *dest.MCP, true)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO configuration_resource_bindings(organization_id,kind,target_id,resource_version,destination_id,destination_version,credential_purpose_id,credential_purpose_version) VALUES($1,$2,$3,1,$4,1,$5,1)`, p.OrganizationID, kind, dest.Destination.TargetID, dest.Destination.ID, purpose.ID)
		}
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(ctx)
		}
		if err != nil {
			t.Fatal("configured fixture binding", err)
		}
	}
	a := agent(t, s, p)
	f.webhook = webhookAuthorFixture{s: s, p: p, task: task(t, s, p, a), profile: domain.WebhookDestinationProfile{Destination: domain.WebhookDestination{ID: "webhook_author", OrganizationID: p.OrganizationID, Version: 1, Name: "Historical metadata", SigningKeyID: "historical_key", UnmeteredApproved: true, Active: true}, Endpoint: "https://historical-hook.invalid/events", SigningKeyEnvironment: "PLATFORM_WEBHOOK_KEY_HISTORICAL", TimeoutSeconds: 2}}
	if _, err = s.InstallWebhookDestination(ctx, f.webhook.profile); err != nil {
		t.Fatal(err)
	}
	f.sub = f.webhook.subscribe(t, 2)
	f.webhook.material(t)
	return f
}

func recoveryProfilesWantValid(t *testing.T, tx pgx.Tx) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := validateRecoveryWebhookProfiles(ctx, tx, "pg_temp"); err != nil {
		for _, table := range []string{"model_connections", "tool_connections", "credential_purpose_profiles", "configuration_destinations", "mcp_connections", "webhook_destinations"} {
			if recoveryValidateProfiles(ctx, tx, "pg_temp", table) != nil {
				t.Fatalf("valid profile rejected: %s", table)
			}
		}
		if recoveryValidateModelTools(ctx, tx, "pg_temp") != nil {
			t.Fatal("valid static tools rejected")
		}
		if recoveryValidateResourceBindings(ctx, tx, "pg_temp") != nil {
			t.Fatal("valid resource bindings rejected")
		}
		if recoveryValidateWebhookSubscriptions(ctx, tx, "pg_temp") != nil {
			t.Fatal("valid subscriptions rejected")
		}
		if recoveryValidateWebhookEnvelopes(ctx, tx, "pg_temp") != nil {
			t.Fatal("valid envelope rejected")
		}
		for _, q := range recoveryWebhookProfileQueries("pg_temp") {
			var valid bool
			if tx.QueryRow(ctx, q.sql).Scan(&valid) != nil {
				t.Fatalf("query invalid: %s", q.name)
			}
			if !valid {
				t.Fatalf("projection rejected: %s", q.name)
			}
		}
		t.Fatal("valid aggregate rejected")
	}
}

func TestRecoveryWebhookProfilesAuthorCanonicalLegacyAndRetirement(t *testing.T) {
	f := recoveryProfilesSetup(t)
	tx := recoveryInvariantStage(t, f.s)
	recoveryProfilesWantValid(t, tx)
	var raw []byte
	if tx.QueryRow(context.Background(), `SELECT profile FROM pg_temp.model_connections WHERE id='historical_model'`).Scan(&raw) != nil {
		t.Fatal("legacy read")
	}
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		t.Fatal("legacy JSON")
	}
	if _, ok := v["accounting_mode"]; ok {
		t.Fatal("legacy mode unexpectedly materialized")
	}
	for _, sql := range []string{`UPDATE pg_temp.model_connections SET active=false`, `UPDATE pg_temp.tool_connections SET active=false`, `UPDATE pg_temp.mcp_connections SET active=false`, `UPDATE pg_temp.webhook_destinations SET active=false`, `UPDATE pg_temp.configuration_destinations SET active=false`, `UPDATE pg_temp.credential_purpose_profiles SET active=false`, `UPDATE pg_temp.memberships SET active=false,security_epoch=security_epoch+1`, `UPDATE pg_temp.organization_login_policies SET human_authority_epoch=human_authority_epoch+1,service_authority_epoch=service_authority_epoch+1`, `UPDATE pg_temp.organizations SET active=false`} {
		if _, err := tx.Exec(context.Background(), sql); err != nil {
			t.Fatal("historical retirement fixture")
		}
	}
	recoveryProfilesWantValid(t, tx)
}

func TestRecoveryWebhookProfilesAuthorForgedProfiles(t *testing.T) {
	f := recoveryProfilesSetup(t)
	cases := map[string]string{
		"model_denorm":                 `UPDATE pg_temp.model_connections SET provider_account='different' WHERE id='historical_model'`,
		"model_mode_zero":              `UPDATE pg_temp.model_connections SET profile=profile||'{"accounting_mode":"operator_unmetered_local"}' WHERE id='historical_model'`,
		"model_unknown":                `UPDATE pg_temp.model_connections SET profile=profile||'{"Unknown":true}' WHERE id='historical_model'`,
		"model_missing_required_false": `UPDATE pg_temp.model_connections SET profile=profile-'allow_insecure_loopback' WHERE id='historical_model'`,
		"model_forged_liveness":        `UPDATE pg_temp.model_connections SET profile=profile||'{"active":true}' WHERE id='historical_model'`,
		"model_alias":                  `UPDATE pg_temp.model_connections SET profile=profile||jsonb_build_object('Model',model) WHERE id='historical_model'`,
		"tool_digest":                  `UPDATE pg_temp.tool_connections SET profile_digest=repeat('a',64)`,
		"tool_schema_forged_digest":    `UPDATE pg_temp.tool_connections SET profile=jsonb_set(profile,'{tool,input_schema_digest}',to_jsonb(repeat('b',64)))`,
		"tool_automation_write":        `UPDATE pg_temp.tool_connections SET profile=jsonb_set(jsonb_set(profile,'{tool,effect}','"write"'),'{tool,automation_allowed}','true')`,
		"purpose_endpoint":             `UPDATE pg_temp.credential_purpose_profiles SET endpoint_digest=repeat('a',64),profile=jsonb_set(profile,'{endpoint_digest}',to_jsonb(repeat('a',64)))`,
		"destination_cap":              `UPDATE pg_temp.configuration_destinations SET profile=jsonb_set(profile,'{destination,max_verify_charge_microusd}','"1"') WHERE kind='model'`,
		"mcp_timeout":                  `UPDATE pg_temp.mcp_connections SET profile=jsonb_set(profile,'{timeout_seconds}','31')`,
		"mcp_changed_endpoint":         `UPDATE pg_temp.mcp_connections SET profile=jsonb_set(profile,'{endpoint}','"https://changed.invalid/mcp"')`,
		"mcp_unbound":                  `DELETE FROM pg_temp.configuration_resource_bindings WHERE kind='mcp'`,
		"model_binding_changed":        `UPDATE pg_temp.model_connections SET profile=jsonb_set(profile,'{max_charge_microusd}','149') WHERE id='configured_model'`,
		"webhook_digest":               `UPDATE pg_temp.webhook_destinations SET profile_digest=repeat('a',64)`,
		"webhook_forged_signer":        `UPDATE pg_temp.webhook_destinations SET profile=jsonb_set(profile,'{destination,signing_key_id}','"changed_key"')`,
		"pin_ordered_id":               `UPDATE pg_temp.agents SET tools='["different_tool"]' WHERE jsonb_array_length(model_tools)>0`,
		"pin_description":              `UPDATE pg_temp.agents SET model_tools=jsonb_set(model_tools,'{0,description}','"changed"') WHERE jsonb_array_length(model_tools)>0`,
		"pin_digest":                   `UPDATE pg_temp.agents SET model_tools=jsonb_set(model_tools,'{0,tool_profile_digest}',to_jsonb(repeat('a',64))) WHERE jsonb_array_length(model_tools)>0`,
		"pin_name":                     `UPDATE pg_temp.agents SET model_tools=jsonb_set(model_tools,'{0,name}','"not_deterministic"') WHERE jsonb_array_length(model_tools)>0`,
		"task_model_association":       `UPDATE pg_temp.agents SET model='different-model' WHERE model='historical-model'`,
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			tx := recoveryInvariantStage(t, f.s)
			recoveryProfilesWantValid(t, tx)
			if _, err := tx.Exec(context.Background(), sql); err != nil {
				t.Fatal("fault must satisfy copied row constraints")
			}
			if err := validateRecoveryWebhookProfiles(context.Background(), tx, "pg_temp"); !errors.Is(err, ErrRecoveryWebhookProfiles) {
				t.Fatal("forged history accepted or wrong error", err)
			}
		})
	}
}

func TestRecoveryWebhookProfilesAuthorWebhookProjections(t *testing.T) {
	f := recoveryProfilesSetup(t)
	cases := map[string]string{
		"subscription_counter":         `UPDATE pg_temp.webhook_subscriptions SET admitted_notifications=2,status='exhausted',version=version+1`,
		"subscription_sequence":        `UPDATE pg_temp.webhook_subscriptions SET last_admitted_sequence=last_admitted_sequence+1`,
		"subscription_unknown_event":   `UPDATE pg_temp.webhook_subscriptions SET event_types=ARRAY['material.created','unknown.event']`,
		"subscription_duplicate_event": `UPDATE pg_temp.webhook_subscriptions SET event_types=ARRAY['material.created','material.created']`,
		"subscription_consent_future":  `UPDATE pg_temp.webhook_subscriptions SET creator_authority_epoch=creator_authority_epoch+1`,
		"subscription_policy_future":   `UPDATE pg_temp.webhook_subscriptions SET owner_policy_epoch=owner_policy_epoch+1`,
		"subscription_digest":          `UPDATE pg_temp.webhook_subscriptions SET profile_digest=repeat('a',64)`,
		"delivery_profile":             `UPDATE pg_temp.webhook_deliveries SET profile_digest=repeat('a',64)`,
		"delivery_expiry":              `UPDATE pg_temp.webhook_deliveries SET expires_at=expires_at-interval '1 second'`,
		"delivery_current_count":       `UPDATE pg_temp.webhook_deliveries SET attempt_count=2`,
		"delivery_aggregate_ack":       `UPDATE pg_temp.webhook_deliveries SET status='acknowledged'`,
		"envelope_extra_rehash":        `UPDATE pg_temp.webhook_deliveries SET envelope=(envelope::jsonb||'{"payload":"private"}')::text,envelope_digest=encode(sha256(convert_to((envelope::jsonb||'{"payload":"private"}')::text,'UTF8')),'hex')`,
		"envelope_event_rehash":        `UPDATE pg_temp.webhook_deliveries SET envelope=jsonb_set(envelope::jsonb,'{event,type}','"task.created"')::text,envelope_digest=encode(sha256(convert_to(jsonb_set(envelope::jsonb,'{event,type}','"task.created"')::text,'UTF8')),'hex')`,
		"envelope_time_rehash":         `UPDATE pg_temp.webhook_deliveries SET envelope=jsonb_set(envelope::jsonb,'{event,created_at}','"2000-01-01T00:00:00Z"')::text,envelope_digest=encode(sha256(convert_to(jsonb_set(envelope::jsonb,'{event,created_at}','"2000-01-01T00:00:00Z"')::text,'UTF8')),'hex')`,
		"envelope_format_rehash":       `UPDATE pg_temp.webhook_deliveries SET envelope=envelope||' ',envelope_digest=encode(sha256(convert_to(envelope||' ','UTF8')),'hex')`,
		"attempt_number":               `UPDATE pg_temp.webhook_attempts SET number=2`,
		"attempt_creation":             `UPDATE pg_temp.webhook_attempts SET created_at=created_at+interval '1 microsecond'`,
		"queued_reason":                `UPDATE pg_temp.webhook_attempts SET reason_code='http_acknowledged'`,
		"queued_fence":                 `UPDATE pg_temp.webhook_attempts SET fencing_token=1`,
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			tx := recoveryInvariantStage(t, f.s)
			recoveryProfilesWantValid(t, tx)
			if _, err := tx.Exec(context.Background(), sql); err != nil {
				t.Fatal("fault must satisfy copied row constraints")
			}
			if err := validateRecoveryWebhookProfiles(context.Background(), tx, "pg_temp"); !errors.Is(err, ErrRecoveryWebhookProfiles) {
				t.Fatal("forged projection accepted", err)
			}
		})
	}
}

func TestRecoveryWebhookProfilesAuthorLateAckUnknownAndRevocation(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmarked_replacement", true: "marked_replacement"}[marked], func(t *testing.T) {
			f := recoveryProfilesSetup(t)
			ctx := context.Background()
			first, _, err := f.s.ClaimWebhook(ctx, "history_worker", time.Minute)
			if err != nil || first == nil {
				t.Fatal("claim fixture")
			}
			if _, err = f.s.PrepareWebhookDispatch(ctx, first); err != nil {
				t.Fatal("mark fixture", err)
			}
			d, err := f.s.RecordWebhookObservation(ctx, independentWebhookObservation(first, 0))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := f.s.RetryWebhookDelivery(ctx, f.p, first.TaskID, first.DeliveryID, domain.NewID("key"), domain.RetryWebhookDelivery{ExpectedVersion: d.Version, ExpectedAttemptID: d.CurrentAttemptID, EnvelopeDigest: d.EnvelopeDigest, AcknowledgePossibleDuplicate: true, Reason: "Historical operator duplicate-risk acknowledgment"})
			_ = configurationAuthorDecode[domain.WebhookDelivery](t, raw, err)
			var second *domain.WebhookLease
			if marked {
				second, _, err = f.s.ClaimWebhook(ctx, "replacement_worker", time.Minute)
				if err != nil || second == nil {
					t.Fatal("replacement claim")
				}
				if _, err = f.s.PrepareWebhookDispatch(ctx, second); err != nil {
					t.Fatal("replacement mark")
				}
			}
			if _, err = f.s.RecordWebhookObservation(ctx, independentWebhookObservation(first, 204)); err != nil {
				t.Fatal("late ack")
			}
			if marked {
				if _, err = f.s.RecordWebhookObservation(ctx, independentWebhookObservation(second, 0)); err != nil {
					t.Fatal("replacement unknown")
				}
			}
			tx := recoveryInvariantStage(t, f.s)
			recoveryProfilesWantValid(t, tx)
			// Historical ACK must remain sticky with a current unknown; no request runs.
			if marked {
				if _, err = tx.Exec(ctx, `UPDATE pg_temp.webhook_deliveries SET status='unknown'`); err != nil {
					t.Fatal(err)
				}
				if !errors.Is(validateRecoveryWebhookProfiles(ctx, tx, "pg_temp"), ErrRecoveryWebhookProfiles) {
					t.Fatal("ACK aggregate downgraded")
				}
			}
		})
	}
}

func TestRecoveryWebhookProfilesAuthorInputBoundary(t *testing.T) {
	if !errors.Is(validateRecoveryWebhookProfiles(context.Background(), nil, "public"), ErrRecoveryWebhookProfiles) {
		t.Fatal("nil transaction accepted")
	}
	f := recoveryProfilesSetup(t)
	tx := recoveryInvariantStage(t, f.s)
	if !errors.Is(validateRecoveryWebhookProfiles(context.Background(), tx, "unsafe;schema"), ErrRecoveryWebhookProfiles) {
		t.Fatal("unsafe identifier accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(validateRecoveryWebhookProfiles(ctx, tx, "pg_temp"), context.Canceled) {
		t.Fatal("cancellation lost")
	}
}

func TestRecoveryWebhookProfilesAuthorMatchingDigestAndZeroLocal(t *testing.T) {
	f := recoveryProfilesSetup(t)
	ctx := context.Background()
	zero := f.model
	zero.ID = "historical_local_zero"
	zero.Model = "historical-local-zero"
	zero.AccountingMode = "operator_unmetered_local"
	zero.Endpoint = "http://127.0.0.1:11434/v1/chat/completions"
	zero.AllowInsecureLoopback = true
	zero.MaxChargeMicroUSD = 0
	zero.InputMicroUSDPerMillion = 0
	zero.OutputMicroUSDPerMillion = 0
	zero.FixedMicroUSD = 0
	if _, err := f.s.InstallModelConnection(ctx, zero); err != nil {
		t.Fatal("zero local profile install", err)
	}
	for _, name := range []string{"valid_zero_and_legacy", "tool_invalid_with_new_digest", "webhook_invalid_with_all_new_pins", "audit_missing_created", "audit_unknown_action", "audit_wrong_task", "envelope_duplicate_rehashed"} {
		t.Run(name, func(t *testing.T) {
			tx := recoveryInvariantStage(t, f.s)
			recoveryProfilesWantValid(t, tx)
			switch name {
			case "valid_zero_and_legacy":
				return
			case "tool_invalid_with_new_digest":
				var raw []byte
				var tool domain.ToolConnection
				if tx.QueryRow(ctx, `SELECT profile FROM pg_temp.tool_connections`).Scan(&raw) != nil || json.Unmarshal(raw, &tool) != nil {
					t.Fatal("tool fixture read")
				}
				tool.Tool.AutomationAllowed = true
				tool.Tool.Effect = "write"
				raw, _ = json.Marshal(tool)
				if _, err := tx.Exec(ctx, `UPDATE pg_temp.tool_connections SET profile=$1,profile_digest=$2`, raw, domain.ToolConnectionDigest(tool)); err != nil {
					t.Fatal("coherent invalid tool hash fixture")
				}
			case "webhook_invalid_with_all_new_pins":
				var raw []byte
				var p domain.WebhookDestinationProfile
				if tx.QueryRow(ctx, `SELECT profile FROM pg_temp.webhook_destinations`).Scan(&raw) != nil || json.Unmarshal(raw, &p) != nil {
					t.Fatal("webhook fixture read")
				}
				p.TimeoutSeconds = 31
				raw, _ = json.Marshal(p)
				digest := Hash(raw)
				if _, err := tx.Exec(ctx, `UPDATE pg_temp.webhook_destinations SET profile=$1,profile_digest=$2`, raw, digest); err != nil {
					t.Fatal("invalid webhook hash fixture")
				}
				for _, table := range []string{"webhook_subscriptions", "webhook_deliveries"} {
					if _, err := tx.Exec(ctx, "UPDATE pg_temp."+table+" SET profile_digest=$1", digest); err != nil {
						t.Fatal("matching webhook pins fixture")
					}
				}
			case "audit_missing_created":
				if _, err := tx.Exec(ctx, `DELETE FROM pg_temp.webhook_audit WHERE action='subscription.created'`); err != nil {
					t.Fatal("audit delete")
				}
			case "audit_unknown_action":
				if _, err := tx.Exec(ctx, `UPDATE pg_temp.webhook_audit SET action='invented.action'`); err != nil {
					t.Fatal("audit action")
				}
			case "audit_wrong_task":
				if _, err := tx.Exec(ctx, `UPDATE pg_temp.webhook_audit SET task_id=(SELECT id FROM pg_temp.tasks WHERE id<>webhook_audit.task_id LIMIT 1)`); err != nil {
					t.Fatal("audit cross-task fixture")
				}
			case "envelope_duplicate_rehashed":
				if _, err := tx.Exec(ctx, `UPDATE pg_temp.webhook_deliveries SET envelope=regexp_replace(envelope,'^\{','{"envelope_version":1,'),envelope_digest=encode(sha256(convert_to(regexp_replace(envelope,'^\{','{"envelope_version":1,'),'UTF8')),'hex')`); err != nil {
					t.Fatal("duplicate envelope fixture")
				}
			}
			if !errors.Is(validateRecoveryWebhookProfiles(ctx, tx, "pg_temp"), ErrRecoveryWebhookProfiles) {
				t.Fatal("forged history with coherent hashes accepted")
			}
		})
	}
}

func TestRecoveryWebhookProfilesAuthorDispatchHistoryFaults(t *testing.T) {
	f := recoveryProfilesSetup(t)
	ctx := context.Background()
	lease, _, err := f.s.ClaimWebhook(ctx, "historical_worker", time.Minute)
	if err != nil || lease == nil {
		t.Fatal("claim")
	}
	if _, err = f.s.PrepareWebhookDispatch(ctx, lease); err != nil {
		t.Fatal("mark")
	}
	if _, err = f.s.RecordWebhookObservation(ctx, independentWebhookObservation(lease, 0)); err != nil {
		t.Fatal("unknown")
	}
	if _, err = f.s.RecordWebhookObservation(ctx, independentWebhookObservation(lease, 204)); err != nil {
		t.Fatal("late ack")
	}
	cases := map[string]string{
		"ack_reason_unknown":           `UPDATE pg_temp.webhook_attempts SET reason_code='transport_unknown'`,
		"ack_reason_empty":             `UPDATE pg_temp.webhook_attempts SET reason_code=''`,
		"sent_without_worker":          `UPDATE pg_temp.webhook_attempts SET worker_id=NULL,lease_until=NULL`,
		"sent_fence":                   `UPDATE pg_temp.webhook_attempts SET fencing_token=fencing_token+1`,
		"sent_recovery":                `UPDATE pg_temp.webhook_attempts SET recoveries=recoveries+1`,
		"sent_expired_lease":           `UPDATE pg_temp.webhook_attempts SET lease_until=sent_at`,
		"signature_timestamp":          `UPDATE pg_temp.webhook_attempts SET signature_timestamp=signature_timestamp+1`,
		"unknown_history_removed":      `UPDATE pg_temp.webhook_attempts SET unknown_at=NULL`,
		"audit_unknown_relabelled_ack": `UPDATE pg_temp.webhook_audit SET reason_code='http_acknowledged' WHERE action='attempt.observed' AND reason_code='transport_unknown'`,
		"audit_ack_relabelled_unknown": `UPDATE pg_temp.webhook_audit SET reason_code='transport_unknown' WHERE action='attempt.observed' AND reason_code='http_acknowledged'`,
		"audit_missing_sent":           `DELETE FROM pg_temp.webhook_audit WHERE action='attempt.sent'`,
		"audit_missing_unknown":        `DELETE FROM pg_temp.webhook_audit WHERE action='attempt.observed' AND reason_code='transport_unknown'`,
		"audit_missing_ack":            `DELETE FROM pg_temp.webhook_audit WHERE action='attempt.observed' AND reason_code='http_acknowledged'`,
		"audit_claim_duplicate":        `INSERT INTO pg_temp.webhook_audit SELECT organization_id,id||'_duplicate',task_id,actor_id,object_id,action,reason_code,reason_digest,created_at FROM pg_temp.webhook_audit WHERE action='attempt.claimed'`,
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			tx := recoveryInvariantStage(t, f.s)
			recoveryProfilesWantValid(t, tx)
			if _, err := tx.Exec(ctx, sql); err != nil {
				t.Fatal("fault must satisfy copied row constraints")
			}
			if name == "audit_unknown_relabelled_ack" || name == "audit_ack_relabelled_unknown" {
				// Independent review identified that total=2 and per-row allowed reasons
				// alone accepted this coherent redistribution. Preserve that witness.
				var total int
				if tx.QueryRow(ctx, `SELECT count(*) FROM pg_temp.webhook_audit WHERE action='attempt.observed'`).Scan(&total) != nil || total != 2 {
					t.Fatal("distribution fault changed total")
				}
				for _, q := range recoveryWebhookProfileQueries("pg_temp") {
					if q.name == "webhook_audit_objects" {
						var valid bool
						if tx.QueryRow(ctx, q.sql).Scan(&valid) != nil || !valid {
							t.Fatal("distribution fault must preserve each allowed audit object")
						}
					}
				}
			}
			if !errors.Is(validateRecoveryWebhookProfiles(ctx, tx, "pg_temp"), ErrRecoveryWebhookProfiles) {
				t.Fatal("forged dispatch history accepted")
			}
		})
	}
}

func TestRecoveryWebhookProfilesAuthorRevokedQueuedAndTimezone(t *testing.T) {
	f := recoveryProfilesSetup(t)
	ctx := context.Background()
	// Source body's historical offset must not be reserialized in target timezone.
	tx := recoveryInvariantStage(t, f.s)
	if _, err := tx.Exec(ctx, `SET LOCAL TIME ZONE 'Asia/Kathmandu'`); err != nil {
		t.Fatal("timezone fixture")
	}
	recoveryProfilesWantValid(t, tx)
	_ = tx.Rollback(ctx)
	raw, err := f.s.RevokeWebhookSubscription(ctx, f.p, f.webhook.task.ID, f.sub.ID, domain.NewID("key"), domain.RevokeWebhookSubscription{ExpectedVersion: f.sub.Version + 1, Reason: "Historical explicit revocation"})
	_ = configurationAuthorDecode[domain.WebhookSubscription](t, raw, err)
	tx = recoveryInvariantStage(t, f.s)
	recoveryProfilesWantValid(t, tx)
}

func TestRecoveryWebhookProfilesAuthorUnsentStopReason(t *testing.T) {
	f := recoveryProfilesSetup(t)
	ctx := context.Background()
	l, _, err := f.s.ClaimWebhook(ctx, "historical_unsent", time.Minute)
	if err != nil || l == nil {
		t.Fatal("claim unsent")
	}
	if err = f.s.StopWebhookUnsent(ctx, l, "execution_not_admitted"); err != nil {
		t.Fatal("stop unsent")
	}
	tx := recoveryInvariantStage(t, f.s)
	recoveryProfilesWantValid(t, tx)
	if _, err = tx.Exec(ctx, `UPDATE pg_temp.webhook_attempts SET reason_code='subscription_revoked'`); err != nil {
		t.Fatal("direct stop forged reason")
	}
	if !errors.Is(validateRecoveryWebhookProfiles(ctx, tx, "pg_temp"), ErrRecoveryWebhookProfiles) {
		t.Fatal("nonrevoked direct reason accepted")
	}
}
