-- SPDX-License-Identifier: Elastic-2.0
-- Draft until the first successful application. Historical rows are not
-- updated: all organizations start at epoch one and old snapshots default one.
ALTER TABLE principals ADD COLUMN kind text NOT NULL DEFAULT 'human' CHECK(kind IN('human','service'));
ALTER TABLE principals ADD COLUMN service_organization_id text REFERENCES organizations(id);
ALTER TABLE principals ADD CONSTRAINT principal_service_home CHECK((kind='human' AND service_organization_id IS NULL) OR (kind='service' AND service_organization_id IS NOT NULL));
CREATE FUNCTION guard_principal_kind() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.kind IS DISTINCT FROM OLD.kind OR NEW.service_organization_id IS DISTINCT FROM OLD.service_organization_id THEN
  RAISE EXCEPTION 'principal kind and service home are immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER principal_kind_immutable BEFORE UPDATE ON principals FOR EACH ROW EXECUTE FUNCTION guard_principal_kind();
CREATE FUNCTION guard_service_membership() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE k text; home text;
BEGIN
 SELECT kind,service_organization_id INTO STRICT k,home FROM principals WHERE id=NEW.principal_id;
 IF k='service' AND (NEW.organization_id<>home OR NEW.role NOT IN('member','viewer')) THEN
  RAISE EXCEPTION 'service membership must stay in its home organization with a bounded role' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER membership_service_guard BEFORE INSERT OR UPDATE ON memberships FOR EACH ROW EXECUTE FUNCTION guard_service_membership();

CREATE TABLE organization_login_policies (
 organization_id text PRIMARY KEY REFERENCES organizations(id),
 mode text NOT NULL DEFAULT 'mixed' CHECK(mode IN('mixed','oidc_required')),
 oidc_issuer text NOT NULL DEFAULT '' CHECK(octet_length(oidc_issuer)<=4096),
 service_access_enabled boolean NOT NULL DEFAULT true,
 max_service_key_ttl_seconds integer NOT NULL DEFAULT 3600 CHECK(max_service_key_ttl_seconds BETWEEN 1 AND 86400),
 version integer NOT NULL DEFAULT 1 CHECK(version>0),
 human_authority_epoch bigint NOT NULL DEFAULT 1 CHECK(human_authority_epoch>0),
 service_authority_epoch bigint NOT NULL DEFAULT 1 CHECK(service_authority_epoch>0),
 human_credential_cutoff bigint NOT NULL DEFAULT 0 CHECK(human_credential_cutoff>=0),
 service_credential_cutoff bigint NOT NULL DEFAULT 0 CHECK(service_credential_cutoff>=0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((mode='mixed' AND oidc_issuer='') OR (mode='oidc_required' AND octet_length(oidc_issuer)>0))
);
INSERT INTO organization_login_policies(organization_id) SELECT id FROM organizations;
CREATE FUNCTION initialize_organization_login_policy() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO organization_login_policies(organization_id) VALUES(NEW.id);
 RETURN NEW;
END $$;
CREATE TRIGGER organization_initialize_login_policy AFTER INSERT ON organizations FOR EACH ROW EXECUTE FUNCTION initialize_organization_login_policy();
CREATE FUNCTION guard_organization_login_policy() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='INSERT' THEN
  IF NEW.version<>1 OR NEW.human_authority_epoch<>1 OR NEW.service_authority_epoch<>1 OR NEW.human_credential_cutoff<>0 OR NEW.service_credential_cutoff<>0 THEN
   RAISE EXCEPTION 'login policy requires fresh authority' USING ERRCODE='23514'; END IF;
  RETURN NEW;
 END IF;
 IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.created_at IS DISTINCT FROM OLD.created_at
  OR NEW.human_authority_epoch<>OLD.human_authority_epoch OR NEW.service_authority_epoch<>OLD.service_authority_epoch
  OR NEW.human_credential_cutoff<>OLD.human_credential_cutoff OR NEW.service_credential_cutoff<>OLD.service_credential_cutoff
  OR NEW.version NOT IN(OLD.version,OLD.version+1) THEN
  RAISE EXCEPTION 'login policy identity and authority cannot be supplied or rewound' USING ERRCODE='23514';
 END IF;
 NEW.version=OLD.version+1;
 NEW.updated_at=GREATEST(clock_timestamp(),OLD.updated_at+interval '1 microsecond');
 IF NEW.mode IS DISTINCT FROM OLD.mode OR NEW.oidc_issuer IS DISTINCT FROM OLD.oidc_issuer THEN
  NEW.human_authority_epoch=OLD.human_authority_epoch+1;
  NEW.human_credential_cutoff=nextval('credential_origin_sequence');
 END IF;
 IF NEW.service_access_enabled IS DISTINCT FROM OLD.service_access_enabled THEN
  NEW.service_authority_epoch=OLD.service_authority_epoch+1;
  NEW.service_credential_cutoff=nextval('credential_origin_sequence');
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER organization_login_policy_guard BEFORE INSERT OR UPDATE ON organization_login_policies FOR EACH ROW EXECUTE FUNCTION guard_organization_login_policy();
CREATE TRIGGER organization_login_policy_no_delete BEFORE DELETE ON organization_login_policies FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE TABLE service_credentials (
 organization_id text NOT NULL REFERENCES organizations(id),id text NOT NULL,
 principal_id text NOT NULL,token_hash text NOT NULL UNIQUE CHECK(token_hash~'^[a-f0-9]{64}$'),
 role_ceiling text NOT NULL CHECK(role_ceiling IN('member','viewer')),
 ttl_seconds integer NOT NULL CHECK(ttl_seconds BETWEEN 1 AND 86400),
 version integer NOT NULL DEFAULT 1 CHECK(version>0),revoked boolean NOT NULL DEFAULT false,
 credential_generation bigint NOT NULL DEFAULT nextval('credential_origin_sequence') CHECK(credential_generation>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),expires_at timestamptz NOT NULL,
 PRIMARY KEY(organization_id,id),
 FOREIGN KEY(organization_id,principal_id) REFERENCES memberships(organization_id,principal_id),
 CHECK(expires_at=created_at+ttl_seconds*interval '1 second')
);
CREATE INDEX service_credentials_listing ON service_credentials(organization_id,principal_id,created_at DESC,id DESC);
CREATE FUNCTION guard_service_credential() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE allowed boolean;
BEGIN
 IF TG_OP='INSERT' THEN
  SELECT p.active AND m.active AND o.active AND p.kind='service' AND p.service_organization_id=NEW.organization_id
   AND m.role IN('member','viewer') AND policy.service_access_enabled AND NEW.ttl_seconds<=policy.max_service_key_ttl_seconds
   INTO allowed FROM principals p JOIN memberships m ON m.principal_id=p.id AND m.organization_id=NEW.organization_id
   JOIN organizations o ON o.id=m.organization_id JOIN organization_login_policies policy ON policy.organization_id=m.organization_id
   WHERE p.id=NEW.principal_id;
  IF NOT COALESCE(allowed,false) OR NEW.version<>1 OR NEW.revoked OR EXISTS(SELECT 1 FROM access_tokens WHERE token_hash=NEW.token_hash) THEN
   RAISE EXCEPTION 'service credential admission rejected' USING ERRCODE='23514'; END IF;
  NEW.created_at=clock_timestamp();
  NEW.expires_at=NEW.created_at+NEW.ttl_seconds*interval '1 second';
  NEW.credential_generation=nextval('credential_origin_sequence');
  RETURN NEW;
 END IF;
 IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.id IS DISTINCT FROM OLD.id OR NEW.principal_id IS DISTINCT FROM OLD.principal_id
  OR NEW.token_hash IS DISTINCT FROM OLD.token_hash OR NEW.role_ceiling IS DISTINCT FROM OLD.role_ceiling OR NEW.ttl_seconds IS DISTINCT FROM OLD.ttl_seconds
  OR NEW.created_at IS DISTINCT FROM OLD.created_at OR NEW.expires_at IS DISTINCT FROM OLD.expires_at OR NEW.credential_generation IS DISTINCT FROM OLD.credential_generation
  OR OLD.revoked OR NOT NEW.revoked OR NEW.version NOT IN(OLD.version,OLD.version+1) THEN
  RAISE EXCEPTION 'service credential is immutable except its one-way revocation' USING ERRCODE='23514';
 END IF;
 NEW.version=OLD.version+1;
 RETURN NEW;
END $$;
CREATE TRIGGER service_credential_guard BEFORE INSERT OR UPDATE ON service_credentials FOR EACH ROW EXECUTE FUNCTION guard_service_credential();
CREATE TRIGGER service_credential_no_delete BEFORE DELETE ON service_credentials FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

-- A service principal cannot obtain a legacy human credential or an OIDC
-- identity/session through an alternate trusted-host or internal code path.
CREATE FUNCTION guard_human_credential() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE k text;
BEGIN
 SELECT kind INTO STRICT k FROM principals WHERE id=NEW.principal_id;
 IF k<>'human' THEN RAISE EXCEPTION 'human identity required' USING ERRCODE='23514'; END IF;
 IF TG_TABLE_NAME='access_tokens' THEN
  IF EXISTS(SELECT 1 FROM service_credentials WHERE token_hash=NEW.token_hash) THEN
   RAISE EXCEPTION 'credential domain collision' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER access_token_human_guard BEFORE INSERT OR UPDATE ON access_tokens FOR EACH ROW EXECUTE FUNCTION guard_human_credential();
CREATE TRIGGER browser_session_human_guard BEFORE INSERT OR UPDATE ON browser_sessions FOR EACH ROW EXECUTE FUNCTION guard_human_credential();
CREATE TRIGGER identity_mapping_human_guard BEFORE INSERT OR UPDATE ON identity_mappings FOR EACH ROW EXECUTE FUNCTION guard_human_credential();

CREATE FUNCTION current_principal_policy_epoch(org text,principal text) RETURNS bigint LANGUAGE sql STABLE AS $$
 SELECT CASE p.kind WHEN 'human' THEN policy.human_authority_epoch WHEN 'service' THEN policy.service_authority_epoch END
 FROM principals p JOIN organization_login_policies policy ON policy.organization_id=org WHERE p.id=principal
$$;
ALTER TABLE runs ADD COLUMN owner_policy_epoch bigint NOT NULL DEFAULT 1 CHECK(owner_policy_epoch>0);
ALTER TABLE runs ADD COLUMN principal_policy_epoch bigint NOT NULL DEFAULT 1 CHECK(principal_policy_epoch>0);
ALTER TABLE operations ADD COLUMN owner_policy_epoch bigint NOT NULL DEFAULT 1 CHECK(owner_policy_epoch>0);
ALTER TABLE operations ADD COLUMN principal_policy_epoch bigint NOT NULL DEFAULT 1 CHECK(principal_policy_epoch>0);
ALTER TABLE task_automation_grants ADD COLUMN owner_policy_epoch bigint NOT NULL DEFAULT 1 CHECK(owner_policy_epoch>0);
ALTER TABLE task_automation_grants ADD COLUMN principal_policy_epoch bigint NOT NULL DEFAULT 1 CHECK(principal_policy_epoch>0);
ALTER TABLE tool_executions ADD COLUMN owner_policy_epoch bigint NOT NULL DEFAULT 1 CHECK(owner_policy_epoch>0);
ALTER TABLE tool_executions ADD COLUMN creator_policy_epoch bigint NOT NULL DEFAULT 1 CHECK(creator_policy_epoch>0);
ALTER TABLE tool_executions ADD COLUMN dispatcher_policy_epoch bigint NOT NULL DEFAULT 1 CHECK(dispatcher_policy_epoch>0);
ALTER TABLE approvals ADD COLUMN decider_policy_epoch bigint NOT NULL DEFAULT 1 CHECK(decider_policy_epoch>0);

CREATE FUNCTION guard_run_policy_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE owner text;
BEGIN
 IF TG_OP='INSERT' THEN
  SELECT owner_id INTO STRICT owner FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.task_id;
  NEW.owner_policy_epoch=current_principal_policy_epoch(NEW.organization_id,owner);
  NEW.principal_policy_epoch=current_principal_policy_epoch(NEW.organization_id,NEW.principal_id);
 ELSIF NEW.owner_policy_epoch IS DISTINCT FROM OLD.owner_policy_epoch OR NEW.principal_policy_epoch IS DISTINCT FROM OLD.principal_policy_epoch THEN
  RAISE EXCEPTION 'run policy snapshot is immutable' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER run_policy_snapshot BEFORE INSERT OR UPDATE ON runs FOR EACH ROW EXECUTE FUNCTION guard_run_policy_snapshot();
CREATE FUNCTION guard_operation_policy_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE owner text;
BEGIN
 IF TG_OP='INSERT' THEN
  SELECT owner_id INTO STRICT owner FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.task_id;
  NEW.owner_policy_epoch=current_principal_policy_epoch(NEW.organization_id,owner);
  NEW.principal_policy_epoch=current_principal_policy_epoch(NEW.organization_id,NEW.authority_principal_id);
 ELSIF NEW.owner_policy_epoch IS DISTINCT FROM OLD.owner_policy_epoch OR NEW.principal_policy_epoch IS DISTINCT FROM OLD.principal_policy_epoch THEN
  RAISE EXCEPTION 'operation policy snapshot is immutable' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER operation_policy_snapshot BEFORE INSERT OR UPDATE ON operations FOR EACH ROW EXECUTE FUNCTION guard_operation_policy_snapshot();
CREATE FUNCTION guard_automation_policy_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='INSERT' THEN
  NEW.owner_policy_epoch=current_principal_policy_epoch(NEW.organization_id,NEW.owner_id);
  NEW.principal_policy_epoch=current_principal_policy_epoch(NEW.organization_id,NEW.principal_id);
 ELSIF NEW.owner_policy_epoch IS DISTINCT FROM OLD.owner_policy_epoch OR NEW.principal_policy_epoch IS DISTINCT FROM OLD.principal_policy_epoch THEN
  RAISE EXCEPTION 'automation policy snapshot is immutable' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER automation_policy_snapshot BEFORE INSERT OR UPDATE ON task_automation_grants FOR EACH ROW EXECUTE FUNCTION guard_automation_policy_snapshot();
CREATE FUNCTION guard_tool_policy_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE owner text;
BEGIN
 IF TG_OP='INSERT' THEN
  SELECT owner_id INTO STRICT owner FROM tasks WHERE organization_id=NEW.organization_id AND id=NEW.task_id;
  NEW.owner_policy_epoch=current_principal_policy_epoch(NEW.organization_id,owner);
  NEW.creator_policy_epoch=current_principal_policy_epoch(NEW.organization_id,NEW.created_by);
  NEW.dispatcher_policy_epoch=1;
 ELSE
  IF NEW.owner_policy_epoch IS DISTINCT FROM OLD.owner_policy_epoch OR NEW.creator_policy_epoch IS DISTINCT FROM OLD.creator_policy_epoch THEN
   RAISE EXCEPTION 'tool creation policy is immutable' USING ERRCODE='23514'; END IF;
  IF OLD.dispatch_requested_by IS NULL AND NEW.dispatch_requested_by IS NOT NULL THEN
   NEW.dispatcher_policy_epoch=current_principal_policy_epoch(NEW.organization_id,NEW.dispatch_requested_by);
  ELSIF NEW.dispatcher_policy_epoch IS DISTINCT FROM OLD.dispatcher_policy_epoch THEN
   RAISE EXCEPTION 'tool dispatch policy is immutable' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER tool_policy_snapshot BEFORE INSERT OR UPDATE ON tool_executions FOR EACH ROW EXECUTE FUNCTION guard_tool_policy_snapshot();
CREATE FUNCTION guard_approval_policy_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.execution_id IS NOT NULL THEN
  IF TG_OP='INSERT' THEN
   NEW.decider_policy_epoch=CASE WHEN NEW.decided_by IS NULL THEN 1 ELSE current_principal_policy_epoch(NEW.organization_id,NEW.decided_by) END;
  ELSIF OLD.decided_by IS NULL AND NEW.decided_by IS NOT NULL THEN
   NEW.decider_policy_epoch=current_principal_policy_epoch(NEW.organization_id,NEW.decided_by);
  ELSIF NEW.decider_policy_epoch IS DISTINCT FROM OLD.decider_policy_epoch THEN
   RAISE EXCEPTION 'approval policy is immutable' USING ERRCODE='23514'; END IF;
 ELSIF TG_OP='UPDATE' AND NEW.decider_policy_epoch IS DISTINCT FROM OLD.decider_policy_epoch THEN
  RAISE EXCEPTION 'approval policy is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER approval_policy_snapshot BEFORE INSERT OR UPDATE ON approvals FOR EACH ROW EXECUTE FUNCTION guard_approval_policy_snapshot();

CREATE TABLE organization_security_audit (
 organization_id text NOT NULL REFERENCES organizations(id),id text NOT NULL,
 actor_type text NOT NULL CHECK(actor_type IN('principal','trusted_host')),actor_id text,
 object_type text NOT NULL CHECK(object_type IN('policy','service_account','service_key')),object_id text NOT NULL,
 target_principal_id text,
 action text NOT NULL CHECK(action IN('policy.updated','policy.recovered','service_account.created','service_account.updated','service_key.issued','service_key.revoked')),
 reason text NOT NULL CHECK(octet_length(reason) BETWEEN 1 AND 4096),before_state jsonb,after_state jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),PRIMARY KEY(organization_id,id),
 FOREIGN KEY(organization_id,actor_id) REFERENCES memberships(organization_id,principal_id),
 FOREIGN KEY(organization_id,target_principal_id) REFERENCES memberships(organization_id,principal_id),
 CHECK((actor_type='principal' AND actor_id IS NOT NULL) OR (actor_type='trusted_host' AND actor_id IS NULL)),
 CHECK(before_state IS NULL OR jsonb_typeof(before_state)='object'),CHECK(jsonb_typeof(after_state)='object'),
 CHECK(octet_length(after_state::text)<=16384 AND (before_state IS NULL OR octet_length(before_state::text)<=16384))
);
CREATE INDEX security_audit_listing ON organization_security_audit(organization_id,created_at DESC,id DESC);
CREATE INDEX security_audit_target ON organization_security_audit(organization_id,target_principal_id,created_at DESC,id DESC);
CREATE TRIGGER security_audit_no_update BEFORE UPDATE ON organization_security_audit FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER security_audit_no_delete BEFORE DELETE ON organization_security_audit FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
