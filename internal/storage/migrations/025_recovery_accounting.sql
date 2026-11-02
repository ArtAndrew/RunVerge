-- SPDX-License-Identifier: Elastic-2.0
-- Trusted-host accounting only. No transition out of the execution hold.
-- Database owners remain trusted: this is not a defence against an owner
-- rewriting functions or disabling triggers. No tenant API creates receipts.
CREATE TABLE recovery_accounting_batches (
 id text PRIMARY KEY CHECK(id ~ '^[a-f0-9]{32}$'),
 restore_id text NOT NULL REFERENCES recovery_import_receipts(restore_id),
 target_installation_id text NOT NULL CHECK(target_installation_id ~ '^inst_[a-f0-9]{32}$'),
 evidence_batch_id text NOT NULL REFERENCES recovery_evidence_batches(id),
 organization_id text NOT NULL,
 reservation_id text NOT NULL,
 expected_revision bigint NOT NULL CHECK(expected_revision>0),
 ordinal bigint NOT NULL CHECK(ordinal>0),
 previous_id text REFERENCES recovery_accounting_batches(id),
 request_sha256 text NOT NULL CHECK(request_sha256 ~ '^[a-f0-9]{64}$'),
 plan_sha256 text NOT NULL CHECK(plan_sha256 ~ '^[a-f0-9]{64}$'),
 mutation_count integer NOT NULL CHECK(mutation_count BETWEEN 1 AND 10000),
 mutation_bytes bigint NOT NULL CHECK(mutation_bytes BETWEEN 1 AND 16777216),
 creating_transaction xid8 NOT NULL DEFAULT pg_current_xact_id(),
 operator_role name NOT NULL DEFAULT session_user,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(organization_id,reservation_id) REFERENCES reservations(organization_id,id),
 UNIQUE(restore_id,ordinal), UNIQUE(creating_transaction),
 CHECK((ordinal=1 AND previous_id IS NULL) OR (ordinal>1 AND previous_id IS NOT NULL))
);
CREATE TABLE recovery_accounting_mutations (
 batch_id text NOT NULL REFERENCES recovery_accounting_batches(id),
 ordinal integer NOT NULL CHECK(ordinal BETWEEN 0 AND 9999),
 table_name text NOT NULL CHECK(table_name IN('organizations','tasks','reservations','cost_evidence','ledger_entries','cost_quarantines','cost_quarantine_resolutions','financial_incidents')),
 operation text NOT NULL CHECK(operation IN('INSERT','UPDATE')),
 key_parts jsonb NOT NULL CHECK(jsonb_typeof(key_parts)='array' AND octet_length(key_parts::text)<=1024),
 before_row jsonb,
 after_row jsonb NOT NULL CHECK(jsonb_typeof(after_row)='object'),
 CHECK((operation='INSERT' AND before_row IS NULL) OR (operation='UPDATE' AND jsonb_typeof(before_row)='object' AND before_row<>after_row)),
 CHECK(octet_length(COALESCE(before_row::text,''))+octet_length(after_row::text)<=1048576),
 PRIMARY KEY(batch_id,ordinal), UNIQUE(batch_id,table_name,key_parts)
);
CREATE TABLE recovery_accounting_applied (
 batch_id text NOT NULL,
 ordinal integer NOT NULL,
 before_sha256 text,
 after_sha256 text NOT NULL CHECK(after_sha256 ~ '^[a-f0-9]{64}$'),
 PRIMARY KEY(batch_id,ordinal),
 FOREIGN KEY(batch_id,ordinal) REFERENCES recovery_accounting_mutations(batch_id,ordinal),
 CHECK(before_sha256 IS NULL OR before_sha256 ~ '^[a-f0-9]{64}$')
);
REVOKE ALL ON recovery_accounting_batches,recovery_accounting_mutations,recovery_accounting_applied FROM PUBLIC;

-- Always read the real pinned-schema relation. Trigger nesting alone cannot
-- prove an effect: a caller could attach a trigger to a temporary lookalike.
CREATE FUNCTION recovery_accounting_current_row(relation_name text,key_parts jsonb) RETURNS jsonb LANGUAGE plpgsql AS $$
DECLARE predicate text; expected_parts integer; result jsonb;
BEGIN
 IF relation_name='organizations' THEN
  predicate='t.id=($1->>0)'; expected_parts=1;
 ELSIF relation_name='cost_evidence' THEN
  predicate='t.organization_id=($1->>0) AND t.source=($1->>1) AND t.reference=($1->>2)'; expected_parts=3;
 ELSIF relation_name IN('tasks','reservations','ledger_entries','cost_quarantines','cost_quarantine_resolutions','financial_incidents') THEN
  predicate='t.organization_id=($1->>0) AND t.id=($1->>1)'; expected_parts=2;
 ELSE RAISE EXCEPTION 'unsupported accounting relation' USING ERRCODE='55000'; END IF;
 IF pg_catalog.jsonb_typeof(key_parts) IS DISTINCT FROM 'array' OR pg_catalog.jsonb_array_length(key_parts)<>expected_parts THEN
  RAISE EXCEPTION 'invalid accounting key' USING ERRCODE='55000';
 END IF;
 EXECUTE pg_catalog.format('SELECT pg_catalog.to_jsonb(t) FROM %I.%I t WHERE %s',pg_catalog.current_schema(),relation_name,predicate)
 INTO result USING key_parts;
 RETURN result;
END $$;

CREATE FUNCTION recovery_accounting_metadata_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE b recovery_accounting_batches%ROWTYPE; previous recovery_accounting_batches%ROWTYPE;
 held runtime_recovery_control%ROWTYPE; imported recovery_import_receipts%ROWTYPE;
 evidence recovery_evidence_batches%ROWTYPE; r reservations%ROWTYPE;
 m recovery_accounting_mutations%ROWTYPE; allowed text[]; expected_key jsonb; org text;
BEGIN
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'recovery accounting audit is permanent' USING ERRCODE='55000'; END IF;
 PERFORM pg_catalog.pg_advisory_xact_lock(841932080,pg_catalog.hashtext(TG_TABLE_SCHEMA));
 SELECT * INTO STRICT held FROM runtime_recovery_control WHERE singleton;
 SELECT * INTO STRICT imported FROM recovery_import_receipts WHERE singleton;
 IF held.state<>'held' OR held.restore_id IS DISTINCT FROM imported.restore_id
  OR held.installation_id IS DISTINCT FROM imported.target_installation_id THEN
  RAISE EXCEPTION 'accounting requires exact imported held target' USING ERRCODE='55000';
 END IF;
 IF TG_TABLE_NAME='recovery_accounting_batches' THEN
  SELECT * INTO STRICT evidence FROM recovery_evidence_batches WHERE id=NEW.evidence_batch_id;
  SELECT * INTO STRICT r FROM reservations WHERE organization_id=NEW.organization_id AND id=NEW.reservation_id;
  SELECT * INTO previous FROM recovery_accounting_batches WHERE restore_id=held.restore_id ORDER BY ordinal DESC LIMIT 1;
  IF NEW.restore_id IS DISTINCT FROM held.restore_id OR NEW.target_installation_id IS DISTINCT FROM held.installation_id
   OR evidence.restore_id IS DISTINCT FROM held.restore_id OR NEW.expected_revision IS DISTINCT FROM r.revision::bigint
   OR NEW.ordinal IS DISTINCT FROM COALESCE(previous.ordinal,0)+1 OR NEW.previous_id IS DISTINCT FROM previous.id
   OR NEW.creating_transaction IS DISTINCT FROM pg_catalog.pg_current_xact_id() OR NEW.operator_role IS DISTINCT FROM session_user
   OR NOT recovery_is_historical('reservations',pg_catalog.jsonb_build_array(r.organization_id,r.id)) THEN
   RAISE EXCEPTION 'accounting batch identity mismatch' USING ERRCODE='55000';
  END IF;
  RETURN NEW;
 END IF;
 SELECT * INTO STRICT b FROM recovery_accounting_batches WHERE id=NEW.batch_id;
 IF b.restore_id IS DISTINCT FROM held.restore_id OR b.creating_transaction IS DISTINCT FROM pg_catalog.pg_current_xact_id()
  OR b.operator_role IS DISTINCT FROM session_user OR NEW.ordinal>=b.mutation_count THEN
  RAISE EXCEPTION 'accounting batch already sealed' USING ERRCODE='55000';
 END IF;
 IF TG_TABLE_NAME='recovery_accounting_applied' THEN
  -- Only the AFTER row observer records actual effects. The immutable finite
  -- slot and FK forbid a second consumption, including after early constraints.
  SELECT * INTO STRICT m FROM recovery_accounting_mutations WHERE batch_id=NEW.batch_id AND ordinal=NEW.ordinal;
  IF pg_catalog.pg_trigger_depth()<2
   OR recovery_accounting_current_row(m.table_name,m.key_parts) IS DISTINCT FROM m.after_row
   OR NEW.before_sha256 IS DISTINCT FROM CASE WHEN m.before_row IS NULL THEN NULL ELSE pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(m.before_row::text,'UTF8')),'hex') END
   OR NEW.after_sha256 IS DISTINCT FROM pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(m.after_row::text,'UTF8')),'hex') THEN
   RAISE EXCEPTION 'accounting mutation was not observed' USING ERRCODE='55000';
  END IF;
  RETURN NEW;
 END IF;
 SELECT * INTO STRICT r FROM reservations WHERE organization_id=b.organization_id AND id=b.reservation_id;
 org=CASE WHEN NEW.table_name='organizations' THEN NEW.after_row->>'id' ELSE NEW.after_row->>'organization_id' END;
 IF org IS DISTINCT FROM b.organization_id THEN RAISE EXCEPTION 'accounting scope mismatch' USING ERRCODE='55000'; END IF;
 expected_key=CASE NEW.table_name
  WHEN 'organizations' THEN pg_catalog.jsonb_build_array(NEW.after_row->'id')
  WHEN 'cost_evidence' THEN pg_catalog.jsonb_build_array(NEW.after_row->'organization_id',NEW.after_row->'source',NEW.after_row->'reference')
  ELSE pg_catalog.jsonb_build_array(NEW.after_row->'organization_id',NEW.after_row->'id') END;
 IF NEW.key_parts IS DISTINCT FROM expected_key THEN RAISE EXCEPTION 'accounting key mismatch' USING ERRCODE='55000'; END IF;
 IF recovery_accounting_current_row(NEW.table_name,NEW.key_parts) IS DISTINCT FROM NEW.before_row THEN
  RAISE EXCEPTION 'accounting baseline mismatch' USING ERRCODE='55000';
 END IF;
 IF NEW.table_name IN('organizations','tasks','reservations') THEN
  allowed=CASE NEW.table_name WHEN 'reservations' THEN ARRAY['status','actual_microusd','revision','pending_quarantine_id']
   ELSE ARRAY['reserved_microusd','spent_microusd','financial_hold'] END;
  IF NEW.operation<>'UPDATE' OR NEW.before_row IS NULL OR NEW.before_row-allowed IS DISTINCT FROM NEW.after_row-allowed THEN
   RAISE EXCEPTION 'nonfinancial recovery mutation' USING ERRCODE='55000';
  END IF;
  IF NEW.table_name='reservations' THEN
   IF NEW.after_row->>'id' IS DISTINCT FROM b.reservation_id
    OR (NEW.before_row->>'revision')::bigint IS DISTINCT FROM b.expected_revision
    OR (NEW.after_row->>'revision')::bigint IS DISTINCT FROM b.expected_revision+1 THEN
    RAISE EXCEPTION 'reservation revision mismatch' USING ERRCODE='55000';
   END IF;
  ELSE
   IF NEW.before_row->>'financial_hold'='true' AND NEW.after_row->>'financial_hold' IS DISTINCT FROM 'true' THEN
    RAISE EXCEPTION 'financial hold cannot be cleared' USING ERRCODE='55000';
   END IF;
   IF NEW.table_name='tasks' AND NOT (NEW.after_row->>'id'=ANY(r.scope_task_ids)) THEN
    RAISE EXCEPTION 'accounting task scope mismatch' USING ERRCODE='55000';
   END IF;
  END IF;
 ELSE
  IF NEW.operation<>'INSERT' OR NEW.before_row IS NOT NULL THEN RAISE EXCEPTION 'financial facts are append only' USING ERRCODE='55000'; END IF;
  IF NEW.table_name='cost_quarantine_resolutions' THEN
   IF NOT EXISTS(SELECT 1 FROM cost_quarantines q WHERE q.organization_id=b.organization_id AND q.id=NEW.after_row->>'quarantine_id' AND q.reservation_id=b.reservation_id) THEN
    RAISE EXCEPTION 'quarantine scope mismatch' USING ERRCODE='55000';
   END IF;
  ELSIF NEW.after_row->>'reservation_id' IS DISTINCT FROM b.reservation_id THEN
   RAISE EXCEPTION 'financial fact scope mismatch' USING ERRCODE='55000';
  END IF;
 END IF;
 RETURN NEW;
END $$;

CREATE FUNCTION recovery_accounting_matches(relation_name text,operation_name text,before_value jsonb,after_value jsonb) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM recovery_accounting_batches b
 JOIN runtime_recovery_control c ON c.singleton AND c.state='held' AND c.restore_id=b.restore_id AND c.installation_id=b.target_installation_id
 JOIN recovery_accounting_mutations m ON m.batch_id=b.id
 WHERE b.creating_transaction=pg_catalog.pg_current_xact_id() AND b.operator_role=session_user
  AND m.table_name=relation_name AND m.operation=operation_name
  AND m.before_row IS NOT DISTINCT FROM before_value AND m.after_row=after_value
  AND NOT EXISTS(SELECT 1 FROM recovery_accounting_applied a WHERE a.batch_id=m.batch_id AND a.ordinal=m.ordinal))
$$;

CREATE FUNCTION recovery_accounting_observe() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE b recovery_accounting_batches%ROWTYPE; m recovery_accounting_mutations%ROWTYPE; before_value jsonb; after_value jsonb;
BEGIN
 IF TG_TABLE_SCHEMA IS DISTINCT FROM pg_catalog.current_schema()
  OR TG_RELID IS DISTINCT FROM pg_catalog.to_regclass(pg_catalog.format('%I.%I',pg_catalog.current_schema(),TG_TABLE_NAME))::oid THEN
  RAISE EXCEPTION 'accounting observer relation mismatch' USING ERRCODE='55000';
 END IF;
 SELECT * INTO b FROM recovery_accounting_batches WHERE creating_transaction=pg_catalog.pg_current_xact_id();
 IF NOT FOUND THEN RETURN NULL; END IF;
 IF TG_OP='UPDATE' THEN before_value=pg_catalog.to_jsonb(OLD); END IF;
 after_value=pg_catalog.to_jsonb(NEW);
 SELECT * INTO STRICT m FROM recovery_accounting_mutations WHERE batch_id=b.id AND table_name=TG_TABLE_NAME
  AND operation=TG_OP AND before_row IS NOT DISTINCT FROM before_value AND after_row=after_value;
 INSERT INTO recovery_accounting_applied(batch_id,ordinal,before_sha256,after_sha256) VALUES(b.id,m.ordinal,
  CASE WHEN before_value IS NULL THEN NULL ELSE pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(before_value::text,'UTF8')),'hex') END,
  pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(after_value::text,'UTF8')),'hex'));
 RETURN NULL;
END $$;

CREATE FUNCTION recovery_accounting_complete() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE prescribed bigint; observed bigint; bytes bigint; m recovery_accounting_mutations%ROWTYPE;
BEGIN
 SELECT count(*),COALESCE(sum(octet_length(COALESCE(before_row::text,''))+octet_length(after_row::text)),0)
  INTO prescribed,bytes FROM recovery_accounting_mutations WHERE batch_id=NEW.id;
 SELECT count(*) INTO observed FROM recovery_accounting_applied WHERE batch_id=NEW.id;
 IF prescribed<>NEW.mutation_count OR observed<>NEW.mutation_count OR bytes<>NEW.mutation_bytes THEN
  RAISE EXCEPTION 'accounting batch is incomplete' USING ERRCODE='55000';
 END IF;
 FOR m IN SELECT * FROM recovery_accounting_mutations WHERE batch_id=NEW.id LOOP
  IF recovery_accounting_current_row(m.table_name,m.key_parts) IS DISTINCT FROM m.after_row THEN
   RAISE EXCEPTION 'accounting final state mismatch' USING ERRCODE='55000';
  END IF;
 END LOOP;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER recovery_accounting_complete AFTER INSERT ON recovery_accounting_batches
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION recovery_accounting_complete();

CREATE OR REPLACE FUNCTION runtime_recovery_write_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE permitted boolean;
BEGIN
 EXECUTE pg_catalog.format('SELECT EXISTS(SELECT 1 FROM %I.recovery_accounting_batches b JOIN %I.recovery_accounting_mutations m ON m.batch_id=b.id JOIN %I.runtime_recovery_control c ON c.singleton AND c.state=''held'' AND c.restore_id=b.restore_id AND c.installation_id=b.target_installation_id WHERE b.creating_transaction=pg_catalog.pg_current_xact_id() AND b.operator_role=session_user AND m.table_name=$1 AND m.operation=$2)',TG_TABLE_SCHEMA,TG_TABLE_SCHEMA,TG_TABLE_SCHEMA)
 INTO permitted USING TG_TABLE_NAME,TG_OP;
 IF NOT permitted THEN
  EXECUTE pg_catalog.format('SELECT %I.runtime_recovery_admit(false)',TG_TABLE_SCHEMA);
 END IF;
 RETURN NULL;
END $$;

-- The historical row guard below retains all earlier execution restrictions.
-- Only exact, unconsumed financial changes can take its early return.
CREATE OR REPLACE FUNCTION recovery_history_row_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE before_row jsonb; after_row jsonb; old_key jsonb:='[]'; new_key jsonb:='[]';
 field text; item text; i integer; allowed text[]; org text; target_relation text;
BEGIN
 IF TG_OP IN('INSERT','UPDATE') AND recovery_accounting_matches(TG_TABLE_NAME,TG_OP,
  CASE WHEN TG_OP='UPDATE' THEN pg_catalog.to_jsonb(OLD) ELSE NULL END,pg_catalog.to_jsonb(NEW)) THEN
  RETURN NEW;
 END IF;
 IF EXISTS(SELECT 1 FROM recovery_accounting_batches WHERE creating_transaction=pg_catalog.pg_current_xact_id()) THEN
  RAISE EXCEPTION 'unprescribed recovery accounting row' USING ERRCODE='55000';
 END IF;
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

DO $$
DECLARE target_schema text:=current_schema(); item record; relation_name text;
BEGIN
 FOR item IN SELECT p.proname,pg_catalog.pg_get_function_identity_arguments(p.oid) AS args
  FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname=target_schema AND p.proname IN('recovery_accounting_current_row','recovery_accounting_metadata_guard','recovery_accounting_matches','recovery_accounting_observe','recovery_accounting_complete','runtime_recovery_write_guard','recovery_history_row_guard') LOOP
  EXECUTE pg_catalog.format('ALTER FUNCTION %I.%I(%s) SET search_path TO %I,pg_catalog,pg_temp',target_schema,item.proname,item.args,target_schema);
 END LOOP;
 FOREACH relation_name IN ARRAY ARRAY['recovery_accounting_batches','recovery_accounting_mutations','recovery_accounting_applied'] LOOP
  EXECUTE pg_catalog.format('CREATE TRIGGER recovery_accounting_insert BEFORE INSERT ON %I.%I FOR EACH ROW EXECUTE FUNCTION %I.recovery_accounting_metadata_guard()',target_schema,relation_name,target_schema);
  EXECUTE pg_catalog.format('CREATE TRIGGER recovery_accounting_permanent BEFORE UPDATE OR DELETE OR TRUNCATE ON %I.%I FOR EACH STATEMENT EXECUTE FUNCTION %I.recovery_accounting_metadata_guard()',target_schema,relation_name,target_schema);
 END LOOP;
 FOREACH relation_name IN ARRAY ARRAY['organizations','tasks','reservations','cost_evidence','ledger_entries','cost_quarantines','cost_quarantine_resolutions','financial_incidents'] LOOP
  EXECUTE pg_catalog.format('CREATE TRIGGER zz_recovery_accounting_observe AFTER INSERT OR UPDATE ON %I.%I FOR EACH ROW EXECUTE FUNCTION %I.recovery_accounting_observe()',target_schema,relation_name,target_schema);
 END LOOP;
END $$;
