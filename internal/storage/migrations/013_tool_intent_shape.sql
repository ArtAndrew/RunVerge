-- SPDX-License-Identifier: Elastic-2.0
-- Forward hardening of the applied 012 snapshot guard. Missing JSON keys must
-- fail explicitly: SQL NULL is not a successful validation result.
CREATE FUNCTION guard_tool_intent_shape() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE source tasks; amount numeric;
BEGIN
 IF jsonb_typeof(NEW.intent) IS DISTINCT FROM 'object' OR NOT NEW.intent ?& ARRAY[
 'organization_id','id','task_id','run_id','operation_id','action_id','reservation_id','approval_id',
 'tool_id','tool_version','tool_profile_digest','candidate_id','artifact_id','artifact_version','content_digest',
 'intent_hash','created_by','occurrence_id','arguments_json','arguments_digest','source_task_version',
 'expires_at','max_charge_microusd','provider','price_version'] THEN
 RAISE EXCEPTION 'tool intent requires complete immutable snapshot' USING ERRCODE='23514'; END IF;
 IF jsonb_typeof(NEW.intent->'arguments_json') IS DISTINCT FROM 'string'
 OR jsonb_typeof(NEW.intent->'arguments_digest') IS DISTINCT FROM 'string'
 OR jsonb_typeof(NEW.intent->'source_task_version') IS DISTINCT FROM 'number'
 OR jsonb_typeof(NEW.intent->'expires_at') IS DISTINCT FROM 'string'
 OR jsonb_typeof(NEW.intent->'max_charge_microusd') IS DISTINCT FROM 'string'
 OR jsonb_typeof(NEW.intent->'provider') IS DISTINCT FROM 'string'
 OR jsonb_typeof(NEW.intent->'price_version') IS DISTINCT FROM 'string'
 OR jsonb_typeof(NEW.intent->'occurrence_id') IS DISTINCT FROM 'string'
 OR NEW.intent->>'arguments_digest' !~ '^[0-9a-f]{64}$'
 OR NEW.intent->>'source_task_version' !~ '^[1-9][0-9]{0,9}$'
 OR NEW.intent->>'max_charge_microusd' !~ '^(0|[1-9][0-9]{0,12})$'
 OR octet_length(NEW.intent->>'provider') NOT BETWEEN 1 AND 128
 OR octet_length(NEW.intent->>'price_version') NOT BETWEEN 1 AND 128 THEN
 RAISE EXCEPTION 'invalid typed tool intent fields' USING ERRCODE='23514'; END IF;
 amount=(NEW.intent->>'max_charge_microusd')::numeric;
 IF amount>1000000000000 OR (NEW.intent->>'source_task_version')::bigint>2147483647
 OR jsonb_typeof((NEW.intent->>'arguments_json')::jsonb) IS DISTINCT FROM 'object'
 OR NEW.intent->>'arguments_digest' IS DISTINCT FROM encode(sha256(convert_to(NEW.intent->>'arguments_json','UTF8')),'hex')
 OR (NEW.intent->>'expires_at')::timestamptz IS DISTINCT FROM NEW.expires_at THEN
 RAISE EXCEPTION 'tool argument or expiry snapshot mismatch' USING ERRCODE='23514'; END IF;
 SELECT * INTO source FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.task_id;
 -- Prepare captures the checked Task version, then applies exactly one atomic
 -- reserve delta (including a zero-value reserve) before inserting this intent.
 IF NOT FOUND OR (NEW.intent->>'source_task_version')::bigint IS DISTINCT FROM (source.version-1)::bigint
 OR NEW.expires_at>source.execution_deadline THEN
 RAISE EXCEPTION 'tool source version or deadline mismatch' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER aa_tool_intent_shape BEFORE INSERT ON tool_executions FOR EACH ROW EXECUTE FUNCTION guard_tool_intent_shape();
