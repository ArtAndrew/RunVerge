-- SPDX-License-Identifier: Elastic-2.0
-- Fresh-target recovery admission. This is not an import or resume protocol.
CREATE TABLE runtime_recovery_control (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 installation_id text NOT NULL DEFAULT ('inst_'||replace(gen_random_uuid()::text,'-','')) CHECK(installation_id ~ '^inst_[a-f0-9]{32}$'),
 state text NOT NULL DEFAULT 'normal' CHECK(state IN('normal','held')),
 restore_id text CHECK(restore_id ~ '^restore_[a-f0-9]{32}$'),
 backup_id text CHECK(backup_id ~ '^bak_[a-f0-9]{32}$'),
 source_id text CHECK(source_id ~ '^inst_[a-f0-9]{32}$'),
 cut_at timestamptz,
 schema_sha256 text CHECK(schema_sha256 ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 held_at timestamptz,
 CHECK((state='normal' AND restore_id IS NULL AND backup_id IS NULL AND source_id IS NULL AND cut_at IS NULL AND schema_sha256 IS NULL AND held_at IS NULL)
    OR (state='held' AND restore_id IS NOT NULL AND backup_id IS NOT NULL AND source_id IS NOT NULL AND source_id<>installation_id AND cut_at IS NOT NULL AND schema_sha256 IS NOT NULL AND held_at IS NOT NULL))
);
INSERT INTO runtime_recovery_control(singleton) VALUES(true);

CREATE FUNCTION runtime_recovery_admit(read_only boolean DEFAULT false) RETURNS void LANGUAGE plpgsql AS $$
DECLARE current_state text;
BEGIN
 -- search_path is fixed to this function's real schema below. Temporary or
 -- caller-supplied lookalike tables must not select a different gate.
 PERFORM pg_catalog.pg_advisory_xact_lock_shared(841932080,pg_catalog.hashtext(pg_catalog.current_schema()));
 IF read_only THEN
  SELECT state INTO current_state FROM runtime_recovery_control WHERE singleton;
 ELSE
  -- A stale repeatable-read writer must fail serialization after waiting for
  -- preparation, rather than authorize using the old normal snapshot.
  SELECT state INTO current_state FROM runtime_recovery_control WHERE singleton FOR SHARE;
 END IF;
 IF current_state IS DISTINCT FROM 'normal' THEN
  RAISE EXCEPTION 'runtime recovery hold' USING ERRCODE='55000';
 END IF;
END $$;

CREATE FUNCTION runtime_recovery_normal() RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT COALESCE((SELECT state='normal' FROM runtime_recovery_control WHERE singleton),false)
$$;

CREATE FUNCTION runtime_recovery_write_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 -- The affected relation determines authority, never the caller's search_path.
 EXECUTE pg_catalog.format('SELECT %I.runtime_recovery_admit(false)',TG_TABLE_SCHEMA);
 RETURN NULL;
END $$;

CREATE FUNCTION runtime_recovery_control_statement() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP<>'UPDATE' THEN
  RAISE EXCEPTION 'recovery control is permanent' USING ERRCODE='55000';
 END IF;
 PERFORM pg_catalog.pg_advisory_xact_lock(841932080,pg_catalog.hashtext(TG_TABLE_SCHEMA));
 RETURN NULL;
END $$;

CREATE FUNCTION runtime_recovery_control_change() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE item record; populated boolean;
BEGIN
 IF OLD.state<>'normal' OR NEW.state<>'held'
  OR NEW.singleton IS DISTINCT FROM OLD.singleton
  OR NEW.installation_id IS DISTINCT FROM OLD.installation_id
  OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
  RAISE EXCEPTION 'recovery transition is not supported' USING ERRCODE='55000';
 END IF;
 -- This phase only prepares an empty target. A generic hold of a live,
 -- populated source and new-work clearance require separate protocols.
 FOR item IN SELECT c.relname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname=TG_TABLE_SCHEMA AND c.relkind IN('r','p')
   AND c.relname NOT IN('runtime_recovery_control','schema_migrations') ORDER BY c.relname
 LOOP
  EXECUTE pg_catalog.format('SELECT EXISTS(SELECT 1 FROM %I.%I LIMIT 1)',TG_TABLE_SCHEMA,item.relname) INTO populated;
  IF populated THEN RAISE EXCEPTION 'recovery target is not empty' USING ERRCODE='55000'; END IF;
 END LOOP;
 NEW.held_at=pg_catalog.clock_timestamp();
 RETURN NEW;
END $$;

CREATE TRIGGER recovery_control_statement BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON runtime_recovery_control
 FOR EACH STATEMENT EXECUTE FUNCTION runtime_recovery_control_statement();
CREATE TRIGGER recovery_control_change BEFORE UPDATE ON runtime_recovery_control
 FOR EACH ROW EXECUTE FUNCTION runtime_recovery_control_change();

DO $$
DECLARE target_schema text := current_schema(); item record;
BEGIN
 -- Include pg_temp explicitly LAST: otherwise PostgreSQL may implicitly search
 -- a temporary relation before a supposedly pinned application search_path.
 EXECUTE format('ALTER FUNCTION %I.runtime_recovery_admit(boolean) SET search_path TO %I,pg_catalog,pg_temp',target_schema,target_schema);
 EXECUTE format('ALTER FUNCTION %I.runtime_recovery_normal() SET search_path TO %I,pg_catalog,pg_temp',target_schema,target_schema);
 EXECUTE format('ALTER FUNCTION %I.runtime_recovery_write_guard() SET search_path TO %I,pg_catalog,pg_temp',target_schema,target_schema);
 EXECUTE format('ALTER FUNCTION %I.runtime_recovery_control_statement() SET search_path TO %I,pg_catalog,pg_temp',target_schema,target_schema);
 EXECUTE format('ALTER FUNCTION %I.runtime_recovery_control_change() SET search_path TO %I,pg_catalog,pg_temp',target_schema,target_schema);
 FOR item IN SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname=target_schema AND c.relkind IN('r','p')
   AND c.relname NOT IN('runtime_recovery_control','schema_migrations') ORDER BY c.relname
 LOOP
  EXECUTE format('CREATE TRIGGER recovery_runtime_write BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON %I.%I FOR EACH STATEMENT EXECUTE FUNCTION %I.runtime_recovery_write_guard()',target_schema,item.relname,target_schema);
 END LOOP;
END $$;
