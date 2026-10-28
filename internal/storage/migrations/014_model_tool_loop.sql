-- SPDX-License-Identifier: Elastic-2.0
ALTER TABLE agents ADD COLUMN model_tools jsonb NOT NULL DEFAULT '[]' CHECK(jsonb_typeof(model_tools)='array' AND jsonb_array_length(model_tools)<=16);
CREATE FUNCTION protect_model_tool_pins() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.model_tools IS DISTINCT FROM OLD.model_tools THEN
 RAISE EXCEPTION 'agent tool pins are immutable; create another version' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER agent_model_tool_pins BEFORE UPDATE ON agents FOR EACH ROW EXECUTE FUNCTION protect_model_tool_pins();
ALTER TABLE task_candidates DROP CONSTRAINT task_candidate_source;
ALTER TABLE task_candidates ADD CONSTRAINT task_candidate_source CHECK(source IN('model','model_tools','team','fixture','material'));
DROP INDEX one_generated_candidate_per_run;
CREATE UNIQUE INDEX one_generated_candidate_per_operation ON task_candidates(organization_id,operation_id) WHERE source IN('model','model_tools','team','fixture');

CREATE TABLE model_turn_counts (
 -- Old roots could legitimately have up to384 model attempts. Preserve their
 -- full history on upgrade; the admission trigger below denies new turns once
 -- the new32/128 policy is exhausted, without clamping or resetting counters.
 organization_id text NOT NULL,task_id text NOT NULL,own_turns integer NOT NULL DEFAULT 0 CHECK(own_turns BETWEEN 0 AND 384),
 tree_turns integer NOT NULL DEFAULT 0 CHECK(tree_turns BETWEEN 0 AND 384),PRIMARY KEY(organization_id,task_id),
 FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id)
);
CREATE TABLE model_turns (
 organization_id text NOT NULL,operation_id text NOT NULL,task_id text NOT NULL,run_id text NOT NULL,
 ordinal integer NOT NULL CHECK(ordinal BETWEEN 1 AND 32),messages_json text NOT NULL CHECK(octet_length(messages_json) BETWEEN 2 AND 2097152),
 messages_digest text NOT NULL CHECK(messages_digest ~ '^[0-9a-f]{64}$'),source_candidate_id text,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),PRIMARY KEY(organization_id,operation_id),
 UNIQUE(organization_id,run_id,ordinal),UNIQUE(organization_id,source_candidate_id),
 FOREIGN KEY(organization_id,operation_id,run_id,task_id) REFERENCES operations(organization_id,id,run_id,task_id) DEFERRABLE INITIALLY DEFERRED,
 FOREIGN KEY(organization_id,source_candidate_id,task_id,run_id) REFERENCES task_candidates(organization_id,id,task_id,run_id),
 CHECK((ordinal=1 AND source_candidate_id IS NULL) OR (ordinal>1 AND source_candidate_id IS NOT NULL)),
 CHECK(messages_digest=encode(sha256(convert_to(messages_json,'UTF8')),'hex')),
 CHECK(jsonb_typeof(messages_json::jsonb)='array' AND jsonb_array_length(messages_json::jsonb) BETWEEN 2 AND 290)
);
-- Preserve prior operations in lifetime counts. Existing text-only runs had one
-- model operation each. The exact original system/user inputs remain durable.
INSERT INTO model_turns(organization_id,operation_id,task_id,run_id,ordinal,messages_json,messages_digest)
SELECT o.organization_id,o.id,o.task_id,o.run_id,1,m.body,encode(sha256(convert_to(m.body,'UTF8')),'hex')
FROM operations o JOIN tasks t ON t.organization_id=o.organization_id AND t.id=o.task_id
JOIN runs r ON r.organization_id=o.organization_id AND r.id=o.run_id
JOIN agents a ON a.organization_id=t.organization_id AND a.id=t.agent_id AND a.version=t.agent_version
CROSS JOIN LATERAL (SELECT jsonb_build_array(jsonb_build_object('role','system','content',a.instructions),jsonb_build_object('role','user','content',COALESCE(NULLIF(r.input_context,''),t.goal)))::text AS body) m
WHERE t.model_connection_id IS NOT NULL;
INSERT INTO model_turn_counts(organization_id,task_id,own_turns,tree_turns)
SELECT t.organization_id,t.id,(SELECT count(*) FROM model_turns m WHERE m.organization_id=t.organization_id AND m.task_id=t.id),
CASE WHEN t.parent_id IS NULL THEN (SELECT count(*) FROM model_turns m JOIN tasks c ON c.organization_id=m.organization_id AND c.id=m.task_id WHERE c.organization_id=t.organization_id AND c.root_id=t.id) ELSE 0 END
FROM tasks t WHERE EXISTS(SELECT 1 FROM model_turns m WHERE m.organization_id=t.organization_id AND (m.task_id=t.id OR (t.parent_id IS NULL AND EXISTS(SELECT 1 FROM tasks c WHERE c.organization_id=m.organization_id AND c.id=m.task_id AND c.root_id=t.id))));

CREATE FUNCTION guard_model_turn() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE t tasks; expected integer;
BEGIN
 SELECT * INTO t FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.task_id;
 IF NOT FOUND OR t.model_connection_id IS NULL OR t.current_run_id IS DISTINCT FROM NEW.run_id THEN
 RAISE EXCEPTION 'model turn requires current pinned model run' USING ERRCODE='23514'; END IF;
 SELECT COALESCE(max(ordinal),0)+1 INTO expected FROM model_turns WHERE organization_id=NEW.organization_id AND run_id=NEW.run_id;
 IF NEW.ordinal<>expected OR (NEW.source_candidate_id IS NOT NULL AND (t.status<>'waiting_input' OR t.waiting_reason<>'model_tools_pending' OR t.candidate_id IS DISTINCT FROM NEW.source_candidate_id
 OR NOT EXISTS(SELECT 1 FROM task_candidates c WHERE c.organization_id=NEW.organization_id AND c.id=NEW.source_candidate_id AND c.task_id=NEW.task_id AND c.run_id=NEW.run_id AND c.source='model_tools'))) THEN
 RAISE EXCEPTION 'model continuation requires exact current tool proposal candidate' USING ERRCODE='23514'; END IF;
 INSERT INTO model_turn_counts(organization_id,task_id) VALUES(NEW.organization_id,NEW.task_id) ON CONFLICT DO NOTHING;
 INSERT INTO model_turn_counts(organization_id,task_id) VALUES(NEW.organization_id,t.root_id) ON CONFLICT DO NOTHING;
 UPDATE model_turn_counts SET own_turns=own_turns+1 WHERE organization_id=NEW.organization_id AND task_id=NEW.task_id AND own_turns<32;
 IF NOT FOUND THEN RAISE EXCEPTION 'model task lifetime turn limit' USING ERRCODE='23514'; END IF;
 UPDATE model_turn_counts SET tree_turns=tree_turns+1 WHERE organization_id=NEW.organization_id AND task_id=t.root_id AND tree_turns<128;
 IF NOT FOUND THEN RAISE EXCEPTION 'model root lifetime turn limit' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER model_turn_guard BEFORE INSERT ON model_turns FOR EACH ROW EXECUTE FUNCTION guard_model_turn();
CREATE TRIGGER model_turn_immutable BEFORE UPDATE OR DELETE ON model_turns FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER model_turn_count_no_delete BEFORE DELETE ON model_turn_counts FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE FUNCTION guard_model_turn_count() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.task_id IS DISTINCT FROM OLD.task_id
 OR NEW.own_turns<OLD.own_turns OR NEW.tree_turns<OLD.tree_turns OR NEW.own_turns>OLD.own_turns+1 OR NEW.tree_turns>OLD.tree_turns+1
 OR (NEW.own_turns=OLD.own_turns AND NEW.tree_turns=OLD.tree_turns) THEN
 RAISE EXCEPTION 'model turn counters cannot reset or jump' USING ERRCODE='23514'; END IF; RETURN NEW;
END $$;
CREATE TRIGGER model_turn_count_guard BEFORE UPDATE ON model_turn_counts FOR EACH ROW EXECUTE FUNCTION guard_model_turn_count();

CREATE FUNCTION initialize_model_turn() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE t tasks; body text;
BEGIN
 SELECT * INTO t FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.task_id;
 IF t.model_connection_id IS NULL THEN RETURN NEW; END IF;
 IF EXISTS(SELECT 1 FROM model_turns m WHERE m.organization_id=NEW.organization_id AND m.operation_id=NEW.id AND m.task_id=NEW.task_id AND m.run_id=NEW.run_id) THEN RETURN NEW; END IF;
 SELECT jsonb_build_array(jsonb_build_object('role','system','content',a.instructions),jsonb_build_object('role','user','content',COALESCE(NULLIF(r.input_context,''),t.goal)))::text INTO body
 FROM agents a JOIN runs r ON r.organization_id=a.organization_id WHERE a.organization_id=t.organization_id AND a.id=t.agent_id AND a.version=t.agent_version AND r.id=NEW.run_id AND r.task_id=t.id;
 INSERT INTO model_turns(organization_id,operation_id,task_id,run_id,ordinal,messages_json,messages_digest)
 VALUES(NEW.organization_id,NEW.id,NEW.task_id,NEW.run_id,1,body,encode(sha256(convert_to(body,'UTF8')),'hex'));
 RETURN NEW;
END $$;
CREATE TRIGGER zz_initialize_model_turn AFTER INSERT ON operations FOR EACH ROW EXECUTE FUNCTION initialize_model_turn();

CREATE TABLE model_tool_proposals (
 organization_id text NOT NULL,id text NOT NULL,task_id text NOT NULL,run_id text NOT NULL,operation_id text NOT NULL,
 candidate_id text NOT NULL,artifact_id text NOT NULL,artifact_version integer NOT NULL CHECK(artifact_version>0),content_digest text NOT NULL CHECK(content_digest ~ '^[0-9a-f]{64}$'),
 call_id text NOT NULL CHECK(call_id ~ '^[A-Za-z0-9_-]{1,64}$'),tool_id text NOT NULL,tool_version integer NOT NULL,tool_profile_digest text NOT NULL,
 tool_name text NOT NULL CHECK(tool_name ~ '^[A-Za-z0-9_-]{1,64}$'),arguments_json text NOT NULL CHECK(octet_length(arguments_json) BETWEEN 2 AND 32768),
 arguments_digest text NOT NULL CHECK(arguments_digest=encode(sha256(convert_to(arguments_json,'UTF8')),'hex')),
 ordinal integer NOT NULL CHECK(ordinal BETWEEN 1 AND 8),created_at timestamptz NOT NULL DEFAULT clock_timestamp(),PRIMARY KEY(organization_id,id),
 UNIQUE(organization_id,operation_id,call_id),UNIQUE(organization_id,operation_id,ordinal),
 FOREIGN KEY(organization_id,operation_id) REFERENCES model_turns(organization_id,operation_id),
 FOREIGN KEY(organization_id,candidate_id,task_id,run_id,operation_id) REFERENCES task_candidates(organization_id,id,task_id,run_id,operation_id),
 FOREIGN KEY(organization_id,candidate_id,task_id,run_id,artifact_id,artifact_version,content_digest) REFERENCES task_candidates(organization_id,id,task_id,run_id,artifact_id,artifact_version,content_digest),
 FOREIGN KEY(organization_id,tool_id,tool_version,tool_profile_digest) REFERENCES tool_connections(organization_id,id,version,profile_digest),
 CHECK(jsonb_typeof(arguments_json::jsonb)='object')
);
CREATE INDEX model_proposal_listing ON model_tool_proposals(organization_id,task_id,created_at DESC,id DESC);
CREATE TRIGGER model_proposal_immutable BEFORE UPDATE OR DELETE ON model_tool_proposals FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
ALTER TABLE tool_executions ADD COLUMN model_proposal_id text GENERATED ALWAYS AS (intent->>'model_proposal_id') STORED;
ALTER TABLE tool_executions ADD CONSTRAINT tool_model_proposal_identity UNIQUE(organization_id,model_proposal_id);
ALTER TABLE tool_executions ADD CONSTRAINT tool_model_proposal_binding FOREIGN KEY(organization_id,model_proposal_id) REFERENCES model_tool_proposals(organization_id,id);
CREATE FUNCTION guard_model_proposal_execution() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE source text; proposal text;
BEGIN
 SELECT c.source INTO source FROM task_candidates c WHERE c.organization_id=NEW.organization_id AND c.id=NEW.candidate_id;
 proposal=NEW.intent->>'model_proposal_id';
 IF source='model_tools' THEN
  IF proposal IS NULL OR NOT EXISTS(SELECT 1 FROM model_tool_proposals p WHERE p.organization_id=NEW.organization_id AND p.id=proposal
  AND p.task_id=NEW.task_id AND p.run_id=NEW.run_id AND p.operation_id=NEW.operation_id AND p.candidate_id=NEW.candidate_id
  AND p.tool_id=NEW.tool_id AND p.tool_version=NEW.tool_version AND p.tool_profile_digest=NEW.tool_profile_digest
  AND p.arguments_json=NEW.intent->>'arguments_json' AND p.arguments_digest=NEW.intent->>'arguments_digest'
  AND NEW.intent->>'occurrence_id'='model:'||p.id) THEN
  RAISE EXCEPTION 'model execution requires exact native proposal' USING ERRCODE='23514'; END IF;
 ELSIF proposal IS NOT NULL THEN
  RAISE EXCEPTION 'native proposal cannot attach to a final candidate' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER ab_model_proposal_execution BEFORE INSERT ON tool_executions FOR EACH ROW EXECUTE FUNCTION guard_model_proposal_execution();
