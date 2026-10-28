-- SPDX-License-Identifier: Elastic-2.0
CREATE TABLE tool_connections (
 organization_id text NOT NULL REFERENCES organizations(id),id text NOT NULL,version integer NOT NULL CHECK(version>0),
 profile jsonb NOT NULL,profile_digest text NOT NULL CHECK(profile_digest ~ '^[0-9a-f]{64}$'),active boolean NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),PRIMARY KEY(organization_id,id,version),
 UNIQUE(organization_id,id,version,profile_digest)
);
CREATE UNIQUE INDEX tool_one_active_version ON tool_connections(organization_id,id) WHERE active;
CREATE FUNCTION immutable_tool_connection() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.id IS DISTINCT FROM OLD.id OR NEW.version IS DISTINCT FROM OLD.version
 OR NEW.profile IS DISTINCT FROM OLD.profile OR NEW.profile_digest IS DISTINCT FROM OLD.profile_digest OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
 RAISE EXCEPTION 'tool connection version is immutable' USING ERRCODE='23514'; END IF; RETURN NEW;
END $$;
CREATE TRIGGER tool_connection_immutable BEFORE UPDATE ON tool_connections FOR EACH ROW EXECUTE FUNCTION immutable_tool_connection();
CREATE TRIGGER tool_connection_no_delete BEFORE DELETE ON tool_connections FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE TABLE tool_execution_counts (
 organization_id text NOT NULL,task_id text NOT NULL,own_executions integer NOT NULL DEFAULT 0 CHECK(own_executions BETWEEN 0 AND 32),tree_executions integer NOT NULL DEFAULT 0 CHECK(tree_executions BETWEEN 0 AND 128),
 PRIMARY KEY(organization_id,task_id),FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id)
);
CREATE TABLE tool_executions (
 organization_id text NOT NULL,id text NOT NULL,task_id text NOT NULL,run_id text NOT NULL,operation_id text NOT NULL,
 action_id text NOT NULL,reservation_id text NOT NULL,approval_id text NOT NULL,
 tool_id text NOT NULL,tool_version integer NOT NULL,tool_profile_digest text NOT NULL,
 candidate_id text NOT NULL,artifact_id text NOT NULL,artifact_version integer NOT NULL,content_digest text NOT NULL,
 intent_hash text NOT NULL CHECK(intent_hash ~ '^[0-9a-f]{64}$'),intent jsonb NOT NULL,
 created_by text NOT NULL,dispatch_requested_by text,status text NOT NULL CHECK(status IN('awaiting_approval','approved','queued','leased','sent','unknown','succeeded','rejected','denied','expired','cancelled')),
 version integer NOT NULL DEFAULT 1 CHECK(version>0),reason text NOT NULL DEFAULT '' CHECK(octet_length(reason)<=4096),result_receipt_id text,
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 5),lease_owner text,lease_until timestamptz,fencing_token bigint NOT NULL DEFAULT 0 CHECK(fencing_token>=0),
 dispatch_issued_at timestamptz,dispatch_not_after timestamptz,dispatch_fencing_token bigint,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),expires_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,id),UNIQUE(organization_id,id,intent_hash),UNIQUE(organization_id,id,task_id,action_id,intent_hash),
 UNIQUE(organization_id,action_id),UNIQUE(organization_id,reservation_id),UNIQUE(organization_id,approval_id),
 FOREIGN KEY(organization_id,operation_id,run_id,task_id) REFERENCES operations(organization_id,id,run_id,task_id),
 FOREIGN KEY(organization_id,candidate_id,task_id,run_id,operation_id) REFERENCES task_candidates(organization_id,id,task_id,run_id,operation_id),
 FOREIGN KEY(organization_id,candidate_id,task_id,run_id,artifact_id,artifact_version,content_digest) REFERENCES task_candidates(organization_id,id,task_id,run_id,artifact_id,artifact_version,content_digest),
 FOREIGN KEY(organization_id,action_id,operation_id,task_id) REFERENCES actions(organization_id,id,operation_id,task_id),
 FOREIGN KEY(organization_id,action_id,task_id,intent_hash) REFERENCES actions(organization_id,id,task_id,intent_hash),
 FOREIGN KEY(organization_id,reservation_id,operation_id,task_id) REFERENCES reservations(organization_id,id,operation_id,task_id),
 FOREIGN KEY(organization_id,tool_id,tool_version,tool_profile_digest) REFERENCES tool_connections(organization_id,id,version,profile_digest),
 FOREIGN KEY(organization_id,created_by) REFERENCES memberships(organization_id,principal_id),
 FOREIGN KEY(organization_id,dispatch_requested_by) REFERENCES memberships(organization_id,principal_id),
 CHECK(expires_at>created_at),CHECK((lease_owner IS NULL)=(lease_until IS NULL)),
 CHECK((dispatch_issued_at IS NULL AND dispatch_not_after IS NULL AND dispatch_fencing_token IS NULL)
 OR (dispatch_issued_at IS NOT NULL AND dispatch_not_after>dispatch_issued_at AND dispatch_fencing_token>0))
);
CREATE UNIQUE INDEX tool_occurrence_identity ON tool_executions(organization_id,task_id,(intent->>'occurrence_id'));
CREATE INDEX tool_execution_listing ON tool_executions(organization_id,task_id,created_at DESC,id DESC);
CREATE UNIQUE INDEX one_open_tool_execution ON tool_executions(organization_id,task_id) WHERE status IN('awaiting_approval','approved','queued','leased','sent','unknown');
ALTER TABLE approvals ADD COLUMN execution_id text;
ALTER TABLE approvals ADD COLUMN decided_at timestamptz;
ALTER TABLE approvals ADD COLUMN decision_method text;
ALTER TABLE approvals ADD COLUMN reason text NOT NULL DEFAULT '' CHECK(octet_length(reason)<=4096);
ALTER TABLE approvals ADD CONSTRAINT approval_execution_binding FOREIGN KEY(organization_id,execution_id,task_id,action_id,intent_hash) REFERENCES tool_executions(organization_id,id,task_id,action_id,intent_hash) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE approvals ADD CONSTRAINT approval_full_identity UNIQUE(organization_id,id,task_id,action_id,intent_hash,execution_id);
ALTER TABLE approvals ADD CONSTRAINT approval_current_decider FOREIGN KEY(organization_id,decided_by) REFERENCES memberships(organization_id,principal_id);
ALTER TABLE tool_executions ADD CONSTRAINT tool_approval_binding FOREIGN KEY(organization_id,approval_id,task_id,action_id,intent_hash,id) REFERENCES approvals(organization_id,id,task_id,action_id,intent_hash,execution_id) DEFERRABLE INITIALLY DEFERRED;
CREATE TABLE tool_outcome_receipts (
 organization_id text NOT NULL,id text NOT NULL,execution_id text NOT NULL,intent_hash text NOT NULL,
 outcome text NOT NULL CHECK(outcome IN('succeeded','rejected')),method text NOT NULL CHECK(method IN('registered_tool_response','operator_assertion')),
 effect_reference text NOT NULL CHECK(octet_length(effect_reference) BETWEEN 1 AND 256),artifact_id text NOT NULL,artifact_version integer NOT NULL DEFAULT 1 CHECK(artifact_version=1),content_digest text NOT NULL CHECK(content_digest ~ '^[0-9a-f]{64}$'),
 evidence_digest text NOT NULL CHECK(evidence_digest ~ '^[0-9a-f]{64}$'),principal_id text,reason text NOT NULL DEFAULT '' CHECK(octet_length(reason)<=4096),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),PRIMARY KEY(organization_id,id),UNIQUE(organization_id,execution_id),UNIQUE(organization_id,id,execution_id),
 FOREIGN KEY(organization_id,execution_id,intent_hash) REFERENCES tool_executions(organization_id,id,intent_hash),
 FOREIGN KEY(organization_id,artifact_id,artifact_version) REFERENCES artifacts(organization_id,id,version),
 FOREIGN KEY(organization_id,principal_id) REFERENCES memberships(organization_id,principal_id),
 CHECK((method='operator_assertion' AND principal_id IS NOT NULL AND reason<>'') OR (method='registered_tool_response' AND principal_id IS NULL))
);
ALTER TABLE tool_executions ADD CONSTRAINT tool_result_receipt_binding FOREIGN KEY(organization_id,result_receipt_id,id) REFERENCES tool_outcome_receipts(organization_id,id,execution_id) DEFERRABLE INITIALLY DEFERRED;
CREATE FUNCTION guard_tool_receipt_artifact() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM artifacts a JOIN tool_executions e ON e.organization_id=a.organization_id AND e.task_id=a.task_id WHERE e.organization_id=NEW.organization_id AND e.id=NEW.execution_id AND a.id=NEW.artifact_id AND a.version=1 AND encode(sha256(convert_to(a.content,'UTF8')),'hex')=NEW.content_digest) THEN
 RAISE EXCEPTION 'tool receipt requires exact same task artifact' USING ERRCODE='23514'; END IF; RETURN NEW;
END $$;
CREATE TRIGGER tool_receipt_artifact_guard BEFORE INSERT ON tool_outcome_receipts FOR EACH ROW EXECUTE FUNCTION guard_tool_receipt_artifact();
CREATE TRIGGER tool_receipt_immutable BEFORE UPDATE OR DELETE ON tool_outcome_receipts FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TABLE tool_outcome_conflicts (
 organization_id text NOT NULL,id text NOT NULL,execution_id text NOT NULL,receipt_id text NOT NULL,
 slot integer NOT NULL CHECK(slot BETWEEN 1 AND 8),payload_digest text NOT NULL CHECK(payload_digest ~ '^[0-9a-f]{64}$'),
 outcome text NOT NULL CHECK(outcome IN('succeeded','rejected')),effect_reference text NOT NULL CHECK(octet_length(effect_reference) BETWEEN 1 AND 256),
 artifact_id text NOT NULL,artifact_version integer NOT NULL DEFAULT 1 CHECK(artifact_version=1),content_digest text NOT NULL CHECK(content_digest ~ '^[0-9a-f]{64}$'),
 evidence_digest text NOT NULL CHECK(evidence_digest ~ '^[0-9a-f]{64}$'),created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,id),UNIQUE(organization_id,execution_id,payload_digest),UNIQUE(organization_id,execution_id,slot),
 FOREIGN KEY(organization_id,receipt_id,execution_id) REFERENCES tool_outcome_receipts(organization_id,id,execution_id),
 FOREIGN KEY(organization_id,artifact_id,artifact_version) REFERENCES artifacts(organization_id,id,version)
);
CREATE TRIGGER tool_conflict_artifact_guard BEFORE INSERT ON tool_outcome_conflicts FOR EACH ROW EXECUTE FUNCTION guard_tool_receipt_artifact();
CREATE TRIGGER tool_conflict_immutable BEFORE UPDATE OR DELETE ON tool_outcome_conflicts FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER tool_execution_no_delete BEFORE DELETE ON tool_executions FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER tool_count_no_delete BEFORE DELETE ON tool_execution_counts FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE FUNCTION guard_tool_execution() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE root text; current_task tasks; r reservations; bytes integer;
BEGIN
 IF TG_OP='UPDATE' THEN
  IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.id IS DISTINCT FROM OLD.id OR NEW.task_id IS DISTINCT FROM OLD.task_id
   OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.operation_id IS DISTINCT FROM OLD.operation_id OR NEW.action_id IS DISTINCT FROM OLD.action_id
   OR NEW.reservation_id IS DISTINCT FROM OLD.reservation_id OR NEW.approval_id IS DISTINCT FROM OLD.approval_id OR NEW.tool_id IS DISTINCT FROM OLD.tool_id
   OR NEW.tool_version IS DISTINCT FROM OLD.tool_version OR NEW.tool_profile_digest IS DISTINCT FROM OLD.tool_profile_digest OR NEW.candidate_id IS DISTINCT FROM OLD.candidate_id
   OR NEW.artifact_id IS DISTINCT FROM OLD.artifact_id OR NEW.artifact_version IS DISTINCT FROM OLD.artifact_version OR NEW.content_digest IS DISTINCT FROM OLD.content_digest
   OR NEW.intent_hash IS DISTINCT FROM OLD.intent_hash OR NEW.intent IS DISTINCT FROM OLD.intent OR NEW.created_by IS DISTINCT FROM OLD.created_by
   OR (OLD.dispatch_requested_by IS NOT NULL AND NEW.dispatch_requested_by IS DISTINCT FROM OLD.dispatch_requested_by)
   OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.expires_at IS DISTINCT FROM OLD.expires_at OR NEW.version<>OLD.version+1 OR NEW.fencing_token<OLD.fencing_token
   OR (OLD.dispatch_issued_at IS NOT NULL AND (NEW.dispatch_issued_at IS DISTINCT FROM OLD.dispatch_issued_at OR NEW.dispatch_not_after IS DISTINCT FROM OLD.dispatch_not_after OR NEW.dispatch_fencing_token IS DISTINCT FROM OLD.dispatch_fencing_token)) THEN
   RAISE EXCEPTION 'tool intent and dispatch identity are immutable' USING ERRCODE='23514'; END IF;
  IF NOT (CASE OLD.status
   WHEN 'awaiting_approval' THEN NEW.status IN('approved','denied','expired','cancelled')
   WHEN 'approved' THEN NEW.status IN('queued','expired','cancelled')
   WHEN 'queued' THEN NEW.status IN('leased','expired','cancelled','rejected')
   WHEN 'leased' THEN NEW.status IN('leased','sent','expired','cancelled','rejected')
   WHEN 'sent' THEN NEW.status IN('unknown','succeeded','rejected')
   WHEN 'unknown' THEN NEW.status IN('succeeded','rejected') ELSE false END) THEN
   RAISE EXCEPTION 'invalid tool job transition' USING ERRCODE='23514'; END IF;
  RETURN NEW;
 END IF;
 IF NEW.status<>'awaiting_approval' OR NEW.version<>1 OR NEW.fencing_token<>0 OR NEW.lease_owner IS NOT NULL OR NEW.dispatch_issued_at IS NOT NULL OR NEW.dispatch_requested_by IS NOT NULL OR NEW.result_receipt_id IS NOT NULL OR NEW.intent->>'organization_id' IS DISTINCT FROM NEW.organization_id::text OR NEW.intent->>'id' IS DISTINCT FROM NEW.id::text OR NEW.intent->>'task_id' IS DISTINCT FROM NEW.task_id::text OR NEW.intent->>'run_id' IS DISTINCT FROM NEW.run_id::text OR NEW.intent->>'operation_id' IS DISTINCT FROM NEW.operation_id::text OR NEW.intent->>'action_id' IS DISTINCT FROM NEW.action_id::text OR NEW.intent->>'reservation_id' IS DISTINCT FROM NEW.reservation_id::text OR NEW.intent->>'approval_id' IS DISTINCT FROM NEW.approval_id::text OR NEW.intent->>'tool_id' IS DISTINCT FROM NEW.tool_id::text OR NEW.intent->>'tool_version' IS DISTINCT FROM NEW.tool_version::text OR NEW.intent->>'tool_profile_digest' IS DISTINCT FROM NEW.tool_profile_digest::text OR NEW.intent->>'candidate_id' IS DISTINCT FROM NEW.candidate_id::text OR NEW.intent->>'artifact_id' IS DISTINCT FROM NEW.artifact_id::text OR NEW.intent->>'artifact_version' IS DISTINCT FROM NEW.artifact_version::text OR NEW.intent->>'content_digest' IS DISTINCT FROM NEW.content_digest::text OR NEW.intent->>'intent_hash' IS DISTINCT FROM NEW.intent_hash::text OR NEW.intent->>'created_by' IS DISTINCT FROM NEW.created_by::text THEN
 RAISE EXCEPTION 'tool intent snapshot does not match binding' USING ERRCODE='23514'; END IF;
 IF NEW.intent->>'occurrence_id' IS NULL OR NEW.intent->>'occurrence_id' !~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$' OR octet_length(NEW.intent->>'arguments_json')>32768 OR NEW.intent->>'arguments_digest' IS DISTINCT FROM encode(sha256(convert_to(NEW.intent->>'arguments_json','UTF8')),'hex') THEN
 RAISE EXCEPTION 'invalid tool occurrence or arguments snapshot' USING ERRCODE='23514'; END IF;
 SELECT * INTO current_task FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.task_id;
 IF NOT FOUND OR current_task.status<>'waiting_input' OR current_task.current_run_id IS DISTINCT FROM NEW.run_id OR current_task.candidate_id IS DISTINCT FROM NEW.candidate_id THEN
  RAISE EXCEPTION 'tool intent requires current waiting candidate' USING ERRCODE='23514'; END IF;
 SELECT * INTO r FROM reservations WHERE organization_id=NEW.organization_id AND id=NEW.reservation_id;
 IF NOT FOUND OR r.action_id<>NEW.action_id OR r.category<>'tool' OR r.amount_microusd IS DISTINCT FROM (NEW.intent->>'max_charge_microusd')::bigint OR r.provider IS DISTINCT FROM NEW.intent->>'provider' OR r.price_version IS DISTINCT FROM NEW.intent->>'price_version' THEN RAISE EXCEPTION 'tool reservation binding mismatch' USING ERRCODE='23514'; END IF;
 root=current_task.root_id;
 INSERT INTO tool_execution_counts(organization_id,task_id) VALUES(NEW.organization_id,NEW.task_id) ON CONFLICT DO NOTHING;
 INSERT INTO tool_execution_counts(organization_id,task_id) VALUES(NEW.organization_id,root) ON CONFLICT DO NOTHING;
 UPDATE tool_execution_counts SET own_executions=own_executions+1 WHERE organization_id=NEW.organization_id AND task_id=NEW.task_id AND own_executions<32;
 IF NOT FOUND THEN RAISE EXCEPTION 'task tool lifetime limit' USING ERRCODE='23514'; END IF;
 UPDATE tool_execution_counts SET tree_executions=tree_executions+1 WHERE organization_id=NEW.organization_id AND task_id=root AND tree_executions<128;
 IF NOT FOUND THEN RAISE EXCEPTION 'root tool lifetime limit' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER tool_execution_guard BEFORE INSERT OR UPDATE ON tool_executions FOR EACH ROW EXECUTE FUNCTION guard_tool_execution();
CREATE FUNCTION guard_tool_count() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.task_id IS DISTINCT FROM OLD.task_id OR NEW.own_executions<OLD.own_executions OR NEW.tree_executions<OLD.tree_executions OR NEW.own_executions>OLD.own_executions+1 OR NEW.tree_executions>OLD.tree_executions+1 OR (NEW.own_executions=OLD.own_executions AND NEW.tree_executions=OLD.tree_executions) THEN
 RAISE EXCEPTION 'tool count cannot reset' USING ERRCODE='23514'; END IF; RETURN NEW; END $$;
CREATE TRIGGER tool_count_guard BEFORE UPDATE ON tool_execution_counts FOR EACH ROW EXECUTE FUNCTION guard_tool_count();
CREATE FUNCTION guard_tool_approval() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.execution_id IS NOT NULL AND (NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.id IS DISTINCT FROM OLD.id OR NEW.task_id IS DISTINCT FROM OLD.task_id
 OR NEW.action_id IS DISTINCT FROM OLD.action_id OR NEW.intent_hash IS DISTINCT FROM OLD.intent_hash OR NEW.execution_id IS DISTINCT FROM OLD.execution_id
 OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.expires_at IS DISTINCT FROM OLD.expires_at OR OLD.status<>'pending' OR NEW.status NOT IN('approved','denied','expired') OR NEW.version<>OLD.version+1) THEN
 RAISE EXCEPTION 'tool approval is immutable or already decided' USING ERRCODE='23514'; END IF; RETURN NEW;
END $$;
CREATE FUNCTION guard_tool_approval_shape() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.execution_id IS NOT NULL THEN
  IF NOT EXISTS(SELECT 1 FROM tool_executions e WHERE e.organization_id=NEW.organization_id AND e.id=NEW.execution_id AND e.task_id=NEW.task_id AND e.action_id=NEW.action_id AND e.intent_hash=NEW.intent_hash AND e.expires_at=NEW.expires_at) THEN
   RAISE EXCEPTION 'tool approval requires exact immutable expiry and binding' USING ERRCODE='23514'; END IF;
  IF TG_OP='INSERT' AND (NEW.status<>'pending' OR NEW.version<>1 OR NEW.decided_by IS NOT NULL OR NEW.decided_at IS NOT NULL OR NEW.decision_method IS NOT NULL) THEN
   RAISE EXCEPTION 'tool approval starts undecided' USING ERRCODE='23514'; END IF;
  IF NEW.status IN('approved','denied') AND (NEW.decided_by IS NULL OR NEW.decided_at IS NULL OR NEW.decision_method IS DISTINCT FROM 'authorized_principal_decision' OR NEW.reason='') THEN
   RAISE EXCEPTION 'tool decision requires attributed receipt' USING ERRCODE='23514'; END IF;
 END IF; RETURN NEW;
END $$;
CREATE TRIGGER tool_approval_shape_guard BEFORE INSERT OR UPDATE ON approvals FOR EACH ROW EXECUTE FUNCTION guard_tool_approval_shape();
CREATE TRIGGER tool_approval_guard BEFORE UPDATE ON approvals FOR EACH ROW EXECUTE FUNCTION guard_tool_approval();
CREATE TRIGGER tool_approval_no_delete BEFORE DELETE ON approvals FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
