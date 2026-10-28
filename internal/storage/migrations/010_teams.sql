-- SPDX-License-Identifier: Elastic-2.0
-- Root admission compiles/pins the full
-- bounded tree and materializes its Task rows atomically. Dependency-waiting
-- children have no executable operation until their immutable RunInput is ready.
-- Task-tree lifetime controls/current-run/input/candidate tables belong to 009.

CREATE TABLE team_executions (
 organization_id text NOT NULL,
 task_id text NOT NULL,
 run_id text NOT NULL,
 root_id text NOT NULL,
 root_run_id text NOT NULL,
 plan jsonb NOT NULL CHECK(jsonb_typeof(plan)='object'),
 plan_digest text NOT NULL CHECK(plan_digest ~ '^[a-f0-9]{64}$'),
 status text NOT NULL DEFAULT 'running' CHECK(status IN('running','candidate','blocked','cancelled')),
 version integer NOT NULL DEFAULT 1 CHECK(version>0),
 next_check_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,run_id),
 UNIQUE(organization_id,run_id,task_id),
 FOREIGN KEY(organization_id,run_id,task_id) REFERENCES runs(organization_id,id,task_id),
 FOREIGN KEY(organization_id,root_id) REFERENCES tasks(organization_id,id),
 FOREIGN KEY(organization_id,root_run_id,root_id) REFERENCES runs(organization_id,id,task_id)
);
CREATE INDEX team_execution_ready ON team_executions(next_check_at,organization_id,run_id) WHERE status='running';

CREATE TABLE team_members (
 organization_id text NOT NULL,
 parent_run_id text NOT NULL,
 member_id text NOT NULL CHECK(octet_length(member_id) BETWEEN 1 AND 128),
 ordinal integer NOT NULL CHECK(ordinal BETWEEN 0 AND 31),
 child_task_id text NOT NULL,
 child_run_id text,
 -- A closed subtree with all published agent and model profile pins. Nested
-- execution may only activate this snapshot, never resolve latest profiles.
 plan jsonb NOT NULL CHECK(jsonb_typeof(plan)='object'),
 plan_digest text NOT NULL CHECK(plan_digest ~ '^[a-f0-9]{64}$'),
 status text NOT NULL DEFAULT 'planned' CHECK(status IN('planned','running','candidate','failed','cancelled','skipped')),
 selected_candidate_id text,
 selected_artifact_id text,
 selected_artifact_version integer,
 selected_content_digest text,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,parent_run_id,member_id),
 UNIQUE(organization_id,parent_run_id,ordinal),
 UNIQUE(organization_id,child_task_id),
 FOREIGN KEY(organization_id,parent_run_id) REFERENCES team_executions(organization_id,run_id),
 FOREIGN KEY(organization_id,child_task_id) REFERENCES tasks(organization_id,id),
 FOREIGN KEY(organization_id,child_run_id,child_task_id) REFERENCES runs(organization_id,id,task_id),
 FOREIGN KEY(organization_id,selected_candidate_id,child_task_id,child_run_id,selected_artifact_id,selected_artifact_version,selected_content_digest) REFERENCES task_candidates(organization_id,id,task_id,run_id,artifact_id,artifact_version,content_digest),
 FOREIGN KEY(organization_id,selected_artifact_id,child_task_id) REFERENCES artifacts(organization_id,id,task_id),
 FOREIGN KEY(organization_id,selected_artifact_id,selected_artifact_version) REFERENCES artifacts(organization_id,id,version),
 CHECK((selected_candidate_id IS NULL)=(selected_artifact_id IS NULL)),
 CHECK(selected_candidate_id IS NULL OR child_run_id IS NOT NULL),
 CHECK((selected_artifact_id IS NULL)=(selected_artifact_version IS NULL)),
 CHECK((selected_artifact_id IS NULL)=(selected_content_digest IS NULL)),
 CHECK(selected_content_digest IS NULL OR selected_content_digest ~ '^[a-f0-9]{64}$')
);

CREATE FUNCTION immutable_team_execution_plan() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.task_id IS DISTINCT FROM OLD.task_id
 OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.root_id IS DISTINCT FROM OLD.root_id
 OR NEW.root_run_id IS DISTINCT FROM OLD.root_run_id OR NEW.plan IS DISTINCT FROM OLD.plan
 OR NEW.plan_digest IS DISTINCT FROM OLD.plan_digest OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
  RAISE EXCEPTION 'team execution plan is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER team_execution_plan_immutable BEFORE UPDATE ON team_executions FOR EACH ROW EXECUTE FUNCTION immutable_team_execution_plan();
CREATE TRIGGER team_execution_no_delete BEFORE DELETE ON team_executions FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE FUNCTION immutable_team_member_binding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.parent_run_id IS DISTINCT FROM OLD.parent_run_id
 OR NEW.member_id IS DISTINCT FROM OLD.member_id OR NEW.ordinal IS DISTINCT FROM OLD.ordinal
 OR NEW.child_task_id IS DISTINCT FROM OLD.child_task_id OR NEW.plan IS DISTINCT FROM OLD.plan
 OR NEW.plan_digest IS DISTINCT FROM OLD.plan_digest OR NEW.created_at IS DISTINCT FROM OLD.created_at
 OR (OLD.child_run_id IS NOT NULL AND NEW.child_run_id IS DISTINCT FROM OLD.child_run_id)
 OR (OLD.selected_artifact_id IS NOT NULL AND (
    NEW.selected_candidate_id IS DISTINCT FROM OLD.selected_candidate_id
    OR NEW.selected_artifact_id IS DISTINCT FROM OLD.selected_artifact_id
    OR NEW.selected_artifact_version IS DISTINCT FROM OLD.selected_artifact_version
    OR NEW.selected_content_digest IS DISTINCT FROM OLD.selected_content_digest)) THEN
  RAISE EXCEPTION 'team member binding is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER team_member_binding_immutable BEFORE UPDATE ON team_members FOR EACH ROW EXECUTE FUNCTION immutable_team_member_binding();
CREATE TRIGGER team_member_no_delete BEFORE DELETE ON team_members FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE FUNCTION enforce_team_lineage() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE actual_root text; actual_parent text; expected_parent text; expected_root text;
BEGIN
 IF TG_TABLE_NAME='team_executions' THEN
  SELECT root_id INTO actual_root FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.task_id;
  IF actual_root IS NULL OR actual_root<>NEW.root_id THEN
   RAISE EXCEPTION 'team execution root does not match task' USING ERRCODE='23514';
  END IF;
 ELSE
  SELECT parent_id,root_id INTO actual_parent,actual_root FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.child_task_id;
  SELECT task_id,root_id INTO expected_parent,expected_root FROM team_executions WHERE organization_id=NEW.organization_id AND run_id=NEW.parent_run_id;
  IF actual_parent IS NULL OR expected_parent IS NULL OR actual_parent<>expected_parent OR actual_root<>expected_root THEN
   RAISE EXCEPTION 'team member is outside parent task lineage' USING ERRCODE='23514';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER team_execution_lineage BEFORE INSERT ON team_executions FOR EACH ROW EXECUTE FUNCTION enforce_team_lineage();
CREATE TRIGGER team_member_lineage BEFORE INSERT ON team_members FOR EACH ROW EXECUTE FUNCTION enforce_team_lineage();
