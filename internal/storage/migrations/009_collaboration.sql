-- SPDX-License-Identifier: Elastic-2.0
ALTER TABLE agents ADD COLUMN team jsonb;
ALTER TABLE tasks ADD COLUMN current_run_id text;
ALTER TABLE tasks ADD COLUMN attempt_count integer NOT NULL DEFAULT 0 CHECK(attempt_count>=0);
ALTER TABLE tasks ADD COLUMN max_runs integer NOT NULL DEFAULT 3 CHECK(max_runs BETWEEN 1 AND 10);
ALTER TABLE tasks ADD COLUMN execution_deadline timestamptz NOT NULL DEFAULT clock_timestamp()+interval '24 hours';
ALTER TABLE tasks ADD COLUMN tree_depth_limit integer NOT NULL DEFAULT 16 CHECK(tree_depth_limit BETWEEN 1 AND 16);
ALTER TABLE tasks ADD COLUMN candidate_id text;
ALTER TABLE tasks ADD COLUMN accepted_decision_id text;
-- Legacy descendants inherit one root clock; new descendants cannot extend it.
UPDATE tasks c SET execution_deadline=r.execution_deadline FROM tasks r WHERE c.organization_id=r.organization_id AND c.root_id=r.id AND c.parent_id IS NOT NULL;
CREATE FUNCTION inherit_execution_deadline() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE parent_deadline timestamptz;
BEGIN
 IF NEW.parent_id IS NOT NULL THEN
  SELECT execution_deadline INTO parent_deadline FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.parent_id;
  IF NOT FOUND THEN RAISE EXCEPTION 'missing execution parent' USING ERRCODE='23514'; END IF;
  NEW.execution_deadline=LEAST(NEW.execution_deadline,parent_deadline);
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER zz_execution_deadline BEFORE INSERT ON tasks FOR EACH ROW EXECUTE FUNCTION inherit_execution_deadline();
ALTER TABLE runs ADD COLUMN attempt integer NOT NULL DEFAULT 1 CHECK(attempt BETWEEN 1 AND 10);
ALTER TABLE runs ADD COLUMN input_context text NOT NULL DEFAULT '' CHECK(octet_length(input_context)<=900000);
ALTER TABLE runs ADD COLUMN input_digest text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN feedback text NOT NULL DEFAULT '' CHECK(octet_length(feedback)<=8192);
ALTER TABLE runs ADD COLUMN source_candidate_id text;
WITH numbered AS (SELECT organization_id,id,row_number() OVER(PARTITION BY organization_id,task_id ORDER BY created_at,id)::integer AS n FROM runs)
UPDATE runs r SET attempt=n.n FROM numbered n WHERE r.organization_id=n.organization_id AND r.id=n.id;
UPDATE tasks t SET attempt_count=r.n,max_runs=GREATEST(3,r.n),current_run_id=r.id
FROM (SELECT DISTINCT ON(organization_id,task_id) organization_id,task_id,id,attempt AS n FROM runs ORDER BY organization_id,task_id,attempt DESC) r
WHERE t.organization_id=r.organization_id AND t.id=r.task_id;
ALTER TABLE runs ADD CONSTRAINT run_attempt_identity UNIQUE(organization_id,task_id,attempt);
ALTER TABLE runs ADD CONSTRAINT run_current_binding UNIQUE(organization_id,id,task_id,attempt);
ALTER TABLE tasks ADD CONSTRAINT current_run_binding FOREIGN KEY(organization_id,current_run_id,id,attempt_count) REFERENCES runs(organization_id,id,task_id,attempt) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE runs ADD CONSTRAINT run_input_digest_shape CHECK((input_context='' AND input_digest='') OR (input_context<>'' AND input_digest ~ '^[0-9a-f]{64}$'));

CREATE TABLE task_tree_controls (
 organization_id text NOT NULL,root_id text NOT NULL,
 max_tasks integer NOT NULL DEFAULT 128 CHECK(max_tasks BETWEEN 1 AND 128),
 used_tasks integer NOT NULL DEFAULT 0 CHECK(used_tasks>=0),
 max_total_attempts integer NOT NULL DEFAULT 384 CHECK(max_total_attempts BETWEEN 1 AND 384),
 used_attempts integer NOT NULL DEFAULT 0 CHECK(used_attempts>=0),
 max_depth integer NOT NULL DEFAULT 16 CHECK(max_depth BETWEEN 1 AND 16),
 PRIMARY KEY(organization_id,root_id),
 FOREIGN KEY(organization_id,root_id) REFERENCES tasks(organization_id,id),
 CHECK(used_tasks<=max_tasks),CHECK(used_attempts<=max_total_attempts)
);
INSERT INTO task_tree_controls(organization_id,root_id,used_tasks,used_attempts)
SELECT t.organization_id,t.id,(SELECT count(*) FROM tasks c WHERE c.organization_id=t.organization_id AND c.root_id=t.id),
(SELECT count(*) FROM runs r JOIN tasks c ON c.organization_id=r.organization_id AND c.id=r.task_id WHERE c.organization_id=t.organization_id AND c.root_id=t.id)
FROM tasks t WHERE t.parent_id IS NULL;
ALTER TABLE tasks ADD CONSTRAINT task_tree_control_binding FOREIGN KEY(organization_id,root_id) REFERENCES task_tree_controls(organization_id,root_id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE artifacts ADD CONSTRAINT artifact_version_binding UNIQUE(organization_id,id,version);
ALTER TABLE operations ADD CONSTRAINT operation_full_run_binding UNIQUE(organization_id,id,run_id,task_id);

CREATE TABLE task_candidates (
 organization_id text NOT NULL,id text NOT NULL,task_id text NOT NULL,run_id text NOT NULL,operation_id text NOT NULL,
 artifact_id text NOT NULL,artifact_version integer NOT NULL CHECK(artifact_version>0),content_digest text NOT NULL CHECK(content_digest ~ '^[0-9a-f]{64}$'),
 source text NOT NULL CHECK(source IN('model','team','fixture')),created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,id),UNIQUE(organization_id,run_id),UNIQUE(organization_id,id,task_id,run_id),
 UNIQUE(organization_id,id,task_id,run_id,artifact_id,artifact_version,content_digest),
 FOREIGN KEY(organization_id,run_id,task_id) REFERENCES runs(organization_id,id,task_id),
 FOREIGN KEY(organization_id,operation_id,run_id,task_id) REFERENCES operations(organization_id,id,run_id,task_id),
 FOREIGN KEY(organization_id,artifact_id,task_id) REFERENCES artifacts(organization_id,id,task_id),
 FOREIGN KEY(organization_id,artifact_id,artifact_version) REFERENCES artifacts(organization_id,id,version)
);
INSERT INTO task_candidates(organization_id,id,task_id,run_id,operation_id,artifact_id,artifact_version,content_digest,source,created_at)
SELECT m.organization_id,'cand_'||m.operation_id,m.task_id,o.run_id,m.operation_id,m.artifact_id,a.version,m.content_digest,'model',m.created_at
FROM model_candidates m JOIN operations o ON o.organization_id=m.organization_id AND o.id=m.operation_id
JOIN artifacts a ON a.organization_id=m.organization_id AND a.id=m.artifact_id;
UPDATE tasks t SET candidate_id=c.id FROM task_candidates c WHERE c.organization_id=t.organization_id AND c.task_id=t.id AND c.run_id=t.current_run_id;
CREATE TABLE task_decisions (
 organization_id text NOT NULL,id text NOT NULL,task_id text NOT NULL,run_id text NOT NULL,candidate_id text NOT NULL,
 artifact_id text NOT NULL,artifact_version integer NOT NULL CHECK(artifact_version>0),content_digest text NOT NULL,
 decision text NOT NULL CHECK(decision IN('accept','reject','request_changes')),reason text NOT NULL DEFAULT '',principal_id text NOT NULL,
 method text NOT NULL CHECK(method IN('authorized_user_decision','inherited_authorized_user_decision')),
 task_version_before integer NOT NULL,task_version_after integer NOT NULL CHECK(task_version_after=task_version_before+1),
 parent_decision_id text,created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,id),UNIQUE(organization_id,candidate_id),UNIQUE(organization_id,id,task_id,run_id),
 FOREIGN KEY(organization_id,candidate_id,task_id,run_id,artifact_id,artifact_version,content_digest) REFERENCES task_candidates(organization_id,id,task_id,run_id,artifact_id,artifact_version,content_digest),
 FOREIGN KEY(organization_id,artifact_id,task_id) REFERENCES artifacts(organization_id,id,task_id),
 FOREIGN KEY(organization_id,principal_id) REFERENCES memberships(organization_id,principal_id),
 FOREIGN KEY(organization_id,parent_decision_id) REFERENCES task_decisions(organization_id,id) DEFERRABLE INITIALLY DEFERRED,
 CHECK((method='inherited_authorized_user_decision')=(parent_decision_id IS NOT NULL))
);
ALTER TABLE tasks ADD CONSTRAINT current_candidate_binding FOREIGN KEY(organization_id,candidate_id,id,current_run_id) REFERENCES task_candidates(organization_id,id,task_id,run_id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE tasks ADD CONSTRAINT accepted_decision_binding FOREIGN KEY(organization_id,accepted_decision_id,id,current_run_id) REFERENCES task_decisions(organization_id,id,task_id,run_id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE runs ADD CONSTRAINT run_source_candidate FOREIGN KEY(organization_id,source_candidate_id) REFERENCES task_candidates(organization_id,id);
CREATE TRIGGER task_candidate_immutable BEFORE UPDATE OR DELETE ON task_candidates FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER task_decision_immutable BEFORE UPDATE OR DELETE ON task_decisions FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE FUNCTION protect_collaboration_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_TABLE_NAME='runs' THEN
  IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.id IS DISTINCT FROM OLD.id OR NEW.task_id IS DISTINCT FROM OLD.task_id
   OR NEW.principal_id IS DISTINCT FROM OLD.principal_id OR NEW.agent_id IS DISTINCT FROM OLD.agent_id OR NEW.agent_version IS DISTINCT FROM OLD.agent_version
   OR NEW.attempt IS DISTINCT FROM OLD.attempt OR NEW.input_context IS DISTINCT FROM OLD.input_context OR NEW.input_digest IS DISTINCT FROM OLD.input_digest
   OR NEW.feedback IS DISTINCT FROM OLD.feedback OR NEW.source_candidate_id IS DISTINCT FROM OLD.source_candidate_id OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
   RAISE EXCEPTION 'run input and identity are immutable' USING ERRCODE='23514'; END IF;
 ELSIF TG_TABLE_NAME='agents' THEN
  IF OLD.published AND NEW.team IS DISTINCT FROM OLD.team THEN RAISE EXCEPTION 'published team is immutable' USING ERRCODE='23514'; END IF;
 ELSIF TG_TABLE_NAME='tasks' THEN
  IF NEW.max_runs IS DISTINCT FROM OLD.max_runs OR NEW.execution_deadline IS DISTINCT FROM OLD.execution_deadline OR NEW.tree_depth_limit IS DISTINCT FROM OLD.tree_depth_limit THEN
   RAISE EXCEPTION 'task lifetime limits are immutable' USING ERRCODE='23514'; END IF;
 ELSIF TG_TABLE_NAME='task_tree_controls' THEN
  IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.root_id IS DISTINCT FROM OLD.root_id OR NEW.max_tasks IS DISTINCT FROM OLD.max_tasks
   OR NEW.max_total_attempts IS DISTINCT FROM OLD.max_total_attempts OR NEW.max_depth IS DISTINCT FROM OLD.max_depth
   OR NEW.used_tasks<OLD.used_tasks OR NEW.used_attempts<OLD.used_attempts THEN
   RAISE EXCEPTION 'tree identity/limits and lifetime counters cannot reset' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER run_snapshot_immutable BEFORE UPDATE ON runs FOR EACH ROW EXECUTE FUNCTION protect_collaboration_snapshot();
CREATE TRIGGER agent_team_immutable BEFORE UPDATE ON agents FOR EACH ROW EXECUTE FUNCTION protect_collaboration_snapshot();
CREATE TRIGGER task_lifetime_immutable BEFORE UPDATE ON tasks FOR EACH ROW EXECUTE FUNCTION protect_collaboration_snapshot();
CREATE TRIGGER tree_control_immutable BEFORE UPDATE ON task_tree_controls FOR EACH ROW EXECUTE FUNCTION protect_collaboration_snapshot();
CREATE TRIGGER tree_control_no_delete BEFORE DELETE ON task_tree_controls FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE FUNCTION count_tree_task() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.parent_id IS NULL THEN
  INSERT INTO task_tree_controls(organization_id,root_id,used_tasks,max_depth) VALUES(NEW.organization_id,NEW.id,1,NEW.tree_depth_limit);
 ELSE
  UPDATE task_tree_controls SET used_tasks=used_tasks+1 WHERE organization_id=NEW.organization_id AND root_id=NEW.root_id AND used_tasks<max_tasks AND NEW.depth<=max_depth;
  IF NOT FOUND THEN RAISE EXCEPTION 'tree task/depth limit exceeded' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER zz_count_tree_task AFTER INSERT ON tasks FOR EACH ROW EXECUTE FUNCTION count_tree_task();
CREATE FUNCTION count_tree_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE v_task tasks;
BEGIN
 SELECT * INTO v_task FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.task_id;
 IF NOT FOUND OR v_task.status IN('succeeded','failed','cancelled') OR NEW.attempt<>v_task.attempt_count+1 OR NEW.attempt>v_task.max_runs THEN
  RAISE EXCEPTION 'task attempt limit or ordinal mismatch' USING ERRCODE='23514'; END IF;
 IF v_task.execution_deadline<=clock_timestamp() THEN
  RAISE EXCEPTION 'task execution deadline expired' USING ERRCODE='23514'; END IF;
 UPDATE task_tree_controls SET used_attempts=used_attempts+1 WHERE organization_id=NEW.organization_id AND root_id=v_task.root_id AND used_attempts<max_total_attempts;
 IF NOT FOUND THEN RAISE EXCEPTION 'tree attempt limit exceeded' USING ERRCODE='23514'; END IF;
 UPDATE tasks SET current_run_id=NEW.id,attempt_count=NEW.attempt,candidate_id=NULL,accepted_decision_id=NULL WHERE organization_id=NEW.organization_id AND id=NEW.task_id;
 RETURN NEW;
END $$;
CREATE TRIGGER zz_count_tree_attempt AFTER INSERT ON runs FOR EACH ROW EXECUTE FUNCTION count_tree_attempt();
CREATE FUNCTION protect_candidate_artifact() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM task_candidates WHERE organization_id=OLD.organization_id AND artifact_id=OLD.id) THEN
  RAISE EXCEPTION 'candidate artifact is immutable; create a new revision' USING ERRCODE='23514'; END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF; RETURN NEW;
END $$;
CREATE TRIGGER candidate_artifact_immutable BEFORE UPDATE OR DELETE ON artifacts FOR EACH ROW EXECUTE FUNCTION protect_candidate_artifact();
