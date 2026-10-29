// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
	"github.com/jackc/pgx/v5"
)

var ErrRecoveryConfigurationHistory = errors.New("invalid recovery configuration history")

// validateRecoveryConfigurationHistory checks retained immutable configuration
// intent and observations in the exclusively owned, typed historical stage.
// Declarative/FK, historical authority, allocation and private-profile validators
// are separate prerequisites. No current liveness/admission or external probe is
// run. Transient ConfigurationProbeDigest permits were never persisted: this
// validates their stored profile/resource inputs, not a past permit's existence.
func validateRecoveryConfigurationHistory(ctx context.Context, tx pgx.Tx, schema string) error {
	if ctx == nil || tx == nil || !recoverySnapshotName.MatchString(schema) {
		return ErrRecoveryConfigurationHistory
	}
	for _, check := range []func(context.Context, pgx.Tx, string) error{
		recoveryConfigurationAutomationGrants, recoveryConfigurationGrants, recoveryConfigurationPlans, recoveryConfigurationValidations,
		recoveryConfigurationApprovals, recoveryConfigurationVerifications,
	} {
		if err := check(ctx, tx, schema); err != nil {
			return recoverySnapshotError(ctx, ErrRecoveryConfigurationHistory)
		}
	}
	for _, query := range recoveryConfigurationHistoryQueries(schema) {
		var valid bool
		if err := tx.QueryRow(ctx, query).Scan(&valid); err != nil || !valid {
			return recoverySnapshotError(ctx, ErrRecoveryConfigurationHistory)
		}
	}
	return nil
}

// These DTOs were introduced in SQL019 with all their non-omitempty fields.
// Require the same JSON value after the exact historical typed serializer;
// PostgreSQL's object ordering/whitespace does not matter. This also rejects
// null/missing scalar fields which ordinary json.Unmarshal silently defaults.
func recoveryConfigurationJSON(raw []byte, out any, limit int) bool {
	if decodeRecoveryHistoryJSON(raw, out, limit) != nil {
		return false
	}
	b, err := json.Marshal(out)
	if err != nil {
		return false
	}
	var before, after any
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	if d.Decode(&before) != nil {
		return false
	}
	d = json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	return d.Decode(&after) == nil && reflect.DeepEqual(before, after)
}

func recoveryConfigurationRows(ctx context.Context, tx pgx.Tx, query string, check func(pgx.Rows) bool) error {
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return ErrRecoveryConfigurationHistory
	}
	defer rows.Close()
	for rows.Next() {
		if !check(rows) {
			return ErrRecoveryConfigurationHistory
		}
	}
	if rows.Err() != nil {
		return ErrRecoveryConfigurationHistory
	}
	return nil
}

func recoveryConfigurationName(schema, table string) string {
	return pgx.Identifier{schema, table}.Sanitize()
}

// The lateral aggregate enforces exactly one original audit receipt and carries
// only its digest. No private payload or input name is put into returned errors.
func recoveryConfigurationAudit(schema, object, action, actor string) string {
	return " LEFT JOIN LATERAL (SELECT pg_catalog.count(*) AS n,pg_catalog.min(a.digest) AS digest FROM " + recoveryConfigurationName(schema, "configuration_audit") + " a WHERE a.organization_id=x.organization_id AND a.object_id=x.id AND a.object_type='" + object + "' AND a.action='" + action + "' AND a.actor_type='principal' AND a.actor_id=" + actor + ") audit ON true "
}

func recoveryConfigurationAutomationGrants(ctx context.Context, tx pgx.Tx, schema string) error {
	q := "SELECT grant_body,organization_id,id,task_id,principal_id,owner_id,created_at,expires_at FROM " + recoveryConfigurationName(schema, "task_automation_grants")
	return recoveryConfigurationRows(ctx, tx, q, func(rows pgx.Rows) bool {
		var raw []byte
		var org, id, task, principal, owner string
		var created, expires time.Time
		if rows.Scan(&raw, &org, &id, &task, &principal, &owner, &created, &expires) != nil {
			return false
		}
		var v domain.TaskAutomationGrant
		if !recoveryConfigurationJSON(raw, &v, 32768) || v.OrganizationID != org || v.ID != id || v.TaskID != task || v.PrincipalID != principal || v.OwnerID != owner || !v.CreatedAt.Equal(created) || !v.ExpiresAt.Equal(expires) || v.Version != 1 || v.Status != "active" || v.StopReason != "" || !validText(v.Reason, 4096) || len(v.ToolPins) < 1 || len(v.ToolPins) > domain.MaxModelTools {
			return false
		}
		seen := map[string]bool{}
		for _, p := range v.ToolPins {
			if !validText(p.ToolID, 128) || p.ToolVersion < 1 || !validDigest(p.ToolProfileDigest) || seen[p.ToolID] {
				return false
			}
			seen[p.ToolID] = true
		}
		return true // Allocation validator checks exact pins, limits and counters.
	})
}

func recoveryConfigurationGrants(ctx context.Context, tx pgx.Tx, schema string) error {
	q := "SELECT x.body,x.organization_id,x.id,x.issuer_id,x.subject_id,COALESCE(x.parent_grant_id,''),x.depth,x.max_plans,x.max_verifications,x.max_allocated_charge_microusd,x.created_at,x.expires_at,audit.n,COALESCE(audit.digest,'') FROM " + recoveryConfigurationName(schema, "configuration_grants") + " x" + recoveryConfigurationAudit(schema, "grant", "grant.created", "x.issuer_id")
	return recoveryConfigurationRows(ctx, tx, q, func(rows pgx.Rows) bool {
		var raw []byte
		var org, id, issuer, subject, parent, digest string
		var depth, plans, checks int
		var charge, n int64
		var created, expires time.Time
		if rows.Scan(&raw, &org, &id, &issuer, &subject, &parent, &depth, &plans, &checks, &charge, &created, &expires, &n, &digest) != nil {
			return false
		}
		var v domain.CapabilityEnvelope
		if !recoveryConfigurationJSON(raw, &v, 32768) || v.ID != id || v.OrganizationID != org || v.IssuerID != issuer || v.SubjectID != subject || v.ParentGrantID != parent || v.Depth != depth || v.Audience != domain.ConfigurationAudience || v.MaxPlans != plans || v.MaxVerifications != checks || v.MaxAllocatedChargeMicroUSD != charge || !v.CreatedAt.Equal(created) || !v.ExpiresAt.Equal(expires) || v.Version != 1 || v.UsedPlans != 0 || v.UsedVerifications != 0 || v.AllocatedChargeMicroUSD != 0 || v.Revoked || v.Status != "active" || len(v.AvailableCommands) != 0 || n != 1 || digestValue(v) != digest {
			return false
		}
		seen := map[string]bool{}
		if len(v.Actions) < 1 || len(v.Actions) > len(domain.ConfigurationActions) || len(v.Scopes) < 1 || len(v.Scopes) > domain.MaxConfigurationScopes {
			return false
		}
		for _, a := range v.Actions {
			allowed := false
			for _, known := range domain.ConfigurationActions {
				if a == known {
					allowed = true
				}
			}
			if !allowed || seen[a] {
				return false
			}
			seen[a] = true
		}
		return true
	})
}

func recoveryConfigurationPlanDigest(v domain.ConfigurationPlan) string {
	// Exact anonymous field names, order and types from CreateConfigurationPlan.
	return digestValue(struct {
		OrganizationID, ActorID, GrantID string
		BaseRevision                     int64
		ExpiresAt                        time.Time
		Operations                       []domain.ConfigurationOperation
		Reason                           string
	}{v.OrganizationID, v.ActorID, v.GrantID, v.BaseRevision, v.ExpiresAt, v.Operations, v.Reason})
}

func recoveryConfigurationPlanBody(raw []byte) (domain.ConfigurationPlan, bool) {
	var v domain.ConfigurationPlan
	if !recoveryConfigurationJSON(raw, &v, 32768) || v.Version != 1 || v.Status != "planned" || v.AppliedAt != nil || v.AppliedRevision != 0 || v.ReceiptID != "" || len(v.AvailableCommands) != 0 || v.CreatedAt.IsZero() || !v.ExpiresAt.After(v.CreatedAt) || v.BaseRevision < 1 || !validText(v.Reason, 4096) || len(v.Operations) < 1 || len(v.Operations) > 8 || len(v.Diff) != len(v.Operations) || recoveryConfigurationPlanDigest(v) != v.Digest {
		return v, false
	}
	ids, slots := map[string]bool{}, map[string]bool{}
	for i, op := range v.Operations {
		s := op.Scope
		slot := s.Kind + "\x1f" + s.TargetID
		if !configurationIdentifier.MatchString(op.ID) || ids[op.ID] || slots[slot] || op.ExpectedResourceVersion < 0 || op.ExpectedResourceVersion > 2147483646 || (s.Kind != "model" && s.Kind != "mcp") || !configurationIdentifier.MatchString(s.DestinationID) || !configurationIdentifier.MatchString(s.TargetID) || !configurationIdentifier.MatchString(s.CredentialPurposeID) || s.DestinationVersion < 1 || s.CredentialPurposeVersion < 1 {
			return v, false
		}
		ids[op.ID], slots[slot] = true, true
		d := v.Diff[i]
		if d.OperationID != op.ID || d.Kind != s.Kind || d.TargetID != s.TargetID || d.ExpectedVersion != op.ExpectedResourceVersion || d.NewVersion != op.ExpectedResourceVersion+1 || d.Active != op.Active || d.MayStopPinnedTasks != (s.Kind == "model" && len(d.RetiredVersions) > 0) {
			return v, false
		}
		last := 0
		for _, version := range d.RetiredVersions {
			if version <= last || version > op.ExpectedResourceVersion {
				return v, false
			}
			last = version
		}
	}
	return v, true
}

func recoveryConfigurationPlans(ctx context.Context, tx pgx.Tx, schema string) error {
	q := "SELECT x.body,x.organization_id,x.id,x.actor_id,COALESCE(x.grant_id,''),x.base_revision,x.digest,x.created_at,x.expires_at,x.status,x.version,r.revision,COALESCE(x.applied_revision,0),audit.n,COALESCE(audit.digest,'') FROM " + recoveryConfigurationName(schema, "configuration_plans") + " x JOIN " + recoveryConfigurationName(schema, "configuration_revisions") + " r USING(organization_id)" + recoveryConfigurationAudit(schema, "plan", "plan.created", "x.actor_id")
	return recoveryConfigurationRows(ctx, tx, q, func(rows pgx.Rows) bool {
		var raw []byte
		var org, id, actor, grant, digest, status, audit string
		var base, revision, applied, n int64
		var version int
		var created, expires time.Time
		if rows.Scan(&raw, &org, &id, &actor, &grant, &base, &digest, &created, &expires, &status, &version, &revision, &applied, &n, &audit) != nil {
			return false
		}
		v, ok := recoveryConfigurationPlanBody(raw)
		return ok && v.OrganizationID == org && v.ID == id && v.ActorID == actor && v.GrantID == grant && v.BaseRevision == base && v.Digest == digest && v.CreatedAt.Equal(created) && v.ExpiresAt.Equal(expires) && base <= revision && applied <= revision && ((status == "planned" && version == 1) || (status == "applied" && version == 2)) && n == 1 && audit == digest
	})
}

func recoveryConfigurationValidations(ctx context.Context, tx pgx.Tx, schema string) error {
	q := "SELECT x.body,x.organization_id,x.id,x.plan_id,p.body,p.version,r.revision,audit.n,COALESCE(audit.digest,'') FROM " + recoveryConfigurationName(schema, "configuration_validations") + " x JOIN " + recoveryConfigurationName(schema, "configuration_plans") + " p ON p.organization_id=x.organization_id AND p.id=x.plan_id JOIN " + recoveryConfigurationName(schema, "configuration_revisions") + " r ON r.organization_id=x.organization_id" + recoveryConfigurationAudit(schema, "validation", "plan.validated", "p.actor_id")
	return recoveryConfigurationRows(ctx, tx, q, func(rows pgx.Rows) bool {
		var raw, plan []byte
		var org, id, pid, digest string
		var version int
		var revision, n int64
		if rows.Scan(&raw, &org, &id, &pid, &plan, &version, &revision, &n, &digest) != nil {
			return false
		}
		p, ok := recoveryConfigurationPlanBody(plan)
		var v domain.ConfigurationValidation
		if !ok || !recoveryConfigurationJSON(raw, &v, 65536) || v.ID != id || v.OrganizationID != org || v.PlanID != pid || v.PlanDigest != p.Digest || v.BaseRevision != p.BaseRevision || v.PlanVersion < 1 || v.PlanVersion > version || v.CurrentRevision < p.BaseRevision || v.CurrentRevision > revision || !reflect.DeepEqual(v.Diff, p.Diff) || v.CreatedAt.IsZero() || n != 1 || digestValue(v) != digest {
			return false
		}
		d := v.Decision
		if !configurationIdentifier.MatchString(d.ID) || d.OrganizationID != org || d.Action != "apply" || d.TargetID != pid || d.GrantID != p.GrantID || d.ConfigurationRevision != v.CurrentRevision || !d.EvaluatedAt.Equal(v.CreatedAt) || v.Valid != (len(d.ReasonCodes) == 0) {
			return false
		}
		if len(d.Obligations) > 1 || (len(d.Obligations) == 1 && d.Obligations[0] != "owner_approval") {
			return false
		}
		for _, r := range d.ReasonCodes {
			switch r {
			case "profile_inactive", "resource_version_changed", "model_name_collision", "plan_expired", "configuration_revision_changed", "plan_already_applied":
			default:
				return false
			}
		}
		for reason, expected := range map[string]bool{"configuration_revision_changed": v.CurrentRevision != p.BaseRevision, "plan_already_applied": v.PlanVersion == 2, "plan_expired": !v.CreatedAt.Before(p.ExpiresAt)} {
			count := 0
			for _, r := range d.ReasonCodes {
				if r == reason {
					count++
				}
			}
			if (expected && count != 1) || (!expected && count != 0) {
				return false
			}
		}
		outcome := "allow"
		if !v.Valid {
			outcome = "deny"
		} else if len(d.Obligations) > 0 {
			outcome = "requires_approval"
		}
		return d.Outcome == outcome && (!v.Valid || (v.PlanVersion == 1 && v.CurrentRevision == p.BaseRevision && v.CreatedAt.Before(p.ExpiresAt)))
	})
}

func recoveryConfigurationApprovals(ctx context.Context, tx pgx.Tx, schema string) error {
	q := "SELECT x.body,x.organization_id,x.id,x.plan_id,x.validation_id,x.principal_id,p.digest,p.expires_at,v.body,audit.n,COALESCE(audit.digest,'') FROM " + recoveryConfigurationName(schema, "configuration_approvals") + " x JOIN " + recoveryConfigurationName(schema, "configuration_plans") + " p ON p.organization_id=x.organization_id AND p.id=x.plan_id JOIN " + recoveryConfigurationName(schema, "configuration_validations") + " v ON v.organization_id=x.organization_id AND v.id=x.validation_id AND v.plan_id=x.plan_id" + recoveryConfigurationAudit(schema, "approval", "plan.approved", "x.principal_id")
	return recoveryConfigurationRows(ctx, tx, q, func(rows pgx.Rows) bool {
		var raw, validation []byte
		var org, id, pid, vid, actor, planDigest, audit string
		var n int64
		var planExpires time.Time
		if rows.Scan(&raw, &org, &id, &pid, &vid, &actor, &planDigest, &planExpires, &validation, &n, &audit) != nil {
			return false
		}
		var v domain.ConfigurationApproval
		var valid domain.ConfigurationValidation
		return recoveryConfigurationJSON(raw, &v, 16384) && decodeRecoveryHistoryJSON(validation, &valid, 65536) == nil && v.ID == id && v.OrganizationID == org && v.PlanID == pid && v.ValidationID == vid && v.PrincipalID == actor && v.PlanDigest == planDigest && valid.Valid && valid.PlanVersion == 1 && valid.PlanDigest == planDigest && valid.PlanID == pid && valid.CurrentRevision == valid.BaseRevision && !v.CreatedAt.IsZero() && v.CreatedAt.Before(planExpires) && validText(v.Reason, 4096) && n == 1 && digestValue(v) == audit
	})
}

func recoveryConfigurationVerifications(ctx context.Context, tx pgx.Tx, schema string) error {
	q := "SELECT x.body,x.organization_id,x.id,x.plan_id,x.operation_id,x.actor_id,COALESCE(x.grant_id,''),x.kind,x.target_id,x.resource_version,COALESCE(x.task_id,''),x.created_at,p.body,p.status,x.outcome,COALESCE(x.evidence_digest,''),x.status,audit.n,COALESCE(audit.digest,'') FROM " + recoveryConfigurationName(schema, "configuration_verifications") + " x JOIN " + recoveryConfigurationName(schema, "configuration_plans") + " p ON p.organization_id=x.organization_id AND p.id=x.plan_id" + recoveryConfigurationAudit(schema, "verification", "verification.created", "x.actor_id")
	return recoveryConfigurationRows(ctx, tx, q, func(rows pgx.Rows) bool {
		var raw, plan, outcome []byte
		var org, id, pid, opid, actor, grant, kind, target, task, planStatus, digest, status, audit string
		var version int
		var n int64
		var created time.Time
		if rows.Scan(&raw, &org, &id, &pid, &opid, &actor, &grant, &kind, &target, &version, &task, &created, &plan, &planStatus, &outcome, &digest, &status, &n, &audit) != nil {
			return false
		}
		p, ok := recoveryConfigurationPlanBody(plan)
		var v domain.ConfigurationVerification
		if !ok || !recoveryConfigurationJSON(raw, &v, 32768) || v.OrganizationID != org || v.ID != id || v.PlanID != pid || v.OperationID != opid || v.ActorID != actor || actor != p.ActorID || v.GrantID != grant || grant != p.GrantID || v.Kind != kind || v.Scope.Kind != kind || v.Scope.TargetID != target || v.ResourceVersion != version || v.TaskID != task || !v.CreatedAt.Equal(created) || !v.UpdatedAt.Equal(created) || v.Status != "queued" || v.Version != 1 || v.ProtocolVersion != "" || v.ToolCount != 0 || v.CatalogDigest != "" || v.ResponseDigest != "" || v.ReasonCode != "" || v.MaxChargeMicroUSD < 0 || v.MaxChargeMicroUSD > 1_000_000_000_000 || planStatus != "applied" || n != 1 || digestValue(v) != audit {
			return false
		}
		found := false
		for _, op := range p.Operations {
			if op.ID == opid {
				found = op.Active && op.Scope == v.Scope && op.ExpectedResourceVersion+1 == version
			}
		}
		if !found || (kind == "model" && (task == "" || v.CostStatus != "not_reserved")) || (kind == "mcp" && (task != "" || v.CostStatus != "unmetered" || v.MaxChargeMicroUSD != 0)) {
			return false
		}
		if len(outcome) == 0 {
			return digest == "" && status != "response_received"
		}
		if status != "response_received" {
			return false
		}
		var o struct {
			Outcome         string `json:"outcome"`
			ProtocolVersion string `json:"protocol_version,omitempty"`
			ToolCount       *int   `json:"tool_count,omitempty"`
			CatalogDigest   string `json:"catalog_digest,omitempty"`
			ResponseDigest  string `json:"response_digest,omitempty"`
		}
		if !recoveryConfigurationJSON(outcome, &o, 8192) || o.Outcome != "response_received" {
			return false
		}
		m := map[string]any{"outcome": o.Outcome}
		if kind == "model" {
			if o.ProtocolVersion != "" || o.ToolCount != nil || o.CatalogDigest != "" || !validDigest(o.ResponseDigest) {
				return false
			}
			m["response_digest"] = o.ResponseDigest
		} else {
			if o.ProtocolVersion != mcpbridge.ProtocolVersion || o.ToolCount == nil || *o.ToolCount < 0 || *o.ToolCount > 128 || !validDigest(o.CatalogDigest) || (o.ResponseDigest != "" && !validDigest(o.ResponseDigest)) {
				return false
			}
			m["protocol_version"], m["tool_count"], m["catalog_digest"] = o.ProtocolVersion, *o.ToolCount, o.CatalogDigest
			if o.ResponseDigest != "" {
				m["response_digest"] = o.ResponseDigest
			}
		}
		return digestValue(m) == digest
	})
}

// JSON casts below run only AFTER the streaming strict DTO passes above. Every
// identifier is from the fixed source list; operators/functions use pg_catalog.
func recoveryConfigurationHistoryQueries(schema string) []string {
	queries := []string{
		`SELECT NOT EXISTS(SELECT 1 FROM {configuration_plans} p CROSS JOIN LATERAL pg_catalog.jsonb_array_elements(p.body->'operations') o
 WHERE NOT EXISTS(SELECT 1 FROM {configuration_destinations} d WHERE d.organization_id=p.organization_id AND d.id=o#>>'{scope,destination_id}' AND d.version=(o#>>'{scope,destination_version}')::integer AND d.kind=o#>>'{scope,kind}' AND d.target_id=o#>>'{scope,target_id}' AND d.credential_purpose_id=o#>>'{scope,credential_purpose_id}' AND d.credential_purpose_version=(o#>>'{scope,credential_purpose_version}')::integer)
 OR (p.status='applied' AND NOT EXISTS(SELECT 1 FROM {configuration_resource_bindings} b WHERE b.organization_id=p.organization_id AND b.kind=o#>>'{scope,kind}' AND b.target_id=o#>>'{scope,target_id}' AND b.resource_version=(o->>'expected_resource_version')::integer+1 AND b.destination_id=o#>>'{scope,destination_id}' AND b.destination_version=(o#>>'{scope,destination_version}')::integer AND b.credential_purpose_id=o#>>'{scope,credential_purpose_id}' AND b.credential_purpose_version=(o#>>'{scope,credential_purpose_version}')::integer)))`,
		`SELECT NOT EXISTS(SELECT 1 FROM {configuration_validations} v JOIN {configuration_plans} p ON p.organization_id=v.organization_id AND p.id=v.plan_id
 WHERE (v.body#>'{decision,obligations}'='["owner_approval"]'::jsonb) IS DISTINCT FROM EXISTS(SELECT 1 FROM pg_catalog.jsonb_array_elements(p.body->'operations') o JOIN {configuration_destinations} d ON d.organization_id=p.organization_id AND d.id=o#>>'{scope,destination_id}' AND d.version=(o#>>'{scope,destination_version}')::integer WHERE d.profile#>'{destination,requires_approval}'='true'::jsonb))`,
		`SELECT NOT EXISTS(SELECT 1 FROM {configuration_plans} p WHERE p.status='applied' AND (NOT EXISTS(SELECT 1 FROM {configuration_validations} v WHERE v.organization_id=p.organization_id AND v.plan_id=p.id AND v.body->'valid'='true'::jsonb AND v.body->>'plan_version'='1' AND v.body->>'current_revision'=p.base_revision::text AND (v.body#>'{decision,obligations}'='[]'::jsonb OR EXISTS(SELECT 1 FROM {configuration_approvals} a WHERE a.organization_id=p.organization_id AND a.plan_id=p.id AND a.validation_id=v.id))) OR (SELECT pg_catalog.count(*) FROM {configuration_audit} a WHERE a.organization_id=p.organization_id AND a.object_type='plan' AND a.object_id=p.id AND a.action='plan.applied' AND a.actor_type='principal' AND a.actor_id=p.actor_id AND a.digest=p.digest)<>1))`,
		`SELECT NOT EXISTS(SELECT 1 FROM {configuration_verifications} v WHERE NOT EXISTS(SELECT 1 FROM {configuration_resource_bindings} b WHERE b.organization_id=v.organization_id AND b.kind=v.kind AND b.target_id=v.target_id AND b.resource_version=v.resource_version AND b.destination_id=v.body#>>'{scope,destination_id}' AND b.destination_version=(v.body#>>'{scope,destination_version}')::integer AND b.credential_purpose_id=v.body#>>'{scope,credential_purpose_id}' AND b.credential_purpose_version=(v.body#>>'{scope,credential_purpose_version}')::integer)
 OR (v.kind='model' AND NOT EXISTS(SELECT 1 FROM {tasks} t WHERE t.organization_id=v.organization_id AND t.id=v.task_id AND t.owner_id=v.actor_id AND t.model_connection_id=v.target_id AND t.model_connection_version=v.resource_version))
 OR (v.kind='model' AND v.dispatch_started_at IS NOT NULL AND NOT EXISTS(SELECT 1 FROM {reservations} r JOIN {actions} a ON a.organization_id=r.organization_id AND a.id=r.action_id WHERE r.organization_id=v.organization_id AND r.task_id=v.task_id AND r.category='model' AND a.task_id=v.task_id AND a.operation_id=r.operation_id AND a.dispatch_issued_at=v.dispatch_started_at AND a.dispatch_fencing_token=v.dispatched_fencing_token))
 OR (v.status='response_received' AND (SELECT pg_catalog.count(*) FROM {configuration_audit} a WHERE a.organization_id=v.organization_id AND a.object_type='verification' AND a.object_id=v.id AND a.action='response_received' AND a.actor_type='worker' AND a.actor_id=v.dispatched_worker_id AND a.digest=v.evidence_digest)<>1))`,
		`SELECT NOT EXISTS(SELECT 1 FROM {configuration_verifications} v JOIN {model_candidates} c ON c.organization_id=v.organization_id AND c.task_id=v.task_id JOIN {artifacts} a ON a.organization_id=c.organization_id AND a.id=c.artifact_id WHERE v.kind='model' AND (a.content<>'Connection returned a structurally valid response. Response content was discarded.' OR a.provenance->>'provider_request_id' IS DISTINCT FROM '' OR a.provenance->'proposal_count' IS DISTINCT FROM '0'::jsonb))`,
		`SELECT NOT EXISTS(SELECT 1 FROM {configuration_grants} g CROSS JOIN LATERAL pg_catalog.jsonb_array_elements(g.body->'scopes') s WHERE NOT EXISTS(SELECT 1 FROM {configuration_destinations} d WHERE d.organization_id=g.organization_id AND d.id=s->>'destination_id' AND d.version::text=s->>'destination_version' AND d.kind=s->>'kind' AND d.target_id=s->>'target_id' AND d.credential_purpose_id=s->>'credential_purpose_id' AND d.credential_purpose_version::text=s->>'credential_purpose_version'))`,
		`SELECT NOT EXISTS(SELECT 1 FROM {configuration_resource_bindings} b WHERE (SELECT pg_catalog.count(*) FROM {configuration_plans} p CROSS JOIN LATERAL pg_catalog.jsonb_array_elements(p.body->'operations') o WHERE p.organization_id=b.organization_id AND p.status='applied' AND o#>>'{scope,kind}'=b.kind AND o#>>'{scope,target_id}'=b.target_id AND (o->>'expected_resource_version')::integer+1=b.resource_version)<>1)
 AND NOT EXISTS(SELECT organization_id,receipt_id FROM {configuration_plans} WHERE status='applied' GROUP BY organization_id,receipt_id HAVING pg_catalog.count(*)<>1)`,
	}
	tables := []string{"configuration_grants", "configuration_plans", "configuration_validations", "configuration_approvals", "configuration_verifications", "configuration_audit", "configuration_destinations", "configuration_resource_bindings", "tasks", "reservations", "actions", "model_candidates", "artifacts"}
	pairs := []string{}
	for _, table := range tables {
		pairs = append(pairs, "{"+table+"}", recoveryConfigurationName(schema, table))
	}
	r := strings.NewReplacer(pairs...)
	for i := range queries {
		queries[i] = r.Replace(queries[i])
	}
	return queries
}
