-- SPDX-License-Identifier: Elastic-2.0
-- Import metadata belongs to this target, not to source business snapshots.
-- There is deliberately no hold-clearance operation in this migration.
CREATE TABLE recovery_import_receipts (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 profile text NOT NULL CHECK(profile='pg18-held-import-v1'),
 target_installation_id text NOT NULL CHECK(target_installation_id ~ '^inst_[a-f0-9]{32}$'),
 restore_id text NOT NULL UNIQUE CHECK(restore_id ~ '^restore_[a-f0-9]{32}$'),
 backup_id text NOT NULL CHECK(backup_id ~ '^bak_[a-f0-9]{32}$'),
 source_id text NOT NULL CHECK(source_id ~ '^inst_[a-f0-9]{32}$' AND source_id<>target_installation_id),
 cut_at timestamptz NOT NULL,
 snapshot_sha256 text NOT NULL CHECK(snapshot_sha256 ~ '^[a-f0-9]{64}$'),
 schema_sha256 text NOT NULL CHECK(schema_sha256 ~ '^[a-f0-9]{64}$'),
 inventory_sha256 text NOT NULL CHECK(inventory_sha256 ~ '^[a-f0-9]{64}$'),
 catalog_sha256 text NOT NULL CHECK(catalog_sha256 ~ '^[a-f0-9]{64}$'),
 tables jsonb NOT NULL CHECK(jsonb_typeof(tables)='array'),
 sequences jsonb NOT NULL CHECK(jsonb_typeof(sequences)='array'),
 cost_history jsonb NOT NULL CHECK(jsonb_typeof(cost_history)='object'),
 history_coverage jsonb NOT NULL CHECK(jsonb_typeof(history_coverage)='object'),
 imported_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE recovery_historical_rows (
 restore_id text NOT NULL REFERENCES recovery_import_receipts(restore_id),
 table_name text NOT NULL CHECK(table_name ~ '^[a-z][a-z0-9_]{0,127}$'),
 pk_sha256 text NOT NULL CHECK(pk_sha256 ~ '^[a-f0-9]{64}$'),
 PRIMARY KEY(table_name,pk_sha256)
);
REVOKE ALL ON recovery_import_receipts,recovery_historical_rows FROM PUBLIC;

CREATE FUNCTION recovery_import_metadata_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE held runtime_recovery_control%ROWTYPE;
BEGIN
 IF TG_OP<>'INSERT' THEN
  RAISE EXCEPTION 'recovery metadata is permanent' USING ERRCODE='55000';
 END IF;
 PERFORM pg_catalog.pg_advisory_xact_lock(841932080,pg_catalog.hashtext(TG_TABLE_SCHEMA));
 SELECT * INTO STRICT held FROM runtime_recovery_control WHERE singleton;
 IF held.state<>'held' OR NEW.restore_id IS DISTINCT FROM held.restore_id THEN
  RAISE EXCEPTION 'recovery metadata requires held target' USING ERRCODE='55000';
 END IF;
 IF TG_TABLE_NAME='recovery_import_receipts' THEN
  IF (
    NEW.target_installation_id IS DISTINCT FROM held.installation_id OR NEW.restore_id IS DISTINCT FROM held.restore_id
    OR NEW.source_id IS DISTINCT FROM held.source_id OR NEW.backup_id IS DISTINCT FROM held.backup_id
    OR NEW.cut_at IS DISTINCT FROM held.cut_at OR NEW.schema_sha256 IS DISTINCT FROM held.schema_sha256) THEN
  RAISE EXCEPTION 'recovery receipt target mismatch' USING ERRCODE='55000';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER recovery_receipt_insert BEFORE INSERT ON recovery_import_receipts FOR EACH ROW EXECUTE FUNCTION recovery_import_metadata_guard();
CREATE TRIGGER recovery_receipt_permanent BEFORE UPDATE OR DELETE OR TRUNCATE ON recovery_import_receipts FOR EACH STATEMENT EXECUTE FUNCTION recovery_import_metadata_guard();
CREATE TRIGGER recovery_mark_insert BEFORE INSERT ON recovery_historical_rows FOR EACH ROW EXECUTE FUNCTION recovery_import_metadata_guard();
CREATE TRIGGER recovery_mark_permanent BEFORE UPDATE OR DELETE OR TRUNCATE ON recovery_historical_rows FOR EACH STATEMENT EXECUTE FUNCTION recovery_import_metadata_guard();

-- Canonical SQL jsonb array, trusted physical PK order. This is a target-bound
-- identity reference, not a content digest or secret. It remains stable if a
-- later separately reviewed protocol changes the global execution hold.
CREATE FUNCTION recovery_row_key(relation_name text,key_parts jsonb) RETURNS text LANGUAGE sql STABLE AS $$
 SELECT pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(
   pg_catalog.jsonb_build_array('recovery-row-key-v1',r.target_installation_id,r.restore_id,relation_name,key_parts)::text,'UTF8')),'hex')
 FROM recovery_import_receipts r WHERE r.singleton
$$;
CREATE FUNCTION recovery_is_historical(relation_name text,key_parts jsonb) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM recovery_historical_rows h
 WHERE h.table_name=relation_name AND h.pk_sha256=recovery_row_key(relation_name,key_parts))
$$;
CREATE FUNCTION recovery_historical_task(org text,task text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT recovery_is_historical('tasks',pg_catalog.jsonb_build_array(org,task))
$$;
CREATE FUNCTION recovery_historical_principal(principal text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT recovery_is_historical('principals',pg_catalog.jsonb_build_array(principal))
$$;

CREATE FUNCTION recovery_history_row_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE before_row jsonb; after_row jsonb; old_key jsonb:='[]'; new_key jsonb:='[]';
 field text; item text; i integer; allowed text[]; org text; target_relation text;
BEGIN
 IF NOT EXISTS(SELECT 1 FROM recovery_import_receipts) THEN
  IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
 END IF;
 IF TG_OP<>'INSERT' THEN
  before_row=pg_catalog.to_jsonb(OLD);
  FOR i IN 0..TG_NARGS-1 LOOP old_key=old_key||pg_catalog.jsonb_build_array(before_row->TG_ARGV[i]); END LOOP;
 END IF;
 IF TG_OP<>'DELETE' THEN
  after_row=pg_catalog.to_jsonb(NEW);
  FOR i IN 0..TG_NARGS-1 LOOP new_key=new_key||pg_catalog.jsonb_build_array(after_row->TG_ARGV[i]); END LOOP;
 END IF;
 IF TG_OP<>'INSERT' AND recovery_is_historical(TG_TABLE_NAME,old_key) THEN
  -- Only shared aggregates needed by genuinely NEW work may advance. Original
  -- trusted constraints/triggers and fresh authority remain independently needed.
  allowed=CASE TG_TABLE_NAME
   WHEN 'organizations' THEN ARRAY['reserved_microusd','spent_microusd','budget_microusd','financial_hold']
   WHEN 'material_storage' THEN ARRAY['used_bytes','used_documents']
   WHEN 'configuration_revisions' THEN ARRAY['revision','updated_at']
   ELSE NULL END;
  IF TG_OP<>'UPDATE' OR allowed IS NULL OR old_key<>new_key
   OR before_row-allowed IS DISTINCT FROM after_row-allowed THEN
   RAISE EXCEPTION 'historical recovery row is fenced' USING ERRCODE='55000';
  END IF;
 ELSIF TG_OP<>'DELETE' AND recovery_is_historical(TG_TABLE_NAME,new_key) THEN
  RAISE EXCEPTION 'historical recovery identity is fenced' USING ERRCODE='55000';
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 org=after_row->>'organization_id';
 -- Explicit execution/authority references. Immutable profiles and quoted
 -- historical input are not treated as inherited execution permission here.
 FOREACH field IN ARRAY ARRAY['task_id','root_id','parent_id','child_task_id'] LOOP
  IF recovery_historical_task(org,after_row->>field) THEN
   RAISE EXCEPTION 'historical task reference is fenced' USING ERRCODE='55000';
  END IF;
 END LOOP;
 IF pg_catalog.jsonb_typeof(after_row->'scope_task_ids')='array' THEN
  FOR item IN SELECT pg_catalog.jsonb_array_elements_text(after_row->'scope_task_ids') LOOP
   IF recovery_historical_task(org,item) THEN RAISE EXCEPTION 'historical cost scope is fenced' USING ERRCODE='55000'; END IF;
  END LOOP;
 END IF;
 FOREACH field IN ARRAY ARRAY['owner_id','principal_id','issuer_id','subject_id','actor_id','created_by','creator_id','decided_by','dispatch_requested_by','authority_principal_id','published_by'] LOOP
  IF recovery_historical_principal(after_row->>field) THEN
   RAISE EXCEPTION 'historical principal authority is fenced' USING ERRCODE='55000';
  END IF;
 END LOOP;
 IF EXISTS(SELECT 1 FROM cost_evidence e WHERE e.organization_id=org AND e.id=after_row->>'evidence_id'
   AND recovery_is_historical('cost_evidence',pg_catalog.jsonb_build_array(e.organization_id,e.source,e.reference))) THEN
  RAISE EXCEPTION 'historical financial evidence is fenced' USING ERRCODE='55000';
 END IF;
 FOREACH field IN ARRAY ARRAY['run_id','current_run_id','root_run_id','parent_run_id','child_run_id','operation_id','source_operation_id','reservation_id','quarantine_id','approval_id','validation_id','last_validation_id','grant_id','parent_grant_id','automation_grant_id','plan_id','verification_id','subscription_id','delivery_id','parent_decision_id'] LOOP
  item=after_row->>field;
  target_relation=CASE
   WHEN field IN('run_id','current_run_id','root_run_id','parent_run_id','child_run_id') THEN 'runs'
   WHEN field IN('operation_id','source_operation_id') THEN 'operations'
   WHEN field='reservation_id' THEN 'reservations'
   WHEN field='quarantine_id' THEN 'cost_quarantines'
   WHEN field='approval_id' THEN CASE WHEN TG_TABLE_NAME LIKE 'configuration_%' THEN 'configuration_approvals' ELSE 'approvals' END
   WHEN field IN('validation_id','last_validation_id') THEN 'configuration_validations'
   WHEN field IN('grant_id','parent_grant_id') THEN 'configuration_grants'
   WHEN field='automation_grant_id' THEN 'task_automation_grants'
   WHEN field='plan_id' THEN 'configuration_plans'
   WHEN field='verification_id' THEN 'configuration_verifications'
   WHEN field='subscription_id' THEN 'webhook_subscriptions'
   WHEN field='delivery_id' THEN 'webhook_deliveries'
   WHEN field='parent_decision_id' THEN 'task_decisions' END;
  IF item IS NOT NULL AND recovery_is_historical(target_relation,pg_catalog.jsonb_build_array(org,item)) THEN
   RAISE EXCEPTION 'historical execution authority is fenced' USING ERRCODE='55000';
  END IF;
 END LOOP;
 IF after_row->>'event_sequence' IS NOT NULL AND recovery_is_historical('events',pg_catalog.jsonb_build_array((after_row->>'event_sequence')::bigint))
  OR TG_TABLE_NAME='outbox' AND recovery_is_historical('events',pg_catalog.jsonb_build_array((after_row->>'sequence')::bigint)) THEN
  RAISE EXCEPTION 'historical event dispatch is fenced' USING ERRCODE='55000';
 END IF;
 RETURN NEW;
END $$;

CREATE FUNCTION recovery_history_truncate_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM recovery_historical_rows WHERE table_name=TG_TABLE_NAME) THEN
  RAISE EXCEPTION 'historical recovery relation is fenced' USING ERRCODE='55000';
 END IF;
 RETURN NULL;
END $$;

DO $$
DECLARE target_schema text:=current_schema(); item record; argument_sql text;
BEGIN
 FOR item IN SELECT p.proname,pg_catalog.pg_get_function_identity_arguments(p.oid) AS args
  FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname=target_schema AND p.proname IN('recovery_import_metadata_guard','recovery_row_key','recovery_is_historical','recovery_historical_task','recovery_historical_principal','recovery_history_row_guard','recovery_history_truncate_guard') LOOP
  EXECUTE pg_catalog.format('ALTER FUNCTION %I.%I(%s) SET search_path TO %I,pg_catalog,pg_temp',target_schema,item.proname,item.args,target_schema);
 END LOOP;
 FOR item IN SELECT c.relname,c.oid FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
  WHERE n.nspname=target_schema AND c.relkind='r' AND c.relname NOT IN('schema_migrations','runtime_recovery_control','recovery_import_receipts','recovery_historical_rows') ORDER BY c.relname LOOP
  SELECT pg_catalog.string_agg(pg_catalog.quote_literal(a.attname),',' ORDER BY key.ordinality) INTO argument_sql
   FROM pg_catalog.pg_index ix CROSS JOIN LATERAL pg_catalog.unnest(ix.indkey) WITH ORDINALITY key(attnum,ordinality)
   JOIN pg_catalog.pg_attribute a ON a.attrelid=ix.indrelid AND a.attnum=key.attnum
   WHERE ix.indrelid=item.oid AND ix.indisprimary AND key.ordinality<=ix.indnkeyatts;
  IF argument_sql IS NULL THEN RAISE EXCEPTION 'recovery requires physical primary keys'; END IF;
  EXECUTE pg_catalog.format('CREATE TRIGGER "000_recovery_history_row" BEFORE INSERT OR UPDATE OR DELETE ON %I.%I FOR EACH ROW EXECUTE FUNCTION %I.recovery_history_row_guard(%s)',target_schema,item.relname,target_schema,argument_sql);
  EXECUTE pg_catalog.format('CREATE TRIGGER "000_recovery_history_truncate" BEFORE TRUNCATE ON %I.%I FOR EACH STATEMENT EXECUTE FUNCTION %I.recovery_history_truncate_guard()',target_schema,item.relname,target_schema);
 END LOOP;
END $$;
