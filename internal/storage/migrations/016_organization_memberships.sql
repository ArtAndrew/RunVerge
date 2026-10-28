-- SPDX-License-Identifier: Elastic-2.0
-- Credential origin is database-issued, shared across credential types and
-- immutable. Session rotation explicitly copies its predecessor's origin.
CREATE SEQUENCE credential_origin_sequence AS bigint MINVALUE 1 NO CYCLE;
ALTER TABLE access_tokens ADD COLUMN credential_generation bigint NOT NULL DEFAULT nextval('credential_origin_sequence') CHECK(credential_generation>0);
ALTER TABLE browser_sessions ADD COLUMN credential_generation bigint NOT NULL DEFAULT nextval('credential_origin_sequence') CHECK(credential_generation>0);
CREATE FUNCTION guard_credential_origin() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.credential_generation IS DISTINCT FROM OLD.credential_generation THEN
  RAISE EXCEPTION 'credential origin is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER credential_origin_immutable BEFORE UPDATE ON access_tokens FOR EACH ROW EXECUTE FUNCTION guard_credential_origin();
CREATE TRIGGER credential_origin_immutable BEFORE UPDATE ON browser_sessions FOR EACH ROW EXECUTE FUNCTION guard_credential_origin();

ALTER TABLE memberships ADD COLUMN display_name text;
UPDATE memberships m SET display_name=p.name FROM principals p WHERE p.id=m.principal_id;
ALTER TABLE memberships ALTER COLUMN display_name SET NOT NULL;
ALTER TABLE memberships ADD COLUMN version integer NOT NULL DEFAULT 1 CHECK(version>0);
ALTER TABLE memberships ADD COLUMN security_epoch bigint NOT NULL DEFAULT 1 CHECK(security_epoch>0);
ALTER TABLE memberships ADD COLUMN credential_cutoff bigint NOT NULL DEFAULT 0 CHECK(credential_cutoff>=0);
ALTER TABLE memberships ADD COLUMN created_at timestamptz NOT NULL DEFAULT clock_timestamp();
ALTER TABLE memberships ADD COLUMN updated_at timestamptz NOT NULL DEFAULT clock_timestamp();
ALTER TABLE memberships ADD COLUMN credentials_revoked_at timestamptz;
CREATE INDEX membership_listing ON memberships(organization_id,created_at DESC,principal_id DESC);
CREATE FUNCTION guard_organization_membership() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='INSERT' THEN
  IF NEW.display_name IS NULL THEN SELECT name INTO NEW.display_name FROM principals WHERE id=NEW.principal_id; END IF;
  IF NEW.version<>1 OR NEW.security_epoch<>1 OR NEW.credential_cutoff<>0 OR NEW.credentials_revoked_at IS NOT NULL THEN
   RAISE EXCEPTION 'membership requires fresh authority' USING ERRCODE='23514'; END IF;
  RETURN NEW;
 END IF;
 IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.principal_id IS DISTINCT FROM OLD.principal_id OR NEW.created_at IS DISTINCT FROM OLD.created_at
  OR NEW.security_epoch IS DISTINCT FROM OLD.security_epoch OR NEW.credential_cutoff<OLD.credential_cutoff
  OR NEW.version NOT IN(OLD.version,OLD.version+1) THEN
  RAISE EXCEPTION 'membership identity and authority cannot rewind or be supplied' USING ERRCODE='23514'; END IF;
 NEW.version=OLD.version+1;
 NEW.updated_at=GREATEST(clock_timestamp(),OLD.updated_at+interval '1 microsecond');
 IF NEW.role IS DISTINCT FROM OLD.role OR NEW.active IS DISTINCT FROM OLD.active THEN NEW.security_epoch=OLD.security_epoch+1; END IF;
 IF NEW.active IS DISTINCT FROM OLD.active THEN NEW.credential_cutoff=nextval('credential_origin_sequence'); END IF;
 IF NEW.credential_cutoff>OLD.credential_cutoff THEN
  NEW.credentials_revoked_at=clock_timestamp();
 ELSIF NEW.credentials_revoked_at IS DISTINCT FROM OLD.credentials_revoked_at THEN
  RAISE EXCEPTION 'credential revocation timestamp requires a new cutoff' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER membership_authority_guard BEFORE INSERT OR UPDATE ON memberships FOR EACH ROW EXECUTE FUNCTION guard_organization_membership();
CREATE TRIGGER membership_no_delete BEFORE DELETE ON memberships FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

-- Private relational snapshots preserve immutable historical wire/intent JSON.
ALTER TABLE runs ADD COLUMN owner_authority_epoch bigint NOT NULL DEFAULT 1 CHECK(owner_authority_epoch>0);
ALTER TABLE runs ADD COLUMN principal_authority_epoch bigint NOT NULL DEFAULT 1 CHECK(principal_authority_epoch>0);
ALTER TABLE operations ADD COLUMN authority_principal_id text;
UPDATE operations o SET authority_principal_id=r.principal_id FROM runs r WHERE r.organization_id=o.organization_id AND r.id=o.run_id;
ALTER TABLE operations ALTER COLUMN authority_principal_id SET NOT NULL;
ALTER TABLE operations ADD CONSTRAINT operation_authority_member FOREIGN KEY(organization_id,authority_principal_id) REFERENCES memberships(organization_id,principal_id);
ALTER TABLE operations ADD COLUMN owner_authority_epoch bigint NOT NULL DEFAULT 1 CHECK(owner_authority_epoch>0);
ALTER TABLE operations ADD COLUMN principal_authority_epoch bigint NOT NULL DEFAULT 1 CHECK(principal_authority_epoch>0);
ALTER TABLE task_automation_grants ADD COLUMN owner_authority_epoch bigint NOT NULL DEFAULT 1 CHECK(owner_authority_epoch>0);
ALTER TABLE task_automation_grants ADD COLUMN principal_authority_epoch bigint NOT NULL DEFAULT 1 CHECK(principal_authority_epoch>0);
ALTER TABLE tool_executions ADD COLUMN owner_authority_epoch bigint NOT NULL DEFAULT 1 CHECK(owner_authority_epoch>0);
ALTER TABLE tool_executions ADD COLUMN creator_authority_epoch bigint NOT NULL DEFAULT 1 CHECK(creator_authority_epoch>0);
ALTER TABLE tool_executions ADD COLUMN dispatcher_authority_epoch bigint CHECK(dispatcher_authority_epoch>0);
-- The old guards intentionally reject any update of terminal history. Backfill
-- only the new private column in this migration transaction, then restore the
-- guards before any application transaction can observe the new schema.
ALTER TABLE tool_executions DISABLE TRIGGER tool_execution_guard;
UPDATE tool_executions SET dispatcher_authority_epoch=1 WHERE dispatch_requested_by IS NOT NULL;
ALTER TABLE tool_executions ENABLE TRIGGER tool_execution_guard;
ALTER TABLE approvals ADD COLUMN decider_authority_epoch bigint CHECK(decider_authority_epoch>0);
ALTER TABLE approvals DISABLE TRIGGER tool_approval_guard;
-- The shape guard also validates CURRENT automation-grant admission. Historical
-- automatic approvals may refer to grants now revoked/completed/expired; this
-- private snapshot backfill must not re-admit or rewrite their decisions.
ALTER TABLE approvals DISABLE TRIGGER tool_approval_shape_guard;
UPDATE approvals SET decider_authority_epoch=1 WHERE execution_id IS NOT NULL AND decided_by IS NOT NULL;
ALTER TABLE approvals ENABLE TRIGGER tool_approval_shape_guard;
ALTER TABLE approvals ENABLE TRIGGER tool_approval_guard;

CREATE FUNCTION current_member_epoch(org text,principal text) RETURNS bigint LANGUAGE sql STABLE AS $$
 SELECT security_epoch FROM memberships WHERE organization_id=org AND principal_id=principal
$$;
CREATE FUNCTION guard_run_authority_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE owner text;
BEGIN
 IF TG_OP='INSERT' THEN
  SELECT owner_id INTO STRICT owner FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.task_id;
  NEW.owner_authority_epoch=current_member_epoch(NEW.organization_id,owner);
  NEW.principal_authority_epoch=current_member_epoch(NEW.organization_id,NEW.principal_id);
 ELSIF NEW.owner_authority_epoch IS DISTINCT FROM OLD.owner_authority_epoch OR NEW.principal_authority_epoch IS DISTINCT FROM OLD.principal_authority_epoch
  OR NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.id IS DISTINCT FROM OLD.id OR NEW.task_id IS DISTINCT FROM OLD.task_id OR NEW.principal_id IS DISTINCT FROM OLD.principal_id THEN
  RAISE EXCEPTION 'run authority snapshot is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER run_authority_snapshot BEFORE INSERT OR UPDATE ON runs FOR EACH ROW EXECUTE FUNCTION guard_run_authority_snapshot();
CREATE FUNCTION guard_operation_authority_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE owner text;
BEGIN
 IF TG_OP='INSERT' THEN
  IF NEW.authority_principal_id IS NULL THEN SELECT principal_id INTO STRICT NEW.authority_principal_id FROM runs WHERE organization_id=NEW.organization_id AND id=NEW.run_id; END IF;
  SELECT owner_id INTO STRICT owner FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.task_id;
  NEW.owner_authority_epoch=current_member_epoch(NEW.organization_id,owner);
  NEW.principal_authority_epoch=current_member_epoch(NEW.organization_id,NEW.authority_principal_id);
 ELSIF NEW.authority_principal_id IS DISTINCT FROM OLD.authority_principal_id OR NEW.owner_authority_epoch IS DISTINCT FROM OLD.owner_authority_epoch OR NEW.principal_authority_epoch IS DISTINCT FROM OLD.principal_authority_epoch
  OR NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.id IS DISTINCT FROM OLD.id OR NEW.task_id IS DISTINCT FROM OLD.task_id OR NEW.run_id IS DISTINCT FROM OLD.run_id THEN
  RAISE EXCEPTION 'operation authority snapshot is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER operation_authority_snapshot BEFORE INSERT OR UPDATE ON operations FOR EACH ROW EXECUTE FUNCTION guard_operation_authority_snapshot();
CREATE FUNCTION guard_automation_authority_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='INSERT' THEN
  NEW.owner_authority_epoch=current_member_epoch(NEW.organization_id,NEW.owner_id);
  NEW.principal_authority_epoch=current_member_epoch(NEW.organization_id,NEW.principal_id);
 ELSIF NEW.owner_authority_epoch IS DISTINCT FROM OLD.owner_authority_epoch OR NEW.principal_authority_epoch IS DISTINCT FROM OLD.principal_authority_epoch THEN
  RAISE EXCEPTION 'automation authority snapshot is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER automation_authority_snapshot BEFORE INSERT OR UPDATE ON task_automation_grants FOR EACH ROW EXECUTE FUNCTION guard_automation_authority_snapshot();
CREATE FUNCTION guard_tool_authority_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE owner text;
BEGIN
 IF TG_OP='INSERT' THEN
  SELECT owner_id INTO STRICT owner FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.task_id;
  NEW.owner_authority_epoch=current_member_epoch(NEW.organization_id,owner);
  NEW.creator_authority_epoch=current_member_epoch(NEW.organization_id,NEW.created_by);
  NEW.dispatcher_authority_epoch=NULL;
 ELSE
  IF NEW.owner_authority_epoch IS DISTINCT FROM OLD.owner_authority_epoch OR NEW.creator_authority_epoch IS DISTINCT FROM OLD.creator_authority_epoch THEN
   RAISE EXCEPTION 'tool creation authority is immutable' USING ERRCODE='23514'; END IF;
  IF OLD.dispatch_requested_by IS NULL AND NEW.dispatch_requested_by IS NOT NULL THEN
   NEW.dispatcher_authority_epoch=current_member_epoch(NEW.organization_id,NEW.dispatch_requested_by);
  ELSIF NEW.dispatcher_authority_epoch IS DISTINCT FROM OLD.dispatcher_authority_epoch THEN
   RAISE EXCEPTION 'tool dispatch authority is immutable' USING ERRCODE='23514'; END IF;
 END IF;
 IF (NEW.dispatch_requested_by IS NULL) IS DISTINCT FROM (NEW.dispatcher_authority_epoch IS NULL) THEN
  RAISE EXCEPTION 'tool dispatcher authority is required' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER tool_authority_snapshot BEFORE INSERT OR UPDATE ON tool_executions FOR EACH ROW EXECUTE FUNCTION guard_tool_authority_snapshot();
CREATE FUNCTION guard_approval_authority_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.execution_id IS NOT NULL THEN
  IF TG_OP='INSERT' THEN NEW.decider_authority_epoch=NULL;
  ELSIF OLD.decided_by IS NULL AND NEW.decided_by IS NOT NULL THEN NEW.decider_authority_epoch=current_member_epoch(NEW.organization_id,NEW.decided_by);
  ELSIF NEW.decider_authority_epoch IS DISTINCT FROM OLD.decider_authority_epoch THEN
   RAISE EXCEPTION 'approval authority is immutable' USING ERRCODE='23514'; END IF;
  IF (NEW.decided_by IS NULL) IS DISTINCT FROM (NEW.decider_authority_epoch IS NULL) THEN
   RAISE EXCEPTION 'approval decider authority is required' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER approval_authority_snapshot BEFORE INSERT OR UPDATE ON approvals FOR EACH ROW EXECUTE FUNCTION guard_approval_authority_snapshot();

CREATE TABLE organization_membership_audit (
 organization_id text NOT NULL REFERENCES organizations(id),id text NOT NULL,
 actor_id text NOT NULL,target_principal_id text NOT NULL,
 action text NOT NULL CHECK(action IN('member.created','member.updated','member.credentials_revoked')),
 reason text NOT NULL CHECK(octet_length(reason) BETWEEN 1 AND 4096),
 before_state jsonb,after_state jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,id),
 FOREIGN KEY(organization_id,actor_id) REFERENCES memberships(organization_id,principal_id),
 FOREIGN KEY(organization_id,target_principal_id) REFERENCES memberships(organization_id,principal_id),
 CHECK(before_state IS NULL OR jsonb_typeof(before_state)='object'),
 CHECK(jsonb_typeof(after_state)='object'),
 CHECK(octet_length(after_state::text)<=16384 AND (before_state IS NULL OR octet_length(before_state::text)<=16384))
);
CREATE INDEX membership_audit_listing ON organization_membership_audit(organization_id,created_at DESC,id DESC);
CREATE INDEX membership_audit_target ON organization_membership_audit(organization_id,target_principal_id,created_at DESC,id DESC);
CREATE TRIGGER membership_audit_no_update BEFORE UPDATE ON organization_membership_audit FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER membership_audit_no_delete BEFORE DELETE ON organization_membership_audit FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
