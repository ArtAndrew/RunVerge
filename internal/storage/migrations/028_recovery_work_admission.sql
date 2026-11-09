-- SPDX-License-Identifier: Elastic-2.0
-- The import remains held for recovery operations. Ordinary work has a
-- separate, append-only grant to a fresh database login and reviewed scopes.
CREATE TABLE recovery_work_admissions (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 activation_id text NOT NULL UNIQUE CHECK(activation_id ~ '^activate_[a-f0-9]{32}$'),
 restore_id text NOT NULL REFERENCES recovery_import_receipts(restore_id),
 target_installation_id text NOT NULL CHECK(target_installation_id ~ '^inst_[a-f0-9]{32}$'),
 database_oid oid NOT NULL CHECK(database_oid<>0),
 runtime_role text NOT NULL CHECK(runtime_role ~ '^rvg_runtime_[a-f0-9]{32}$'),
 runtime_role_oid oid NOT NULL CHECK(runtime_role_oid<>0),
 organizations text[] NOT NULL CHECK(cardinality(organizations) BETWEEN 1 AND 64 AND array_position(organizations,NULL) IS NULL),
 review_sha256 text NOT NULL CHECK(review_sha256 ~ '^[a-f0-9]{64}$'),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[a-f0-9]{64}$'),
 import_receipt_sha256 text NOT NULL CHECK(import_receipt_sha256 ~ '^[a-f0-9]{64}$'),
 source_stop_sha256 text NOT NULL CHECK(source_stop_sha256 ~ '^[a-f0-9]{64}$'),
 retirement_seal_sha256 text NOT NULL CHECK(retirement_seal_sha256 ~ '^[a-f0-9]{64}$'),
 peer_journal_sha256 text NOT NULL CHECK(peer_journal_sha256 ~ '^[a-f0-9]{64}$'),
 peer_signer_sha256 text NOT NULL CHECK(peer_signer_sha256 ~ '^[a-f0-9]{64}$'),
 accounting_head_id text NOT NULL CHECK(octet_length(accounting_head_id)<=128),
 accounting_head_ordinal bigint NOT NULL CHECK(accounting_head_ordinal>=0),
 operator_reference text NOT NULL CHECK(octet_length(operator_reference) BETWEEN 1 AND 128),
 admitted_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CHECK((accounting_head_ordinal=0)=(accounting_head_id=''))
);
REVOKE ALL ON recovery_work_admissions FROM PUBLIC;

CREATE FUNCTION recovery_work_admission_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP<>'INSERT' THEN
  RAISE EXCEPTION 'recovery work admission is permanent' USING ERRCODE='55000';
 END IF;
 PERFORM pg_catalog.pg_advisory_xact_lock(841932080,pg_catalog.hashtext(TG_TABLE_SCHEMA));
 IF SESSION_USER<>CURRENT_USER OR NOT EXISTS(SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=SESSION_USER AND rolsuper)
 OR NOT EXISTS(
  SELECT 1 FROM runtime_recovery_control c JOIN recovery_import_receipts i
   ON i.restore_id=c.restore_id AND i.target_installation_id=c.installation_id
  WHERE c.singleton AND c.state='held' AND c.restore_id=NEW.restore_id AND c.installation_id=NEW.target_installation_id)
 OR NEW.database_oid IS DISTINCT FROM (SELECT oid FROM pg_catalog.pg_database WHERE datname=pg_catalog.current_database())
 OR NEW.runtime_role<>'rvg_runtime_'||pg_catalog.substr(NEW.restore_id,9)
 OR NOT EXISTS(SELECT 1 FROM pg_catalog.pg_roles r WHERE r.oid=NEW.runtime_role_oid AND r.rolname=NEW.runtime_role
  AND r.rolcanlogin AND NOT (r.rolsuper OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolbypassrls OR r.rolinherit)
  AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members m WHERE m.member=r.oid OR m.roleid=r.oid))
 OR (SELECT count(DISTINCT id) FROM organizations WHERE id=ANY(NEW.organizations) AND active)<>pg_catalog.cardinality(NEW.organizations)
 THEN
  RAISE EXCEPTION 'recovery work admission binding differs' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;

CREATE TRIGGER recovery_work_admission_insert BEFORE INSERT ON recovery_work_admissions
 FOR EACH ROW EXECUTE FUNCTION recovery_work_admission_guard();
CREATE TRIGGER recovery_work_admission_permanent BEFORE UPDATE OR DELETE OR TRUNCATE ON recovery_work_admissions
 FOR EACH STATEMENT EXECUTE FUNCTION recovery_work_admission_guard();

CREATE FUNCTION recovery_runtime_login() RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER AS $$
 SELECT EXISTS(SELECT 1 FROM runtime_recovery_control c JOIN recovery_work_admissions a
 ON a.restore_id=c.restore_id AND a.target_installation_id=c.installation_id
 JOIN pg_catalog.pg_roles r ON r.oid=a.runtime_role_oid AND r.rolname=a.runtime_role
 WHERE c.singleton AND c.state='held' AND r.rolname=SESSION_USER AND r.rolcanlogin
 AND NOT (r.rolsuper OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication OR r.rolbypassrls OR r.rolinherit)
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members m WHERE m.member=r.oid OR m.roleid=r.oid) AND
 a.database_oid=(SELECT oid FROM pg_catalog.pg_database WHERE datname=pg_catalog.current_database()))
$$;

CREATE FUNCTION recovery_runtime_organization_available(org text) RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER AS $$
 SELECT COALESCE((SELECT state='normal' OR
  (recovery_runtime_login() AND EXISTS(SELECT 1 FROM recovery_work_admissions WHERE org=ANY(organizations)))
  FROM runtime_recovery_control WHERE singleton),false)
$$;

CREATE OR REPLACE FUNCTION runtime_recovery_admit(read_only boolean DEFAULT false) RETURNS void LANGUAGE plpgsql SECURITY DEFINER AS $$
DECLARE current_state text;
BEGIN
 PERFORM pg_catalog.pg_advisory_xact_lock_shared(841932080,pg_catalog.hashtext(pg_catalog.current_schema()));
 IF read_only THEN
  SELECT state INTO current_state FROM runtime_recovery_control WHERE singleton;
 ELSE
  SELECT state INTO current_state FROM runtime_recovery_control WHERE singleton FOR SHARE;
 END IF;
 IF current_state IS DISTINCT FROM 'normal' AND (current_state IS DISTINCT FROM 'held' OR NOT recovery_runtime_login()) THEN
  RAISE EXCEPTION 'runtime recovery hold' USING ERRCODE='55000';
 END IF;
END $$;

CREATE FUNCTION recovery_runtime_scope_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE old_row jsonb; new_row jsonb; org_field text:=TG_ARGV[0];
BEGIN
 IF pg_catalog.left(SESSION_USER,12)='rvg_runtime_' AND recovery_runtime_login() THEN
  IF TG_OP<>'INSERT' THEN
   old_row=pg_catalog.to_jsonb(OLD);
   IF (old_row->>org_field IS NOT NULL OR org_field<>'service_organization_id')
    AND NOT recovery_runtime_organization_available(old_row->>org_field) THEN
    RAISE EXCEPTION 'organization recovery hold' USING ERRCODE='55000';
   END IF;
  END IF;
  IF TG_OP<>'DELETE' THEN
   new_row=pg_catalog.to_jsonb(NEW);
   IF (new_row->>org_field IS NOT NULL OR org_field<>'service_organization_id')
    AND NOT recovery_runtime_organization_available(new_row->>org_field) THEN
    RAISE EXCEPTION 'organization recovery hold' USING ERRCODE='55000';
   END IF;
  END IF;
  IF TG_TABLE_NAME='organizations' AND TG_OP='UPDATE'
   AND old_row->>'financial_hold'='true' AND new_row->>'financial_hold'<>'true' THEN
   RAISE EXCEPTION 'recovery preserves the financial hold' USING ERRCODE='55000';
  END IF;
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;

-- Runtime writes use the existing accounting and history triggers. Give
-- those triggers access to protected metadata without granting the runtime
-- login direct access or letting it attach the elevated triggers elsewhere.
ALTER FUNCTION runtime_recovery_write_guard() SECURITY DEFINER;
ALTER FUNCTION recovery_history_row_guard() SECURITY DEFINER;
ALTER FUNCTION recovery_accounting_observe() SECURITY DEFINER;
REVOKE EXECUTE ON FUNCTION runtime_recovery_write_guard(),recovery_history_row_guard(),recovery_accounting_observe() FROM PUBLIC;

DO $$
DECLARE target_schema text:=current_schema(); item record;
BEGIN
 FOR item IN SELECT p.proname,pg_catalog.pg_get_function_identity_arguments(p.oid) AS args
  FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname=target_schema AND p.proname IN('recovery_work_admission_guard','recovery_runtime_login','recovery_runtime_organization_available','runtime_recovery_admit','recovery_runtime_scope_guard') LOOP
  EXECUTE pg_catalog.format('ALTER FUNCTION %I.%I(%s) SET search_path TO %I,pg_catalog,pg_temp',target_schema,item.proname,item.args,target_schema);
 END LOOP;
 -- Only tables participating in the existing runtime admission receive an
 -- organization guard. Trusted operator metadata keeps its own insert guards.
 FOR item IN SELECT c.relname,CASE WHEN c.relname='organizations' THEN 'id'
  WHEN c.relname='principals' THEN 'service_organization_id' ELSE 'organization_id' END AS org_field
  FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname=target_schema AND c.relkind='r'
  AND EXISTS(SELECT 1 FROM pg_catalog.pg_trigger t WHERE t.tgrelid=c.oid AND t.tgname='recovery_runtime_write')
  AND (c.relname IN('organizations','principals') OR EXISTS(SELECT 1 FROM pg_catalog.pg_attribute a
   WHERE a.attrelid=c.oid AND a.attname='organization_id' AND NOT a.attisdropped))
 LOOP
  EXECUTE pg_catalog.format('CREATE TRIGGER recovery_runtime_scope BEFORE INSERT OR UPDATE OR DELETE ON %I.%I FOR EACH ROW EXECUTE FUNCTION %I.recovery_runtime_scope_guard(%L)',target_schema,item.relname,target_schema,item.org_field);
 END LOOP;
END $$;
