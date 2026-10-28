-- SPDX-License-Identifier: Elastic-2.0
-- Budgets constrain new admission, not the recording of an authentic bill.
ALTER TABLE organizations DROP CONSTRAINT organizations_check;
ALTER TABLE tasks DROP CONSTRAINT tasks_check;
ALTER TABLE reservations DROP CONSTRAINT reservations_check;
ALTER TABLE organizations ADD COLUMN financial_hold boolean NOT NULL DEFAULT false;
ALTER TABLE tasks ADD COLUMN financial_hold boolean NOT NULL DEFAULT false;
ALTER TABLE tasks ADD COLUMN root_id text;
ALTER TABLE tasks ADD COLUMN parent_id text;
ALTER TABLE tasks ADD COLUMN depth integer NOT NULL DEFAULT 0 CHECK(depth BETWEEN 0 AND 16);
UPDATE tasks SET root_id=id;
ALTER TABLE tasks ALTER COLUMN root_id SET NOT NULL;
ALTER TABLE tasks ADD CONSTRAINT task_root_scope FOREIGN KEY(organization_id,root_id) REFERENCES tasks(organization_id,id);
ALTER TABLE tasks ADD CONSTRAINT task_parent_scope FOREIGN KEY(organization_id,parent_id) REFERENCES tasks(organization_id,id);
CREATE INDEX tasks_tree ON tasks(organization_id,root_id,depth,id);

CREATE FUNCTION enforce_task_lineage() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE ancestor_root text; ancestor_depth integer; ancestor_owner text;
BEGIN
 IF TG_OP='UPDATE' THEN
  IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.id IS DISTINCT FROM OLD.id
     OR NEW.root_id IS DISTINCT FROM OLD.root_id OR NEW.parent_id IS DISTINCT FROM OLD.parent_id OR NEW.depth IS DISTINCT FROM OLD.depth
     OR NEW.owner_id IS DISTINCT FROM OLD.owner_id OR NEW.agent_id IS DISTINCT FROM OLD.agent_id OR NEW.agent_version IS DISTINCT FROM OLD.agent_version THEN
   RAISE EXCEPTION 'task lineage is immutable' USING ERRCODE='23514';
  END IF;
 ELSE
  IF NEW.parent_id IS NULL THEN
   IF NEW.root_id IS NOT NULL AND NEW.root_id<>NEW.id THEN RAISE EXCEPTION 'invalid root' USING ERRCODE='23514'; END IF;
   NEW.root_id:=NEW.id; NEW.depth:=0;
  ELSE
   SELECT root_id,depth,owner_id INTO ancestor_root,ancestor_depth,ancestor_owner FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.parent_id;
   IF NOT FOUND THEN RAISE EXCEPTION 'parent not found' USING ERRCODE='23503'; END IF;
   IF NEW.owner_id<>ancestor_owner THEN RAISE EXCEPTION 'child authority must inherit owner' USING ERRCODE='23514'; END IF;
   IF NEW.root_id IS NOT NULL AND NEW.root_id<>ancestor_root THEN RAISE EXCEPTION 'invalid root' USING ERRCODE='23514'; END IF;
   NEW.root_id:=ancestor_root; NEW.depth:=ancestor_depth+1;
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER task_lineage BEFORE INSERT OR UPDATE ON tasks FOR EACH ROW EXECUTE FUNCTION enforce_task_lineage();

ALTER TABLE reservations DROP CONSTRAINT reservations_organization_id_operation_id_key;
ALTER TABLE reservations ADD COLUMN charge_key text;
ALTER TABLE reservations ADD COLUMN category text NOT NULL DEFAULT 'fixture_legacy' CHECK(category IN('fixture_legacy','model','tool','compute','verification','recovery'));
ALTER TABLE reservations ADD COLUMN provider text NOT NULL DEFAULT 'fixture';
ALTER TABLE reservations ADD COLUMN price_version text NOT NULL DEFAULT 'fixture:1';
ALTER TABLE reservations ADD COLUMN action_id text;
ALTER TABLE reservations ADD COLUMN root_id text;
ALTER TABLE reservations ADD COLUMN scope_task_ids text[];
ALTER TABLE reservations ADD COLUMN revision integer NOT NULL DEFAULT 1 CHECK(revision>0);
UPDATE reservations SET charge_key=id,root_id=task_id,scope_task_ids=ARRAY[task_id];
ALTER TABLE reservations ADD CONSTRAINT charge_key_unique UNIQUE(organization_id,charge_key);
CREATE UNIQUE INDEX fixture_one_logical_charge ON reservations(organization_id,operation_id) WHERE category='fixture_legacy';
ALTER TABLE reservations ADD CONSTRAINT reservation_action_scope FOREIGN KEY(organization_id,action_id) REFERENCES actions(organization_id,id);
ALTER TABLE reservations ADD CONSTRAINT reservation_root_scope FOREIGN KEY(organization_id,root_id) REFERENCES tasks(organization_id,id);
CREATE INDEX reservation_task ON reservations(organization_id,task_id,created_at,id);
CREATE INDEX reservation_scope_lookup ON reservations USING gin(scope_task_ids);

CREATE TABLE ledger_entries (
 organization_id text NOT NULL, id text NOT NULL, reservation_id text NOT NULL, task_id text NOT NULL, root_id text NOT NULL,
 event_key text NOT NULL, event_hash text NOT NULL, kind text NOT NULL,
 delta_reserved_microusd bigint NOT NULL, delta_spent_microusd bigint NOT NULL,
 synthetic boolean NOT NULL DEFAULT false, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(organization_id,id), UNIQUE(organization_id,reservation_id,event_key),
 FOREIGN KEY(organization_id,reservation_id) REFERENCES reservations(organization_id,id),
 FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id), FOREIGN KEY(organization_id,root_id) REFERENCES tasks(organization_id,id)
);
CREATE INDEX ledger_root ON ledger_entries(organization_id,root_id,created_at,id);
INSERT INTO ledger_entries(organization_id,id,reservation_id,task_id,root_id,event_key,event_hash,kind,delta_reserved_microusd,delta_spent_microusd,synthetic)
 SELECT organization_id,'legacy_'||id,id,task_id,root_id,'legacy.import','migration003','legacy_import',
 CASE WHEN status IN('reserved','unknown') THEN amount_microusd ELSE 0 END,COALESCE(actual_microusd,0),true FROM reservations;

CREATE TABLE cost_evidence (
 organization_id text NOT NULL, source text NOT NULL, reference text NOT NULL, reservation_id text NOT NULL,
 payload_digest text NOT NULL, actual_microusd bigint NOT NULL CHECK(actual_microusd>=0),
 reason text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(organization_id,source,reference), FOREIGN KEY(organization_id,reservation_id) REFERENCES reservations(organization_id,id)
);
CREATE TABLE financial_incidents (
 organization_id text NOT NULL, id text NOT NULL, reservation_id text NOT NULL, task_id text NOT NULL,
 kind text NOT NULL, amount_microusd bigint NOT NULL CHECK(amount_microusd>=0), created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(organization_id,id), FOREIGN KEY(organization_id,reservation_id) REFERENCES reservations(organization_id,id),
 FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id)
);
CREATE TABLE approvals (
 organization_id text NOT NULL, id text NOT NULL, task_id text NOT NULL, action_id text NOT NULL, intent_hash text NOT NULL,
 status text NOT NULL CHECK(status IN('pending','approved','denied','expired')), version integer NOT NULL DEFAULT 1,
 decided_by text REFERENCES principals(id), created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,id), FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id),
 FOREIGN KEY(organization_id,action_id) REFERENCES actions(organization_id,id)
);

ALTER TABLE runs ADD CONSTRAINT run_task_unique UNIQUE(organization_id,id,task_id);
ALTER TABLE operations ADD CONSTRAINT operation_task_unique UNIQUE(organization_id,id,task_id);
ALTER TABLE operations ADD CONSTRAINT operation_run_task FOREIGN KEY(organization_id,run_id,task_id) REFERENCES runs(organization_id,id,task_id);
ALTER TABLE actions ADD CONSTRAINT action_binding_unique UNIQUE(organization_id,id,operation_id,task_id);
ALTER TABLE actions ADD CONSTRAINT action_intent_unique UNIQUE(organization_id,id,task_id,intent_hash);
ALTER TABLE actions ADD CONSTRAINT action_operation_task FOREIGN KEY(organization_id,operation_id,task_id) REFERENCES operations(organization_id,id,task_id);
ALTER TABLE reservations ADD CONSTRAINT reservation_operation_task FOREIGN KEY(organization_id,operation_id,task_id) REFERENCES operations(organization_id,id,task_id);
ALTER TABLE reservations ADD CONSTRAINT reservation_action_binding FOREIGN KEY(organization_id,action_id,operation_id,task_id) REFERENCES actions(organization_id,id,operation_id,task_id);
ALTER TABLE approvals ADD CONSTRAINT approval_intent_binding FOREIGN KEY(organization_id,action_id,task_id,intent_hash) REFERENCES actions(organization_id,id,task_id,intent_hash);

CREATE FUNCTION freeze_reservation_scope() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE lineage text[]; task_root text;
BEGIN
 IF TG_OP='UPDATE' THEN
  IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.id IS DISTINCT FROM OLD.id
   OR NEW.task_id IS DISTINCT FROM OLD.task_id OR NEW.operation_id IS DISTINCT FROM OLD.operation_id
   OR NEW.action_id IS DISTINCT FROM OLD.action_id OR NEW.root_id IS DISTINCT FROM OLD.root_id
   OR NEW.scope_task_ids IS DISTINCT FROM OLD.scope_task_ids OR NEW.charge_key IS DISTINCT FROM OLD.charge_key
   OR NEW.amount_microusd IS DISTINCT FROM OLD.amount_microusd OR NEW.provider IS DISTINCT FROM OLD.provider
   OR NEW.price_version IS DISTINCT FROM OLD.price_version OR NEW.category IS DISTINCT FROM OLD.category THEN
    RAISE EXCEPTION 'reservation admission snapshot is immutable' USING ERRCODE='23514';
  END IF;
 ELSE
  WITH RECURSIVE chain AS (
   SELECT id,parent_id,root_id,depth FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.task_id
   UNION ALL SELECT p.id,p.parent_id,p.root_id,p.depth FROM tasks p JOIN chain c ON p.id=c.parent_id WHERE p.organization_id=NEW.organization_id
  ) SELECT array_agg(id ORDER BY depth),min(root_id) INTO lineage,task_root FROM chain;
  IF lineage IS NULL THEN RAISE EXCEPTION 'task not found' USING ERRCODE='23503'; END IF;
  IF NEW.scope_task_ids IS NOT NULL AND NEW.scope_task_ids<>lineage THEN RAISE EXCEPTION 'invalid accounting scopes' USING ERRCODE='23514'; END IF;
  IF NEW.root_id IS NOT NULL AND NEW.root_id<>task_root THEN RAISE EXCEPTION 'invalid accounting root' USING ERRCODE='23514'; END IF;
  NEW.scope_task_ids:=lineage; NEW.root_id:=task_root;
  IF NEW.charge_key IS NULL THEN NEW.charge_key:=NEW.id; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER reservation_scope BEFORE INSERT OR UPDATE ON reservations FOR EACH ROW EXECUTE FUNCTION freeze_reservation_scope();
ALTER TABLE reservations ALTER COLUMN charge_key SET NOT NULL;
ALTER TABLE reservations ALTER COLUMN root_id SET NOT NULL;
ALTER TABLE reservations ALTER COLUMN scope_task_ids SET NOT NULL;
ALTER TABLE reservations ADD CONSTRAINT reservation_requires_action CHECK((category='fixture_legacy' AND action_id IS NULL) OR (category<>'fixture_legacy' AND action_id IS NOT NULL));

CREATE FUNCTION immutable_action_intent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.id IS DISTINCT FROM OLD.id OR NEW.task_id IS DISTINCT FROM OLD.task_id
 OR NEW.operation_id IS DISTINCT FROM OLD.operation_id OR NEW.intent_hash IS DISTINCT FROM OLD.intent_hash OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key THEN
  RAISE EXCEPTION 'action intent is immutable' USING ERRCODE='23514';
 END IF; RETURN NEW;
END $$;
CREATE TRIGGER action_intent_immutable BEFORE UPDATE ON actions FOR EACH ROW EXECUTE FUNCTION immutable_action_intent();

CREATE FUNCTION immutable_published_agent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.id IS DISTINCT FROM OLD.id OR NEW.version IS DISTINCT FROM OLD.version
 OR (OLD.published AND (NOT NEW.published OR NEW.name IS DISTINCT FROM OLD.name OR NEW.instructions IS DISTINCT FROM OLD.instructions
 OR NEW.model IS DISTINCT FROM OLD.model OR NEW.tools IS DISTINCT FROM OLD.tools OR NEW.budget_microusd IS DISTINCT FROM OLD.budget_microusd)) THEN
  RAISE EXCEPTION 'published agent version is immutable' USING ERRCODE='23514';
 END IF; RETURN NEW;
END $$;
CREATE TRIGGER published_agent_immutable BEFORE UPDATE ON agents FOR EACH ROW EXECUTE FUNCTION immutable_published_agent();

CREATE FUNCTION protect_ledger_history() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'accounting history is append only' USING ERRCODE='23514'; END $$;
CREATE TRIGGER ledger_immutable BEFORE UPDATE OR DELETE ON ledger_entries FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER evidence_immutable BEFORE UPDATE OR DELETE ON cost_evidence FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER incident_immutable BEFORE UPDATE OR DELETE ON financial_incidents FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
