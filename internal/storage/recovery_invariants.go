// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

var ErrRecoveryInvariant = errors.New("recovery historical invariant validation failed")

// validateRecoveryInvariants checks a bounded set of timeless financial,
// identity and history projections. The caller must first establish a trusted
// typed schema, enforce all declarative constraints/FKs, hold exclusive staging
// access, and provide a bounded context. It is SELECT-only and never admits or
// repairs historical work. Success is NOT full semantic-import acceptance:
// versioned JSON/profile/intent hashes, automation/configuration allocation and
// webhook receipt/envelope projections require separate validators.
// schema may be the caller's private pg_temp namespace. No sequence is mutated
// or trusted here; generation/sequence high-water restoration is caller-owned.
func validateRecoveryInvariants(ctx context.Context, tx pgx.Tx, schema string) error {
	if ctx == nil || tx == nil || !recoverySnapshotName.MatchString(schema) {
		return ErrRecoveryInvariant
	}
	for _, check := range recoveryInvariantQueries(schema) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var valid bool
		if err := tx.QueryRow(ctx, check.sql).Scan(&valid); err != nil || !valid {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrRecoveryInvariant
		}
	}
	return nil
}

type recoveryInvariantQuery struct{ name, sql string }

// Every replacement key/table is a source-code constant. Archive names never
// enter SQL. Explicit pg_catalog functions avoid dependence on search_path.
func recoveryInvariantQueries(schema string) []recoveryInvariantQuery {
	queries := []recoveryInvariantQuery{
		{"task_lineage", `SELECT NOT EXISTS (
 SELECT 1 FROM {tasks} t LEFT JOIN {tasks} p ON p.organization_id=t.organization_id AND p.id=t.parent_id
 LEFT JOIN {tasks} root ON root.organization_id=t.organization_id AND root.id=t.root_id
 WHERE root.id IS NULL OR root.parent_id IS NOT NULL OR root.depth<>0 OR t.depth NOT BETWEEN 0 AND 16
 OR (t.parent_id IS NULL AND (t.root_id<>t.id OR t.depth<>0))
 OR (t.parent_id IS NOT NULL AND (p.id IS NULL OR t.depth<>p.depth+1 OR t.root_id<>p.root_id OR t.owner_id<>p.owner_id))
)`},
		{"reservation_lineage", `WITH RECURSIVE paths AS (
 SELECT organization_id,id,root_id,ARRAY[id]::text[] AS scope,0 AS depth FROM {tasks} WHERE parent_id IS NULL
 UNION ALL SELECT c.organization_id,c.id,c.root_id,p.scope||c.id,p.depth+1 FROM {tasks} c JOIN paths p
 ON c.organization_id=p.organization_id AND c.parent_id=p.id WHERE p.depth<16
) SELECT NOT EXISTS(SELECT 1 FROM {reservations} r LEFT JOIN paths p ON p.organization_id=r.organization_id AND p.id=r.task_id
 WHERE p.id IS NULL OR r.root_id IS DISTINCT FROM p.root_id OR r.scope_task_ids IS DISTINCT FROM p.scope)`},
		{"ledger_binding", `SELECT NOT EXISTS(SELECT 1 FROM {ledger_entries} l LEFT JOIN {reservations} r
 ON r.organization_id=l.organization_id AND r.id=l.reservation_id
 WHERE r.id IS NULL OR l.task_id<>r.task_id OR l.root_id<>r.root_id OR l.synthetic IS DISTINCT FROM (r.category='fixture_legacy'))`},
		{"reservation_projection", `WITH amounts AS (
 SELECT organization_id,reservation_id,pg_catalog.sum(delta_reserved_microusd::numeric) AS reserved,
 pg_catalog.sum(delta_spent_microusd::numeric) AS spent,pg_catalog.count(*) AS entries FROM {ledger_entries} GROUP BY organization_id,reservation_id
) SELECT NOT EXISTS(SELECT 1 FROM {reservations} r LEFT JOIN amounts a ON a.organization_id=r.organization_id AND a.reservation_id=r.id
 WHERE a.entries IS NULL OR a.reserved IS DISTINCT FROM (CASE WHEN r.status IN('reserved','unknown') THEN r.amount_microusd::numeric ELSE 0::numeric END)
 OR a.spent IS DISTINCT FROM COALESCE(r.actual_microusd::numeric,0)
 OR (r.status='settled') IS DISTINCT FROM (r.actual_microusd IS NOT NULL))`},
		{"organization_projection", `WITH amounts AS (
 SELECT organization_id,pg_catalog.sum(CASE WHEN status IN('reserved','unknown') THEN amount_microusd::numeric ELSE 0::numeric END) AS reserved,
 pg_catalog.sum(COALESCE(actual_microusd::numeric,0)) AS spent FROM {reservations} GROUP BY organization_id
) SELECT NOT EXISTS(SELECT 1 FROM {organizations} o LEFT JOIN amounts a ON a.organization_id=o.id
 WHERE o.reserved_microusd::numeric<>COALESCE(a.reserved,0) OR o.spent_microusd::numeric<>COALESCE(a.spent,0))`},
		{"task_projection", `WITH amounts AS (
 SELECT r.organization_id,scope.id,pg_catalog.sum(CASE WHEN r.status IN('reserved','unknown') THEN r.amount_microusd::numeric ELSE 0::numeric END) AS reserved,
 pg_catalog.sum(COALESCE(r.actual_microusd::numeric,0)) AS spent FROM {reservations} r CROSS JOIN LATERAL pg_catalog.unnest(r.scope_task_ids) AS scope(id)
 GROUP BY r.organization_id,scope.id
) SELECT NOT EXISTS(SELECT 1 FROM {tasks} t LEFT JOIN amounts a ON a.organization_id=t.organization_id AND a.id=t.id
 WHERE t.reserved_microusd::numeric<>COALESCE(a.reserved,0) OR t.spent_microusd::numeric<>COALESCE(a.spent,0))`},
		{"incident_and_quarantine_holds", `WITH held AS (
 SELECT r.organization_id,r.scope_task_ids FROM {reservations} r WHERE EXISTS(SELECT 1 FROM {financial_incidents} i WHERE i.organization_id=r.organization_id AND i.reservation_id=r.id)
 OR EXISTS(SELECT 1 FROM {cost_quarantines} q WHERE q.organization_id=r.organization_id AND q.reservation_id=r.id)
) SELECT NOT EXISTS(SELECT 1 FROM {financial_incidents} i LEFT JOIN {reservations} r ON r.organization_id=i.organization_id AND r.id=i.reservation_id WHERE r.id IS NULL OR i.task_id<>r.task_id)
 AND NOT EXISTS(SELECT 1 FROM held h LEFT JOIN {organizations} o ON o.id=h.organization_id WHERE o.id IS NULL OR NOT o.financial_hold)
 AND NOT EXISTS(SELECT 1 FROM held h CROSS JOIN LATERAL pg_catalog.unnest(h.scope_task_ids) scope(id)
 LEFT JOIN {tasks} t ON t.organization_id=h.organization_id AND t.id=scope.id WHERE t.id IS NULL OR NOT t.financial_hold)`},
		{"evidence_provider_and_booking", `SELECT NOT EXISTS(SELECT 1 FROM {cost_evidence} e LEFT JOIN {reservations} r ON r.organization_id=e.organization_id AND r.id=e.reservation_id
 WHERE r.id IS NULL OR e.source<>r.provider OR (
 NOT (e.id~'^evi_legacy_[0-9]+$' AND e.request_hash='legacy-unavailable')
 AND NOT EXISTS(SELECT 1 FROM {cost_quarantines} q WHERE q.organization_id=e.organization_id AND q.evidence_id=e.id AND q.reservation_id=e.reservation_id)
 AND NOT EXISTS(SELECT 1 FROM {ledger_entries} l WHERE l.organization_id=e.organization_id AND l.reservation_id=e.reservation_id AND l.event_hash=e.request_hash
 AND ((l.kind='settle' AND l.event_key='settle:'||e.source||':'||e.reference) OR (l.kind='adjustment' AND l.event_key='adjustment:'||e.source||':'||e.reference)))
 ))`},
		{"quarantine_state", `SELECT NOT EXISTS(SELECT 1 FROM {cost_quarantines} q
 LEFT JOIN {reservations} r ON r.organization_id=q.organization_id AND r.id=q.reservation_id
 LEFT JOIN {cost_evidence} e ON e.organization_id=q.organization_id AND e.id=q.evidence_id AND e.reservation_id=q.reservation_id
 LEFT JOIN {cost_quarantine_resolutions} x ON x.organization_id=q.organization_id AND x.quarantine_id=q.id
 WHERE r.id IS NULL OR e.id IS NULL OR (q.requested_kind='adjustment') IS DISTINCT FROM (q.base_status='settled')
 OR NOT EXISTS(SELECT 1 FROM {ledger_entries} l WHERE l.organization_id=q.organization_id AND l.reservation_id=q.reservation_id AND l.event_key='quarantine:'||q.id
 AND l.kind='quarantine' AND l.event_hash=e.request_hash AND l.delta_reserved_microusd=0 AND l.delta_spent_microusd=0)
 OR (x.id IS NULL AND (r.pending_quarantine_id IS DISTINCT FROM q.id OR r.revision<>q.revision
 OR r.status<>CASE WHEN q.base_status='reserved' THEN 'unknown' ELSE q.base_status END OR r.actual_microusd IS DISTINCT FROM q.base_actual_microusd))
 OR (x.id IS NOT NULL AND (r.pending_quarantine_id=q.id OR x.resulting_revision::numeric<>q.revision::numeric+1 OR r.revision<x.resulting_revision
 OR NOT EXISTS(SELECT 1 FROM {ledger_entries} l WHERE l.organization_id=q.organization_id AND l.reservation_id=q.reservation_id AND l.event_key='resolve:'||q.id
 AND l.kind='quarantine_resolution' AND l.event_hash=e.request_hash
 AND l.delta_reserved_microusd::numeric=CASE WHEN q.base_status IN('reserved','unknown') THEN -r.amount_microusd::numeric ELSE 0::numeric END
 AND l.delta_spent_microusd::numeric=e.actual_microusd::numeric-COALESCE(q.base_actual_microusd::numeric,0))))
 ) AND NOT EXISTS(SELECT 1 FROM {reservations} r WHERE r.pending_quarantine_id IS NOT NULL AND NOT EXISTS(
 SELECT 1 FROM {cost_quarantines} q WHERE q.organization_id=r.organization_id AND q.id=r.pending_quarantine_id AND q.reservation_id=r.id
 AND NOT EXISTS(SELECT 1 FROM {cost_quarantine_resolutions} x WHERE x.organization_id=q.organization_id AND x.quarantine_id=q.id)))`},
		{"credential_kinds_and_hash_domains", `SELECT NOT EXISTS(SELECT 1 FROM {memberships} m JOIN {principals} p ON p.id=m.principal_id
 WHERE p.kind='service' AND (m.organization_id IS DISTINCT FROM p.service_organization_id OR m.role NOT IN('member','viewer')))
 AND NOT EXISTS(SELECT 1 FROM {service_credentials} c LEFT JOIN {principals} p ON p.id=c.principal_id
 WHERE p.id IS NULL OR p.kind<>'service' OR p.service_organization_id IS DISTINCT FROM c.organization_id
 OR EXISTS(SELECT 1 FROM {access_tokens} a WHERE a.token_hash=c.token_hash))
 AND NOT EXISTS(SELECT 1 FROM (SELECT principal_id FROM {access_tokens} UNION ALL SELECT principal_id FROM {browser_sessions} UNION ALL SELECT principal_id FROM {identity_mappings}) h
 LEFT JOIN {principals} p ON p.id=h.principal_id WHERE p.id IS NULL OR p.kind<>'human')
 AND NOT EXISTS(SELECT 1 FROM {browser_sessions} s LEFT JOIN {identity_mappings} m ON m.issuer=s.issuer AND m.subject=s.subject
 WHERE m.principal_id IS DISTINCT FROM s.principal_id)`},
		{"credential_origins", `WITH origins AS (
 SELECT credential_generation AS generation,'access' AS kind FROM {access_tokens}
 UNION ALL SELECT credential_generation,'service' FROM {service_credentials}
 UNION ALL SELECT DISTINCT credential_generation,'browser' FROM {browser_sessions}
) SELECT NOT EXISTS(SELECT generation FROM origins GROUP BY generation HAVING generation<=0 OR pg_catalog.count(*)<>1)
 AND NOT EXISTS(SELECT credential_generation FROM {browser_sessions} GROUP BY credential_generation
 HAVING pg_catalog.count(DISTINCT ROW(principal_id,issuer,subject,authenticated_at,expires_at))<>1 OR pg_catalog.count(*) FILTER(WHERE NOT revoked)>1)
 AND NOT EXISTS(SELECT 1 FROM {memberships} WHERE credential_cutoff<0 OR security_epoch<=0 OR (credential_cutoff>0) IS DISTINCT FROM (credentials_revoked_at IS NOT NULL))
 AND NOT EXISTS(SELECT 1 FROM {organization_login_policies} WHERE human_credential_cutoff<0 OR service_credential_cutoff<0 OR human_authority_epoch<=0 OR service_authority_epoch<=0)
 AND NOT EXISTS(SELECT 1 FROM {organizations} o WHERE NOT EXISTS(SELECT 1 FROM {organization_login_policies} p WHERE p.organization_id=o.id))`},
		{"authority_snapshot_shape", `SELECT NOT EXISTS(SELECT 1 FROM {tool_executions} e
 WHERE (e.dispatch_requested_by IS NULL) IS DISTINCT FROM (e.dispatcher_authority_epoch IS NULL)
 OR (e.dispatch_requested_by IS NULL AND e.dispatcher_policy_epoch<>1))
 AND NOT EXISTS(SELECT 1 FROM {approvals} a WHERE a.execution_id IS NOT NULL AND
 ((a.decided_by IS NULL) IS DISTINCT FROM (a.decider_authority_epoch IS NULL) OR (a.decided_by IS NULL AND a.decider_policy_epoch<>1)))`},
		{"historical_authority_upper_bounds", `WITH snapshots AS (
 SELECT r.organization_id,t.owner_id AS principal,r.owner_authority_epoch AS authority_epoch,r.owner_policy_epoch AS policy_epoch FROM {runs} r JOIN {tasks} t ON t.organization_id=r.organization_id AND t.id=r.task_id
 UNION ALL SELECT organization_id,principal_id,principal_authority_epoch,principal_policy_epoch FROM {runs}
 UNION ALL SELECT o.organization_id,t.owner_id,o.owner_authority_epoch,o.owner_policy_epoch FROM {operations} o JOIN {tasks} t ON t.organization_id=o.organization_id AND t.id=o.task_id
 UNION ALL SELECT organization_id,authority_principal_id,principal_authority_epoch,principal_policy_epoch FROM {operations}
 UNION ALL SELECT organization_id,owner_id,owner_authority_epoch,owner_policy_epoch FROM {task_automation_grants}
 UNION ALL SELECT organization_id,principal_id,principal_authority_epoch,principal_policy_epoch FROM {task_automation_grants}
 UNION ALL SELECT e.organization_id,t.owner_id,e.owner_authority_epoch,e.owner_policy_epoch FROM {tool_executions} e JOIN {tasks} t ON t.organization_id=e.organization_id AND t.id=e.task_id
 UNION ALL SELECT organization_id,created_by,creator_authority_epoch,creator_policy_epoch FROM {tool_executions}
 UNION ALL SELECT organization_id,dispatch_requested_by,dispatcher_authority_epoch,dispatcher_policy_epoch FROM {tool_executions} WHERE dispatch_requested_by IS NOT NULL
 UNION ALL SELECT organization_id,decided_by,decider_authority_epoch,decider_policy_epoch FROM {approvals} WHERE execution_id IS NOT NULL AND decided_by IS NOT NULL
 UNION ALL SELECT organization_id,issuer_id,issuer_authority_epoch,issuer_policy_epoch FROM {configuration_grants}
 UNION ALL SELECT organization_id,subject_id,subject_authority_epoch,subject_policy_epoch FROM {configuration_grants}
 UNION ALL SELECT organization_id,actor_id,actor_authority_epoch,actor_policy_epoch FROM {configuration_plans}
 UNION ALL SELECT organization_id,principal_id,authority_epoch,policy_epoch FROM {configuration_approvals}
 UNION ALL SELECT organization_id,actor_id,actor_authority_epoch,actor_policy_epoch FROM {configuration_verifications}
) SELECT NOT EXISTS(SELECT 1 FROM snapshots s LEFT JOIN {memberships} m ON m.organization_id=s.organization_id AND m.principal_id=s.principal
 LEFT JOIN {principals} p ON p.id=s.principal LEFT JOIN {organization_login_policies} q ON q.organization_id=s.organization_id
 WHERE m.principal_id IS NULL OR p.id IS NULL OR q.organization_id IS NULL OR s.authority_epoch IS NULL OR s.policy_epoch IS NULL
 OR s.authority_epoch<=0 OR s.authority_epoch>m.security_epoch OR s.policy_epoch<=0
 OR s.policy_epoch>CASE p.kind WHEN 'human' THEN q.human_authority_epoch WHEN 'service' THEN q.service_authority_epoch ELSE 0 END)`},
		{"task_run_lifetime_projection", `WITH runs AS (SELECT organization_id,task_id,pg_catalog.count(*) AS n,pg_catalog.max(attempt) AS last_attempt,pg_catalog.min(attempt) AS first_attempt FROM {runs} GROUP BY organization_id,task_id)
 SELECT NOT EXISTS(SELECT 1 FROM {tasks} t LEFT JOIN runs r ON r.organization_id=t.organization_id AND r.task_id=t.id
 WHERE t.attempt_count<>COALESCE(r.n,0) OR (r.n IS NOT NULL AND (r.first_attempt<>1 OR r.last_attempt<>r.n))
 OR (t.current_run_id IS NULL) IS DISTINCT FROM (t.attempt_count=0))
 AND NOT EXISTS(SELECT 1 FROM {task_tree_controls} c LEFT JOIN {tasks} root ON root.organization_id=c.organization_id AND root.id=c.root_id
 WHERE root.id IS NULL OR root.parent_id IS NOT NULL OR c.used_tasks<>(SELECT pg_catalog.count(*) FROM {tasks} t WHERE t.organization_id=c.organization_id AND t.root_id=c.root_id)
 OR c.used_attempts<>(SELECT pg_catalog.count(*) FROM {runs} r JOIN {tasks} t ON t.organization_id=r.organization_id AND t.id=r.task_id WHERE t.organization_id=c.organization_id AND t.root_id=c.root_id))
 AND NOT EXISTS(SELECT 1 FROM {tasks} t WHERE t.parent_id IS NULL AND NOT EXISTS(SELECT 1 FROM {task_tree_controls} c WHERE c.organization_id=t.organization_id AND c.root_id=t.id))`},
		{"run_and_candidate_content", `SELECT NOT EXISTS(SELECT 1 FROM {runs} r WHERE r.input_context<>'' AND r.input_digest<>pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(r.input_context,'UTF8')),'hex'))
 AND NOT EXISTS(SELECT 1 FROM {task_candidates} c LEFT JOIN {artifacts} a ON a.organization_id=c.organization_id AND a.id=c.artifact_id AND a.version=c.artifact_version
 WHERE a.id IS NULL OR a.task_id<>c.task_id OR c.content_digest<>pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(a.content,'UTF8')),'hex'))
 AND NOT EXISTS(SELECT 1 FROM {material_revisions} r WHERE r.content_digest<>pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(r.content,'UTF8')),'hex'))`},
		{"tool_receipt_content", `SELECT NOT EXISTS(SELECT 1 FROM (
 SELECT organization_id,execution_id,artifact_id,artifact_version,content_digest FROM {tool_outcome_receipts}
 UNION ALL SELECT organization_id,execution_id,artifact_id,artifact_version,content_digest FROM {tool_outcome_conflicts}) r
 LEFT JOIN {tool_executions} e ON e.organization_id=r.organization_id AND e.id=r.execution_id
 LEFT JOIN {artifacts} a ON a.organization_id=r.organization_id AND a.id=r.artifact_id AND a.version=r.artifact_version
 WHERE e.id IS NULL OR a.id IS NULL OR a.task_id<>e.task_id OR r.content_digest<>pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(a.content,'UTF8')),'hex'))`},
		{"outbox_scope", `SELECT NOT EXISTS(SELECT 1 FROM {outbox} o LEFT JOIN {events} e ON e.sequence=o.sequence WHERE e.sequence IS NULL OR o.organization_id<>e.organization_id)`},
	}
	tables := []string{"tasks", "reservations", "ledger_entries", "organizations", "financial_incidents", "cost_quarantines", "cost_evidence", "cost_quarantine_resolutions", "memberships", "principals", "service_credentials", "access_tokens", "browser_sessions", "identity_mappings", "organization_login_policies", "tool_executions", "approvals", "runs", "operations", "task_automation_grants", "task_tree_controls", "task_candidates", "artifacts", "material_revisions", "tool_outcome_receipts", "tool_outcome_conflicts", "outbox", "events", "configuration_grants", "configuration_plans", "configuration_approvals", "configuration_verifications"}
	pairs := make([]string, 0, len(tables)*2)
	for _, table := range tables {
		pairs = append(pairs, "{"+table+"}", pgx.Identifier{schema, table}.Sanitize())
	}
	replace := strings.NewReplacer(pairs...)
	for i := range queries {
		queries[i].sql = replace.Replace(queries[i].sql)
	}
	return queries
}
