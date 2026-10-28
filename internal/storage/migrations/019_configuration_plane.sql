-- SPDX-License-Identifier: Elastic-2.0
-- Draft: never apply to the main profile before populated-upgrade verification.
CREATE TABLE configuration_revisions (
 organization_id text PRIMARY KEY REFERENCES organizations(id),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
INSERT INTO configuration_revisions(organization_id) SELECT id FROM organizations;
CREATE FUNCTION initialize_configuration_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN INSERT INTO configuration_revisions(organization_id) VALUES(NEW.id); RETURN NEW; END $$;
CREATE TRIGGER organization_configuration_revision AFTER INSERT ON organizations FOR EACH ROW EXECUTE FUNCTION initialize_configuration_revision();
CREATE FUNCTION guard_configuration_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.revision<>OLD.revision+1 THEN
  RAISE EXCEPTION 'configuration revision cannot change identity or skip/rewind' USING ERRCODE='23514'; END IF;
 NEW.updated_at=GREATEST(clock_timestamp(),OLD.updated_at+interval '1 microsecond'); RETURN NEW;
END $$;
CREATE TRIGGER configuration_revision_guard BEFORE UPDATE ON configuration_revisions FOR EACH ROW EXECUTE FUNCTION guard_configuration_revision();
CREATE TRIGGER configuration_revision_no_delete BEFORE DELETE ON configuration_revisions FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE TABLE credential_purpose_profiles (
 organization_id text NOT NULL REFERENCES organizations(id),id text NOT NULL,version integer NOT NULL CHECK(version>0),
 kind text NOT NULL CHECK(kind IN('model','mcp')),
 endpoint_digest text NOT NULL CHECK(endpoint_digest~'^[a-f0-9]{64}$'),
 profile jsonb NOT NULL CHECK(jsonb_typeof(profile)='object'),active boolean NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,id,version),
 CHECK((profile->>'id'=id AND profile->>'organization_id'=organization_id AND profile->'version'=to_jsonb(version)
  AND profile->>'kind'=kind AND profile->>'endpoint_digest'=endpoint_digest) IS TRUE),
 CHECK(((kind='model' AND profile->>'credential_environment'~'^PLATFORM_MODEL_KEY_[A-Z0-9_]{1,64}$')
    OR (kind='mcp' AND profile->>'credential_environment'~'^PLATFORM_MCP_KEY_[A-Z0-9_]{1,64}$')) IS TRUE),
 CHECK(octet_length(profile::text)<=4096)
);
CREATE UNIQUE INDEX credential_purpose_one_active ON credential_purpose_profiles(organization_id,id) WHERE active;

CREATE TABLE configuration_destinations (
 organization_id text NOT NULL REFERENCES organizations(id),id text NOT NULL,version integer NOT NULL CHECK(version>0),
 kind text NOT NULL CHECK(kind IN('model','mcp')),target_id text NOT NULL,
 credential_purpose_id text NOT NULL,credential_purpose_version integer NOT NULL,
 profile jsonb NOT NULL CHECK(jsonb_typeof(profile)='object'),active boolean NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,id,version),
 UNIQUE(organization_id,id,version,kind,target_id,credential_purpose_id,credential_purpose_version),
 FOREIGN KEY(organization_id,credential_purpose_id,credential_purpose_version) REFERENCES credential_purpose_profiles(organization_id,id,version),
 CHECK((profile#>>'{destination,id}'=id AND profile#>>'{destination,organization_id}'=organization_id
  AND profile#>'{destination,version}'=to_jsonb(version) AND profile#>>'{destination,kind}'=kind
  AND profile#>>'{destination,target_id}'=target_id AND profile#>>'{destination,credential_purpose_id}'=credential_purpose_id
  AND profile#>'{destination,credential_purpose_version}'=to_jsonb(credential_purpose_version)) IS TRUE),
 CHECK(((kind='model' AND jsonb_typeof(profile->'model')='object' AND NOT(profile?'mcp') AND profile#>>'{model,credential_environment}'='')
    OR (kind='mcp' AND jsonb_typeof(profile->'mcp')='object' AND NOT(profile?'model') AND profile#>'{destination,discovery_unmetered_approved}'='true'::jsonb)) IS TRUE),
 CHECK(octet_length(profile::text)<=131072)
);
CREATE UNIQUE INDEX configuration_destination_one_active ON configuration_destinations(organization_id,id) WHERE active;

-- Profiles are immutable. Retiring a version cannot reactivate it; a new
-- version is an explicit new owner-approved destination/purpose identity.
CREATE FUNCTION guard_configuration_profile() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF to_jsonb(NEW)-'active' IS DISTINCT FROM to_jsonb(OLD)-'active' OR NOT OLD.active OR NEW.active THEN
  RAISE EXCEPTION 'configuration profile is immutable except one-way retirement' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER credential_purpose_immutable BEFORE UPDATE ON credential_purpose_profiles FOR EACH ROW EXECUTE FUNCTION guard_configuration_profile();
CREATE TRIGGER credential_purpose_no_delete BEFORE DELETE ON credential_purpose_profiles FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TRIGGER configuration_destination_immutable BEFORE UPDATE ON configuration_destinations FOR EACH ROW EXECUTE FUNCTION guard_configuration_profile();
CREATE TRIGGER configuration_destination_no_delete BEFORE DELETE ON configuration_destinations FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE TABLE configuration_grants (
 organization_id text NOT NULL REFERENCES organizations(id),id text NOT NULL,
 issuer_id text NOT NULL,subject_id text NOT NULL,parent_grant_id text,depth integer NOT NULL CHECK(depth BETWEEN 0 AND 4),
 issuer_authority_epoch bigint NOT NULL CHECK(issuer_authority_epoch>0),subject_authority_epoch bigint NOT NULL CHECK(subject_authority_epoch>0),
 issuer_policy_epoch bigint NOT NULL CHECK(issuer_policy_epoch>0),subject_policy_epoch bigint NOT NULL CHECK(subject_policy_epoch>0),
 body jsonb NOT NULL CHECK(jsonb_typeof(body)='object'),version integer NOT NULL DEFAULT 1 CHECK(version>0),revoked boolean NOT NULL DEFAULT false,
 max_plans integer NOT NULL CHECK(max_plans BETWEEN 1 AND 100),used_plans integer NOT NULL DEFAULT 0 CHECK(used_plans BETWEEN 0 AND max_plans),
 max_verifications integer NOT NULL CHECK(max_verifications BETWEEN 1 AND 100),used_verifications integer NOT NULL DEFAULT 0 CHECK(used_verifications BETWEEN 0 AND max_verifications),
 max_allocated_charge_microusd bigint NOT NULL CHECK(max_allocated_charge_microusd BETWEEN 0 AND 1000000000000),
 allocated_charge_microusd bigint NOT NULL DEFAULT 0 CHECK(allocated_charge_microusd BETWEEN 0 AND max_allocated_charge_microusd),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),expires_at timestamptz NOT NULL CHECK(expires_at>created_at),
 PRIMARY KEY(organization_id,id),
 FOREIGN KEY(organization_id,issuer_id) REFERENCES memberships(organization_id,principal_id),
 FOREIGN KEY(organization_id,subject_id) REFERENCES memberships(organization_id,principal_id),
 FOREIGN KEY(organization_id,parent_grant_id) REFERENCES configuration_grants(organization_id,id),
 CHECK((parent_grant_id IS NULL AND depth=0) OR (parent_grant_id IS NOT NULL AND depth>0)),
 CHECK((body->>'id'=id AND body->>'organization_id'=organization_id AND body->>'issuer_id'=issuer_id AND body->>'subject_id'=subject_id) IS TRUE),
 CHECK(octet_length(body::text)<=32768),
 CHECK(expires_at<=created_at+interval '24 hours')
);
CREATE INDEX configuration_grant_listing ON configuration_grants(organization_id,created_at DESC,id DESC);
CREATE FUNCTION guard_configuration_grant() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF to_jsonb(NEW)-ARRAY['used_plans','used_verifications','allocated_charge_microusd','revoked','version']
    IS DISTINCT FROM to_jsonb(OLD)-ARRAY['used_plans','used_verifications','allocated_charge_microusd','revoked','version']
  OR NEW.used_plans<OLD.used_plans OR NEW.used_verifications<OLD.used_verifications OR NEW.allocated_charge_microusd<OLD.allocated_charge_microusd
  OR (OLD.revoked AND NOT NEW.revoked) OR NEW.version<>OLD.version THEN
  RAISE EXCEPTION 'grant authority is immutable and counters cannot rewind' USING ERRCODE='23514'; END IF;
 IF NOT OLD.revoked AND NEW.revoked THEN NEW.version=OLD.version+1; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER configuration_grant_guard BEFORE UPDATE ON configuration_grants FOR EACH ROW EXECUTE FUNCTION guard_configuration_grant();
CREATE TRIGGER configuration_grant_no_delete BEFORE DELETE ON configuration_grants FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE TABLE configuration_plans (
 organization_id text NOT NULL REFERENCES organizations(id),id text NOT NULL,actor_id text NOT NULL,grant_id text,
 actor_kind text NOT NULL CHECK(actor_kind IN('human','service')),admitted_role text NOT NULL CHECK(admitted_role IN('owner','admin','member')),
 actor_authority_epoch bigint NOT NULL CHECK(actor_authority_epoch>0),actor_policy_epoch bigint NOT NULL CHECK(actor_policy_epoch>0),
 base_revision bigint NOT NULL CHECK(base_revision>0),digest text NOT NULL CHECK(digest~'^[a-f0-9]{64}$'),
 body jsonb NOT NULL CHECK(jsonb_typeof(body)='object'),version integer NOT NULL DEFAULT 1 CHECK(version>0),status text NOT NULL DEFAULT 'planned' CHECK(status IN('planned','applied')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),expires_at timestamptz NOT NULL CHECK(expires_at>created_at),
 applied_at timestamptz,applied_revision bigint,receipt_id text,
 PRIMARY KEY(organization_id,id),
 FOREIGN KEY(organization_id,actor_id) REFERENCES memberships(organization_id,principal_id),
 FOREIGN KEY(organization_id,grant_id) REFERENCES configuration_grants(organization_id,id),
 CHECK((status='planned' AND applied_at IS NULL AND applied_revision IS NULL AND receipt_id IS NULL)
    OR (status='applied' AND applied_at IS NOT NULL AND applied_revision IS NOT NULL AND applied_revision>base_revision AND receipt_id IS NOT NULL AND octet_length(receipt_id)>0)),
 CHECK((body->>'id'=id AND body->>'organization_id'=organization_id AND body->>'actor_id'=actor_id AND body->>'digest'=digest) IS TRUE),
 CHECK(octet_length(body::text)<=65536),
 CHECK(expires_at<=created_at+interval '1 hour'),
 CHECK(actor_kind<>'service' OR (admitted_role='member' AND grant_id IS NOT NULL))
);
CREATE INDEX configuration_plan_listing ON configuration_plans(organization_id,created_at DESC,id DESC);
CREATE FUNCTION guard_configuration_plan() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF to_jsonb(NEW)-ARRAY['version','status','applied_at','applied_revision','receipt_id']
    IS DISTINCT FROM to_jsonb(OLD)-ARRAY['version','status','applied_at','applied_revision','receipt_id']
  OR OLD.status<>'planned' OR NEW.status<>'applied' OR NEW.version<>OLD.version THEN
  RAISE EXCEPTION 'plan intent is immutable and applies once' USING ERRCODE='23514'; END IF;
 NEW.version=OLD.version+1;NEW.applied_at=clock_timestamp();RETURN NEW;
END $$;
CREATE TRIGGER configuration_plan_guard BEFORE UPDATE ON configuration_plans FOR EACH ROW EXECUTE FUNCTION guard_configuration_plan();
CREATE TRIGGER configuration_plan_no_delete BEFORE DELETE ON configuration_plans FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE TABLE configuration_validations (
 organization_id text NOT NULL,id text NOT NULL,plan_id text NOT NULL,body jsonb NOT NULL CHECK(jsonb_typeof(body)='object'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),PRIMARY KEY(organization_id,id),UNIQUE(organization_id,id,plan_id),
 FOREIGN KEY(organization_id,plan_id) REFERENCES configuration_plans(organization_id,id)
);
CREATE TRIGGER configuration_validation_immutable BEFORE UPDATE OR DELETE ON configuration_validations FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TABLE configuration_approvals (
 organization_id text NOT NULL,id text NOT NULL,plan_id text NOT NULL,validation_id text NOT NULL,principal_id text NOT NULL,
 authority_epoch bigint NOT NULL CHECK(authority_epoch>0),policy_epoch bigint NOT NULL CHECK(policy_epoch>0),
 body jsonb NOT NULL CHECK(jsonb_typeof(body)='object'),created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,id),FOREIGN KEY(organization_id,plan_id) REFERENCES configuration_plans(organization_id,id),
 FOREIGN KEY(organization_id,validation_id,plan_id) REFERENCES configuration_validations(organization_id,id,plan_id),
 FOREIGN KEY(organization_id,principal_id) REFERENCES memberships(organization_id,principal_id)
);
CREATE TRIGGER configuration_approval_immutable BEFORE UPDATE OR DELETE ON configuration_approvals FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE TABLE mcp_connections (
 organization_id text NOT NULL REFERENCES organizations(id),id text NOT NULL,version integer NOT NULL CHECK(version>0),
 profile jsonb NOT NULL CHECK(jsonb_typeof(profile)='object'),active boolean NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),PRIMARY KEY(organization_id,id,version)
);
CREATE UNIQUE INDEX mcp_connection_one_active ON mcp_connections(organization_id,id) WHERE active;
CREATE TRIGGER mcp_connection_immutable BEFORE UPDATE ON mcp_connections FOR EACH ROW EXECUTE FUNCTION guard_configuration_profile();
CREATE TRIGGER mcp_connection_no_delete BEFORE DELETE ON mcp_connections FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();
CREATE TABLE configuration_resource_bindings (
 organization_id text NOT NULL,kind text NOT NULL CHECK(kind IN('model','mcp')),target_id text NOT NULL,resource_version integer NOT NULL CHECK(resource_version>0),
 destination_id text NOT NULL,destination_version integer NOT NULL,credential_purpose_id text NOT NULL,credential_purpose_version integer NOT NULL,
 PRIMARY KEY(organization_id,kind,target_id,resource_version),
 FOREIGN KEY(organization_id,destination_id,destination_version,kind,target_id,credential_purpose_id,credential_purpose_version)
 REFERENCES configuration_destinations(organization_id,id,version,kind,target_id,credential_purpose_id,credential_purpose_version)
);
CREATE FUNCTION guard_configuration_resource_binding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF (NEW.kind='model' AND NOT EXISTS(SELECT 1 FROM model_connections WHERE organization_id=NEW.organization_id AND id=NEW.target_id AND version=NEW.resource_version))
 OR (NEW.kind='mcp' AND NOT EXISTS(SELECT 1 FROM mcp_connections WHERE organization_id=NEW.organization_id AND id=NEW.target_id AND version=NEW.resource_version)) THEN
  RAISE EXCEPTION 'configuration resource must exist in its organization' USING ERRCODE='23514';END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER configuration_resource_binding_insert BEFORE INSERT ON configuration_resource_bindings FOR EACH ROW EXECUTE FUNCTION guard_configuration_resource_binding();
CREATE TRIGGER configuration_resource_binding_immutable BEFORE UPDATE OR DELETE ON configuration_resource_bindings FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE TABLE configuration_verifications (
 organization_id text NOT NULL,id text NOT NULL,plan_id text NOT NULL,operation_id text NOT NULL,actor_id text NOT NULL,grant_id text,
 kind text NOT NULL CHECK(kind IN('model','mcp')),target_id text NOT NULL,resource_version integer NOT NULL,
 actor_authority_epoch bigint NOT NULL CHECK(actor_authority_epoch>0),actor_policy_epoch bigint NOT NULL CHECK(actor_policy_epoch>0),
 actor_kind text NOT NULL CHECK(actor_kind IN('human','service')),admitted_role text NOT NULL CHECK(admitted_role IN('owner','admin','member')),
 body jsonb NOT NULL CHECK(jsonb_typeof(body)='object'),task_id text,
 status text NOT NULL DEFAULT 'queued' CHECK(status IN('queued','sent','response_received','unknown','failed','cancelled')),
 version integer NOT NULL DEFAULT 1 CHECK(version>0),worker_id text NOT NULL DEFAULT '',lease_until timestamptz,fencing_token bigint NOT NULL DEFAULT 0 CHECK(fencing_token>=0),
 dispatch_started_at timestamptz,dispatched_worker_id text,dispatched_fencing_token bigint,
 outcome jsonb,evidence_digest text CHECK(evidence_digest~'^[a-f0-9]{64}$'),reason_code text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(organization_id,id),UNIQUE(organization_id,plan_id,operation_id),
 FOREIGN KEY(organization_id,plan_id) REFERENCES configuration_plans(organization_id,id),
 FOREIGN KEY(organization_id,actor_id) REFERENCES memberships(organization_id,principal_id),
 FOREIGN KEY(organization_id,grant_id) REFERENCES configuration_grants(organization_id,id),
 FOREIGN KEY(organization_id,kind,target_id,resource_version) REFERENCES configuration_resource_bindings(organization_id,kind,target_id,resource_version),
 FOREIGN KEY(organization_id,task_id) REFERENCES tasks(organization_id,id),
 CHECK((kind='model' AND task_id IS NOT NULL) OR (kind='mcp' AND task_id IS NULL)),
 CHECK((dispatch_started_at IS NULL AND dispatched_worker_id IS NULL AND dispatched_fencing_token IS NULL)
    OR (dispatch_started_at IS NOT NULL AND dispatched_worker_id IS NOT NULL AND octet_length(dispatched_worker_id)>0 AND dispatched_fencing_token IS NOT NULL AND dispatched_fencing_token>0)),
 CHECK((outcome IS NULL AND evidence_digest IS NULL) OR (outcome IS NOT NULL AND jsonb_typeof(outcome)='object' AND evidence_digest IS NOT NULL)),
 CHECK((status IN('queued','failed','cancelled') AND dispatch_started_at IS NULL AND outcome IS NULL)
    OR (status IN('sent','unknown') AND dispatch_started_at IS NOT NULL AND outcome IS NULL)
    OR (status='response_received' AND dispatch_started_at IS NOT NULL AND outcome IS NOT NULL)),
 CHECK((worker_id='' AND lease_until IS NULL) OR (octet_length(worker_id) BETWEEN 1 AND 128 AND lease_until IS NOT NULL AND fencing_token>0)),
 CHECK(actor_kind<>'service' OR (admitted_role='member' AND grant_id IS NOT NULL)),
 CHECK(octet_length(body::text)<=32768 AND (outcome IS NULL OR octet_length(outcome::text)<=8192))
);
CREATE INDEX configuration_verification_claim ON configuration_verifications(created_at,id) WHERE kind='mcp' AND status='queued';
CREATE FUNCTION guard_configuration_verification() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF to_jsonb(NEW)-ARRAY['status','version','worker_id','lease_until','fencing_token','dispatch_started_at','dispatched_worker_id','dispatched_fencing_token','outcome','evidence_digest','reason_code','updated_at']
    IS DISTINCT FROM to_jsonb(OLD)-ARRAY['status','version','worker_id','lease_until','fencing_token','dispatch_started_at','dispatched_worker_id','dispatched_fencing_token','outcome','evidence_digest','reason_code','updated_at']
  OR NEW.version<>OLD.version OR NEW.fencing_token<OLD.fencing_token
  OR (OLD.dispatch_started_at IS NOT NULL AND (NEW.dispatch_started_at IS DISTINCT FROM OLD.dispatch_started_at
    OR NEW.dispatched_worker_id IS DISTINCT FROM OLD.dispatched_worker_id OR NEW.dispatched_fencing_token IS DISTINCT FROM OLD.dispatched_fencing_token))
  OR (OLD.outcome IS NOT NULL AND (NEW.outcome IS DISTINCT FROM OLD.outcome OR NEW.evidence_digest IS DISTINCT FROM OLD.evidence_digest))
  OR NOT ((OLD.status='queued' AND NEW.status IN('queued','sent','failed','cancelled'))
    OR (OLD.status='sent' AND NEW.status IN('sent','unknown','response_received'))
    OR (OLD.status='unknown' AND NEW.status IN('unknown','response_received'))
    OR (OLD.status IN('response_received','failed','cancelled') AND NEW.status=OLD.status)) THEN
  RAISE EXCEPTION 'verification cannot rewrite intent, resend, rewind or replace a receipt' USING ERRCODE='23514';END IF;
 IF NEW.status IN('sent','unknown','response_received') AND NEW.dispatch_started_at IS NULL THEN
  RAISE EXCEPTION 'verification requires a durable dispatch marker' USING ERRCODE='23514';END IF;
 NEW.version=OLD.version+1;NEW.updated_at=GREATEST(clock_timestamp(),OLD.updated_at+interval '1 microsecond');RETURN NEW;
END $$;
CREATE TRIGGER configuration_verification_guard BEFORE UPDATE ON configuration_verifications FOR EACH ROW EXECUTE FUNCTION guard_configuration_verification();
CREATE TRIGGER configuration_verification_no_delete BEFORE DELETE ON configuration_verifications FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE TABLE configuration_audit (
 organization_id text NOT NULL REFERENCES organizations(id),id text NOT NULL,actor_type text NOT NULL CHECK(actor_type IN('principal','trusted_host','worker')),
 actor_id text,object_type text NOT NULL,object_id text NOT NULL,action text NOT NULL,
 digest text NOT NULL CHECK(digest~'^[a-f0-9]{64}$'),reason text NOT NULL CHECK(octet_length(reason) BETWEEN 1 AND 4096),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),PRIMARY KEY(organization_id,id)
);
CREATE INDEX configuration_audit_listing ON configuration_audit(organization_id,created_at DESC,id DESC);
CREATE TRIGGER configuration_audit_immutable BEFORE UPDATE OR DELETE ON configuration_audit FOR EACH ROW EXECUTE FUNCTION protect_ledger_history();

CREATE FUNCTION bump_configuration_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN UPDATE configuration_revisions SET revision=revision+1 WHERE organization_id=NEW.organization_id;RETURN NEW;END $$;
CREATE TRIGGER model_configuration_revision AFTER INSERT OR UPDATE OF active ON model_connections FOR EACH ROW EXECUTE FUNCTION bump_configuration_revision();
CREATE TRIGGER tool_configuration_revision AFTER INSERT OR UPDATE OF active ON tool_connections FOR EACH ROW EXECUTE FUNCTION bump_configuration_revision();
CREATE TRIGGER mcp_configuration_revision AFTER INSERT OR UPDATE OF active ON mcp_connections FOR EACH ROW EXECUTE FUNCTION bump_configuration_revision();
CREATE TRIGGER destination_configuration_revision AFTER INSERT OR UPDATE OF active ON configuration_destinations FOR EACH ROW EXECUTE FUNCTION bump_configuration_revision();
CREATE TRIGGER purpose_configuration_revision AFTER INSERT OR UPDATE OF active ON credential_purpose_profiles FOR EACH ROW EXECUTE FUNCTION bump_configuration_revision();

-- These small SQL validators fail closed for absent keys and JSON null. They
-- validate persisted intent, not a caller-supplied assertion of authority.
CREATE FUNCTION configuration_json_strings(value jsonb,names text[],maximum integer) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE name text;
BEGIN
 IF jsonb_typeof(value) IS DISTINCT FROM 'object' THEN RETURN false;END IF;
 FOREACH name IN ARRAY names LOOP
  IF jsonb_typeof(value->name) IS DISTINCT FROM 'string' OR octet_length(value->>name) NOT BETWEEN 1 AND maximum OR btrim(value->>name)='' THEN RETURN false;END IF;
 END LOOP;
 RETURN true;
END $$;
CREATE FUNCTION configuration_json_integer(value jsonb,minimum bigint,maximum bigint) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
 SELECT CASE WHEN jsonb_typeof(value)='number' AND octet_length(value::text)<=20 AND value::text~'^(0|[1-9][0-9]*)$'
 THEN value::text::numeric BETWEEN minimum AND maximum ELSE false END
$$;
CREATE FUNCTION configuration_json_decimal(value jsonb,minimum bigint,maximum bigint) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
 SELECT CASE WHEN jsonb_typeof(value)='string' AND octet_length(value#>>'{}')<=19 AND (value#>>'{}')~'^(0|[1-9][0-9]*)$'
 THEN (value#>>'{}')::numeric BETWEEN minimum AND maximum ELSE false END
$$;
CREATE FUNCTION configuration_scope_is_valid(org text,scope jsonb,require_active boolean) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM configuration_destinations d JOIN credential_purpose_profiles p
 ON p.organization_id=d.organization_id AND p.id=d.credential_purpose_id AND p.version=d.credential_purpose_version
 WHERE d.organization_id=org AND (NOT require_active OR (d.active AND p.active)) AND scope=jsonb_build_object(
 'destination_id',d.id,'destination_version',d.version,'kind',d.kind,'target_id',d.target_id,
 'credential_purpose_id',d.credential_purpose_id,'credential_purpose_version',d.credential_purpose_version))
$$;

CREATE FUNCTION guard_new_configuration_purpose() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE latest credential_purpose_profiles;
BEGIN
 IF NOT configuration_json_strings(NEW.profile,ARRAY['id','organization_id','kind','endpoint_digest','credential_environment'],128)
 OR NOT configuration_json_integer(NEW.profile->'version',1,2147483647) THEN
  RAISE EXCEPTION 'credential purpose requires typed exact identity' USING ERRCODE='23514';END IF;
 SELECT * INTO latest FROM credential_purpose_profiles WHERE organization_id=NEW.organization_id AND id=NEW.id ORDER BY version DESC LIMIT 1;
 IF (FOUND AND (NEW.version<>latest.version+1 OR NEW.kind<>latest.kind)) OR (NOT FOUND AND NEW.version<>1) THEN
  RAISE EXCEPTION 'credential purpose requires the next immutable version' USING ERRCODE='23514';END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER credential_purpose_initial BEFORE INSERT ON credential_purpose_profiles FOR EACH ROW EXECUTE FUNCTION guard_new_configuration_purpose();

CREATE FUNCTION guard_new_configuration_destination() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE purpose credential_purpose_profiles;latest configuration_destinations;d jsonb;endpoint text;
BEGIN
 d=NEW.profile->'destination';
 IF NOT configuration_json_strings(d,ARRAY['id','organization_id','kind','target_id','credential_purpose_id','name'],128)
 OR NOT configuration_json_integer(d->'version',1,2147483647) OR NOT configuration_json_integer(d->'credential_purpose_version',1,2147483647)
 OR NOT configuration_json_decimal(d->'max_verify_charge_microusd',0,1000000000000)
 OR jsonb_typeof(d->'requires_approval') IS DISTINCT FROM 'boolean' OR jsonb_typeof(d->'discovery_unmetered_approved') IS DISTINCT FROM 'boolean' THEN
  RAISE EXCEPTION 'destination requires typed bounded descriptor' USING ERRCODE='23514';END IF;
 IF NEW.kind='model' THEN
  IF NOT configuration_json_strings(NEW.profile->'model',ARRAY['id','organization_id','model','endpoint'],2048)
  OR NEW.profile#>>'{model,id}' IS DISTINCT FROM NEW.target_id OR NEW.profile#>>'{model,organization_id}' IS DISTINCT FROM NEW.organization_id
  OR NEW.profile#>>'{model,model}' IS DISTINCT FROM d->>'model' OR d->'discovery_unmetered_approved' IS DISTINCT FROM 'false'::jsonb
  OR NOT configuration_json_integer(NEW.profile#>'{model,max_charge_microusd}',0,(d->>'max_verify_charge_microusd')::bigint) THEN
   RAISE EXCEPTION 'model destination requires exact approved template and charge' USING ERRCODE='23514';END IF;
  endpoint=NEW.profile#>>'{model,endpoint}';
 ELSE
  IF NOT configuration_json_strings(NEW.profile->'mcp',ARRAY['endpoint'],2048)
  OR NOT configuration_json_integer(NEW.profile#>'{mcp,timeout_seconds}',1,30)
  OR jsonb_typeof(NEW.profile#>'{mcp,allow_private_network}') IS DISTINCT FROM 'boolean'
  OR d->>'max_verify_charge_microusd' IS DISTINCT FROM '0' OR COALESCE(d->>'model','')<>'' THEN
   RAISE EXCEPTION 'MCP discovery requires explicit unmetered approved profile' USING ERRCODE='23514';END IF;
  endpoint=NEW.profile#>>'{mcp,endpoint}';
 END IF;
 SELECT * INTO purpose FROM credential_purpose_profiles WHERE organization_id=NEW.organization_id AND id=NEW.credential_purpose_id AND version=NEW.credential_purpose_version;
 IF NOT FOUND OR NOT purpose.active OR purpose.kind<>NEW.kind OR purpose.endpoint_digest<>encode(sha256(convert_to(endpoint,'UTF8')),'hex') THEN
  RAISE EXCEPTION 'destination and credential purpose must match exact endpoint and kind' USING ERRCODE='23514';END IF;
 SELECT * INTO latest FROM configuration_destinations WHERE organization_id=NEW.organization_id AND id=NEW.id ORDER BY version DESC LIMIT 1;
 IF (FOUND AND (NEW.version<>latest.version+1 OR NEW.kind<>latest.kind OR NEW.target_id<>latest.target_id OR NEW.credential_purpose_id<>latest.credential_purpose_id))
 OR (NOT FOUND AND NEW.version<>1) THEN
  RAISE EXCEPTION 'destination requires the next version of its fixed target and purpose' USING ERRCODE='23514';END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER configuration_destination_initial BEFORE INSERT ON configuration_destinations FOR EACH ROW EXECUTE FUNCTION guard_new_configuration_destination();

-- Fresh liveness is used only for admissions. Receipt guards below deliberately
-- do not call it: revocation cannot erase an already sent factual observation.
CREATE FUNCTION configuration_grant_is_live(org text,grant_id text,subject text,action text) RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE g configuration_grants;expected_subject text=subject;next_id text=grant_id;visited text[]='{}';level integer=0;valid boolean;
BEGIN
 LOOP
  IF next_id=ANY(visited) OR level>4 THEN RETURN false;END IF;
  visited=array_append(visited,next_id);
  SELECT * INTO g FROM configuration_grants WHERE organization_id=org AND id=next_id;
  IF NOT FOUND OR g.revoked OR g.expires_at<=clock_timestamp() OR g.subject_id IS DISTINCT FROM expected_subject
  OR NOT COALESCE(g.body->'actions' ? action,false) THEN RETURN false;END IF;
  SELECT EXISTS(SELECT 1 FROM memberships im JOIN principals ip ON ip.id=im.principal_id
   JOIN memberships sm ON sm.organization_id=im.organization_id AND sm.principal_id=g.subject_id JOIN principals sp ON sp.id=sm.principal_id
   JOIN organizations o ON o.id=im.organization_id JOIN organization_login_policies policy ON policy.organization_id=o.id
   WHERE im.organization_id=org AND im.principal_id=g.issuer_id AND im.active AND ip.active AND sm.active AND sp.active AND o.active
   AND im.role<>'viewer' AND sm.role<>'viewer' AND ip.kind IN('human','service') AND sp.kind IN('human','service')
   AND (ip.kind='human' OR (policy.service_access_enabled AND ip.service_organization_id=org))
   AND (sp.kind='human' OR (policy.service_access_enabled AND sp.service_organization_id=org))
   AND g.issuer_authority_epoch=im.security_epoch AND g.subject_authority_epoch=sm.security_epoch
   AND g.issuer_policy_epoch=current_principal_policy_epoch(org,g.issuer_id) AND g.subject_policy_epoch=current_principal_policy_epoch(org,g.subject_id)
   AND (g.parent_grant_id IS NOT NULL OR (ip.kind='human' AND im.role='owner'))) INTO valid;
  IF NOT valid THEN RETURN false;END IF;
  IF g.parent_grant_id IS NULL THEN RETURN g.depth=0;END IF;
  expected_subject=g.issuer_id;next_id=g.parent_grant_id;level=level+1;
 END LOOP;
END $$;

CREATE FUNCTION guard_new_configuration_grant() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE parent configuration_grants;item jsonb;valid boolean;name text;
BEGIN
 IF NEW.version<>1 OR NEW.revoked OR NEW.used_plans<>0 OR NEW.used_verifications<>0 OR NEW.allocated_charge_microusd<>0
 OR NEW.expires_at<=clock_timestamp() OR NOT configuration_json_strings(NEW.body,ARRAY['id','organization_id','issuer_id','subject_id','audience'],128)
 OR NEW.body->>'audience' IS DISTINCT FROM 'platform.configuration.v1'
 OR (NEW.body?'parent_grant_id' AND jsonb_typeof(NEW.body->'parent_grant_id') IS DISTINCT FROM 'string')
 OR COALESCE(NEW.body->>'parent_grant_id','') IS DISTINCT FROM COALESCE(NEW.parent_grant_id,'')
 OR NEW.body->'depth' IS DISTINCT FROM to_jsonb(NEW.depth) OR NEW.body->'max_plans' IS DISTINCT FROM to_jsonb(NEW.max_plans)
 OR NEW.body->'max_verifications' IS DISTINCT FROM to_jsonb(NEW.max_verifications)
 OR NEW.body->'max_allocated_charge_microusd' IS DISTINCT FROM to_jsonb(NEW.max_allocated_charge_microusd::text)
 OR jsonb_typeof(NEW.body->'actions') IS DISTINCT FROM 'array' OR jsonb_typeof(NEW.body->'scopes') IS DISTINCT FROM 'array' THEN
  RAISE EXCEPTION 'grant requires typed exact initial consent and limits' USING ERRCODE='23514';END IF;
 IF jsonb_array_length(NEW.body->'actions') NOT BETWEEN 1 AND 6 OR jsonb_array_length(NEW.body->'scopes') NOT BETWEEN 1 AND 16 THEN
  RAISE EXCEPTION 'grant action and scope sets must be bounded' USING ERRCODE='23514';END IF;
 FOR item IN SELECT value FROM jsonb_array_elements(NEW.body->'actions') LOOP
  IF jsonb_typeof(item) IS DISTINCT FROM 'string' OR item#>>'{}' NOT IN('inspect','plan','validate','apply','verify','delegate') THEN
   RAISE EXCEPTION 'grant contains unknown action' USING ERRCODE='23514';END IF;
 END LOOP;
 IF (SELECT count(*)<>count(DISTINCT value) FROM jsonb_array_elements(NEW.body->'actions'))
 OR (SELECT count(*)<>count(DISTINCT value->>'destination_id') FROM jsonb_array_elements(NEW.body->'scopes')) THEN
  RAISE EXCEPTION 'grant cannot duplicate actions or destination versions' USING ERRCODE='23514';END IF;
 FOR item IN SELECT value FROM jsonb_array_elements(NEW.body->'scopes') LOOP
  IF NOT configuration_scope_is_valid(NEW.organization_id,item,true) THEN
   RAISE EXCEPTION 'grant scope must be an exact active destination and purpose' USING ERRCODE='23514';END IF;
 END LOOP;
 SELECT EXISTS(SELECT 1 FROM memberships im JOIN principals ip ON ip.id=im.principal_id
  JOIN memberships sm ON sm.organization_id=im.organization_id AND sm.principal_id=NEW.subject_id JOIN principals sp ON sp.id=sm.principal_id
  JOIN organizations o ON o.id=im.organization_id JOIN organization_login_policies policy ON policy.organization_id=o.id
  WHERE im.organization_id=NEW.organization_id AND im.principal_id=NEW.issuer_id AND im.active AND ip.active AND sm.active AND sp.active AND o.active
  AND im.role<>'viewer' AND sm.role<>'viewer' AND ip.kind IN('human','service') AND sp.kind IN('human','service')
  AND (ip.kind='human' OR (policy.service_access_enabled AND ip.service_organization_id=o.id))
  AND (sp.kind='human' OR (policy.service_access_enabled AND sp.service_organization_id=o.id))
  AND (NEW.parent_grant_id IS NOT NULL OR (ip.kind='human' AND im.role='owner'))) INTO valid;
 IF NOT valid THEN RAISE EXCEPTION 'grant requires current local issuer and subject authority' USING ERRCODE='23514';END IF;
 IF NEW.parent_grant_id IS NOT NULL THEN
  SELECT * INTO parent FROM configuration_grants WHERE organization_id=NEW.organization_id AND id=NEW.parent_grant_id;
  IF NOT FOUND OR NEW.id=NEW.parent_grant_id OR NEW.depth<>parent.depth+1 OR NEW.depth>4
  OR NOT configuration_grant_is_live(NEW.organization_id,parent.id,NEW.issuer_id,'delegate')
  OR NOT(parent.body->'actions' @> NEW.body->'actions') OR NOT(parent.body->'scopes' @> NEW.body->'scopes')
  OR NEW.expires_at>parent.expires_at OR NEW.max_plans>parent.max_plans OR NEW.max_verifications>parent.max_verifications
  OR NEW.max_allocated_charge_microusd>parent.max_allocated_charge_microusd THEN
   RAISE EXCEPTION 'child grant must attenuate the actual live parent authority' USING ERRCODE='23514';END IF;
 END IF;
 NEW.issuer_authority_epoch=current_member_epoch(NEW.organization_id,NEW.issuer_id);
 NEW.subject_authority_epoch=current_member_epoch(NEW.organization_id,NEW.subject_id);
 NEW.issuer_policy_epoch=current_principal_policy_epoch(NEW.organization_id,NEW.issuer_id);
 NEW.subject_policy_epoch=current_principal_policy_epoch(NEW.organization_id,NEW.subject_id);
 RETURN NEW;
END $$;
CREATE TRIGGER configuration_grant_initial BEFORE INSERT ON configuration_grants FOR EACH ROW EXECUTE FUNCTION guard_new_configuration_grant();

CREATE OR REPLACE FUNCTION guard_configuration_resource_binding() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE d configuration_destinations;p credential_purpose_profiles;actual jsonb;expected jsonb;
BEGIN
 SELECT * INTO d FROM configuration_destinations WHERE organization_id=NEW.organization_id AND id=NEW.destination_id AND version=NEW.destination_version;
 IF NOT FOUND OR NOT d.active OR d.kind IS DISTINCT FROM NEW.kind OR d.target_id IS DISTINCT FROM NEW.target_id
 OR d.credential_purpose_id IS DISTINCT FROM NEW.credential_purpose_id OR d.credential_purpose_version IS DISTINCT FROM NEW.credential_purpose_version THEN
  RAISE EXCEPTION 'resource binding requires exact active destination' USING ERRCODE='23514';END IF;
 SELECT * INTO p FROM credential_purpose_profiles WHERE organization_id=NEW.organization_id AND id=NEW.credential_purpose_id AND version=NEW.credential_purpose_version;
 IF NOT FOUND OR NOT p.active OR p.kind<>NEW.kind THEN RAISE EXCEPTION 'resource binding requires active matching purpose' USING ERRCODE='23514';END IF;
 IF NEW.kind='model' THEN
  SELECT profile-ARRAY['active','created_at'] INTO actual FROM model_connections WHERE organization_id=NEW.organization_id AND id=NEW.target_id AND version=NEW.resource_version;
  expected=(d.profile->'model')-ARRAY['active','created_at'];
  expected=expected||jsonb_build_object('version',NEW.resource_version,'credential_environment',p.profile->>'credential_environment');
 ELSE
  SELECT profile INTO actual FROM mcp_connections WHERE organization_id=NEW.organization_id AND id=NEW.target_id AND version=NEW.resource_version;
  expected=d.profile->'mcp';
 END IF;
 IF actual IS NULL OR actual IS DISTINCT FROM expected THEN
  RAISE EXCEPTION 'installed resource must equal the approved destination and credential purpose' USING ERRCODE='23514';END IF;
 RETURN NEW;
END $$;

CREATE FUNCTION configuration_actor_is_live(org text,actor text,kind text,admitted text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM memberships m JOIN principals p ON p.id=m.principal_id JOIN organizations o ON o.id=m.organization_id
 JOIN organization_login_policies policy ON policy.organization_id=o.id WHERE m.organization_id=org AND m.principal_id=actor
 AND m.active AND p.active AND o.active AND p.kind=kind AND m.role=admitted AND admitted IN('owner','admin','member')
 AND (kind='human' OR (kind='service' AND admitted='member' AND policy.service_access_enabled AND p.service_organization_id=org)))
$$;
CREATE FUNCTION guard_new_configuration_plan() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE operation jsonb;difference jsonb;ids text[]='{}';targets text[]='{}';revision_now bigint;g configuration_grants;
BEGIN
 IF NEW.version<>1 OR NEW.status<>'planned' OR NEW.applied_at IS NOT NULL OR NEW.applied_revision IS NOT NULL OR NEW.receipt_id IS NOT NULL
 OR NEW.expires_at<=clock_timestamp() OR NOT configuration_actor_is_live(NEW.organization_id,NEW.actor_id,NEW.actor_kind,NEW.admitted_role)
 OR NOT configuration_json_strings(NEW.body,ARRAY['id','organization_id','actor_id','digest'],128)
 OR NOT configuration_json_strings(NEW.body,ARRAY['reason'],4096)
 OR NEW.body->'base_revision' IS DISTINCT FROM to_jsonb(NEW.base_revision::text)
 OR COALESCE(NEW.body->>'grant_id','') IS DISTINCT FROM COALESCE(NEW.grant_id,'')
 OR (NEW.body?'grant_id' AND jsonb_typeof(NEW.body->'grant_id') IS DISTINCT FROM 'string')
 OR jsonb_typeof(NEW.body->'operations') IS DISTINCT FROM 'array' OR jsonb_typeof(NEW.body->'diff') IS DISTINCT FROM 'array' THEN
  RAISE EXCEPTION 'plan requires typed immutable intent and fresh initial admission' USING ERRCODE='23514';END IF;
 IF jsonb_array_length(NEW.body->'operations') NOT BETWEEN 1 AND 8 OR jsonb_array_length(NEW.body->'diff')<>jsonb_array_length(NEW.body->'operations') THEN
  RAISE EXCEPTION 'plan operations and complete diff must be bounded' USING ERRCODE='23514';END IF;
 SELECT revision INTO revision_now FROM configuration_revisions WHERE organization_id=NEW.organization_id;
 IF NEW.base_revision IS DISTINCT FROM revision_now THEN RAISE EXCEPTION 'plan requires current configuration revision' USING ERRCODE='23514';END IF;
 IF NEW.grant_id IS NULL THEN
  IF NEW.actor_kind<>'human' OR NEW.admitted_role NOT IN('owner','admin') THEN RAISE EXCEPTION 'non-administrator plan requires explicit grant' USING ERRCODE='23514';END IF;
 ELSE
  SELECT * INTO g FROM configuration_grants WHERE organization_id=NEW.organization_id AND id=NEW.grant_id;
  IF NOT FOUND OR NEW.expires_at>g.expires_at OR NOT configuration_grant_is_live(NEW.organization_id,NEW.grant_id,NEW.actor_id,'plan') THEN
   RAISE EXCEPTION 'plan requires exact live grant and expiry' USING ERRCODE='23514';END IF;
 END IF;
 FOR operation IN SELECT value FROM jsonb_array_elements(NEW.body->'operations') LOOP
  IF NOT configuration_json_strings(operation,ARRAY['id'],128) OR NOT configuration_json_integer(operation->'expected_resource_version',0,2147483646)
  OR jsonb_typeof(operation->'active') IS DISTINCT FROM 'boolean' OR NOT configuration_scope_is_valid(NEW.organization_id,operation->'scope',true)
  OR operation->>'id'=ANY(ids) OR ((operation#>>'{scope,kind}')||':'||(operation#>>'{scope,target_id}'))=ANY(targets)
  OR (NEW.grant_id IS NOT NULL AND NOT(g.body->'scopes' @> jsonb_build_array(operation->'scope'))) THEN
   RAISE EXCEPTION 'plan requires unique exact authorized operations' USING ERRCODE='23514';END IF;
  ids=array_append(ids,operation->>'id');targets=array_append(targets,(operation#>>'{scope,kind}')||':'||(operation#>>'{scope,target_id}'));
  SELECT value INTO difference FROM jsonb_array_elements(NEW.body->'diff') WHERE value->>'operation_id'=operation->>'id';
  IF NOT FOUND OR (SELECT count(*) FROM jsonb_array_elements(NEW.body->'diff') WHERE value->>'operation_id'=operation->>'id')<>1
  OR difference->>'kind' IS DISTINCT FROM operation#>>'{scope,kind}' OR difference->>'target_id' IS DISTINCT FROM operation#>>'{scope,target_id}'
  OR difference->'expected_version' IS DISTINCT FROM operation->'expected_resource_version'
  OR difference->'new_version' IS DISTINCT FROM to_jsonb((operation->>'expected_resource_version')::integer+1)
  OR difference->'active' IS DISTINCT FROM operation->'active' OR jsonb_typeof(difference->'may_stop_pinned_tasks') IS DISTINCT FROM 'boolean' THEN
   RAISE EXCEPTION 'plan diff must describe every exact operation' USING ERRCODE='23514';END IF;
 END LOOP;
 NEW.actor_authority_epoch=current_member_epoch(NEW.organization_id,NEW.actor_id);
 NEW.actor_policy_epoch=current_principal_policy_epoch(NEW.organization_id,NEW.actor_id);
 RETURN NEW;
END $$;
CREATE TRIGGER configuration_plan_initial BEFORE INSERT ON configuration_plans FOR EACH ROW EXECUTE FUNCTION guard_new_configuration_plan();

CREATE OR REPLACE FUNCTION guard_configuration_plan() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE current_revision bigint;operation jsonb;difference jsonb;valid boolean;approval_required boolean=false;
BEGIN
 IF to_jsonb(NEW)-ARRAY['version','status','applied_at','applied_revision','receipt_id']
    IS DISTINCT FROM to_jsonb(OLD)-ARRAY['version','status','applied_at','applied_revision','receipt_id']
 OR OLD.status<>'planned' OR NEW.status<>'applied' OR NEW.version<>OLD.version OR OLD.expires_at<=clock_timestamp()
 OR NOT configuration_actor_is_live(OLD.organization_id,OLD.actor_id,OLD.actor_kind,OLD.admitted_role)
 OR OLD.actor_authority_epoch IS DISTINCT FROM current_member_epoch(OLD.organization_id,OLD.actor_id)
 OR OLD.actor_policy_epoch IS DISTINCT FROM current_principal_policy_epoch(OLD.organization_id,OLD.actor_id)
 OR (OLD.grant_id IS NOT NULL AND NOT configuration_grant_is_live(OLD.organization_id,OLD.grant_id,OLD.actor_id,'apply')) THEN
  RAISE EXCEPTION 'plan intent is immutable and applies once under current authority' USING ERRCODE='23514';END IF;
 SELECT revision INTO current_revision FROM configuration_revisions WHERE organization_id=OLD.organization_id;
 IF NEW.applied_revision IS NULL OR NEW.applied_revision<=OLD.base_revision OR NEW.applied_revision IS DISTINCT FROM current_revision
 OR NEW.receipt_id IS NULL OR octet_length(NEW.receipt_id) NOT BETWEEN 1 AND 128 THEN
  RAISE EXCEPTION 'application requires exact committed revision and receipt identity' USING ERRCODE='23514';END IF;
 FOR operation IN SELECT value FROM jsonb_array_elements(OLD.body->'operations') LOOP
  IF NOT configuration_scope_is_valid(OLD.organization_id,operation->'scope',true) THEN RAISE EXCEPTION 'application requires current approved scope' USING ERRCODE='23514';END IF;
  SELECT value INTO difference FROM jsonb_array_elements(OLD.body->'diff') WHERE value->>'operation_id'=operation->>'id';
  SELECT EXISTS(SELECT 1 FROM configuration_resource_bindings b WHERE b.organization_id=OLD.organization_id AND b.kind=operation#>>'{scope,kind}'
   AND b.target_id=operation#>>'{scope,target_id}' AND to_jsonb(b.resource_version)=difference->'new_version'
   AND b.destination_id=operation#>>'{scope,destination_id}' AND to_jsonb(b.destination_version)=operation#>'{scope,destination_version}'
   AND b.credential_purpose_id=operation#>>'{scope,credential_purpose_id}' AND to_jsonb(b.credential_purpose_version)=operation#>'{scope,credential_purpose_version}'
   AND ((b.kind='model' AND EXISTS(SELECT 1 FROM model_connections r WHERE r.organization_id=b.organization_id AND r.id=b.target_id AND r.version=b.resource_version AND to_jsonb(r.active)=operation->'active'))
     OR (b.kind='mcp' AND EXISTS(SELECT 1 FROM mcp_connections r WHERE r.organization_id=b.organization_id AND r.id=b.target_id AND r.version=b.resource_version AND to_jsonb(r.active)=operation->'active')))) INTO valid;
  IF NOT valid THEN RAISE EXCEPTION 'application receipt requires exact installed operation result' USING ERRCODE='23514';END IF;
  IF EXISTS(SELECT 1 FROM configuration_destinations d WHERE d.organization_id=OLD.organization_id AND d.id=operation#>>'{scope,destination_id}'
   AND to_jsonb(d.version)=operation#>'{scope,destination_version}' AND d.profile#>'{destination,requires_approval}'='true'::jsonb) THEN approval_required=true;END IF;
 END LOOP;
 SELECT EXISTS(SELECT 1 FROM configuration_validations v WHERE v.organization_id=OLD.organization_id AND v.plan_id=OLD.id
  AND v.body->'valid'='true'::jsonb AND v.body->>'plan_digest'=OLD.digest AND v.body->'plan_version'=to_jsonb(OLD.version)
  AND v.body->'current_revision'=to_jsonb(OLD.base_revision::text)
  AND (NOT approval_required OR EXISTS(SELECT 1 FROM configuration_approvals a JOIN memberships m ON m.organization_id=a.organization_id AND m.principal_id=a.principal_id
   JOIN principals actor ON actor.id=m.principal_id WHERE a.organization_id=v.organization_id AND a.plan_id=v.plan_id AND a.validation_id=v.id
   AND m.active AND actor.active AND m.role='owner' AND actor.kind='human' AND a.authority_epoch=m.security_epoch
   AND a.policy_epoch=current_principal_policy_epoch(a.organization_id,a.principal_id)))) INTO valid;
 IF NOT valid THEN RAISE EXCEPTION 'application requires exact valid validation and current required owner approval' USING ERRCODE='23514';END IF;
 NEW.version=OLD.version+1;NEW.applied_at=clock_timestamp();RETURN NEW;
END $$;

CREATE FUNCTION guard_new_configuration_validation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE p configuration_plans;decision jsonb;revision_now bigint;
BEGIN
 SELECT * INTO p FROM configuration_plans WHERE organization_id=NEW.organization_id AND id=NEW.plan_id;
 IF NOT FOUND OR NOT configuration_json_strings(NEW.body,ARRAY['id','organization_id','plan_id','plan_digest'],128)
 OR NEW.body->>'id' IS DISTINCT FROM NEW.id OR NEW.body->>'organization_id' IS DISTINCT FROM NEW.organization_id
 OR NEW.body->>'plan_id' IS DISTINCT FROM NEW.plan_id OR NEW.body->>'plan_digest' IS DISTINCT FROM p.digest
 OR NEW.body->'plan_version' IS DISTINCT FROM to_jsonb(p.version) OR NEW.body->'base_revision' IS DISTINCT FROM to_jsonb(p.base_revision::text)
 OR NOT configuration_json_decimal(NEW.body->'current_revision',1,9223372036854775807)
 OR jsonb_typeof(NEW.body->'valid') IS DISTINCT FROM 'boolean' OR jsonb_typeof(NEW.body->'decision') IS DISTINCT FROM 'object'
 OR NEW.body->'diff' IS DISTINCT FROM p.body->'diff' OR octet_length(NEW.body::text)>65536 THEN
  RAISE EXCEPTION 'validation must bind exact plan snapshot and diff' USING ERRCODE='23514';END IF;
 decision=NEW.body->'decision';
 SELECT revision INTO revision_now FROM configuration_revisions WHERE organization_id=NEW.organization_id;
 IF NOT configuration_json_strings(decision,ARRAY['id','organization_id','action','target_id','outcome'],128)
 OR decision->>'organization_id' IS DISTINCT FROM NEW.organization_id
 OR decision->>'action' IS DISTINCT FROM 'apply' OR decision->>'target_id' IS DISTINCT FROM NEW.plan_id
 OR COALESCE(decision->>'grant_id','') IS DISTINCT FROM COALESCE(p.grant_id,'')
 OR decision->>'outcome' NOT IN('allow','deny','requires_approval')
 OR jsonb_typeof(decision->'reason_codes') IS DISTINCT FROM 'array' OR jsonb_typeof(decision->'obligations') IS DISTINCT FROM 'array'
 OR decision->'configuration_revision' IS DISTINCT FROM NEW.body->'current_revision'
 OR NEW.body->'current_revision' IS DISTINCT FROM to_jsonb(revision_now::text)
 OR (NEW.body->'valid'='true'::jsonb AND (decision->>'outcome'='deny' OR p.status<>'planned'
   OR p.expires_at<=clock_timestamp() OR p.base_revision IS DISTINCT FROM revision_now)) THEN
  RAISE EXCEPTION 'validation requires typed policy explanation, not cached authority' USING ERRCODE='23514';END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER configuration_validation_initial BEFORE INSERT ON configuration_validations FOR EACH ROW EXECUTE FUNCTION guard_new_configuration_validation();
CREATE FUNCTION guard_new_configuration_approval() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE p configuration_plans;v configuration_validations;valid boolean;
BEGIN
 SELECT * INTO p FROM configuration_plans WHERE organization_id=NEW.organization_id AND id=NEW.plan_id;
 SELECT * INTO v FROM configuration_validations WHERE organization_id=NEW.organization_id AND id=NEW.validation_id AND plan_id=NEW.plan_id;
 IF NOT FOUND OR p.status IS DISTINCT FROM 'planned' OR p.expires_at<=clock_timestamp() OR v.body->'valid' IS DISTINCT FROM 'true'::jsonb
 OR v.body->'plan_version' IS DISTINCT FROM to_jsonb(p.version) OR v.body->>'plan_digest' IS DISTINCT FROM p.digest
 OR NOT configuration_json_strings(NEW.body,ARRAY['id','organization_id','plan_id','plan_digest','validation_id','principal_id'],128)
 OR NOT configuration_json_strings(NEW.body,ARRAY['reason'],4096) OR octet_length(NEW.body::text)>16384
 OR NEW.body->>'id' IS DISTINCT FROM NEW.id OR NEW.body->>'organization_id' IS DISTINCT FROM NEW.organization_id
 OR NEW.body->>'plan_id' IS DISTINCT FROM NEW.plan_id OR NEW.body->>'plan_digest' IS DISTINCT FROM p.digest
 OR NEW.body->>'validation_id' IS DISTINCT FROM NEW.validation_id OR NEW.body->>'principal_id' IS DISTINCT FROM NEW.principal_id THEN
  RAISE EXCEPTION 'approval must bind same valid validation and immutable plan' USING ERRCODE='23514';END IF;
 SELECT EXISTS(SELECT 1 FROM memberships m JOIN principals actor ON actor.id=m.principal_id JOIN organizations o ON o.id=m.organization_id
 WHERE m.organization_id=NEW.organization_id AND m.principal_id=NEW.principal_id AND m.active AND actor.active AND o.active AND m.role='owner' AND actor.kind='human') INTO valid;
 IF NOT valid THEN RAISE EXCEPTION 'configuration approval requires current human owner' USING ERRCODE='23514';END IF;
 NEW.authority_epoch=current_member_epoch(NEW.organization_id,NEW.principal_id);
 NEW.policy_epoch=current_principal_policy_epoch(NEW.organization_id,NEW.principal_id);
 RETURN NEW;
END $$;
CREATE TRIGGER configuration_approval_initial BEFORE INSERT ON configuration_approvals FOR EACH ROW EXECUTE FUNCTION guard_new_configuration_approval();

CREATE FUNCTION guard_new_configuration_verification() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE p configuration_plans;operation jsonb;difference jsonb;scope jsonb;g configuration_grants;valid boolean;
BEGIN
 IF NEW.status<>'queued' OR NEW.version<>1 OR NEW.worker_id<>'' OR NEW.lease_until IS NOT NULL OR NEW.fencing_token<>0
 OR NEW.dispatch_started_at IS NOT NULL OR NEW.dispatched_worker_id IS NOT NULL OR NEW.dispatched_fencing_token IS NOT NULL
 OR NEW.outcome IS NOT NULL OR NEW.evidence_digest IS NOT NULL OR NEW.reason_code<>''
 OR NOT configuration_actor_is_live(NEW.organization_id,NEW.actor_id,NEW.actor_kind,NEW.admitted_role)
 OR NOT configuration_json_strings(NEW.body,ARRAY['id','organization_id','plan_id','operation_id','actor_id','kind'],128)
 OR NOT configuration_json_decimal(NEW.body->'max_charge_microusd',0,1000000000000) THEN
  RAISE EXCEPTION 'verification requires exact queued initial admission' USING ERRCODE='23514';END IF;
 SELECT * INTO p FROM configuration_plans WHERE organization_id=NEW.organization_id AND id=NEW.plan_id;
 IF NOT FOUND OR p.status<>'applied' OR NEW.actor_id IS DISTINCT FROM p.actor_id OR NEW.grant_id IS DISTINCT FROM p.grant_id
 OR p.actor_authority_epoch IS DISTINCT FROM current_member_epoch(NEW.organization_id,p.actor_id)
 OR p.actor_policy_epoch IS DISTINCT FROM current_principal_policy_epoch(NEW.organization_id,p.actor_id) THEN
  RAISE EXCEPTION 'verification must retain applied plan actor and authority' USING ERRCODE='23514';END IF;
 IF NEW.grant_id IS NOT NULL AND NOT configuration_grant_is_live(NEW.organization_id,NEW.grant_id,NEW.actor_id,'verify') THEN
  RAISE EXCEPTION 'verification requires current exact delegated authority' USING ERRCODE='23514';END IF;
 SELECT value INTO operation FROM jsonb_array_elements(p.body->'operations') WHERE value->>'id'=NEW.operation_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'verification operation must belong to exact plan' USING ERRCODE='23514';END IF;
 SELECT value INTO difference FROM jsonb_array_elements(p.body->'diff') WHERE value->>'operation_id'=NEW.operation_id;
 scope=operation->'scope';
 IF NOT FOUND OR operation->'active' IS DISTINCT FROM 'true'::jsonb OR NOT configuration_scope_is_valid(NEW.organization_id,scope,true)
 OR scope->>'kind' IS DISTINCT FROM NEW.kind OR scope->>'target_id' IS DISTINCT FROM NEW.target_id
 OR difference->'new_version' IS DISTINCT FROM to_jsonb(NEW.resource_version)
 OR NEW.body->>'id' IS DISTINCT FROM NEW.id OR NEW.body->>'organization_id' IS DISTINCT FROM NEW.organization_id
 OR NEW.body->>'plan_id' IS DISTINCT FROM NEW.plan_id OR NEW.body->>'operation_id' IS DISTINCT FROM NEW.operation_id
 OR NEW.body->>'actor_id' IS DISTINCT FROM NEW.actor_id OR NEW.body->>'kind' IS DISTINCT FROM NEW.kind
 OR COALESCE(NEW.body->>'grant_id','') IS DISTINCT FROM COALESCE(NEW.grant_id,'')
 OR COALESCE(NEW.body->>'task_id','') IS DISTINCT FROM COALESCE(NEW.task_id,'')
 OR NEW.body->'resource_version' IS DISTINCT FROM to_jsonb(NEW.resource_version) OR NEW.body->'scope' IS DISTINCT FROM scope THEN
  RAISE EXCEPTION 'verification must bind exact plan operation and installed resource' USING ERRCODE='23514';END IF;
 IF NOT EXISTS(SELECT 1 FROM configuration_resource_bindings b WHERE b.organization_id=NEW.organization_id AND b.kind=NEW.kind AND b.target_id=NEW.target_id
 AND b.resource_version=NEW.resource_version AND b.destination_id=scope->>'destination_id'
 AND to_jsonb(b.destination_version)=scope->'destination_version' AND b.credential_purpose_id=scope->>'credential_purpose_id'
 AND to_jsonb(b.credential_purpose_version)=scope->'credential_purpose_version') THEN
  RAISE EXCEPTION 'verification requires exact approved installed binding' USING ERRCODE='23514';END IF;
 IF NEW.kind='model' THEN
  SELECT EXISTS(SELECT 1 FROM tasks t WHERE t.organization_id=NEW.organization_id AND t.id=NEW.task_id AND t.owner_id=NEW.actor_id
   AND t.model_connection_id=NEW.target_id AND t.model_connection_version=NEW.resource_version) INTO valid;
  IF NOT valid THEN RAISE EXCEPTION 'model probe requires exact actor and installed model task pin' USING ERRCODE='23514';END IF;
 ELSE
  IF NEW.body->>'max_charge_microusd' IS DISTINCT FROM '0' THEN RAISE EXCEPTION 'MCP discovery must be owner-approved unmetered' USING ERRCODE='23514';END IF;
 END IF;
 NEW.actor_authority_epoch=current_member_epoch(NEW.organization_id,NEW.actor_id);
 NEW.actor_policy_epoch=current_principal_policy_epoch(NEW.organization_id,NEW.actor_id);
 RETURN NEW;
END $$;
CREATE TRIGGER configuration_verification_initial BEFORE INSERT ON configuration_verifications FOR EACH ROW EXECUTE FUNCTION guard_new_configuration_verification();

CREATE OR REPLACE FUNCTION guard_configuration_verification() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE marker_valid boolean;
BEGIN
 IF to_jsonb(NEW)-ARRAY['status','version','worker_id','lease_until','fencing_token','dispatch_started_at','dispatched_worker_id','dispatched_fencing_token','outcome','evidence_digest','reason_code','updated_at']
    IS DISTINCT FROM to_jsonb(OLD)-ARRAY['status','version','worker_id','lease_until','fencing_token','dispatch_started_at','dispatched_worker_id','dispatched_fencing_token','outcome','evidence_digest','reason_code','updated_at']
 OR NEW.version<>OLD.version OR NEW.fencing_token<OLD.fencing_token OR (NEW.kind='mcp' AND NEW.fencing_token>OLD.fencing_token+1)
 OR NOT ((OLD.status='queued' AND NEW.status IN('queued','sent','failed','cancelled'))
   OR (OLD.status='sent' AND NEW.status IN('sent','unknown','response_received'))
   OR (OLD.status='unknown' AND NEW.status IN('unknown','response_received'))
   OR (OLD.status IN('response_received','failed','cancelled') AND NEW.status=OLD.status)) THEN
  RAISE EXCEPTION 'verification cannot rewrite intent, restart or skip a fence' USING ERRCODE='23514';END IF;
 IF OLD.dispatch_started_at IS NOT NULL AND (NEW.dispatch_started_at IS DISTINCT FROM OLD.dispatch_started_at
 OR NEW.dispatched_worker_id IS DISTINCT FROM OLD.dispatched_worker_id OR NEW.dispatched_fencing_token IS DISTINCT FROM OLD.dispatched_fencing_token
 OR NEW.worker_id IS DISTINCT FROM OLD.worker_id OR NEW.lease_until IS DISTINCT FROM OLD.lease_until OR NEW.fencing_token IS DISTINCT FROM OLD.fencing_token) THEN
  RAISE EXCEPTION 'sent verification cannot replace original marker or claim a resend' USING ERRCODE='23514';END IF;
 IF OLD.status IN('response_received','failed','cancelled') AND to_jsonb(NEW)-ARRAY['version','updated_at'] IS DISTINCT FROM to_jsonb(OLD)-ARRAY['version','updated_at'] THEN
  RAISE EXCEPTION 'terminal verification fact is immutable' USING ERRCODE='23514';END IF;
 IF OLD.outcome IS NOT NULL AND (NEW.outcome IS DISTINCT FROM OLD.outcome OR NEW.evidence_digest IS DISTINCT FROM OLD.evidence_digest) THEN
  RAISE EXCEPTION 'verification cannot replace recorded evidence' USING ERRCODE='23514';END IF;
 IF NEW.outcome IS NOT NULL THEN
  IF NEW.outcome->>'outcome' IS DISTINCT FROM 'response_received'
  OR (NEW.kind='model' AND (NEW.outcome-ARRAY['outcome','response_digest']<>'{}'::jsonb
    OR jsonb_typeof(NEW.outcome->'response_digest') IS DISTINCT FROM 'string'
    OR NOT COALESCE(NEW.outcome->>'response_digest'~'^[a-f0-9]{64}$',false)))
  OR (NEW.kind='mcp' AND (NEW.outcome-ARRAY['outcome','protocol_version','tool_count','catalog_digest','response_digest']<>'{}'::jsonb
    OR NOT configuration_json_strings(NEW.outcome,ARRAY['protocol_version'],128)
    OR NOT configuration_json_integer(NEW.outcome->'tool_count',0,128)
    OR jsonb_typeof(NEW.outcome->'catalog_digest') IS DISTINCT FROM 'string'
    OR NOT COALESCE(NEW.outcome->>'catalog_digest'~'^[a-f0-9]{64}$',false)
    OR (NEW.outcome?'response_digest' AND (jsonb_typeof(NEW.outcome->'response_digest') IS DISTINCT FROM 'string'
      OR NOT COALESCE(NEW.outcome->>'response_digest'~'^[a-f0-9]{64}$',false))))) THEN
   RAISE EXCEPTION 'verification receipt permits only bounded safe structural facts' USING ERRCODE='23514';END IF;
 END IF;
 IF OLD.dispatch_started_at IS NULL AND NEW.dispatch_started_at IS NOT NULL THEN
  IF OLD.status<>'queued' OR NEW.status<>'sent' THEN RAISE EXCEPTION 'only queued send may create a marker' USING ERRCODE='23514';END IF;
  IF NEW.kind='mcp' THEN
   IF OLD.worker_id='' OR OLD.fencing_token<1 OR OLD.lease_until IS NULL OR OLD.lease_until<=clock_timestamp()
   OR NEW.dispatched_worker_id IS DISTINCT FROM OLD.worker_id OR NEW.dispatched_fencing_token IS DISTINCT FROM OLD.fencing_token
   OR NEW.worker_id IS DISTINCT FROM OLD.worker_id OR NEW.lease_until IS DISTINCT FROM OLD.lease_until OR NEW.fencing_token<>OLD.fencing_token THEN
    RAISE EXCEPTION 'MCP send requires exact live claim and fence' USING ERRCODE='23514';END IF;
   NEW.dispatch_started_at=clock_timestamp();
  ELSE
   SELECT EXISTS(SELECT 1 FROM actions a JOIN operations o ON o.organization_id=a.organization_id AND o.id=a.operation_id AND o.task_id=a.task_id
    JOIN reservations r ON r.organization_id=a.organization_id AND r.action_id=a.id AND r.operation_id=o.id AND r.task_id=a.task_id
    WHERE a.organization_id=OLD.organization_id AND a.task_id=OLD.task_id AND r.category='model'
    AND a.dispatch_issued_at=NEW.dispatch_started_at AND a.dispatch_fencing_token=NEW.dispatched_fencing_token
    AND o.lease_owner=NEW.dispatched_worker_id AND o.fencing_token=NEW.dispatched_fencing_token
    AND NEW.worker_id=NEW.dispatched_worker_id AND NEW.fencing_token=NEW.dispatched_fencing_token
    AND NEW.lease_until=o.lease_until AND o.lease_until>clock_timestamp()) INTO marker_valid;
   IF NOT marker_valid THEN RAISE EXCEPTION 'model marker must be the exact ordinary action dispatch' USING ERRCODE='23514';END IF;
  END IF;
 END IF;
 IF NEW.status='queued' THEN
  IF NEW.dispatch_started_at IS NOT NULL OR (NEW.worker_id<>OLD.worker_id AND NEW.fencing_token<>OLD.fencing_token+1)
  OR (NEW.fencing_token=OLD.fencing_token+1 AND (NEW.kind<>'mcp' OR NEW.worker_id='' OR NEW.lease_until IS NULL OR NEW.lease_until<=clock_timestamp()
    OR (OLD.lease_until IS NOT NULL AND OLD.lease_until>clock_timestamp())))
  OR (OLD.worker_id='' AND NEW.worker_id<>'' AND NEW.fencing_token<>OLD.fencing_token+1) THEN
   RAISE EXCEPTION 'verification claim requires a fresh bounded fence after prior lease expiry' USING ERRCODE='23514';END IF;
  IF NEW.fencing_token=OLD.fencing_token AND NEW.lease_until IS DISTINCT FROM OLD.lease_until
  AND (OLD.lease_until IS NULL OR OLD.lease_until<=clock_timestamp()) THEN
   RAISE EXCEPTION 'expired claim cannot renew without a new fence' USING ERRCODE='23514';END IF;
 END IF;
 -- No current-grant/role check here: exact sent receipts remain admissible
 -- after cancellation, key expiry, policy changes and actor deprovisioning.
 NEW.version=OLD.version+1;NEW.updated_at=GREATEST(clock_timestamp(),OLD.updated_at+interval '1 microsecond');RETURN NEW;
END $$;
