-- SPDX-License-Identifier: Elastic-2.0
CREATE TABLE task_automation_grants (
 organization_id text NOT NULL,id text NOT NULL,task_id text NOT NULL,
 principal_id text NOT NULL,owner_id text NOT NULL,grant_body jsonb NOT NULL,
 status text NOT NULL DEFAULT 'active' CHECK(status IN('active','revoked','expired','stopped','completed')),
 version integer NOT NULL DEFAULT 1 CHECK(version>=1),stop_reason text NOT NULL DEFAULT '' CHECK(octet_length(stop_reason)<=4096),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),expires_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,id),UNIQUE(organization_id,id,task_id),
 FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id),
 FOREIGN KEY(organization_id,principal_id) REFERENCES memberships(organization_id,principal_id),
 FOREIGN KEY(organization_id,owner_id) REFERENCES memberships(organization_id,principal_id),
 CHECK(expires_at>created_at AND expires_at<=created_at+interval '24 hours'),
 CHECK(jsonb_typeof(grant_body)='object'),
 CHECK(grant_body ?& ARRAY['id','organization_id','task_id','principal_id','owner_id','agent_id','agent_version','model_connection_id','model_connection_version','mode','tool_pins','max_model_turns','max_tool_executions','max_charge_microusd','reason']),
 CHECK(grant_body->>'id'=id AND grant_body->>'organization_id'=organization_id AND grant_body->>'task_id'=task_id AND grant_body->>'principal_id'=principal_id AND grant_body->>'owner_id'=owner_id),
 CHECK(grant_body->>'mode' IN('read_only','prepare_and_continue')),
 CHECK(jsonb_typeof(grant_body->'tool_pins')='array' AND jsonb_array_length(grant_body->'tool_pins') BETWEEN 1 AND 16),
 CHECK(jsonb_typeof(grant_body->'max_model_turns')='number' AND (grant_body->>'max_model_turns') ~ '^[0-9]+$' AND (grant_body->>'max_model_turns')::integer BETWEEN 1 AND 31),
 CHECK(jsonb_typeof(grant_body->'max_tool_executions')='number' AND (grant_body->>'max_tool_executions') ~ '^[0-9]+$' AND (grant_body->>'max_tool_executions')::integer BETWEEN 1 AND 32),
 CHECK(jsonb_typeof(grant_body->'max_charge_microusd')='string' AND (grant_body->>'max_charge_microusd') ~ '^[1-9][0-9]{0,12}$' AND (grant_body->>'max_charge_microusd')::bigint BETWEEN 1 AND 1000000000000),
 CHECK(jsonb_typeof(grant_body->'reason')='string' AND octet_length(grant_body->>'reason') BETWEEN 1 AND 4096)
);
CREATE UNIQUE INDEX one_active_task_automation ON task_automation_grants(organization_id,task_id) WHERE status='active';
CREATE INDEX automation_latest ON task_automation_grants(organization_id,task_id,created_at DESC,id DESC);
CREATE FUNCTION guard_new_task_automation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE t tasks; pins jsonb; p jsonb; name text;
BEGIN
 FOREACH name IN ARRAY ARRAY['id','organization_id','task_id','principal_id','owner_id','agent_id','model_connection_id','mode','reason'] LOOP
  IF jsonb_typeof(NEW.grant_body->name) IS DISTINCT FROM 'string' OR octet_length(NEW.grant_body->>name)=0 THEN
  RAISE EXCEPTION 'automation requires typed nonempty consent fields' USING ERRCODE='23514'; END IF;
 END LOOP;
 FOREACH name IN ARRAY ARRAY['agent_version','model_connection_version','max_model_turns','max_tool_executions'] LOOP
  IF jsonb_typeof(NEW.grant_body->name) IS DISTINCT FROM 'number' OR (NEW.grant_body->>name) !~ '^[1-9][0-9]{0,8}$' THEN
  RAISE EXCEPTION 'automation requires bounded integer versions and limits' USING ERRCODE='23514'; END IF;
 END LOOP;
 IF jsonb_typeof(NEW.grant_body->'max_charge_microusd') IS DISTINCT FROM 'string' OR jsonb_typeof(NEW.grant_body->'tool_pins') IS DISTINCT FROM 'array' THEN
 RAISE EXCEPTION 'automation requires typed money and exact tool pins' USING ERRCODE='23514'; END IF;
 SELECT * INTO t FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.task_id;
 IF NOT FOUND OR NEW.owner_id IS DISTINCT FROM t.owner_id OR NEW.grant_body->>'agent_id' IS DISTINCT FROM t.agent_id
 OR (NEW.grant_body->>'agent_version')::integer IS DISTINCT FROM t.agent_version OR NEW.grant_body->>'model_connection_id' IS DISTINCT FROM t.model_connection_id
 OR (NEW.grant_body->>'model_connection_version')::integer IS DISTINCT FROM t.model_connection_version OR t.model_connection_id IS NULL OR NEW.expires_at>t.execution_deadline
 OR NEW.status<>'active' OR NEW.version<>1 OR NEW.stop_reason<>'' THEN
 RAISE EXCEPTION 'automation requires exact pinned task and initial consent' USING ERRCODE='23514'; END IF;
 SELECT model_tools INTO pins FROM agents WHERE organization_id=t.organization_id AND id=t.agent_id AND version=t.agent_version;
 FOR p IN SELECT value FROM jsonb_array_elements(NEW.grant_body->'tool_pins') LOOP
  IF jsonb_typeof(p) IS DISTINCT FROM 'object' OR jsonb_typeof(p->'tool_id') IS DISTINCT FROM 'string'
   OR jsonb_typeof(p->'tool_version') IS DISTINCT FROM 'number' OR (p->>'tool_version') !~ '^[1-9][0-9]{0,8}$'
   OR jsonb_typeof(p->'tool_profile_digest') IS DISTINCT FROM 'string' OR (p->>'tool_profile_digest') !~ '^[0-9a-f]{64}$'
   OR NOT EXISTS(SELECT 1 FROM jsonb_array_elements(pins) d WHERE d->>'tool_id'=p->>'tool_id' AND d->'tool_version'=p->'tool_version' AND d->>'tool_profile_digest'=p->>'tool_profile_digest') THEN
   RAISE EXCEPTION 'automation tool pin is not the task agent exact pin' USING ERRCODE='23514'; END IF;
 END LOOP;
 IF (SELECT count(*) FROM jsonb_array_elements(NEW.grant_body->'tool_pins'))<>(SELECT count(DISTINCT value->>'tool_id') FROM jsonb_array_elements(NEW.grant_body->'tool_pins')) THEN
 RAISE EXCEPTION 'automation cannot duplicate tool pins' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER task_automation_initial_guard BEFORE INSERT ON task_automation_grants FOR EACH ROW EXECUTE FUNCTION guard_new_task_automation();
CREATE FUNCTION guard_task_automation_grant() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.id IS DISTINCT FROM OLD.id OR NEW.task_id IS DISTINCT FROM OLD.task_id
 OR NEW.principal_id IS DISTINCT FROM OLD.principal_id OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.grant_body IS DISTINCT FROM OLD.grant_body
 OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.expires_at IS DISTINCT FROM OLD.expires_at OR NEW.version<>OLD.version+1
 OR OLD.status<>'active' OR NEW.status NOT IN('revoked','expired','stopped','completed') OR NEW.stop_reason='' THEN
 RAISE EXCEPTION 'task automation consent is immutable and terminal states cannot restart' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER task_automation_grant_guard BEFORE UPDATE ON task_automation_grants FOR EACH ROW EXECUTE FUNCTION guard_task_automation_grant();
CREATE TRIGGER task_automation_grant_no_delete BEFORE DELETE ON task_automation_grants FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE TABLE task_automation_usage (
 organization_id text NOT NULL,task_id text NOT NULL,model_turns integer NOT NULL DEFAULT 0 CHECK(model_turns BETWEEN 0 AND 31),
 tool_executions integer NOT NULL DEFAULT 0 CHECK(tool_executions BETWEEN 0 AND 32),allocated_microusd bigint NOT NULL DEFAULT 0 CHECK(allocated_microusd BETWEEN 0 AND 1000000000000),
 PRIMARY KEY(organization_id,task_id),FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id)
);
CREATE FUNCTION guard_task_automation_usage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.task_id IS DISTINCT FROM OLD.task_id OR NEW.model_turns<OLD.model_turns
 OR NEW.tool_executions<OLD.tool_executions OR NEW.allocated_microusd<OLD.allocated_microusd
 OR NEW.model_turns>OLD.model_turns+1 OR NEW.tool_executions>OLD.tool_executions+1
 OR NEW.model_turns+NEW.tool_executions<>OLD.model_turns+OLD.tool_executions+1 THEN
 RAISE EXCEPTION 'automation allocation cannot reset or skip a logical operation' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER task_automation_usage_guard BEFORE UPDATE ON task_automation_usage FOR EACH ROW EXECUTE FUNCTION guard_task_automation_usage();
CREATE TRIGGER task_automation_usage_no_delete BEFORE DELETE ON task_automation_usage FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE TABLE progress_signals (
 organization_id text NOT NULL,id text NOT NULL,task_id text NOT NULL,grant_id text NOT NULL,
 code text NOT NULL CHECK(code ~ '^[a-z][a-z0-9_]{0,63}$'),detector_version text NOT NULL CHECK(octet_length(detector_version) BETWEEN 1 AND 64),
 fingerprint text NOT NULL DEFAULT '' CHECK(fingerprint='' OR fingerprint ~ '^[0-9a-f]{64}$'),
 evidence_ids jsonb NOT NULL DEFAULT '[]' CHECK(jsonb_typeof(evidence_ids)='array' AND jsonb_array_length(evidence_ids)<=512 AND octet_length(evidence_ids::text)<=131072),
 snapshot_digest text NOT NULL CHECK(snapshot_digest ~ '^[0-9a-f]{64}$'),signal_key text NOT NULL CHECK(signal_key ~ '^[0-9a-f]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),PRIMARY KEY(organization_id,id),UNIQUE(organization_id,task_id,signal_key),
 FOREIGN KEY(organization_id,grant_id,task_id) REFERENCES task_automation_grants(organization_id,id,task_id)
);
CREATE INDEX progress_signals_page ON progress_signals(organization_id,task_id,created_at DESC,id DESC);
CREATE TRIGGER progress_signal_immutable BEFORE UPDATE OR DELETE ON progress_signals FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

ALTER TABLE model_turns ADD COLUMN automation_grant_id text;
ALTER TABLE model_turns ADD CONSTRAINT model_turn_automation_binding FOREIGN KEY(organization_id,automation_grant_id,task_id) REFERENCES task_automation_grants(organization_id,id,task_id);
ALTER TABLE tool_executions ADD COLUMN automation_grant_id text GENERATED ALWAYS AS (NULLIF(intent->>'automation_grant_id','')) STORED;
ALTER TABLE tool_executions ADD CONSTRAINT tool_automation_binding FOREIGN KEY(organization_id,automation_grant_id,task_id) REFERENCES task_automation_grants(organization_id,id,task_id);

-- Extend the approval-attribution guard without accepting fake human decisions.
CREATE OR REPLACE FUNCTION guard_tool_approval_shape() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE e tool_executions; g task_automation_grants;
BEGIN
 IF NEW.execution_id IS NULL THEN RETURN NEW; END IF;
 SELECT * INTO e FROM tool_executions WHERE organization_id=NEW.organization_id AND id=NEW.execution_id;
 IF NOT FOUND OR NEW.task_id IS DISTINCT FROM e.task_id OR NEW.action_id IS DISTINCT FROM e.action_id OR NEW.intent_hash IS DISTINCT FROM e.intent_hash
 OR NEW.expires_at IS DISTINCT FROM e.expires_at THEN
 RAISE EXCEPTION 'tool approval exact identity mismatch' USING ERRCODE='23514'; END IF;
 IF TG_OP='INSERT' AND (NEW.status<>'pending' OR NEW.version<>1 OR NEW.decided_by IS NOT NULL OR NEW.decided_at IS NOT NULL OR NEW.decision_method IS NOT NULL) THEN
 RAISE EXCEPTION 'tool approval starts undecided' USING ERRCODE='23514'; END IF;
 IF NEW.status IN('approved','denied') THEN
  IF NEW.decided_by IS NULL OR NEW.decided_at IS NULL OR NEW.reason='' OR NEW.decision_method IS NULL OR NEW.decision_method NOT IN('authorized_principal_decision','task_automation_grant') THEN
  RAISE EXCEPTION 'tool decision requires explicit attribution' USING ERRCODE='23514'; END IF;
  IF NEW.decision_method='task_automation_grant' THEN
   SELECT * INTO g FROM task_automation_grants WHERE organization_id=e.organization_id AND id=e.automation_grant_id AND task_id=e.task_id;
   IF NOT FOUND OR NEW.status<>'approved' OR g.status<>'active' OR g.expires_at<=clock_timestamp() OR g.principal_id<>NEW.decided_by
    OR e.model_proposal_id IS NULL OR g.grant_body->>'mode'<>'read_only'
    OR NOT EXISTS(SELECT 1 FROM jsonb_array_elements(g.grant_body->'tool_pins') p WHERE p->>'tool_id'=e.tool_id AND (p->>'tool_version')::integer=e.tool_version AND p->>'tool_profile_digest'=e.tool_profile_digest)
    OR NOT EXISTS(SELECT 1 FROM tool_connections c WHERE c.organization_id=e.organization_id AND c.id=e.tool_id AND c.version=e.tool_version
     AND c.active AND c.profile->'tool'->>'effect'='read' AND c.profile->'tool'->>'automation_allowed'='true' AND c.profile->'tool'->>'require_distinct_approver'='false') THEN
    RAISE EXCEPTION 'automatic approval requires current narrow read grant' USING ERRCODE='23514'; END IF;
  END IF;
 ELSIF NEW.decided_by IS NOT NULL OR NEW.decided_at IS NOT NULL OR NEW.decision_method IS NOT NULL THEN
 RAISE EXCEPTION 'undecided approval cannot impersonate a decision' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
