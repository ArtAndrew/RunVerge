// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

var ErrRecoveryAllocation = errors.New("recovery historical allocation validation failed")

// validateRecoveryAllocations checks retained lifetime and storage projections,
// not current admission. The caller supplies an exclusively owned, typed stage
// with all trusted declarative constraints/FKs, and a bounded context. Versioned
// JSON/digest validation and historical authorization remain separate checks.
// It does not run application functions, change counters, or authorize a commit.
func validateRecoveryAllocations(ctx context.Context, tx pgx.Tx, schema string) error {
	if ctx == nil || tx == nil || !recoverySnapshotName.MatchString(schema) {
		return ErrRecoveryAllocation
	}
	for _, q := range recoveryAllocationQueries(schema) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var valid bool
		if err := tx.QueryRow(ctx, q.sql).Scan(&valid); err != nil || !valid {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrRecoveryAllocation
		}
	}
	return nil
}

type recoveryAllocationQuery struct{ name, sql string }

func recoveryAllocationQueries(schema string) []recoveryAllocationQuery {
	queries := []recoveryAllocationQuery{
		{"tool_lifetime", `WITH actual AS (
 SELECT t.organization_id,t.id,pg_catalog.count(e.id) AS own,
 CASE WHEN t.parent_id IS NULL THEN (SELECT pg_catalog.count(*) FROM {tool_executions} x JOIN {tasks} child ON child.organization_id=x.organization_id AND child.id=x.task_id WHERE child.organization_id=t.organization_id AND child.root_id=t.id) ELSE 0 END AS tree
 FROM {tasks} t LEFT JOIN {tool_executions} e ON e.organization_id=t.organization_id AND e.task_id=t.id GROUP BY t.organization_id,t.id,t.parent_id
) SELECT NOT EXISTS(SELECT 1 FROM actual a LEFT JOIN {tool_execution_counts} c ON c.organization_id=a.organization_id AND c.task_id=a.id
 WHERE (c.task_id IS NULL AND (a.own<>0 OR a.tree<>0)) OR (c.task_id IS NOT NULL AND (c.own_executions<>a.own OR c.tree_executions<>a.tree)))`},
		{"model_lifetime", `WITH actual AS (
 SELECT t.organization_id,t.id,pg_catalog.count(m.operation_id) AS own,
 CASE WHEN t.parent_id IS NULL THEN (SELECT pg_catalog.count(*) FROM {model_turns} x JOIN {tasks} child ON child.organization_id=x.organization_id AND child.id=x.task_id WHERE child.organization_id=t.organization_id AND child.root_id=t.id) ELSE 0 END AS tree
 FROM {tasks} t LEFT JOIN {model_turns} m ON m.organization_id=t.organization_id AND m.task_id=t.id GROUP BY t.organization_id,t.id,t.parent_id
) SELECT NOT EXISTS(SELECT 1 FROM actual a LEFT JOIN {model_turn_counts} c ON c.organization_id=a.organization_id AND c.task_id=a.id
 WHERE (c.task_id IS NULL AND (a.own<>0 OR a.tree<>0)) OR (c.task_id IS NOT NULL AND (c.own_turns<>a.own OR c.tree_turns<>a.tree)))
 AND NOT EXISTS(SELECT organization_id,run_id FROM {model_turns} GROUP BY organization_id,run_id HAVING pg_catalog.min(ordinal)<>1 OR pg_catalog.max(ordinal)<>pg_catalog.count(*))
 AND NOT EXISTS(SELECT 1 FROM {model_turns} m JOIN {tasks} t ON t.organization_id=m.organization_id AND t.id=m.task_id
 LEFT JOIN {task_candidates} c ON c.organization_id=m.organization_id AND c.id=m.source_candidate_id
 LEFT JOIN {model_turns} previous ON previous.organization_id=m.organization_id AND previous.run_id=m.run_id AND previous.ordinal=m.ordinal-1
 WHERE t.model_connection_id IS NULL OR (m.ordinal>1 AND (c.id IS NULL OR c.source<>'model_tools' OR c.operation_id IS DISTINCT FROM previous.operation_id)))`},
		// Shape is checked in a separate statement before any casts. Exclusive
		// staging ownership prevents changes between this and dependent queries.
		{"allocation_operand_shapes", `SELECT NOT EXISTS(SELECT 1 FROM {task_automation_grants} g WHERE
 pg_catalog.jsonb_typeof(g.grant_body->'max_model_turns') IS DISTINCT FROM 'number'
 OR (g.grant_body->>'max_model_turns' ~ '^[1-9][0-9]{0,2}$') IS NOT TRUE
 OR pg_catalog.jsonb_typeof(g.grant_body->'max_tool_executions') IS DISTINCT FROM 'number'
 OR (g.grant_body->>'max_tool_executions' ~ '^[1-9][0-9]{0,2}$') IS NOT TRUE
 OR pg_catalog.jsonb_typeof(g.grant_body->'max_charge_microusd') IS DISTINCT FROM 'string'
 OR (g.grant_body->>'max_charge_microusd' ~ '^(0|[1-9][0-9]{0,12})$') IS NOT TRUE
 OR pg_catalog.jsonb_typeof(g.grant_body->'agent_version') IS DISTINCT FROM 'number'
 OR pg_catalog.jsonb_typeof(g.grant_body->'model_connection_version') IS DISTINCT FROM 'number'
 OR pg_catalog.jsonb_typeof(g.grant_body->'tool_pins') IS DISTINCT FROM 'array')
 AND NOT EXISTS(SELECT 1 FROM {model_turns} m JOIN {tasks} t ON t.organization_id=m.organization_id AND t.id=m.task_id
 LEFT JOIN {model_connections} p ON p.organization_id=t.organization_id AND p.id=t.model_connection_id AND p.version=t.model_connection_version
 WHERE m.automation_grant_id IS NOT NULL AND (p.id IS NULL OR pg_catalog.jsonb_typeof(p.profile->'max_charge_microusd') IS DISTINCT FROM 'number'
 OR (p.profile->>'max_charge_microusd' ~ '^(0|[1-9][0-9]{0,12})$') IS NOT TRUE))
 AND NOT EXISTS(SELECT 1 FROM {tool_executions} e JOIN {tool_connections} p ON p.organization_id=e.organization_id AND p.id=e.tool_id AND p.version=e.tool_version
 WHERE e.automation_grant_id IS NOT NULL AND (pg_catalog.jsonb_typeof(e.intent->'max_charge_microusd') IS DISTINCT FROM 'string'
 OR (e.intent->>'max_charge_microusd' ~ '^(0|[1-9][0-9]{0,12})$') IS NOT TRUE
 OR pg_catalog.jsonb_typeof(p.profile->'tool'->'max_charge_microusd') IS DISTINCT FROM 'string'
 OR (p.profile->'tool'->>'max_charge_microusd' ~ '^(0|[1-9][0-9]{0,12})$') IS NOT TRUE))
 AND NOT EXISTS(SELECT 1 FROM {configuration_verifications} v WHERE pg_catalog.jsonb_typeof(v.body->'max_charge_microusd') IS DISTINCT FROM 'string'
 OR (v.body->>'max_charge_microusd' ~ '^(0|[1-9][0-9]{0,12})$') IS NOT TRUE)
 AND NOT EXISTS(SELECT 1 FROM {configuration_grants} g WHERE pg_catalog.jsonb_typeof(g.body->'actions') IS DISTINCT FROM 'array' OR pg_catalog.jsonb_typeof(g.body->'scopes') IS DISTINCT FROM 'array')`},
		{"automation_bindings", `SELECT NOT EXISTS(SELECT 1 FROM {task_automation_grants} g JOIN {tasks} t ON t.organization_id=g.organization_id AND t.id=g.task_id
 JOIN {agents} a ON a.organization_id=t.organization_id AND a.id=t.agent_id AND a.version=t.agent_version
 WHERE g.owner_id<>t.owner_id OR g.expires_at>t.execution_deadline OR t.model_connection_id IS NULL
 OR g.grant_body->>'id' IS DISTINCT FROM g.id OR g.grant_body->>'organization_id' IS DISTINCT FROM g.organization_id
 OR g.grant_body->>'task_id' IS DISTINCT FROM g.task_id OR g.grant_body->>'principal_id' IS DISTINCT FROM g.principal_id OR g.grant_body->>'owner_id' IS DISTINCT FROM g.owner_id
 OR g.grant_body->>'agent_id' IS DISTINCT FROM t.agent_id OR g.grant_body->'agent_version' IS DISTINCT FROM pg_catalog.to_jsonb(t.agent_version)
 OR g.grant_body->>'model_connection_id' IS DISTINCT FROM t.model_connection_id OR g.grant_body->'model_connection_version' IS DISTINCT FROM pg_catalog.to_jsonb(t.model_connection_version)
 OR (g.grant_body->>'mode' IN('read_only','prepare_and_continue')) IS NOT TRUE
 OR NOT ((a.model_tools) @> (g.grant_body->'tool_pins'))
 OR (SELECT pg_catalog.count(*)<>pg_catalog.count(DISTINCT pin->>'tool_id') FROM pg_catalog.jsonb_array_elements(g.grant_body->'tool_pins') pin))
 AND NOT EXISTS(SELECT 1 FROM {model_turns} m JOIN {task_automation_grants} g ON g.organization_id=m.organization_id AND g.id=m.automation_grant_id
 JOIN {operations} o ON o.organization_id=m.organization_id AND o.id=m.operation_id
 WHERE m.task_id<>g.task_id OR m.ordinal=1 OR o.authority_principal_id<>g.principal_id)
 AND NOT EXISTS(SELECT 1 FROM {tool_executions} e JOIN {task_automation_grants} g ON g.organization_id=e.organization_id AND g.id=e.automation_grant_id
 JOIN {reservations} r ON r.organization_id=e.organization_id AND r.id=e.reservation_id
 JOIN {tool_connections} p ON p.organization_id=e.organization_id AND p.id=e.tool_id AND p.version=e.tool_version
 WHERE e.task_id<>g.task_id OR e.created_by<>g.principal_id OR e.model_proposal_id IS NULL OR e.expires_at>g.expires_at
 OR NOT EXISTS(SELECT 1 FROM pg_catalog.jsonb_array_elements(g.grant_body->'tool_pins') pin WHERE pin->>'tool_id'=e.tool_id AND pin->'tool_version'=pg_catalog.to_jsonb(e.tool_version) AND pin->>'tool_profile_digest'=e.tool_profile_digest)
 OR (e.intent->>'max_charge_microusd')::numeric<>r.amount_microusd::numeric
 OR (e.intent->>'max_charge_microusd')::numeric<>(p.profile->'tool'->>'max_charge_microusd')::numeric)`},
		{"automation_projection", `WITH admissions AS (
 SELECT m.organization_id,m.task_id,m.automation_grant_id AS grant_id,1 AS models,0 AS tools,(p.profile->>'max_charge_microusd')::numeric AS charge
 FROM {model_turns} m JOIN {tasks} t ON t.organization_id=m.organization_id AND t.id=m.task_id
 JOIN {model_connections} p ON p.organization_id=t.organization_id AND p.id=t.model_connection_id AND p.version=t.model_connection_version WHERE m.automation_grant_id IS NOT NULL
 UNION ALL SELECT e.organization_id,e.task_id,e.automation_grant_id,0,1,(e.intent->>'max_charge_microusd')::numeric FROM {tool_executions} e WHERE e.automation_grant_id IS NOT NULL
), totals AS (SELECT organization_id,task_id,pg_catalog.sum(models) AS models,pg_catalog.sum(tools) AS tools,pg_catalog.sum(charge) AS charge FROM admissions GROUP BY organization_id,task_id),
 per_grant AS (SELECT organization_id,grant_id,pg_catalog.sum(models) AS models,pg_catalog.sum(tools) AS tools,pg_catalog.sum(charge) AS charge FROM admissions GROUP BY organization_id,grant_id)
 SELECT NOT EXISTS(SELECT 1 FROM {task_automation_usage} u FULL JOIN totals a ON a.organization_id=u.organization_id AND a.task_id=u.task_id
 WHERE u.task_id IS NULL OR u.model_turns<>COALESCE(a.models,0) OR u.tool_executions<>COALESCE(a.tools,0) OR u.allocated_microusd::numeric<>COALESCE(a.charge,0))
 AND NOT EXISTS(SELECT 1 FROM per_grant a JOIN {task_automation_grants} g ON g.organization_id=a.organization_id AND g.id=a.grant_id
 WHERE a.models>(g.grant_body->>'max_model_turns')::numeric OR a.tools>(g.grant_body->>'max_tool_executions')::numeric OR a.charge>(g.grant_body->>'max_charge_microusd')::numeric)`},
		{"configuration_lineage", `SELECT NOT EXISTS(SELECT 1 FROM {configuration_grants} g LEFT JOIN {configuration_grants} p ON p.organization_id=g.organization_id AND p.id=g.parent_grant_id
 WHERE g.body->>'audience' IS DISTINCT FROM 'platform.configuration.v1'
 OR g.body->'depth' IS DISTINCT FROM pg_catalog.to_jsonb(g.depth)
 OR COALESCE(g.body->>'parent_grant_id','') IS DISTINCT FROM COALESCE(g.parent_grant_id,'')
 OR g.body->'max_plans' IS DISTINCT FROM pg_catalog.to_jsonb(g.max_plans)
 OR g.body->'max_verifications' IS DISTINCT FROM pg_catalog.to_jsonb(g.max_verifications)
 OR g.body->'max_allocated_charge_microusd' IS DISTINCT FROM pg_catalog.to_jsonb(g.max_allocated_charge_microusd::text)
 OR (g.parent_grant_id IS NULL AND g.depth<>0) OR (g.parent_grant_id IS NOT NULL AND (p.id IS NULL OR g.depth<>p.depth+1 OR g.depth>4
 OR g.issuer_id<>p.subject_id OR g.expires_at>p.expires_at OR g.max_plans>p.max_plans OR g.max_verifications>p.max_verifications OR g.max_allocated_charge_microusd>p.max_allocated_charge_microusd
 OR NOT ((p.body->'actions') @> '["delegate"]'::jsonb) OR NOT ((p.body->'actions') @> (g.body->'actions')) OR NOT ((p.body->'scopes') @> (g.body->'scopes')))))
 AND NOT EXISTS(SELECT 1 FROM {configuration_plans} p JOIN {configuration_grants} g ON g.organization_id=p.organization_id AND g.id=p.grant_id WHERE p.actor_id<>g.subject_id OR p.expires_at>g.expires_at)
 AND NOT EXISTS(SELECT 1 FROM {configuration_verifications} v JOIN {configuration_plans} p ON p.organization_id=v.organization_id AND p.id=v.plan_id
 WHERE v.grant_id IS DISTINCT FROM p.grant_id OR v.actor_id<>p.actor_id OR (v.body->>'max_charge_microusd')::numeric>1000000000000 OR (v.kind='mcp' AND v.body->>'max_charge_microusd'<>'0'))`},
		{"configuration_projection", `WITH RECURSIVE descendants AS (
 SELECT organization_id,id AS ancestor,id AS descendant,0 AS depth FROM {configuration_grants}
 UNION ALL SELECT d.organization_id,d.ancestor,g.id,d.depth+1 FROM descendants d JOIN {configuration_grants} g ON g.organization_id=d.organization_id AND g.parent_grant_id=d.descendant WHERE d.depth<4
), plans AS (SELECT d.organization_id,d.ancestor,pg_catalog.count(*) AS used FROM descendants d JOIN {configuration_plans} p ON p.organization_id=d.organization_id AND p.grant_id=d.descendant GROUP BY d.organization_id,d.ancestor),
 verifications AS (SELECT d.organization_id,d.ancestor,pg_catalog.count(*) AS used,pg_catalog.sum((v.body->>'max_charge_microusd')::numeric) AS charge FROM descendants d JOIN {configuration_verifications} v ON v.organization_id=d.organization_id AND v.grant_id=d.descendant GROUP BY d.organization_id,d.ancestor)
 SELECT NOT EXISTS(SELECT 1 FROM {configuration_grants} g LEFT JOIN plans p ON p.organization_id=g.organization_id AND p.ancestor=g.id LEFT JOIN verifications v ON v.organization_id=g.organization_id AND v.ancestor=g.id
 WHERE g.used_plans<>COALESCE(p.used,0) OR g.used_verifications<>COALESCE(v.used,0) OR g.allocated_charge_microusd::numeric<>COALESCE(v.charge,0))`},
		{"material_projection", `WITH documents AS (SELECT organization_id,pg_catalog.count(*) AS used FROM {materials} GROUP BY organization_id),
 bytes AS (SELECT organization_id,pg_catalog.sum(amount) AS used FROM (
 SELECT organization_id,pg_catalog.octet_length(content)::numeric+pg_catalog.octet_length(reason)::numeric AS amount FROM {material_revisions}
 UNION ALL SELECT c.organization_id,pg_catalog.octet_length(r.content)::numeric FROM {task_candidates} c JOIN {material_revisions} r ON r.organization_id=c.organization_id AND r.material_id=c.material_id AND r.version=c.material_version WHERE c.source='material'
 ) b GROUP BY organization_id)
 SELECT NOT EXISTS(SELECT 1 FROM {organizations} o LEFT JOIN {material_storage} s ON s.organization_id=o.id LEFT JOIN documents d ON d.organization_id=o.id LEFT JOIN bytes b ON b.organization_id=o.id
 WHERE (s.organization_id IS NULL AND (COALESCE(d.used,0)<>0 OR COALESCE(b.used,0)<>0)) OR (s.organization_id IS NOT NULL AND (s.used_documents<>COALESCE(d.used,0) OR s.used_bytes::numeric<>COALESCE(b.used,0))))
 AND NOT EXISTS(SELECT 1 FROM {material_task_counts} c FULL JOIN (SELECT organization_id,task_id,pg_catalog.count(*) AS n FROM {materials} GROUP BY organization_id,task_id) a ON a.organization_id=c.organization_id AND a.task_id=c.task_id WHERE c.task_id IS NULL OR c.documents<>COALESCE(a.n,0))
 AND NOT EXISTS(SELECT 1 FROM {material_publication_counts} c FULL JOIN (SELECT organization_id,task_id,pg_catalog.count(*) AS n FROM {task_candidates} WHERE source='material' GROUP BY organization_id,task_id) a ON a.organization_id=c.organization_id AND a.task_id=c.task_id WHERE c.task_id IS NULL OR c.publications<>COALESCE(a.n,0))`},
		{"material_revisions", `WITH revisions AS (SELECT organization_id,material_id,pg_catalog.count(*) AS n,pg_catalog.min(version) AS first,pg_catalog.max(version) AS last FROM {material_revisions} GROUP BY organization_id,material_id)
 SELECT NOT EXISTS(SELECT 1 FROM {materials} m LEFT JOIN revisions r ON r.organization_id=m.organization_id AND r.material_id=m.id LEFT JOIN {material_revisions} first ON first.organization_id=m.organization_id AND first.material_id=m.id AND first.version=1
 WHERE r.n IS NULL OR r.first<>1 OR r.last<>r.n OR m.head_version<>r.last OR first.principal_id IS DISTINCT FROM m.created_by)
 AND NOT EXISTS(SELECT 1 FROM {material_revisions} r WHERE r.version<>r.base_version+1 OR (r.version>1 AND r.reason='') OR r.content_digest<>pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(r.content,'UTF8')),'hex'))`},
		{"material_publication", `SELECT NOT EXISTS(SELECT 1 FROM {task_candidates} c JOIN {materials} m ON m.organization_id=c.organization_id AND m.id=c.material_id
 JOIN {material_revisions} r ON r.organization_id=c.organization_id AND r.material_id=c.material_id AND r.version=c.material_version
 JOIN {artifacts} a ON a.organization_id=c.organization_id AND a.id=c.artifact_id AND a.version=c.artifact_version
 JOIN {task_candidates} p ON p.organization_id=c.organization_id AND p.id=c.parent_candidate_id JOIN {task_candidates} b ON b.organization_id=c.organization_id AND b.id=c.review_basis_candidate_id
 WHERE c.source='material' AND (b.source IN('material','model_tools') OR c.review_basis_candidate_id IS DISTINCT FROM CASE WHEN p.source='material' THEN p.review_basis_candidate_id ELSE p.id END
 OR c.artifact_version<>1 OR a.task_id<>c.task_id OR a.name<>m.name OR a.media_type<>m.media_type OR a.content<>r.content
 OR c.content_digest<>r.content_digest OR c.content_digest<>pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(a.content,'UTF8')),'hex')
 OR a.provenance IS DISTINCT FROM pg_catalog.jsonb_build_object('adapter','material:revision','verified',false,'verification','awaiting_authorized_decision','material_id',c.material_id,'material_version',c.material_version,'content_digest',c.content_digest,'parent_candidate_id',c.parent_candidate_id,'review_basis_candidate_id',c.review_basis_candidate_id,'published_by',c.published_by)))
 AND NOT EXISTS(SELECT organization_id,parent_candidate_id FROM {task_candidates} WHERE source='material' GROUP BY organization_id,parent_candidate_id HAVING pg_catalog.count(*)<>1)`},
		{"material_chain", `WITH RECURSIVE chain AS (
 SELECT organization_id,id AS origin,parent_candidate_id AS next,review_basis_candidate_id AS basis,1 AS depth FROM {task_candidates} WHERE source='material'
 UNION ALL SELECT c.organization_id,c.origin,p.parent_candidate_id,c.basis,c.depth+1 FROM chain c JOIN {task_candidates} p ON p.organization_id=c.organization_id AND p.id=c.next WHERE p.source='material' AND c.depth<64
) SELECT NOT EXISTS(SELECT 1 FROM {task_candidates} c WHERE c.source='material' AND NOT EXISTS(SELECT 1 FROM chain x JOIN {task_candidates} b ON b.organization_id=x.organization_id AND b.id=x.next WHERE x.organization_id=c.organization_id AND x.origin=c.id AND b.id=x.basis AND b.source NOT IN('material','model_tools')))`},
	}
	tables := []string{"tasks", "tool_executions", "tool_execution_counts", "model_turns", "model_turn_counts", "task_candidates", "task_automation_grants", "task_automation_usage", "model_connections", "tool_connections", "agents", "operations", "reservations", "configuration_verifications", "configuration_plans", "configuration_grants", "materials", "material_revisions", "material_storage", "material_task_counts", "material_publication_counts", "organizations", "artifacts"}
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
